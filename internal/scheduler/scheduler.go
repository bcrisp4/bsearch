// Package scheduler is the daemon's indexing loop: it keeps the catalog in
// step with the filesystem and works the queue down, in the background, on a
// laptop that is often on battery and often talking to services that are not
// running (DESIGN.md: Indexing pipeline and queue; ADR 0011).
//
// Three properties are load-bearing, and each is a decision rather than an
// implementation detail:
//
// One worker, no claims. A single goroutine does the indexing, so there is no
// contention to arbitrate and no claimed state to reconcile after a crash.
// Every pipeline stage is an idempotent upsert, which is what makes "redo the
// in-flight document on restart" a complete recovery story.
//
// Prefer Old on overrun. When a cycle takes longer than the interval, the
// next cycle is simply late: the timer is reset after the drain returns, not
// on an absolute schedule, so triggers are shed rather than queued. The
// alternative policies both assume the overrun was a transient fault that the
// next run will not hit; here the opposite is true — a drain that ran long
// did so because there is a lot of work, and a second concurrent drain would
// contend for the same rows and the same inference server while making the
// same amount of progress.
//
// Outages are not the document's fault. Before working a batch the scheduler
// proves the embedding endpoint is up, and it re-proves it before charging any
// document for a failure. An inference server that is switched off for a week
// costs zero attempts, so nothing is quietly marked failed while the user was
// not looking.
//
// Logging is operational: counts, timings, gate reasons, and the paths of
// documents that failed or were skipped, because a failure you cannot locate
// is not actionable. Never document content, never summaries, never queries
// (DESIGN.md: Privacy).
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/bcrisp4/bsearch/internal/config"
	"github.com/bcrisp4/bsearch/internal/discovery"
	"github.com/bcrisp4/bsearch/internal/domain"
	"github.com/bcrisp4/bsearch/internal/pipeline"
)

const (
	// defaultBatchSize is how many documents one claim returns. It bounds
	// how long a file saved a moment ago waits behind a bulk backlog: the
	// queue is re-read, and priority re-evaluated, every batch.
	defaultBatchSize = 32
	// defaultScanInterval is how often the filesystem is walked. It is the
	// freshness SLO for now: until FSEvents lands (#13) the scan is the only
	// way a new file is noticed, so anything slower than five minutes would
	// miss DESIGN.md's "new or changed file searchable ≤ 5 min on AC".
	//
	// Affordable because the deny-list does the work: a walk of a ~500k-file
	// $HOME with VCS, dependency and Library trees pruned measures around a
	// second warm, so this is well under a percent of one core. Once the
	// watcher is primary this becomes the missed-event backstop and should
	// get slower, not faster.
	//
	// Independent of the drain interval on purpose — one is a stat walk, the
	// other talks to an inference server, and tying them together would mean
	// choosing which of the two cadences to get wrong.
	defaultScanInterval = 5 * time.Minute
	// deferRecheckInterval is how often a deferred scheduler wakes to see
	// whether the power policy still says defer. Short enough that plugging
	// the laptop in has a visible effect, cheap enough to be free.
	deferRecheckInterval = 5 * time.Minute
	// maxLoggedPathErrors bounds per-scan path-error logging. A revoked Full
	// Disk Access grant produces one error per directory; logging all of them
	// turns a diagnosis into a haystack.
	maxLoggedPathErrors = 5
)

// Gate reasons. These are the user-facing explanations for a cycle that did
// no work, and `bsearch status` renders them verbatim. A stalled queue
// must always be distinguishable from a deferred one.
const (
	GateNone      = ""
	GateIdle      = "idle — nothing to index"
	GateBattery   = "deferred: on battery"
	GateEmbedder  = "embedding endpoint unreachable"
	GateStoreFail = "index write failed"
	// GateUnreadable means the drain ran out of documents it could read, not
	// of documents to do — files are being skipped every cycle (permissions,
	// or files that vanished). Distinct from GateIdle because reporting an
	// index as complete while it is quietly passing over files is the failure
	// this whole gate vocabulary exists to prevent.
	GateUnreadable = "files could not be read — check Full Disk Access"
)

// Queue is the catalog access the scheduler needs. Composed from the domain
// port plus the one DocumentStore method that belongs to the same story:
// exhausting a document's attempts is a queue decision, not a pipeline one.
type Queue interface {
	domain.Queue
	// MarkFailed records a document as permanently failed with a reason.
	MarkFailed(ctx context.Context, docID, reason string) error
}

// Indexer is the per-document work, as implemented by *pipeline.Indexer.
type Indexer interface {
	// Prepare proves the embedding endpoint is up and returns its dimension
	// count, creating the vector table if needed. It is the health gate.
	Prepare(ctx context.Context) (int, error)
	// StageVersions is what documents processed now are stamped with.
	StageVersions(dims int) map[string]string
	// ProcessDocument runs one document through the pipeline. Its error
	// return is fatal; everything about the document arrives as a Result.
	ProcessDocument(ctx context.Context, doc domain.Document, sv map[string]string) (pipeline.Result, error)
}

// Scanner reconciles the catalog with the filesystem, as implemented by
// *discovery.Scanner.
type Scanner interface {
	Scan(ctx context.Context) (discovery.Result, error)
}

// Options wires a Scheduler. Queue, Indexer and Scanner are required;
// everything else has a working default.
type Options struct {
	Queue   Queue
	Indexer Indexer
	Scanner Scanner
	Logger  *slog.Logger

	// Power reports the policy for the current power state. Nil means always
	// on AC — the honest default until macOS power detection lands (M7),
	// because guessing "battery" would silently stop indexing on a desktop.
	Power func() config.PowerPolicy
	// ScanInterval is how often the filesystem is walked; zero means the
	// default.
	ScanInterval time.Duration
	// BatchSize is how many documents one claim returns; zero means the
	// default.
	BatchSize int
	// MaxAttempts is how many transient failures a document absorbs before
	// it is called failed; zero means the default.
	MaxAttempts int

	// Clock is the time source, for tests. Nil means time.Now.
	Clock func() time.Time
	// Rand returns a value in [0, n), for deterministic backoff in tests.
	// Nil means math/rand/v2.
	Rand func(n int64) int64
}

// PathError is one path the last scan could not read, as reported to
// `bsearch status`. The error is flattened to a string so a Snapshot is an
// immutable value that a status handler can hold onto while the next scan
// runs.
type PathError struct {
	Path string
	Err  string
}

// Snapshot is what the scheduler is doing, as `bsearch status` reports it.
// Counters are cumulative since start; timestamps are zero until they happen.
type Snapshot struct {
	// Gate says why the last cycle did no work, in the user's terms. Empty
	// means the last cycle worked normally.
	Gate string
	// LastError is the most recent failure that stopped a cycle.
	LastError string
	// LastScan is when the filesystem was last walked to completion.
	LastScan time.Time
	// LastCycle is when the last cycle finished, worked or gated.
	LastCycle time.Time
	// LastProgress is when a document was last indexed — the timestamp that
	// distinguishes a queue that is slow from one that is stuck.
	LastProgress time.Time

	Indexed  int // documents indexed since start
	Failed   int // documents given up on since start
	Skipped  int // documents unreadable at the time since start
	Retried  int // transient failures rescheduled since start
	Swept    int // documents re-queued by a stale sweep since start
	ScanErrs int // per-path problems in the last scan (TCC, unreadable)
	// PathErrors is a sample of those problems — the first few, capped at
	// maxLoggedPathErrors. A count alone says something is unreadable without
	// saying what, and "~/Documents: operation not permitted" is the whole
	// diagnosis (issue #14 turns it into remediation).
	PathErrors []PathError
	// ScanReachedNothing means the last scan hit errors and reached no files
	// at all — the signature of a missing Full Disk Access grant, and the one
	// scan outcome that leaves the daemon looking healthy while indexing
	// nothing (DESIGN.md: TCC is first-class state).
	ScanReachedNothing bool
	Deferring          bool
}

// Scheduler runs the indexing loop. Construct with New, drive with Run.
type Scheduler struct {
	queue   Queue
	indexer Indexer
	scanner Scanner
	log     *slog.Logger

	power        func() config.PowerPolicy
	scanInterval time.Duration
	batchSize    int
	maxAttempts  int
	now          func() time.Time
	rnd          func(int64) int64

	// notify is the wake hint. Buffered by one and written non-blockingly:
	// the table is the source of truth, so a dropped hint costs latency, not
	// correctness, and the timer is the fallback that makes that true.
	notify chan struct{}

	// sweptVersions and sweptDims track the once-per-process stale sweeps.
	// Once per process, not once per cycle: configuration is read at startup
	// and cannot change under a running daemon, so a second sweep could only
	// ever move zero rows.
	sweptVersions bool
	sweptDims     bool

	mu   sync.Mutex
	snap Snapshot
}

// New builds a Scheduler.
func New(opts Options) (*Scheduler, error) {
	if opts.Queue == nil || opts.Indexer == nil || opts.Scanner == nil {
		return nil, errors.New("scheduler: Queue, Indexer, and Scanner are all required")
	}
	s := &Scheduler{
		queue:        opts.Queue,
		indexer:      opts.Indexer,
		scanner:      opts.Scanner,
		log:          opts.Logger,
		power:        opts.Power,
		scanInterval: opts.ScanInterval,
		batchSize:    opts.BatchSize,
		maxAttempts:  opts.MaxAttempts,
		now:          opts.Clock,
		rnd:          opts.Rand,
		notify:       make(chan struct{}, 1),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.power == nil {
		s.power = func() config.PowerPolicy {
			return config.PowerPolicy{IndexInterval: config.Interval{Duration: 5 * time.Minute}}
		}
	}
	if s.scanInterval <= 0 {
		s.scanInterval = defaultScanInterval
	}
	if s.batchSize <= 0 {
		s.batchSize = defaultBatchSize
	}
	if s.maxAttempts <= 0 {
		s.maxAttempts = defaultMaxAttempts
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.rnd == nil {
		//nolint:gosec // G404: jitter spreads retries, it is not a secret
		s.rnd = rand.Int64N
	}
	return s, nil
}

// Notify hints that there may be new work. It never blocks and never fails:
// a hint carries no information beyond "look again", so dropping one when a
// hint is already pending is exactly right — waking once and finding fifty
// new rows beats waking fifty times.
//
// #13's watcher is the caller. Nothing depends on delivery: the periodic
// cycle is the fallback that makes a lost hint a latency problem rather than
// a correctness one.
func (s *Scheduler) Notify() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Snapshot reports what the scheduler is doing. Safe for concurrent use —
// the HTTP status handler calls it while a cycle is running.
//
// PathErrors is copied rather than shared. Returning the struct by value
// copies the slice header, not the backing array, so the caller would
// otherwise be reading the same memory the next scan overwrites — a data race
// on the one field a user reads when something is already wrong.
func (s *Scheduler) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.snap
	snap.PathErrors = slices.Clone(s.snap.PathErrors)
	return snap
}

// Run drives the loop until ctx is cancelled, which is the only way it ends;
// the returned error is always nil, and exists so Run reads like every other
// long-running call in the codebase.
//
// The first cycle runs immediately. A daemon that starts after the machine
// has been off for a week should not sit idle waiting for a tick before
// noticing that the corpus moved on without it.
func (s *Scheduler) Run(ctx context.Context) error {
	s.log.Info("indexing scheduler started",
		"scan_interval", s.scanInterval,
		"batch_size", s.batchSize,
		"max_attempts", s.maxAttempts)

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("indexing scheduler stopped")
			return nil
		case <-timer.C:
		case <-s.notify:
		}

		next := s.cycle(ctx)
		if ctx.Err() != nil {
			s.log.Info("indexing scheduler stopped")
			return nil
		}
		if next <= 0 {
			// A policy with no interval — a zero-value PowerPolicy from a
			// future power adapter — would otherwise reset the timer to fire
			// immediately, spinning this loop on a full core. Config validates
			// intervals as positive, so this only ever catches a caller bug.
			next = deferRecheckInterval
		}
		// Reset after the cycle rather than on a fixed schedule: that is what
		// makes the overrun policy Prefer Old (see the package comment) and
		// what lets a power-state change take effect on the next interval
		// rather than the next restart.
		//
		// No drain of timer.C between Stop and Reset. That idiom is for Go
		// 1.22 and earlier, where the channel was buffered and a tick that
		// landed while the cycle was running would survive the Reset and fire
		// the next select immediately — which would defeat exactly the policy
		// above. Since 1.23 the channel is unbuffered and Reset guarantees no
		// stale value is received; go.mod requires 1.26, so the drain would
		// now block forever on the common path where the timer never fired.
		timer.Stop()
		timer.Reset(next)
	}
}

// cycle runs one scan-if-due plus one drain, and returns how long to wait
// before the next one.
func (s *Scheduler) cycle(ctx context.Context) time.Duration {
	defer s.mark(func(snap *Snapshot) { snap.LastCycle = s.now() })

	policy := s.power()

	// Discovery runs whether or not indexing does. It is a stat walk, not
	// inference: cheap enough to keep the catalog honest on battery, and
	// leaving it out would mean a laptop that spent the day unplugged came
	// back with no idea what had changed (DESIGN.md: cheap stages always run).
	if s.scanDue() {
		s.scan(ctx)
	}
	if ctx.Err() != nil {
		return deferRecheckInterval
	}

	if policy.IndexInterval.Defer {
		s.gate(GateBattery, true)
		s.log.Debug("indexing deferred", "reason", GateBattery)
		return deferRecheckInterval
	}
	s.mark(func(snap *Snapshot) { snap.Deferring = false })

	s.drain(ctx)
	return policy.IndexInterval.Duration
}

func (s *Scheduler) scanDue() bool {
	snap := s.Snapshot()
	return snap.LastScan.IsZero() || s.now().Sub(snap.LastScan) >= s.scanInterval
}

// scan walks the filesystem and reconciles the catalog. Per-path problems are
// reported, never fatal: a single unreadable directory must not stop the rest
// of the corpus being indexed, and a permission error is a first-class state
// the user needs to see (DESIGN.md: TCC).
func (s *Scheduler) scan(ctx context.Context) {
	start := s.now()
	res, err := s.scanner.Scan(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.log.Error("scan failed", "error", err)
		s.mark(func(snap *Snapshot) { snap.LastError = err.Error() })
		return
	}

	// The same cap serves the log and status: enough paths to recognise the
	// pattern, few enough that neither turns into a haystack.
	var sample []PathError
	for i, pe := range res.PathErrors {
		if i == maxLoggedPathErrors {
			s.log.Warn("further scan path errors suppressed",
				"suppressed", len(res.PathErrors)-maxLoggedPathErrors)
			break
		}
		s.log.Warn("scan could not read a path", "path", pe.Path, "error", pe.Err)
		sample = append(sample, PathError{Path: pe.Path, Err: pe.Err.Error()})
	}
	reached := res.Discovered + res.Unchanged + res.Renamed + res.Dataless
	if len(res.PathErrors) > 0 && reached == 0 {
		// Every root failed and nothing was reachable. That is a permissions
		// problem — almost always a missing or revoked Full Disk Access grant
		// — not a successful scan of an empty machine, and it is the one scan
		// outcome that leaves the daemon looking healthy while indexing
		// nothing at all. The removed one-shot command exited non-zero here;
		// a daemon cannot, so it says so at Error and keeps the reason where
		// `bsearch status` will find it (CLAUDE.md: EPERM on scan is
		// first-class state, never a silent skip).
		s.log.Error("scan reached no files at all — check Full Disk Access for bsearch in System Settings",
			"path_errors", len(res.PathErrors))
	}

	s.log.Info("scan complete",
		"discovered", res.Discovered,
		"unchanged", res.Unchanged,
		"renamed", res.Renamed,
		"dataless", res.Dataless,
		"path_errors", len(res.PathErrors),
		"duration_ms", s.now().Sub(start).Milliseconds())

	s.mark(func(snap *Snapshot) {
		snap.LastScan = s.now()
		snap.ScanErrs = len(res.PathErrors)
		snap.PathErrors = sample
		snap.ScanReachedNothing = len(res.PathErrors) > 0 && reached == 0
	})
}

// drain works the queue until it is empty, gated, or cancelled.
//
// It re-claims rather than iterating one list, so a file saved during a long
// backlog drain is picked up within a batch instead of at the end. The cost
// is that a document which stays claimable — one skipped because it is
// unreadable right now, and therefore left untouched by design — would be
// claimed forever; seen is what bounds the drain to one pass per document.
func (s *Scheduler) drain(ctx context.Context) {
	seen := make(map[string]struct{})
	dims, prepared := 0, false
	skipped := 0

	for {
		if ctx.Err() != nil {
			return
		}
		// The version sweep needs no network, so it runs before anything can
		// decide the queue is empty: a corpus that is entirely indexed under
		// a superseded model looks like no work at all until it is swept.
		if !s.sweptVersions {
			sv := s.indexer.StageVersions(0)
			delete(sv, domain.StageEmbeddingDims) // unknown until Prepare
			if !s.sweep(ctx, sv) {
				return
			}
			s.sweptVersions = true
			continue
		}

		docs, err := s.queue.ClaimBatch(ctx, s.now(), s.batchSize)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Error("claim batch failed", "error", err)
			s.gate(GateStoreFail, false)
			s.mark(func(snap *Snapshot) { snap.LastError = err.Error() })
			return
		}
		// Filtered here, recorded in process: a batch can be claimed and then
		// set aside (the dims sweep re-claims from scratch), and marking a
		// document seen before it has actually been through the pipeline
		// would drop it for the rest of the drain.
		fresh := docs[:0]
		for _, doc := range docs {
			if _, dup := seen[doc.ID]; !dup {
				fresh = append(fresh, doc)
			}
		}

		// Nothing to do and nothing left to check: stop before Prepare, so an
		// up-to-date corpus costs zero network calls per cycle. The daemon is
		// idle for weeks at a time; probing an endpoint every five minutes to
		// confirm there is still nothing to embed would keep a model resident
		// and a laptop awake for no reason.
		if len(fresh) == 0 && s.sweptDims {
			s.gate(idleGate(skipped), false)
			return
		}

		if !prepared {
			d, ok := s.prepare(ctx)
			if !ok {
				return
			}
			dims, prepared = d, true
			if !s.sweptDims {
				// Dims are part of the vector table's identity but outside
				// the embedding fingerprint, so a server that changed them
				// under an unchanged model name strands documents that look
				// current. Only knowable after the probe, hence the second
				// sweep.
				if !s.sweep(ctx, map[string]string{
					domain.StageEmbeddingDims: s.indexer.StageVersions(d)[domain.StageEmbeddingDims],
				}) {
					return
				}
				s.sweptDims = true
				continue
			}
		}
		if len(fresh) == 0 {
			s.gate(idleGate(skipped), false)
			return
		}

		sv := s.indexer.StageVersions(dims)
		n, ok := s.process(ctx, fresh, sv, seen)
		skipped += n
		if !ok {
			return
		}
		s.gate(GateNone, false)

		// Power is re-read between batches, not once per cycle: unplugging
		// the laptop part-way through a backlog should stop the drain, not
		// be noticed an hour later.
		if s.power().IndexInterval.Defer {
			s.gate(GateBattery, true)
			s.log.Debug("drain paused", "reason", GateBattery)
			return
		}
	}
}

// prepare runs the health gate. A failure defers the whole batch and charges
// nothing to any document: an endpoint that is switched off is not evidence
// about any file.
func (s *Scheduler) prepare(ctx context.Context) (int, bool) {
	dims, err := s.indexer.Prepare(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return 0, false
		}
		s.log.Warn("indexing deferred", "reason", GateEmbedder, "error", err)
		s.gate(GateEmbedder, false)
		s.mark(func(snap *Snapshot) { snap.LastError = err.Error() })
		return 0, false
	}
	return dims, true
}

// sweep re-queues documents whose derived data predates current, reporting
// whether the drain should continue.
func (s *Scheduler) sweep(ctx context.Context, current map[string]string) bool {
	moved, err := s.queue.ResetStale(ctx, current)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		s.log.Error("stale sweep failed", "error", err)
		s.gate(GateStoreFail, false)
		s.mark(func(snap *Snapshot) { snap.LastError = err.Error() })
		return false
	}
	if moved > 0 {
		// Worth Info: this is the daemon explaining why it is about to spend
		// an hour of inference on a corpus the user thought was indexed.
		s.log.Info("re-queued documents built by a superseded pipeline stage", "documents", moved)
		s.mark(func(snap *Snapshot) { snap.Swept += moved })
	}
	return true
}

// process works one batch, reporting how many documents were unreadable and
// whether the drain should continue. Each document is recorded in seen as it
// is handled, which is what bounds the drain to one pass per document however
// the outcome leaves the row.
func (s *Scheduler) process(ctx context.Context, docs []domain.Document, sv map[string]string, seen map[string]struct{}) (skipped int, ok bool) {
	for _, doc := range docs {
		if ctx.Err() != nil {
			return skipped, false
		}
		seen[doc.ID] = struct{}{}
		res, err := s.indexer.ProcessDocument(ctx, doc, sv)
		if err != nil {
			if ctx.Err() != nil {
				return skipped, false
			}
			// The machinery failed, not the document: a store write that did
			// not land says nothing about this file, so nothing is charged.
			s.log.Error("indexing failed", "path", doc.Path, "error", err)
			s.gate(GateStoreFail, false)
			s.mark(func(snap *Snapshot) { snap.LastError = err.Error() })
			return skipped, false
		}

		switch res.Outcome {
		case pipeline.OutcomeIndexed:
			s.log.Debug("indexed", "path", doc.Path)
			s.mark(func(snap *Snapshot) {
				snap.Indexed++
				snap.LastProgress = s.now()
			})
		case pipeline.OutcomeFailed:
			s.log.Warn("document cannot be indexed", "path", doc.Path, "error", res.Err)
			s.mark(func(snap *Snapshot) { snap.Failed++ })
		case pipeline.OutcomeSkipped:
			// Unreadable right now — vanished, or TCC. The row is untouched
			// on purpose: restoring the file or granting access must take
			// effect without the file having to change.
			s.log.Debug("document unreadable, leaving it alone", "path", doc.Path, "error", res.Err)
			skipped++
			s.mark(func(snap *Snapshot) { snap.Skipped++ })
		case pipeline.OutcomeTransient:
			if !s.retry(ctx, doc, res.Err) {
				return skipped, false
			}
		}
	}
	return skipped, true
}

// idleGate names why a drain stopped with nothing left to claim.
//
// "Idle" and "everything left is unreadable" look identical from the queue's
// side — a skipped document is deliberately left untouched, so it stays
// claimable and simply falls out of this drain's seen set — but they are
// opposite situations for the user. One means the corpus is indexed; the
// other means files are being passed over every cycle, which is what a
// revoked Full Disk Access grant looks like from here.
func idleGate(skipped int) string {
	if skipped > 0 {
		return GateUnreadable
	}
	return GateIdle
}

// retry decides what a transient failure means, and reports whether the drain
// should continue.
//
// The decision rests on one question: was the service healthy when this
// document failed? Re-probing answers it. A failed probe means the endpoint
// went down mid-batch, so the failure is the outage's and the batch is
// deferred with nothing charged. A passing probe means the endpoint is fine
// and this document is the odd one out, which is the only case where an
// attempt is spent.
//
// That is the whole of "attempts are only counted while the service is
// healthy": without the re-probe, an inference server switched off overnight
// would quietly mark a chunk of the corpus permanently failed.
func (s *Scheduler) retry(ctx context.Context, doc domain.Document, cause error) bool {
	if _, err := s.indexer.Prepare(ctx); err != nil {
		if ctx.Err() != nil {
			return false
		}
		s.log.Warn("indexing deferred mid-batch", "reason", GateEmbedder, "error", err)
		s.gate(GateEmbedder, false)
		s.mark(func(snap *Snapshot) { snap.LastError = err.Error() })
		return false
	}

	reason := "unknown error"
	if cause != nil {
		reason = cause.Error()
	}
	attempts := doc.Attempts + 1
	if attempts >= s.maxAttempts {
		s.log.Warn("giving up on a document after repeated failures",
			"path", doc.Path, "attempts", attempts, "error", cause)
		// Record the final count before flipping the state, so the row does
		// not end up claiming "after 5 attempts" in last_error while the
		// attempts column still reads 4 — MarkFailed writes state and reason
		// only. The next_retry_at this also sets is inert once the second
		// write lands: the row is terminal, so dispatch stops looking at it,
		// and anything that revives it (a file change, a stale sweep) clears
		// it. A crash between the two writes leaves the row claimable with
		// attempts already at the cap, so it is re-claimed and goes terminal
		// one attempt later — self-correcting, and the reason collapsing this
		// into one transaction is not worth a new store method.
		if err := s.queue.Reschedule(ctx, doc.ID, attempts, s.now(), reason); err != nil {
			return s.storeFailure(ctx, "record final attempt", err)
		}
		if err := s.queue.MarkFailed(ctx, doc.ID,
			fmt.Sprintf("%s (after %d attempts)", reason, attempts)); err != nil {
			return s.storeFailure(ctx, "mark failed", err)
		}
		s.mark(func(snap *Snapshot) { snap.Failed++ })
		return true
	}

	wait := fullJitter(attempts, defaultBackoffBase, defaultBackoffCap, s.rnd)
	if err := s.queue.Reschedule(ctx, doc.ID, attempts, s.now().Add(wait), reason); err != nil {
		return s.storeFailure(ctx, "reschedule", err)
	}
	s.log.Debug("retrying a document later",
		"path", doc.Path, "attempts", attempts, "retry_in_s", int(wait.Seconds()))
	s.mark(func(snap *Snapshot) { snap.Retried++ })
	return true
}

// storeFailure records a write that did not land and stops the drain.
func (s *Scheduler) storeFailure(ctx context.Context, op string, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	s.log.Error(op+" failed", "error", err)
	s.gate(GateStoreFail, false)
	s.mark(func(snap *Snapshot) { snap.LastError = err.Error() })
	return false
}

// gate records why the scheduler is not making progress.
func (s *Scheduler) gate(reason string, deferring bool) {
	s.mark(func(snap *Snapshot) {
		snap.Gate = reason
		snap.Deferring = deferring
	})
}

// mark applies an update to the snapshot under the lock.
func (s *Scheduler) mark(update func(*Snapshot)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	update(&s.snap)
}

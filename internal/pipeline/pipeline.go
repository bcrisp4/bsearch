// Package pipeline wires read → chunk → embed → store for one content at a
// time (DESIGN.md: Indexing pipeline and queue). This is the M1 one-shot
// subset: no conversion, no summaries, no retry/backoff machinery — those
// land with the daemon (M3, M6). Cross-content embed batching is likewise
// daemon territory; per-content calls keep failure attribution and
// resumability simple, and the adapter already batches chunks within a
// request.
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"

	"github.com/bcrisp4/bsearch/internal/chunker"
	"github.com/bcrisp4/bsearch/internal/domain"
)

// Options wires an Indexer's collaborators.
type Options struct {
	Store    domain.ContentStore
	Vectors  domain.VectorStore
	Embedder domain.Embedder
	// Transient classifies embed errors: true aborts the run (endpoint
	// down — the content stays chunked and resumes next run), false marks
	// the content failed (poison input). Nil treats every embed error as
	// transient — the conservative default, never wrongly burning content
	// on an outage (DESIGN.md: health gates).
	Transient func(error) bool
	// Progress receives per-file progress lines; nil is silent. Lines
	// carry paths, never document content (DESIGN.md: Privacy).
	Progress io.Writer
}

// Indexer runs the one-shot indexing pipeline.
type Indexer struct {
	opts Options
	spec domain.EmbeddingSpec
	// fp caches spec.Fingerprint() — compared per content.
	fp string
}

// New builds an Indexer. Store, Vectors, and Embedder are required.
func New(opts Options) (*Indexer, error) {
	if opts.Store == nil || opts.Vectors == nil || opts.Embedder == nil {
		return nil, errors.New("pipeline: Store, Vectors, and Embedder are all required")
	}
	if opts.Transient == nil {
		opts.Transient = func(error) bool { return true }
	}
	if opts.Progress == nil {
		opts.Progress = io.Discard
	}
	spec := opts.Embedder.Spec()
	return &Indexer{opts: opts, spec: spec, fp: spec.Fingerprint()}, nil
}

// Summary is one Run's outcome counts.
type Summary struct {
	Indexed  int // processed to state=indexed this run
	UpToDate int // skipped: already indexed with current stage versions
	Failed   int // marked failed this run (undecodable, poison embed input)
	Skipped  int // unreadable right now (vanished, permissions) — retried next run
	Warnings int // oversized-atomic-block chunk warnings
}

// Outcome classifies what happened to one content. The distinction that
// matters is who is at fault, because that decides who pays: only Failed
// reflects the bytes themselves, so only Failed is allowed to be terminal.
// Skipped and Transient are the environment's fault and must leave the
// content's budget alone (DESIGN.md: health gates).
type Outcome int

const (
	// OutcomeIndexed: chunked, embedded, stored, state=indexed.
	OutcomeIndexed Outcome = iota
	// OutcomeFailed: the content cannot be indexed and re-trying it
	// unchanged would fail identically. Already recorded as state=failed —
	// permanently, since the bytes a hash names cannot change (a config
	// change re-queues it via the stale sweep).
	OutcomeFailed
	// OutcomeSkipped: unreadable right now — vanished between scan and
	// pipeline, or permission denied (TCC). The row is untouched.
	OutcomeSkipped
	// OutcomeTransient: an external service failed. Nothing is recorded;
	// the caller decides between retrying with backoff and treating it as
	// an outage, which is a judgement only the caller can make.
	OutcomeTransient
	// OutcomeChanged: the bytes at the path no longer hash to the content
	// this work was claimed for — the file changed underneath the claim.
	// Nothing was written anywhere: not the content row (it describes the
	// old bytes, which may still live at another path), and never the
	// documents row (discovery owns it, and its next pass re-points the
	// path). The claim is simply abandoned.
	//
	// Distinct from Skipped because in bulk Skipped is how a missing Full
	// Disk Access grant announces itself, and a hot-edited file must not
	// read as a permissions problem.
	OutcomeChanged
	// OutcomeSuperseded: the content moved on while it was being worked on
	// — its row was swept (every referencing path deleted mid-flight), or
	// its chunk rows were replaced under an in-flight vector write. Nothing
	// is owed: whatever superseded this pass already left the catalog in
	// the right state, and the next pass sees it.
	//
	// The two arms mean different things to the caller (see movedOn): a
	// sweep is routine, a mid-flight rewrite means the single-writer
	// invariant broke (ADR 0014).
	OutcomeSuperseded
)

// Result is one content's outcome and, when there is one, the cause.
type Result struct {
	Outcome Outcome
	// Err is why, for every outcome except Indexed. It is not a failure of
	// the call — ProcessContent's own error return is.
	Err error
	// Warnings counts oversized-atomic-block chunk warnings.
	Warnings int
}

// Run processes items (typically Store.ListWorkItems output): stale or
// unfinished content is read, chunked, embedded, and stored; content already
// indexed with current stage versions is skipped, as is content that already
// failed under the current stage versions (a config change gives failed
// content a fresh attempt — previously-failed content is not re-counted in
// Summary). A fully up-to-date corpus makes zero network calls.
//
// The returned error is an abort — context cancellation, a store failure,
// or a transient embed failure (endpoint down). Partial progress is durable
// either way: every completed content is committed, and an aborted one is
// left in state=chunked to resume on the next run. Per-content permanent
// problems never abort; they are counted in Summary.Failed. Files that
// cannot be read right now (vanished between scan and pipeline, permission
// denied) are counted in Summary.Skipped and left untouched — the cause is
// environmental, not the content's fault, so the content must not be
// burned (it retries on the next run).
func (ix *Indexer) Run(ctx context.Context, items []domain.WorkItem) (Summary, error) {
	var sum Summary

	// First pass, before dims are known: split on state + chunker version +
	// embedding fingerprint. Items that pass re-check against dims below.
	var work, current []domain.WorkItem
	for _, item := range items {
		c := item.Content
		switch {
		case c.State == domain.ContentStateFailed && ix.versionsCurrent(c):
			// Still failed under the exact config that failed it — a fresh
			// attempt would fail identically, because the bytes a hash names
			// cannot change. Only a config change (different fingerprint)
			// gives it a new attempt. Not counted: it is not this run's
			// failure.
		case c.State == domain.ContentStateIndexed && ix.versionsCurrent(c):
			current = append(current, item)
		default:
			work = append(work, item)
		}
	}
	if len(work) == 0 {
		// No probe: a fully up-to-date corpus makes zero network calls. A
		// server-side dims change is undetectable here and surfaces loudly
		// at query time instead (query/table dimension mismatch).
		sum.UpToDate = len(current)
		return sum, nil
	}

	dims, err := ix.Prepare(ctx)
	if err != nil {
		return sum, err
	}

	// Second pass: the vector-table generation identity includes dims, so a
	// server-side dims change under an unchanged model name would otherwise
	// strand "up to date" content outside the generation search now uses.
	sv := ix.StageVersions(dims)
	for _, item := range current {
		if item.Content.StageVersions[domain.StageEmbeddingDims] != sv[domain.StageEmbeddingDims] {
			work = append(work, item)
			continue
		}
		sum.UpToDate++
	}

	for _, item := range work {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		res, err := ix.ProcessContent(ctx, item, sv)
		if err != nil {
			return sum, err
		}
		sum.Warnings += res.Warnings
		switch res.Outcome {
		case OutcomeIndexed:
			sum.Indexed++
		case OutcomeFailed:
			sum.Failed++
		case OutcomeSkipped:
			sum.Skipped++
		case OutcomeChanged, OutcomeSuperseded:
			// Nothing to count: the content this run was working on is not
			// what the catalog (or the filesystem) holds any more, and
			// whatever replaced it is already queued or will be at the next
			// scan.
		case OutcomeTransient:
			// One-shot has nowhere to put a retry, so endpoint trouble ends
			// the run. The content stays chunked and resumes next time;
			// nothing is burned. The daemon's scheduler is what turns this
			// same outcome into backoff instead of an exit.
			return sum, res.Err
		}
	}
	return sum, nil
}

// Prepare proves the embedding endpoint is up and serving the configured
// model, and returns the dimension count vec0 fixes at CREATE. It creates the
// vector table for spec+dims if there isn't one.
//
// One tiny query embedding does both jobs, which is why the daemon's health
// gate is this call rather than a separate endpoint: a probe that exercises a
// different code path than the work is a probe that can pass while the work
// fails.
func (ix *Indexer) Prepare(ctx context.Context) (int, error) {
	probe, err := ix.opts.Embedder.EmbedQuery(ctx, "bsearch dimension probe")
	if err != nil {
		return 0, fmt.Errorf("embedding endpoint check failed (is the inference server running and serving %q?): %w",
			ix.spec.Model, err)
	}
	if len(probe) == 0 {
		return 0, fmt.Errorf("embedding endpoint returned a zero-dimension vector for model %q", ix.spec.Model)
	}
	dims := len(probe)
	if err := ix.opts.Vectors.EnsureVecTable(ctx, ix.spec, dims); err != nil {
		return 0, err
	}
	return dims, nil
}

// StageVersions is what content processed right now is stamped with, and
// therefore also what a staleness check compares against (DESIGN.md: Pipeline
// metadata and model migration). dims comes from Prepare.
func (ix *Indexer) StageVersions(dims int) map[string]string {
	return map[string]string{
		domain.StageChunker:       chunker.Version,
		domain.StageEmbedding:     ix.fp,
		domain.StageEmbeddingDims: strconv.Itoa(dims),
		domain.StageVecMetric:     domain.VectorMetric,
	}
}

// versionsCurrent reports whether c's derived data was produced by this
// chunker version, this embedding spec, and the current vector metric, dims
// aside (DESIGN.md: Pipeline metadata — StageVersions diffed against
// current config). Dims are only known after the probe and are re-checked
// separately in Run.
func (ix *Indexer) versionsCurrent(c domain.Content) bool {
	return c.StageVersions[domain.StageChunker] == chunker.Version &&
		c.StageVersions[domain.StageEmbedding] == ix.fp &&
		c.StageVersions[domain.StageVecMetric] == domain.VectorMetric
}

// ProcessContent runs one claimed work item through read → verify → chunk →
// embed → store and classifies what happened. sv comes from StageVersions.
//
// The returned error is reserved for failures of the machinery itself — a
// store write that didn't land, a cancelled context — and every caller should
// treat it as fatal. Everything that is a fact about this one content, up to
// and including "the embedding server is down", arrives as a Result, because
// only the caller knows whether the right response is to stop, to retry
// later, or to move on to the next item.
func (ix *Indexer) ProcessContent(ctx context.Context, item domain.WorkItem, sv map[string]string) (Result, error) {
	c := item.Content

	// Merge, don't replace: stage keys owned by other stages (converter,
	// summarizer — later milestones) must survive a re-index, or partial
	// rebuild decisions lose their inputs.
	merged := make(map[string]string, len(c.StageVersions)+len(sv))
	for k, v := range c.StageVersions {
		merged[k] = v
	}
	for k, v := range sv {
		merged[k] = v
	}
	c.StageVersions = merged

	raw, err := os.ReadFile(item.Path)
	if err != nil {
		// Environmental, not the content's fault: the file vanished after
		// the scan, or reading is denied (TCC). Leave the row alone — it
		// retries next run, and granting access or restoring the file
		// needs no content change to take effect.
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(ix.opts.Progress, "skipped %s: file no longer exists\n", item.Path)
		} else {
			fmt.Fprintf(ix.opts.Progress, "skipped %s: %v (will retry next run)\n", item.Path, err)
		}
		return Result{Outcome: OutcomeSkipped, Err: err}, nil
	}

	// The pipeline hashes what it actually read and writes under *that*
	// identity, never trusting the hash discovery recorded earlier
	// (ADR 0015). A mismatch means the file changed between the claim and
	// this read: abandon — writing chunks under c.Hash would attach the new
	// bytes' chunks to the old bytes' identity, everywhere that content is
	// referenced from.
	digest := sha256.Sum256(raw)
	if got := hex.EncodeToString(digest[:]); got != c.Hash {
		fmt.Fprintf(ix.opts.Progress, "abandoned %s: changed while queued\n", item.Path)
		return Result{
			Outcome: OutcomeChanged,
			Err:     fmt.Errorf("content at %s changed while queued", item.Path),
		}, nil
	}

	text, err := chunker.Normalize(raw)
	if err != nil {
		// Undecodable — permanent, and under content addressing permanent
		// forever: these exact bytes cannot decode differently tomorrow
		// (DESIGN.md: Chunking/Encoding).
		return ix.fail(ctx, c, item.Path, fmt.Sprintf("normalize: %v", err))
	}

	res := chunker.Chunk(text, ix.spec.CeilingTokens)
	out := Result{Outcome: OutcomeIndexed, Warnings: len(res.Warnings)}
	for _, w := range res.Warnings {
		// Path, ordinal, and reason only — the heading path is document
		// content and must stay out of default output (DESIGN.md: Privacy).
		fmt.Fprintf(ix.opts.Progress, "warning: %s: %s (chunk %d)\n", item.Path, w.Reason, w.Ordinal)
	}

	// Short write transaction before any network call (DESIGN.md:
	// transactions never wrap network calls).
	c.State = domain.ContentStateChunked
	chunkIDs, err := ix.opts.Store.StoreChunks(ctx, c, res.Chunks)
	if err != nil {
		if gone, r := ix.movedOn(item.Path, err); gone {
			return r, nil
		}
		return Result{}, fmt.Errorf("store %s: %w", item.Path, err)
	}

	// Zero chunks is a legitimate terminal outcome — an empty or
	// whitespace-only file reaches indexed with nothing to embed, and the
	// absence of chunks *is* "not searchable" (DESIGN.md: Identity).
	if len(res.Chunks) > 0 {
		vectors, err := ix.opts.Embedder.EmbedPassages(ctx, res.Chunks)
		if err != nil {
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			if ix.opts.Transient(err) {
				// Endpoint trouble, not the content's fault. The chunks are
				// already committed, so the content resumes from chunked
				// whenever the caller comes back to it — nothing is burned
				// and nothing is redone.
				out.Outcome = OutcomeTransient
				out.Err = fmt.Errorf("embed %s: %w", item.Path, err)
				return out, nil
			}
			return ix.failWith(ctx, c, item.Path, out, fmt.Sprintf("embed: %v", err))
		}
		if err := ix.opts.Vectors.UpsertVectors(ctx, chunkIDs, vectors); err != nil {
			// The widest window in the pipeline: an embed is seconds of
			// network, and both a sweep and a re-chunk inside it land here as
			// chunk IDs that no longer exist.
			if gone, r := ix.movedOn(item.Path, err); gone {
				return r, nil
			}
			return Result{}, fmt.Errorf("store vectors for %s: %w", item.Path, err)
		}
	}

	if err := ix.opts.Store.UpdateContentState(ctx, c.Hash, domain.ContentStateIndexed); err != nil {
		if gone, r := ix.movedOn(item.Path, err); gone {
			return r, nil
		}
		return Result{}, fmt.Errorf("finalize %s: %w", item.Path, err)
	}
	fmt.Fprintf(ix.opts.Progress, "indexed %s (%d chunks)\n", item.Path, len(res.Chunks))
	return out, nil
}

// movedOn classifies a store error as "the content moved on while we were
// working on it" — swept (ErrContentGone) or its chunks replaced mid-write
// (ErrContentSuperseded) — which is an outcome rather than a fault.
//
// Not failed: failed is a claim about the bytes that would sit in `bsearch
// status` under a reason, and the bytes this pass was making claims about
// are not in the catalog any more.
//
// The two arms are not alike. A sweep between items is ordinary — the
// scheduler reconciles and sweeps between claims, so a row in the batch it
// claimed really can be gone. A *rewrite* is not: the scheduler re-reads
// each row immediately before working on it and nothing writes the catalog
// while it does, so reaching that arm means the single-writer invariant
// broke (ADR 0014). The caller is what tells them apart; both arrive here
// as OutcomeSuperseded, and the scheduler reports the second loudly.
func (ix *Indexer) movedOn(path string, err error) (bool, Result) {
	switch {
	case errors.Is(err, domain.ErrContentGone):
		fmt.Fprintf(ix.opts.Progress, "skipped %s: deleted while it was being indexed\n", path)
	case errors.Is(err, domain.ErrContentSuperseded):
		fmt.Fprintf(ix.opts.Progress, "skipped %s: changed again while it was being indexed\n", path)
	default:
		return false, Result{}
	}
	return true, Result{Outcome: OutcomeSuperseded, Err: err}
}

// fail records a permanent failure for content that has no Result in
// flight yet.
func (ix *Indexer) fail(ctx context.Context, c domain.Content, path, reason string) (Result, error) {
	return ix.failWith(ctx, c, path, Result{}, reason)
}

// failWith marks c permanently failed, preserving any warnings already
// counted. The content row is first written with the current stage versions
// (and its chunks cleared — failed content must not serve stale chunks), so a
// later config change is detectable as a fingerprint mismatch and re-attempts
// it. Only a store error (or cancellation) is returned as an error.
func (ix *Indexer) failWith(ctx context.Context, c domain.Content, path string, out Result, reason string) (Result, error) {
	c.State = domain.ContentStateFailed
	if _, err := ix.opts.Store.StoreChunks(ctx, c, nil); err != nil {
		if gone, r := ix.movedOn(path, err); gone {
			return r, nil
		}
		return Result{}, fmt.Errorf("record failure for %s: %w", path, err)
	}
	if err := ix.opts.Store.MarkFailed(ctx, c.Hash, reason); err != nil {
		if gone, r := ix.movedOn(path, err); gone {
			return r, nil
		}
		return Result{}, err
	}
	fmt.Fprintf(ix.opts.Progress, "failed %s: %s\n", path, reason)
	out.Outcome = OutcomeFailed
	out.Err = errors.New(reason)
	return out, nil
}

// Package pipeline wires discover → chunk → embed → store for one document
// at a time (DESIGN.md: Indexing pipeline and queue). This is the M1
// one-shot subset: no conversion, no summaries, no retry/backoff machinery
// — those land with the daemon (M3, M6). Cross-document embed batching is
// likewise daemon territory; per-document calls keep failure attribution
// and resumability simple, and the adapter already batches chunks within a
// request.
package pipeline

import (
	"context"
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
	Store    domain.DocumentStore
	Vectors  domain.VectorStore
	Embedder domain.Embedder
	// Transient classifies embed errors: true aborts the run (endpoint
	// down — the document stays chunked and resumes next run), false marks
	// the document failed (poison input). Nil treats every embed error as
	// transient — the conservative default, never wrongly burning a
	// document on an outage (DESIGN.md: health gates).
	Transient func(error) bool
	// Progress receives per-file progress lines; nil is silent. Lines
	// carry paths, never document content (DESIGN.md: Privacy).
	Progress io.Writer
}

// Indexer runs the one-shot indexing pipeline.
type Indexer struct {
	opts Options
	spec domain.EmbeddingSpec
	// fp caches spec.Fingerprint() — compared per document.
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

// Outcome classifies what happened to one document. The distinction that
// matters is who is at fault, because that decides who pays: only Failed
// reflects the document's own content, so only Failed is allowed to be
// terminal. Skipped and Transient are the environment's fault and must leave
// the document's budget alone (DESIGN.md: health gates).
type Outcome int

const (
	// OutcomeIndexed: chunked, embedded, stored, state=indexed.
	OutcomeIndexed Outcome = iota
	// OutcomeFailed: the content cannot be indexed and re-trying it
	// unchanged would fail identically. Already recorded as state=failed.
	OutcomeFailed
	// OutcomeSkipped: unreadable right now — vanished between scan and
	// pipeline, or permission denied (TCC). The row is untouched.
	OutcomeSkipped
	// OutcomeTransient: an external service failed. Nothing is recorded;
	// the caller decides between retrying with backoff and treating it as
	// an outage, which is a judgement only the caller can make.
	OutcomeTransient
	// OutcomeSuperseded: the document moved on while it was being worked on
	// — its file was deleted and the row purged, or the file was saved again
	// and the row reset to `discovered` with fresh chunks. Nothing is wrong
	// and nothing is owed: whatever superseded this pass already left the
	// catalog in the right state, and the next pass sees it.
	//
	// Distinct from Skipped because the two mean opposite things to the
	// caller. Skipped is "this file could not be read", which in bulk is how
	// a missing Full Disk Access grant announces itself; treating a routine
	// save-during-index as that would have `bsearch status` blame
	// permissions for a working daemon.
	OutcomeSuperseded
)

// Result is one document's outcome and, when there is one, the cause.
type Result struct {
	Outcome Outcome
	// Err is why, for every outcome except Indexed. It is not a failure of
	// the call — ProcessDocument's own error return is.
	Err error
	// Warnings counts oversized-atomic-block chunk warnings.
	Warnings int
}

// Run processes docs (typically DocumentStore.ListIndexable output): stale
// or unfinished documents are read, chunked, embedded, and stored; documents
// already indexed with current stage versions are skipped, as are documents
// that already failed under the current stage versions (a config change
// gives failed documents a fresh attempt — previously-failed docs are not
// re-counted in Summary). A fully up-to-date corpus makes zero network
// calls.
//
// The returned error is an abort — context cancellation, a store failure,
// or a transient embed failure (endpoint down). Partial progress is durable
// either way: every completed document is committed, and an aborted one is
// left in state=chunked to resume on the next run. Per-document permanent
// problems never abort; they are counted in Summary.Failed. Files that
// cannot be read right now (vanished between scan and pipeline, permission
// denied) are counted in Summary.Skipped and left untouched — the cause is
// environmental, not the content's fault, so the document must not be
// burned (it retries on the next run).
func (ix *Indexer) Run(ctx context.Context, docs []domain.Document) (Summary, error) {
	var sum Summary

	// First pass, before dims are known: split on state + chunker version +
	// embedding fingerprint. Docs that pass re-check against dims below.
	var work, current []domain.Document
	for _, doc := range docs {
		switch {
		case doc.State == domain.DocStateFailed && ix.versionsCurrent(doc):
			// Still failed under the exact config that failed it — a fresh
			// attempt would fail identically. A config change (different
			// fingerprint) or a file change (discovery resets state) gives
			// it a new attempt. Not counted: it is not this run's failure.
		case doc.State == domain.DocStateIndexed && ix.versionsCurrent(doc):
			current = append(current, doc)
		default:
			work = append(work, doc)
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
	// strand "up to date" documents outside the generation search now uses.
	sv := ix.StageVersions(dims)
	for _, doc := range current {
		if doc.StageVersions[domain.StageEmbeddingDims] != sv[domain.StageEmbeddingDims] {
			work = append(work, doc)
			continue
		}
		sum.UpToDate++
	}

	for _, doc := range work {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		res, err := ix.ProcessDocument(ctx, doc, sv)
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
		case OutcomeSuperseded:
			// Nothing to count: the document this run was working on is not
			// the document in the catalog any more, and whatever replaced it
			// is already queued.
		case OutcomeTransient:
			// One-shot has nowhere to put a retry, so endpoint trouble ends
			// the run. The document stays chunked and resumes next time;
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

// StageVersions is what documents processed right now are stamped with, and
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

// versionsCurrent reports whether doc's derived data was produced by this
// chunker version, this embedding spec, and the current vector metric, dims
// aside (DESIGN.md: Pipeline metadata — StageVersions diffed against
// current config). Dims are only known after the probe and are re-checked
// separately in Run.
func (ix *Indexer) versionsCurrent(doc domain.Document) bool {
	return doc.StageVersions[domain.StageChunker] == chunker.Version &&
		doc.StageVersions[domain.StageEmbedding] == ix.fp &&
		doc.StageVersions[domain.StageVecMetric] == domain.VectorMetric
}

// ProcessDocument runs one document through read → chunk → embed → store and
// classifies what happened. sv comes from StageVersions.
//
// The returned error is reserved for failures of the machinery itself — a
// store write that didn't land, a cancelled context — and every caller should
// treat it as fatal. Everything that is a fact about this one document, up to
// and including "the embedding server is down", arrives as a Result, because
// only the caller knows whether the right response is to stop, to retry
// later, or to move on to the next document.
func (ix *Indexer) ProcessDocument(ctx context.Context, doc domain.Document, sv map[string]string) (Result, error) {
	// Merge, don't replace: stage keys owned by other stages (converter,
	// summarizer — later milestones) must survive a re-index, or partial
	// rebuild decisions lose their inputs.
	merged := make(map[string]string, len(doc.StageVersions)+len(sv))
	for k, v := range doc.StageVersions {
		merged[k] = v
	}
	for k, v := range sv {
		merged[k] = v
	}
	doc.StageVersions = merged

	raw, err := os.ReadFile(doc.Path)
	if err != nil {
		// Environmental, not the content's fault: the file vanished after
		// the scan, or reading is denied (TCC). Leave the document alone —
		// it retries next run, and granting access or restoring the file
		// needs no content change to take effect.
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(ix.opts.Progress, "skipped %s: file no longer exists\n", doc.Path)
		} else {
			fmt.Fprintf(ix.opts.Progress, "skipped %s: %v (will retry next run)\n", doc.Path, err)
		}
		return Result{Outcome: OutcomeSkipped, Err: err}, nil
	}
	text, err := chunker.Normalize(raw)
	if err != nil {
		// Undecodable — permanent until the file changes (DESIGN.md:
		// Chunking/Encoding).
		return ix.fail(ctx, doc, fmt.Sprintf("normalize: %v", err))
	}

	res := chunker.Chunk(doc.ID, text, ix.spec.CeilingTokens)
	out := Result{Outcome: OutcomeIndexed, Warnings: len(res.Warnings)}
	for _, w := range res.Warnings {
		// Path, ordinal, and reason only — the heading path is document
		// content and must stay out of default output (DESIGN.md: Privacy).
		fmt.Fprintf(ix.opts.Progress, "warning: %s: %s (chunk %d)\n", doc.Path, w.Reason, w.Ordinal)
	}

	// Short write transaction before any network call (DESIGN.md:
	// transactions never wrap network calls).
	doc.State = domain.DocStateChunked
	chunkIDs, err := ix.opts.Store.UpsertDocument(ctx, doc, res.Chunks)
	if err != nil {
		if gone, r := ix.movedOn(doc, err); gone {
			return r, nil
		}
		return Result{}, fmt.Errorf("store %s: %w", doc.Path, err)
	}

	if len(res.Chunks) > 0 {
		vectors, err := ix.opts.Embedder.EmbedPassages(ctx, res.Chunks)
		if err != nil {
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			if ix.opts.Transient(err) {
				// Endpoint trouble, not the document's fault. The chunks are
				// already committed, so the document resumes from chunked
				// whenever the caller comes back to it — nothing is burned
				// and nothing is redone.
				out.Outcome = OutcomeTransient
				out.Err = fmt.Errorf("embed %s: %w", doc.Path, err)
				return out, nil
			}
			return ix.failWith(ctx, doc, out, fmt.Sprintf("embed: %v", err))
		}
		if err := ix.opts.Vectors.UpsertVectors(ctx, chunkIDs, vectors); err != nil {
			// Fatal, with no "moved on" arm any more. This used to be the
			// widest window in the pipeline — an embed is seconds of network,
			// and a concurrent reconcile could delete the chunk IDs inside it
			// — but nothing writes the catalog while this goroutine is here
			// (ADR 0014). The store still checks those IDs are live, and if
			// that check ever fires it is a broken invariant rather than a
			// file that changed, so it must stop the drain.
			return Result{}, fmt.Errorf("store vectors for %s: %w", doc.Path, err)
		}
	}

	if err := ix.opts.Store.UpdateDocumentState(ctx, doc.ID,
		domain.DocStateChunked, domain.DocStateIndexed); err != nil {
		if gone, r := ix.movedOn(doc, err); gone {
			return r, nil
		}
		return Result{}, fmt.Errorf("finalize %s: %w", doc.Path, err)
	}
	fmt.Fprintf(ix.opts.Progress, "indexed %s (%d chunks)\n", doc.Path, len(res.Chunks))
	return out, nil
}

// movedOn classifies a store error as "the document was deleted while we were
// working on it" (ErrDocumentGone), which is an outcome rather than a fault.
//
// Not failed: failed is a claim about the document's content that would sit
// in `bsearch status` under a reason, and the content this pass was making
// claims about is not in the catalog any more.
//
// One case, since ADR 0014. A purge between documents is ordinary — the
// scheduler drains reconciles there, so a row in the batch it claimed really
// can be gone by the time the batch reaches it — and nothing is owed, because
// the row is gone. Every *other* way a write can miss its row is a broken
// invariant now: the scheduler re-reads each row immediately before working on
// it and nothing writes the catalog while it does. Those return false from
// here and are fatal at the call site rather than being stood down from.
func (ix *Indexer) movedOn(doc domain.Document, err error) (bool, Result) {
	if !errors.Is(err, domain.ErrDocumentGone) {
		return false, Result{}
	}
	fmt.Fprintf(ix.opts.Progress, "skipped %s: deleted while it was being indexed\n", doc.Path)
	return true, Result{Outcome: OutcomeSuperseded, Err: err}
}

// fail records a permanent failure for a document that has no Result in
// flight yet.
func (ix *Indexer) fail(ctx context.Context, doc domain.Document, reason string) (Result, error) {
	return ix.failWith(ctx, doc, Result{}, reason)
}

// failWith marks doc permanently failed, preserving any warnings already
// counted. The document row is first upserted with the current stage versions
// (and its chunks cleared — failed content must not serve stale chunks), so a
// later config change is detectable as a fingerprint mismatch and re-attempts
// the doc. Only a store error (or cancellation) is returned as an error.
func (ix *Indexer) failWith(ctx context.Context, doc domain.Document, out Result, reason string) (Result, error) {
	doc.State = domain.DocStateFailed
	if _, err := ix.opts.Store.UpsertDocument(ctx, doc, nil); err != nil {
		if gone, r := ix.movedOn(doc, err); gone {
			return r, nil
		}
		return Result{}, fmt.Errorf("record failure for %s: %w", doc.Path, err)
	}
	if err := ix.opts.Store.MarkFailed(ctx, doc.ID, reason); err != nil {
		if gone, r := ix.movedOn(doc, err); gone {
			return r, nil
		}
		return Result{}, err
	}
	fmt.Fprintf(ix.opts.Progress, "failed %s: %s\n", doc.Path, reason)
	out.Outcome = OutcomeFailed
	out.Err = errors.New(reason)
	return out, nil
}

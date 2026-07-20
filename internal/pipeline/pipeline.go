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

	// Fail fast before touching any document: one tiny query embedding
	// proves the endpoint is up and serving the configured model, and its
	// length is the dimension count vec0 fixes at CREATE.
	probe, err := ix.opts.Embedder.EmbedQuery(ctx, "bsearch dimension probe")
	if err != nil {
		return sum, fmt.Errorf("embedding endpoint check failed (is the inference server running and serving %q?): %w",
			ix.spec.Model, err)
	}
	if len(probe) == 0 {
		return sum, fmt.Errorf("embedding endpoint returned a zero-dimension vector for model %q", ix.spec.Model)
	}
	dims := len(probe)
	if err := ix.opts.Vectors.EnsureVecTable(ctx, ix.spec, dims); err != nil {
		return sum, err
	}

	// Second pass: the vector-table generation identity includes dims, so a
	// server-side dims change under an unchanged model name would otherwise
	// strand "up to date" documents outside the generation search now uses.
	sv := map[string]string{
		domain.StageChunker:       chunker.Version,
		domain.StageEmbedding:     ix.fp,
		domain.StageEmbeddingDims: strconv.Itoa(dims),
	}
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
		if err := ix.processDoc(ctx, doc, sv, &sum); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

// versionsCurrent reports whether doc's derived data was produced by this
// chunker version and this embedding spec, dims aside (DESIGN.md: Pipeline
// metadata — StageVersions diffed against current config). Dims are only
// known after the probe and are re-checked separately in Run.
func (ix *Indexer) versionsCurrent(doc domain.Document) bool {
	return doc.StageVersions[domain.StageChunker] == chunker.Version &&
		doc.StageVersions[domain.StageEmbedding] == ix.fp
}

// processDoc runs one document through read → chunk → embed → store. A
// non-nil return aborts the whole run; per-document permanent failures are
// recorded via fail and return nil.
func (ix *Indexer) processDoc(ctx context.Context, doc domain.Document, sv map[string]string, sum *Summary) error {
	doc.StageVersions = sv

	raw, err := os.ReadFile(doc.Path)
	if err != nil {
		// Environmental, not the content's fault: the file vanished after
		// the scan, or reading is denied (TCC). Leave the document alone —
		// it retries next run, and granting access or restoring the file
		// needs no content change to take effect.
		sum.Skipped++
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(ix.opts.Progress, "skipped %s: file no longer exists\n", doc.Path)
		} else {
			fmt.Fprintf(ix.opts.Progress, "skipped %s: %v (will retry next run)\n", doc.Path, err)
		}
		return nil
	}
	text, err := chunker.Normalize(raw)
	if err != nil {
		// Undecodable — permanent until the file changes (DESIGN.md:
		// Chunking/Encoding).
		return ix.fail(ctx, doc, sum, fmt.Sprintf("normalize: %v", err))
	}

	res := chunker.Chunk(doc.ID, text, ix.spec.CeilingTokens)
	for _, w := range res.Warnings {
		sum.Warnings++
		// Path, ordinal, and reason only — the heading path is document
		// content and must stay out of default output (DESIGN.md: Privacy).
		fmt.Fprintf(ix.opts.Progress, "warning: %s: %s (chunk %d)\n", doc.Path, w.Reason, w.Ordinal)
	}

	// Short write transaction before any network call (DESIGN.md:
	// transactions never wrap network calls).
	doc.State = domain.DocStateChunked
	chunkIDs, err := ix.opts.Store.UpsertDocument(ctx, doc, res.Chunks)
	if err != nil {
		return fmt.Errorf("store %s: %w", doc.Path, err)
	}

	if len(res.Chunks) > 0 {
		vectors, err := ix.opts.Embedder.EmbedPassages(ctx, res.Chunks)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if ix.opts.Transient(err) {
				// Endpoint trouble, not the document's fault: abort, burn
				// nothing — the doc resumes from chunked next run.
				return fmt.Errorf("embed %s: %w", doc.Path, err)
			}
			return ix.fail(ctx, doc, sum, fmt.Sprintf("embed: %v", err))
		}
		if err := ix.opts.Vectors.UpsertVectors(ctx, chunkIDs, vectors); err != nil {
			return fmt.Errorf("store vectors for %s: %w", doc.Path, err)
		}
	}

	if err := ix.opts.Store.UpdateDocumentState(ctx, doc.ID, domain.DocStateIndexed); err != nil {
		return err
	}
	sum.Indexed++
	fmt.Fprintf(ix.opts.Progress, "indexed %s (%d chunks)\n", doc.Path, len(res.Chunks))
	return nil
}

// fail marks doc permanently failed and keeps the run going. The document
// row is first upserted with the current stage versions (and its chunks
// cleared — failed content must not serve stale chunks), so a later config
// change is detectable as a fingerprint mismatch and re-attempts the doc.
// Only a store error (or cancellation) aborts.
func (ix *Indexer) fail(ctx context.Context, doc domain.Document, sum *Summary, reason string) error {
	doc.State = domain.DocStateFailed
	if _, err := ix.opts.Store.UpsertDocument(ctx, doc, nil); err != nil {
		return fmt.Errorf("record failure for %s: %w", doc.Path, err)
	}
	if err := ix.opts.Store.MarkFailed(ctx, doc.ID, reason); err != nil {
		return err
	}
	sum.Failed++
	fmt.Fprintf(ix.opts.Progress, "failed %s: %s\n", doc.Path, reason)
	return nil
}

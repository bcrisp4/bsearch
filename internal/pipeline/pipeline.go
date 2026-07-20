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
	"os"

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
	return &Indexer{opts: opts, spec: opts.Embedder.Spec()}, nil
}

// Summary is one Run's outcome counts.
type Summary struct {
	Indexed  int // processed to state=indexed this run
	UpToDate int // skipped: already indexed with current stage versions
	Failed   int // marked failed (unreadable, undecodable, poison embed input)
	Warnings int // oversized-atomic-block chunk warnings
}

// Run processes docs (typically DocumentStore.ListIndexable output): stale
// or unfinished documents are read, chunked, embedded, and stored; documents
// already indexed with current stage versions are skipped. A fully
// up-to-date corpus makes zero network calls.
//
// The returned error is an abort — context cancellation, a store failure,
// or a transient embed failure (endpoint down). Partial progress is durable
// either way: every completed document is committed, and an aborted one is
// left in state=chunked to resume on the next run. Per-document permanent
// problems never abort; they are counted in Summary.Failed.
func (ix *Indexer) Run(ctx context.Context, docs []domain.Document) (Summary, error) {
	var sum Summary

	var work []domain.Document
	for _, doc := range docs {
		if ix.upToDate(doc) {
			sum.UpToDate++
			continue
		}
		work = append(work, doc)
	}
	if len(work) == 0 {
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
	if err := ix.opts.Vectors.EnsureVecTable(ctx, ix.spec, len(probe)); err != nil {
		return sum, err
	}

	for _, doc := range work {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		if err := ix.processDoc(ctx, doc, &sum); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

// upToDate reports whether doc's derived data is current: fully indexed by
// this chunker version and this embedding spec (DESIGN.md: Pipeline
// metadata — StageVersions diffed against current config).
func (ix *Indexer) upToDate(doc domain.Document) bool {
	return doc.State == domain.DocStateIndexed &&
		doc.StageVersions["chunker"] == chunker.Version &&
		doc.StageVersions["embedding"] == ix.spec.Fingerprint()
}

// processDoc runs one document through read → chunk → embed → store. A
// non-nil return aborts the whole run; per-document permanent failures are
// recorded via MarkFailed and return nil.
func (ix *Indexer) processDoc(ctx context.Context, doc domain.Document, sum *Summary) error {
	raw, err := os.ReadFile(doc.Path)
	if err != nil {
		// Vanished or unreadable since the scan.
		return ix.fail(ctx, doc, sum, fmt.Sprintf("read: %v", err))
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
		fmt.Fprintf(ix.opts.Progress, "warning: %s: %s (chunk %d, %q)\n", doc.Path, w.Reason, w.Ordinal, w.HeadingPath)
	}

	// Short write transaction before any network call (DESIGN.md:
	// transactions never wrap network calls).
	doc.State = domain.DocStateChunked
	doc.StageVersions = map[string]string{
		"chunker":   chunker.Version,
		"embedding": ix.spec.Fingerprint(),
	}
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

// fail marks doc permanently failed and keeps the run going. Only a store
// error (or cancellation) aborts.
func (ix *Indexer) fail(ctx context.Context, doc domain.Document, sum *Summary, reason string) error {
	if err := ix.opts.Store.MarkFailed(ctx, doc.ID, reason); err != nil {
		return err
	}
	sum.Failed++
	fmt.Fprintf(ix.opts.Progress, "failed %s: %s\n", doc.Path, reason)
	return nil
}

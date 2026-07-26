package domain

import (
	"context"
	"errors"
	"time"
)

// Ports for milestones M1–M3.5. Converter and Summarizer ports land with
// their own issues (#21, #18).
//
// Naming: DESIGN.md's *Port suffix is conceptual. In code the ports are
// this file's interfaces, with plain Go names (domain.Embedder is
// DESIGN.md's EmbedderPort).
//
// The catalog has two subjects and two owners (ADR 0015): discovery writes
// documents (and creates content rows); the pipeline mutates content and
// writes chunks. ADR 0014 puts every catalog write on one goroutine, so
// these are two modules called by the same worker — the split is a module
// boundary, not a concurrency one.

// Embedder turns text into vectors. Implementations embed the output of
// EmbeddingSpec.ComposeQuery/ComposePassage — templates and breadcrumbs
// are applied identically at index and query time, never by callers.
type Embedder interface {
	// EmbedQuery embeds one search query with the model's query template.
	EmbedQuery(ctx context.Context, query string) ([]float32, error)
	// EmbedPassages embeds chunks for indexing, applying the passage
	// template and each chunk's HeadingPath breadcrumb. Only Text and
	// HeadingPath are read. The result is index-aligned with chunks.
	EmbedPassages(ctx context.Context, chunks []Chunk) ([][]float32, error)
	// Spec reports the identity recorded in pipeline metadata.
	Spec() EmbeddingSpec
}

// ErrContentGone means the content row a write or re-read was aimed at is
// not there — swept as an orphan, or never created.
//
// It exists because discovery is the only thing that creates content rows,
// and the pipeline works on rows it claimed some seconds earlier. Those
// seconds are enough for every path referencing the content to vanish and
// the sweep to collect the row, and a write that quietly re-created it would
// make deleted content permanently searchable — the index would keep serving
// bytes whose every source is gone, with nothing to notice it (DESIGN.md:
// Data retention). So a write against a missing row reports the fact
// instead, and the caller treats the content as what it is: gone, and
// nothing to index.
var ErrContentGone = errors.New("the content row no longer exists (swept while in flight?)")

// ErrContentSuperseded means chunk rows a vector write keyed on were
// replaced while the write was in flight, so the vectors are aimed at text
// that no longer exists in the shape they were computed from.
//
// It exists because a re-chunk during an embed would leave vectors keying on
// chunk IDs that had since been deleted. Writing them anyway would attach one
// version's vectors to another version's text, so the store reports instead
// and the caller stands down.
//
// Since ADR 0014 that race cannot happen: one goroutine owns every catalog
// write, and it is inside the pipeline for the whole window. The check that
// produces this is kept anyway — an orphaned vector row is unreachable by
// every delete in the store and silently displaces a real search hit, so it
// is the one guard here that prevents rather than reports. Reaching it means
// the single-writer invariant broke, and the scheduler treats it as exactly
// that.
var ErrContentSuperseded = errors.New("the chunk rows were replaced while this write was in flight")

// DocumentStore is discovery's port: the documents table, and the eager
// creation of content rows. A documents row carries no pipeline state and no
// chunks, so the whole write surface is one batch upsert.
type DocumentStore interface {
	// GetByPath fetches the catalog row for a path; ok is false when the
	// path has never been stored. Cheap change detection (hash/size/mtime).
	GetByPath(ctx context.Context, path string) (doc Document, ok bool, err error)
	// UpsertDocuments writes the batch in one transaction (#34). For each
	// doc with a ContentHash, the content row is created at discovered if
	// absent — the eager insert that keeps dispatch a plain predicate over
	// content rather than an anti-join (ADR 0015). Existing content rows are
	// never touched: a second identical file schedules no work.
	//
	// An upsert on an existing path replaces the row wholesale: rename,
	// edit, unread→readable and readable→unread are all this one call.
	// Unread docs record the reason with no hash.
	UpsertDocuments(ctx context.Context, docs []Document) error
	// DeleteByPathPrefix removes the document at dir and every document
	// under it, returning how many went. Deletion follows the source
	// (DESIGN.md: Data retention), and a vanished path does not say whether
	// it was a file or a directory — so one call answers both.
	//
	// Documents only: content the deleted rows referenced is collected by
	// the orphan sweep, and search's inner join stops serving it the moment
	// the documents row goes — deletion never waits on the sweep.
	DeleteByPathPrefix(ctx context.Context, dir string) (int, error)
}

// ContentStore is the pipeline's port: everything keyed by content hash.
type ContentStore interface {
	// StoreChunks replaces c.Hash's chunks and records c's state and stage
	// versions in one transaction, returning the storage IDs of the new
	// chunks in ordinal order (vector upserts key on them). Old vectors are
	// deleted — every generation — while the old chunk IDs still resolve.
	// Retry columns are untouched.
	//
	// It never creates the content row: discovery creates rows at
	// discovered, and a missing row reports ErrContentGone rather than
	// resurrecting content whose every source is gone.
	StoreChunks(ctx context.Context, c Content, chunks []Chunk) ([]int64, error)
	// UpdateContentState flips state (and updated_at) only — never chunks,
	// vectors, stage versions, or retry columns. StoreChunks cannot serve
	// this: it replaces chunks wholesale and deletes their vectors.
	UpdateContentState(ctx context.Context, hash string, state ContentState) error
	// MarkFailed sets state=failed and records the reason in last_error.
	// Permanent by construction: content is immutable, so nothing resets
	// it — a changed file is a different content row, and a config change
	// re-queues it via ResetStale, which is a different event.
	MarkFailed(ctx context.Context, hash, reason string) error
}

// Queue is the dispatch half of the catalog: which content the scheduler
// should work on next, and how a failed attempt is recorded so the next pass
// treats it correctly (DESIGN.md: Indexing pipeline and queue).
//
// There is no claim method and no claimed state. One daemon runs one indexing
// worker, and every pipeline stage is an idempotent upsert, so a crash
// mid-content is redone rather than recovered (ADR 0011). That is why
// ClaimBatch is a read: nothing is reserved, so nothing has to be released.
type Queue interface {
	// ClaimBatch returns up to limit content rows due for work now:
	// non-terminal state, and next_retry_at either unset or not in the
	// future relative to now. Ordering is recency-first with aging, so a
	// file saved a moment ago is worked before a backlog without the
	// backlog ever starving.
	ClaimBatch(ctx context.Context, now time.Time, limit int) ([]Content, error)
	// GetWork re-reads one claimed row immediately before working it and
	// picks the path the pipeline should read bytes from: the referencing
	// document with the newest mtime, tie-broken by path ascending — the
	// same rule that picks a search hit's primary path, so "which path does
	// bsearch mean by this content" has one answer. A batch is read once
	// and worked through over the following minutes, so a claimed copy can
	// be stale by the time its turn comes (ADR 0014).
	//
	// ErrContentGone when the row was swept, or when no live path
	// references it — deleted while in flight; the sweep will collect it.
	//
	// The chosen path can still change under the pipeline after this read;
	// the pipeline hashes what it actually read and abandons on mismatch,
	// so a wrong pick costs one file read, never a wrong index entry.
	GetWork(ctx context.Context, hash string) (WorkItem, error)
	// Reschedule records a transient failure: attempts, the next due time,
	// and the reason. State is left alone — the content keeps whatever
	// progress it made, and the next pass resumes from there.
	Reschedule(ctx context.Context, hash string, attempts int, at time.Time, reason string) error
	// ResetStale moves content whose derived data was produced by stage
	// versions other than current back to discovered, clearing the retry
	// columns, and reports how many moved. It is what makes a model or
	// chunker change re-embed the corpus with no command run: the dispatch
	// predicate skips terminal states, so without this a stale index would
	// serve forever (DESIGN.md: Pipeline metadata — partial rebuilds).
	// It applies to failed rows too — a chunker or model change is a
	// legitimate fresh start for content that failed under the old config.
	//
	// current is keyed by the Stage* constants. Keys absent from current are
	// ignored, so a stage that has not run yet (conversion, summaries) never
	// makes every content look stale.
	ResetStale(ctx context.Context, current map[string]string) (int, error)
}

// Watcher reports filesystem changes under a set of roots as they happen —
// DESIGN.md's WatcherPort, and the reason a saved file is searchable in
// seconds rather than at the next walk. The periodic scan remains the
// backstop, which is what makes every degradation below a latency cost
// rather than a correctness one.
type Watcher interface {
	// Watch subscribes to changes under roots and delivers them until ctx is
	// done. An error means no subscription was made at all (an unsupported
	// platform, a rejected root); the caller keeps working from the periodic
	// scan.
	//
	// Closing the channel is the only way an implementation can say its
	// stream is gone, and the caller's fallback to scan-only depends on it:
	// close on ctx cancellation, and close if the stream dies underneath
	// you. An implementation that cannot detect the second case leaves the
	// caller believing it is still being watched — which is the state issue
	// #65 is about.
	Watch(ctx context.Context, roots []string) (<-chan WatchBatch, error)
}

// WatchBatch is one delivery from a Watcher.
//
// Batches rather than single events, because the coalescing is what makes a
// rename resolvable: a move arrives as the old path and the new path
// together, and only a caller holding both can recognise it as one document
// rather than a deletion and a creation.
type WatchBatch struct {
	// Paths are absolute paths whose content or existence may have changed.
	// "May": a watcher is allowed to be imprecise in this direction, and the
	// caller confirms with a stat and a hash.
	Paths []string
	// Rescan means Paths cannot be trusted to be the whole story — the event
	// stream overflowed, a volume appeared or went away, or a watch root was
	// replaced underneath us. The honest response is a full walk, so a
	// Rescan batch is a request for one and its Paths are advisory.
	Rescan bool
}

// Hit is one KNN result row: the matching chunk, one document referencing
// the chunk's content, and the raw distance (model-dependent and
// uncalibrated — DESIGN.md: no score floor).
//
// One row per (chunk, referencing path): a chunk whose content lives at N
// paths arrives N times, consecutively, ordered mtime-descending then path
// ascending. CollapseBestPerContent reduces the fan-out to one ContentHit
// per content. Doc.ContentHash is the chunk's content hash (the join key),
// and (Doc.ContentHash, Chunk.Ordinal) identifies the chunk.
type Hit struct {
	Doc      Document
	Chunk    Chunk
	Distance float64
}

// ErrNoVecTable means there is no current vector table: nothing has been
// embedded yet, or the generation search was using was superseded mid-flight
// by an indexer running in another process. It lives here, not in the storage
// adapter, because the query path must recognise it without importing an
// adapter — "nothing to search right now" is a port-level outcome, not a
// SQLite detail.
var ErrNoVecTable = errors.New("no current vector table (nothing embedded yet?)")

// VectorStore persists chunk embeddings and serves KNN search. A search can
// only use one embedding model, so the store tracks a current vector table
// per model+dims (DESIGN.md: Pipeline metadata and model migration).
type VectorStore interface {
	// EnsureVecTable makes a vector table for spec+dims the current one,
	// creating a new generation if none matches. Model, dims, and prefix
	// templates are the identity: differently-prefixed vectors are as
	// incompatible as a different model's. The input ceiling is recorded
	// but excluded — it shapes chunk boundaries, not vectors. Dims come
	// from the first embedding batch — vec0 fixes them at CREATE.
	EnsureVecTable(ctx context.Context, spec EmbeddingSpec, dims int) error
	// UpsertVectors stores one vector per chunk storage ID (from
	// ContentStore.StoreChunks), replacing any existing rows. A chunk ID
	// that no longer exists means the content was re-chunked during the
	// embed, and reports ErrContentSuperseded rather than orphaning vectors.
	UpsertVectors(ctx context.Context, chunkIDs []int64, vectors [][]float32) error
	// SearchVectors returns the limit nearest chunks by ascending distance,
	// fanned out per referencing path (see Hit). limit counts chunks, not
	// rows. Loud error when no current vec table exists (nothing embedded
	// yet or model mismatch) — never a silent empty result.
	SearchVectors(ctx context.Context, query []float32, limit int) ([]Hit, error)
}

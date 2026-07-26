package domain

import (
	"context"
	"errors"
	"time"
)

// Ports for milestones M1 and M3. Converter and Summarizer ports land with
// their own issues (#21, #18).
//
// Naming: DESIGN.md's *Port suffix is conceptual. In code the ports are
// this file's interfaces, with plain Go names (domain.Embedder is
// DESIGN.md's EmbedderPort).

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

// ErrDocumentGone means the catalog row a write was aimed at is not there.
//
// It exists because discovery is the only thing that creates catalog rows,
// and the pipeline writes to rows it claimed some seconds earlier. Those
// seconds are enough for the file to be deleted and the watcher's reconcile
// to purge it, and a write that quietly re-created the row would make the
// deleted file permanently searchable — the index would keep serving content
// whose source is gone, with nothing to notice it (DESIGN.md: Data
// retention). So a write against a missing row reports the fact instead, and
// the caller treats the document as what it is: gone, and nothing to index.
var ErrDocumentGone = errors.New("the catalog row no longer exists (deleted while in flight?)")

// DocumentStore persists the catalog: documents and their chunks.
type DocumentStore interface {
	// UpsertDocument writes the document row and replaces its chunks in one
	// transaction, returning the storage IDs of the new chunks in ordinal
	// order (vector upserts key on them).
	//
	// A state of `discovered` creates the row if it is missing — that is
	// discovery announcing a file. Any other state is a pipeline write to a
	// row that is expected to exist, and reports ErrDocumentGone rather than
	// creating one.
	UpsertDocument(ctx context.Context, doc Document, chunks []Chunk) ([]int64, error)
	// GetByPath fetches the catalog row for a path; ok is false when the
	// path has never been stored. Cheap change detection (hash/size/mtime).
	GetByPath(ctx context.Context, path string) (doc Document, ok bool, err error)
	// GetByID fetches one catalog row, reporting ErrDocumentGone when it is
	// not there. The scheduler re-reads a claimed document through this
	// immediately before working on it: a batch is read once and worked
	// through over the following minutes, so by the time a document comes up
	// its copy can name a path the file no longer has (ADR 0014).
	//
	// A missing row is an error here where GetByPath returns ok=false,
	// because the two answer different questions. Discovery asks GetByPath
	// about paths it has never seen, and "never seen" is the normal answer
	// and the reason discovery exists. An id, by contrast, came out of a
	// claim — the row was there, so its absence is a purge, which is exactly
	// what ErrDocumentGone names.
	GetByID(ctx context.Context, docID string) (Document, error)
	// GetByContentHash returns every catalog row with this content hash,
	// for discovery's rename detection (DESIGN.md: doc_id Closed issue).
	GetByContentHash(ctx context.Context, hash string) ([]Document, error)
	// UpdateDocumentStat refreshes size/mtime on an existing row without
	// touching state, chunks, stage versions, or retry columns — for files
	// touched on disk but content-identical.
	UpdateDocumentStat(ctx context.Context, docID string, size int64, mtime time.Time) error
	// DeleteDocument removes the document and everything derived from it
	// (chunks, summaries, vectors).
	DeleteDocument(ctx context.Context, docID string) error
	// DeleteByPathPrefix removes the document at dir and every document
	// under it, returning how many went. Deletion follows the source
	// (DESIGN.md: Data retention), and a vanished path does not say whether
	// it was a file or a directory — so one call answers both.
	DeleteByPathPrefix(ctx context.Context, dir string) (int, error)
	// ListIndexable returns every catalog row the pipeline may need to work
	// on — every state except deleted — ordered by path. Metadata only
	// (no chunk text). Indexed and failed rows are included: whether a row
	// is stale, or failed under stage versions that have since changed and
	// deserves a fresh attempt, is the caller's call (DESIGN.md: Pipeline
	// metadata).
	ListIndexable(ctx context.Context) ([]Document, error)
	// UpdateDocumentState flips state (and updated_at) only — never chunks,
	// vectors, stage versions, or retry columns. UpsertDocument cannot serve
	// this: it replaces chunks wholesale and deletes their vectors.
	//
	// The write lands only if the row is still in state `from`. That guard is
	// an assertion that one goroutine owns every catalog write (ADR 0014),
	// not concurrency control — there is nothing to control, and a reader who
	// takes it for a lock will reconstruct a concurrency story that no longer
	// exists. It is here because DESIGN.md commits to summarization running
	// alongside embedding, and a second writer arriving without it would
	// strand documents as `indexed` with no chunks, silently, exactly as
	// issue #63 described.
	//
	// A row that is gone reports ErrDocumentGone and the caller stands down —
	// a purge landing between documents is legitimate. A row that is present
	// in some other state reports a plain error naming both states, and is
	// never anything but a bug.
	UpdateDocumentState(ctx context.Context, docID string, from, to DocState) error
	// MarkFailed sets state=failed and records the reason in last_error.
	// A subsequent file change resets it (UpsertDocument clears retry
	// columns).
	MarkFailed(ctx context.Context, docID, reason string) error
}

// Queue is the dispatch half of the catalog: which documents the scheduler
// should work on next, and how a failed attempt is recorded so the next pass
// treats it correctly (DESIGN.md: Indexing pipeline and queue).
//
// There is no claim method and no claimed state. One daemon runs one indexing
// worker, and every pipeline stage is an idempotent upsert, so a crash
// mid-document is redone rather than recovered (ADR 0011). That is why
// ClaimBatch is a read: nothing is reserved, so nothing has to be released.
type Queue interface {
	// ClaimBatch returns up to limit documents due for work now: non-terminal
	// state, and next_retry_at either unset or not in the future relative to
	// now. Ordering is recency-first with aging, so a file saved a moment ago
	// is worked before a backlog without the backlog ever starving.
	ClaimBatch(ctx context.Context, now time.Time, limit int) ([]Document, error)
	// Reschedule records a transient failure: attempts, the next due time,
	// and the reason. State is left alone — the document keeps whatever
	// progress it made, and the next pass resumes from there.
	Reschedule(ctx context.Context, docID string, attempts int, at time.Time, reason string) error
	// ResetStale moves documents whose derived data was produced by stage
	// versions other than current back to discovered, clearing the retry
	// columns, and reports how many moved. It is what makes a model or
	// chunker change re-embed the corpus with no command run: the dispatch
	// predicate skips terminal states, so without this a stale index would
	// serve forever (DESIGN.md: Pipeline metadata — partial rebuilds).
	//
	// current is keyed by the Stage* constants. Keys absent from current are
	// ignored, so a stage that has not run yet (conversion, summaries) never
	// makes every document look stale.
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

// Hit is one KNN result: the matching chunk, its document, and the raw
// distance (model-dependent and uncalibrated — DESIGN.md: no score floor).
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
	// DocumentStore.UpsertDocument), replacing any existing rows.
	//
	// A chunk ID that no longer exists is refused rather than written. Under
	// ADR 0014 nothing can delete those chunks between the upsert that minted
	// them and this call, so reaching that error means the single-writer
	// invariant broke — but it is refused rather than merely reported because
	// the resulting vector row would be unreachable by every delete in the
	// store while still displacing real search hits. It is the one check here
	// that prevents rather than reports.
	UpsertVectors(ctx context.Context, chunkIDs []int64, vectors [][]float32) error
	// SearchVectors returns the limit nearest chunks by ascending distance.
	// Loud error when no current vec table exists (nothing embedded yet or
	// model mismatch) — never a silent empty result.
	SearchVectors(ctx context.Context, query []float32, limit int) ([]Hit, error)
}

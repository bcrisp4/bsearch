package domain

import (
	"context"
	"time"
)

// Ports for milestone M1. Converter, Watcher, and Summarizer ports land
// with their own issues (#21, #13, #18).
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

// DocumentStore persists the catalog: documents and their chunks.
type DocumentStore interface {
	// UpsertDocument writes the document row and replaces its chunks in one
	// transaction, returning the storage IDs of the new chunks in ordinal
	// order (vector upserts key on them).
	UpsertDocument(ctx context.Context, doc Document, chunks []Chunk) ([]int64, error)
	// GetByPath fetches the catalog row for a path; ok is false when the
	// path has never been stored. Cheap change detection (hash/size/mtime).
	GetByPath(ctx context.Context, path string) (doc Document, ok bool, err error)
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
}

// Hit is one KNN result: the matching chunk, its document, and the raw
// distance (model-dependent and uncalibrated — DESIGN.md: no score floor).
type Hit struct {
	Doc      Document
	Chunk    Chunk
	Distance float64
}

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
	UpsertVectors(ctx context.Context, chunkIDs []int64, vectors [][]float32) error
	// SearchVectors returns the limit nearest chunks by ascending distance.
	// Loud error when no current vec table exists (nothing embedded yet or
	// model mismatch) — never a silent empty result.
	SearchVectors(ctx context.Context, query []float32, limit int) ([]Hit, error)
}

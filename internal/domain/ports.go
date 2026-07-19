package domain

import "context"

// Ports for milestone M1. Signatures are deliberately minimal and
// provisional: the issue that first implements each port refines it
// (embedder #5, storage #2). Converter, Watcher, and Summarizer ports
// land with their own issues (#21, #13, #18).
//
// Naming: DESIGN.md's *Port suffix is conceptual. In code the ports are
// this file's interfaces, with plain Go names (domain.Embedder is
// DESIGN.md's EmbedderPort).

// Embedder turns text into vectors via an OpenAI-compatible endpoint.
// Query/passage prefix templates (asymmetric models) are the adapter's
// concern and arrive with issue #5 — callers pass raw text.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
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
	// EnsureVecTable makes a vector table for model+dims the current one,
	// creating a new generation if none matches. Dims come from the first
	// embedding batch — vec0 fixes them at CREATE.
	EnsureVecTable(ctx context.Context, model string, dims int) error
	// UpsertVectors stores one vector per chunk storage ID (from
	// DocumentStore.UpsertDocument), replacing any existing rows.
	UpsertVectors(ctx context.Context, chunkIDs []int64, vectors [][]float32) error
	// SearchVectors returns the limit nearest chunks by ascending distance.
	// Loud error when no current vec table exists (nothing embedded yet or
	// model mismatch) — never a silent empty result.
	SearchVectors(ctx context.Context, query []float32, limit int) ([]Hit, error)
}

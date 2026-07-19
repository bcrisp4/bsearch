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
// Issue #2 owns the real shape (states, stage versions, retry columns).
type DocumentStore interface {
	UpsertDocument(ctx context.Context, doc Document, chunks []Chunk) error
	DeleteDocument(ctx context.Context, docID string) error
}

// VectorStore persists chunk embeddings and serves KNN search.
// Issue #2 owns the real shape.
type VectorStore interface {
	UpsertVectors(ctx context.Context, docID string, vectors [][]float32) error
	Search(ctx context.Context, query []float32, limit int) ([]Chunk, error)
}

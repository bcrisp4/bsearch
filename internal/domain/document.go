package domain

import "time"

// DocState is a document's position in the indexing pipeline (DESIGN.md:
// Indexing pipeline and queue). Terminal states are failed and deleted;
// summarization is tracked separately, never a pipeline gate.
type DocState string

const (
	DocStateDiscovered DocState = "discovered"
	DocStateConverted  DocState = "converted"
	DocStateChunked    DocState = "chunked"
	DocStateEmbedded   DocState = "embedded"
	DocStateIndexed    DocState = "indexed"
	DocStateFailed     DocState = "failed"
	DocStateDeleted    DocState = "deleted"
)

// Document is one indexed file. ID is the opaque surrogate doc_id from
// DESIGN.md: minted at first discovery, stable across content edits and
// renames (subject to the rename-detection rules in the design doc).
type Document struct {
	ID          string
	Path        string
	ContentHash string
	Size        int64
	MTime       time.Time
	State       DocState
	// StageVersions records which version of each pipeline stage produced
	// this document's derived data (chunker, embedding model, …), keyed by
	// stage name. Partial rebuilds diff it against current config
	// (DESIGN.md: Pipeline metadata and model migration). Nil = none
	// recorded yet.
	StageVersions map[string]string
}

// Chunk is one embeddable unit of a document's converted markdown
// (DESIGN.md: Chunking). Byte offsets index into the normalized markdown
// (the UTF-8 output of chunker.Normalize — BOM stripped, UTF-16
// transcoded), not the raw file bytes; retrieval must slice that same
// normalized text to return chunk-in-context.
type Chunk struct {
	DocID       string
	Ordinal     int
	Text        string
	HeadingPath string // "Mortgage Renewal 2026 > Offers > Broker A"
	ByteStart   int
	ByteEnd     int
}

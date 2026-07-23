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

// StageVersions keys. These are persisted schema (the stage_versions
// column): every reader and writer must use the constants, never literals —
// a typo'd key compiles, reads as "", and makes every document look stale.
const (
	// StageChunker records chunker.Version.
	StageChunker = "chunker"
	// StageEmbedding records EmbeddingSpec.Fingerprint().
	StageEmbedding = "embedding"
	// StageEmbeddingDims records the embedding dimension count, discovered
	// at run time from the endpoint. Tracked separately from the fingerprint
	// because a server can change dims under an unchanged model name, and
	// the vector-table generation identity includes dims — without this key
	// such a change would strand up-to-date documents outside the new
	// generation.
	StageEmbeddingDims = "embedding_dims"
	// StageVecMetric records the distance metric of the vector table the
	// document was embedded into (VectorMetric). Like dims, metric is part
	// of the vector-table generation identity but outside the embedding
	// fingerprint — without this key a metric change would strand
	// "up-to-date" documents outside the generation search uses. Documents
	// indexed before the key existed read as "" and re-embed (ADR 0007).
	StageVecMetric = "vec_metric"
)

// VectorMetric is the vec0 distance metric for every vector-table
// generation: cosine, so rankings are magnitude-invariant no matter what
// the embedding model emits (ADR 0007). A single constant shared by the
// vector store (table DDL, descriptor identity) and the pipeline (stage
// versioning) — the two must never disagree.
const VectorMetric = "cosine"

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

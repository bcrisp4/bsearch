package domain

import (
	"slices"
	"time"
)

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

// DocStates is every state, in pipeline order. Reporting surfaces enumerate
// it so a state with no documents is reported as an explicit zero rather than
// omitted — a consumer must never have to distinguish "absent" from "none"
// (DESIGN.md: status is observable). Keep in sync with the CHECK constraint
// on documents.state.
var DocStates = []DocState{
	DocStateDiscovered,
	DocStateConverted,
	DocStateChunked,
	DocStateEmbedded,
	DocStateIndexed,
	DocStateFailed,
	DocStateDeleted,
}

// TerminalDocStates are the states the scheduler never dispatches. They are
// the negative half of the dispatch predicate, named once here so the queue's
// SQL, the partial claim index, and Terminal cannot drift apart.
//
// Terminal means "not queue work", not "never changes again": a file change
// resets an indexed or failed row (UpsertDocument clears the retry columns),
// a config change sweeps stale rows back to discovered, and purging deleted
// rows is its own path (DESIGN.md: Dispatch).
var TerminalDocStates = []DocState{
	DocStateIndexed,
	DocStateFailed,
	DocStateDeleted,
}

// Terminal reports whether s is a state the scheduler skips.
func (s DocState) Terminal() bool {
	return slices.Contains(TerminalDocStates, s)
}

// docTransitions is the state machine: which states a document may move to
// from each state.
//
// It documents and asserts the pipeline's shape; it does not enforce it. No
// writer consults ValidTransition — UpsertDocument, UpdateDocumentState and
// MarkFailed will each persist any state from any state — so this is a
// specification the tests hold the code to, not a runtime guard. Making it a
// real invariant means checking it in every writer, which is worth doing when
// there are enough writers to lose track of; today there are three.
//
// The v1 pipeline walks discovered → chunked → indexed. DocStateConverted and
// DocStateEmbedded are declared, accepted by the schema's CHECK constraint,
// and unreachable: conversion needs the bscribe adapter (#21) and there is no
// point writing an embedded row between the vector write and the indexed flip
// while summaries are a fill-later field rather than a gate. Their edges are
// recorded now so the ladder is legible when those stages land.
var docTransitions = map[DocState][]DocState{
	DocStateDiscovered: {DocStateConverted, DocStateChunked, DocStateFailed, DocStateDeleted},
	DocStateConverted:  {DocStateChunked, DocStateFailed, DocStateDeleted},
	DocStateChunked:    {DocStateEmbedded, DocStateIndexed, DocStateFailed, DocStateDeleted},
	DocStateEmbedded:   {DocStateIndexed, DocStateFailed, DocStateDeleted},
	// Terminal states re-enter the pipeline only at discovered: a file change
	// or a stale sweep resets them, never a direct hop to a mid-pipeline state.
	DocStateIndexed: {DocStateDiscovered, DocStateFailed, DocStateDeleted},
	DocStateFailed:  {DocStateDiscovered, DocStateDeleted},
	DocStateDeleted: {DocStateDiscovered},
}

// ValidTransition reports whether from → to is an edge of the pipeline state
// machine. A self-transition is always valid: every stage is an idempotent
// upsert, so redoing one after a crash must not look like an illegal move.
func ValidTransition(from, to DocState) bool {
	if from == to {
		return true
	}
	return slices.Contains(docTransitions[from], to)
}

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

	// Attempts counts failed tries since the last reset. Only transient
	// failures met while the external service was healthy increment it — an
	// outage must never burn a healthy document (DESIGN.md: health gates).
	// UpsertDocument zeroes it, so a file change gives a fresh budget.
	Attempts int
	// NextRetryAt is when the scheduler may dispatch this document again.
	// Zero means "as soon as it comes up", which is the normal case: backoff
	// is the exception, not the rule.
	NextRetryAt time.Time
	// LastError is why the last attempt failed, in the user's terms. Kept
	// after a retry succeeds until the next write clears it — a document
	// that took three goes is worth being able to notice.
	LastError string
}

// QueueDepth is the dispatchable backlog: documents the scheduler would work
// on now, and documents waiting out a retry backoff. Reported by `bsearch
// status`, where the split is the point — a queue draining a backlog and a
// queue whose every remaining document is failing have the same total.
type QueueDepth struct {
	// Pending is non-terminal documents due at the time asked about.
	Pending int
	// Retrying is non-terminal documents deferred by backoff.
	Retrying int
}

// FailureGroup is one reason documents were given up on: how many share it,
// and one path to look at. `bsearch status` reports the largest few rather
// than every failed document — a corpus fails in a handful of ways, and the
// reason plus one example is what makes each of them actionable.
type FailureGroup struct {
	// Reason is the recorded last_error, verbatim. Untrusted display text:
	// it may quote a parser's message about a file's contents.
	Reason string
	// Documents is how many failed documents share this reason.
	Documents int
	// ExamplePath is one of them, so the reason can be reproduced.
	ExamplePath string
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

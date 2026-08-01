package domain

import (
	"slices"
	"time"
)

// ContentState is a content's position in the indexing pipeline (DESIGN.md:
// Indexing pipeline and queue). The state describes processing *content* —
// one distinct byte sequence — never a file (ADR 0015). Terminal states are
// indexed and failed; summarization is tracked separately, never a pipeline
// gate.
//
// There is no deleted state: a deleted file is a removed documents row, and
// content nothing references is collected by the orphan sweep.
//
// The v1 ladder is discovered → chunked → indexed, with failed reachable from
// any non-terminal state. Terminal states re-enter only at discovered and
// only via the stale sweep (ResetStale), never by a direct hop to a
// mid-pipeline state. ContentStateConverted and ContentStateEmbedded are
// declared and accepted by the schema's CHECK, but unreachable: conversion
// needs the bscribe adapter (#21), and there is no point writing an embedded
// row between the vector write and the indexed flip while summaries are a
// fill-later field rather than a gate. They are named now so the ladder is
// legible when those stages land.
//
// Nothing checks a transition. Discovery creates rows at discovered
// (UpsertDocuments), and four writers move them afterwards — StoreChunks,
// UpdateContentState, MarkFailed and ResetStale. Only ResetStale constrains
// where it moves from (WHERE state IN ('indexed','failed')). What keeps the
// ladder legal is that the pipeline has one code path per step, not a guard.
//
// A domain.ValidTransition predicate and an edge map used to live here; #74
// deleted them, unused. ADR 0013 and ADR 0014 name them as the deferred
// remedy for #62 and #63, so this is what those records point at. #69 is why
// nothing replaced them: state is a lifecycle marker, not a version token,
// and the writer such a guard would defend against rewrites state to the same
// value while replacing everything under it. The hazard is a legal-looking
// chunked → chunked, which no transition check can catch.
type ContentState string

const (
	ContentStateDiscovered ContentState = "discovered"
	ContentStateConverted  ContentState = "converted"
	ContentStateChunked    ContentState = "chunked"
	ContentStateEmbedded   ContentState = "embedded"
	ContentStateIndexed    ContentState = "indexed"
	ContentStateFailed     ContentState = "failed"
)

// ContentStates is every state, in pipeline order. Reporting surfaces
// enumerate it so a state with no content is reported as an explicit zero
// rather than omitted — a consumer must never have to distinguish "absent"
// from "none" (DESIGN.md: status is observable). Keep in sync with the CHECK
// constraint on content.state.
var ContentStates = []ContentState{
	ContentStateDiscovered,
	ContentStateConverted,
	ContentStateChunked,
	ContentStateEmbedded,
	ContentStateIndexed,
	ContentStateFailed,
}

// TerminalContentStates are the states the scheduler never dispatches. They
// are the negative half of the dispatch predicate, named once here so the
// queue's SQL, the partial claim index, and Terminal cannot drift apart.
//
// Terminal means "not queue work", not "never changes again": a config
// change sweeps stale rows back to discovered (ResetStale). failed is
// otherwise permanent by construction — content is immutable, so a failure
// against those bytes cannot expire, and a changed file is a *different*
// content row that starts at discovered. Nothing has to reset it.
var TerminalContentStates = []ContentState{
	ContentStateIndexed,
	ContentStateFailed,
}

// Terminal reports whether s is a state the scheduler skips.
func (s ContentState) Terminal() bool {
	return slices.Contains(TerminalContentStates, s)
}

// StageVersions keys. These are persisted schema (the stage_versions
// column): every reader and writer must use the constants, never literals —
// a typo'd key compiles, reads as "", and makes every content look stale.
const (
	// StageChunker records chunker.Version.
	StageChunker = "chunker"
	// StageEmbedding records EmbeddingSpec.Fingerprint().
	StageEmbedding = "embedding"
	// StageEmbeddingDims records the embedding dimension count, discovered
	// at run time from the endpoint. Tracked separately from the fingerprint
	// because a server can change dims under an unchanged model name, and
	// the vector-table generation identity includes dims — without this key
	// such a change would strand up-to-date content outside the new
	// generation.
	StageEmbeddingDims = "embedding_dims"
	// StageVecMetric records the distance metric of the vector table the
	// content was embedded into (VectorMetric). Like dims, metric is part
	// of the vector-table generation identity but outside the embedding
	// fingerprint — without this key a metric change would strand
	// "up-to-date" content outside the generation search uses. Content
	// indexed before the key existed reads as "" and re-embeds (ADR 0007).
	StageVecMetric = "vec_metric"
)

// VectorMetric is the vec0 distance metric for every vector-table
// generation: cosine, so rankings are magnitude-invariant no matter what
// the embedding model emits (ADR 0007). A single constant shared by the
// vector store (table DDL, descriptor identity) and the pipeline (stage
// versioning) — the two must never disagree.
const VectorMetric = "cosine"

// Content is one distinct byte sequence's row: its queue state and retry
// columns. Everything derived from the bytes (chunks, vectors, summaries)
// keys on Hash (ADR 0015). One queue row per distinct content, so a second
// identical file schedules no work at all.
type Content struct {
	Hash  string
	State ContentState
	// StageVersions records which version of each pipeline stage produced
	// this content's derived data (chunker, embedding model, …), keyed by
	// stage name. Partial rebuilds diff it against current config
	// (DESIGN.md: Pipeline metadata and model migration). Nil = none
	// recorded yet.
	StageVersions map[string]string

	// Attempts counts failed tries since the last reset. Only transient
	// failures met while the external service was healthy increment it — an
	// outage must never burn healthy content (DESIGN.md: health gates).
	Attempts int
	// NextRetryAt is when the scheduler may dispatch this content again.
	// Zero means "as soon as it comes up", which is the normal case: backoff
	// is the exception, not the rule.
	NextRetryAt time.Time
	// LastError is why the last attempt failed, in the user's terms. Kept
	// after a retry succeeds until the next write clears it — content that
	// took three goes is worth being able to notice.
	LastError string
}

// WorkItem is one claimed piece of work: the content row plus every path
// holding those bytes, resolved at re-read time (Queue.GetWork). Paths is
// ordered newest mtime first, tie-broken by path ascending — the same rule
// that picks a search hit's primary path — and Path is its head, the
// primary.
//
// All paths ride along because any copy is as good as another: the pipeline
// verifies the bytes it reads against Content.Hash, so it may fall back to
// the next copy when one is unreadable — one unmounted volume or revoked
// grant must not block a perfectly readable duplicate.
type WorkItem struct {
	Content Content
	Path    string
	Paths   []string
}

// SweepScope selects which orphaned content an orphan sweep collects.
//
// The split exists because the two orphan populations cost differently.
// Non-terminal orphans pollute the queue — claimed and Gone-skipped every
// drain — so they are collected eagerly. Terminal orphans cost nothing to
// leave (the partial claim index excludes them) and are precisely the
// expensive-to-rebuild ones: an undo, a `git checkout`, or a rename whose
// halves land in different debounce windows re-references them, and an
// eager collection would turn each of those into a full re-embed. They
// wait for the startup sweep, so the free-restore window is the life of
// the process.
type SweepScope int

const (
	// SweepScopeQueue collects only non-terminal orphans — the ones that
	// would otherwise be claimed and skipped forever.
	SweepScopeQueue SweepScope = iota
	// SweepScopeAll collects every orphan, terminal included. Run at
	// startup: whatever a crash or a previous build left, plus terminal
	// orphans accumulated over the last process's lifetime.
	SweepScopeAll
)

// QueueDepth is the dispatchable backlog: content the scheduler would work
// on now, and content waiting out a retry backoff. Reported by `bsearch
// status`, where the split is the point — a queue draining a backlog and a
// queue whose every remaining content is failing have the same total.
type QueueDepth struct {
	// Pending is non-terminal content due at the time asked about.
	Pending int
	// Retrying is non-terminal content deferred by backoff.
	Retrying int
}

// FailureGroup is one reason content was given up on: how many share it,
// and one path to look at. `bsearch status` reports the largest few rather
// than every failure — a corpus fails in a handful of ways, and the reason
// plus one example is what makes each of them actionable.
type FailureGroup struct {
	// Reason is the recorded last_error, verbatim. Untrusted display text:
	// it may quote a parser's message about a file's contents.
	Reason string
	// Contents is how many distinct failed contents share this reason.
	Contents int
	// ExamplePath is one path holding one of them, so the reason can be
	// reproduced. Empty when the failed content is orphaned (no path
	// references it; the sweep will collect it).
	ExamplePath string
}

// CatalogCounts is the three populations `bsearch status` reports, read in
// one transaction so they reconcile exactly (ADR 0015): Files counts paths,
// Unread counts the paths whose bytes were never obtained by reason, and
// Content counts *distinct contents* by state. Files − sum(Unread) is the
// files that have content, sum(Content) is how many distinct contents those
// are, and the gap between them is what deduplication saved. Reporting only
// one of the three is how a corpus with twelve permission-denied files ends
// up looking healthy.
type CatalogCounts struct {
	Files   int
	Content map[ContentState]int // zero-filled over ContentStates
	Unread  map[UnreadReason]int // zero-filled over UnreadReasons
}

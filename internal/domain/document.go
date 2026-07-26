package domain

import (
	"time"
)

// UnreadReason says why a path's bytes were never obtained (ADR 0015). It is
// a fact about the file, not a pipeline state: a document with no content
// cannot be processing. The reason is recorded because a TCC denial and a
// deliberately-skipped iCloud placeholder are opposite situations — one is
// broken, the other is working — and must never report as one number
// (DESIGN.md: Identity).
type UnreadReason string

const (
	// UnreadDenied is a permission error (TCC/EPERM) — broken; the fix is
	// granting Full Disk Access.
	UnreadDenied UnreadReason = "denied"
	// UnreadDataless is an iCloud Optimize-Storage placeholder, skipped by
	// design — indexing must never trigger cloud downloads.
	UnreadDataless UnreadReason = "dataless"
	// UnreadIOError is any other read failure.
	UnreadIOError UnreadReason = "io_error"
)

// UnreadReasons is every reason. Reporting surfaces enumerate it so a reason
// with no documents is reported as an explicit zero rather than omitted — a
// consumer must never have to distinguish "absent" from "none". Keep in sync
// with the CHECK constraint on documents.unread_reason.
var UnreadReasons = []UnreadReason{
	UnreadDenied,
	UnreadDataless,
	UnreadIOError,
}

// Document mirrors one file on disk. Path is the identity — there is no
// doc_id (ADR 0015): a path is what an agent can act on, and the index is
// wholly derived data with nothing to keep continuous across a rebuild.
//
// Exactly one of ContentHash / UnreadReason is set ("" is the Go spelling of
// the schema's NULL, and the schema's CHECK enforces the exclusion). A file
// we read always has a hash, however unpromising — an empty file hashes to
// sha256("") — so a zero ContentHash always means the bytes were never
// obtained and UnreadReason says why.
type Document struct {
	Path         string
	ContentHash  string
	UnreadReason UnreadReason
	Size         int64
	MTime        time.Time
}

// Chunk is one embeddable unit of a content's converted markdown
// (DESIGN.md: Chunking). Byte offsets index into the normalized markdown
// (the UTF-8 output of chunker.Normalize — BOM stripped, UTF-16
// transcoded), not the raw file bytes; retrieval must slice that same
// normalized text to return chunk-in-context.
//
// A chunk belongs to a content hash, not a file; the hash is supplied at
// write time (ContentStore.StoreChunks), never carried per chunk.
type Chunk struct {
	Ordinal     int
	Text        string
	HeadingPath string // "Mortgage Renewal 2026 > Offers > Broker A"
	ByteStart   int
	ByteEnd     int
}

package domain

import "time"

// Document is one indexed file. ID is the opaque surrogate doc_id from
// DESIGN.md: minted at first discovery, stable across content edits and
// renames (subject to the rename-detection rules in the design doc).
type Document struct {
	ID          string
	Path        string
	ContentHash string
	Size        int64
	MTime       time.Time
}

// Chunk is one embeddable unit of a document's converted markdown
// (DESIGN.md: Chunking). Byte offsets index into the converted markdown,
// letting retrieval return chunk-in-context.
type Chunk struct {
	DocID       string
	Ordinal     int
	Text        string
	HeadingPath string // "Mortgage Renewal 2026 > Offers > Broker A"
	ByteStart   int
	ByteEnd     int
}

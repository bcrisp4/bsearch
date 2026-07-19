// Package adapters is the parent for bsearch's driven adapters — the
// concrete implementations of internal/domain ports. Each adapter is a
// subpackage that imports domain (never the reverse): sqlite (storage,
// issue #2), openai (embedder/summarizer, issue #5), bscribe (converter,
// issue #21), fsevents (watcher, issue #13).
package adapters

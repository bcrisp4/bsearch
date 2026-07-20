// Package chunker splits converted markdown into embeddable chunks
// (DESIGN.md: Chunking). It is a set of pure functions, not a port: only
// external I/O boundaries get port interfaces, and chunking is in-process
// domain logic. Dependency rule: stdlib + internal/domain only — the
// markdown parsing is hand-rolled, no parser dependency.
//
// The pipeline calls Normalize on raw file bytes, then Chunk on the
// result, and records Version under Document.StageVersions["chunker"].
// Chunk byte offsets index into the normalized text.
//
// Scope (v1) — deliberate exclusions, each revisitable behind a Version
// bump:
//   - ATX headings only ("#"–"######"). Setext headings (=== / ---
//     underlines) are not recognized: a paragraph followed by --- stays a
//     paragraph, because a false setext positive would turn horizontal
//     rules and stray YAML delimiters into fake headings — worse than
//     missing the rare real setext heading.
//   - Fenced code blocks only (``` / ~~~, matched as pairs). Indented
//     (4-space) code blocks fall through as paragraphs — harmless for
//     chunk-boundary purposes.
//   - UTF-8 and BOM-marked UTF-16 input only. Other encodings fail
//     Normalize loudly and the file is marked failed, never garbled.
//
// Sizing is byte-based with the ~4 bytes/token heuristic; there is no
// tokenizer in-process ("no models in-process, ever"). The heuristic
// over-counts English prose by roughly 40% (measured in lore, this
// project's predecessor), so real chunks land under nominal targets —
// accepted, and why nothing downstream may treat token counts as real.
package chunker

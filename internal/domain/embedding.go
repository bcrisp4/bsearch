package domain

import "strings"

// Placeholders in embedding prefix templates. Substitution is plain string
// replacement — no escaping, no template engine (the strings come from a
// small registry or the user's own config).
const (
	// PlaceholderQuery is replaced with the search query text.
	PlaceholderQuery = "{q}"
	// PlaceholderPassage is replaced with the chunk text.
	PlaceholderPassage = "{d}"
	// PlaceholderTitle is replaced with the chunk's heading-path breadcrumb,
	// for models with a dedicated title slot (EmbeddingGemma's "title:").
	PlaceholderTitle = "{t}"
)

// EmbeddingSpec identifies how vectors are produced: the model plus the
// exact prefix templates and input ceiling applied at embed time. It is
// recorded in pipeline metadata; a change to any field makes existing
// vectors incompatible (asymmetric embedders lose substantial recall when
// query and passage prefixes don't match — DESIGN.md: Embeddings/LLM).
//
// Composition lives here, on the spec, so every adapter — whatever the
// transport protocol — applies templates identically at index and query
// time. Adapters must embed the output of ComposeQuery/ComposePassage,
// never raw text.
type EmbeddingSpec struct {
	Model string
	// QueryTemplate contains {q}; empty means raw (no prefix).
	QueryTemplate string
	// PassageTemplate contains {d} and optionally {t}; empty means raw.
	PassageTemplate string
	// CeilingTokens is the model's input limit in tokens; 0 means unlimited.
	// Token counts are heuristic (≈ chars/4) — no tokenizer in-process.
	CeilingTokens int
}

// ComposeQuery applies the query template to a search query.
func (s EmbeddingSpec) ComposeQuery(query string) string {
	if s.QueryTemplate == "" {
		return query
	}
	return strings.ReplaceAll(s.QueryTemplate, PlaceholderQuery, query)
}

// ComposePassage applies the passage template and the chunk's heading-path
// breadcrumb (the lore lesson: breadcrumbs contextualize the chunk for the
// embedding model — DESIGN.md: Chunking). Only Text and HeadingPath are
// read.
//
// Rules:
//   - Template has {t}: the breadcrumb fills it, or the literal "none" when
//     the chunk has no heading path (EmbeddingGemma model-card convention).
//   - No {t} and a breadcrumb exists: the breadcrumb is prepended to the
//     text ("breadcrumb\n\ntext") before {d} substitution.
//   - Empty template: breadcrumb-prepended text, unprefixed.
func (s EmbeddingSpec) ComposePassage(c Chunk) string {
	if strings.Contains(s.PassageTemplate, PlaceholderTitle) {
		title := c.HeadingPath
		if title == "" {
			title = "none"
		}
		out := strings.ReplaceAll(s.PassageTemplate, PlaceholderTitle, title)
		return strings.ReplaceAll(out, PlaceholderPassage, c.Text)
	}

	text := c.Text
	if c.HeadingPath != "" {
		text = c.HeadingPath + "\n\n" + text
	}
	if s.PassageTemplate == "" {
		return text
	}
	return strings.ReplaceAll(s.PassageTemplate, PlaceholderPassage, text)
}

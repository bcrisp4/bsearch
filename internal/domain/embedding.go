package domain

import (
	"fmt"
	"strings"
)

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

const (
	// BytesPerToken is the heuristic byte-per-token estimate used wherever
	// token budgets are enforced without a tokenizer (chunker ceilings, the
	// embedder's input guard). One shared constant so the two sides of the
	// budget can never disagree.
	BytesPerToken = 4

	// TemplateReserveBytes is the ceiling headroom the chunker reserves for
	// the passage template literal. A PassageTemplate longer than this can
	// compose over the model's input ceiling on a full-size chunk;
	// EmbeddingSpec.Validate rejects it. The breadcrumb is budgeted
	// separately (the chunker subtracts each section's heading-path length).
	TemplateReserveBytes = 256

	// MinCeilingTokens is the smallest usable input ceiling: the template
	// reserve plus the chunker's 64-byte minimum chunk budget, in tokens.
	// Below this the chunker's byte budget zeroes out (its documented
	// out-of-contract range) while the embedder guard still enforces the
	// ceiling — every document would fail. No real embedding model is
	// remotely this small; Validate rejects such ceilings up front.
	MinCeilingTokens = (TemplateReserveBytes + 64) / BytesPerToken
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

// Validate reports whether the spec's templates are usable: placeholders
// present where a template is set, passage template within the chunker's
// reserve, ceiling non-negative. The single choke point for specs from any
// source (registry, config override, or hand-built).
func (s EmbeddingSpec) Validate() error {
	if t := s.QueryTemplate; t != "" && !strings.Contains(t, PlaceholderQuery) {
		return fmt.Errorf("query template %q does not contain %s", t, PlaceholderQuery)
	}
	if t := s.PassageTemplate; t != "" && !strings.Contains(t, PlaceholderPassage) {
		return fmt.Errorf("passage template %q does not contain %s", t, PlaceholderPassage)
	}
	if n := len(s.PassageTemplate); n > TemplateReserveBytes {
		return fmt.Errorf(
			"passage template is %d bytes, over the %d-byte reserve the chunker budgets — composed text would exceed the input ceiling on full-size chunks",
			n, TemplateReserveBytes)
	}
	if s.CeilingTokens < 0 {
		return fmt.Errorf("input ceiling %d is negative", s.CeilingTokens)
	}
	if s.CeilingTokens > 0 && s.CeilingTokens < MinCeilingTokens {
		return fmt.Errorf("input ceiling %d is below the %d-token minimum (template reserve + minimum chunk budget)",
			s.CeilingTokens, MinCeilingTokens)
	}
	return nil
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
		// Single pass: a heading path containing a literal placeholder
		// (document text is untrusted) must not be re-substituted.
		return strings.NewReplacer(
			PlaceholderTitle, title,
			PlaceholderPassage, c.Text,
		).Replace(s.PassageTemplate)
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

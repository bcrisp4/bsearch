// Package embedding resolves the EmbeddingSpec for a configured model:
// per-field config overrides, else a built-in registry of known models,
// else raw (no prefixes, no ceiling).
//
// The registry is deliberately protocol-agnostic — prefix templates are a
// property of the model, not of the adapter that reaches it — and
// deliberately sparse: entries are added when the M2 bake-off (issue #10)
// validates them, not copied speculatively from model cards. Unknown
// models are not an error (BYO inference); config overrides cover them.
package embedding

import (
	"sort"
	"strings"

	"github.com/bcrisp4/bsearch/internal/domain"
)

// registryEntry is the validated default spec for one model family.
type registryEntry struct {
	queryTemplate   string
	passageTemplate string
	ceilingTokens   int
}

// registry maps a lowercase substring of the model identifier to its
// defaults. Substring matching absorbs server-specific naming (LM Studio
// serves "text-embedding-embeddinggemma-300m-qat"); the longest matching
// key wins so overlapping keys resolve deterministically.
var registry = map[string]registryEntry{
	// google/embeddinggemma-300m — research doc
	// docs/research/2026-07-19-embedding-models-*.md. The only entry until
	// the M2 bake-off validates more.
	"embeddinggemma": { // #nosec G101 -- prompt prefix templates, not credentials
		queryTemplate:   "task: search result | query: {q}",
		passageTemplate: "title: {t} | text: {d}",
		ceilingTokens:   2048,
	},
}

// lookup finds the registry entry whose key is the longest substring of
// the lowercased model identifier.
func lookup(model string) (registryEntry, bool) {
	lower := strings.ToLower(model)
	keys := make([]string, 0, len(registry))
	for key := range registry {
		if strings.Contains(lower, key) {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return registryEntry{}, false
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	return registry[keys[0]], true
}

// ResolveSpec builds the EmbeddingSpec for model, merging per field:
// a non-empty/non-zero override wins, else the registry entry, else
// raw/unlimited. Overrides come from config ([inference] query_template,
// passage_template, input_ceiling_tokens).
func ResolveSpec(model, queryOverride, passageOverride string, ceilingOverride int) domain.EmbeddingSpec {
	spec := domain.EmbeddingSpec{
		Model:           model,
		QueryTemplate:   queryOverride,
		PassageTemplate: passageOverride,
		CeilingTokens:   ceilingOverride,
	}
	entry, ok := lookup(model)
	if !ok {
		return spec
	}
	if spec.QueryTemplate == "" {
		spec.QueryTemplate = entry.queryTemplate
	}
	if spec.PassageTemplate == "" {
		spec.PassageTemplate = entry.passageTemplate
	}
	if spec.CeilingTokens == 0 {
		spec.CeilingTokens = entry.ceilingTokens
	}
	return spec
}

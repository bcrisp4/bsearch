package embedding

import (
	"testing"

	"github.com/bcrisp4/bsearch/internal/domain"
)

func TestResolveSpecRegistry(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  domain.EmbeddingSpec
	}{
		{
			name:  "exact family name",
			model: "embeddinggemma",
			want: domain.EmbeddingSpec{
				Model:           "embeddinggemma",
				QueryTemplate:   "task: search result | query: {q}",
				PassageTemplate: "title: {t} | text: {d}",
				CeilingTokens:   2048,
			},
		},
		{
			name:  "LM Studio style identifier",
			model: "text-embedding-embeddinggemma-300m-qat",
			want: domain.EmbeddingSpec{
				Model:           "text-embedding-embeddinggemma-300m-qat",
				QueryTemplate:   "task: search result | query: {q}",
				PassageTemplate: "title: {t} | text: {d}",
				CeilingTokens:   2048,
			},
		},
		{
			name:  "case insensitive",
			model: "Google/EmbeddingGemma-300M",
			want: domain.EmbeddingSpec{
				Model:           "Google/EmbeddingGemma-300M",
				QueryTemplate:   "task: search result | query: {q}",
				PassageTemplate: "title: {t} | text: {d}",
				CeilingTokens:   2048,
			},
		},
		{
			name:  "unknown model is raw and unlimited",
			model: "mystery-embedder-9000",
			want:  domain.EmbeddingSpec{Model: "mystery-embedder-9000"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveSpec(tt.model, "", "", 0); got != tt.want {
				t.Errorf("ResolveSpec() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestResolveSpecOverridesWinPerField(t *testing.T) {
	got := ResolveSpec("embeddinggemma", "custom query: {q}", "", 512)
	want := domain.EmbeddingSpec{
		Model:           "embeddinggemma",
		QueryTemplate:   "custom query: {q}",      // override
		PassageTemplate: "title: {t} | text: {d}", // registry
		CeilingTokens:   512,                      // override
	}
	if got != want {
		t.Errorf("ResolveSpec() = %+v, want %+v", got, want)
	}
}

func TestResolveSpecOverridesForUnknownModel(t *testing.T) {
	got := ResolveSpec("mystery", "q: {q}", "p: {d}", 1024)
	want := domain.EmbeddingSpec{
		Model:           "mystery",
		QueryTemplate:   "q: {q}",
		PassageTemplate: "p: {d}",
		CeilingTokens:   1024,
	}
	if got != want {
		t.Errorf("ResolveSpec() = %+v, want %+v", got, want)
	}
}

func TestLookupEqualLengthTieIsDeterministic(t *testing.T) {
	// Two equal-length keys matching one model id must resolve the same
	// way every run (lexically smaller wins) — map order is randomized.
	registry["aa-tie"] = registryEntry{queryTemplate: "first: {q}"}
	registry["bb-tie"] = registryEntry{queryTemplate: "second: {q}"}
	t.Cleanup(func() {
		delete(registry, "aa-tie")
		delete(registry, "bb-tie")
	})

	for range 20 {
		got := ResolveSpec("model-aa-tie-bb-tie", "", "", 0)
		if want := "first: {q}"; got.QueryTemplate != want {
			t.Fatalf("QueryTemplate = %q, want lexically-first %q", got.QueryTemplate, want)
		}
	}
}

func TestLookupLongestKeyWins(t *testing.T) {
	// Guard the tie-break rule with temporary overlapping keys.
	registry["gemma"] = registryEntry{queryTemplate: "short: {q}"}
	t.Cleanup(func() { delete(registry, "gemma") })

	got := ResolveSpec("embeddinggemma-300m", "", "", 0)
	if want := "task: search result | query: {q}"; got.QueryTemplate != want {
		t.Errorf("QueryTemplate = %q, want longest-key match %q", got.QueryTemplate, want)
	}
}

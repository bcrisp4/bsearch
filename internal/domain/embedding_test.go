package domain

import (
	"strings"
	"testing"
)

func TestComposeQuery(t *testing.T) {
	tests := []struct {
		name     string
		template string
		query    string
		want     string
	}{
		{
			name:     "empty template is raw",
			template: "",
			query:    "heat pump quote",
			want:     "heat pump quote",
		},
		{
			name:     "placeholder substituted",
			template: "task: search result | query: {q}",
			query:    "heat pump quote",
			want:     "task: search result | query: heat pump quote",
		},
		{
			name:     "template without placeholder passes through unchanged",
			template: "query prefix only",
			query:    "ignored",
			want:     "query prefix only",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := EmbeddingSpec{QueryTemplate: tt.template}
			if got := spec.ComposeQuery(tt.query); got != tt.want {
				t.Errorf("ComposeQuery() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEmbeddingSpecValidate(t *testing.T) {
	long := EmbeddingSpec{PassageTemplate: "prefix: " + strings.Repeat("x", TemplateReserveBytes) + " {d}"}
	tests := []struct {
		name    string
		spec    EmbeddingSpec
		wantErr bool
	}{
		{name: "zero spec is valid", spec: EmbeddingSpec{}},
		{name: "full valid spec", spec: EmbeddingSpec{
			QueryTemplate: "q: {q}", PassageTemplate: "t: {t} d: {d}", CeilingTokens: 2048,
		}},
		{name: "query template missing {q}", spec: EmbeddingSpec{QueryTemplate: "q: "}, wantErr: true},
		{name: "passage template missing {d}", spec: EmbeddingSpec{PassageTemplate: "p: {q}"}, wantErr: true},
		{name: "passage template over reserve", spec: long, wantErr: true},
		{name: "negative ceiling", spec: EmbeddingSpec{CeilingTokens: -1}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.spec.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestComposePassage(t *testing.T) {
	tests := []struct {
		name     string
		template string
		chunk    Chunk
		want     string
	}{
		{
			name:     "empty template no breadcrumb is raw",
			template: "",
			chunk:    Chunk{Text: "chunk text"},
			want:     "chunk text",
		},
		{
			name:     "empty template prepends breadcrumb",
			template: "",
			chunk:    Chunk{Text: "chunk text", HeadingPath: "Doc > Section"},
			want:     "Doc > Section\n\nchunk text",
		},
		{
			name:     "title slot takes breadcrumb",
			template: "title: {t} | text: {d}",
			chunk:    Chunk{Text: "chunk text", HeadingPath: "Doc > Section"},
			want:     "title: Doc > Section | text: chunk text",
		},
		{
			name:     "title slot without breadcrumb is literal none",
			template: "title: {t} | text: {d}",
			chunk:    Chunk{Text: "chunk text"},
			want:     "title: none | text: chunk text",
		},
		{
			name:     "no title slot prepends breadcrumb before substitution",
			template: "search_document: {d}",
			chunk:    Chunk{Text: "chunk text", HeadingPath: "Doc > Section"},
			want:     "search_document: Doc > Section\n\nchunk text",
		},
		{
			name:     "no title slot no breadcrumb",
			template: "search_document: {d}",
			chunk:    Chunk{Text: "chunk text"},
			want:     "search_document: chunk text",
		},
		{
			// Document text is untrusted: a heading containing a literal
			// placeholder must not be re-substituted (single-pass rule).
			name:     "heading containing placeholder survives",
			template: "title: {t} | text: {d}",
			chunk:    Chunk{Text: "BODY", HeadingPath: "H > {d} note"},
			want:     "title: H > {d} note | text: BODY",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := EmbeddingSpec{PassageTemplate: tt.template}
			if got := spec.ComposePassage(tt.chunk); got != tt.want {
				t.Errorf("ComposePassage() = %q, want %q", got, tt.want)
			}
		})
	}
}

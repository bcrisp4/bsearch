package domain

import "testing"

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

package domain

import (
	"reflect"
	"testing"
)

// hit builds a minimal Hit for collapse tests: identity is Doc.ID, order is
// the caller's (ascending distance per the SearchVectors contract).
func hit(docID string, ordinal int, distance float64) Hit {
	return Hit{
		Doc:      Document{ID: docID},
		Chunk:    Chunk{DocID: docID, Ordinal: ordinal},
		Distance: distance,
	}
}

func TestCollapseBestPerDoc(t *testing.T) {
	tests := []struct {
		name  string
		hits  []Hit
		limit int
		want  []Hit
	}{
		{
			name: "dedupes to first (best) chunk per doc, preserving order",
			hits: []Hit{
				hit("a", 0, 0.1),
				hit("b", 3, 0.2),
				hit("a", 1, 0.3),
				hit("c", 0, 0.4),
			},
			limit: 10,
			want:  []Hit{hit("a", 0, 0.1), hit("b", 3, 0.2), hit("c", 0, 0.4)},
		},
		{
			name: "respects limit",
			hits: []Hit{
				hit("a", 0, 0.1),
				hit("b", 0, 0.2),
				hit("c", 0, 0.3),
			},
			limit: 2,
			want:  []Hit{hit("a", 0, 0.1), hit("b", 0, 0.2)},
		},
		{
			name: "fewer docs than limit returns what exists",
			hits: []Hit{
				hit("a", 0, 0.1),
				hit("a", 1, 0.2),
				hit("a", 2, 0.3),
			},
			limit: 5,
			want:  []Hit{hit("a", 0, 0.1)},
		},
		{
			name:  "empty input",
			hits:  nil,
			limit: 10,
			want:  nil,
		},
		{
			name:  "zero limit",
			hits:  []Hit{hit("a", 0, 0.1)},
			limit: 0,
			want:  nil,
		},
		{
			name:  "negative limit",
			hits:  []Hit{hit("a", 0, 0.1)},
			limit: -1,
			want:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CollapseBestPerDoc(tt.hits, tt.limit)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CollapseBestPerDoc() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

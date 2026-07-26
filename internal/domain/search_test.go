package domain

import (
	"reflect"
	"testing"
	"time"
)

// hit builds a minimal Hit for collapse tests: identity is Doc.ContentHash,
// the chunk is identified by ordinal, and order is the caller's — ascending
// distance, then chunk, then mtime descending / path ascending within a
// chunk (the SearchVectors contract).
func hit(hash, path string, ordinal int, distance float64, mtime int64) Hit {
	return Hit{
		Doc:      Document{Path: path, ContentHash: hash, MTime: time.Unix(mtime, 0)},
		Chunk:    Chunk{Ordinal: ordinal},
		Distance: distance,
	}
}

func TestCollapseBestPerContent(t *testing.T) {
	tests := []struct {
		name  string
		hits  []Hit
		limit int
		want  []ContentHit
	}{
		{
			name: "dedupes to first (best) chunk per content, preserving order",
			hits: []Hit{
				hit("h1", "/a", 0, 0.1, 100),
				hit("h2", "/b", 3, 0.2, 100),
				hit("h1", "/a", 1, 0.3, 100),
				hit("h3", "/c", 0, 0.4, 100),
			},
			limit: 10,
			want: []ContentHit{
				{Hit: hit("h1", "/a", 0, 0.1, 100)},
				{Hit: hit("h2", "/b", 3, 0.2, 100)},
				{Hit: hit("h3", "/c", 0, 0.4, 100)},
			},
		},
		{
			name: "fan-out over duplicate paths collapses to primary plus also_at",
			hits: []Hit{
				// winning chunk's run: newest mtime first (the primary),
				// then the rest.
				hit("h1", "/new/copy.md", 0, 0.1, 200),
				hit("h1", "/z/copy.md", 0, 0.1, 100),
				hit("h1", "/archive/copy.md", 0, 0.1, 100),
				// a worse chunk of the same content fans out too — skipped
				// entirely, its paths must not re-enter also_at.
				hit("h1", "/new/copy.md", 1, 0.3, 200),
				hit("h1", "/z/copy.md", 1, 0.3, 100),
				hit("h2", "/other.md", 0, 0.4, 100),
			},
			limit: 10,
			want: []ContentHit{
				{
					Hit:    hit("h1", "/new/copy.md", 0, 0.1, 200),
					AlsoAt: []string{"/archive/copy.md", "/z/copy.md"},
				},
				{Hit: hit("h2", "/other.md", 0, 0.4, 100)},
			},
		},
		{
			name: "limit counts contents, and also_at still accumulates for selected content",
			hits: []Hit{
				hit("h1", "/primary.md", 0, 0.1, 200),
				hit("h1", "/dup.md", 0, 0.1, 100),
				hit("h2", "/b.md", 0, 0.2, 100),
				hit("h3", "/c.md", 0, 0.3, 100),
			},
			limit: 2,
			want: []ContentHit{
				{Hit: hit("h1", "/primary.md", 0, 0.1, 200), AlsoAt: []string{"/dup.md"}},
				{Hit: hit("h2", "/b.md", 0, 0.2, 100)},
			},
		},
		{
			name: "fewer contents than limit returns what exists",
			hits: []Hit{
				hit("h1", "/a", 0, 0.1, 100),
				hit("h1", "/a", 1, 0.2, 100),
				hit("h1", "/a", 2, 0.3, 100),
			},
			limit: 5,
			want:  []ContentHit{{Hit: hit("h1", "/a", 0, 0.1, 100)}},
		},
		{
			name:  "empty input",
			hits:  nil,
			limit: 10,
			want:  nil,
		},
		{
			name:  "zero limit",
			hits:  []Hit{hit("h1", "/a", 0, 0.1, 100)},
			limit: 0,
			want:  nil,
		},
		{
			name:  "negative limit",
			hits:  []Hit{hit("h1", "/a", 0, 0.1, 100)},
			limit: -1,
			want:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CollapseBestPerContent(tt.hits, tt.limit)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CollapseBestPerContent() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

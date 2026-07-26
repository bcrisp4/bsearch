package domain

import "sort"

// ContentHit is one content's best chunk plus every path holding that
// content. Hit carries the primary path's documents row (the referencing
// document with the newest mtime, tie-broken by path ascending — the rule
// DESIGN.md fixes so the same corpus always yields the same answer); AlsoAt
// is the other paths, ascending, nil when the content is unique.
type ContentHit struct {
	Hit    Hit
	AlsoAt []string
}

// CollapseBestPerContent reduces fanned-out chunk hits to at most limit
// contents, keeping each content's best chunk (DESIGN.md: retrieval
// granularity — KNN runs at chunk level, results collapse to
// best-chunk-per-content, ADR 0015).
//
// Input must be ordered by ascending distance, then chunk, then mtime
// descending / path ascending within a chunk (the SearchVectors contract).
// The first row of an unseen content is therefore its best chunk carrying
// its primary path; the winning chunk's remaining fan-out rows contribute
// AlsoAt; rows for a worse chunk of an already-collapsed content are
// skipped entirely. Output preserves distance order. limit counts contents
// and limit <= 0 returns nil.
func CollapseBestPerContent(hits []Hit, limit int) []ContentHit {
	if limit <= 0 || len(hits) == 0 {
		return nil
	}
	out := make([]ContentHit, 0, min(limit, len(hits)))
	seen := make(map[string]int, min(limit, len(hits)))
	for _, h := range hits {
		if i, dup := seen[h.Doc.ContentHash]; dup {
			// (ContentHash, Ordinal) identifies a chunk, so an equal
			// ordinal is another path holding the winning chunk's content;
			// any other ordinal is a worse chunk of a content already
			// collapsed.
			if h.Chunk.Ordinal == out[i].Hit.Chunk.Ordinal {
				out[i].AlsoAt = append(out[i].AlsoAt, h.Doc.Path)
			}
			continue
		}
		if len(out) == limit {
			// Never break: fan-out rows for already-collapsed contents may
			// still follow and belong in their AlsoAt.
			continue
		}
		seen[h.Doc.ContentHash] = len(out)
		out = append(out, ContentHit{Hit: h})
	}
	for i := range out {
		sort.Strings(out[i].AlsoAt)
	}
	return out
}

package domain

// CollapseBestPerDoc reduces chunk-level hits to at most limit documents,
// keeping each document's best chunk (DESIGN.md: retrieval granularity —
// KNN runs at chunk level, results collapse to best-chunk-per-document).
// Input must be sorted ascending by distance (the SearchVectors contract);
// output preserves that order, so a document's first hit is its best.
// limit <= 0 returns nil.
func CollapseBestPerDoc(hits []Hit, limit int) []Hit {
	if limit <= 0 || len(hits) == 0 {
		return nil
	}
	out := make([]Hit, 0, min(limit, len(hits)))
	seen := make(map[string]struct{}, min(limit, len(hits)))
	for _, h := range hits {
		if _, dup := seen[h.Doc.ID]; dup {
			continue
		}
		seen[h.Doc.ID] = struct{}{}
		out = append(out, h)
		if len(out) == limit {
			break
		}
	}
	return out
}

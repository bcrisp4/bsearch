package evalharness

import "slices"

// RankedDoc is one document in the collapsed, distance-ordered ranking.
type RankedDoc struct {
	Path     string  `json:"path"` // corpus-relative
	Distance float64 `json:"distance"`
}

// SortRanked orders docs by ascending distance, ties broken by ascending
// path. This is the single place the determinism rule from the spec is
// applied: equal-distance results are a reproducibility hazard (two runs of
// the same model could otherwise disagree on ordering), and trec_eval
// breaks the same tie by doc id for the same reason. Sorts in place.
func SortRanked(docs []RankedDoc) {
	slices.SortStableFunc(docs, func(a, b RankedDoc) int {
		if a.Distance < b.Distance {
			return -1
		}
		if a.Distance > b.Distance {
			return 1
		}
		if a.Path < b.Path {
			return -1
		}
		if a.Path > b.Path {
			return 1
		}
		return 0
	})
}

// QueryScore holds one query's metrics at cutoff k.
type QueryScore struct {
	RecallAtK     float64 `json:"recall_at_10"`
	RR            float64 `json:"rr"`
	SuccessAt1    int     `json:"success_at_1"`
	AcceptableAtK int     `json:"acceptable_at_10"`
}

// ScoreQuery scores one query's ranking against its golden answers using
// condensed-list scoring: acceptable docs are removed from the ranking
// before recall/RR/success are computed, so they occupy no rank slot —
// neither a hit nor a miss — and don't push a genuinely relevant doc out of
// the cutoff. AcceptableAtK is the exception: it counts acceptable docs
// seen in the *uncondensed* top k, since it measures what the raw ranking
// surfaced, not the condensed view.
//
// ranked need not be pre-sorted by SortRanked's tie rule, but callers
// should sort it beforehand for reproducible results — ScoreQuery does not
// re-sort.
//
// q.Relevant empty (a zero-answer query) returns the zero-value QueryScore
// rather than dividing by zero; in production these queries never reach
// ScoreQuery (Aggregate excludes them), but the guard keeps the function
// total.
func ScoreQuery(q Query, ranked []RankedDoc, k int) QueryScore {
	if len(q.Relevant) == 0 {
		return QueryScore{}
	}

	relevant := make(map[string]bool, len(q.Relevant))
	for _, p := range q.Relevant {
		relevant[p] = true
	}
	acceptable := make(map[string]bool, len(q.Acceptable))
	for _, p := range q.Acceptable {
		acceptable[p] = true
	}

	uncondensedK := ranked
	if len(uncondensedK) > k {
		uncondensedK = uncondensedK[:k]
	}
	acceptableAtK := 0
	for _, d := range uncondensedK {
		if acceptable[d.Path] {
			acceptableAtK++
		}
	}

	condensed := make([]RankedDoc, 0, len(ranked))
	for _, d := range ranked {
		if acceptable[d.Path] {
			continue
		}
		condensed = append(condensed, d)
	}
	if len(condensed) > k {
		condensed = condensed[:k]
	}

	hits := 0
	rr := 0.0
	successAt1 := 0
	for i, d := range condensed {
		if !relevant[d.Path] {
			continue
		}
		hits++
		if rr == 0.0 {
			rr = 1.0 / float64(i+1)
		}
		if i == 0 {
			successAt1 = 1
		}
	}

	return QueryScore{
		RecallAtK:     float64(hits) / float64(len(q.Relevant)),
		RR:            rr,
		SuccessAt1:    successAt1,
		AcceptableAtK: acceptableAtK,
	}
}

// ScoredQuery pairs a query with its ranking and score: the input to
// Aggregate and the shape written to the results file.
type ScoredQuery struct {
	Query Query
	// Ranked is the full collapsed ranking, top k, uncondensed — the
	// condensing ScoreQuery performs for scoring is not reflected here, so
	// results retain what the model actually returned.
	Ranked []RankedDoc
	Score  QueryScore
}

// SliceStats are mean metrics over a set of queries (AcceptableAtK is the
// exception: a total, not a mean, since "how many acceptable docs did this
// slice surface" is more useful summed than averaged).
type SliceStats struct {
	N             int     `json:"n"`
	RecallAtK     float64 `json:"recall_at_10"`
	MRRAtK        float64 `json:"mrr_at_10"`
	SuccessAt1    float64 `json:"success_at_1"`
	AcceptableAtK int     `json:"acceptable_at_10"` // total, not mean
}

// Aggregates collects headline and per-tag metrics over a scored query set.
type Aggregates struct {
	// OverallNoExact excludes queries tagged "exact" as well as
	// "zero-answer" — the harness's default headline number, since
	// exact-match queries (e.g. "find the doc titled X") are easier than
	// genuine semantic retrieval and would inflate it.
	OverallNoExact SliceStats `json:"overall_no_exact"`
	// Overall includes "exact"-tagged queries but still excludes
	// "zero-answer" (which has no recall/RR to average).
	Overall SliceStats `json:"overall"`
	// Slices holds one entry per tag seen on a non-zero-answer query,
	// including "exact". "zero-answer" is never a key: those queries are
	// omitted from aggregation entirely, not just averaged as a slice.
	Slices map[string]SliceStats `json:"slices"`
}

// accumulator sums a slice's metrics before the final mean/total pass.
type accumulator struct {
	n                  int
	recallSum, rrSum   float64
	successSum         float64
	acceptableAtKTotal int
}

func (a *accumulator) add(s QueryScore) {
	a.n++
	a.recallSum += s.RecallAtK
	a.rrSum += s.RR
	a.successSum += float64(s.SuccessAt1)
	a.acceptableAtKTotal += s.AcceptableAtK
}

func (a accumulator) stats() SliceStats {
	if a.n == 0 {
		return SliceStats{}
	}
	n := float64(a.n)
	return SliceStats{
		N:             a.n,
		RecallAtK:     a.recallSum / n,
		MRRAtK:        a.rrSum / n,
		SuccessAt1:    a.successSum / n,
		AcceptableAtK: a.acceptableAtKTotal,
	}
}

// Aggregate computes headline and per-tag statistics over scored. Queries
// tagged "zero-answer" are excluded everywhere (they have no relevant docs
// to score against); queries tagged "exact" are excluded from
// OverallNoExact but included in Overall and in Slices["exact"].
func Aggregate(scored []ScoredQuery) Aggregates {
	var overall, overallNoExact accumulator
	tagAccs := make(map[string]*accumulator)

	for _, sq := range scored {
		if sq.Query.HasTag("zero-answer") {
			continue
		}

		overall.add(sq.Score)
		if !sq.Query.HasTag("exact") {
			overallNoExact.add(sq.Score)
		}

		for _, tag := range sq.Query.Tags {
			if tag == "zero-answer" {
				continue
			}
			acc, ok := tagAccs[tag]
			if !ok {
				acc = &accumulator{}
				tagAccs[tag] = acc
			}
			acc.add(sq.Score)
		}
	}

	slicesOut := make(map[string]SliceStats, len(tagAccs))
	for tag, acc := range tagAccs {
		slicesOut[tag] = acc.stats()
	}

	return Aggregates{
		OverallNoExact: overallNoExact.stats(),
		Overall:        overall.stats(),
		Slices:         slicesOut,
	}
}

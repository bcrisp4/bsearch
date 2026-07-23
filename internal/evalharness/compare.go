package evalharness

import (
	"fmt"
	"sort"
	"strings"
)

// SliceComparison compares one slice (or the headline) across two runs: the
// recomputed per-run stats over the paired queries, per-query win/loss/tie
// counts on RR, and paired t-tests on the RR and recall deltas.
type SliceComparison struct {
	N int `json:"n"`
	// A and B are recomputed from the paired queries' QueryScores, not
	// copied from the results files' stored Aggregates, so a comparison
	// stays internally consistent even against a hand-edited results file.
	A SliceStats `json:"a"`
	B SliceStats `json:"b"`
	// Wins/Losses/Ties classify each paired query by strict comparison of
	// B's RR against A's RR (equal RR is a tie, even if recall differs).
	Wins   int `json:"wins"`
	Losses int `json:"losses"`
	Ties   int `json:"ties"`
	// MRRTTest is the paired t-test over per-query deltas B.RR - A.RR.
	MRRTTest TTestResult `json:"mrr_ttest"`
	// RecallTTest is the paired t-test over per-query deltas
	// B.RecallAtK - A.RecallAtK.
	RecallTTest TTestResult `json:"recall_ttest"`
}

// Comparison is eval compare's output: run B measured against run A on a
// shared corpus and golden query set.
type Comparison struct {
	Corpus CorpusInfo `json:"corpus"`
	ModelA ModelInfo  `json:"model_a"`
	ModelB ModelInfo  `json:"model_b"`
	// OverallNoExact is the headline comparison, excluding "exact"-tagged
	// and "zero-answer"-tagged queries (see Aggregate).
	OverallNoExact SliceComparison `json:"overall_no_exact"`
	// Overall includes "exact"-tagged queries but still excludes
	// "zero-answer".
	Overall SliceComparison `json:"overall"`
	// Slices holds one entry per tag seen on a non-zero-answer query,
	// including "exact". "zero-answer" is never a key.
	Slices map[string]SliceComparison `json:"slices"`
}

// queryPair is one query's QueryResult from each run, matched by id.
type queryPair struct {
	A, B QueryResult
}

// CompareResults pairs run b against run a per query id and reports how b
// changed relative to a. It refuses to compare runs that didn't score the
// same golden set: a mismatched corpus name or version, or a query id
// present in one run but not the other, is an error rather than a partial
// comparison.
//
// Zero-answer-tagged queries are excluded everywhere (they have no RR or
// recall to compare); exact-tagged queries are excluded from OverallNoExact
// but present in Overall and Slices["exact"] — the same rules Aggregate
// applies within a single run.
//
// Tag/slice membership and exclusion decisions (zero-answer, exact, and
// per-tag slicing) use run A's tags for a given query id only — B's tags
// for the same id are never consulted. This is safe because the corpus
// version gate above means paired runs were scored against byte-identical
// golden.yaml, so A and B necessarily agree on every query's tags anyway.
func CompareResults(a, b Results) (Comparison, error) {
	if a.Corpus.Name != b.Corpus.Name {
		return Comparison{}, fmt.Errorf("compare: corpus name differs: a=%q b=%q", a.Corpus.Name, b.Corpus.Name)
	}
	if a.Corpus.Version != b.Corpus.Version {
		return Comparison{}, fmt.Errorf("compare: corpus version differs: a=%q b=%q", a.Corpus.Version, b.Corpus.Version)
	}
	if a.Run.Limit != b.Run.Limit {
		return Comparison{}, fmt.Errorf("compare: limit differs: a=%d b=%d", a.Run.Limit, b.Run.Limit)
	}
	if err := checkQueryIDsMatch(a.Queries, b.Queries); err != nil {
		return Comparison{}, err
	}

	bByID := make(map[string]QueryResult, len(b.Queries))
	for _, q := range b.Queries {
		bByID[q.ID] = q
	}

	var overall, overallNoExact []queryPair
	tagPairs := make(map[string][]queryPair)

	// Iterate a.Queries in order (never map iteration order) so slice
	// membership and pairing are deterministic across runs of compare.
	for _, qa := range a.Queries {
		if resultHasTag(qa, "zero-answer") {
			continue
		}
		p := queryPair{A: qa, B: bByID[qa.ID]}

		overall = append(overall, p)
		if !resultHasTag(qa, "exact") {
			overallNoExact = append(overallNoExact, p)
		}

		for _, tag := range qa.Tags {
			if tag == "zero-answer" {
				continue
			}
			tagPairs[tag] = append(tagPairs[tag], p)
		}
	}

	slices := make(map[string]SliceComparison, len(tagPairs))
	for tag, pairs := range tagPairs {
		slices[tag] = compareSlice(pairs)
	}

	return Comparison{
		Corpus:         a.Corpus,
		ModelA:         a.Model,
		ModelB:         b.Model,
		OverallNoExact: compareSlice(overallNoExact),
		Overall:        compareSlice(overall),
		Slices:         slices,
	}, nil
}

// resultHasTag reports whether tag is present in q.Tags. QueryResult (unlike
// Query) has no HasTag method of its own — it carries a plain tags slice
// copied into the results file rather than the golden.Query type.
func resultHasTag(q QueryResult, tag string) bool {
	for _, t := range q.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// compareSlice recomputes A/B SliceStats and win/loss/tie counts from
// pairs' QueryScores, and runs paired t-tests on the RR and recall deltas.
func compareSlice(pairs []queryPair) SliceComparison {
	var accA, accB accumulator
	deltaRR := make([]float64, 0, len(pairs))
	deltaRecall := make([]float64, 0, len(pairs))
	wins, losses, ties := 0, 0, 0

	for _, p := range pairs {
		accA.add(p.A.QueryScore)
		accB.add(p.B.QueryScore)
		deltaRR = append(deltaRR, p.B.RR-p.A.RR)
		deltaRecall = append(deltaRecall, p.B.RecallAtK-p.A.RecallAtK)

		switch {
		case p.B.RR > p.A.RR:
			wins++
		case p.B.RR < p.A.RR:
			losses++
		default:
			ties++
		}
	}

	return SliceComparison{
		N:           len(pairs),
		A:           accA.stats(),
		B:           accB.stats(),
		Wins:        wins,
		Losses:      losses,
		Ties:        ties,
		MRRTTest:    PairedTTest(deltaRR),
		RecallTTest: PairedTTest(deltaRecall),
	}
}

// checkQueryIDsMatch returns an error listing ids present in one query set
// but not the other (sorted, both directions), or nil if the id sets match
// exactly.
func checkQueryIDsMatch(a, b []QueryResult) error {
	aIDs := make(map[string]bool, len(a))
	for _, q := range a {
		aIDs[q.ID] = true
	}
	bIDs := make(map[string]bool, len(b))
	for _, q := range b {
		bIDs[q.ID] = true
	}

	var onlyInA, onlyInB []string
	for id := range aIDs {
		if !bIDs[id] {
			onlyInA = append(onlyInA, id)
		}
	}
	for id := range bIDs {
		if !aIDs[id] {
			onlyInB = append(onlyInB, id)
		}
	}
	if len(onlyInA) == 0 && len(onlyInB) == 0 {
		return nil
	}
	sort.Strings(onlyInA)
	sort.Strings(onlyInB)

	var parts []string
	if len(onlyInA) > 0 {
		parts = append(parts, fmt.Sprintf("in a but not b: %s", strings.Join(onlyInA, ", ")))
	}
	if len(onlyInB) > 0 {
		parts = append(parts, fmt.Sprintf("in b but not a: %s", strings.Join(onlyInB, ", ")))
	}
	return fmt.Errorf("compare: query id sets differ: %s", strings.Join(parts, "; "))
}

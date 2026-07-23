package evalharness

import (
	"math"
	"testing"
)

// approxEqual reports whether a and b differ by no more than 1e-9, guarding
// float comparisons in these tests against binary representation noise.
func approxEqual(t *testing.T, got, want float64) bool {
	t.Helper()
	return math.Abs(got-want) < 1e-9
}

func TestSortRanked_TieBrokenByPath(t *testing.T) {
	docs := []RankedDoc{
		{Path: "b.md", Distance: 0.5},
		{Path: "c.md", Distance: 0.3},
		{Path: "a.md", Distance: 0.5},
	}
	SortRanked(docs)

	want := []RankedDoc{
		{Path: "c.md", Distance: 0.3},
		{Path: "a.md", Distance: 0.5},
		{Path: "b.md", Distance: 0.5},
	}
	for i, w := range want {
		if docs[i].Path != w.Path || docs[i].Distance != w.Distance {
			t.Fatalf("docs[%d] = %+v, want %+v", i, docs[i], w)
		}
	}
}

func TestScoreQuery_PerfectFirstHit(t *testing.T) {
	q := Query{Relevant: []string{"R"}}
	ranked := []RankedDoc{{Path: "R", Distance: 0.1}, {Path: "X", Distance: 0.2}}
	got := ScoreQuery(q, ranked, 10)

	if !approxEqual(t, got.RecallAtK, 1.0) {
		t.Errorf("RecallAtK = %v, want 1.0", got.RecallAtK)
	}
	if !approxEqual(t, got.RR, 1.0) {
		t.Errorf("RR = %v, want 1.0", got.RR)
	}
	if got.SuccessAt1 != 1 {
		t.Errorf("SuccessAt1 = %v, want 1", got.SuccessAt1)
	}
}

func TestScoreQuery_RelevantAtRank3(t *testing.T) {
	q := Query{Relevant: []string{"R"}}
	ranked := []RankedDoc{{Path: "X", Distance: 0.1}, {Path: "Y", Distance: 0.2}, {Path: "R", Distance: 0.3}}
	got := ScoreQuery(q, ranked, 10)

	if !approxEqual(t, got.RR, 1.0/3.0) {
		t.Errorf("RR = %v, want 1/3", got.RR)
	}
	if got.SuccessAt1 != 0 {
		t.Errorf("SuccessAt1 = %v, want 0", got.SuccessAt1)
	}
	if !approxEqual(t, got.RecallAtK, 1.0) {
		t.Errorf("RecallAtK = %v, want 1.0", got.RecallAtK)
	}
}

func TestScoreQuery_MissBeyondK(t *testing.T) {
	q := Query{Relevant: []string{"R"}}
	ranked := make([]RankedDoc, 0, 11)
	for i := 0; i < 10; i++ {
		ranked = append(ranked, RankedDoc{Path: string(rune('a' + i)), Distance: float64(i) * 0.01})
	}
	ranked = append(ranked, RankedDoc{Path: "R", Distance: 0.11})
	got := ScoreQuery(q, ranked, 10)

	if !approxEqual(t, got.RecallAtK, 0.0) {
		t.Errorf("RecallAtK = %v, want 0.0", got.RecallAtK)
	}
	if !approxEqual(t, got.RR, 0.0) {
		t.Errorf("RR = %v, want 0.0", got.RR)
	}
}

func TestScoreQuery_MultiRelevantPartial(t *testing.T) {
	q := Query{Relevant: []string{"R1", "R2"}}
	ranked := []RankedDoc{{Path: "R1", Distance: 0.1}, {Path: "X", Distance: 0.2}}
	got := ScoreQuery(q, ranked, 10)

	if !approxEqual(t, got.RecallAtK, 0.5) {
		t.Errorf("RecallAtK = %v, want 0.5", got.RecallAtK)
	}
}

func TestScoreQuery_AcceptableCondensed(t *testing.T) {
	q := Query{Relevant: []string{"R"}, Acceptable: []string{"A"}}
	ranked := []RankedDoc{{Path: "A", Distance: 0.1}, {Path: "R", Distance: 0.2}, {Path: "X", Distance: 0.3}}
	got := ScoreQuery(q, ranked, 10)

	if !approxEqual(t, got.RR, 1.0) {
		t.Errorf("RR = %v, want 1.0 (A removed, R now rank 1)", got.RR)
	}
	if got.SuccessAt1 != 1 {
		t.Errorf("SuccessAt1 = %v, want 1", got.SuccessAt1)
	}
	if got.AcceptableAtK != 1 {
		t.Errorf("AcceptableAtK = %v, want 1", got.AcceptableAtK)
	}
}

func TestScoreQuery_AcceptableNotAHit(t *testing.T) {
	q := Query{Relevant: []string{"R"}, Acceptable: []string{"A"}}
	ranked := []RankedDoc{{Path: "A", Distance: 0.1}, {Path: "X", Distance: 0.2}}
	got := ScoreQuery(q, ranked, 10)

	if !approxEqual(t, got.RecallAtK, 0.0) {
		t.Errorf("RecallAtK = %v, want 0.0", got.RecallAtK)
	}
	if !approxEqual(t, got.RR, 0.0) {
		t.Errorf("RR = %v, want 0.0", got.RR)
	}
	if got.AcceptableAtK != 1 {
		t.Errorf("AcceptableAtK = %v, want 1", got.AcceptableAtK)
	}
}

func TestScoreQuery_AcceptableBeyondKNotCounted(t *testing.T) {
	q := Query{Relevant: []string{"R"}, Acceptable: []string{"A"}}
	ranked := make([]RankedDoc, 0, 11)
	for i := 0; i < 10; i++ {
		ranked = append(ranked, RankedDoc{Path: string(rune('a' + i)), Distance: float64(i) * 0.01})
	}
	ranked = append(ranked, RankedDoc{Path: "A", Distance: 0.11})
	got := ScoreQuery(q, ranked, 10)

	if got.AcceptableAtK != 0 {
		t.Errorf("AcceptableAtK = %v, want 0", got.AcceptableAtK)
	}
}

func TestScoreQuery_EmptyRelevantNoNaN(t *testing.T) {
	q := Query{}
	ranked := []RankedDoc{{Path: "X", Distance: 0.1}}
	got := ScoreQuery(q, ranked, 10)

	if got != (QueryScore{}) {
		t.Errorf("ScoreQuery on empty Relevant = %+v, want zero-value QueryScore", got)
	}
}

func TestAggregate_ExactExcludedFromHeadline(t *testing.T) {
	scored := []ScoredQuery{
		{Query: Query{ID: "q1", Tags: []string{"exact"}}, Score: QueryScore{RR: 1.0}},
		{Query: Query{ID: "q2"}, Score: QueryScore{RR: 0.5}},
		{Query: Query{ID: "q3"}, Score: QueryScore{RR: 0.5}},
	}
	agg := Aggregate(scored)

	if agg.OverallNoExact.N != 2 {
		t.Errorf("OverallNoExact.N = %v, want 2", agg.OverallNoExact.N)
	}
	if !approxEqual(t, agg.OverallNoExact.MRRAtK, 0.5) {
		t.Errorf("OverallNoExact.MRRAtK = %v, want 0.5", agg.OverallNoExact.MRRAtK)
	}
	if agg.Overall.N != 3 {
		t.Errorf("Overall.N = %v, want 3", agg.Overall.N)
	}
	if !approxEqual(t, agg.Overall.MRRAtK, 2.0/3.0) {
		t.Errorf("Overall.MRRAtK = %v, want 2/3", agg.Overall.MRRAtK)
	}
	if agg.Slices["exact"].N != 1 {
		t.Errorf(`Slices["exact"].N = %v, want 1`, agg.Slices["exact"].N)
	}
}

func TestAggregate_ZeroAnswerExcludedEverywhere(t *testing.T) {
	scored := []ScoredQuery{
		{Query: Query{ID: "q1", Tags: []string{"zero-answer"}}, Score: QueryScore{}},
		{Query: Query{ID: "q2"}, Score: QueryScore{RR: 1.0}},
	}
	agg := Aggregate(scored)

	if agg.Overall.N != 1 {
		t.Errorf("Overall.N = %v, want 1 (zero-answer query excluded)", agg.Overall.N)
	}
	if _, ok := agg.Slices["zero-answer"]; ok {
		t.Errorf(`Slices["zero-answer"] present, want no such key`)
	}
}

func TestAggregate_SliceMeans(t *testing.T) {
	scored := []ScoredQuery{
		{Query: Query{ID: "q1", Tags: []string{"kw"}}, Score: QueryScore{RecallAtK: 1.0}},
		{Query: Query{ID: "q2", Tags: []string{"kw"}}, Score: QueryScore{RecallAtK: 0.0}},
	}
	agg := Aggregate(scored)

	if agg.Slices["kw"].N != 2 {
		t.Errorf(`Slices["kw"].N = %v, want 2`, agg.Slices["kw"].N)
	}
	if !approxEqual(t, agg.Slices["kw"].RecallAtK, 0.5) {
		t.Errorf(`Slices["kw"].RecallAtK = %v, want 0.5`, agg.Slices["kw"].RecallAtK)
	}
}

package evalharness

import (
	"sort"
	"strings"
	"testing"
)

// queryFixture is the shorthand makeResults uses to build a QueryResult:
// just the metrics and tags compare.go actually reads.
type queryFixture struct {
	rr, recall float64
	tags       []string
}

// makeResults builds a minimal Results with fixed corpus name "c" and
// version "sha256:x" (the values CompareResults' mismatch checks compare
// against), one QueryResult per entry in queries. Query ids are sorted
// before building so tests get deterministic Queries order regardless of
// map iteration order.
func makeResults(queries map[string]queryFixture) Results {
	ids := make([]string, 0, len(queries))
	for id := range queries {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	qrs := make([]QueryResult, 0, len(ids))
	for _, id := range ids {
		f := queries[id]
		qrs = append(qrs, QueryResult{
			ID:   id,
			Tags: f.tags,
			QueryScore: QueryScore{
				RR:        f.rr,
				RecallAtK: f.recall,
			},
		})
	}

	return Results{
		Bsearch: BsearchInfo{ChunkerVersion: "chunker-v1"},
		Corpus:  CorpusInfo{Name: "c", Version: "sha256:x"},
		Queries: qrs,
	}
}

func TestCompareResults_MismatchedVersionRefused(t *testing.T) {
	a := makeResults(map[string]queryFixture{"q1": {rr: 1, recall: 1}})
	b := makeResults(map[string]queryFixture{"q1": {rr: 1, recall: 1}})
	b.Corpus.Version = "sha256:y"

	_, err := CompareResults(a, b)
	if err == nil {
		t.Fatal("CompareResults() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("error %q does not mention version", err.Error())
	}
	if !strings.Contains(err.Error(), "sha256:x") || !strings.Contains(err.Error(), "sha256:y") {
		t.Errorf("error %q does not name both versions", err.Error())
	}
}

// TestCompareResults_MismatchedChunkerVersionRefused asserts Finding 3's
// fix: BsearchInfo's doc comment promises compare refuses a chunker-version
// mismatch (results scored against different chunk boundaries aren't
// comparable), but until this fix only corpus name/version, limit, and
// query ids were actually gated.
func TestCompareResults_MismatchedChunkerVersionRefused(t *testing.T) {
	a := makeResults(map[string]queryFixture{"q1": {rr: 1, recall: 1}})
	b := makeResults(map[string]queryFixture{"q1": {rr: 1, recall: 1}})
	a.Bsearch.ChunkerVersion = "chunker-v1"
	b.Bsearch.ChunkerVersion = "chunker-v2"

	_, err := CompareResults(a, b)
	if err == nil {
		t.Fatal("CompareResults() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "chunker version") {
		t.Errorf("error %q does not mention chunker version", err.Error())
	}
	if !strings.Contains(err.Error(), "chunker-v1") || !strings.Contains(err.Error(), "chunker-v2") {
		t.Errorf("error %q does not name both chunker versions", err.Error())
	}
}

func TestCompareResults_MismatchedLimitRefused(t *testing.T) {
	a := makeResults(map[string]queryFixture{"q1": {rr: 1, recall: 1}})
	b := makeResults(map[string]queryFixture{"q1": {rr: 1, recall: 1}})
	a.Run.Limit = 10
	b.Run.Limit = 20

	_, err := CompareResults(a, b)
	if err == nil {
		t.Fatal("CompareResults() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error %q does not mention limit", err.Error())
	}
	if !strings.Contains(err.Error(), "10") || !strings.Contains(err.Error(), "20") {
		t.Errorf("error %q does not name both limits", err.Error())
	}
}

func TestCompareResults_MismatchedQueryIDsRefused(t *testing.T) {
	a := makeResults(map[string]queryFixture{
		"q1": {rr: 1, recall: 1},
		"q2": {rr: 1, recall: 1},
	})
	b := makeResults(map[string]queryFixture{
		"q1": {rr: 1, recall: 1},
	})

	_, err := CompareResults(a, b)
	if err == nil {
		t.Fatal("CompareResults() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "q2") {
		t.Errorf("error %q does not mention missing id q2", err.Error())
	}
}

func TestCompareResults_WinLossTie(t *testing.T) {
	a := makeResults(map[string]queryFixture{
		"win":  {rr: 0.5, recall: 0.5},
		"lose": {rr: 1.0, recall: 1.0},
		"tie":  {rr: 0.5, recall: 0.5},
	})
	b := makeResults(map[string]queryFixture{
		"win":  {rr: 1.0, recall: 1.0},
		"lose": {rr: 0.5, recall: 0.5},
		"tie":  {rr: 0.5, recall: 0.5},
	})

	got, err := CompareResults(a, b)
	if err != nil {
		t.Fatalf("CompareResults() error = %v, want nil", err)
	}
	if got.Overall.Wins != 1 || got.Overall.Losses != 1 || got.Overall.Ties != 1 {
		t.Errorf("Overall Wins/Losses/Ties = %d/%d/%d, want 1/1/1",
			got.Overall.Wins, got.Overall.Losses, got.Overall.Ties)
	}
	if got.Overall.N != 3 {
		t.Errorf("Overall.N = %d, want 3", got.Overall.N)
	}
}

func TestCompareResults_TTestWiredToDeltas(t *testing.T) {
	a := makeResults(map[string]queryFixture{
		"q1": {rr: 0.2, recall: 0.2},
		"q2": {rr: 0.4, recall: 0.4},
		"q3": {rr: 0.6, recall: 0.6},
		"q4": {rr: 0.8, recall: 0.8},
		"q5": {rr: 1.0, recall: 1.0},
	})
	b := makeResults(map[string]queryFixture{
		"q1": {rr: 0.3, recall: 0.3},
		"q2": {rr: 0.5, recall: 0.5},
		"q3": {rr: 0.5, recall: 0.5},
		"q4": {rr: 0.9, recall: 0.9},
		"q5": {rr: 1.0, recall: 1.0},
	})
	// deltas (b-a): 0.1, 0.1, -0.1, 0.1, 0.0
	wantMean := (0.1 + 0.1 - 0.1 + 0.1 + 0.0) / 5

	got, err := CompareResults(a, b)
	if err != nil {
		t.Fatalf("CompareResults() error = %v, want nil", err)
	}
	if !approxEqual(t, got.OverallNoExact.MRRTTest.MeanDelta, wantMean) {
		t.Errorf("OverallNoExact.MRRTTest.MeanDelta = %v, want %v", got.OverallNoExact.MRRTTest.MeanDelta, wantMean)
	}
	if got.OverallNoExact.MRRTTest.N != 5 {
		t.Errorf("OverallNoExact.MRRTTest.N = %d, want 5", got.OverallNoExact.MRRTTest.N)
	}
}

func TestCompareResults_ExactSliceExcludedFromHeadline(t *testing.T) {
	a := makeResults(map[string]queryFixture{
		"exact1": {rr: 1.0, recall: 1.0, tags: []string{"exact"}},
		"q1":     {rr: 0.5, recall: 0.5},
	})
	b := makeResults(map[string]queryFixture{
		"exact1": {rr: 0.0, recall: 0.0, tags: []string{"exact"}},
		"q1":     {rr: 0.5, recall: 0.5},
	})

	got, err := CompareResults(a, b)
	if err != nil {
		t.Fatalf("CompareResults() error = %v, want nil", err)
	}

	if got.OverallNoExact.N != 1 {
		t.Errorf("OverallNoExact.N = %d, want 1 (exact query excluded)", got.OverallNoExact.N)
	}
	if !approxEqual(t, got.OverallNoExact.MRRTTest.MeanDelta, 0) {
		t.Errorf("OverallNoExact.MRRTTest.MeanDelta = %v, want 0 (unaffected by exact query)", got.OverallNoExact.MRRTTest.MeanDelta)
	}

	if got.Overall.N != 2 {
		t.Errorf("Overall.N = %d, want 2 (exact query included)", got.Overall.N)
	}

	sc, ok := got.Slices["exact"]
	if !ok {
		t.Fatal(`Slices["exact"] missing`)
	}
	if sc.N != 1 {
		t.Errorf(`Slices["exact"].N = %d, want 1`, sc.N)
	}
	if !approxEqual(t, sc.A.MRRAtK, 1.0) || !approxEqual(t, sc.B.MRRAtK, 0.0) {
		t.Errorf(`Slices["exact"].A/B MRRAtK = %v/%v, want 1.0/0.0`, sc.A.MRRAtK, sc.B.MRRAtK)
	}
}

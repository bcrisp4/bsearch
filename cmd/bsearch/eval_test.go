package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bcrisp4/bsearch/internal/evalharness"
)

// evalTestVec is content-keyed like the search-package fake (testutil_test.go
// documents the convention): the corpus and its queries share the keywords
// "rent" and "boiler", so a query embeds to the same vector as the document
// it should retrieve.
func evalTestVec(_ int, input string) []float32 {
	switch {
	case strings.Contains(input, "rent"):
		return []float32{1, 0, 0}
	case strings.Contains(input, "boiler"):
		return []float32{0, 1, 0}
	default:
		return []float32{0.5, 0.5, 0}
	}
}

// writeEvalCorpus builds a minimal golden corpus under dir/corpus-root:
// two documents and a golden.yaml with a keyword query, an exact query, and
// a zero-answer query. Returns the corpus root (the --corpus argument).
func writeEvalCorpus(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	corpus := filepath.Join(root, "corpus")
	if err := os.MkdirAll(filepath.Join(corpus, "letters"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(corpus, "invoices"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corpus, "letters", "renewal.md"),
		[]byte("Your rent is going up this year.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corpus, "invoices", "boiler.md"),
		[]byte("Invoice DS-26417 for the boiler service.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	golden := `queries:
  - id: q1
    query: rent going up
    relevant:
      - corpus/letters/renewal.md
    tags: [kw]
  - id: q2
    query: boiler invoice cost
    relevant:
      - corpus/invoices/boiler.md
    tags: [exact]
  - id: q3
    query: car insurance renewal
    relevant: []
    tags: [zero-answer]
`
	if err := os.WriteFile(filepath.Join(root, "golden.yaml"), []byte(golden), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestEvalRun_EndToEnd(t *testing.T) {
	srv := fakeEmbeddingsServer(t, evalTestVec)

	dir := t.TempDir()
	corpusDir := writeEvalCorpus(t)
	// config validates paths.include non-empty and absolute, but eval never
	// reads it — point it at an arbitrary absolute directory.
	cfgPath := writeTestConfig(t, dir, dir, srv.URL)
	workDir := filepath.Join(dir, "work")
	outPath := filepath.Join(dir, "out", "results.json")

	var buf strings.Builder
	err := run([]string{
		"eval", "run",
		"--corpus", corpusDir,
		"--config", cfgPath,
		"--work-dir", workDir,
		"--out", outPath,
	}, &buf)
	if err != nil {
		t.Fatalf("run(eval run) = %v\noutput:\n%s", err, buf.String())
	}

	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("results file not written: %v", err)
	}

	res, err := evalharness.ReadResults(outPath)
	if err != nil {
		t.Fatalf("ReadResults() error = %v", err)
	}

	if res.Run.Queries != 3 {
		t.Errorf("Run.Queries = %d, want 3", res.Run.Queries)
	}
	if res.Aggregates.OverallNoExact.N != 1 {
		t.Errorf("OverallNoExact.N = %d, want 1 (exact and zero-answer queries excluded)", res.Aggregates.OverallNoExact.N)
	}
	if res.Aggregates.OverallNoExact.RecallAtK != 1.0 {
		t.Errorf("OverallNoExact.RecallAtK = %v, want 1.0", res.Aggregates.OverallNoExact.RecallAtK)
	}
	if len(res.Queries) == 0 || len(res.Queries[0].Ranked) == 0 {
		t.Fatalf("Queries[0].Ranked is empty, want a non-empty ranking")
	}
	for _, rd := range res.Queries[0].Ranked {
		if !strings.HasPrefix(rd.Path, "corpus/") {
			t.Errorf("ranked path %q does not have corpus/ prefix", rd.Path)
		}
	}
	if !strings.HasPrefix(res.Corpus.Version, "sha256:") {
		t.Errorf("Corpus.Version = %q, want sha256: prefix", res.Corpus.Version)
	}
	if res.Model.Dims != 3 {
		t.Errorf("Model.Dims = %d, want 3", res.Model.Dims)
	}
}

func TestEvalRun_SecondRunSkipsIndexing(t *testing.T) {
	srv := fakeEmbeddingsServer(t, evalTestVec)

	dir := t.TempDir()
	corpusDir := writeEvalCorpus(t)
	cfgPath := writeTestConfig(t, dir, dir, srv.URL)
	workDir := filepath.Join(dir, "work")

	runArgs := func(outPath string) []string {
		return []string{
			"eval", "run",
			"--corpus", corpusDir,
			"--config", cfgPath,
			"--work-dir", workDir,
			"--out", outPath,
		}
	}

	var buf strings.Builder
	firstOut := filepath.Join(dir, "first.json")
	if err := run(runArgs(firstOut), &buf); err != nil {
		t.Fatalf("first run: %v\noutput:\n%s", err, buf.String())
	}

	buf.Reset()
	secondOut := filepath.Join(dir, "second.json")
	if err := run(runArgs(secondOut), &buf); err != nil {
		t.Fatalf("second run: %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "up to date") {
		t.Errorf("second run output = %q, want it to mention up to date", buf.String())
	}

	res, err := evalharness.ReadResults(secondOut)
	if err != nil {
		t.Fatalf("ReadResults() error = %v", err)
	}
	if res.Run.IndexSeconds != 0 {
		t.Errorf("second run Run.IndexSeconds = %v, want 0 (no indexing work happened)", res.Run.IndexSeconds)
	}
	if res.Run.IndexedDocs != 0 {
		t.Errorf("second run Run.IndexedDocs = %d, want 0 (no indexing work happened)", res.Run.IndexedDocs)
	}
}

func TestRunEvalUsage(t *testing.T) {
	var out strings.Builder
	err := run([]string{"eval"}, &out)
	if err == nil {
		t.Fatal("run(eval) = nil, want usage error")
	}
	for _, want := range []string{"run", "compare", "summarize"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("run(eval) error = %v, want it to mention %q", err, want)
		}
	}
}

func TestRunEvalUnknownSubcommand(t *testing.T) {
	var out strings.Builder
	err := run([]string{"eval", "bogus"}, &out)
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("run(eval bogus) = %v, want unknown-subcommand error naming bogus", err)
	}
}

// evalCompareResult builds a minimal two-query Results value for
// eval-compare tests: q1 ("kw") always ties, q2 ("nl") varies by run so
// tests can control win/loss/tie outcomes via its scores.
func evalCompareResult(corpusVersion, modelName, fingerprint string, q2RR, q2Recall float64, q2Success int) evalharness.Results {
	return evalharness.Results{
		Bsearch: evalharness.BsearchInfo{Version: "test", ChunkerVersion: "1"},
		Corpus:  evalharness.CorpusInfo{Name: "synthetic-v1", Path: "/corpus", Version: corpusVersion},
		Model:   evalharness.ModelInfo{Name: modelName, Dims: 3, Fingerprint: fingerprint},
		Run:     evalharness.RunInfo{Queries: 2},
		Queries: []evalharness.QueryResult{
			{
				ID:         "q1",
				Query:      "rent going up",
				Tags:       []string{"kw"},
				Relevant:   []string{"corpus/letters/renewal.md"},
				QueryScore: evalharness.QueryScore{RecallAtK: 1.0, RR: 1.0, SuccessAt1: 1},
			},
			{
				ID:         "q2",
				Query:      "boiler invoice cost",
				Tags:       []string{"nl"},
				Relevant:   []string{"corpus/invoices/boiler.md"},
				QueryScore: evalharness.QueryScore{RecallAtK: q2Recall, RR: q2RR, SuccessAt1: q2Success},
			},
		},
	}
}

// writeCompareResults writes a and b under dir and returns their paths.
func writeCompareResults(t *testing.T, dir string, a, b evalharness.Results) (string, string) {
	t.Helper()
	aPath := filepath.Join(dir, "a.json")
	bPath := filepath.Join(dir, "b.json")
	if err := evalharness.WriteResults(aPath, a); err != nil {
		t.Fatalf("WriteResults(a) = %v", err)
	}
	if err := evalharness.WriteResults(bPath, b); err != nil {
		t.Fatalf("WriteResults(b) = %v", err)
	}
	return aPath, bPath
}

func TestEvalCompare_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	// B is strictly better on q2 (RR 1.0 vs 0.5) and ties on q1 — one win,
	// one tie, no losses.
	resA := evalCompareResult("sha256:abc", "model-a", "fpA", 0.5, 1.0, 0)
	resB := evalCompareResult("sha256:abc", "model-b", "fpB", 1.0, 1.0, 1)
	aPath, bPath := writeCompareResults(t, dir, resA, resB)

	var buf strings.Builder
	err := run([]string{"eval", "compare", aPath, bPath}, &buf)
	if err != nil {
		t.Fatalf("run(eval compare) = %v\noutput:\n%s", err, buf.String())
	}

	out := buf.String()
	if !strings.Contains(out, "win") {
		t.Errorf("output = %q, want it to mention win/loss/tie", out)
	}
	wantCaveat := "caveat: 2 scored queries gives modest power; consistent per-slice deltas beat single aggregate gaps."
	if !strings.Contains(out, wantCaveat) {
		t.Errorf("output = %q, want it to contain %q", out, wantCaveat)
	}
	if !strings.Contains(out, "model-a") || !strings.Contains(out, "model-b") {
		t.Errorf("output = %q, want it to name both models", out)
	}
	// Privacy: query text must never appear in compare output.
	if strings.Contains(out, "rent going up") || strings.Contains(out, "boiler invoice cost") {
		t.Errorf("output = %q, must not contain query text", out)
	}
}

func TestEvalCompare_JSON(t *testing.T) {
	dir := t.TempDir()
	resA := evalCompareResult("sha256:abc", "model-a", "fpA", 0.5, 1.0, 0)
	resB := evalCompareResult("sha256:abc", "model-b", "fpB", 1.0, 1.0, 1)
	aPath, bPath := writeCompareResults(t, dir, resA, resB)

	var buf strings.Builder
	err := run([]string{"eval", "compare", "--json", aPath, bPath}, &buf)
	if err != nil {
		t.Fatalf("run(eval compare --json) = %v\noutput:\n%s", err, buf.String())
	}

	var cmp evalharness.Comparison
	if err := json.NewDecoder(strings.NewReader(buf.String())).Decode(&cmp); err != nil {
		t.Fatalf("decode Comparison: %v\noutput:\n%s", err, buf.String())
	}
	if cmp.ModelA.Name != "model-a" || cmp.ModelB.Name != "model-b" {
		t.Errorf("ModelA/ModelB = %q/%q, want model-a/model-b", cmp.ModelA.Name, cmp.ModelB.Name)
	}
	if cmp.Overall.Wins != 1 || cmp.Overall.Losses != 0 || cmp.Overall.Ties != 1 {
		t.Errorf("Overall win/loss/tie = %d/%d/%d, want 1/0/1", cmp.Overall.Wins, cmp.Overall.Losses, cmp.Overall.Ties)
	}
}

func TestEvalCompare_VersionMismatch(t *testing.T) {
	dir := t.TempDir()
	resA := evalCompareResult("sha256:aaa", "model-a", "fpA", 0.5, 1.0, 0)
	resB := evalCompareResult("sha256:bbb", "model-b", "fpB", 1.0, 1.0, 1)
	aPath, bPath := writeCompareResults(t, dir, resA, resB)

	var buf strings.Builder
	err := run([]string{"eval", "compare", aPath, bPath}, &buf)
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("run(eval compare) = %v, want error mentioning version", err)
	}
}

func TestEvalCompare_WrongArgCount(t *testing.T) {
	var buf strings.Builder
	err := run([]string{"eval", "compare", "onlyone.json"}, &buf)
	if err == nil {
		t.Fatal("run(eval compare onlyone.json) = nil, want usage error")
	}
}

func TestRunEvalSummarizeNotImplemented(t *testing.T) {
	var out strings.Builder
	err := run([]string{"eval", "summarize"}, &out)
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("run(eval summarize) = %v, want not-implemented error", err)
	}
}

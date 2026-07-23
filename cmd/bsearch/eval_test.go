package main

import (
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

func TestRunEvalCompareNotImplemented(t *testing.T) {
	var out strings.Builder
	err := run([]string{"eval", "compare"}, &out)
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("run(eval compare) = %v, want not-implemented error", err)
	}
}

func TestRunEvalSummarizeNotImplemented(t *testing.T) {
	var out strings.Builder
	err := run([]string{"eval", "summarize"}, &out)
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("run(eval summarize) = %v, want not-implemented error", err)
	}
}

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// condenseTestVec is content-keyed (like evalTestVec) so the query and the
// three fixture documents map to controlled cosine directions: identical to
// the query (distance 0), close-but-not-identical (small positive
// distance), and orthogonal (large distance) respectively. This lets
// TestEvalRun_CondensesBeforeCutoff arrange an acceptable doc strictly
// nearer the query than the relevant doc, with a distractor furthest.
func condenseTestVec(_ int, input string) []float32 {
	switch {
	case strings.Contains(input, "alpha-marker"):
		return []float32{1, 0, 0}
	case strings.Contains(input, "beta-marker"):
		return []float32{0.9, 0.1, 0}
	default:
		return []float32{0, 1, 0}
	}
}

// writeCondenseTestCorpus builds a 3-document corpus and single golden query
// for Finding 1's regression case (spec §Scoring step 1: acceptable docs
// occupy no rank slot). "acceptable.md" shares the query's exact direction
// (nearest, distance 0), "relevant.md" is close but not identical (second
// nearest), and "distractor.md" is orthogonal (furthest). With --limit 1:
// collapsing to *limit* before condensing (the bug) truncates the ranking
// to just the acceptable doc, which condensing then removes entirely,
// scoring a miss; collapsing with the over-fetch cap and condensing
// afterward (the fix) drops the acceptable doc and leaves the relevant doc
// at condensed rank 1.
func writeCondenseTestCorpus(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	corpus := filepath.Join(root, "corpus")
	if err := os.MkdirAll(corpus, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"acceptable.md": "Document alpha-marker content, acceptable but not the target.\n",
		"relevant.md":   "Document beta-marker content, the actually relevant target.\n",
		"distractor.md": "Document gamma content, wholly unrelated distractor.\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(corpus, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	golden := `queries:
  - id: q1
    query: alpha-marker seeking content
    relevant:
      - corpus/relevant.md
    acceptable:
      - corpus/acceptable.md
    tags: [kw]
`
	if err := os.WriteFile(filepath.Join(root, "golden.yaml"), []byte(golden), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestEvalRun_CondensesBeforeCutoff(t *testing.T) {
	srv := fakeEmbeddingsServer(t, condenseTestVec)

	dir := t.TempDir()
	corpusDir := writeCondenseTestCorpus(t)
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
		"--limit", "1",
	}, &buf)
	if err != nil {
		t.Fatalf("run(eval run) = %v\noutput:\n%s", err, buf.String())
	}

	res, err := evalharness.ReadResults(outPath)
	if err != nil {
		t.Fatalf("ReadResults() error = %v", err)
	}
	if len(res.Queries) != 1 {
		t.Fatalf("len(Queries) = %d, want 1", len(res.Queries))
	}
	q := res.Queries[0]

	if q.RecallAtK != 1.0 {
		t.Errorf("RecallAtK = %v, want 1.0 (acceptable doc must not occupy the rank-1 slot)", q.RecallAtK)
	}
	if q.SuccessAt1 != 1 {
		t.Errorf("SuccessAt1 = %d, want 1", q.SuccessAt1)
	}
	if q.RR != 1.0 {
		t.Errorf("RR = %v, want 1.0", q.RR)
	}
	if len(q.Ranked) != 1 {
		t.Fatalf("len(Ranked) = %d, want 1 (recorded ranking is the uncondensed top --limit)", len(q.Ranked))
	}
	if q.Ranked[0].Path != "corpus/acceptable.md" {
		t.Errorf("Ranked[0].Path = %q, want corpus/acceptable.md (nearest doc, uncondensed)", q.Ranked[0].Path)
	}
}

// writeSymlinkedEvalCorpus builds the same fixture as writeEvalCorpus, but
// <root>/corpus is a symlink to a separate real directory holding the
// documents, rather than a real directory itself — Finding 6's regression
// case: discovery canonicalizes the full include root
// (<corpusDir>/corpus), so a hit's Document.Path lands under the symlink's
// target, not under <corpusDir>/corpus.
func writeSymlinkedEvalCorpus(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	realDocs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(realDocs, "letters"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(realDocs, "invoices"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDocs, "letters", "renewal.md"),
		[]byte("Your rent is going up this year.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDocs, "invoices", "boiler.md"),
		[]byte("Invoice DS-26417 for the boiler service.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDocs, filepath.Join(root, "corpus")); err != nil {
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
`
	if err := os.WriteFile(filepath.Join(root, "golden.yaml"), []byte(golden), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestEvalRun_SymlinkedCorpusSubdir asserts Finding 6's fix: when
// <corpusDir>/corpus is itself a symlink (not just an ancestor of
// --corpus), ranked hits must still relativize to clean "corpus/..." paths
// that match golden.yaml — not a path full of "../" that can never match,
// silently zeroing every score.
func TestEvalRun_SymlinkedCorpusSubdir(t *testing.T) {
	srv := fakeEmbeddingsServer(t, evalTestVec)

	dir := t.TempDir()
	corpusDir := writeSymlinkedEvalCorpus(t)
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

	res, err := evalharness.ReadResults(outPath)
	if err != nil {
		t.Fatalf("ReadResults() error = %v", err)
	}
	if res.Aggregates.Overall.RecallAtK != 1.0 {
		t.Errorf("Overall.RecallAtK = %v, want 1.0 (symlinked corpus/ must still match golden.yaml paths)", res.Aggregates.Overall.RecallAtK)
	}
	for _, q := range res.Queries {
		for _, rd := range q.Ranked {
			if !strings.HasPrefix(rd.Path, "corpus/") {
				t.Errorf("query %s: ranked path %q does not have corpus/ prefix", q.ID, rd.Path)
			}
			if strings.Contains(rd.Path, "..") {
				t.Errorf("query %s: ranked path %q contains .. (symlink not resolved correctly)", q.ID, rd.Path)
			}
		}
	}
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

// TestEvalRun_WorkDBKeyedByDocsVersion asserts Finding 4's fix: the work db
// filename folds in the corpus's DOCUMENT-set hash (evalharness.DocsVersion
// — corpus/manifest.json alone) and the embedding fingerprint, not the
// combined CorpusVersion (golden.yaml + manifest.json). Regenerating a
// corpus in place (new manifest, same name) can never reuse a stale db that
// still has a deleted document's vectors — discovery has no deletion pass
// to clean that up itself — while editing only golden.yaml keeps the same
// db key and reuses the index (TestEvalRun_SecondRunSkipsIndexing-style
// idempotency), because writeEvalCorpus's fixture has no manifest.json and
// DocsVersion falls back to the constant "nomanifest" either way.
func TestEvalRun_WorkDBKeyedByDocsVersion(t *testing.T) {
	srv := fakeEmbeddingsServer(t, evalTestVec)

	dir := t.TempDir()
	corpusDir := writeEvalCorpus(t)
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

	res, err := evalharness.ReadResults(outPath)
	if err != nil {
		t.Fatalf("ReadResults() error = %v", err)
	}
	docsVersion, err := evalharness.DocsVersion(corpusDir)
	if err != nil {
		t.Fatalf("DocsVersion() error = %v", err)
	}
	if docsVersion != "nomanifest" {
		t.Fatalf("DocsVersion(fixture with no manifest.json) = %q, want %q", docsVersion, "nomanifest")
	}
	fp8 := fmt.Sprintf("%x", sha256.Sum256([]byte(res.Model.Fingerprint)))[:8]

	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatalf("ReadDir(workDir) = %v", err)
	}
	var dbNames []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".db") {
			dbNames = append(dbNames, e.Name())
		}
	}
	if len(dbNames) != 1 {
		t.Fatalf("work dir .db files = %v, want exactly 1", dbNames)
	}
	if !strings.Contains(dbNames[0], docsVersion) {
		t.Errorf("db filename %q does not contain docs-version fragment %q", dbNames[0], docsVersion)
	}
	if !strings.Contains(dbNames[0], fp8) {
		t.Errorf("db filename %q does not contain fingerprint fragment %q", dbNames[0], fp8)
	}
}

// TestEvalRun_WorkDBUnaffectedByGoldenEdit asserts the other half of
// Finding 4: editing golden.yaml labels (query text, tags) alone — no
// change to corpus/ — must reuse the existing work db rather than minting
// a new key and re-embedding an unchanged document set. CorpusVersion (the
// results-file field) does change, since it hashes golden.yaml too; only
// the work-db key must stay put.
func TestEvalRun_WorkDBUnaffectedByGoldenEdit(t *testing.T) {
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
	firstEntries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatalf("ReadDir(workDir) = %v", err)
	}

	// Relabel q1's tag — golden.yaml content changes (CorpusVersion
	// changes), corpus/ does not (DocsVersion unchanged).
	goldenPath := filepath.Join(corpusDir, "golden.yaml")
	data, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden.yaml: %v", err)
	}
	edited := strings.Replace(string(data), "tags: [kw]", "tags: [kw, relabeled]", 1)
	if edited == string(data) {
		t.Fatal("editGolden: tags: [kw] not found in golden.yaml")
	}
	if err := os.WriteFile(goldenPath, []byte(edited), 0o600); err != nil {
		t.Fatalf("write golden.yaml: %v", err)
	}

	buf.Reset()
	secondOut := filepath.Join(dir, "second.json")
	if err := run(runArgs(secondOut), &buf); err != nil {
		t.Fatalf("second run: %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "up to date") {
		t.Errorf("second run output = %q, want it to mention up to date (no re-embed after a golden.yaml-only edit)", buf.String())
	}

	secondEntries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatalf("ReadDir(workDir) = %v", err)
	}
	if len(firstEntries) != 1 || len(secondEntries) != 1 || firstEntries[0].Name() != secondEntries[0].Name() {
		t.Errorf("work db filename changed after a golden.yaml-only edit: before=%v after=%v", firstEntries, secondEntries)
	}

	firstRes, err := evalharness.ReadResults(firstOut)
	if err != nil {
		t.Fatalf("ReadResults(first) error = %v", err)
	}
	secondRes, err := evalharness.ReadResults(secondOut)
	if err != nil {
		t.Fatalf("ReadResults(second) error = %v", err)
	}
	if firstRes.Corpus.Version == secondRes.Corpus.Version {
		t.Errorf("Corpus.Version unchanged despite editing golden.yaml: %q", firstRes.Corpus.Version)
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

// TestEvalRun_SkippedDocErrors asserts Finding 1's fix: an eval corpus that
// indexes incompletely (pipeline Summary.Skipped > 0) must fail the run —
// unlike production indexing, where an environmental skip is retried next
// run and never fails the invocation, an eval corpus that silently drops a
// document corrupts every score computed against it.
//
// Driving Summary.Skipped > 0 needs two runs against the same work db:
// discovery's cheap up-to-date check (size/mtime match, no reopen) means a
// file already known to the store is never reopened by discovery itself,
// only by the pipeline when the document still needs (re)processing. So:
// run 1 lets discovery read the file normally (state becomes discovered,
// then chunked) but a server rigged to fail passage-embedding requests
// aborts indexing before the document reaches state=indexed, leaving it
// chunked in the work db. Only then is the file chmod'd unreadable — size
// and mtime are untouched, so run 2's discovery scan takes the cheap path
// (no PathError) and hands the still-chunked document to the pipeline,
// which fails to open it and increments Summary.Skipped, exactly the "scan
// saw it, pipeline couldn't read it" scenario this fix targets.
func TestEvalRun_SkippedDocErrors(t *testing.T) {
	dir := t.TempDir()
	corpusRoot := t.TempDir()
	corpus := filepath.Join(corpusRoot, "corpus")
	if err := os.MkdirAll(corpus, 0o700); err != nil {
		t.Fatal(err)
	}
	docPath := filepath.Join(corpus, "doc.md")
	if err := os.WriteFile(docPath, []byte("Content for the skip-detection fixture.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	golden := `queries:
  - id: q1
    query: zero answer probe
    relevant: []
    tags: [zero-answer]
`
	if err := os.WriteFile(filepath.Join(corpusRoot, "golden.yaml"), []byte(golden), 0o600); err != nil {
		t.Fatal(err)
	}

	// The fake server answers only the pipeline's dimension probe
	// (single-input request containing "dimension probe") successfully;
	// every other request — i.e. passage embedding for doc.md's chunk —
	// gets HTTP 500, which openai.Transient classifies as retryable,
	// aborting pipeline.Run before the document is marked indexed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if len(req.Input) == 1 && strings.Contains(req.Input[0], "dimension probe") {
			type datum struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}
			var resp struct {
				Data []datum `json:"data"`
			}
			resp.Data = append(resp.Data, datum{Index: 0, Embedding: []float32{1, 0, 0}})
			if err := json.NewEncoder(w).Encode(&resp); err != nil {
				t.Fatalf("encode response: %v", err)
			}
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "embedding endpoint unavailable")
	}))
	t.Cleanup(srv.Close)

	cfgPath := writeTestConfig(t, dir, dir, srv.URL)
	workDir := filepath.Join(dir, "work")
	runArgs := func(outPath string) []string {
		return []string{
			"eval", "run",
			"--corpus", corpusRoot,
			"--config", cfgPath,
			"--work-dir", workDir,
			"--out", outPath,
		}
	}

	var buf strings.Builder
	// Run 1: rigged to fail passage embedding — expected to error and not
	// itself under test, just the setup that leaves doc.md chunked.
	if err := run(runArgs(filepath.Join(dir, "first.json")), &buf); err == nil {
		t.Fatalf("first run(eval run) = nil, want an embed error (server rigged to fail passage embedding)\noutput:\n%s", buf.String())
	}

	if err := os.Chmod(docPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(docPath, 0o600)
	})

	buf.Reset()
	err := run(runArgs(filepath.Join(dir, "second.json")), &buf)
	if err == nil {
		t.Fatalf("second run(eval run) = nil, want error (skipped doc must fail an eval run)\noutput:\n%s", buf.String())
	}
	if !strings.Contains(err.Error(), "skip") {
		t.Errorf("error = %v, want it to mention skip", err)
	}
	if !strings.Contains(err.Error(), "1") {
		t.Errorf("error = %v, want it to mention the skipped count (1)", err)
	}
}

// TestEvalRun_EmptyCorpusErrors asserts Finding 5's fix: a golden corpus
// whose corpus/ subtree has no scannable files (index.go's "scan reached no
// files" guard has no equivalent here) must fail the run rather than
// silently score every query against zero indexed documents. Unlike
// index.go's guard, this one fires regardless of PathErrors — a golden
// corpus must contain files, full stop.
func TestEvalRun_EmptyCorpusErrors(t *testing.T) {
	srv := fakeEmbeddingsServer(t, evalTestVec)

	dir := t.TempDir()
	corpusRoot := t.TempDir()
	// corpus/ exists but is empty — discovery scans it and finds nothing,
	// but that is not an error at the discovery layer (an empty directory
	// isn't a permission problem).
	if err := os.MkdirAll(filepath.Join(corpusRoot, "corpus"), 0o700); err != nil {
		t.Fatal(err)
	}
	golden := `queries:
  - id: q1
    query: zero answer probe
    relevant: []
    tags: [zero-answer]
`
	if err := os.WriteFile(filepath.Join(corpusRoot, "golden.yaml"), []byte(golden), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeTestConfig(t, dir, dir, srv.URL)
	workDir := filepath.Join(dir, "work")
	outPath := filepath.Join(dir, "out", "results.json")

	var buf strings.Builder
	err := run([]string{
		"eval", "run",
		"--corpus", corpusRoot,
		"--config", cfgPath,
		"--work-dir", workDir,
		"--out", outPath,
	}, &buf)
	if err == nil {
		t.Fatalf("run(eval run) = nil, want error (empty corpus/ must not silently score)\noutput:\n%s", buf.String())
	}
	if !strings.Contains(err.Error(), "no files") {
		t.Errorf("error = %v, want it to mention no files scanned", err)
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

// fakeChatServer duplicates evalharness's sseServer test helper (unexported
// there, so cmd/bsearch can't import it): serves body verbatim as the SSE
// response, flushing after every line so the client observes a stream
// rather than one buffered read.
func fakeChatServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("ResponseWriter does not support flushing")
		}
		for _, line := range strings.Split(body, "\n") {
			fmt.Fprintln(w, line)
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEvalSummarize_EndToEnd(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Summary: "}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"stuff."}}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	srv := fakeChatServer(t, sse)

	dir := t.TempDir()
	corpusDir := writeEvalCorpus(t)
	cfgPath := writeTestConfig(t, dir, dir, srv.URL)
	outDir := filepath.Join(dir, "summaries")

	var buf strings.Builder
	err := run([]string{
		"eval", "summarize",
		"--corpus", corpusDir,
		"--config", cfgPath,
		"--model", "test-sum",
		"--out-dir", outDir,
		"--docs", "2",
	}, &buf)
	if err != nil {
		t.Fatalf("run(eval summarize) = %v\noutput:\n%s", err, buf.String())
	}

	wantSummary := "Summary: stuff."
	for _, name := range []string{"invoices-boiler.md", "letters-renewal.md"} {
		content, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			t.Fatalf("read summary %s: %v", name, err)
		}
		if string(content) != wantSummary {
			t.Errorf("summary %s = %q, want %q", name, content, wantSummary)
		}
	}

	var metrics struct {
		Model string `json:"model"`
		Docs  []struct {
			Path             string  `json:"path"`
			PromptTokens     int     `json:"prompt_tokens"`
			CompletionTokens int     `json:"completion_tokens"`
			WallSeconds      float64 `json:"wall_seconds"`
			TokensPerSec     float64 `json:"tokens_per_sec"`
			UsageReported    bool    `json:"usage_reported"`
		} `json:"docs"`
		Aggregate struct {
			TotalCompletionTokens int     `json:"total_completion_tokens"`
			MeanTokensPerSec      float64 `json:"mean_tokens_per_sec"`
		} `json:"aggregate"`
	}
	metricsBytes, err := os.ReadFile(filepath.Join(outDir, "metrics.json"))
	if err != nil {
		t.Fatalf("read metrics.json: %v", err)
	}
	if err := json.Unmarshal(metricsBytes, &metrics); err != nil {
		t.Fatalf("decode metrics.json: %v\n%s", err, metricsBytes)
	}
	if metrics.Model != "test-sum" {
		t.Errorf("Model = %q, want %q", metrics.Model, "test-sum")
	}
	if len(metrics.Docs) != 2 {
		t.Fatalf("len(Docs) = %d, want 2", len(metrics.Docs))
	}
	if metrics.Docs[0].Path != "corpus/invoices/boiler.md" || metrics.Docs[1].Path != "corpus/letters/renewal.md" {
		t.Errorf("Docs paths = %q, %q, want corpus/invoices/boiler.md, corpus/letters/renewal.md",
			metrics.Docs[0].Path, metrics.Docs[1].Path)
	}
	for _, d := range metrics.Docs {
		if d.CompletionTokens != 5 {
			t.Errorf("Docs[%s].CompletionTokens = %d, want 5", d.Path, d.CompletionTokens)
		}
		if d.TokensPerSec <= 0 {
			t.Errorf("Docs[%s].TokensPerSec = %v, want > 0", d.Path, d.TokensPerSec)
		}
		if !d.UsageReported {
			t.Errorf("Docs[%s].UsageReported = false, want true (fake server sends a usage object)", d.Path)
		}
	}
	if metrics.Aggregate.TotalCompletionTokens != 10 {
		t.Errorf("TotalCompletionTokens = %d, want 10", metrics.Aggregate.TotalCompletionTokens)
	}
	if metrics.Aggregate.MeanTokensPerSec <= 0 {
		t.Errorf("MeanTokensPerSec = %v, want > 0", metrics.Aggregate.MeanTokensPerSec)
	}

	out := buf.String()
	if n := strings.Count(out, "summarized "); n != 2 {
		t.Errorf("output has %d %q lines, want 2\noutput:\n%s", n, "summarized ", out)
	}
	if strings.Contains(out, wantSummary) {
		t.Errorf("output = %q, must not contain summary text", out)
	}
	if !strings.Contains(out, "wrote 2 summaries + metrics.json to") {
		t.Errorf("output = %q, want final wrote-summary line", out)
	}
	if strings.Contains(out, "did not report token usage") {
		t.Errorf("output = %q, must not warn about missing usage (fake server always sends one)", out)
	}
}

// TestEvalSummarize_WarnsWhenUsageNotReported asserts Finding 7's fix: when
// the server never sends a usage object, eval summarize must print a
// visible warning that its token/throughput numbers are the unflagged SSE
// delta-count fallback, not silently report them as if the server had
// confirmed them.
func TestEvalSummarize_WarnsWhenUsageNotReported(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Summary: "}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"stuff."}}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	srv := fakeChatServer(t, sse)

	dir := t.TempDir()
	corpusDir := writeEvalCorpus(t)
	cfgPath := writeTestConfig(t, dir, dir, srv.URL)
	outDir := filepath.Join(dir, "summaries")

	var buf strings.Builder
	err := run([]string{
		"eval", "summarize",
		"--corpus", corpusDir,
		"--config", cfgPath,
		"--model", "test-sum",
		"--out-dir", outDir,
		"--docs", "2",
	}, &buf)
	if err != nil {
		t.Fatalf("run(eval summarize) = %v\noutput:\n%s", err, buf.String())
	}

	metricsBytes, err := os.ReadFile(filepath.Join(outDir, "metrics.json"))
	if err != nil {
		t.Fatalf("read metrics.json: %v", err)
	}
	var metrics struct {
		Docs []struct {
			UsageReported bool `json:"usage_reported"`
		} `json:"docs"`
	}
	if err := json.Unmarshal(metricsBytes, &metrics); err != nil {
		t.Fatalf("decode metrics.json: %v\n%s", err, metricsBytes)
	}
	for i, d := range metrics.Docs {
		if d.UsageReported {
			t.Errorf("Docs[%d].UsageReported = true, want false (server never sends usage)", i)
		}
	}

	out := buf.String()
	if !strings.Contains(out, "warning: server did not report token usage for 2 doc(s)") {
		t.Errorf("output = %q, want a warning naming the affected doc count", out)
	}
}

func TestEvalSummarize_MissingModel(t *testing.T) {
	dir := t.TempDir()
	corpusDir := writeEvalCorpus(t)

	var buf strings.Builder
	err := run([]string{
		"eval", "summarize",
		"--corpus", corpusDir,
		"--out-dir", filepath.Join(dir, "summaries"),
	}, &buf)
	if err == nil {
		t.Fatal("run(eval summarize) without --model = nil, want usage error")
	}
	if !strings.Contains(err.Error(), "model") {
		t.Errorf("error = %v, want it to mention --model", err)
	}
}

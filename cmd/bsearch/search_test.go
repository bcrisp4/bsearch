package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunSearchRequiresQuery(t *testing.T) {
	var out strings.Builder
	err := run([]string{"search"}, &out)
	if err == nil || !strings.Contains(err.Error(), "requires a query") {
		t.Fatalf("run(search) = %v, want requires-a-query error", err)
	}
}

func TestRunSearchRejectsExtraArgs(t *testing.T) {
	var out strings.Builder
	err := run([]string{"search", "heat", "pump"}, &out)
	if err == nil || !strings.Contains(err.Error(), "quote") {
		t.Fatalf("run(search heat pump) = %v, want error hinting to quote the query", err)
	}
	// Flags after the query hit the same path — the hint must cover it.
	out.Reset()
	err = run([]string{"search", "heat pump", "--json"}, &out)
	if err == nil || !strings.Contains(err.Error(), "flags go before the query") {
		t.Fatalf("run(search q --json) = %v, want flags-before-query hint", err)
	}
}

func TestRunSearchRejectsBlankQuery(t *testing.T) {
	var out strings.Builder
	err := run([]string{"search", "   "}, &out)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("run(search '   ') = %v, want empty-query error", err)
	}
}

func TestRunSearchBadLimit(t *testing.T) {
	for _, limit := range []string{"0", "-1", "1001"} {
		t.Run(limit, func(t *testing.T) {
			var out strings.Builder
			err := run([]string{"search", "--limit", limit, "q"}, &out)
			if err == nil || !strings.Contains(err.Error(), "out of range") {
				t.Fatalf("run(search --limit %s) = %v, want out-of-range error", limit, err)
			}
		})
	}
}

func TestKNNKClampedToVecCeiling(t *testing.T) {
	if got := knnK(10); got != 80 {
		t.Errorf("knnK(10) = %d, want 80", got)
	}
	// sqlite-vec rejects k > 4096 outright; the largest --limit values
	// must clamp rather than fail the query (found the hard way: any
	// --limit >= 513 used to error).
	if got := knnK(maxLimit); got != maxKNNK {
		t.Errorf("knnK(%d) = %d, want clamped to %d", maxLimit, got, maxKNNK)
	}
	if maxLimit > maxKNNK {
		t.Errorf("maxLimit %d > maxKNNK %d: clamped k could return fewer chunks than documents requested", maxLimit, maxKNNK)
	}
}

func TestRunSearchBadFlag(t *testing.T) {
	var out strings.Builder
	if err := run([]string{"search", "--nope", "q"}, &out); err == nil {
		t.Fatal("run(search --nope) = nil, want flag error")
	}
}

func TestRunSearchHelp(t *testing.T) {
	var out strings.Builder
	if err := run([]string{"search", "-h"}, &out); err != nil {
		t.Fatalf("run(search -h) = %v, want nil (help is not a failure)", err)
	}
	if !strings.Contains(out.String(), "usage: bsearch search") {
		t.Errorf("run(search -h) printed %q, want usage text", out.String())
	}
}

func TestRunSearchNoIndex(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTestConfig(t, dir, dir, "http://localhost:1")
	dbPath := filepath.Join(dir, "data", "bsearch.db")

	var out strings.Builder
	err := run([]string{"search", "--config", cfgPath, "--db", dbPath, "q"}, &out)
	if err == nil || !strings.Contains(err.Error(), "run 'bsearch index' first") {
		t.Fatalf("run(search, no db) = %v, want run-index-first error", err)
	}
	// A read-only command must not create the database as a side effect.
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Errorf("search created %s (stat err %v), want it untouched", dbPath, statErr)
	}
}

func TestRunSearchQueryTooLong(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	// Ceiling 80 tokens ≈ 320 bytes (domain.MinCeilingTokens floor).
	cfg := "[inference]\nendpoint = \"http://localhost:1\"\nembedding_model = \"test-model\"\ninput_ceiling_tokens = 80\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	err := run([]string{"search", "--config", cfgPath, "--db", filepath.Join(dir, "db"), strings.Repeat("x", 400)}, &out)
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("run(search, 400-byte query, 320-byte ceiling) = %v, want query-too-long error", err)
	}
}

// contentVec keys fake embeddings by input content: alpha → x-axis,
// beta → y-axis, else z-axis. Works on template-composed passage text
// since composition only prepends/wraps.
func contentVec(_ int, input string) []float32 {
	switch {
	case strings.Contains(input, "alpha"):
		return []float32{1, 0, 0}
	case strings.Contains(input, "beta"):
		return []float32{0, 1, 0}
	default:
		return []float32{0, 0, 1}
	}
}

// searchFixture indexes a temp corpus against a content-keyed fake
// embeddings server and returns config and db paths.
func searchFixture(t *testing.T) (cfgPath, dbPath string) {
	t.Helper()
	srv := fakeEmbeddingsServer(t, contentVec)

	dir := t.TempDir()
	// a.md has TWO alpha sections: collapse must return it once.
	corpus := writeTestCorpus(t, dir, map[string]string{
		"a.md": "# Alpha One\n\nalpha text here\n\n# Alpha Two\n\nmore alpha text\n",
		"b.md": "# Beta\n\nbeta text\n",
	})
	cfgPath = writeTestConfig(t, dir, corpus, srv.URL)
	dbPath = filepath.Join(dir, "data", "bsearch.db")

	var out strings.Builder
	if err := run([]string{"index", "--config", cfgPath, "--db", dbPath}, &out); err != nil {
		t.Fatalf("index fixture: %v\noutput:\n%s", err, out.String())
	}
	return cfgPath, dbPath
}

func TestRunSearchEndToEnd(t *testing.T) {
	cfgPath, dbPath := searchFixture(t)

	var out strings.Builder
	if err := run([]string{"search", "--config", cfgPath, "--db", dbPath, "alpha"}, &out); err != nil {
		t.Fatalf("search: %v\noutput:\n%s", err, out.String())
	}
	got := out.String()

	// Best doc first; the two alpha chunks collapsed into one hit.
	if n := strings.Count(got, "a.md"); n != 1 {
		t.Errorf("a.md appears %d times, want 1 (collapse to best chunk per doc)\noutput:\n%s", n, got)
	}
	first := strings.Index(got, "a.md")
	second := strings.Index(got, "b.md")
	if first == -1 || second == -1 || first > second {
		t.Errorf("want a.md ranked before b.md\noutput:\n%s", got)
	}
	if !strings.Contains(got, "distance") {
		t.Errorf("output missing distance\noutput:\n%s", got)
	}
	// Both alpha chunks embed identically (distance tie) — either section
	// may win; assert on their common substrings.
	if !strings.Contains(got, "alpha text") {
		t.Errorf("output missing chunk preview\noutput:\n%s", got)
	}
	if !strings.Contains(got, "Alpha") {
		t.Errorf("output missing heading path\noutput:\n%s", got)
	}
}

func TestRunSearchLargeLimitSucceeds(t *testing.T) {
	cfgPath, dbPath := searchFixture(t)

	// --limit 600 → un-clamped k would be 4800, over sqlite-vec's 4096
	// ceiling; the clamp must keep the query working.
	var out strings.Builder
	if err := run([]string{"search", "--config", cfgPath, "--db", dbPath, "--limit", "600", "alpha"}, &out); err != nil {
		t.Fatalf("search --limit 600: %v\noutput:\n%s", err, out.String())
	}
}

func TestRunSearchJSON(t *testing.T) {
	cfgPath, dbPath := searchFixture(t)

	var out strings.Builder
	if err := run([]string{"search", "--config", cfgPath, "--db", dbPath, "--json", "alpha"}, &out); err != nil {
		t.Fatalf("search --json: %v\noutput:\n%s", err, out.String())
	}

	var resp struct {
		Hits []struct {
			DocID        string  `json:"doc_id"`
			Path         string  `json:"path"`
			Distance     float64 `json:"distance"`
			ChunkPreview string  `json:"chunk_preview"`
			HeadingPath  string  `json:"heading_path"`
			Modified     string  `json:"modified"`
		} `json:"hits"`
		TookMS *int64 `json:"took_ms"`
	}
	if err := json.Unmarshal([]byte(out.String()), &resp); err != nil {
		t.Fatalf("decode JSON: %v\noutput:\n%s", err, out.String())
	}
	if len(resp.Hits) != 2 {
		t.Fatalf("hits = %d, want 2 (one per doc)\noutput:\n%s", len(resp.Hits), out.String())
	}
	if !strings.HasSuffix(resp.Hits[0].Path, "a.md") {
		t.Errorf("best hit path = %q, want a.md", resp.Hits[0].Path)
	}
	if !filepath.IsAbs(resp.Hits[0].Path) {
		t.Errorf("JSON path %q not absolute", resp.Hits[0].Path)
	}
	for i, h := range resp.Hits {
		if h.DocID == "" {
			t.Errorf("hit %d missing doc_id", i)
		}
		if h.Modified == "" {
			t.Errorf("hit %d missing modified", i)
		}
	}
	if resp.Hits[0].Distance > resp.Hits[1].Distance {
		t.Errorf("distances not ascending: %v then %v", resp.Hits[0].Distance, resp.Hits[1].Distance)
	}
	if resp.TookMS == nil || *resp.TookMS < 0 {
		t.Errorf("took_ms = %v, want >= 0", resp.TookMS)
	}
}

func TestWriteSearchJSONEmptyHits(t *testing.T) {
	// hits must encode as [], never null — JSON consumers iterate it.
	var out strings.Builder
	if err := writeSearchJSON(&out, nil, 0); err != nil {
		t.Fatal(err)
	}
	var resp map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out.String()), &resp); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(resp["hits"])); got != "[]" {
		t.Errorf("hits encoded as %s, want []", got)
	}
}

func TestRunSearchModelChanged(t *testing.T) {
	cfgPath, dbPath := searchFixture(t)

	// Rewrite config to a different model name (same fake server, same dims):
	// searching would hit the wrong vector space — must fail loud.
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(raw), `embedding_model = "test-model"`, `embedding_model = "other-model"`, 1)
	if changed == string(raw) {
		t.Fatal("fixture config did not contain expected model line")
	}
	if err := os.WriteFile(cfgPath, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	err = run([]string{"search", "--config", cfgPath, "--db", dbPath, "alpha"}, &out)
	if err == nil || !strings.Contains(err.Error(), "re-embed") {
		t.Fatalf("run(search, model changed) = %v, want built-with-model error", err)
	}
	for _, model := range []string{"test-model", "other-model"} {
		if !strings.Contains(err.Error(), model) {
			t.Errorf("error %q missing %q", err.Error(), model)
		}
	}
}

func TestRunSearchDimsChanged(t *testing.T) {
	cfgPath, dbPath := searchFixture(t)

	// Same model name, but the server now returns 4-dim vectors (the
	// operator swapped the model behind the name). The identity pre-flight
	// passes; the post-embed dims check must name the remedy.
	srv := fakeEmbeddingsServer(t, func(int, string) []float32 {
		return []float32{1, 0, 0, 0}
	})
	dir := filepath.Dir(cfgPath)
	newCfg := writeTestConfig(t, dir, dir, srv.URL)

	var out strings.Builder
	err := run([]string{"search", "--config", newCfg, "--db", dbPath, "alpha"}, &out)
	if err == nil || !strings.Contains(err.Error(), "run 'bsearch index'") {
		t.Fatalf("run(search, dims changed) = %v, want re-index error", err)
	}
	if !strings.Contains(err.Error(), "dimensions") {
		t.Errorf("error %q does not mention dimensions", err)
	}
}

func TestPreview(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"short passthrough", "hello world", 150, "hello world"},
		{"whitespace collapse", "a\n\nb\t c  d", 150, "a b c d"},
		{"leading and trailing whitespace trimmed", "  a b  ", 150, "a b"},
		{"exact limit not truncated", strings.Repeat("x", 150), 150, strings.Repeat("x", 150)},
		{"over limit truncated", strings.Repeat("x", 151), 150, strings.Repeat("x", 150) + "…"},
		{"multibyte at boundary", strings.Repeat("é", 151), 150, strings.Repeat("é", 150) + "…"},
		{"emoji at boundary", strings.Repeat("🐟", 151), 150, strings.Repeat("🐟", 150) + "…"},
		{"control chars stripped", "a\x1b]0;owned\x07b", 150, "a]0;ownedb"},
		{"ansi escape initiator stripped", "red \x1b[31mtext", 150, "red [31mtext"},
		{"empty", "", 150, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := preview(tt.in, tt.max); got != tt.want {
				t.Errorf("preview(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
		})
	}
}

func TestStripControl(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"clean passthrough", "~/notes/a b.md", "~/notes/a b.md"},
		{"esc stripped", "~/notes/\x1b]0;owned\x07.md", "~/notes/]0;owned.md"},
		{"newline and tab stripped", "~/no\ntes/a\t.md", "~/notes/a.md"},
		{"spaces preserved", "a  b", "a  b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripControl(tt.in); got != tt.want {
				t.Errorf("stripControl(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestTildePath(t *testing.T) {
	home := "/Users/testuser"
	tests := []struct {
		in, want string
	}{
		{filepath.Join(home, "notes", "a.md"), filepath.Join("~", "notes", "a.md")},
		{home, "~"},
		{"/opt/other/a.md", "/opt/other/a.md"},
		{home + "stuff/a.md", home + "stuff/a.md"}, // prefix but not a path boundary
	}
	for _, tt := range tests {
		if got := tildePath(home, tt.in); got != tt.want {
			t.Errorf("tildePath(%q, %q) = %q, want %q", home, tt.in, got, tt.want)
		}
	}
	if got := tildePath("", "/Users/x/a.md"); got != "/Users/x/a.md" {
		t.Errorf("tildePath with no home = %q, want path unchanged", got)
	}
}

package main

import (
	"encoding/json"
	"io"
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
	// Rejected locally: an obviously bad query should not need a running
	// daemon to be turned away.
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

func TestRunSearchWithoutADaemon(t *testing.T) {
	socketPath, _ := shortSocketPath(t) // never bound

	var out strings.Builder
	err := run([]string{"search", "--socket", socketPath, "alpha"}, &out)
	if err == nil {
		t.Fatal("run(search) with no daemon = nil, want an error")
	}
	// The fix is to start the daemon, so the error has to say so.
	if !strings.Contains(err.Error(), "not running") || !strings.Contains(err.Error(), "bsearch serve") {
		t.Errorf("error %q should say the daemon is not running and how to start it", err)
	}
}

func TestRunSearchAgainstAnEmptyIndex(t *testing.T) {
	f := startDaemon(t)

	var out strings.Builder
	err := run([]string{"search", "--socket", f.socketPath, "alpha"}, &out)
	if err == nil || !strings.Contains(err.Error(), "bsearch index") {
		t.Fatalf("run(search) with nothing indexed = %v, want a run-index error", err)
	}
	// The daemon's prose reaches the user directly — never the envelope.
	if strings.Contains(err.Error(), "{") {
		t.Errorf("error %q looks like raw JSON", err)
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

// indexedCorpus writes a two-document corpus, indexes it, and returns the
// config and database paths. a.md has two alpha sections, so collapse has
// something to collapse.
func indexedCorpus(t *testing.T) (cfgPath, dbPath string) {
	t.Helper()
	srv := fakeEmbeddingsServer(t, contentVec)
	dir := t.TempDir()
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

// searchFixture indexes a corpus and serves it, returning the daemon.
func searchFixture(t *testing.T) *daemonFixture {
	t.Helper()
	cfgPath, dbPath := indexedCorpus(t)
	return startDaemonWith(t, cfgPath, dbPath)
}

func TestRunSearchEndToEnd(t *testing.T) {
	f := searchFixture(t)

	var out strings.Builder
	if err := run([]string{"search", "--socket", f.socketPath, "alpha"}, &out); err != nil {
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
	f := searchFixture(t)

	// --limit 600 → un-clamped k would be 4800, over sqlite-vec's 4096
	// ceiling; the clamp must keep the query working.
	var out strings.Builder
	if err := run([]string{"search", "--socket", f.socketPath, "--limit", "600", "alpha"}, &out); err != nil {
		t.Fatalf("search --limit 600: %v\noutput:\n%s", err, out.String())
	}
}

func TestRunSearchJSON(t *testing.T) {
	f := searchFixture(t)

	var out strings.Builder
	if err := run([]string{"search", "--socket", f.socketPath, "--json", "alpha"}, &out); err != nil {
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

func TestRunSearchJSONIsTheDaemonsBytes(t *testing.T) {
	// --json copies the daemon's body through rather than re-encoding it,
	// so a field this binary has never heard of still reaches a consumer
	// that understands it.
	f := searchFixture(t)

	var out strings.Builder
	if err := run([]string{"search", "--socket", f.socketPath, "--json", "alpha"}, &out); err != nil {
		t.Fatalf("search --json: %v", err)
	}
	direct, status := f.post(t, "/v1/search", `{"query":"alpha","limit":10}`)
	if direct != 200 {
		t.Fatalf("direct search: status %d", direct)
	}
	// took_ms differs between the two calls; compare the hits only.
	if gotHits, wantHits := hitsOnly(t, out.String()), hitsOnly(t, string(status)); gotHits != wantHits {
		t.Errorf("CLI --json hits = %s, daemon hits = %s; want them identical", gotHits, wantHits)
	}
}

// hitsOnly extracts the raw hits array from a search response.
func hitsOnly(t *testing.T, body string) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return string(fields["hits"])
}

func TestRunSearchModelChanged(t *testing.T) {
	cfgPath, dbPath := indexedCorpus(t)

	// Point the daemon at a different model name (same fake server, same
	// dims): searching would hit the wrong vector space — must fail loud.
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(raw), `embedding_model = "test-model"`, `embedding_model = "other-model"`, 1)
	if changed == string(raw) {
		t.Fatal("fixture config did not contain expected model line")
	}
	changedPath := filepath.Join(filepath.Dir(cfgPath), "changed.toml")
	if err := os.WriteFile(changedPath, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startDaemonWith(t, changedPath, dbPath)

	var out strings.Builder
	err = run([]string{"search", "--socket", f.socketPath, "alpha"}, &out)
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
	cfgPath, dbPath := indexedCorpus(t)

	// Same model name, but the server now returns 4-dim vectors (the
	// operator swapped the model behind the name). The identity pre-flight
	// passes; the post-embed dims check must name the remedy.
	srv := fakeEmbeddingsServer(t, func(int, string) []float32 {
		return []float32{1, 0, 0, 0}
	})
	dir := filepath.Dir(cfgPath)
	newCfg := writeTestConfig(t, dir, dir, srv.URL)
	f := startDaemonWith(t, newCfg, dbPath)

	var out strings.Builder
	err := run([]string{"search", "--socket", f.socketPath, "alpha"}, &out)
	if err == nil || !strings.Contains(err.Error(), "run 'bsearch index'") {
		t.Fatalf("run(search, dims changed) = %v, want re-index error", err)
	}
	if !strings.Contains(err.Error(), "dimensions") {
		t.Errorf("error %q does not mention dimensions", err)
	}
}

func TestRunSearchStatusIsReachableWhileSearching(t *testing.T) {
	// Sanity that the CLI and a raw client can share one daemon: the CLI is
	// not a privileged path.
	f := searchFixture(t)

	if err := run([]string{"search", "--socket", f.socketPath, "alpha"}, io.Discard); err != nil {
		t.Fatalf("search: %v", err)
	}
	if status, body := f.get(t, "/v1/status"); status != 200 {
		t.Fatalf("status = %d (%s)", status, body)
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
		{"/Users/testuser/notes/a.md", "~/notes/a.md"},
		{"/Users/testuser", "~"},
		{"/Users/testuser2/notes/a.md", "/Users/testuser2/notes/a.md"},
		{"/etc/hosts", "/etc/hosts"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := tildePath(home, tt.in); got != tt.want {
				t.Errorf("tildePath(%q, %q) = %q, want %q", home, tt.in, got, tt.want)
			}
		})
	}
}

func TestTildePathWithoutHome(t *testing.T) {
	if got := tildePath("", "/Users/testuser/a.md"); got != "/Users/testuser/a.md" {
		t.Errorf("tildePath(\"\", path) = %q, want the path unchanged", got)
	}
}

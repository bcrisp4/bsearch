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

// Searching a corpus with no documents in it is not an error: there is
// nothing to find, and saying so is the honest answer. It used to be an error
// telling the user to run an indexing command, which no longer exists.
func TestRunSearchOverAnEmptyCorpus(t *testing.T) {
	f := startDaemonOverAnEmptyCorpus(t)
	f.waitForReady(t)

	var out strings.Builder
	if err := run([]string{"search", "--socket", f.socketPath, "alpha"}, &out); err != nil {
		t.Fatalf("run(search) over an empty corpus = %v, want no error", err)
	}
	if got := out.String(); !strings.Contains(got, "no results") {
		t.Errorf("run(search) printed %q, want it to say there are no results", got)
	}
}

// Before the daemon has had a chance to build anything, search must explain
// itself in the daemon's own prose — never a raw error envelope.
func TestRunSearchBeforeTheIndexExists(t *testing.T) {
	dir := t.TempDir()
	corpus := writeTestCorpus(t, dir, map[string]string{"alpha.md": "# Alpha\n\nalpha\n"})
	// No inference endpoint is reachable, so the daemon never gets as far as
	// creating a vector table — the state a user hits when their inference
	// server is not running yet.
	cfgPath := writeTestConfig(t, dir, corpus, "http://127.0.0.1:1")
	f := startDaemonWith(t, cfgPath, filepath.Join(dir, "data", "bsearch.db"))

	var out strings.Builder
	err := run([]string{"search", "--socket", f.socketPath, "alpha"}, &out)
	if err == nil {
		t.Fatal("run(search) with no vectors = nil, want an error")
	}
	// The advice must not be a command to run: there is no longer one.
	if strings.Contains(err.Error(), "bsearch index") {
		t.Errorf("error %q still tells the user to run a removed command", err)
	}
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

// testCorpus writes a two-document corpus and returns the config and database
// paths. a.md has two alpha sections, so collapse has something to collapse.
func testCorpus(t *testing.T) (cfgPath, dbPath string) {
	t.Helper()
	srv := fakeEmbeddingsServer(t, contentVec)
	dir := t.TempDir()
	corpus := writeTestCorpus(t, dir, map[string]string{
		"a.md": "# Alpha One\n\nalpha text here\n\n# Alpha Two\n\nmore alpha text\n",
		"b.md": "# Beta\n\nbeta text\n",
	})
	return writeTestConfig(t, dir, corpus, srv.URL), filepath.Join(dir, "data", "bsearch.db")
}

// searchFixture serves a corpus and waits for the daemon to index it. There
// is no indexing step to run: the daemon getting there on its own is the
// behaviour every test below is standing on.
func searchFixture(t *testing.T) *daemonFixture {
	t.Helper()
	cfgPath, dbPath := testCorpus(t)
	f := startDaemonWith(t, cfgPath, dbPath)
	f.waitForIndexed(t, 2)
	return f
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
			Path         string  `json:"path"`
			ContentHash  string  `json:"content_hash"`
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
		t.Fatalf("hits = %d, want 2 (one per content)\noutput:\n%s", len(resp.Hits), out.String())
	}
	if !strings.HasSuffix(resp.Hits[0].Path, "a.md") {
		t.Errorf("best hit path = %q, want a.md", resp.Hits[0].Path)
	}
	if !filepath.IsAbs(resp.Hits[0].Path) {
		t.Errorf("JSON path %q not absolute", resp.Hits[0].Path)
	}
	for i, h := range resp.Hits {
		if h.Path == "" {
			t.Errorf("hit %d missing path", i)
		}
		if h.ContentHash == "" {
			t.Errorf("hit %d missing content_hash", i)
		}
		if h.Modified == "" {
			t.Errorf("hit %d missing modified", i)
		}
	}
	// doc_id is retired from every surface (ADR 0015), and each corpus file
	// is unique content, so also_at must be omitted rather than [].
	for _, gone := range []string{`"doc_id"`, `"also_at"`} {
		if strings.Contains(out.String(), gone) {
			t.Errorf("output carries %s, want it absent\noutput:\n%s", gone, out.String())
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

// Changing the embedding model used to mean running an indexing command; with
// that command gone, the daemon has to notice the corpus was built by a
// superseded model and re-embed it, or the index would silently stay in the
// old vector space forever. This is the stale sweep, end to end.
//
// The mismatch *message*, which the user sees during the window before the
// re-embed completes, is unit-tested in internal/search
// (TestSearchModelMismatch, TestSearchDimsMismatch).
func TestChangingTheModelReEmbedsTheCorpus(t *testing.T) {
	cfgPath, dbPath := testCorpus(t)

	first := startDaemonWith(t, cfgPath, dbPath)
	first.waitForIndexed(t, 2)
	// Only one daemon at a time: shutdown signals this process.
	first.stop(t)

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

	second := startDaemonWith(t, changedPath, dbPath)
	second.waitForIndexed(t, 2)

	// Searching must work again, against the new model's vectors.
	var out strings.Builder
	if err := run([]string{"search", "--socket", second.socketPath, "alpha"}, &out); err != nil {
		t.Fatalf("search after the model change: %v\noutput:\n%s", err, out.String())
	}

	_, body := second.get(t, "/v1/status")
	var st struct {
		Index struct {
			Ready bool   `json:"ready"`
			Model string `json:"model"`
		} `json:"index"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("decode status: %v (%s)", err, body)
	}
	if !st.Index.Ready || st.Index.Model != "other-model" {
		t.Errorf("index = %+v, want ready under other-model", st.Index)
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

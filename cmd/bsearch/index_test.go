package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunIndexRejectsArgs(t *testing.T) {
	var out strings.Builder
	err := run([]string{"index", "some/path"}, &out)
	if err == nil || !strings.Contains(err.Error(), "no arguments") {
		t.Fatalf("run(index some/path) = %v, want no-arguments error", err)
	}
}

func TestRunIndexBadFlag(t *testing.T) {
	var out strings.Builder
	if err := run([]string{"index", "--nope"}, &out); err == nil {
		t.Fatal("run(index --nope) = nil, want flag error")
	}
}

func TestRunIndexHelp(t *testing.T) {
	var out strings.Builder
	if err := run([]string{"index", "-h"}, &out); err != nil {
		t.Fatalf("run(index -h) = %v, want nil (help is not a failure)", err)
	}
	if !strings.Contains(out.String(), "usage: bsearch index") {
		t.Errorf("run(index -h) printed %q, want usage text", out.String())
	}
}

func TestRunIndexRequiresEmbeddingModel(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[inference]\nendpoint = \"http://localhost:1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err := run([]string{"index", "--config", cfgPath, "--db", filepath.Join(dir, "db")}, &out)
	if err == nil || !strings.Contains(err.Error(), "embedding_model") {
		t.Fatalf("run(index) = %v, want embedding_model error", err)
	}
}

// TestRunIndexEndToEnd is the M1 demo in test form: a temp corpus, a fake
// OpenAI-compatible embeddings server, two runs — the second fully skips.
func TestRunIndexEndToEnd(t *testing.T) {
	srv := fakeEmbeddingsServer(t, func(n int, _ string) []float32 {
		return []float32{float32(n), 1, 2}
	})

	dir := t.TempDir()
	corpus := writeTestCorpus(t, dir, map[string]string{
		"a.md": "# Alpha\n\nalpha text\n",
		"b.md": "# Beta\n\nbeta text\n",
	})
	cfgPath := writeTestConfig(t, dir, corpus, srv.URL)
	dbPath := filepath.Join(dir, "data", "bsearch.db")
	args := []string{"index", "--config", cfgPath, "--db", dbPath}

	var out strings.Builder
	if err := run(args, &out); err != nil {
		t.Fatalf("first run: %v\noutput:\n%s", err, out.String())
	}
	if got := out.String(); !strings.Contains(got, "done: 2 indexed, 0 up to date, 0 failed") {
		t.Errorf("first run output:\n%s", got)
	}

	out.Reset()
	if err := run(args, &out); err != nil {
		t.Fatalf("second run: %v\noutput:\n%s", err, out.String())
	}
	if got := out.String(); !strings.Contains(got, "done: 0 indexed, 2 up to date, 0 failed") {
		t.Errorf("second run output (want fully up to date):\n%s", got)
	}
}

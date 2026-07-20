package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		type datum struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var resp struct {
			Data []datum `json:"data"`
		}
		for n := range req.Input {
			resp.Data = append(resp.Data, datum{Index: n, Embedding: []float32{float32(n), 1, 2}})
		}
		if err := json.NewEncoder(w).Encode(&resp); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	corpus := filepath.Join(dir, "notes")
	if err := os.MkdirAll(corpus, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corpus, "a.md"), []byte("# Alpha\n\nalpha text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corpus, "b.md"), []byte("# Beta\n\nbeta text\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.toml")
	cfg := fmt.Sprintf("[paths]\ninclude = [%q]\n\n[inference]\nendpoint = %q\nembedding_model = \"test-model\"\n", corpus, srv.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
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

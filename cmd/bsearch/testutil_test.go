package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeEmbeddingsServer runs an OpenAI-compatible /embeddings endpoint whose
// vectors are chosen by vecFor(position, input) — position-keyed for the
// index tests, content-keyed for the search tests. One fake per package so
// a response-shape change is edited in exactly one place.
func fakeEmbeddingsServer(t *testing.T, vecFor func(n int, input string) []float32) *httptest.Server {
	t.Helper()
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
		for n, input := range req.Input {
			resp.Data = append(resp.Data, datum{Index: n, Embedding: vecFor(n, input)})
		}
		if err := json.NewEncoder(w).Encode(&resp); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeTestCorpus writes the given files into dir/notes and returns that
// corpus directory.
func writeTestCorpus(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	corpus := filepath.Join(dir, "notes")
	if err := os.MkdirAll(corpus, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(corpus, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return corpus
}

// writeTestConfig writes a config.toml indexing corpus against the given
// inference endpoint (model "test-model") and returns its path.
func writeTestConfig(t *testing.T, dir, corpus, endpoint string) string {
	t.Helper()
	cfgPath := filepath.Join(dir, "config.toml")
	cfg := fmt.Sprintf("[paths]\ninclude = [%q]\n\n[inference]\nendpoint = %q\nembedding_model = \"test-model\"\n", corpus, endpoint)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

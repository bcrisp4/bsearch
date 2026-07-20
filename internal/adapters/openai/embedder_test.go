package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
)

var testSpec = domain.EmbeddingSpec{
	Model:           "test-model",
	QueryTemplate:   "query: {q}",
	PassageTemplate: "title: {t} | text: {d}",
}

// echoServer answers /embeddings with one deterministic vector per input:
// [float(position), 1] — position within the request, so tests can verify
// end-to-end ordering.
func echoServer(t *testing.T, requests *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests != nil {
			requests.Add(1)
		}
		var req embeddingsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		var resp embeddingsResponse
		for n := range req.Input {
			resp.Data = append(resp.Data, struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{Index: n, Embedding: []float32{float32(n), 1}})
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fixedBodyServer answers every request with the given body.
func fixedBodyServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestEmbedder(t *testing.T, cfg EmbedderConfig) *Embedder {
	t.Helper()
	e, err := NewEmbedder(cfg)
	if err != nil {
		t.Fatalf("NewEmbedder = %v", err)
	}
	return e
}

func TestNewEmbedderValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  EmbedderConfig
	}{
		{name: "empty endpoint", cfg: EmbedderConfig{Spec: testSpec}},
		{name: "empty model", cfg: EmbedderConfig{Endpoint: "http://localhost:1234/v1"}},
		{name: "negative batch size", cfg: EmbedderConfig{
			Endpoint: "http://localhost:1234/v1", Spec: testSpec, BatchSize: -1,
		}},
		{name: "invalid spec rejected at choke point", cfg: EmbedderConfig{
			Endpoint: "http://localhost:1234/v1",
			Spec: domain.EmbeddingSpec{
				Model:           "m",
				PassageTemplate: strings.Repeat("x", domain.TemplateReserveBytes+1) + "{d}",
			},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewEmbedder(tt.cfg); err == nil {
				t.Error("NewEmbedder = nil error, want error")
			}
		})
	}
}

func TestEmbedQueryComposesAndReturnsVector(t *testing.T) {
	var gotBody embeddingsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %s, want /v1/embeddings", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		fmt.Fprint(w, `{"data":[{"index":0,"embedding":[0.1,0.2,0.3]}]}`)
	}))
	t.Cleanup(srv.Close)

	e := newTestEmbedder(t, EmbedderConfig{Endpoint: srv.URL + "/v1", Spec: testSpec})
	vec, err := e.EmbedQuery(t.Context(), "heat pump")
	if err != nil {
		t.Fatalf("EmbedQuery = %v", err)
	}
	if gotBody.Model != "test-model" {
		t.Errorf("request model = %q, want test-model", gotBody.Model)
	}
	if want := []string{"query: heat pump"}; len(gotBody.Input) != 1 || gotBody.Input[0] != want[0] {
		t.Errorf("request input = %v, want %v (query template applied)", gotBody.Input, want)
	}
	if len(vec) != 3 || vec[0] != 0.1 {
		t.Errorf("vector = %v, want [0.1 0.2 0.3]", vec)
	}
}

func TestEmbedPassagesComposesBreadcrumbs(t *testing.T) {
	var gotInput []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embeddingsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		gotInput = req.Input
		fmt.Fprint(w, `{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[2]}]}`)
	}))
	t.Cleanup(srv.Close)

	e := newTestEmbedder(t, EmbedderConfig{Endpoint: srv.URL + "/v1", Spec: testSpec})
	chunks := []domain.Chunk{
		{Text: "first", HeadingPath: "Doc > A"},
		{Text: "second"},
	}
	vecs, err := e.EmbedPassages(t.Context(), chunks)
	if err != nil {
		t.Fatalf("EmbedPassages = %v", err)
	}
	want := []string{"title: Doc > A | text: first", "title: none | text: second"}
	if len(gotInput) != 2 || gotInput[0] != want[0] || gotInput[1] != want[1] {
		t.Errorf("request input = %v, want %v (passage template applied)", gotInput, want)
	}
	if len(vecs) != 2 || vecs[0][0] != 1 || vecs[1][0] != 2 {
		t.Errorf("vectors = %v, want [[1] [2]]", vecs)
	}
}

func TestEmbedPassagesBatches(t *testing.T) {
	var requests atomic.Int64
	srv := echoServer(t, &requests)

	e := newTestEmbedder(t, EmbedderConfig{Endpoint: srv.URL, Spec: testSpec, BatchSize: 64})
	chunks := make([]domain.Chunk, 130)
	for n := range chunks {
		chunks[n] = domain.Chunk{Text: fmt.Sprintf("chunk %d", n)}
	}
	vecs, err := e.EmbedPassages(t.Context(), chunks)
	if err != nil {
		t.Fatalf("EmbedPassages = %v", err)
	}
	if got := requests.Load(); got != 3 {
		t.Errorf("requests = %d, want 3 (130 inputs / batch 64)", got)
	}
	if len(vecs) != 130 {
		t.Fatalf("len(vecs) = %d, want 130", len(vecs))
	}
	// Echo vectors encode position-within-request: batch boundaries at 64
	// and 128 restart the sequence. Order preserved end to end.
	for n, vec := range vecs {
		if want := float32(n % 64); vec[0] != want {
			t.Fatalf("vecs[%d][0] = %v, want %v", n, vec[0], want)
		}
	}
}

func TestEmbedReordersByIndexField(t *testing.T) {
	// Shuffled response order; index field is authoritative.
	srv := fixedBodyServer(t, `{"data":[
		{"index":2,"embedding":[2]},
		{"index":0,"embedding":[0]},
		{"index":1,"embedding":[1]}]}`)

	e := newTestEmbedder(t, EmbedderConfig{Endpoint: srv.URL, Spec: testSpec})
	vecs, err := e.EmbedPassages(t.Context(), []domain.Chunk{{Text: "a"}, {Text: "b"}, {Text: "c"}})
	if err != nil {
		t.Fatalf("EmbedPassages = %v", err)
	}
	for n := range vecs {
		if vecs[n][0] != float32(n) {
			t.Errorf("vecs[%d][0] = %v, want %d (reordered by index)", n, vecs[n][0], n)
		}
	}
}

func TestEmbedMalformedResponses(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantSub string
	}{
		{
			name:    "count mismatch",
			body:    `{"data":[{"index":0,"embedding":[1]}]}`,
			wantSub: "sent 2 inputs, got 1",
		},
		{
			name:    "duplicate index",
			body:    `{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[2]}]}`,
			wantSub: "duplicate embedding index 0",
		},
		{
			name:    "index out of range",
			body:    `{"data":[{"index":0,"embedding":[1]},{"index":5,"embedding":[2]}]}`,
			wantSub: "index 5 out of range",
		},
		{
			name:    "empty embedding",
			body:    `{"data":[{"index":0,"embedding":[]},{"index":1,"embedding":[1]}]}`,
			wantSub: "empty embedding at index 0",
		},
		{
			name:    "dims mismatch within batch",
			body:    `{"data":[{"index":0,"embedding":[1,2]},{"index":1,"embedding":[1]}]}`,
			wantSub: "dimension mismatch at index 1: got 1, want 2",
		},
		{
			name:    "not json",
			body:    `<html>oops</html>`,
			wantSub: "decode response",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := fixedBodyServer(t, tt.body)

			e := newTestEmbedder(t, EmbedderConfig{Endpoint: srv.URL, Spec: testSpec})
			_, err := e.EmbedPassages(t.Context(), []domain.Chunk{{Text: "a"}, {Text: "b"}})
			if err == nil {
				t.Fatal("EmbedPassages = nil error, want error")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not mention %q", err, tt.wantSub)
			}
		})
	}
}

func TestEmbedDimsMismatchAcrossBatches(t *testing.T) {
	var call atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if call.Add(1) == 1 {
			fmt.Fprint(w, `{"data":[{"index":0,"embedding":[1,2]}]}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"index":0,"embedding":[1,2,3]}]}`)
	}))
	t.Cleanup(srv.Close)

	e := newTestEmbedder(t, EmbedderConfig{Endpoint: srv.URL, Spec: testSpec, BatchSize: 1})
	_, err := e.EmbedPassages(t.Context(), []domain.Chunk{{Text: "a"}, {Text: "b"}})
	if err == nil || !strings.Contains(err.Error(), "dimension mismatch") {
		t.Errorf("EmbedPassages = %v, want cross-batch dimension mismatch error", err)
	}
}

func TestEmbedErrorClassification(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		wantTransient bool
	}{
		{name: "500 transient", status: 500, wantTransient: true},
		{name: "429 transient", status: 429, wantTransient: true},
		{name: "408 transient", status: 408, wantTransient: true},
		{name: "400 permanent", status: 400, wantTransient: false},
		{name: "404 permanent", status: 404, wantTransient: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "model not loaded", tt.status)
			}))
			t.Cleanup(srv.Close)

			e := newTestEmbedder(t, EmbedderConfig{Endpoint: srv.URL, Spec: testSpec})
			_, err := e.EmbedQuery(t.Context(), "q")
			if err == nil {
				t.Fatal("EmbedQuery = nil error, want error")
			}
			if got := Transient(err); got != tt.wantTransient {
				t.Errorf("Transient(%v) = %v, want %v", err, got, tt.wantTransient)
			}
			if !strings.Contains(err.Error(), "model not loaded") {
				t.Errorf("error %q does not carry response body", err)
			}
		})
	}
}

func TestEmbedClientTimeoutIsTransient(t *testing.T) {
	// The adapter's own http.Client timeout surfaces as
	// context.DeadlineExceeded; a slow-but-alive server is retry
	// territory, never a permanent failure.
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
	}))
	t.Cleanup(func() {
		close(blocked)
		srv.Close()
	})

	e := newTestEmbedder(t, EmbedderConfig{
		Endpoint:   srv.URL,
		Spec:       testSpec,
		HTTPClient: &http.Client{Timeout: 20 * time.Millisecond},
	})
	_, err := e.EmbedQuery(t.Context(), "q")
	if err == nil {
		t.Fatal("EmbedQuery = nil error, want timeout error")
	}
	if !Transient(err) {
		t.Errorf("Transient(%v) = false, want true for client timeout", err)
	}
}

func TestEmbedConnectionRefusedIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // now nothing listens on srv.URL

	e := newTestEmbedder(t, EmbedderConfig{Endpoint: srv.URL, Spec: testSpec})
	_, err := e.EmbedQuery(t.Context(), "q")
	if err == nil {
		t.Fatal("EmbedQuery = nil error, want connection error")
	}
	if !Transient(err) {
		t.Errorf("Transient(%v) = false, want true for connection refused", err)
	}
}

func TestEmbedContextCancellation(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
	}))
	t.Cleanup(func() {
		close(blocked)
		srv.Close()
	})

	ctx, cancel := context.WithCancel(t.Context())
	e := newTestEmbedder(t, EmbedderConfig{Endpoint: srv.URL, Spec: testSpec})
	done := make(chan error, 1)
	go func() {
		_, err := e.EmbedQuery(ctx, "q")
		done <- err
	}()
	cancel()
	err := <-done
	if err == nil {
		t.Fatal("EmbedQuery = nil error, want cancellation error")
	}
	if Transient(err) {
		t.Errorf("Transient(%v) = true, want false for context cancellation", err)
	}
}

func TestEmbedCeilingGuardBeforeHTTP(t *testing.T) {
	var requests atomic.Int64
	srv := echoServer(t, &requests)

	spec := testSpec
	spec.CeilingTokens = 10 // ≈ 40 bytes
	e := newTestEmbedder(t, EmbedderConfig{Endpoint: srv.URL, Spec: spec})
	_, err := e.EmbedPassages(t.Context(), []domain.Chunk{
		{Text: strings.Repeat("x", 100)},
	})
	if err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("EmbedPassages = %v, want ceiling error", err)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("requests = %d, want 0 (guard must fire before HTTP)", got)
	}
}

func TestEmbedPassagesEmptyInput(t *testing.T) {
	var requests atomic.Int64
	srv := echoServer(t, &requests)

	e := newTestEmbedder(t, EmbedderConfig{Endpoint: srv.URL, Spec: testSpec})
	vecs, err := e.EmbedPassages(t.Context(), nil)
	if err != nil {
		t.Fatalf("EmbedPassages = %v", err)
	}
	if vecs != nil {
		t.Errorf("vecs = %v, want nil", vecs)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("requests = %d, want 0", got)
	}
}

func TestEndpointTrailingSlashJoin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %s, want /v1/embeddings", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":[{"index":0,"embedding":[1]}]}`)
	}))
	t.Cleanup(srv.Close)

	e := newTestEmbedder(t, EmbedderConfig{Endpoint: srv.URL + "/v1/", Spec: testSpec})
	if _, err := e.EmbedQuery(t.Context(), "q"); err != nil {
		t.Fatalf("EmbedQuery = %v", err)
	}
}

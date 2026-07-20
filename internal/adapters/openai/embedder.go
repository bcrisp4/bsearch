// Package openai is the OpenAI-compatible HTTP adapter for inference
// endpoints (LM Studio, Ollama, vLLM, …). It is pure transport: prefix
// templates and breadcrumbs are composed by domain.EmbeddingSpec, and
// the resolved spec arrives via internal/embedding.ResolveSpec — this
// package never decides what text to embed, only how to ship it.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
)

var _ domain.Embedder = (*Embedder)(nil)

const (
	defaultTimeout   = 60 * time.Second
	defaultBatchSize = 64
	// maxErrorBody caps how much of an error response is kept for the
	// StatusError message.
	maxErrorBody = 2048
)

// EmbedderConfig configures NewEmbedder. Endpoint and Spec.Model are
// required; the rest defaults.
type EmbedderConfig struct {
	// Endpoint is the OpenAI-compatible base URL, e.g.
	// "http://localhost:1234/v1".
	Endpoint string
	// Spec is the resolved embedding spec (embedding.ResolveSpec).
	Spec domain.EmbeddingSpec
	// HTTPClient defaults to a client with a 60 s timeout.
	HTTPClient *http.Client
	// BatchSize is the number of inputs per HTTP request; default 64.
	BatchSize int
}

// Embedder implements domain.Embedder over POST {endpoint}/embeddings.
type Embedder struct {
	url    string
	spec   domain.EmbeddingSpec
	client *http.Client
	batch  int
}

// NewEmbedder validates cfg and builds an Embedder.
func NewEmbedder(cfg EmbedderConfig) (*Embedder, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("openai embedder: endpoint must not be empty")
	}
	if cfg.Spec.Model == "" {
		return nil, errors.New("openai embedder: model must not be empty")
	}
	if err := cfg.Spec.Validate(); err != nil {
		return nil, fmt.Errorf("openai embedder: %w", err)
	}
	if cfg.BatchSize < 0 {
		return nil, fmt.Errorf("openai embedder: batch size %d is negative", cfg.BatchSize)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	batch := cfg.BatchSize
	if batch == 0 {
		batch = defaultBatchSize
	}
	return &Embedder{
		url:    strings.TrimSuffix(cfg.Endpoint, "/") + "/embeddings",
		spec:   cfg.Spec,
		client: client,
		batch:  batch,
	}, nil
}

// Spec reports the identity recorded in pipeline metadata.
func (e *Embedder) Spec() domain.EmbeddingSpec { return e.spec }

// EmbedQuery embeds one search query with the model's query template.
//
// The shared HTTP client's timeout is sized for bulk passage batches
// (60 s default); interactive callers own the search latency SLO and
// should bound ctx with their own, much shorter deadline.
func (e *Embedder) EmbedQuery(ctx context.Context, query string) ([]float32, error) {
	vecs, err := e.embed(ctx, []string{e.spec.ComposeQuery(query)})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

// EmbedPassages embeds chunks for indexing, applying the passage template
// and each chunk's heading-path breadcrumb. The result is index-aligned
// with chunks.
func (e *Embedder) EmbedPassages(ctx context.Context, chunks []domain.Chunk) ([][]float32, error) {
	if len(chunks) == 0 {
		return nil, nil
	}
	texts := make([]string, len(chunks))
	for n, c := range chunks {
		texts[n] = e.spec.ComposePassage(c)
	}
	return e.embed(ctx, texts)
}

// embed ships composed texts in batches and returns one vector per text,
// order preserved. Any batch failure fails the whole call — the caller's
// vector upsert is idempotent, so partial results buy nothing.
func (e *Embedder) embed(ctx context.Context, texts []string) ([][]float32, error) {
	// Ceiling guard: the chunker already budgets template+breadcrumb
	// headroom, so an over-ceiling composed text means a bug or a
	// config/model mismatch — fail loudly, never truncate (DESIGN.md:
	// Chunking, hard ceiling).
	if ceil := e.spec.CeilingTokens; ceil > 0 {
		limit := ceil * domain.BytesPerToken
		for n, text := range texts {
			if len(text) > limit {
				return nil, fmt.Errorf(
					"input %d exceeds embedding input ceiling (%d tokens ≈ %d bytes): %d bytes composed",
					n, ceil, limit, len(text))
			}
		}
	}

	vectors := make([][]float32, 0, len(texts))
	dims := 0
	for start := 0; start < len(texts); start += e.batch {
		batch := texts[start:min(start+e.batch, len(texts))]
		got, err := e.embedBatch(ctx, batch)
		if err != nil {
			return nil, err
		}
		// A model cannot change dimensions mid-corpus: the first batch
		// fixes dims for the whole call.
		if dims == 0 {
			dims = len(got[0])
		}
		if len(got[0]) != dims {
			return nil, fmt.Errorf("embed batch: dimension mismatch across batches: got %d, want %d",
				len(got[0]), dims)
		}
		vectors = append(vectors, got...)
	}
	return vectors, nil
}

type embeddingsRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embeddingsResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// embedBatch is one POST /embeddings round trip. All vectors in the
// response must share one dimension; the caller checks consistency
// across batches.
func (e *Embedder) embedBatch(ctx context.Context, batch []string) ([][]float32, error) {
	body, err := json.Marshal(embeddingsRequest{Model: e.spec.Model, Input: batch})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed batch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, fmt.Errorf("embed batch: %w",
			&StatusError{StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(msg))})
	}

	var decoded embeddingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("embed batch: decode response: %w", err)
	}
	if len(decoded.Data) != len(batch) {
		return nil, fmt.Errorf("embed batch: sent %d inputs, got %d embeddings", len(batch), len(decoded.Data))
	}

	// Place vectors by the response index field — OpenAI-compatible
	// servers don't guarantee order.
	vectors := make([][]float32, len(batch))
	dims := 0
	for _, item := range decoded.Data {
		if item.Index < 0 || item.Index >= len(batch) {
			return nil, fmt.Errorf("embed batch: embedding index %d out of range [0, %d)", item.Index, len(batch))
		}
		if vectors[item.Index] != nil {
			return nil, fmt.Errorf("embed batch: duplicate embedding index %d", item.Index)
		}
		if len(item.Embedding) == 0 {
			return nil, fmt.Errorf("embed batch: empty embedding at index %d", item.Index)
		}
		if dims == 0 {
			dims = len(item.Embedding)
		}
		if len(item.Embedding) != dims {
			return nil, fmt.Errorf("embed batch: dimension mismatch at index %d: got %d, want %d",
				item.Index, len(item.Embedding), dims)
		}
		vectors[item.Index] = item.Embedding
	}
	return vectors, nil
}

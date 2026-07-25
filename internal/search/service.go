// Package search is the query path: validate a request, check that the index
// and the configured embedding model still agree, embed the query, run KNN
// over the current vector generation, and collapse chunk hits to one hit per
// document.
//
// It is the single implementation shared by every caller — the daemon's HTTP
// handler and the eval harness — so the over-fetch policy, the compatibility
// gate, and the response shape cannot drift between how bsearch is measured
// and how it answers. The CLI reaches it over the socket, not directly
// (ADR 0010).
package search

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/bcrisp4/bsearch/internal/domain"
)

const (
	// DefaultLimit is the document limit when a request omits one
	// (DESIGN.md: search response).
	DefaultLimit = 10

	// OverFetchFactor over-fetches chunk hits before collapsing to
	// best-chunk-per-document, so documents with many matching chunks
	// can't crowd others out of the top limit. Echoes DESIGN.md's
	// top-k×8 rescore factor; brute-force KNN scans every vector
	// regardless of k, so a larger k is nearly free.
	OverFetchFactor = 8

	// MaxKNNK is sqlite-vec's KNN k ceiling (SQLITE_VEC_VEC0_K_MAX) —
	// a k above it fails the query outright, so over-fetch clamps here.
	// Above MaxKNNK/OverFetchFactor the over-fetch factor degrades
	// gracefully instead of erroring.
	MaxKNNK = 4096

	// MaxLimit bounds the document limit; must not exceed MaxKNNK or the
	// clamped k could return fewer chunks than documents requested.
	MaxLimit = 1000

	// PreviewRunes is the chunk-preview length (DESIGN.md: ~150 chars).
	PreviewRunes = 150
)

// Index is the read side of the vector store the query path needs. Declared
// here, at the consumer, so this package depends on no storage adapter.
type Index interface {
	// CurrentVecSpec reports the embedding identity and dimensions the
	// index was built with, or domain.ErrNoVecTable if nothing is indexed.
	CurrentVecSpec(ctx context.Context) (domain.EmbeddingSpec, int, error)
	// SearchVectors returns the limit nearest chunks by ascending distance.
	SearchVectors(ctx context.Context, query []float32, limit int) ([]domain.Hit, error)
}

// Service answers search requests against one index with one embedder.
// Safe for concurrent use: it holds no mutable state.
type Service struct {
	index    Index
	embedder domain.Embedder
}

// New builds a Service over an index and the embedder whose spec must match
// the one the index was built with.
func New(index Index, embedder domain.Embedder) *Service {
	return &Service{index: index, embedder: embedder}
}

// Request is one search. The JSON tags are the wire contract (ADR 0009):
// every field DESIGN.md publishes is accepted, including ones this build
// cannot act on yet, so a caller written against the design doc is never
// rejected for asking correctly.
type Request struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
	// Mode is hybrid | semantic | keyword. Hybrid is DESIGN.md's default
	// and, until BM25 fusion lands, runs semantic.
	Mode string `json:"mode,omitempty"`
	// SummaryLevel and MinScore are accepted and ignored: there are no
	// summaries to select and no calibrated score to filter on.
	SummaryLevel int     `json:"summary_level,omitempty"`
	MinScore     float64 `json:"min_score,omitempty"`
}

// Hit is one document in the response, built from its best-matching chunk.
type Hit struct {
	DocID string `json:"doc_id"`
	Path  string `json:"path"`
	// Distance is the raw KNN distance: lower is better, uncalibrated.
	// DESIGN.md reserves `score` for fused (RRF) ranking so the name never
	// carries two meanings.
	Distance     float64   `json:"distance"`
	ChunkPreview string    `json:"chunk_preview"`
	HeadingPath  string    `json:"heading_path,omitempty"`
	Modified     time.Time `json:"modified"`
}

// Response is the search result set.
type Response struct {
	Hits   []Hit `json:"hits"`
	TookMS int64 `json:"took_ms"`
}

// Search answers one request. Errors wrap the package sentinels; the message
// is user-facing and rendered verbatim by clients.
func (s *Service) Search(ctx context.Context, req Request) (Response, error) {
	start := time.Now()

	limit, err := req.Validate()
	if err != nil {
		return Response{}, err
	}
	spec := s.embedder.Spec()
	if err := req.CheckLength(spec); err != nil {
		return Response{}, err
	}

	// Read the index's identity per request, never cached: the daemon's
	// scheduler can cut the vector generation over at any moment, and a
	// drop-and-reindex replaces the file outright (ADR 0009). Doing it before
	// embedding also fails fast, ahead of the network call.
	indexed, dims, err := s.index.CurrentVecSpec(ctx)
	if err != nil {
		if errors.Is(err, domain.ErrNoVecTable) {
			return Response{}, fmt.Errorf("%w: nothing indexed yet — the daemon indexes in the background; check 'bsearch serve' is running", ErrNotIndexed)
		}
		return Response{}, err
	}
	if err := compatible(indexed, spec); err != nil {
		return Response{}, err
	}

	vec, err := s.embedder.EmbedQuery(ctx, req.Query)
	if err != nil {
		// A deadline says "too slow", not "unreachable" — keep them apart,
		// they send the user to different places.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return Response{}, err
		}
		return Response{}, fmt.Errorf("%w: %w", ErrEmbedder, err)
	}
	// Same model name, different dimensions: the model behind the name
	// changed on the inference server. The store would reject this too, but
	// with a raw table error; name the remedy.
	if len(vec) != dims {
		return Response{}, fmt.Errorf("%w: the embedding server returned %d dimensions but the index was built with %d — the model behind %q changed; the daemon re-embeds automatically, so this clears itself once it catches up",
			ErrIndexMismatch, len(vec), dims, spec.Model)
	}

	hits, err := s.index.SearchVectors(ctx, vec, KnnK(limit))
	if err != nil {
		if errors.Is(err, domain.ErrNoVecTable) {
			return Response{}, fmt.Errorf("%w: the index was re-built while this query ran — try again", ErrNotIndexed)
		}
		return Response{}, err
	}

	docs := domain.CollapseBestPerDoc(hits, limit)
	resp := Response{Hits: make([]Hit, 0, len(docs)), TookMS: time.Since(start).Milliseconds()}
	for _, h := range docs {
		resp.Hits = append(resp.Hits, Hit{
			DocID:        h.Doc.ID,
			Path:         h.Doc.Path,
			Distance:     h.Distance,
			ChunkPreview: Preview(h.Chunk.Text, PreviewRunes),
			HeadingPath:  h.Chunk.HeadingPath,
			Modified:     h.Doc.MTime.UTC(),
		})
	}
	return resp, nil
}

// Validate checks everything about a request that needs no configuration and
// no I/O, returning the effective document limit (DefaultLimit when the
// request omits one). Search calls it; callers that want to reject a bad
// request before paying for setup may call it first — it is idempotent, so
// there is still exactly one definition of a valid request.
func (r Request) Validate() (int, error) {
	if strings.TrimSpace(r.Query) == "" {
		return 0, fmt.Errorf("%w: the query is empty", ErrInvalidRequest)
	}
	switch r.Mode {
	case "", "hybrid", "semantic":
		// Hybrid is DESIGN.md's default and degrades to semantic until
		// BM25 fusion lands — accepting it keeps design-doc-shaped callers
		// working rather than failing them for being correct.
	case "keyword":
		return 0, fmt.Errorf("%w: keyword-only search is not implemented yet (it lands with hybrid search)", ErrInvalidRequest)
	default:
		return 0, fmt.Errorf("%w: unknown mode %q (want hybrid, semantic, or keyword)", ErrInvalidRequest, r.Mode)
	}

	limit := r.Limit
	if limit == 0 {
		limit = DefaultLimit
	}
	if limit < 1 || limit > MaxLimit {
		return 0, fmt.Errorf("%w: limit %d is out of range [1, %d]", ErrInvalidRequest, r.Limit, MaxLimit)
	}
	return limit, nil
}

// CheckLength rejects a query too long for the configured model. It is
// separate from Validate because it needs the model's spec: a caller that
// wants to fail before opening an index calls Validate first and this once
// the embedder is built. The embedder enforces the ceiling too, but its
// error is phrased for indexing bugs ("composed"); a pasted-in over-long
// query deserves a message that names the actual problem. Same heuristic
// byte budget as the embedder's guard.
func (r Request) CheckLength(spec domain.EmbeddingSpec) error {
	if spec.CeilingTokens <= 0 {
		return nil
	}
	composed := spec.ComposeQuery(r.Query)
	if len(composed) <= spec.CeilingTokens*domain.BytesPerToken {
		return nil
	}
	return fmt.Errorf("%w: the query is too long for the embedding model (limit %d tokens ≈ %d bytes; it composes to %d bytes) — shorten it",
		ErrInvalidRequest, spec.CeilingTokens, spec.CeilingTokens*domain.BytesPerToken, len(composed))
}

// compatible fails when the index was built under a different embedding
// identity than the one now configured. Dimensions alone can't catch a model
// swapped for another of equal width, which would silently search the wrong
// vector space, so the whole struct is compared — a future identity field
// can't be forgotten here.
func compatible(indexed, configured domain.EmbeddingSpec) error {
	want := configured
	want.CeilingTokens = 0 // a chunking budget, not part of the vector space
	if indexed == want {
		return nil
	}
	// Both remedies are named: the index may be stale, or the daemon may be
	// running with a configuration older than the one that built the index.
	if indexed.Model != want.Model {
		return fmt.Errorf("%w: the index was built with model %q but the configuration says %q — restart the daemon, which re-embeds the corpus in the background under the configured model",
			ErrIndexMismatch, indexed.Model, want.Model)
	}
	return fmt.Errorf("%w: the index was built with a different embedding configuration for model %q (prefix templates changed?) — restart the daemon, which re-embeds the corpus in the background under the configured settings",
		ErrIndexMismatch, want.Model)
}

// KnnK is the chunk over-fetch for a document limit, clamped to sqlite-vec's
// k ceiling. MaxLimit <= MaxKNNK keeps k >= limit after clamping.
func KnnK(limit int) int {
	return min(limit*OverFetchFactor, MaxKNNK)
}

// Preview renders untrusted document text for one line of output: whitespace
// runs collapse to single spaces, control characters are dropped (a crafted
// document must not drive the terminal via escape sequences), and the result
// is truncated to maxRunes runes with an ellipsis. Truncation is
// rune-boundary safe (grapheme clusters may still split; acceptable for a
// preview). Single pass — never allocates proportional to the full text.
func Preview(text string, maxRunes int) string {
	var b strings.Builder
	runes := 0
	pendingSpace := false
	for _, r := range text {
		if unicode.IsSpace(r) {
			pendingSpace = runes > 0
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		need := 1
		if pendingSpace {
			need = 2
		}
		if runes+need > maxRunes {
			return b.String() + "…"
		}
		if pendingSpace {
			b.WriteByte(' ')
			runes++
			pendingSpace = false
		}
		b.WriteRune(r)
		runes++
	}
	return b.String()
}

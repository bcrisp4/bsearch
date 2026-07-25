package search_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
	"github.com/bcrisp4/bsearch/internal/search"
)

// indexedSpec is the embedding identity both the fake index and the fake
// embedder agree on unless a test deliberately diverges them.
var indexedSpec = domain.EmbeddingSpec{
	Model:           "test-model",
	QueryTemplate:   "query: {q}",
	PassageTemplate: "passage: {d}",
}

type fakeIndex struct {
	spec     domain.EmbeddingSpec
	dims     int
	specErr  error
	hits     []domain.Hit
	searchEr error
	gotLimit int
}

func (f *fakeIndex) CurrentVecSpec(context.Context) (domain.EmbeddingSpec, int, error) {
	if f.specErr != nil {
		return domain.EmbeddingSpec{}, 0, f.specErr
	}
	return f.spec, f.dims, nil
}

func (f *fakeIndex) SearchVectors(_ context.Context, _ []float32, limit int) ([]domain.Hit, error) {
	f.gotLimit = limit
	if f.searchEr != nil {
		return nil, f.searchEr
	}
	return f.hits, nil
}

type fakeEmbedder struct {
	spec domain.EmbeddingSpec
	vec  []float32
	err  error
}

func (f *fakeEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.vec, nil
}

func (f *fakeEmbedder) EmbedPassages(context.Context, []domain.Chunk) ([][]float32, error) {
	return nil, errors.New("not used by the query path")
}

func (f *fakeEmbedder) Spec() domain.EmbeddingSpec { return f.spec }

func hit(docID, path, text string, distance float64) domain.Hit {
	return domain.Hit{
		Doc:      domain.Document{ID: docID, Path: path, MTime: time.Unix(1700000000, 0)},
		Chunk:    domain.Chunk{DocID: docID, Text: text, HeadingPath: "Doc > Section"},
		Distance: distance,
	}
}

// newService wires a service whose index and embedder agree, with one hit.
func newService(t *testing.T) (*search.Service, *fakeIndex, *fakeEmbedder) {
	t.Helper()
	index := &fakeIndex{
		spec: indexedSpec,
		dims: 3,
		hits: []domain.Hit{hit("d_1", "/notes/a.md", "alpha text", 0.1)},
	}
	embedder := &fakeEmbedder{spec: indexedSpec, vec: []float32{1, 0, 0}}
	return search.New(index, embedder), index, embedder
}

func TestSearchReturnsHits(t *testing.T) {
	svc, _, _ := newService(t)

	resp, err := svc.Search(context.Background(), search.Request{Query: "alpha", Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(resp.Hits))
	}
	got := resp.Hits[0]
	if got.DocID != "d_1" || got.Path != "/notes/a.md" {
		t.Errorf("hit = %+v, want doc d_1 at /notes/a.md", got)
	}
	if got.Distance != 0.1 {
		t.Errorf("distance = %v, want 0.1", got.Distance)
	}
	if got.ChunkPreview != "alpha text" {
		t.Errorf("chunk_preview = %q, want %q", got.ChunkPreview, "alpha text")
	}
	if got.HeadingPath != "Doc > Section" {
		t.Errorf("heading_path = %q, want %q", got.HeadingPath, "Doc > Section")
	}
	if got.Modified.Location() != time.UTC {
		t.Errorf("modified = %v, want a UTC timestamp", got.Modified)
	}
	if resp.TookMS < 0 {
		t.Errorf("took_ms = %d, want >= 0", resp.TookMS)
	}
}

func TestSearchNeverReturnsNilHits(t *testing.T) {
	svc, index, _ := newService(t)
	index.hits = nil

	resp, err := svc.Search(context.Background(), search.Request{Query: "alpha"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// A nil slice marshals to JSON null; the wire contract says an empty
	// result set is [], because consumers iterate it.
	if resp.Hits == nil {
		t.Fatal("Hits is nil; want an empty slice so it marshals to []")
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := string(fields["hits"]); got != "[]" {
		t.Errorf("hits encoded as %s, want []", got)
	}
}

func TestSearchOverFetchesChunks(t *testing.T) {
	svc, index, _ := newService(t)

	if _, err := svc.Search(context.Background(), search.Request{Query: "alpha", Limit: 5}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if want := search.KnnK(5); index.gotLimit != want {
		t.Errorf("SearchVectors limit = %d, want the over-fetch %d", index.gotLimit, want)
	}
}

func TestSearchDefaultsLimit(t *testing.T) {
	svc, index, _ := newService(t)

	if _, err := svc.Search(context.Background(), search.Request{Query: "alpha"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if want := search.KnnK(search.DefaultLimit); index.gotLimit != want {
		t.Errorf("SearchVectors limit = %d, want the default-limit over-fetch %d", index.gotLimit, want)
	}
}

func TestSearchCollapsesToBestChunkPerDoc(t *testing.T) {
	svc, index, _ := newService(t)
	// Ascending by distance — the SearchVectors contract, which
	// CollapseBestPerDoc relies on to know a document's first hit is its best.
	index.hits = []domain.Hit{
		hit("d_1", "/notes/a.md", "better chunk", 0.1),
		hit("d_2", "/notes/b.md", "other doc", 0.2),
		hit("d_1", "/notes/a.md", "worse chunk", 0.4),
	}

	resp, err := svc.Search(context.Background(), search.Request{Query: "alpha", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Hits) != 2 {
		t.Fatalf("got %d hits, want 2 (one per document)", len(resp.Hits))
	}
	if resp.Hits[0].DocID != "d_1" || resp.Hits[0].ChunkPreview != "better chunk" {
		t.Errorf("first hit = %+v, want d_1's best chunk", resp.Hits[0])
	}
}

func TestSearchInvalidRequests(t *testing.T) {
	tests := []struct {
		name    string
		req     search.Request
		wantMsg string
	}{
		{"empty query", search.Request{Query: ""}, "empty"},
		{"blank query", search.Request{Query: "   \t\n"}, "empty"},
		{"negative limit", search.Request{Query: "a", Limit: -1}, "out of range"},
		{"limit over max", search.Request{Query: "a", Limit: search.MaxLimit + 1}, "out of range"},
		{"keyword mode", search.Request{Query: "a", Mode: "keyword"}, "keyword"},
		{"unknown mode", search.Request{Query: "a", Mode: "vibes"}, "mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, _ := newService(t)
			_, err := svc.Search(context.Background(), tt.req)
			if !errors.Is(err, search.ErrInvalidRequest) {
				t.Fatalf("Search(%+v) error = %v, want ErrInvalidRequest", tt.req, err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tt.wantMsg)
			}
		})
	}
}

func TestSearchAcceptsPublishedModes(t *testing.T) {
	// DESIGN.md publishes hybrid/semantic/keyword; hybrid is the documented
	// default and must not fail against a semantic-only build, or every
	// caller written against the design doc breaks.
	for _, mode := range []string{"", "hybrid", "semantic"} {
		svc, _, _ := newService(t)
		if _, err := svc.Search(context.Background(), search.Request{Query: "alpha", Mode: mode}); err != nil {
			t.Errorf("Search(mode=%q) = %v, want success", mode, err)
		}
	}
}

func TestSearchIgnoresUnimplementedTuning(t *testing.T) {
	// summary_level and min_score are accepted-and-ignored until summaries
	// and fused scoring exist (ADR 0009); they must not fail the request.
	svc, _, _ := newService(t)
	_, err := svc.Search(context.Background(), search.Request{Query: "alpha", SummaryLevel: 64, MinScore: 0.5})
	if err != nil {
		t.Errorf("Search(summary_level, min_score) = %v, want success", err)
	}
}

func TestSearchRejectsQueryOverCeiling(t *testing.T) {
	svc, _, embedder := newService(t)
	embedder.spec.CeilingTokens = domain.MinCeilingTokens

	long := strings.Repeat("x", domain.MinCeilingTokens*domain.BytesPerToken+1)
	_, err := svc.Search(context.Background(), search.Request{Query: long})
	if !errors.Is(err, search.ErrInvalidRequest) {
		t.Fatalf("Search(over-ceiling) error = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "too long") {
		t.Errorf("error %q does not say the query is too long", err)
	}
}

func TestSearchNotIndexed(t *testing.T) {
	svc, index, _ := newService(t)
	index.specErr = domain.ErrNoVecTable

	_, err := svc.Search(context.Background(), search.Request{Query: "alpha"})
	if !errors.Is(err, search.ErrNotIndexed) {
		t.Fatalf("Search(no vec table) error = %v, want ErrNotIndexed", err)
	}
}

func TestSearchModelMismatch(t *testing.T) {
	svc, _, embedder := newService(t)
	embedder.spec.Model = "other-model"

	_, err := svc.Search(context.Background(), search.Request{Query: "alpha"})
	if !errors.Is(err, search.ErrIndexMismatch) {
		t.Fatalf("Search(model changed) error = %v, want ErrIndexMismatch", err)
	}
	for _, want := range []string{"test-model", "other-model"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestSearchTemplateMismatch(t *testing.T) {
	svc, _, embedder := newService(t)
	embedder.spec.QueryTemplate = "different: {q}"

	_, err := svc.Search(context.Background(), search.Request{Query: "alpha"})
	if !errors.Is(err, search.ErrIndexMismatch) {
		t.Fatalf("Search(template changed) error = %v, want ErrIndexMismatch", err)
	}
}

func TestSearchIgnoresCeilingInSpecComparison(t *testing.T) {
	// The input ceiling is a chunking budget, not part of the vector space:
	// changing it must not lock the user out of their index.
	svc, _, embedder := newService(t)
	embedder.spec.CeilingTokens = 4096

	if _, err := svc.Search(context.Background(), search.Request{Query: "alpha"}); err != nil {
		t.Errorf("Search(ceiling differs) = %v, want success", err)
	}
}

func TestSearchDimsMismatch(t *testing.T) {
	svc, index, _ := newService(t)
	index.dims = 768 // the embedder's fake returns 3

	_, err := svc.Search(context.Background(), search.Request{Query: "alpha"})
	if !errors.Is(err, search.ErrIndexMismatch) {
		t.Fatalf("Search(dims changed) error = %v, want ErrIndexMismatch", err)
	}
	if !strings.Contains(err.Error(), "dimensions") {
		t.Errorf("error %q does not mention dimensions", err)
	}
}

func TestSearchEmbedderFailure(t *testing.T) {
	svc, _, embedder := newService(t)
	embedder.err = errors.New("connection refused")

	_, err := svc.Search(context.Background(), search.Request{Query: "alpha"})
	if !errors.Is(err, search.ErrEmbedder) {
		t.Fatalf("Search(embedder down) error = %v, want ErrEmbedder", err)
	}
}

func TestSearchEmbedderTimeoutIsNotAnEmbedderError(t *testing.T) {
	// A deadline must stay distinguishable from an unreachable embedder:
	// they point the user at completely different problems.
	svc, _, embedder := newService(t)
	embedder.err = context.DeadlineExceeded

	_, err := svc.Search(context.Background(), search.Request{Query: "alpha"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Search(deadline) error = %v, want context.DeadlineExceeded", err)
	}
	if errors.Is(err, search.ErrEmbedder) {
		t.Error("a deadline was reported as ErrEmbedder; it must not be")
	}
}

func TestSearchStoreFailure(t *testing.T) {
	svc, index, _ := newService(t)
	index.searchEr = errors.New("disk gone")

	_, err := svc.Search(context.Background(), search.Request{Query: "alpha"})
	if err == nil {
		t.Fatal("Search(store failure) = nil error, want error")
	}
	for _, sentinel := range []error{search.ErrInvalidRequest, search.ErrNotIndexed, search.ErrIndexMismatch, search.ErrEmbedder} {
		if errors.Is(err, sentinel) {
			t.Errorf("store failure classified as %v; want an unclassified error", sentinel)
		}
	}
}

func TestSearchVecGenerationCutoverIsNotIndexed(t *testing.T) {
	// bsearch index runs in another process and can swap the vector
	// generation between the spec read and the KNN query; the table vanishing
	// under us is "not indexed right now", not a server fault (ADR 0009).
	svc, index, _ := newService(t)
	index.searchEr = fmt.Errorf("vector table vec_chunks_3 was retired mid-query: %w", domain.ErrNoVecTable)

	_, err := svc.Search(context.Background(), search.Request{Query: "alpha"})
	if !errors.Is(err, search.ErrNotIndexed) {
		t.Fatalf("Search(generation cutover) error = %v, want ErrNotIndexed", err)
	}
}

func TestKnnKClampedToVecCeiling(t *testing.T) {
	// sqlite-vec rejects k above SQLITE_VEC_VEC0_K_MAX outright, so the
	// over-fetch must clamp rather than error.
	if got := search.KnnK(search.MaxLimit); got > search.MaxKNNK {
		t.Errorf("KnnK(%d) = %d, want <= %d", search.MaxLimit, got, search.MaxKNNK)
	}
	if got := search.KnnK(search.MaxLimit); got < search.MaxLimit {
		t.Errorf("KnnK(%d) = %d, want >= the document limit", search.MaxLimit, got)
	}
	if got, want := search.KnnK(10), 10*search.OverFetchFactor; got != want {
		t.Errorf("KnnK(10) = %d, want %d", got, want)
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
			if got := search.Preview(tt.in, tt.max); got != tt.want {
				t.Errorf("Preview(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
		})
	}
}

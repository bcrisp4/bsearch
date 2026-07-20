package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/adapters/sqlite"
	"github.com/bcrisp4/bsearch/internal/domain"
)

// fakeEmbedder is a deterministic domain.Embedder: every vector is
// [len(text), 1, 2, 3], so dims are stable and tests stay cgo-free on the
// inference side while the store side runs real SQLite.
type fakeEmbedder struct {
	spec         domain.EmbeddingSpec
	queryCalls   int
	passageCalls int
	queryErr     error
	// passageErrOn fails EmbedPassages when the first chunk's text contains
	// this substring; empty never fails.
	passageErrOn string
	passageErr   error
}

func (f *fakeEmbedder) EmbedQuery(_ context.Context, query string) ([]float32, error) {
	f.queryCalls++
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return []float32{float32(len(query)), 1, 2, 3}, nil
}

func (f *fakeEmbedder) EmbedPassages(_ context.Context, chunks []domain.Chunk) ([][]float32, error) {
	f.passageCalls++
	if f.passageErrOn != "" && len(chunks) > 0 && strings.Contains(chunks[0].Text, f.passageErrOn) {
		return nil, f.passageErr
	}
	out := make([][]float32, len(chunks))
	for i, c := range chunks {
		out[i] = []float32{float32(len(c.Text)), 1, 2, 3}
	}
	return out, nil
}

func (f *fakeEmbedder) Spec() domain.EmbeddingSpec { return f.spec }

var _ domain.Embedder = (*fakeEmbedder)(nil)

func testSpec(model string) domain.EmbeddingSpec {
	return domain.EmbeddingSpec{Model: model, QueryTemplate: "query: {q}", PassageTemplate: "text: {d}"}
}

func openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return sqlite.NewStore(db)
}

// seedFile writes content to dir/name, upserts a discovered catalog row for
// it (as discovery would), and returns the document.
func seedFile(t *testing.T, store *sqlite.Store, dir, name, content string) domain.Document {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := domain.Document{
		ID:          "d_" + name,
		Path:        path,
		ContentHash: "hash-" + name,
		Size:        int64(len(content)),
		MTime:       time.Unix(1700000000, 0),
		State:       domain.DocStateDiscovered,
	}
	if _, err := store.UpsertDocument(context.Background(), doc, nil); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	return doc
}

func newIndexer(t *testing.T, store *sqlite.Store, emb domain.Embedder, transient func(error) bool) *Indexer {
	t.Helper()
	ix, err := New(Options{Store: store, Vectors: store, Embedder: emb, Transient: transient})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ix
}

func runAll(t *testing.T, ix *Indexer, store *sqlite.Store) (Summary, error) {
	t.Helper()
	docs, err := store.ListIndexable(context.Background())
	if err != nil {
		t.Fatalf("ListIndexable: %v", err)
	}
	return ix.Run(context.Background(), docs)
}

func docState(t *testing.T, store *sqlite.Store, path string) domain.DocState {
	t.Helper()
	doc, ok, err := store.GetByPath(context.Background(), path)
	if err != nil || !ok {
		t.Fatalf("GetByPath(%s): ok=%v err=%v", path, ok, err)
	}
	return doc.State
}

func TestRunHappyPath(t *testing.T) {
	store := openStore(t)
	dir := t.TempDir()
	a := seedFile(t, store, dir, "a.md", "# Alpha\n\nSome alpha text.\n")
	b := seedFile(t, store, dir, "b.md", "# Beta\n\nSome beta text.\n")
	emb := &fakeEmbedder{spec: testSpec("test-model")}

	sum, err := runAll(t, newIndexer(t, store, emb, nil), store)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Indexed != 2 || sum.UpToDate != 0 || sum.Failed != 0 {
		t.Errorf("Summary = %+v, want 2 indexed", sum)
	}
	for _, doc := range []domain.Document{a, b} {
		if st := docState(t, store, doc.Path); st != domain.DocStateIndexed {
			t.Errorf("%s state = %q, want indexed", doc.Path, st)
		}
	}
	hits, err := store.SearchVectors(context.Background(), []float32{20, 1, 2, 3}, 2)
	if err != nil {
		t.Fatalf("SearchVectors: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("SearchVectors returned %d hits, want 2", len(hits))
	}
}

func TestRunIdempotentRerun(t *testing.T) {
	store := openStore(t)
	dir := t.TempDir()
	seedFile(t, store, dir, "a.md", "# Alpha\n\ntext\n")
	emb := &fakeEmbedder{spec: testSpec("test-model")}

	if _, err := runAll(t, newIndexer(t, store, emb, nil), store); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	calls := emb.queryCalls + emb.passageCalls

	sum, err := runAll(t, newIndexer(t, store, emb, nil), store)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if sum.UpToDate != 1 || sum.Indexed != 0 {
		t.Errorf("Summary = %+v, want 1 up to date", sum)
	}
	if got := emb.queryCalls + emb.passageCalls; got != calls {
		t.Errorf("embedder called %d times on no-work re-run, want 0 (probe skipped)", got-calls)
	}
}

func TestRunResumesFromChunked(t *testing.T) {
	store := openStore(t)
	dir := t.TempDir()
	doc := seedFile(t, store, dir, "a.md", "# Alpha\n\ntext\n")

	// Simulate a crash after chunking: state=chunked, no vectors.
	if err := store.UpdateDocumentState(context.Background(), doc.ID, domain.DocStateChunked); err != nil {
		t.Fatal(err)
	}

	emb := &fakeEmbedder{spec: testSpec("test-model")}
	sum, err := runAll(t, newIndexer(t, store, emb, nil), store)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Indexed != 1 {
		t.Errorf("Summary = %+v, want 1 indexed", sum)
	}
	if st := docState(t, store, doc.Path); st != domain.DocStateIndexed {
		t.Errorf("state = %q, want indexed", st)
	}
}

func TestRunReembedsOnSpecChange(t *testing.T) {
	store := openStore(t)
	dir := t.TempDir()
	doc := seedFile(t, store, dir, "a.md", "# Alpha\n\ntext\n")

	if _, err := runAll(t, newIndexer(t, store, &fakeEmbedder{spec: testSpec("model-a")}, nil), store); err != nil {
		t.Fatalf("Run with model-a: %v", err)
	}

	sum, err := runAll(t, newIndexer(t, store, &fakeEmbedder{spec: testSpec("model-b")}, nil), store)
	if err != nil {
		t.Fatalf("Run with model-b: %v", err)
	}
	if sum.Indexed != 1 || sum.UpToDate != 0 {
		t.Errorf("Summary = %+v, want 1 re-indexed after model change", sum)
	}
	// The new generation serves the doc.
	hits, err := store.SearchVectors(context.Background(), []float32{10, 1, 2, 3}, 1)
	if err != nil {
		t.Fatalf("SearchVectors: %v", err)
	}
	if len(hits) != 1 || hits[0].Doc.ID != doc.ID {
		t.Fatalf("hits = %+v, want the re-embedded doc", hits)
	}
}

func TestRunUndecodableFileFailsAndContinues(t *testing.T) {
	store := openStore(t)
	dir := t.TempDir()
	bad := seedFile(t, store, dir, "bad.md", "\xff\xfe\xff invalid")
	good := seedFile(t, store, dir, "good.md", "# Good\n\ntext\n")
	// Overwrite bad with bytes Normalize rejects (lone continuation bytes).
	if err := os.WriteFile(bad.Path, []byte{0x68, 0x69, 0xC0, 0x80, 0xFF}, 0o600); err != nil {
		t.Fatal(err)
	}

	sum, err := runAll(t, newIndexer(t, store, &fakeEmbedder{spec: testSpec("test-model")}, nil), store)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Failed != 1 || sum.Indexed != 1 {
		t.Errorf("Summary = %+v, want 1 failed + 1 indexed", sum)
	}
	if st := docState(t, store, bad.Path); st != domain.DocStateFailed {
		t.Errorf("bad state = %q, want failed", st)
	}
	if st := docState(t, store, good.Path); st != domain.DocStateIndexed {
		t.Errorf("good state = %q, want indexed", st)
	}
}

func TestRunTransientEmbedErrorAborts(t *testing.T) {
	store := openStore(t)
	dir := t.TempDir()
	a := seedFile(t, store, dir, "a.md", "# Alpha\n\nalpha text\n")
	b := seedFile(t, store, dir, "b.md", "# Beta\n\nbeta text\n")

	emb := &fakeEmbedder{
		spec:         testSpec("test-model"),
		passageErrOn: "beta",
		passageErr:   errors.New("connection refused"),
	}
	sum, err := runAll(t, newIndexer(t, store, emb, func(error) bool { return true }), store)
	if err == nil {
		t.Fatal("Run = nil error, want abort on transient embed failure")
	}
	if sum.Indexed != 1 || sum.Failed != 0 {
		t.Errorf("Summary = %+v, want 1 indexed, 0 failed", sum)
	}
	if st := docState(t, store, a.Path); st != domain.DocStateIndexed {
		t.Errorf("a state = %q, want indexed (durable progress)", st)
	}
	if st := docState(t, store, b.Path); st != domain.DocStateChunked {
		t.Errorf("b state = %q, want chunked (resumes next run, not failed)", st)
	}
}

func TestRunPermanentEmbedErrorFailsDoc(t *testing.T) {
	store := openStore(t)
	dir := t.TempDir()
	a := seedFile(t, store, dir, "a.md", "# Alpha\n\nalpha text\n")
	b := seedFile(t, store, dir, "b.md", "# Beta\n\nbeta text\n")

	emb := &fakeEmbedder{
		spec:         testSpec("test-model"),
		passageErrOn: "alpha",
		passageErr:   errors.New("HTTP 400: bad input"),
	}
	sum, err := runAll(t, newIndexer(t, store, emb, func(error) bool { return false }), store)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Failed != 1 || sum.Indexed != 1 {
		t.Errorf("Summary = %+v, want 1 failed + 1 indexed", sum)
	}
	if st := docState(t, store, a.Path); st != domain.DocStateFailed {
		t.Errorf("a state = %q, want failed", st)
	}
	if st := docState(t, store, b.Path); st != domain.DocStateIndexed {
		t.Errorf("b state = %q, want indexed (run continued)", st)
	}
}

func TestRunEmptyFile(t *testing.T) {
	store := openStore(t)
	dir := t.TempDir()
	doc := seedFile(t, store, dir, "empty.md", "")

	emb := &fakeEmbedder{spec: testSpec("test-model")}
	sum, err := runAll(t, newIndexer(t, store, emb, nil), store)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Indexed != 1 {
		t.Errorf("Summary = %+v, want 1 indexed", sum)
	}
	if st := docState(t, store, doc.Path); st != domain.DocStateIndexed {
		t.Errorf("state = %q, want indexed", st)
	}
	if emb.passageCalls != 0 {
		t.Errorf("EmbedPassages called %d times for empty file, want 0", emb.passageCalls)
	}
}

func TestRunVanishedFileFails(t *testing.T) {
	store := openStore(t)
	dir := t.TempDir()
	doc := seedFile(t, store, dir, "gone.md", "# Gone\n\ntext\n")
	if err := os.Remove(doc.Path); err != nil {
		t.Fatal(err)
	}

	sum, err := runAll(t, newIndexer(t, store, &fakeEmbedder{spec: testSpec("test-model")}, nil), store)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Failed != 1 {
		t.Errorf("Summary = %+v, want 1 failed", sum)
	}
}

package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// [len(text), 1, 2, ...] padded to dims (default 4), so dims are stable and
// tests stay cgo-free on the inference side while the store side runs real
// SQLite.
type fakeEmbedder struct {
	spec         domain.EmbeddingSpec
	dims         int // 0 = 4
	queryCalls   int
	passageCalls int
	queryErr     error
	// passageErrOn fails EmbedPassages when the first chunk's text contains
	// this substring; empty never fails.
	passageErrOn string
	passageErr   error
	// duringEmbed runs inside EmbedPassages, standing in for whatever
	// touches the catalog while the network call is out.
	duringEmbed func()
}

func (f *fakeEmbedder) vector(lead float32) []float32 {
	dims := f.dims
	if dims == 0 {
		dims = 4
	}
	v := make([]float32, dims)
	v[0] = lead
	for i := 1; i < dims; i++ {
		v[i] = float32(i)
	}
	return v
}

func (f *fakeEmbedder) EmbedQuery(_ context.Context, query string) ([]float32, error) {
	f.queryCalls++
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.vector(float32(len(query))), nil
}

func (f *fakeEmbedder) EmbedPassages(_ context.Context, chunks []domain.Chunk) ([][]float32, error) {
	f.passageCalls++
	if f.passageErrOn != "" && len(chunks) > 0 && strings.Contains(chunks[0].Text, f.passageErrOn) {
		return nil, f.passageErr
	}
	if f.duringEmbed != nil {
		f.duringEmbed()
	}
	out := make([][]float32, len(chunks))
	for i, c := range chunks {
		out[i] = f.vector(float32(len(c.Text)))
	}
	return out, nil
}

func (f *fakeEmbedder) Spec() domain.EmbeddingSpec { return f.spec }

var _ domain.Embedder = (*fakeEmbedder)(nil)

func testSpec(model string) domain.EmbeddingSpec {
	return domain.EmbeddingSpec{Model: model, QueryTemplate: "query: {q}", PassageTemplate: "text: {d}"}
}

func openStore(t *testing.T) (*sqlite.Store, *sqlite.DB) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return sqlite.NewStore(db), db
}

// seedFile writes content to dir/name and upserts a documents row for it (as
// discovery would), which creates the content row at discovered. The hash is
// the real sha256 of the bytes — the pipeline verifies what it reads.
func seedFile(t *testing.T, store *sqlite.Store, dir, name string, content []byte) domain.Document {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	doc := domain.Document{
		Path:        path,
		ContentHash: hex.EncodeToString(digest[:]),
		Size:        int64(len(content)),
		MTime:       time.Unix(1700000000, 0),
	}
	if err := store.UpsertDocuments(context.Background(), []domain.Document{doc}); err != nil {
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
	items, err := store.ListWorkItems(context.Background())
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	return ix.Run(context.Background(), items)
}

// workFor reads back the content row behind path as the queue would hand it
// to the pipeline.
func workFor(t *testing.T, store *sqlite.Store, path string) domain.WorkItem {
	t.Helper()
	doc, ok, err := store.GetByPath(context.Background(), path)
	if err != nil || !ok {
		t.Fatalf("GetByPath(%s): ok=%v err=%v", path, ok, err)
	}
	item, err := store.GetWork(context.Background(), doc.ContentHash)
	if err != nil {
		t.Fatalf("GetWork(%s): %v", doc.ContentHash, err)
	}
	return item
}

func contentState(t *testing.T, store *sqlite.Store, path string) domain.ContentState {
	t.Helper()
	return workFor(t, store, path).Content.State
}

func chunkCount(t *testing.T, db *sqlite.DB, hash string) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRow(
		"SELECT count(*) FROM chunks WHERE content_hash = ?", hash).Scan(&n); err != nil {
		t.Fatalf("count chunks for %s: %v", hash, err)
	}
	return n
}

func TestRunHappyPath(t *testing.T) {
	store, _ := openStore(t)
	dir := t.TempDir()
	a := seedFile(t, store, dir, "a.md", []byte("# Alpha\n\nSome alpha text.\n"))
	b := seedFile(t, store, dir, "b.md", []byte("# Beta\n\nSome beta text.\n"))
	emb := &fakeEmbedder{spec: testSpec("test-model")}

	sum, err := runAll(t, newIndexer(t, store, emb, nil), store)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Indexed != 2 || sum.UpToDate != 0 || sum.Failed != 0 {
		t.Errorf("Summary = %+v, want 2 indexed", sum)
	}
	for _, doc := range []domain.Document{a, b} {
		if st := contentState(t, store, doc.Path); st != domain.ContentStateIndexed {
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
	store, _ := openStore(t)
	dir := t.TempDir()
	seedFile(t, store, dir, "a.md", []byte("# Alpha\n\ntext\n"))
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
	store, _ := openStore(t)
	dir := t.TempDir()
	doc := seedFile(t, store, dir, "a.md", []byte("# Alpha\n\ntext\n"))

	// Simulate a crash after chunking: state=chunked, no vectors.
	if err := store.UpdateContentState(context.Background(), doc.ContentHash, domain.ContentStateChunked); err != nil {
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
	if st := contentState(t, store, doc.Path); st != domain.ContentStateIndexed {
		t.Errorf("state = %q, want indexed", st)
	}
}

func TestRunReembedsOnSpecChange(t *testing.T) {
	store, _ := openStore(t)
	dir := t.TempDir()
	doc := seedFile(t, store, dir, "a.md", []byte("# Alpha\n\ntext\n"))

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
	// The new generation serves the content.
	hits, err := store.SearchVectors(context.Background(), []float32{10, 1, 2, 3}, 1)
	if err != nil {
		t.Fatalf("SearchVectors: %v", err)
	}
	if len(hits) != 1 || hits[0].Doc.Path != doc.Path {
		t.Fatalf("hits = %+v, want the re-embedded content at %s", hits, doc.Path)
	}
}

func TestRunUndecodableFileFailsAndContinues(t *testing.T) {
	store, db := openStore(t)
	dir := t.TempDir()
	// Bytes chunker.Normalize rejects (lone continuation bytes) — seeded
	// as-is, so the recorded hash matches what the pipeline reads.
	bad := seedFile(t, store, dir, "bad.md", []byte{0x68, 0x69, 0xC0, 0x80, 0xFF})
	good := seedFile(t, store, dir, "good.md", []byte("# Good\n\ntext\n"))

	sum, err := runAll(t, newIndexer(t, store, &fakeEmbedder{spec: testSpec("test-model")}, nil), store)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Failed != 1 || sum.Indexed != 1 {
		t.Errorf("Summary = %+v, want 1 failed + 1 indexed", sum)
	}
	if st := contentState(t, store, bad.Path); st != domain.ContentStateFailed {
		t.Errorf("bad state = %q, want failed", st)
	}
	if n := chunkCount(t, db, bad.ContentHash); n != 0 {
		t.Errorf("failed content holds %d chunks, want 0 (failed content must not serve stale chunks)", n)
	}
	if st := contentState(t, store, good.Path); st != domain.ContentStateIndexed {
		t.Errorf("good state = %q, want indexed", st)
	}
}

func TestRunTransientEmbedErrorAborts(t *testing.T) {
	store, _ := openStore(t)
	dir := t.TempDir()
	a := seedFile(t, store, dir, "a.md", []byte("# Alpha\n\nalpha text\n"))
	b := seedFile(t, store, dir, "b.md", []byte("# Beta\n\nbeta text\n"))

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
	if st := contentState(t, store, a.Path); st != domain.ContentStateIndexed {
		t.Errorf("a state = %q, want indexed (durable progress)", st)
	}
	if st := contentState(t, store, b.Path); st != domain.ContentStateChunked {
		t.Errorf("b state = %q, want chunked (resumes next run, not failed)", st)
	}
}

func TestRunPermanentEmbedErrorFailsContent(t *testing.T) {
	store, db := openStore(t)
	dir := t.TempDir()
	a := seedFile(t, store, dir, "a.md", []byte("# Alpha\n\nalpha text\n"))
	b := seedFile(t, store, dir, "b.md", []byte("# Beta\n\nbeta text\n"))

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
	if st := contentState(t, store, a.Path); st != domain.ContentStateFailed {
		t.Errorf("a state = %q, want failed", st)
	}
	if n := chunkCount(t, db, a.ContentHash); n != 0 {
		t.Errorf("failed content holds %d chunks, want 0 (cleared on failure)", n)
	}
	if st := contentState(t, store, b.Path); st != domain.ContentStateIndexed {
		t.Errorf("b state = %q, want indexed (run continued)", st)
	}
}

func TestRunPreservesForeignStageVersions(t *testing.T) {
	store, _ := openStore(t)
	dir := t.TempDir()
	doc := seedFile(t, store, dir, "a.md", []byte("# Alpha\n\ntext\n"))

	// A stage key owned by another pipeline stage (converter, M6) must
	// survive re-indexing — partial rebuilds depend on it.
	if _, err := store.StoreChunks(context.Background(), domain.Content{
		Hash:          doc.ContentHash,
		State:         domain.ContentStateDiscovered,
		StageVersions: map[string]string{"converter": "bscribe-9"},
	}, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := runAll(t, newIndexer(t, store, &fakeEmbedder{spec: testSpec("test-model")}, nil), store); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := workFor(t, store, doc.Path).Content.StageVersions
	if got["converter"] != "bscribe-9" {
		t.Errorf("converter stage version lost on re-index: %v", got)
	}
	if got[domain.StageChunker] == "" || got[domain.StageEmbedding] == "" {
		t.Errorf("pipeline stage versions missing: %v", got)
	}
}

func TestRunEmptyFile(t *testing.T) {
	store, db := openStore(t)
	dir := t.TempDir()
	doc := seedFile(t, store, dir, "empty.md", []byte(""))

	emb := &fakeEmbedder{spec: testSpec("test-model")}
	sum, err := runAll(t, newIndexer(t, store, emb, nil), store)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Indexed != 1 {
		t.Errorf("Summary = %+v, want 1 indexed", sum)
	}
	if st := contentState(t, store, doc.Path); st != domain.ContentStateIndexed {
		t.Errorf("state = %q, want indexed", st)
	}
	if n := chunkCount(t, db, doc.ContentHash); n != 0 {
		t.Errorf("empty content holds %d chunks, want 0", n)
	}
	if emb.passageCalls != 0 {
		t.Errorf("EmbedPassages called %d times for empty file, want 0", emb.passageCalls)
	}
}

func TestRunVanishedFileSkippedNotBurned(t *testing.T) {
	store, _ := openStore(t)
	dir := t.TempDir()
	doc := seedFile(t, store, dir, "gone.md", []byte("# Gone\n\ntext\n"))
	if err := os.Remove(doc.Path); err != nil {
		t.Fatal(err)
	}

	sum, err := runAll(t, newIndexer(t, store, &fakeEmbedder{spec: testSpec("test-model")}, nil), store)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Skipped != 1 || sum.Failed != 0 {
		t.Errorf("Summary = %+v, want 1 skipped, 0 failed", sum)
	}
	// The read failure is environmental — the content must NOT be marked
	// failed; restoring the file needs no catalog change to take effect.
	if st := contentState(t, store, doc.Path); st != domain.ContentStateDiscovered {
		t.Errorf("state = %q, want discovered (untouched)", st)
	}
}

// The file changed between the claim and the read: the bytes no longer hash
// to the content the work was claimed for. The claim is abandoned with
// nothing written — writing under the old hash would attach the new bytes'
// chunks to the old bytes' identity everywhere it is referenced.
func TestProcessContentHashMismatchAbandons(t *testing.T) {
	store, db := openStore(t)
	dir := t.TempDir()
	orig := []byte("# Alpha\n\nalpha text\n")
	doc := seedFile(t, store, dir, "a.md", orig)

	ctx := context.Background()
	ix := newIndexer(t, store, &fakeEmbedder{spec: testSpec("test-model")}, nil)
	dims, err := ix.Prepare(ctx)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	item := workFor(t, store, doc.Path)

	// The file is edited after the claim, before the read.
	if err := os.WriteFile(doc.Path, []byte("# Edited\n\ndifferent bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := ix.ProcessContent(ctx, item, ix.StageVersions(dims))
	if err != nil {
		t.Fatalf("ProcessContent returned a fatal error for a changed file: %v", err)
	}
	if res.Outcome != OutcomeChanged {
		t.Errorf("Outcome = %v, want OutcomeChanged", res.Outcome)
	}
	// Nothing was written: the row still sits at discovered with no chunks.
	if st := contentState(t, store, doc.Path); st != domain.ContentStateDiscovered {
		t.Errorf("state = %q, want discovered (untouched)", st)
	}
	if n := chunkCount(t, db, doc.ContentHash); n != 0 {
		t.Errorf("abandoned content holds %d chunks, want 0", n)
	}

	// Restoring the claimed bytes proves nothing was poisoned: the same
	// work item processes cleanly.
	if err := os.WriteFile(doc.Path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = ix.ProcessContent(ctx, item, ix.StageVersions(dims))
	if err != nil {
		t.Fatalf("ProcessContent after restore: %v", err)
	}
	if res.Outcome != OutcomeIndexed {
		t.Errorf("Outcome after restore = %v, want OutcomeIndexed", res.Outcome)
	}
	if st := contentState(t, store, doc.Path); st != domain.ContentStateIndexed {
		t.Errorf("state after restore = %q, want indexed", st)
	}
}

// A write must never resurrect swept content: StoreChunks updates, never
// inserts, so a work item whose content row does not exist (swept while in
// flight, or never discovered) stands down at the first write and leaves no
// row behind. Under ADR 0015 a purged *path* no longer blocks the write —
// the content row survives until the orphan sweep and chunks for it are
// harmless — so the invariant is tested where it now lives: the row itself.
func TestProcessContentSweptContentNotResurrected(t *testing.T) {
	store, _ := openStore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "ghost.md")
	content := []byte("# Ghost\n\ntext\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	hash := hex.EncodeToString(digest[:])

	ctx := context.Background()
	ix := newIndexer(t, store, &fakeEmbedder{spec: testSpec("test-model")}, nil)
	item := domain.WorkItem{
		Content: domain.Content{Hash: hash, State: domain.ContentStateDiscovered},
		Path:    path,
	}

	res, err := ix.ProcessContent(ctx, item, ix.StageVersions(4))
	if err != nil {
		t.Fatalf("ProcessContent returned a fatal error for swept content: %v", err)
	}
	// Superseded, not Skipped: Skipped means "could not be read", which the
	// scheduler reports as a possible Full Disk Access problem. Swept
	// content must not make a healthy daemon blame permissions.
	if res.Outcome != OutcomeSuperseded {
		t.Errorf("Outcome = %v, want OutcomeSuperseded", res.Outcome)
	}
	if !errors.Is(res.Err, domain.ErrContentGone) {
		t.Errorf("Err = %v, want domain.ErrContentGone", res.Err)
	}
	if _, err := store.GetWork(ctx, hash); !errors.Is(err, domain.ErrContentGone) {
		t.Errorf("GetWork after stand-down = %v, want ErrContentGone (no row resurrected)", err)
	}
}

// The widest window of all: the content is re-chunked *during* the embed, so
// the chunk IDs claimed a moment earlier are gone by the time their vectors
// come back. The vector write reports ErrContentSuperseded and the pipeline
// stands down rather than attaching one version's vectors to another
// version's text — or gating the whole drain on one raced file.
func TestProcessContentRechunkedDuringEmbedStandsDown(t *testing.T) {
	store, _ := openStore(t)
	dir := t.TempDir()
	doc := seedFile(t, store, dir, "doomed.md", []byte("# Doomed\n\ntext\n"))

	ctx := context.Background()
	emb := &fakeEmbedder{spec: testSpec("test-model")}
	emb.duringEmbed = func() {
		// Stands in for a rogue writer replacing the chunk rows while the
		// embedding endpoint is thinking (AUTOINCREMENT: the claimed IDs can
		// never come back).
		if _, err := store.StoreChunks(ctx, domain.Content{
			Hash:  doc.ContentHash,
			State: domain.ContentStateChunked,
		}, nil); err != nil {
			t.Errorf("re-chunk: %v", err)
		}
	}
	ix := newIndexer(t, store, emb, nil)
	dims, err := ix.Prepare(ctx)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	res, err := ix.ProcessContent(ctx, workFor(t, store, doc.Path), ix.StageVersions(dims))
	if err != nil {
		t.Fatalf("ProcessContent returned a fatal error for content re-chunked mid-embed: %v", err)
	}
	if res.Outcome != OutcomeSuperseded {
		t.Errorf("Outcome = %v, want OutcomeSuperseded", res.Outcome)
	}
	if !errors.Is(res.Err, domain.ErrContentSuperseded) {
		t.Errorf("Err = %v, want domain.ErrContentSuperseded", res.Err)
	}
	// Whoever superseded this pass owns the row: still chunked, not indexed.
	if st := contentState(t, store, doc.Path); st != domain.ContentStateChunked {
		t.Errorf("state = %q, want chunked (left to the superseding writer)", st)
	}
}

func TestRunFailedContentSkippedUnderSameConfig(t *testing.T) {
	store, _ := openStore(t)
	dir := t.TempDir()
	seedFile(t, store, dir, "bad.md", []byte{0x68, 0x69, 0xC0, 0x80, 0xFF})
	emb := &fakeEmbedder{spec: testSpec("test-model")}

	if _, err := runAll(t, newIndexer(t, store, emb, nil), store); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	sum, err := runAll(t, newIndexer(t, store, emb, nil), store)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	// Same config → the failure is not retried and not re-counted.
	if sum.Failed != 0 || sum.Indexed != 0 {
		t.Errorf("Summary = %+v, want all-zero re-run (failure already recorded)", sum)
	}
}

func TestRunFailedContentRetriedOnConfigChange(t *testing.T) {
	store, _ := openStore(t)
	dir := t.TempDir()
	a := seedFile(t, store, dir, "a.md", []byte("# Alpha\n\nalpha text\n"))

	// Poison under spec A: permanent embed error marks the content failed.
	embA := &fakeEmbedder{
		spec:         testSpec("model-a"),
		passageErrOn: "alpha",
		passageErr:   errors.New("HTTP 400: bad input"),
	}
	if _, err := runAll(t, newIndexer(t, store, embA, func(error) bool { return false }), store); err != nil {
		t.Fatalf("Run under model-a: %v", err)
	}
	if st := contentState(t, store, a.Path); st != domain.ContentStateFailed {
		t.Fatalf("state = %q, want failed", st)
	}

	// Config change (different model) → fresh attempt cures it without
	// touching the file.
	embB := &fakeEmbedder{spec: testSpec("model-b")}
	sum, err := runAll(t, newIndexer(t, store, embB, func(error) bool { return false }), store)
	if err != nil {
		t.Fatalf("Run under model-b: %v", err)
	}
	if sum.Indexed != 1 {
		t.Errorf("Summary = %+v, want 1 indexed after config change", sum)
	}
	if st := contentState(t, store, a.Path); st != domain.ContentStateIndexed {
		t.Errorf("state = %q, want indexed", st)
	}
}

func TestRunReembedsOnDimsChange(t *testing.T) {
	store, _ := openStore(t)
	dir := t.TempDir()
	seedFile(t, store, dir, "a.md", []byte("# Alpha\n\nalpha text\n"))

	if _, err := runAll(t, newIndexer(t, store, &fakeEmbedder{spec: testSpec("test-model")}, nil), store); err != nil {
		t.Fatalf("Run at dims=4: %v", err)
	}

	// Server now returns 6-dim vectors under the same model name and a new
	// file appears: the whole corpus must re-embed into the new generation,
	// not just the new file.
	seedFile(t, store, dir, "b.md", []byte("# Beta\n\nbeta text\n"))
	sum, err := runAll(t, newIndexer(t, store, &fakeEmbedder{spec: testSpec("test-model"), dims: 6}, nil), store)
	if err != nil {
		t.Fatalf("Run at dims=6: %v", err)
	}
	if sum.Indexed != 2 || sum.UpToDate != 0 {
		t.Errorf("Summary = %+v, want both contents re-embedded after dims change", sum)
	}
	hits, err := store.SearchVectors(context.Background(), []float32{10, 1, 2, 3, 4, 5}, 2)
	if err != nil {
		t.Fatalf("SearchVectors: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("SearchVectors returned %d hits, want 2 (both contents in new generation)", len(hits))
	}
}

func TestRunReembedsContentIndexedBeforeMetricStage(t *testing.T) {
	store, _ := openStore(t)
	dir := t.TempDir()
	doc := seedFile(t, store, dir, "a.md", []byte("# Alpha\n\nalpha text\n"))

	if _, err := runAll(t, newIndexer(t, store, &fakeEmbedder{spec: testSpec("test-model")}, nil), store); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Simulate content indexed before the vec-metric stage existed
	// (pre-#40, L2 era): same chunker/embedding/dims versions, no metric
	// key. It must be re-embedded into the current cosine generation, not
	// counted up to date — its vectors live in a table whose rankings used
	// a different metric.
	c := workFor(t, store, doc.Path).Content
	delete(c.StageVersions, domain.StageVecMetric)
	c.State = domain.ContentStateIndexed
	if _, err := store.StoreChunks(context.Background(), c, nil); err != nil {
		t.Fatalf("StoreChunks: %v", err)
	}

	sum, err := runAll(t, newIndexer(t, store, &fakeEmbedder{spec: testSpec("test-model")}, nil), store)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if sum.Indexed != 1 || sum.UpToDate != 0 {
		t.Errorf("Summary = %+v, want the pre-metric content re-embedded", sum)
	}
	got := workFor(t, store, doc.Path).Content.StageVersions
	if got[domain.StageVecMetric] != domain.VectorMetric {
		t.Errorf("StageVersions = %v, missing current vec metric", got)
	}
}

// Two paths, identical bytes: one content row, one work item, one embed call
// (ADR 0015 — a second identical file schedules no work at all).
func TestRunDuplicateContentIndexedOnce(t *testing.T) {
	store, _ := openStore(t)
	dir := t.TempDir()
	content := []byte("# Dup\n\nthe same bytes twice\n")
	a := seedFile(t, store, dir, "a.md", content)
	b := seedFile(t, store, dir, "b.md", content)
	if a.ContentHash != b.ContentHash {
		t.Fatalf("fixture bug: hashes differ (%s vs %s)", a.ContentHash, b.ContentHash)
	}

	items, err := store.ListWorkItems(context.Background())
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("ListWorkItems returned %d items, want 1 (one content row for two paths)", len(items))
	}

	emb := &fakeEmbedder{spec: testSpec("test-model")}
	sum, err := newIndexer(t, store, emb, nil).Run(context.Background(), items)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Indexed != 1 {
		t.Errorf("Summary = %+v, want 1 indexed", sum)
	}
	if emb.passageCalls != 1 {
		t.Errorf("EmbedPassages called %d times for duplicate content, want 1", emb.passageCalls)
	}
	for _, doc := range []domain.Document{a, b} {
		if st := contentState(t, store, doc.Path); st != domain.ContentStateIndexed {
			t.Errorf("%s state = %q, want indexed", doc.Path, st)
		}
	}
}

// An unreadable primary must not block a readable duplicate: the bytes at
// any copy are the claimed bytes (the hash proves it), so the pipeline
// falls back down the path list and indexes from whichever copy reads.
func TestProcessContentFallsBackToReadableDuplicate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 0 does not deny")
	}
	store, _ := openStore(t)
	dir := t.TempDir()
	content := []byte("# duplicated\n\nsame bytes at two paths\n")
	seedFile(t, store, dir, "readable.md", content)
	primary := seedFile(t, store, dir, "dead.md", content)
	if err := os.Chmod(primary.Path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(primary.Path, 0o600) })

	item, err := store.GetWork(context.Background(), primary.ContentHash)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	// Both copies share one mtime in the fixture; force the dead one to be
	// the primary so the fallback is what indexes.
	if item.Paths[0] != primary.Path {
		item.Paths = []string{primary.Path, filepath.Join(dir, "readable.md")}
		item.Path = primary.Path
	}

	ix := newIndexer(t, store, &fakeEmbedder{spec: testSpec("test-model")}, nil)
	dims, err := ix.Prepare(context.Background())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	res, err := ix.ProcessContent(context.Background(), item, ix.StageVersions(dims))
	if err != nil {
		t.Fatalf("ProcessContent: %v", err)
	}
	if res.Outcome != OutcomeIndexed {
		t.Fatalf("Outcome = %v, want OutcomeIndexed via the readable copy", res.Outcome)
	}
	if got := contentState(t, store, primary.Path); got != domain.ContentStateIndexed {
		t.Errorf("state = %v, want indexed", got)
	}
}

// Every copy unreadable is Skipped (environmental, retried); a readable
// copy that no longer matches the hash is Changed (abandoned).
func TestProcessContentAllCopiesUnreadableIsSkipped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 0 does not deny")
	}
	store, _ := openStore(t)
	dir := t.TempDir()
	content := []byte("locked everywhere\n")
	a := seedFile(t, store, dir, "a.md", content)
	b := seedFile(t, store, dir, "b.md", content)
	for _, p := range []string{a.Path, b.Path} {
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = os.Chmod(a.Path, 0o600)
		_ = os.Chmod(b.Path, 0o600)
	})

	item, err := store.GetWork(context.Background(), a.ContentHash)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	ix := newIndexer(t, store, &fakeEmbedder{spec: testSpec("test-model")}, nil)
	res, err := ix.ProcessContent(context.Background(), item, ix.StageVersions(3))
	if err != nil {
		t.Fatalf("ProcessContent: %v", err)
	}
	if res.Outcome != OutcomeSkipped {
		t.Errorf("Outcome = %v, want OutcomeSkipped when no copy reads", res.Outcome)
	}
}

func TestProcessContentReadableMismatchIsChanged(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 0 does not deny")
	}
	store, _ := openStore(t)
	dir := t.TempDir()
	content := []byte("original bytes\n")
	a := seedFile(t, store, dir, "a.md", content)
	b := seedFile(t, store, dir, "b.md", content)

	item, err := store.GetWork(context.Background(), a.ContentHash)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	// One copy unreadable, the other rewritten: something was readable and
	// nothing matched, so the claim is abandoned as changed — not blamed
	// on permissions.
	if err := os.Chmod(a.Path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(a.Path, 0o600) })
	if err := os.WriteFile(b.Path, []byte("rewritten bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ix := newIndexer(t, store, &fakeEmbedder{spec: testSpec("test-model")}, nil)
	res, err := ix.ProcessContent(context.Background(), item, ix.StageVersions(3))
	if err != nil {
		t.Fatalf("ProcessContent: %v", err)
	}
	if res.Outcome != OutcomeChanged {
		t.Errorf("Outcome = %v, want OutcomeChanged", res.Outcome)
	}
}

// A file changing under a one-shot run is a corpus that is not what the
// caller thinks it is — eval's completeness guarantee — so Run aborts
// loudly instead of folding the abandonment into "nothing to count".
func TestRunAbortsWhenTheCorpusChangesMidRun(t *testing.T) {
	store, _ := openStore(t)
	dir := t.TempDir()
	doc := seedFile(t, store, dir, "a.md", []byte("before\n"))
	if err := os.WriteFile(doc.Path, []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ix := newIndexer(t, store, &fakeEmbedder{spec: testSpec("test-model")}, nil)
	items, err := store.ListWorkItems(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Run(context.Background(), items); err == nil ||
		!strings.Contains(err.Error(), "corpus changed") {
		t.Fatalf("Run = %v, want a corpus-changed abort", err)
	}
}

package daemon_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bcrisp4/bsearch/internal/adapters/sqlite"
	"github.com/bcrisp4/bsearch/internal/daemon"
	"github.com/bcrisp4/bsearch/internal/domain"
	"github.com/bcrisp4/bsearch/internal/search"
)

var testSpec = domain.EmbeddingSpec{
	Model:           "test-model",
	QueryTemplate:   "query: {q}",
	PassageTemplate: "passage: {d}",
}

type fakeEmbedder struct {
	spec domain.EmbeddingSpec
	vec  []float32
}

func (f *fakeEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	return f.vec, nil
}

func (f *fakeEmbedder) EmbedPassages(_ context.Context, chunks []domain.Chunk) ([][]float32, error) {
	out := make([][]float32, len(chunks))
	for i := range chunks {
		out[i] = f.vec
	}
	return out, nil
}

func (f *fakeEmbedder) Spec() domain.EmbeddingSpec { return f.spec }

func newEmbedder() *fakeEmbedder {
	return &fakeEmbedder{spec: testSpec, vec: []float32{1, 0, 0}}
}

// writeIndex builds a minimal but real index at path: one document, one
// chunk, one vector, under testSpec.
func writeIndex(t *testing.T, path, docID, text string) {
	t.Helper()
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close() //nolint:errcheck // test fixture

	store := sqlite.NewStore(db)
	ctx := context.Background()
	doc := domain.Document{ID: docID, Path: "/notes/" + docID + ".md", ContentHash: "h", State: domain.DocStateIndexed}
	chunkIDs, err := store.UpsertDocument(ctx, doc, []domain.Chunk{{DocID: docID, Ordinal: 0, Text: text}})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	if err := store.EnsureVecTable(ctx, testSpec, 3); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}
	if err := store.UpsertVectors(ctx, chunkIDs, [][]float32{{1, 0, 0}}); err != nil {
		t.Fatalf("UpsertVectors: %v", err)
	}
}

func newDaemon(t *testing.T, dbPath string) *daemon.Daemon {
	t.Helper()
	d := daemon.New(daemon.Options{DBPath: dbPath, Embedder: newEmbedder()})
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return d
}

func TestSearchWithoutAnIndexIsNotIndexed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data", "bsearch.db")
	d := newDaemon(t, dbPath)

	_, err := d.Search(context.Background(), search.Request{Query: "alpha"})
	if !errors.Is(err, search.ErrNotIndexed) {
		t.Fatalf("Search with no index = %v, want ErrNotIndexed", err)
	}
	// Opening for a read must not manufacture an empty index.
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Errorf("the daemon created %s; a reader must not", dbPath)
	}
}

func TestIndexAppearingAfterStartupIsPickedUp(t *testing.T) {
	// The daemon may well start before anything is indexed — under launchd
	// it starts at login. Requiring a restart to notice the first index
	// would make that ordering a support question.
	dbPath := filepath.Join(t.TempDir(), "data", "bsearch.db")
	d := newDaemon(t, dbPath)

	if _, err := d.Search(context.Background(), search.Request{Query: "alpha"}); !errors.Is(err, search.ErrNotIndexed) {
		t.Fatalf("precondition: want ErrNotIndexed, got %v", err)
	}

	writeIndex(t, dbPath, "d_1", "alpha text")

	resp, err := d.Search(context.Background(), search.Request{Query: "alpha"})
	if err != nil {
		t.Fatalf("Search after the index appeared: %v", err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].DocID != "d_1" {
		t.Errorf("hits = %+v, want the indexed document", resp.Hits)
	}
}

func TestReplacedIndexFileIsPickedUp(t *testing.T) {
	// A drop-and-reindex creates a new inode. Holding the old one open
	// would serve results from a file nothing else can see, with no error
	// to notice.
	dbPath := filepath.Join(t.TempDir(), "data", "bsearch.db")
	writeIndex(t, dbPath, "d_old", "alpha text")
	d := newDaemon(t, dbPath)

	resp, err := d.Search(context.Background(), search.Request{Query: "alpha"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].DocID != "d_old" {
		t.Fatalf("hits = %+v, want the original document", resp.Hits)
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove %s: %v", dbPath+suffix, err)
		}
	}
	writeIndex(t, dbPath, "d_new", "alpha text")

	resp, err = d.Search(context.Background(), search.Request{Query: "alpha"})
	if err != nil {
		t.Fatalf("Search after replacement: %v", err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].DocID != "d_new" {
		t.Errorf("hits = %+v, want the replacement index's document — the daemon is serving a ghost inode", resp.Hits)
	}
}

func TestSearchAfterCloseDoesNotReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data", "bsearch.db")
	writeIndex(t, dbPath, "d_1", "alpha text")
	d := daemon.New(daemon.Options{DBPath: dbPath, Embedder: newEmbedder()})

	if _, err := d.Search(context.Background(), search.Request{Query: "alpha"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A request that arrives after shutdown must not resurrect the handle,
	// or the daemon leaks a database it will never close.
	if _, err := d.Search(context.Background(), search.Request{Query: "alpha"}); err == nil {
		t.Error("Search after Close succeeded; want an error")
	}
	if err := d.Close(); err != nil {
		t.Errorf("second Close: %v, want nil", err)
	}
}

func TestConcurrentSearchesOpenTheIndexOnce(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data", "bsearch.db")
	writeIndex(t, dbPath, "d_1", "alpha text")
	d := newDaemon(t, dbPath)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = d.Search(context.Background(), search.Request{Query: "alpha"})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent search %d: %v", i, err)
		}
	}
}

func TestSearchHonoursContextWhileWaitingToOpen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data", "bsearch.db")
	writeIndex(t, dbPath, "d_1", "alpha text")
	d := newDaemon(t, dbPath)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Search(ctx, search.Request{Query: "alpha"}); !errors.Is(err, context.Canceled) {
		t.Errorf("Search with a cancelled context = %v, want context.Canceled", err)
	}
}

func TestStatusWithoutAnIndex(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data", "bsearch.db")
	d := newDaemon(t, dbPath)

	status, err := d.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Ready {
		t.Error("ready = true with no index")
	}
	if !strings.Contains(status.Reason, dbPath) {
		t.Errorf("reason %q does not name the database path", status.Reason)
	}
}

func TestStatusWithAnIndex(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data", "bsearch.db")
	writeIndex(t, dbPath, "d_1", "alpha text")
	d := newDaemon(t, dbPath)

	status, err := d.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Ready {
		t.Fatalf("ready = false, reason %q", status.Reason)
	}
	if status.Model != testSpec.Model || status.Dims != 3 {
		t.Errorf("status = %+v, want the indexed model and dims", status)
	}
	if status.Documents["indexed"] != 1 {
		t.Errorf("documents = %v, want one indexed document", status.Documents)
	}
	// Every state present, so a consumer never has to tell absent from zero.
	for _, state := range domain.DocStates {
		if _, ok := status.Documents[string(state)]; !ok {
			t.Errorf("documents is missing state %q", state)
		}
	}
}

func TestStatusReportsAModelMismatch(t *testing.T) {
	// Searching is impossible in this state, so "ready" would be a lie —
	// and the reason is the one thing that explains the 409s.
	dbPath := filepath.Join(t.TempDir(), "data", "bsearch.db")
	writeIndex(t, dbPath, "d_1", "alpha text")

	embedder := newEmbedder()
	embedder.spec.Model = "some-other-model"
	d := daemon.New(daemon.Options{DBPath: dbPath, Embedder: embedder})
	t.Cleanup(func() { _ = d.Close() })

	status, err := d.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Ready {
		t.Error("ready = true despite a model mismatch")
	}
	if !strings.Contains(status.Reason, "some-other-model") {
		t.Errorf("reason %q does not name the configured model", status.Reason)
	}
	// Counts still come through: they are what tells the user whether the
	// index is worth re-embedding or empty anyway.
	if status.Documents["indexed"] != 1 {
		t.Errorf("documents = %v, want counts even when not ready", status.Documents)
	}
}

func TestUnconfiguredEmbedderIsReportedNotCrashed(t *testing.T) {
	// A LaunchAgent installed before the user configures a model must not
	// crash-loop: status is the only thing that can explain the problem, so
	// it has to stay reachable.
	dbPath := filepath.Join(t.TempDir(), "data", "bsearch.db")
	writeIndex(t, dbPath, "d_1", "alpha text")
	d := daemon.New(daemon.Options{
		DBPath:   dbPath,
		Embedder: nil,
		NotReady: "inference.embedding_model is not set",
	})
	t.Cleanup(func() { _ = d.Close() })

	status, err := d.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Ready || !strings.Contains(status.Reason, "embedding_model") {
		t.Errorf("status = %+v, want not-ready naming the missing setting", status)
	}
	if status.Documents["indexed"] != 1 {
		t.Errorf("documents = %v, want counts even without an embedder", status.Documents)
	}

	_, err = d.Search(context.Background(), search.Request{Query: "alpha"})
	if !errors.Is(err, search.ErrNotIndexed) {
		t.Errorf("Search without an embedder = %v, want ErrNotIndexed", err)
	}
}

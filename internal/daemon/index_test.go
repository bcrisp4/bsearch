package daemon_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/adapters/sqlite"
	"github.com/bcrisp4/bsearch/internal/daemon"
	"github.com/bcrisp4/bsearch/internal/domain"
	"github.com/bcrisp4/bsearch/internal/search"
	"github.com/bcrisp4/bsearch/internal/server"
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

// openStore opens the index database at path for a fixture write.
func openStore(t *testing.T, path string) (*sqlite.Store, func()) {
	t.Helper()
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return sqlite.NewStore(db), func() { _ = db.Close() }
}

// testDoc is a readable file at /notes/<hash>.md whose bytes hash to hash.
func testDoc(hash string) domain.Document {
	return domain.Document{
		Path:        "/notes/" + hash + ".md",
		ContentHash: hash,
		Size:        42,
		MTime:       time.Unix(1700000000, 0),
	}
}

// writeIndex builds a minimal but real index at path: one document, one
// chunk, one vector, under testSpec. Two modules write it, as in production
// (ADR 0015): discovery announces the path (UpsertDocuments creates the
// content row at discovered), then the pipeline stores chunks and advances
// the content to indexed.
func writeIndex(t *testing.T, path, hash, text string) {
	t.Helper()
	store, closeDB := openStore(t, path)
	defer closeDB()
	ctx := context.Background()

	if err := store.UpsertDocuments(ctx, []domain.Document{testDoc(hash)}); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	c := domain.Content{Hash: hash, State: domain.ContentStateChunked}
	chunkIDs, err := store.StoreChunks(ctx, c, []domain.Chunk{{Ordinal: 0, Text: text, ByteEnd: len(text)}})
	if err != nil {
		t.Fatalf("StoreChunks: %v", err)
	}
	if err := store.EnsureVecTable(ctx, testSpec, 3); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}
	if err := store.UpsertVectors(ctx, chunkIDs, [][]float32{{1, 0, 0}}); err != nil {
		t.Fatalf("UpsertVectors: %v", err)
	}
	if err := store.UpdateContentState(ctx, hash, domain.ContentStateIndexed); err != nil {
		t.Fatalf("UpdateContentState: %v", err)
	}
}

// addContent adds a path and its content row in the given state to an
// existing index, with no chunks — the shape of content the pipeline has not
// finished (or has given up on). A non-empty failure reason is recorded as
// last_error.
func addContent(t *testing.T, path, hash string, state domain.ContentState, failure string) {
	t.Helper()
	store, closeDB := openStore(t, path)
	defer closeDB()
	ctx := context.Background()

	if err := store.UpsertDocuments(ctx, []domain.Document{testDoc(hash)}); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	switch {
	case failure != "":
		if err := store.MarkFailed(ctx, hash, failure); err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
	case state != domain.ContentStateDiscovered:
		if err := store.UpdateContentState(ctx, hash, state); err != nil {
			t.Fatalf("UpdateContentState: %v", err)
		}
	}
}

// addUnread adds a documents row whose bytes were never obtained: no content
// hash, just the reason (ADR 0015).
func addUnread(t *testing.T, path, docPath string, reason domain.UnreadReason) {
	t.Helper()
	store, closeDB := openStore(t, path)
	defer closeDB()
	doc := domain.Document{Path: docPath, UnreadReason: reason, Size: 1, MTime: time.Unix(1700000000, 0)}
	if err := store.UpsertDocuments(context.Background(), []domain.Document{doc}); err != nil {
		t.Fatalf("UpsertDocuments (unread): %v", err)
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

	writeIndex(t, dbPath, "h1", "alpha text")

	resp, err := d.Search(context.Background(), search.Request{Query: "alpha"})
	if err != nil {
		t.Fatalf("Search after the index appeared: %v", err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].ContentHash != "h1" {
		t.Errorf("hits = %+v, want the indexed document", resp.Hits)
	}
}

func TestReplacedIndexFileIsPickedUp(t *testing.T) {
	// A drop-and-reindex creates a new inode. Holding the old one open
	// would serve results from a file nothing else can see, with no error
	// to notice.
	dbPath := filepath.Join(t.TempDir(), "data", "bsearch.db")
	writeIndex(t, dbPath, "h_old", "alpha text")
	d := newDaemon(t, dbPath)

	resp, err := d.Search(context.Background(), search.Request{Query: "alpha"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].ContentHash != "h_old" {
		t.Fatalf("hits = %+v, want the original document", resp.Hits)
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove %s: %v", dbPath+suffix, err)
		}
	}
	writeIndex(t, dbPath, "h_new", "alpha text")

	resp, err = d.Search(context.Background(), search.Request{Query: "alpha"})
	if err != nil {
		t.Fatalf("Search after replacement: %v", err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].ContentHash != "h_new" {
		t.Errorf("hits = %+v, want the replacement index's document — the daemon is serving a ghost inode", resp.Hits)
	}
}

func TestSearchAfterCloseDoesNotReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data", "bsearch.db")
	writeIndex(t, dbPath, "h1", "alpha text")
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
	writeIndex(t, dbPath, "h1", "alpha text")
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
	writeIndex(t, dbPath, "h1", "alpha text")
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
	writeIndex(t, dbPath, "h1", "alpha text")
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
	if status.Files != 1 {
		t.Errorf("files = %d, want the one path", status.Files)
	}
	if status.Content["indexed"] != 1 {
		t.Errorf("content = %v, want one indexed content", status.Content)
	}
	// Every state and reason present, so a consumer never has to tell absent
	// from zero.
	for _, state := range domain.ContentStates {
		if _, ok := status.Content[string(state)]; !ok {
			t.Errorf("content is missing state %q", state)
		}
	}
	for _, reason := range domain.UnreadReasons {
		if _, ok := status.Unread[string(reason)]; !ok {
			t.Errorf("unread is missing reason %q", reason)
		}
	}
}

func TestStatusCountsUnreadFiles(t *testing.T) {
	// A path whose bytes were never obtained has no content and no pipeline
	// state, but it is still a file — Files includes it, and Unread says why
	// it is invisible to search (ADR 0015: denied is the Full Disk Access
	// signal and must never fold into a healthy-looking total).
	dbPath := filepath.Join(t.TempDir(), "data", "bsearch.db")
	writeIndex(t, dbPath, "h1", "alpha text")
	addUnread(t, dbPath, "/notes/locked.md", domain.UnreadDenied)
	d := newDaemon(t, dbPath)

	status, err := d.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Files != 2 {
		t.Errorf("files = %d, want both paths counted", status.Files)
	}
	if status.Unread["denied"] != 1 {
		t.Errorf("unread = %v, want the denied file counted", status.Unread)
	}
	if status.Content["indexed"] != 1 {
		t.Errorf("content = %v, want the readable file's content indexed", status.Content)
	}
}

func TestStatusReportsTheBacklogAndFootprint(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data", "bsearch.db")
	writeIndex(t, dbPath, "h1", "alpha text")
	// A content waiting to be worked and one given up on, so the backlog and
	// the failure list both have something to say.
	addContent(t, dbPath, "h2", domain.ContentStateDiscovered, "")
	addContent(t, dbPath, "h3", domain.ContentStateFailed, "not valid UTF-8")
	d := newDaemon(t, dbPath)

	status, err := d.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Queue == nil || status.Queue.Pending != 1 {
		t.Errorf("queue = %+v, want one pending content", status.Queue)
	}
	want := []server.FailureGroup{{Reason: "not valid UTF-8", Contents: 1, ExamplePath: "/notes/h3.md"}}
	if len(status.Failures) != 1 || status.Failures[0] != want[0] {
		t.Errorf("failures = %+v, want %+v", status.Failures, want)
	}
	if status.Disk == nil || status.Disk.DBBytes == 0 {
		t.Errorf("disk = %+v, want a measured footprint", status.Disk)
	}
	if status.Disk != nil && status.Disk.TotalBytes < status.Disk.DBBytes {
		t.Errorf("disk = %+v, want the total to include the database", status.Disk)
	}
}

// A clean index reports no failures at all rather than an empty-reason group:
// the CLI hides the section entirely, and "Failures (0)" is noise.
func TestStatusOmitsFailuresWhenNothingFailed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data", "bsearch.db")
	writeIndex(t, dbPath, "h1", "alpha text")
	d := newDaemon(t, dbPath)

	status, err := d.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(status.Failures) != 0 {
		t.Errorf("failures = %+v, want none", status.Failures)
	}
}

func TestStatusReportsAModelMismatch(t *testing.T) {
	// Searching is impossible in this state, so "ready" would be a lie —
	// and the reason is the one thing that explains the 409s.
	dbPath := filepath.Join(t.TempDir(), "data", "bsearch.db")
	writeIndex(t, dbPath, "h1", "alpha text")

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
	if status.Content["indexed"] != 1 {
		t.Errorf("content = %v, want counts even when not ready", status.Content)
	}
}

func TestUnconfiguredEmbedderIsReportedNotCrashed(t *testing.T) {
	// A LaunchAgent installed before the user configures a model must not
	// crash-loop: status is the only thing that can explain the problem, so
	// it has to stay reachable.
	dbPath := filepath.Join(t.TempDir(), "data", "bsearch.db")
	writeIndex(t, dbPath, "h1", "alpha text")
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
	if status.Content["indexed"] != 1 {
		t.Errorf("content = %v, want counts even without an embedder", status.Content)
	}

	_, err = d.Search(context.Background(), search.Request{Query: "alpha"})
	if !errors.Is(err, search.ErrNotIndexed) {
		t.Errorf("Search without an embedder = %v, want ErrNotIndexed", err)
	}
}

// blockingEmbedder stalls inside EmbedQuery so a test can hold a search
// mid-flight — after the index handle is acquired, before the KNN query.
type blockingEmbedder struct {
	fakeEmbedder
	entered chan struct{}
	release chan struct{}
}

func (b *blockingEmbedder) EmbedQuery(ctx context.Context, q string) ([]float32, error) {
	b.entered <- struct{}{}
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.fakeEmbedder.EmbedQuery(ctx, q)
}

func TestReplacingTheIndexDoesNotBreakAnInFlightSearch(t *testing.T) {
	// A search holds the index handle across two queries. If another
	// request notices a replaced database in between and closes the handle,
	// the first search dies with "sql: database is closed" — which carries
	// no sentinel, so the user gets an opaque 500 for something they did
	// nothing wrong to cause.
	dbPath := filepath.Join(t.TempDir(), "data", "bsearch.db")
	writeIndex(t, dbPath, "h_old", "alpha text")

	embedder := &blockingEmbedder{
		fakeEmbedder: *newEmbedder(),
		entered:      make(chan struct{}),
		release:      make(chan struct{}),
	}
	d := daemon.New(daemon.Options{DBPath: dbPath, Embedder: embedder})
	t.Cleanup(func() { _ = d.Close() })

	result := make(chan error, 1)
	go func() {
		_, err := d.Search(context.Background(), search.Request{Query: "alpha"})
		result <- err
	}()

	select {
	case <-embedder.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("search never reached the embedder")
	}

	// Replace the database while that search holds its handle.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove: %v", err)
		}
	}
	writeIndex(t, dbPath, "h_new", "alpha text")

	// A concurrent request notices the replacement and retires the old
	// handle. It must not close a database another request is still using.
	if _, err := d.Status(context.Background()); err != nil {
		t.Fatalf("Status during the swap: %v", err)
	}

	close(embedder.release)
	select {
	case err := <-result:
		if err != nil {
			t.Errorf("in-flight search = %v, want it to complete against the index it started on", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight search never finished")
	}
}

func TestUnreadableIndexIsReportedAsNotIndexed(t *testing.T) {
	// Every way opening can fail — a file that isn't a bsearch index database, a
	// schema this build is too old or too new for — says something
	// actionable about the index. Without the sentinel they all arrive as
	// an opaque 500 telling the user to check a log.
	dbPath := filepath.Join(t.TempDir(), "bsearch.db")
	if err := os.WriteFile(dbPath, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := newDaemon(t, dbPath)

	_, err := d.Search(context.Background(), search.Request{Query: "alpha"})
	if !errors.Is(err, search.ErrNotIndexed) {
		t.Errorf("Search against an unreadable index = %v, want ErrNotIndexed", err)
	}

	// Status must still answer, with the reason rather than a failure.
	status, err := d.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Ready || status.Reason == "" {
		t.Errorf("status = %+v, want not-ready with a reason", status)
	}
}

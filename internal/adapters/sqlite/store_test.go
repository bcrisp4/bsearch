package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
)

func testDoc(id, path string) domain.Document {
	return domain.Document{
		ID:          id,
		Path:        path,
		ContentHash: "hash-" + id,
		Size:        42,
		MTime:       time.Unix(1700000000, 123456789),
		State:       domain.DocStateChunked,
	}
}

// seedUpsert is UpsertDocument with discovery's create in front of it, and
// it exists because only discovery creates catalog rows: a pipeline-shaped
// write (any state but discovered) against a row that is not there reports
// domain.ErrDocumentGone rather than conjuring one. Production always gets
// there in two steps — discovery announces the file, the pipeline works on
// it — so a test that wants a chunked or failed document has to as well.
func seedUpsert(t *testing.T, store *Store, doc domain.Document, chunks []domain.Chunk) ([]int64, error) {
	t.Helper()
	ctx := context.Background()
	create := doc
	create.State = domain.DocStateDiscovered
	if _, err := store.UpsertDocument(ctx, create, nil); err != nil {
		t.Fatalf("seed %s at %s: %v", doc.ID, doc.Path, err)
	}
	if doc.State == domain.DocStateDiscovered && len(chunks) == 0 {
		return nil, nil
	}
	return store.UpsertDocument(ctx, doc, chunks)
}

func testChunks(docID string, texts ...string) []domain.Chunk {
	chunks := make([]domain.Chunk, len(texts))
	pos := 0
	for i, text := range texts {
		chunks[i] = domain.Chunk{
			DocID:       docID,
			Ordinal:     i,
			Text:        text,
			HeadingPath: "Doc > Section",
			ByteStart:   pos,
			ByteEnd:     pos + len(text),
		}
		pos += len(text)
	}
	return chunks
}

func TestUpsertDocumentInsertsDocAndChunks(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	ids, err := seedUpsert(t, store, testDoc("d_1", "/notes/a.md"), testChunks("d_1", "alpha", "beta"))
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("chunk ids = %v, want 2", ids)
	}

	doc, ok, err := store.GetByPath(ctx, "/notes/a.md")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if !ok {
		t.Fatal("GetByPath: not found after upsert")
	}
	if doc.ID != "d_1" || doc.ContentHash != "hash-d_1" || doc.Size != 42 {
		t.Errorf("GetByPath = %+v", doc)
	}
	if !doc.MTime.Equal(time.Unix(1700000000, 123456789)) {
		t.Errorf("MTime = %v, want ns-precision round-trip", doc.MTime)
	}
	if doc.State != domain.DocStateChunked {
		t.Errorf("State = %q, want %q", doc.State, domain.DocStateChunked)
	}
}

func TestUpsertDocumentReplacesChunks(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	first, err := seedUpsert(t, store, testDoc("d_1", "/notes/a.md"), testChunks("d_1", "alpha", "beta"))
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	doc := testDoc("d_1", "/notes/a.md")
	doc.ContentHash = "hash-v2"
	second, err := store.UpsertDocument(ctx, doc, testChunks("d_1", "gamma", "delta", "epsilon"))
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if len(second) != 3 {
		t.Fatalf("second chunk ids = %v, want 3", second)
	}
	for _, oldID := range first {
		for _, newID := range second {
			if oldID == newID {
				t.Errorf("chunk id %d survived replacement", oldID)
			}
		}
	}

	var count int
	if err := db.Reader().QueryRow("SELECT count(*) FROM chunks WHERE doc_id = 'd_1'").Scan(&count); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if count != 3 {
		t.Errorf("chunk count = %d, want 3 (old chunks must be gone)", count)
	}

	got, _, err := store.GetByPath(ctx, "/notes/a.md")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if got.ContentHash != "hash-v2" {
		t.Errorf("ContentHash = %q, want hash-v2", got.ContentHash)
	}
}

func TestGetByID(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	want := testDoc("d_1", "/notes/a.md")
	if _, err := seedUpsert(t, store, want, testChunks("d_1", "alpha")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := store.GetByID(ctx, "d_1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Path != want.Path || got.ContentHash != want.ContentHash || got.State != want.State {
		t.Errorf("GetByID = %+v, want path/hash/state from %+v", got, want)
	}

	// A missing id is ErrDocumentGone, not an ok=false — the id came out of a
	// claim, so its absence is a purge and the caller stands down.
	if _, err := store.GetByID(ctx, "d_missing"); !errors.Is(err, domain.ErrDocumentGone) {
		t.Errorf("GetByID(unknown) = %v, want domain.ErrDocumentGone", err)
	}
}

// Why the scheduler re-reads at all: a document claimed minutes ago must be
// worked on at the path it has now, not the path it had then. Issue #63's
// second half is exactly this — a stale copy sends the pipeline to a path a
// rename has since given to a different file.
func TestGetByIDSeesAPathChangedSinceTheClaim(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	claimed := testDoc("d_1", "/notes/a.md")
	if _, err := seedUpsert(t, store, claimed, testChunks("d_1", "alpha")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The write discovery makes for a rename: same id, new path, back to
	// discovered.
	renamed := claimed
	renamed.Path, renamed.State = "/notes/b.md", domain.DocStateDiscovered
	if _, err := store.UpsertDocument(ctx, renamed, nil); err != nil {
		t.Fatalf("rename: %v", err)
	}

	got, err := store.GetByID(ctx, "d_1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Path != "/notes/b.md" {
		t.Errorf("path = %q, want the post-rename path — the claimed copy is stale", got.Path)
	}
}

func TestGetByPathMissing(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)

	_, ok, err := store.GetByPath(context.Background(), "/no/such/file.md")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if ok {
		t.Error("GetByPath reported a document for a path never stored")
	}
}

func TestDeleteDocumentCascades(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if _, err := seedUpsert(t, store, testDoc("d_1", "/notes/a.md"), testChunks("d_1", "alpha")); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := db.Writer().Exec(
		"INSERT INTO summaries (doc_id, level, text) VALUES ('d_1', 4, 'short summary')"); err != nil {
		t.Fatalf("insert summary: %v", err)
	}

	if err := store.DeleteDocument(ctx, "d_1"); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}

	for _, table := range []string{"documents", "chunks", "summaries"} {
		var count int
		if err := db.Reader().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s has %d rows after delete, want 0", table, count)
		}
	}
}

func TestDeleteByPathPrefix(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	// /notes/sub is the target; /notes/sub.md and /notes/subway/ are the
	// near misses a naive LIKE 'prefix%' would take with it.
	paths := map[string]string{
		"d_dir":     "/notes/sub",
		"d_inside":  "/notes/sub/a.md",
		"d_deeper":  "/notes/sub/deep/b.md",
		"d_sibling": "/notes/sub.md",
		"d_subway":  "/notes/subway/c.md",
		"d_other":   "/notes/other.md",
	}
	for id, path := range paths {
		if _, err := seedUpsert(t, store, testDoc(id, path), testChunks(id, "text")); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}

	removed, err := store.DeleteByPathPrefix(ctx, "/notes/sub")
	if err != nil {
		t.Fatalf("DeleteByPathPrefix: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed = %d, want 3 (the directory row and the two under it)", removed)
	}

	for _, id := range []string{"d_sibling", "d_subway", "d_other"} {
		if _, ok, err := store.GetByPath(ctx, paths[id]); err != nil || !ok {
			t.Errorf("%s (%s) was removed; only the prefix and its descendants should go", id, paths[id])
		}
	}
	for _, id := range []string{"d_dir", "d_inside", "d_deeper"} {
		if _, ok, err := store.GetByPath(ctx, paths[id]); err != nil || ok {
			t.Errorf("%s (%s) survived the prefix delete", id, paths[id])
		}
	}
	// Chunks cascade with their documents, as with DeleteDocument.
	var chunks int
	if err := db.Reader().QueryRow(
		"SELECT count(*) FROM chunks WHERE doc_id IN ('d_dir','d_inside','d_deeper')").Scan(&chunks); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunks != 0 {
		t.Errorf("chunks = %d after prefix delete, want 0", chunks)
	}
}

func TestDeleteByPathPrefixRefusesToEmptyTheCatalog(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if _, err := seedUpsert(t, store, testDoc("d_1", "/notes/a.md"), nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for _, prefix := range []string{"", "/"} {
		if _, err := store.DeleteByPathPrefix(ctx, prefix); err == nil {
			t.Errorf("DeleteByPathPrefix(%q) = nil error, want a refusal", prefix)
		}
	}
	if _, ok, err := store.GetByPath(ctx, "/notes/a.md"); err != nil || !ok {
		t.Error("the document was removed by a refused prefix delete")
	}
}

// The window this closes: the drain claims a document, the file is deleted,
// the watcher's reconcile purges the row, and the pipeline then commits its
// chunks. An INSERT there would put the document back and go on to embed and
// finalize it, leaving a deleted file permanently searchable with nothing to
// notice — the index would be serving content whose source is gone.
func TestPipelineWritesDoNotResurrectAPurgedDocument(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if _, err := seedUpsert(t, store, testDoc("d_1", "/notes/a.md"), testChunks("d_1", "alpha")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.DeleteByPathPrefix(ctx, "/notes/a.md"); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// Every write the pipeline makes after the purge, in the order it makes
	// them. All of them must refuse.
	chunked := testDoc("d_1", "/notes/a.md")
	chunked.State = domain.DocStateChunked
	failed := testDoc("d_1", "/notes/a.md")
	failed.State = domain.DocStateFailed
	for name, write := range map[string]func() error{
		"commit chunks": func() error {
			_, err := store.UpsertDocument(ctx, chunked, testChunks("d_1", "alpha"))
			return err
		},
		"record failure": func() error {
			_, err := store.UpsertDocument(ctx, failed, nil)
			return err
		},
		"finalize": func() error {
			return store.UpdateDocumentState(ctx, "d_1", domain.DocStateChunked, domain.DocStateIndexed)
		},
		"mark failed": func() error { return store.MarkFailed(ctx, "d_1", "embed: boom") },
		"refresh stat": func() error {
			return store.UpdateDocumentStat(ctx, "d_1", 99, time.Unix(1700000001, 0))
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := write(); !errors.Is(err, domain.ErrDocumentGone) {
				t.Errorf("err = %v, want domain.ErrDocumentGone", err)
			}
			if _, ok, err := store.GetByPath(ctx, "/notes/a.md"); err != nil || ok {
				t.Error("the purged document is back in the catalog")
			}
		})
	}

	// And nothing derived from it survived either.
	var chunks int
	if err := db.Reader().QueryRow("SELECT count(*) FROM chunks WHERE doc_id = 'd_1'").Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if chunks != 0 {
		t.Errorf("chunks = %d for a purged document, want 0", chunks)
	}
}

// Discovery is the exception, and has to be: it is the thing that announces
// a file exists, so its create cannot depend on the row already being there.
func TestDiscoveryStillCreatesRows(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := context.Background()

	doc := testDoc("d_1", "/notes/a.md")
	doc.State = domain.DocStateDiscovered
	if _, err := store.UpsertDocument(ctx, doc, nil); err != nil {
		t.Fatalf("discovery upsert: %v", err)
	}
	if _, ok, err := store.GetByPath(ctx, "/notes/a.md"); err != nil || !ok {
		t.Error("discovery did not create the row")
	}
}

func TestUpsertDocumentDisplacesPathOwner(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if _, err := seedUpsert(t, store, testDoc("d_old", "/notes/a.md"), testChunks("d_old", "alpha")); err != nil {
		t.Fatalf("upsert d_old: %v", err)
	}
	// New document ID claims the same path (deleted-and-recreated file).
	if _, err := seedUpsert(t, store, testDoc("d_new", "/notes/a.md"), testChunks("d_new", "beta")); err != nil {
		t.Fatalf("upsert d_new over same path: %v", err)
	}

	doc, ok, err := store.GetByPath(ctx, "/notes/a.md")
	if err != nil || !ok {
		t.Fatalf("GetByPath: ok=%v err=%v", ok, err)
	}
	if doc.ID != "d_new" {
		t.Errorf("path owner = %s, want d_new", doc.ID)
	}
	var oldRows int
	if err := db.Reader().QueryRow("SELECT count(*) FROM documents WHERE id = 'd_old'").Scan(&oldRows); err != nil {
		t.Fatal(err)
	}
	if oldRows != 0 {
		t.Error("displaced document row survived")
	}
}

// Discovery upserts with state=discovered when a file is new or its content
// changed, and that is what earns a fresh retry budget: the previous failures
// were about content that no longer exists.
func TestUpsertDocumentResetsRetryColumnsForAChangedFile(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if _, err := seedUpsert(t, store, testDoc("d_1", "/notes/a.md"), nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := db.Writer().Exec(
		"UPDATE documents SET attempts = 5, next_retry_at = 123, last_error = 'boom' WHERE id = 'd_1'"); err != nil {
		t.Fatal(err)
	}

	changed := testDoc("d_1", "/notes/a.md")
	changed.State = domain.DocStateDiscovered
	if _, err := store.UpsertDocument(ctx, changed, nil); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	var attempts int
	var nextRetry, lastError sql.NullString
	if err := db.Reader().QueryRow(
		"SELECT attempts, next_retry_at, last_error FROM documents WHERE id = 'd_1'").
		Scan(&attempts, &nextRetry, &lastError); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || nextRetry.Valid || lastError.Valid {
		t.Errorf("retry columns not reset: attempts=%d next_retry=%v last_error=%v",
			attempts, nextRetry, lastError)
	}
}

// The pipeline commits chunks through UpsertDocument with state=chunked
// *before* the embed network call. Resetting the retry columns there would
// park the row at attempts=0 for the whole duration of that call, so a crash
// in the window would hand the document a fresh budget — and a document that
// can never be embedded would never reach the attempt cap.
func TestUpsertDocumentKeepsRetryColumnsMidPipeline(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if _, err := seedUpsert(t, store, testDoc("d_1", "/notes/a.md"), nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := db.Writer().Exec(
		"UPDATE documents SET attempts = 3, next_retry_at = 123, last_error = 'embed: boom' WHERE id = 'd_1'"); err != nil {
		t.Fatal(err)
	}

	// A retry: same content, re-chunked and committed ahead of embedding.
	chunked := testDoc("d_1", "/notes/a.md")
	chunked.State = domain.DocStateChunked
	if _, err := store.UpsertDocument(ctx, chunked, testChunks("d_1", "hello")); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}

	var attempts int
	var nextRetry sql.NullInt64
	var lastError sql.NullString
	if err := db.Reader().QueryRow(
		"SELECT attempts, next_retry_at, last_error FROM documents WHERE id = 'd_1'").
		Scan(&attempts, &nextRetry, &lastError); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want it preserved at 3 across the pipeline's commit", attempts)
	}
	if !nextRetry.Valid || nextRetry.Int64 != 123 {
		t.Errorf("next_retry_at = %v, want it preserved", nextRetry)
	}
	if lastError.String != "embed: boom" {
		t.Errorf("last_error = %q, want it preserved", lastError.String)
	}
}

// Recording a permanent failure must not wipe the count that justified it,
// or a failed row reads as though it were never tried.
func TestUpsertDocumentKeepsRetryColumnsWhenRecordingFailure(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if _, err := seedUpsert(t, store, testDoc("d_1", "/notes/a.md"), nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := db.Writer().Exec(
		"UPDATE documents SET attempts = 4 WHERE id = 'd_1'"); err != nil {
		t.Fatal(err)
	}

	failed := testDoc("d_1", "/notes/a.md")
	failed.State = domain.DocStateFailed
	if _, err := store.UpsertDocument(ctx, failed, nil); err != nil {
		t.Fatalf("upsert failed doc: %v", err)
	}
	if err := store.MarkFailed(ctx, "d_1", "embed: poison (after 5 attempts)"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	var attempts int
	if err := db.Reader().QueryRow(
		"SELECT attempts FROM documents WHERE id = 'd_1'").Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 4 {
		t.Errorf("attempts = %d, want the count that led to the failure preserved", attempts)
	}
}

func TestGetByContentHash(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	docA := testDoc("d_a", "/notes/a.md")
	docB := testDoc("d_b", "/notes/b.md")
	docB.ContentHash = docA.ContentHash // duplicate content
	docC := testDoc("d_c", "/notes/c.md")
	docC.ContentHash = "hash-other"
	for _, doc := range []domain.Document{docA, docB, docC} {
		if _, err := seedUpsert(t, store, doc, nil); err != nil {
			t.Fatalf("upsert %s: %v", doc.ID, err)
		}
	}

	docs, err := store.GetByContentHash(ctx, docA.ContentHash)
	if err != nil {
		t.Fatalf("GetByContentHash: %v", err)
	}
	if len(docs) != 2 || docs[0].ID != "d_a" || docs[1].ID != "d_b" {
		t.Fatalf("GetByContentHash = %+v, want d_a and d_b", docs)
	}
	// Hydration parity with GetByPath.
	want, _, err := store.GetByPath(ctx, "/notes/a.md")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if docs[0].Path != want.Path || docs[0].Size != want.Size ||
		!docs[0].MTime.Equal(want.MTime) || docs[0].State != want.State {
		t.Errorf("hydration mismatch: GetByContentHash %+v vs GetByPath %+v", docs[0], want)
	}

	none, err := store.GetByContentHash(ctx, "hash-none")
	if err != nil {
		t.Fatalf("GetByContentHash(miss): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("GetByContentHash(miss) = %+v, want empty", none)
	}
}

func TestUpdateDocumentStat(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if _, err := seedUpsert(t, store, testDoc("d_1", "/notes/a.md"), testChunks("d_1", "alpha")); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := db.Writer().Exec(
		"UPDATE documents SET attempts = 5, next_retry_at = 123, last_error = 'boom' WHERE id = 'd_1'"); err != nil {
		t.Fatal(err)
	}

	newMTime := time.Unix(1800000000, 987654321)
	if err := store.UpdateDocumentStat(ctx, "d_1", 99, newMTime); err != nil {
		t.Fatalf("UpdateDocumentStat: %v", err)
	}

	doc, ok, err := store.GetByPath(ctx, "/notes/a.md")
	if err != nil || !ok {
		t.Fatalf("GetByPath: ok=%v err=%v", ok, err)
	}
	if doc.Size != 99 || !doc.MTime.Equal(newMTime) {
		t.Errorf("size/mtime = %d/%v, want 99/%v (ns round-trip)", doc.Size, doc.MTime, newMTime)
	}
	if doc.State != domain.DocStateChunked || doc.ContentHash != "hash-d_1" {
		t.Errorf("state/hash touched: %+v", doc)
	}
	// Chunks and retry columns untouched.
	var chunks, attempts int
	if err := db.Reader().QueryRow(
		"SELECT count(*), (SELECT attempts FROM documents WHERE id = 'd_1') FROM chunks WHERE doc_id = 'd_1'").
		Scan(&chunks, &attempts); err != nil {
		t.Fatal(err)
	}
	if chunks != 1 || attempts != 5 {
		t.Errorf("chunks=%d attempts=%d, want 1/5 (untouched)", chunks, attempts)
	}

	if err := store.UpdateDocumentStat(ctx, "d_missing", 1, newMTime); err == nil {
		t.Error("UpdateDocumentStat(unknown id) = nil, want error")
	}
}

func TestStageVersionsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	doc := testDoc("d_1", "/notes/a.md")
	doc.StageVersions = map[string]string{"chunker": "1", "embedder": "test-model"}
	if _, err := seedUpsert(t, store, doc, nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, ok, err := store.GetByPath(ctx, "/notes/a.md")
	if err != nil || !ok {
		t.Fatalf("GetByPath: ok=%v err=%v", ok, err)
	}
	if got.StageVersions["chunker"] != "1" || got.StageVersions["embedder"] != "test-model" {
		t.Errorf("StageVersions = %v", got.StageVersions)
	}
}

func TestListIndexable(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	seed := []struct {
		id, path string
		state    domain.DocState
	}{
		{"d_1", "/notes/b.md", domain.DocStateIndexed},
		{"d_2", "/notes/a.md", domain.DocStateDiscovered},
		{"d_3", "/notes/c.md", domain.DocStateFailed},
		{"d_4", "/notes/d.md", domain.DocStateDeleted},
		{"d_5", "/notes/e.md", domain.DocStateChunked},
	}
	for _, s := range seed {
		doc := testDoc(s.id, s.path)
		doc.State = s.state
		if _, err := seedUpsert(t, store, doc, nil); err != nil {
			t.Fatalf("seed %s: %v", s.id, err)
		}
	}

	docs, err := store.ListIndexable(ctx)
	if err != nil {
		t.Fatalf("ListIndexable: %v", err)
	}
	var got []string
	for _, d := range docs {
		got = append(got, d.ID)
	}
	// deleted excluded, failed included (config changes give failed docs a
	// fresh attempt); ordered by path (a, b, c, e).
	want := []string{"d_2", "d_1", "d_3", "d_5"}
	if !slices.Equal(got, want) {
		t.Fatalf("ListIndexable ids = %v, want %v", got, want)
	}
}

func TestUpdateDocumentState(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	doc := testDoc("d_1", "/notes/a.md")
	doc.StageVersions = map[string]string{"chunker": "1"}
	ids, err := seedUpsert(t, store, doc, testChunks("d_1", "alpha"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	spec := domain.EmbeddingSpec{Model: "test-model"}
	if err := store.EnsureVecTable(ctx, spec, 3); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}
	if err := store.UpsertVectors(ctx, ids, [][]float32{{1, 2, 3}}); err != nil {
		t.Fatalf("UpsertVectors: %v", err)
	}

	if err := store.UpdateDocumentState(ctx, "d_1",
		domain.DocStateChunked, domain.DocStateIndexed); err != nil {
		t.Fatalf("UpdateDocumentState: %v", err)
	}

	got, ok, err := store.GetByPath(ctx, "/notes/a.md")
	if err != nil || !ok {
		t.Fatalf("GetByPath: ok=%v err=%v", ok, err)
	}
	if got.State != domain.DocStateIndexed {
		t.Errorf("state = %q, want indexed", got.State)
	}
	if got.StageVersions["chunker"] != "1" {
		t.Errorf("stage versions touched: %v", got.StageVersions)
	}
	// Chunks and vectors survive the state flip (the reason this method
	// exists instead of a second UpsertDocument).
	hits, err := store.SearchVectors(ctx, []float32{1, 2, 3}, 1)
	if err != nil {
		t.Fatalf("SearchVectors: %v", err)
	}
	if len(hits) != 1 || hits[0].Chunk.Text != "alpha" {
		t.Fatalf("vectors gone after state flip: %+v", hits)
	}

	if err := store.UpdateDocumentState(ctx, "d_missing",
		domain.DocStateChunked, domain.DocStateIndexed); !errors.Is(err, domain.ErrDocumentGone) {
		t.Errorf("UpdateDocumentState(unknown id) = %v, want domain.ErrDocumentGone", err)
	}
}

// The state guard is an assertion that one goroutine owns every catalog write
// (ADR 0014), so its two failure modes have to stay distinguishable: a row
// that is gone is a purge landing between documents and the caller stands
// down, while a row sitting in an unexpected state is a second writer and must
// be loud. A single "zero rows affected" cannot say which.
func TestUpdateDocumentStateRejectsAnUnexpectedFromState(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	// Seeded chunked, then finalized — so the row is `indexed` and a second
	// finalize is aimed at a state it left.
	if _, err := seedUpsert(t, store, testDoc("d_1", "/notes/a.md"), testChunks("d_1", "alpha")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.UpdateDocumentState(ctx, "d_1", domain.DocStateChunked, domain.DocStateIndexed); err != nil {
		t.Fatalf("first finalize: %v", err)
	}

	err := store.UpdateDocumentState(ctx, "d_1", domain.DocStateChunked, domain.DocStateIndexed)
	if err == nil {
		t.Fatal("UpdateDocumentState(wrong from-state) = nil, want an error")
	}
	if errors.Is(err, domain.ErrDocumentGone) {
		t.Errorf("err = %v, want a loud error rather than ErrDocumentGone — the row is there", err)
	}
	// Both states named, or the report cannot be acted on.
	for _, want := range []string{string(domain.DocStateChunked), string(domain.DocStateIndexed)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name state %q", err, want)
		}
	}

	got, ok, err := store.GetByPath(ctx, "/notes/a.md")
	if err != nil || !ok {
		t.Fatalf("GetByPath: ok=%v err=%v", ok, err)
	}
	if got.State != domain.DocStateIndexed {
		t.Errorf("state = %q, want the row left untouched at indexed", got.State)
	}
}

func TestMarkFailed(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if _, err := seedUpsert(t, store, testDoc("d_1", "/notes/a.md"), nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.MarkFailed(ctx, "d_1", "undecodable: invalid UTF-8"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	var state, lastErr string
	if err := db.Reader().QueryRow(
		"SELECT state, last_error FROM documents WHERE id = 'd_1'").Scan(&state, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || lastErr != "undecodable: invalid UTF-8" {
		t.Errorf("state=%q last_error=%q", state, lastErr)
	}

	// A file change (discovery re-upserting as discovered) clears the failure.
	changed := testDoc("d_1", "/notes/a.md")
	changed.State = domain.DocStateDiscovered
	if _, err := store.UpsertDocument(ctx, changed, nil); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	var cleared sql.NullString
	if err := db.Reader().QueryRow(
		"SELECT last_error FROM documents WHERE id = 'd_1'").Scan(&cleared); err != nil {
		t.Fatal(err)
	}
	if cleared.Valid {
		t.Errorf("last_error = %q after re-upsert, want NULL", cleared.String)
	}

	if err := store.MarkFailed(ctx, "d_missing", "x"); err == nil {
		t.Error("MarkFailed(unknown id) = nil, want error")
	}
}

func TestCountsByStateReportsEveryStateIncludingZeros(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	for _, tc := range []struct {
		id    string
		path  string
		state domain.DocState
	}{
		{"d_1", "/notes/a.md", domain.DocStateIndexed},
		{"d_2", "/notes/b.md", domain.DocStateIndexed},
		{"d_3", "/notes/c.md", domain.DocStateFailed},
	} {
		doc := testDoc(tc.id, tc.path)
		doc.State = tc.state
		if _, err := seedUpsert(t, store, doc, testChunks(tc.id, "text")); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", tc.id, err)
		}
	}

	counts, err := store.CountsByState(ctx)
	if err != nil {
		t.Fatalf("CountsByState: %v", err)
	}

	// Every state must be present: a consumer must never have to tell
	// "no documents in this state" apart from "this build forgot the state".
	if len(counts) != len(domain.DocStates) {
		t.Errorf("CountsByState returned %d states, want %d", len(counts), len(domain.DocStates))
	}
	want := map[domain.DocState]int{
		domain.DocStateDiscovered: 0,
		domain.DocStateConverted:  0,
		domain.DocStateChunked:    0,
		domain.DocStateEmbedded:   0,
		domain.DocStateIndexed:    2,
		domain.DocStateFailed:     1,
		domain.DocStateDeleted:    0,
	}
	for state, n := range want {
		if got, ok := counts[state]; !ok {
			t.Errorf("CountsByState missing state %q", state)
		} else if got != n {
			t.Errorf("CountsByState[%q] = %d, want %d", state, got, n)
		}
	}
}

func TestCountsByStateOnEmptyIndexIsAllZeros(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)

	counts, err := store.CountsByState(context.Background())
	if err != nil {
		t.Fatalf("CountsByState: %v", err)
	}
	for _, state := range domain.DocStates {
		if got, ok := counts[state]; !ok || got != 0 {
			t.Errorf("CountsByState[%q] = %d (present %t), want 0 present", state, got, ok)
		}
	}
}

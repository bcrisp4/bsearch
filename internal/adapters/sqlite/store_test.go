package sqlite

import (
	"context"
	"database/sql"
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

	ids, err := store.UpsertDocument(ctx, testDoc("d_1", "/notes/a.md"), testChunks("d_1", "alpha", "beta"))
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

	first, err := store.UpsertDocument(ctx, testDoc("d_1", "/notes/a.md"), testChunks("d_1", "alpha", "beta"))
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

	if _, err := store.UpsertDocument(ctx, testDoc("d_1", "/notes/a.md"), testChunks("d_1", "alpha")); err != nil {
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

func TestUpsertDocumentDisplacesPathOwner(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if _, err := store.UpsertDocument(ctx, testDoc("d_old", "/notes/a.md"), testChunks("d_old", "alpha")); err != nil {
		t.Fatalf("upsert d_old: %v", err)
	}
	// New document ID claims the same path (deleted-and-recreated file).
	if _, err := store.UpsertDocument(ctx, testDoc("d_new", "/notes/a.md"), testChunks("d_new", "beta")); err != nil {
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

func TestUpsertDocumentResetsRetryColumns(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if _, err := store.UpsertDocument(ctx, testDoc("d_1", "/notes/a.md"), nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := db.Writer().Exec(
		"UPDATE documents SET attempts = 5, next_retry_at = 123, last_error = 'boom' WHERE id = 'd_1'"); err != nil {
		t.Fatal(err)
	}

	// File changed → re-upsert must reset the retry state.
	if _, err := store.UpsertDocument(ctx, testDoc("d_1", "/notes/a.md"), nil); err != nil {
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
		if _, err := store.UpsertDocument(ctx, doc, nil); err != nil {
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

	if _, err := store.UpsertDocument(ctx, testDoc("d_1", "/notes/a.md"), testChunks("d_1", "alpha")); err != nil {
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
	if _, err := store.UpsertDocument(ctx, doc, nil); err != nil {
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
		if _, err := store.UpsertDocument(ctx, doc, nil); err != nil {
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
	// failed and deleted excluded; ordered by path (a, b, e).
	want := []string{"d_2", "d_1", "d_5"}
	if len(got) != len(want) {
		t.Fatalf("ListIndexable ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ListIndexable ids = %v, want %v", got, want)
		}
	}
}

func TestUpdateDocumentState(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	doc := testDoc("d_1", "/notes/a.md")
	doc.StageVersions = map[string]string{"chunker": "1"}
	ids, err := store.UpsertDocument(ctx, doc, testChunks("d_1", "alpha"))
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

	if err := store.UpdateDocumentState(ctx, "d_1", domain.DocStateIndexed); err != nil {
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

	if err := store.UpdateDocumentState(ctx, "d_missing", domain.DocStateIndexed); err == nil {
		t.Error("UpdateDocumentState(unknown id) = nil, want error")
	}
}

func TestMarkFailed(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if _, err := store.UpsertDocument(ctx, testDoc("d_1", "/notes/a.md"), nil); err != nil {
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

	// A file change (re-upsert) clears the failure.
	if _, err := store.UpsertDocument(ctx, testDoc("d_1", "/notes/a.md"), nil); err != nil {
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

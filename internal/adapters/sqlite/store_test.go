package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
)

// seedUnreadPath is the path of the unread document newTestStore always
// plants.
const seedUnreadPath = "/seed/denied.md"

// newTestStore opens a fresh test database and always upserts one unread
// document — a documents row whose content_hash is NULL. The seed is a trap:
// any future query written `... NOT IN (SELECT content_hash FROM documents)`
// silently matches nothing when a NULL is present in that subquery, so the
// seed makes that class of bug fail on arrival (ADR 0015). Tests asserting
// counts must account for it (CatalogCounts.Files and Unread[denied] both
// include it).
func newTestStore(t *testing.T) (*Store, *DB) {
	t.Helper()
	db := openTestDB(t)
	store := NewStore(db)
	seed := domain.Document{
		Path:         seedUnreadPath,
		UnreadReason: domain.UnreadDenied,
		Size:         1,
		MTime:        time.Unix(1600000000, 0),
	}
	if err := store.UpsertDocuments(context.Background(), []domain.Document{seed}); err != nil {
		t.Fatalf("seed unread document: %v", err)
	}
	return store, db
}

func testDoc(hash, path string) domain.Document {
	return domain.Document{
		Path:        path,
		ContentHash: hash,
		Size:        42,
		MTime:       time.Unix(1700000000, 123456789),
	}
}

func unreadDoc(path string, reason domain.UnreadReason) domain.Document {
	return domain.Document{
		Path:         path,
		UnreadReason: reason,
		Size:         42,
		MTime:        time.Unix(1700000000, 0),
	}
}

func testChunks(texts ...string) []domain.Chunk {
	chunks := make([]domain.Chunk, len(texts))
	pos := 0
	for i, text := range texts {
		chunks[i] = domain.Chunk{
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

// seedChunked walks production's two steps: discovery announces the file
// (UpsertDocuments creates the content row at discovered), then the pipeline
// commits chunks and advances the content to chunked. Only discovery creates
// content rows, so a test that wants a chunked content has to do both.
func seedChunked(t *testing.T, store *Store, doc domain.Document, chunks []domain.Chunk) []int64 {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertDocuments(ctx, []domain.Document{doc}); err != nil {
		t.Fatalf("seed %s at %s: %v", doc.ContentHash, doc.Path, err)
	}
	if len(chunks) == 0 {
		return nil
	}
	c := domain.Content{Hash: doc.ContentHash, State: domain.ContentStateChunked}
	ids, err := store.StoreChunks(ctx, c, chunks)
	if err != nil {
		t.Fatalf("seed chunks for %s: %v", doc.ContentHash, err)
	}
	return ids
}

func countRows(t *testing.T, db *DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

func contentState(t *testing.T, db *DB, hash string) string {
	t.Helper()
	var state string
	if err := db.Reader().QueryRow(
		"SELECT state FROM content WHERE content_hash = ?", hash).Scan(&state); err != nil {
		t.Fatalf("read state of %s: %v", hash, err)
	}
	return state
}

func TestUpsertDocumentsCreatesDocumentAndContent(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	if err := store.UpsertDocuments(ctx, []domain.Document{testDoc("hash-1", "/notes/a.md")}); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	doc, ok, err := store.GetByPath(ctx, "/notes/a.md")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if !ok {
		t.Fatal("GetByPath: not found after upsert")
	}
	if doc.ContentHash != "hash-1" || doc.Size != 42 || doc.UnreadReason != "" {
		t.Errorf("GetByPath = %+v", doc)
	}
	if !doc.MTime.Equal(time.Unix(1700000000, 123456789)) {
		t.Errorf("MTime = %v, want ns-precision round-trip", doc.MTime)
	}
	if got := contentState(t, db, "hash-1"); got != string(domain.ContentStateDiscovered) {
		t.Errorf("content state = %q, want discovered (the eager insert)", got)
	}
}

func TestUpsertDocumentsBatch(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	docs := []domain.Document{
		testDoc("hash-1", "/notes/a.md"),
		testDoc("hash-2", "/notes/b.md"),
		testDoc("hash-3", "/notes/c.md"),
	}
	if err := store.UpsertDocuments(ctx, docs); err != nil {
		t.Fatalf("UpsertDocuments(batch): %v", err)
	}

	for _, doc := range docs {
		got, ok, err := store.GetByPath(ctx, doc.Path)
		if err != nil || !ok {
			t.Fatalf("GetByPath(%s): ok=%v err=%v", doc.Path, ok, err)
		}
		if got.ContentHash != doc.ContentHash {
			t.Errorf("%s hash = %q, want %q", doc.Path, got.ContentHash, doc.ContentHash)
		}
	}
	if n := countRows(t, db, "SELECT count(*) FROM content"); n != 3 {
		t.Errorf("content rows = %d, want 3", n)
	}
	// The batch plus the helper's unread seed.
	if n := countRows(t, db, "SELECT count(*) FROM documents"); n != 4 {
		t.Errorf("documents rows = %d, want 4", n)
	}
}

// A changed file is the same path-keyed write with a new hash: the document
// re-points, and the old content row is not this write's to clean up — it
// stays, in whatever state it reached, until the orphan sweep collects it.
func TestUpsertDocumentsRepointsPathAndLeavesOldContent(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	old := testDoc("hash-old", "/notes/a.md")
	oldIDs := seedChunked(t, store, old, testChunks("alpha", "beta"))
	if len(oldIDs) != 2 {
		t.Fatalf("seed chunk ids = %v, want 2", oldIDs)
	}

	changed := testDoc("hash-new", "/notes/a.md")
	if err := store.UpsertDocuments(ctx, []domain.Document{changed}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}

	doc, ok, err := store.GetByPath(ctx, "/notes/a.md")
	if err != nil || !ok {
		t.Fatalf("GetByPath: ok=%v err=%v", ok, err)
	}
	if doc.ContentHash != "hash-new" {
		t.Errorf("hash = %q, want hash-new", doc.ContentHash)
	}
	if got := contentState(t, db, "hash-old"); got != string(domain.ContentStateChunked) {
		t.Errorf("old content state = %q, want chunked — untouched until the sweep", got)
	}
	if n := countRows(t, db, "SELECT count(*) FROM chunks WHERE content_hash = 'hash-old'"); n != 2 {
		t.Errorf("old content chunks = %d, want 2 (the sweep's job, not this write's)", n)
	}
	if got := contentState(t, db, "hash-new"); got != string(domain.ContentStateDiscovered) {
		t.Errorf("new content state = %q, want discovered", got)
	}
}

// One content row per distinct byte sequence: a second identical file
// schedules no work, and re-discovering already-indexed content must not
// drag it back to discovered.
func TestUpsertDocumentsDeduplicatesContent(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	pair := []domain.Document{
		testDoc("hash-dup", "/notes/a.md"),
		testDoc("hash-dup", "/notes/b.md"),
	}
	if err := store.UpsertDocuments(ctx, pair); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM content WHERE content_hash = 'hash-dup'"); n != 1 {
		t.Fatalf("content rows = %d, want 1 for two paths sharing a hash", n)
	}

	if err := store.UpdateContentState(ctx, "hash-dup", domain.ContentStateIndexed); err != nil {
		t.Fatalf("UpdateContentState: %v", err)
	}
	if _, err := db.Writer().Exec(
		"UPDATE content SET updated_at = 100 WHERE content_hash = 'hash-dup'"); err != nil {
		t.Fatal(err)
	}
	// A third copy of the same bytes appears.
	if err := store.UpsertDocuments(ctx, []domain.Document{testDoc("hash-dup", "/notes/c.md")}); err != nil {
		t.Fatalf("third discovery: %v", err)
	}
	if got := contentState(t, db, "hash-dup"); got != string(domain.ContentStateIndexed) {
		t.Errorf("state = %q after re-discovery, want indexed — terminal content is never touched", got)
	}
	if got := contentUpdatedAt(t, db, "hash-dup"); got != 100 {
		t.Errorf("updated_at = %d after re-discovering indexed content, want 100 untouched", got)
	}
}

func contentUpdatedAt(t *testing.T, db *DB, hash string) int64 {
	t.Helper()
	var updatedAt int64
	if err := db.Reader().QueryRow(
		"SELECT updated_at FROM content WHERE content_hash = ?", hash).Scan(&updatedAt); err != nil {
		t.Fatalf("read updated_at of %s: %v", hash, err)
	}
	return updatedAt
}

// Re-referencing a non-terminal hash bumps its updated_at: an undo, a `git
// checkout` restoring an old revision, or a new copy of not-yet-indexed
// bytes must land in the claim's recency slice, not wait out the aging
// quarter behind rows that never stop being older.
func TestUpsertDocumentsReheatsNonTerminalContent(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	if err := store.UpsertDocuments(ctx, []domain.Document{testDoc("hash-1", "/notes/a.md")}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := db.Writer().Exec(
		"UPDATE content SET updated_at = 100 WHERE content_hash = 'hash-1'"); err != nil {
		t.Fatal(err)
	}

	if err := store.UpsertDocuments(ctx, []domain.Document{testDoc("hash-1", "/notes/copy.md")}); err != nil {
		t.Fatalf("re-reference: %v", err)
	}
	if got := contentUpdatedAt(t, db, "hash-1"); got <= 100 {
		t.Errorf("updated_at = %d after re-referencing non-terminal content, want it bumped past 100", got)
	}
	if got := contentState(t, db, "hash-1"); got != string(domain.ContentStateDiscovered) {
		t.Errorf("state = %q, want discovered — only updated_at moves", got)
	}
}

func TestUpsertDocumentsUnreadAndTransition(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	if err := store.UpsertDocuments(ctx, []domain.Document{
		unreadDoc("/notes/locked.md", domain.UnreadDataless),
	}); err != nil {
		t.Fatalf("upsert unread: %v", err)
	}
	doc, ok, err := store.GetByPath(ctx, "/notes/locked.md")
	if err != nil || !ok {
		t.Fatalf("GetByPath: ok=%v err=%v", ok, err)
	}
	if doc.ContentHash != "" || doc.UnreadReason != domain.UnreadDataless {
		t.Errorf("unread doc = %+v, want no hash and reason dataless", doc)
	}
	// An unread doc creates no content row.
	if n := countRows(t, db, "SELECT count(*) FROM content"); n != 0 {
		t.Errorf("content rows = %d after unread upsert, want 0", n)
	}

	// The file becomes readable: the hash fills in as the reason NULLs out.
	if err := store.UpsertDocuments(ctx, []domain.Document{
		testDoc("hash-1", "/notes/locked.md"),
	}); err != nil {
		t.Fatalf("upsert readable: %v", err)
	}
	doc, ok, err = store.GetByPath(ctx, "/notes/locked.md")
	if err != nil || !ok {
		t.Fatalf("GetByPath: ok=%v err=%v", ok, err)
	}
	if doc.ContentHash != "hash-1" || doc.UnreadReason != "" {
		t.Errorf("doc after transition = %+v, want hash set and reason cleared", doc)
	}
}

func TestGetByPathMissing(t *testing.T) {
	store, _ := newTestStore(t)

	_, ok, err := store.GetByPath(context.Background(), "/no/such/file.md")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if ok {
		t.Error("GetByPath reported a document for a path never stored")
	}
}

func TestStoreChunksReplacesChunksAndReturnsOrdinalOrder(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	first := seedChunked(t, store, testDoc("hash-1", "/notes/a.md"), testChunks("alpha", "beta"))
	if len(first) != 2 {
		t.Fatalf("first chunk ids = %v, want 2", first)
	}
	if !slices.IsSorted(first) {
		t.Errorf("chunk ids %v not in ordinal order", first)
	}

	second, err := store.StoreChunks(ctx,
		domain.Content{Hash: "hash-1", State: domain.ContentStateChunked},
		testChunks("gamma", "delta", "epsilon"))
	if err != nil {
		t.Fatalf("second StoreChunks: %v", err)
	}
	if len(second) != 3 {
		t.Fatalf("second chunk ids = %v, want 3", second)
	}
	if !slices.IsSorted(second) {
		t.Errorf("chunk ids %v not in ordinal order", second)
	}
	for _, oldID := range first {
		if slices.Contains(second, oldID) {
			t.Errorf("chunk id %d survived replacement", oldID)
		}
	}
	if n := countRows(t, db, "SELECT count(*) FROM chunks WHERE content_hash = 'hash-1'"); n != 3 {
		t.Errorf("chunk count = %d, want 3 (old chunks must be gone)", n)
	}
}

// The window this closes: the pipeline claims a content, every path
// referencing it vanishes, the sweep collects the row, and the pipeline then
// commits its chunks. An INSERT there would make content whose every source
// is gone permanently searchable.
func TestStoreChunksNeverCreatesTheContentRow(t *testing.T) {
	store, db := newTestStore(t)

	_, err := store.StoreChunks(context.Background(),
		domain.Content{Hash: "hash-never", State: domain.ContentStateChunked},
		testChunks("alpha"))
	if !errors.Is(err, domain.ErrContentGone) {
		t.Errorf("StoreChunks(missing content) = %v, want domain.ErrContentGone", err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM content WHERE content_hash = 'hash-never'"); n != 0 {
		t.Error("StoreChunks resurrected a swept content row")
	}
}

// The pipeline commits chunks with state=chunked *before* the embed network
// call. Resetting the retry columns there would park the row at attempts=0
// for the whole duration of that call — a crash in the window would hand the
// content a fresh budget, and content that can never be embedded would never
// reach the attempt cap.
func TestStoreChunksKeepsRetryColumns(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	if err := store.UpsertDocuments(ctx, []domain.Document{testDoc("hash-1", "/notes/a.md")}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := db.Writer().Exec(
		"UPDATE content SET attempts = 3, next_retry_at = 123, last_error = 'embed: boom' WHERE content_hash = 'hash-1'"); err != nil {
		t.Fatal(err)
	}

	if _, err := store.StoreChunks(ctx,
		domain.Content{Hash: "hash-1", State: domain.ContentStateChunked},
		testChunks("hello")); err != nil {
		t.Fatalf("StoreChunks: %v", err)
	}

	var attempts int
	var nextRetry sql.NullInt64
	var lastError sql.NullString
	if err := db.Reader().QueryRow(
		"SELECT attempts, next_retry_at, last_error FROM content WHERE content_hash = 'hash-1'").
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

func TestUpdateContentState(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	if err := store.UpsertDocuments(ctx, []domain.Document{testDoc("hash-1", "/notes/a.md")}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.UpdateContentState(ctx, "hash-1", domain.ContentStateIndexed); err != nil {
		t.Fatalf("UpdateContentState: %v", err)
	}
	if got := contentState(t, db, "hash-1"); got != string(domain.ContentStateIndexed) {
		t.Errorf("state = %q, want indexed", got)
	}

	err := store.UpdateContentState(ctx, "hash-missing", domain.ContentStateIndexed)
	if !errors.Is(err, domain.ErrContentGone) {
		t.Errorf("UpdateContentState(unknown hash) = %v, want domain.ErrContentGone", err)
	}
}

func TestMarkFailed(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	if err := store.UpsertDocuments(ctx, []domain.Document{testDoc("hash-1", "/notes/a.md")}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.MarkFailed(ctx, "hash-1", "undecodable: invalid UTF-8"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	var state, lastErr string
	if err := db.Reader().QueryRow(
		"SELECT state, last_error FROM content WHERE content_hash = 'hash-1'").Scan(&state, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || lastErr != "undecodable: invalid UTF-8" {
		t.Errorf("state=%q last_error=%q", state, lastErr)
	}

	err := store.MarkFailed(ctx, "hash-missing", "x")
	if !errors.Is(err, domain.ErrContentGone) {
		t.Errorf("MarkFailed(unknown hash) = %v, want domain.ErrContentGone", err)
	}
}

// Both routes to failed — the pipeline's failWith and the scheduler's
// attempt cap — must leave identical state: no chunks, no vectors in any
// generation. The cap route arrives here with chunks already committed
// (StoreChunks lands before the embed call), so MarkFailed has to clean up
// what failWith would have; failed content must not serve stale chunks.
func TestMarkFailedRemovesChunksAndVectors(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	// Chunk + embed under model A, then make model B current: the vectors
	// live in a non-current generation, which MarkFailed must still reach.
	if err := store.EnsureVecTable(ctx, vecSpec("model-a"), 3); err != nil {
		t.Fatal(err)
	}
	ids := seedChunked(t, store, testDoc("hash-1", "/notes/a.md"), testChunks("alpha", "beta"))
	if err := store.UpsertVectors(ctx, ids, [][]float32{{1, 0, 0}, {0, 1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureVecTable(ctx, vecSpec("model-b"), 3); err != nil {
		t.Fatal(err)
	}

	if err := store.MarkFailed(ctx, "hash-1", "embed: gave up after 5 attempts"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	if got := contentState(t, db, "hash-1"); got != "failed" {
		t.Errorf("state = %q, want failed", got)
	}
	if n := countRows(t, db, "SELECT count(*) FROM chunks WHERE content_hash = 'hash-1'"); n != 0 {
		t.Errorf("chunks after MarkFailed = %d, want 0 — failed content must not serve stale chunks", n)
	}
	if n := countRows(t, db, "SELECT count(*) FROM vec_chunks_1"); n != 0 {
		t.Errorf("vectors after MarkFailed = %d, want 0 across every generation", n)
	}
}

func TestDeleteByPathPrefix(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	// /notes/sub is the target; /notes/sub.md and /notes/subway/ are the
	// near misses a naive LIKE 'prefix%' would take with it.
	paths := map[string]string{
		"hash-dir":     "/notes/sub",
		"hash-inside":  "/notes/sub/a.md",
		"hash-deeper":  "/notes/sub/deep/b.md",
		"hash-sibling": "/notes/sub.md",
		"hash-subway":  "/notes/subway/c.md",
		"hash-other":   "/notes/other.md",
	}
	for hash, path := range paths {
		seedChunked(t, store, testDoc(hash, path), testChunks("text"))
	}

	removed, err := store.DeleteByPathPrefix(ctx, "/notes/sub")
	if err != nil {
		t.Fatalf("DeleteByPathPrefix: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed = %d, want 3 (the directory row and the two under it)", removed)
	}

	for _, hash := range []string{"hash-sibling", "hash-subway", "hash-other"} {
		if _, ok, err := store.GetByPath(ctx, paths[hash]); err != nil || !ok {
			t.Errorf("%s was removed; only the prefix and its descendants should go", paths[hash])
		}
	}
	for _, hash := range []string{"hash-dir", "hash-inside", "hash-deeper"} {
		if _, ok, err := store.GetByPath(ctx, paths[hash]); err != nil || ok {
			t.Errorf("%s survived the prefix delete", paths[hash])
		}
	}

	// Documents only: the removed rows' content and chunks stay until the
	// orphan sweep collects them (and contribute nothing to search meanwhile
	// — result assembly inner-joins documents).
	for _, hash := range []string{"hash-dir", "hash-inside", "hash-deeper"} {
		if n := countRows(t, db, "SELECT count(*) FROM content WHERE content_hash = ?", hash); n != 1 {
			t.Errorf("content row for %s gone after prefix delete; that is the sweep's job", hash)
		}
		if n := countRows(t, db, "SELECT count(*) FROM chunks WHERE content_hash = ?", hash); n != 1 {
			t.Errorf("chunks for %s gone after prefix delete; that is the sweep's job", hash)
		}
	}
}

func TestDeleteByPathPrefixRefusesToEmptyTheCatalog(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	if err := store.UpsertDocuments(ctx, []domain.Document{testDoc("hash-1", "/notes/a.md")}); err != nil {
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

func TestListWorkItems(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	// hash-1 lives at two paths; the newer mtime is the primary.
	older := testDoc("hash-1", "/w/a-old.md")
	older.MTime = time.Unix(100, 0)
	newer := testDoc("hash-1", "/w/b-new.md")
	newer.MTime = time.Unix(200, 0)
	solo := testDoc("hash-2", "/w/c.md")
	if err := store.UpsertDocuments(ctx, []domain.Document{older, newer, solo}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Terminal rows are included — stale-vs-current is the caller's call.
	if err := store.UpdateContentState(ctx, "hash-2", domain.ContentStateIndexed); err != nil {
		t.Fatalf("UpdateContentState: %v", err)
	}
	// Orphaned content (sweep pending) has no path to read from: omitted.
	if _, err := db.Writer().Exec(
		"INSERT INTO content (content_hash, state, created_at, updated_at) VALUES ('hash-orphan', 'discovered', 0, 0)"); err != nil {
		t.Fatal(err)
	}

	items, err := store.ListWorkItems(ctx)
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v, want 2", items)
	}
	if items[0].Content.Hash != "hash-1" || items[0].Path != "/w/b-new.md" {
		t.Errorf("items[0] = %+v, want hash-1 at its newest-mtime path", items[0])
	}
	if items[1].Content.Hash != "hash-2" || items[1].Path != "/w/c.md" {
		t.Errorf("items[1] = %+v, want hash-2 at /w/c.md", items[1])
	}
	if items[1].Content.State != domain.ContentStateIndexed {
		t.Errorf("items[1].State = %q, want indexed rows included", items[1].Content.State)
	}
}

func TestStageVersionsRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	if err := store.UpsertDocuments(ctx, []domain.Document{testDoc("hash-1", "/notes/a.md")}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	c := domain.Content{
		Hash:          "hash-1",
		State:         domain.ContentStateChunked,
		StageVersions: map[string]string{"chunker": "1", "embedding": "test-model"},
	}
	if _, err := store.StoreChunks(ctx, c, testChunks("alpha")); err != nil {
		t.Fatalf("StoreChunks: %v", err)
	}

	item, err := store.GetWork(ctx, "hash-1")
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	got := item.Content.StageVersions
	if got["chunker"] != "1" || got["embedding"] != "test-model" {
		t.Errorf("StageVersions = %v", got)
	}
}

func TestCatalogCountsZeroFilledOnFreshStore(t *testing.T) {
	store, _ := newTestStore(t)

	counts, err := store.CatalogCounts(context.Background())
	if err != nil {
		t.Fatalf("CatalogCounts: %v", err)
	}
	if counts.Files != 1 {
		t.Errorf("Files = %d, want 1 (the helper's unread seed)", counts.Files)
	}
	// Every state and reason present, zero included: a consumer must never
	// have to tell "none here" apart from "this build forgot it".
	if len(counts.Content) != len(domain.ContentStates) {
		t.Errorf("Content has %d states, want %d", len(counts.Content), len(domain.ContentStates))
	}
	for _, state := range domain.ContentStates {
		if got, ok := counts.Content[state]; !ok || got != 0 {
			t.Errorf("Content[%q] = %d (present %t), want 0 present", state, got, ok)
		}
	}
	if len(counts.Unread) != len(domain.UnreadReasons) {
		t.Errorf("Unread has %d reasons, want %d", len(counts.Unread), len(domain.UnreadReasons))
	}
	want := map[domain.UnreadReason]int{
		domain.UnreadDenied:   1, // the seed
		domain.UnreadDataless: 0,
		domain.UnreadIOError:  0,
	}
	for reason, n := range want {
		if got, ok := counts.Unread[reason]; !ok || got != n {
			t.Errorf("Unread[%q] = %d (present %t), want %d present", reason, got, ok, n)
		}
	}
}

func TestCatalogCountsReconciles(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// A duplicate pair, one solo readable, one more unread — plus the seed.
	if err := store.UpsertDocuments(ctx, []domain.Document{
		testDoc("hash-dup", "/notes/a.md"),
		testDoc("hash-dup", "/notes/b.md"),
		testDoc("hash-solo", "/notes/c.md"),
		unreadDoc("/notes/d.md", domain.UnreadDataless),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.UpdateContentState(ctx, "hash-solo", domain.ContentStateIndexed); err != nil {
		t.Fatalf("UpdateContentState: %v", err)
	}

	counts, err := store.CatalogCounts(ctx)
	if err != nil {
		t.Fatalf("CatalogCounts: %v", err)
	}
	if counts.Files != 5 {
		t.Errorf("Files = %d, want 5 (4 + seed)", counts.Files)
	}
	if counts.Unread[domain.UnreadDenied] != 1 || counts.Unread[domain.UnreadDataless] != 1 {
		t.Errorf("Unread = %v, want denied=1 (seed) dataless=1", counts.Unread)
	}
	if counts.Content[domain.ContentStateDiscovered] != 1 || counts.Content[domain.ContentStateIndexed] != 1 {
		t.Errorf("Content = %v, want discovered=1 (the dup pair) indexed=1", counts.Content)
	}

	// The reconciliation the one-transaction read exists for: files that have
	// content vs distinct contents, the gap being what deduplication saved.
	unread := 0
	for _, n := range counts.Unread {
		unread += n
	}
	contents := 0
	for _, n := range counts.Content {
		contents += n
	}
	if readable := counts.Files - unread; readable != 3 {
		t.Errorf("Files - sum(Unread) = %d, want 3", readable)
	}
	if contents != 2 {
		t.Errorf("sum(Content) = %d, want 2 distinct contents for 3 readable files", contents)
	}
}

// The orphan sweep runs with the shared NULL-hash seed row present in
// documents — the exact shape that turns a `NOT IN (SELECT content_hash …)`
// rewrite into a permanent silent no-op. If this test starts reporting zero
// collected, the sweep's predicate has regressed to NOT IN (ADR 0015).
func TestSweepOrphansCollectsUnreferencedContent(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, vecSpec("model-a"), 4); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}
	vec := func(lead float32) []float32 { return []float32{lead, 0, 0, 0} }

	keepIDs := seedChunked(t, store, testDoc("h_keep", "/keep.md"), testChunks("kept text"))
	if err := store.UpsertVectors(ctx, keepIDs, [][]float32{vec(1)}); err != nil {
		t.Fatalf("UpsertVectors keep: %v", err)
	}
	orphanIDs := seedChunked(t, store, testDoc("h_orphan", "/orphan.md"), testChunks("doomed text", "more doomed"))
	if err := store.UpsertVectors(ctx, orphanIDs, [][]float32{vec(2), vec(3)}); err != nil {
		t.Fatalf("UpsertVectors orphan: %v", err)
	}

	// A model switch mints generation 2 and leaves the orphan's vectors
	// behind in generation 1 — the sweep must clean every generation, not
	// just the current one.
	if err := store.EnsureVecTable(ctx, vecSpec("model-b"), 4); err != nil {
		t.Fatalf("EnsureVecTable gen2: %v", err)
	}

	if _, err := store.DeleteByPathPrefix(ctx, "/orphan.md"); err != nil {
		t.Fatalf("DeleteByPathPrefix: %v", err)
	}

	removed, err := store.SweepOrphans(ctx)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 — exactly the orphaned content, with the NULL-hash seed row present", removed)
	}
	if got := countRows(t, db, "SELECT count(*) FROM content WHERE content_hash = 'h_orphan'"); got != 0 {
		t.Errorf("orphan content rows = %d, want 0", got)
	}
	if got := countRows(t, db, "SELECT count(*) FROM chunks WHERE content_hash = 'h_orphan'"); got != 0 {
		t.Errorf("orphan chunk rows = %d, want 0 (FK cascade)", got)
	}
	if got := countRows(t, db, "SELECT count(*) FROM vec_chunks_1 WHERE rowid IN (?, ?)",
		orphanIDs[0], orphanIDs[1]); got != 0 {
		t.Errorf("orphan vec rows = %d, want 0 — generation 1 is not current and must be swept anyway", got)
	}

	// The referenced content is untouched, vectors included.
	if got := contentState(t, db, "h_keep"); got != "chunked" {
		t.Errorf("kept content state = %q, want chunked", got)
	}
	if got := countRows(t, db, "SELECT count(*) FROM vec_chunks_1 WHERE rowid = ?", keepIDs[0]); got != 1 {
		t.Errorf("kept vec rows = %d, want 1", got)
	}
}

// A catalog with nothing orphaned sweeps to zero — the common case, and the
// reason the scheduler only arms the sweep after deletions and edits.
func TestSweepOrphansNothingToCollect(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	seedChunked(t, store, testDoc("h_live", "/live.md"), testChunks("text"))
	removed, err := store.SweepOrphans(ctx)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}

// An edit orphans the old hash the same way a deletion does: the path
// re-points, nothing references the old content, and the sweep collects it.
func TestSweepOrphansCollectsTheOldHashOfAnEditedFile(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	seedChunked(t, store, testDoc("h_before", "/note.md"), testChunks("v1"))
	if err := store.UpsertDocuments(ctx, []domain.Document{testDoc("h_after", "/note.md")}); err != nil {
		t.Fatalf("re-point: %v", err)
	}

	removed, err := store.SweepOrphans(ctx)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (h_before)", removed)
	}
	if got := countRows(t, db, "SELECT count(*) FROM content WHERE content_hash = 'h_after'"); got != 1 {
		t.Errorf("new content rows = %d, want 1 — the edit's own row must survive", got)
	}
}

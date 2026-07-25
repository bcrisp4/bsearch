package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
)

// seedQueueDoc inserts a documents row directly. The queue columns are
// deliberately not on domain.Document's write path (UpsertDocument resets
// them), so raw SQL is the only honest way to set up a row that has already
// failed a few times.
func seedQueueDoc(t *testing.T, db *DB, id string, state domain.DocState, updatedAt int64, nextRetry any, stageVersions string) {
	t.Helper()
	if stageVersions == "" {
		stageVersions = "{}"
	}
	_, err := db.Writer().Exec(
		`INSERT INTO documents
		 (id, path, content_hash, size, mtime, state, attempts, next_retry_at, last_error, stage_versions, created_at, updated_at)
		 VALUES (?, ?, 'h', 1, 0, ?, 0, ?, NULL, ?, 0, ?)`,
		id, "/tmp/"+id+".md", string(state), nextRetry, stageVersions, updatedAt)
	if err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func claimedIDs(t *testing.T, docs []domain.Document) []string {
	t.Helper()
	ids := make([]string, len(docs))
	for i, d := range docs {
		ids[i] = d.ID
	}
	return ids
}

func TestClaimBatchSkipsTerminalStates(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	now := time.Unix(1000, 0)

	seedQueueDoc(t, db, "d_discovered", domain.DocStateDiscovered, 100, nil, "")
	seedQueueDoc(t, db, "d_chunked", domain.DocStateChunked, 100, nil, "")
	for _, state := range domain.TerminalDocStates {
		seedQueueDoc(t, db, "d_"+string(state), state, 100, nil, "")
	}

	docs, err := store.ClaimBatch(t.Context(), now, 32)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	got := claimedIDs(t, docs)
	if len(got) != 2 {
		t.Fatalf("claimed %v, want exactly the two non-terminal rows", got)
	}
	for _, id := range got {
		if strings.HasPrefix(id, "d_indexed") || strings.HasPrefix(id, "d_failed") || strings.HasPrefix(id, "d_deleted") {
			t.Errorf("claimed terminal row %q", id)
		}
	}
}

// A document in backoff must stay invisible until its time comes — otherwise
// backoff is decorative and a failing document is retried in a tight loop.
func TestClaimBatchHonoursNextRetryAt(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	now := time.Unix(1000, 0)

	seedQueueDoc(t, db, "d_due_null", domain.DocStateChunked, 100, nil, "")
	seedQueueDoc(t, db, "d_due_past", domain.DocStateChunked, 100, int64(999), "")
	seedQueueDoc(t, db, "d_due_now", domain.DocStateChunked, 100, int64(1000), "")
	seedQueueDoc(t, db, "d_not_due", domain.DocStateChunked, 100, int64(1001), "")

	docs, err := store.ClaimBatch(t.Context(), now, 32)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	for _, id := range claimedIDs(t, docs) {
		if id == "d_not_due" {
			t.Error("claimed a document whose next_retry_at is in the future")
		}
	}
	if len(docs) != 3 {
		t.Errorf("claimed %v, want the three due rows (NULL, past, and exactly now)", claimedIDs(t, docs))
	}
}

// The batch is composed, not just ordered: newly-changed files must reach the
// embedder in the current cycle even with a large backlog ahead of them, and
// the backlog must still drain rather than starve behind them.
func TestClaimBatchMixesRecentAndOldest(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)

	// 20 rows, updated_at 1..20. A limit of 8 takes 6 recent + 2 aging.
	for i := 1; i <= 20; i++ {
		seedQueueDoc(t, db, fmt.Sprintf("d_%02d", i), domain.DocStateDiscovered, int64(i), nil, "")
	}

	docs, err := store.ClaimBatch(t.Context(), time.Unix(1000, 0), 8)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	got := claimedIDs(t, docs)
	if len(got) != 8 {
		t.Fatalf("claimed %d documents, want 8: %v", len(got), got)
	}

	want := []string{
		// recency slice, newest first
		"d_20", "d_19", "d_18", "d_17", "d_16", "d_15",
		// aging slice, oldest first
		"d_01", "d_02",
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("claimed[%d] = %s, want %s (full batch %v)", i, got[i], w, got)
		}
	}
}

// When the queue holds fewer documents than the batch size, the two slices
// overlap. Returning a document twice would mean processing it twice in one
// drain, so the overlap must collapse rather than duplicate.
func TestClaimBatchDeduplicatesOverlappingSlices(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)

	for i := 1; i <= 3; i++ {
		seedQueueDoc(t, db, fmt.Sprintf("d_%d", i), domain.DocStateDiscovered, int64(i), nil, "")
	}

	docs, err := store.ClaimBatch(t.Context(), time.Unix(1000, 0), 32)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	seen := map[string]int{}
	for _, d := range docs {
		seen[d.ID]++
	}
	if len(docs) != 3 {
		t.Errorf("claimed %d documents from a queue of 3: %v", len(docs), claimedIDs(t, docs))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("document %s claimed %d times in one batch", id, n)
		}
	}
}

// updated_at has one-second resolution, so a bulk discovery pass writes
// thousands of rows sharing a timestamp. Without a tiebreak the claim could
// return the same arbitrary subset forever while the rest never came up.
func TestClaimBatchIsDeterministicUnderTiedTimestamps(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)

	for i := 1; i <= 10; i++ {
		seedQueueDoc(t, db, fmt.Sprintf("d_%02d", i), domain.DocStateDiscovered, 500, nil, "")
	}

	first, err := store.ClaimBatch(t.Context(), time.Unix(1000, 0), 4)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	second, err := store.ClaimBatch(t.Context(), time.Unix(1000, 0), 4)
	if err != nil {
		t.Fatalf("ClaimBatch (repeat): %v", err)
	}
	a, b := claimedIDs(t, first), claimedIDs(t, second)
	if len(a) != len(b) {
		t.Fatalf("repeat claim returned %d documents, first returned %d", len(b), len(a))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("claim is not deterministic: %v then %v", a, b)
			break
		}
	}
}

func TestClaimBatchHydratesRetryColumns(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)

	seedQueueDoc(t, db, "d_1", domain.DocStateChunked, 100, int64(900), "")
	if _, err := db.Writer().Exec(
		"UPDATE documents SET attempts = 3, last_error = 'embed: boom' WHERE id = 'd_1'"); err != nil {
		t.Fatalf("seed retry columns: %v", err)
	}

	docs, err := store.ClaimBatch(t.Context(), time.Unix(1000, 0), 8)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("claimed %d documents, want 1", len(docs))
	}
	doc := docs[0]
	if doc.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", doc.Attempts)
	}
	if !doc.NextRetryAt.Equal(time.Unix(900, 0)) {
		t.Errorf("NextRetryAt = %v, want %v", doc.NextRetryAt, time.Unix(900, 0))
	}
	if doc.LastError != "embed: boom" {
		t.Errorf("LastError = %q, want %q", doc.LastError, "embed: boom")
	}
}

// A NULL next_retry_at must hydrate as the zero Time, which the scheduler
// reads as "due now". A non-zero sentinel here would make every never-failed
// document look scheduled.
func TestClaimBatchHydratesNullRetryAsZeroTime(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	seedQueueDoc(t, db, "d_1", domain.DocStateDiscovered, 100, nil, "")

	docs, err := store.ClaimBatch(t.Context(), time.Unix(1000, 0), 8)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("claimed %d documents, want 1", len(docs))
	}
	if !docs[0].NextRetryAt.IsZero() {
		t.Errorf("NextRetryAt = %v, want the zero time for a NULL column", docs[0].NextRetryAt)
	}
	if docs[0].LastError != "" {
		t.Errorf("LastError = %q, want empty for a NULL column", docs[0].LastError)
	}
}

func TestClaimBatchZeroLimitClaimsNothing(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	seedQueueDoc(t, db, "d_1", domain.DocStateDiscovered, 100, nil, "")

	docs, err := store.ClaimBatch(t.Context(), time.Unix(1000, 0), 0)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("claimed %d documents with limit 0", len(docs))
	}
}

// The claim runs on every scheduler cycle for the life of the daemon. If the
// planner ever stops choosing the partial index the cost goes from "bounded
// by the backlog" to "bounded by the corpus", silently.
func TestClaimBatchUsesThePartialIndex(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	for i := range 50 {
		seedQueueDoc(t, db, fmt.Sprintf("d_%02d", i), domain.DocStateIndexed, int64(i), nil, "")
	}
	// ANALYZE so the planner is choosing on statistics, as it will in
	// production, rather than on an empty-table default.
	if _, err := db.Writer().Exec("ANALYZE"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if _, err := store.ClaimBatch(t.Context(), time.Unix(1000, 0), 32); err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}

	plan := explainClaim(t, db)
	if !strings.Contains(plan, "idx_documents_queue") {
		t.Errorf("claim query does not use idx_documents_queue; plan was:\n%s", plan)
	}
	if strings.Contains(plan, "SCAN documents") && !strings.Contains(plan, "USING INDEX") {
		t.Errorf("claim query falls back to a full table scan; plan was:\n%s", plan)
	}
}

// explainClaim renders EXPLAIN QUERY PLAN for the recency half of the claim.
func explainClaim(t *testing.T, db *DB) string {
	t.Helper()
	query := "EXPLAIN QUERY PLAN SELECT " + strings.Join(docColumns, ", ") + `
		FROM documents
		WHERE state NOT IN (` + terminalStatesSQL() + `)
		  AND (next_retry_at IS NULL OR next_retry_at <= ?)
		ORDER BY updated_at DESC, id DESC
		LIMIT ?`
	rows, err := db.Reader().Query(query, 1000, 32)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close() //nolint:errcheck // read-only

	var out []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return strings.Join(out, "\n")
}

func TestRescheduleRecordsBackoffWithoutTouchingState(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	seedQueueDoc(t, db, "d_1", domain.DocStateChunked, 100, nil, "")

	due := time.Unix(1500, 0)
	if err := store.Reschedule(t.Context(), "d_1", 2, due, "embed: connection refused"); err != nil {
		t.Fatalf("Reschedule: %v", err)
	}

	var state string
	var attempts int
	var nextRetry sql.NullInt64
	var lastErr sql.NullString
	err := db.Reader().QueryRow(
		"SELECT state, attempts, next_retry_at, last_error FROM documents WHERE id = 'd_1'").
		Scan(&state, &attempts, &nextRetry, &lastErr)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	// State must survive: the document is still chunked, its chunks are
	// still committed, and the retry resumes from there rather than
	// re-reading and re-chunking the file.
	if state != string(domain.DocStateChunked) {
		t.Errorf("state = %q, want it left at %q", state, domain.DocStateChunked)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
	if !nextRetry.Valid || nextRetry.Int64 != due.Unix() {
		t.Errorf("next_retry_at = %v, want %d", nextRetry, due.Unix())
	}
	if lastErr.String != "embed: connection refused" {
		t.Errorf("last_error = %q, want the reason", lastErr.String)
	}
}

func TestRescheduleUnknownDocumentIsLoud(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	err := store.Reschedule(t.Context(), "d_missing", 1, time.Unix(1500, 0), "boom")
	if err == nil {
		t.Error("Reschedule on an unknown document succeeded, want an error")
	}
}

func TestResetStaleRequeuesOutdatedDocuments(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)

	current := map[string]string{
		domain.StageChunker:       "c2",
		domain.StageEmbedding:     "fp2",
		domain.StageEmbeddingDims: "768",
		domain.StageVecMetric:     domain.VectorMetric,
	}
	fresh := `{"chunker":"c2","embedding":"fp2","embedding_dims":"768","vec_metric":"cosine"}`
	oldModel := `{"chunker":"c2","embedding":"fp1","embedding_dims":"768","vec_metric":"cosine"}`

	seedQueueDoc(t, db, "d_current", domain.DocStateIndexed, 100, nil, fresh)
	seedQueueDoc(t, db, "d_stale", domain.DocStateIndexed, 200, nil, oldModel)
	// A document indexed before a stage key existed reads as NULL through
	// json_extract — the oldest documents, and exactly the ones a <>
	// comparison would silently leave behind.
	seedQueueDoc(t, db, "d_ancient", domain.DocStateIndexed, 300, nil, `{"chunker":"c2"}`)
	// Failed rows are swept too: the configuration may have been the problem.
	seedQueueDoc(t, db, "d_failed_stale", domain.DocStateFailed, 400, nil, oldModel)
	// A deleted row is gone from disk; re-queueing it would index nothing.
	seedQueueDoc(t, db, "d_deleted", domain.DocStateDeleted, 500, nil, oldModel)

	moved, err := store.ResetStale(t.Context(), current)
	if err != nil {
		t.Fatalf("ResetStale: %v", err)
	}
	if moved != 3 {
		t.Errorf("moved = %d, want 3 (stale indexed, ancient, stale failed)", moved)
	}

	for id, want := range map[string]domain.DocState{
		"d_current":      domain.DocStateIndexed,
		"d_stale":        domain.DocStateDiscovered,
		"d_ancient":      domain.DocStateDiscovered,
		"d_failed_stale": domain.DocStateDiscovered,
		"d_deleted":      domain.DocStateDeleted,
	} {
		var state string
		if err := db.Reader().QueryRow("SELECT state FROM documents WHERE id = ?", id).Scan(&state); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if state != string(want) {
			t.Errorf("%s state = %q, want %q", id, state, want)
		}
	}
}

func TestResetStaleClearsRetryColumnsAndPreservesOrdering(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)

	current := map[string]string{domain.StageEmbedding: "fp2"}
	seedQueueDoc(t, db, "d_1", domain.DocStateFailed, 4242, int64(9999), `{"embedding":"fp1"}`)
	if _, err := db.Writer().Exec(
		"UPDATE documents SET attempts = 5, last_error = 'gave up' WHERE id = 'd_1'"); err != nil {
		t.Fatalf("seed retry columns: %v", err)
	}

	if _, err := store.ResetStale(t.Context(), current); err != nil {
		t.Fatalf("ResetStale: %v", err)
	}

	var attempts int
	var nextRetry sql.NullInt64
	var lastErr sql.NullString
	var updatedAt int64
	err := db.Reader().QueryRow(
		"SELECT attempts, next_retry_at, last_error, updated_at FROM documents WHERE id = 'd_1'").
		Scan(&attempts, &nextRetry, &lastErr, &updatedAt)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	// A document that exhausted its attempts under the old configuration
	// gets a full budget under the new one, and is due immediately.
	if attempts != 0 {
		t.Errorf("attempts = %d, want 0", attempts)
	}
	if nextRetry.Valid {
		t.Errorf("next_retry_at = %v, want NULL", nextRetry)
	}
	if lastErr.Valid {
		t.Errorf("last_error = %q, want NULL", lastErr.String)
	}
	// updated_at is deliberately untouched: stamping the whole corpus with
	// one timestamp would flatten the claim's ordering signal at exactly the
	// moment every document is queued at once.
	if updatedAt != 4242 {
		t.Errorf("updated_at = %d, want it preserved at 4242", updatedAt)
	}
}

// A stage that has not run yet contributes no key. If an absent key counted
// as a mismatch, enabling conversion or summaries later would re-embed the
// entire corpus for nothing.
func TestResetStaleIgnoresStagesNotInCurrent(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)

	seedQueueDoc(t, db, "d_1", domain.DocStateIndexed, 100, nil,
		`{"chunker":"c2","embedding":"fp2","converter":"bscribe-1"}`)

	moved, err := store.ResetStale(t.Context(), map[string]string{
		domain.StageChunker:   "c2",
		domain.StageEmbedding: "fp2",
	})
	if err != nil {
		t.Fatalf("ResetStale: %v", err)
	}
	if moved != 0 {
		t.Errorf("moved = %d, want 0 — a key absent from current is not staleness", moved)
	}
}

func TestResetStaleWithNoCurrentVersionsIsANoOp(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	seedQueueDoc(t, db, "d_1", domain.DocStateIndexed, 100, nil, `{"embedding":"fp1"}`)

	moved, err := store.ResetStale(t.Context(), nil)
	if err != nil {
		t.Fatalf("ResetStale: %v", err)
	}
	if moved != 0 {
		t.Errorf("moved = %d, want 0", moved)
	}
	var state string
	if err := db.Reader().QueryRow("SELECT state FROM documents WHERE id = 'd_1'").Scan(&state); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if state != string(domain.DocStateIndexed) {
		t.Errorf("state = %q, want it untouched", state)
	}
}

// Swept documents must be visible to the very next claim — that round trip is
// the whole mechanism by which a model change re-embeds the corpus.
func TestResetStaleMakesDocumentsClaimable(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	seedQueueDoc(t, db, "d_1", domain.DocStateIndexed, 100, nil, `{"embedding":"fp1"}`)

	before, err := store.ClaimBatch(ctx, time.Unix(1000, 0), 8)
	if err != nil {
		t.Fatalf("ClaimBatch before: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("claimed %v before the sweep; an indexed row must be invisible", claimedIDs(t, before))
	}

	if _, err := store.ResetStale(ctx, map[string]string{domain.StageEmbedding: "fp2"}); err != nil {
		t.Fatalf("ResetStale: %v", err)
	}

	after, err := store.ClaimBatch(ctx, time.Unix(1000, 0), 8)
	if err != nil {
		t.Fatalf("ClaimBatch after: %v", err)
	}
	if len(after) != 1 || after[0].ID != "d_1" {
		t.Errorf("claimed %v after the sweep, want [d_1]", claimedIDs(t, after))
	}
}

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
)

// seedQueueContent inserts a content row directly. The queue columns are
// deliberately not writable through any port — discovery only ever creates
// rows at discovered, and no write path sets attempts or next_retry_at
// wholesale — so raw SQL is the only honest way to set up a row that has
// already failed a few times.
func seedQueueContent(t *testing.T, db *DB, hash string, state domain.ContentState, updatedAt int64, nextRetry any, stageVersions string) {
	t.Helper()
	if stageVersions == "" {
		stageVersions = "{}"
	}
	_, err := db.Writer().Exec(
		`INSERT INTO content
		 (content_hash, state, stage_versions, attempts, next_retry_at, last_error, created_at, updated_at)
		 VALUES (?, ?, ?, 0, ?, NULL, 0, ?)`,
		hash, string(state), stageVersions, nextRetry, updatedAt)
	if err != nil {
		t.Fatalf("seed content %s: %v", hash, err)
	}
}

// seedQueuePath adds a documents row referencing hash — GetWork and the
// status queries need a live path to fan out to.
func seedQueuePath(t *testing.T, db *DB, hash, path string, mtime int64) {
	t.Helper()
	_, err := db.Writer().Exec(
		"INSERT INTO documents (path, content_hash, size, mtime) VALUES (?, ?, 1, ?)",
		path, hash, mtime)
	if err != nil {
		t.Fatalf("seed path %s -> %s: %v", path, hash, err)
	}
}

func claimedHashes(t *testing.T, contents []domain.Content) []string {
	t.Helper()
	hashes := make([]string, len(contents))
	for i, c := range contents {
		hashes[i] = c.Hash
	}
	return hashes
}

func TestClaimBatchSkipsTerminalStates(t *testing.T) {
	store, db := newTestStore(t)
	now := time.Unix(1000, 0)

	seedQueueContent(t, db, "h_discovered", domain.ContentStateDiscovered, 100, nil, "")
	seedQueueContent(t, db, "h_chunked", domain.ContentStateChunked, 100, nil, "")
	for _, state := range domain.TerminalContentStates {
		seedQueueContent(t, db, "h_"+string(state), state, 100, nil, "")
	}

	contents, err := store.ClaimBatch(t.Context(), now, 32)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	got := claimedHashes(t, contents)
	if len(got) != 2 {
		t.Fatalf("claimed %v, want exactly the two non-terminal rows", got)
	}
	for _, hash := range got {
		if hash == "h_indexed" || hash == "h_failed" {
			t.Errorf("claimed terminal row %q", hash)
		}
	}
}

// A content in backoff must stay invisible until its time comes — otherwise
// backoff is decorative and a failing content is retried in a tight loop.
func TestClaimBatchHonoursNextRetryAt(t *testing.T) {
	store, db := newTestStore(t)
	now := time.Unix(1000, 0)

	seedQueueContent(t, db, "h_due_null", domain.ContentStateChunked, 100, nil, "")
	seedQueueContent(t, db, "h_due_past", domain.ContentStateChunked, 100, int64(999), "")
	seedQueueContent(t, db, "h_due_now", domain.ContentStateChunked, 100, int64(1000), "")
	seedQueueContent(t, db, "h_not_due", domain.ContentStateChunked, 100, int64(1001), "")

	contents, err := store.ClaimBatch(t.Context(), now, 32)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	for _, hash := range claimedHashes(t, contents) {
		if hash == "h_not_due" {
			t.Error("claimed a content whose next_retry_at is in the future")
		}
	}
	if len(contents) != 3 {
		t.Errorf("claimed %v, want the three due rows (NULL, past, and exactly now)", claimedHashes(t, contents))
	}
}

// The batch is composed, not just ordered: newly-changed files must reach the
// embedder in the current cycle even with a large backlog ahead of them, and
// the backlog must still drain rather than starve behind them.
func TestClaimBatchMixesRecentAndOldest(t *testing.T) {
	store, db := newTestStore(t)

	// 20 rows, updated_at 1..20. A limit of 8 takes 6 recent + 2 aging.
	for i := 1; i <= 20; i++ {
		seedQueueContent(t, db, fmt.Sprintf("h_%02d", i), domain.ContentStateDiscovered, int64(i), nil, "")
	}

	contents, err := store.ClaimBatch(t.Context(), time.Unix(1000, 0), 8)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	got := claimedHashes(t, contents)
	if len(got) != 8 {
		t.Fatalf("claimed %d contents, want 8: %v", len(got), got)
	}

	want := []string{
		// recency slice, newest first
		"h_20", "h_19", "h_18", "h_17", "h_16", "h_15",
		// aging slice, oldest first
		"h_01", "h_02",
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("claimed[%d] = %s, want %s (full batch %v)", i, got[i], w, got)
		}
	}
}

// When the queue holds fewer contents than the batch size, the two slices
// overlap. Returning a content twice would mean processing it twice in one
// drain, so the overlap must collapse rather than duplicate.
func TestClaimBatchDeduplicatesOverlappingSlices(t *testing.T) {
	store, db := newTestStore(t)

	for i := 1; i <= 3; i++ {
		seedQueueContent(t, db, fmt.Sprintf("h_%d", i), domain.ContentStateDiscovered, int64(i), nil, "")
	}

	contents, err := store.ClaimBatch(t.Context(), time.Unix(1000, 0), 32)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	seen := map[string]int{}
	for _, c := range contents {
		seen[c.Hash]++
	}
	if len(contents) != 3 {
		t.Errorf("claimed %d contents from a queue of 3: %v", len(contents), claimedHashes(t, contents))
	}
	for hash, n := range seen {
		if n != 1 {
			t.Errorf("content %s claimed %d times in one batch", hash, n)
		}
	}
}

// updated_at has one-second resolution, so a bulk discovery pass writes
// thousands of rows sharing a timestamp. Without a tiebreak the claim could
// return the same arbitrary subset forever while the rest never came up.
func TestClaimBatchIsDeterministicUnderTiedTimestamps(t *testing.T) {
	store, db := newTestStore(t)

	for i := 1; i <= 10; i++ {
		seedQueueContent(t, db, fmt.Sprintf("h_%02d", i), domain.ContentStateDiscovered, 500, nil, "")
	}

	first, err := store.ClaimBatch(t.Context(), time.Unix(1000, 0), 4)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	second, err := store.ClaimBatch(t.Context(), time.Unix(1000, 0), 4)
	if err != nil {
		t.Fatalf("ClaimBatch (repeat): %v", err)
	}
	a, b := claimedHashes(t, first), claimedHashes(t, second)
	if len(a) != len(b) {
		t.Fatalf("repeat claim returned %d contents, first returned %d", len(b), len(a))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("claim is not deterministic: %v then %v", a, b)
			break
		}
	}
}

func TestClaimBatchHydratesRetryColumns(t *testing.T) {
	store, db := newTestStore(t)

	seedQueueContent(t, db, "h_1", domain.ContentStateChunked, 100, int64(900), "")
	if _, err := db.Writer().Exec(
		"UPDATE content SET attempts = 3, last_error = 'embed: boom' WHERE content_hash = 'h_1'"); err != nil {
		t.Fatalf("seed retry columns: %v", err)
	}

	contents, err := store.ClaimBatch(t.Context(), time.Unix(1000, 0), 8)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("claimed %d contents, want 1", len(contents))
	}
	c := contents[0]
	if c.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", c.Attempts)
	}
	if !c.NextRetryAt.Equal(time.Unix(900, 0)) {
		t.Errorf("NextRetryAt = %v, want %v", c.NextRetryAt, time.Unix(900, 0))
	}
	if c.LastError != "embed: boom" {
		t.Errorf("LastError = %q, want %q", c.LastError, "embed: boom")
	}
}

// A NULL next_retry_at must hydrate as the zero Time, which the scheduler
// reads as "due now". A non-zero sentinel here would make every never-failed
// content look scheduled.
func TestClaimBatchHydratesNullRetryAsZeroTime(t *testing.T) {
	store, db := newTestStore(t)
	seedQueueContent(t, db, "h_1", domain.ContentStateDiscovered, 100, nil, "")

	contents, err := store.ClaimBatch(t.Context(), time.Unix(1000, 0), 8)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("claimed %d contents, want 1", len(contents))
	}
	if !contents[0].NextRetryAt.IsZero() {
		t.Errorf("NextRetryAt = %v, want the zero time for a NULL column", contents[0].NextRetryAt)
	}
	if contents[0].LastError != "" {
		t.Errorf("LastError = %q, want empty for a NULL column", contents[0].LastError)
	}
}

func TestClaimBatchZeroLimitClaimsNothing(t *testing.T) {
	store, db := newTestStore(t)
	seedQueueContent(t, db, "h_1", domain.ContentStateDiscovered, 100, nil, "")

	contents, err := store.ClaimBatch(t.Context(), time.Unix(1000, 0), 0)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	if len(contents) != 0 {
		t.Errorf("claimed %d contents with limit 0", len(contents))
	}
}

// The claim runs on every scheduler cycle for the life of the daemon. If the
// planner ever stops choosing the partial index the cost goes from "bounded
// by the backlog" to "bounded by the corpus", silently.
func TestClaimBatchUsesThePartialIndex(t *testing.T) {
	store, db := newTestStore(t)
	for i := range 50 {
		seedQueueContent(t, db, fmt.Sprintf("h_%02d", i), domain.ContentStateIndexed, int64(i), nil, "")
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
	if !strings.Contains(plan, "idx_content_queue") {
		t.Errorf("claim query does not use idx_content_queue; plan was:\n%s", plan)
	}
	if strings.Contains(plan, "SCAN content") && !strings.Contains(plan, "USING INDEX") {
		t.Errorf("claim query falls back to a full table scan; plan was:\n%s", plan)
	}
}

// explainClaim renders EXPLAIN QUERY PLAN for the recency half of the claim.
// The SELECT below must be edited in lockstep with ClaimBatch's query in
// queue.go (same columns, same literal terminalStatesSQL splice) — a plan for
// a different statement proves nothing about the one production runs.
func explainClaim(t *testing.T, db *DB) string {
	t.Helper()
	query := "EXPLAIN QUERY PLAN SELECT " + contentColumns + `
		FROM content
		WHERE state NOT IN (` + terminalStatesSQL() + `)
		  AND (next_retry_at IS NULL OR next_retry_at <= ?)
		ORDER BY updated_at DESC, content_hash DESC
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

func TestGetWorkPicksNewestMTimePath(t *testing.T) {
	store, db := newTestStore(t)

	seedQueueContent(t, db, "h_1", domain.ContentStateChunked, 100, nil, "")
	seedQueuePath(t, db, "h_1", "/notes/old.md", 100)
	seedQueuePath(t, db, "h_1", "/notes/new.md", 200)

	item, err := store.GetWork(t.Context(), "h_1")
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if item.Path != "/notes/new.md" {
		t.Errorf("Path = %q, want the newest-mtime path", item.Path)
	}
	if item.Content.Hash != "h_1" || item.Content.State != domain.ContentStateChunked {
		t.Errorf("Content = %+v, want the hydrated h_1 row", item.Content)
	}
}

func TestGetWorkBreaksMTimeTiesByPathAscending(t *testing.T) {
	store, db := newTestStore(t)

	seedQueueContent(t, db, "h_1", domain.ContentStateDiscovered, 100, nil, "")
	seedQueuePath(t, db, "h_1", "/notes/b.md", 100)
	seedQueuePath(t, db, "h_1", "/notes/a.md", 100)

	item, err := store.GetWork(t.Context(), "h_1")
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if item.Path != "/notes/a.md" {
		t.Errorf("Path = %q, want the lexicographically smaller path on an mtime tie", item.Path)
	}
}

// Content still in the table but with no live path referencing it is deleted
// work: the file went while this content was in flight, and the sweep will
// collect the row. Both that and a swept row report the same way.
func TestGetWorkOrphanedContentIsGone(t *testing.T) {
	store, db := newTestStore(t)

	seedQueueContent(t, db, "h_orphan", domain.ContentStateChunked, 100, nil, "")

	_, err := store.GetWork(t.Context(), "h_orphan")
	if !errors.Is(err, domain.ErrContentGone) {
		t.Errorf("GetWork(orphaned) = %v, want domain.ErrContentGone", err)
	}
}

func TestGetWorkMissingContentIsGone(t *testing.T) {
	store, _ := newTestStore(t)

	_, err := store.GetWork(t.Context(), "h_never")
	if !errors.Is(err, domain.ErrContentGone) {
		t.Errorf("GetWork(missing) = %v, want domain.ErrContentGone", err)
	}
}

func TestRescheduleRecordsBackoffWithoutTouchingState(t *testing.T) {
	store, db := newTestStore(t)
	seedQueueContent(t, db, "h_1", domain.ContentStateChunked, 100, nil, "")

	due := time.Unix(1500, 0)
	if err := store.Reschedule(t.Context(), "h_1", 2, due, "embed: connection refused"); err != nil {
		t.Fatalf("Reschedule: %v", err)
	}

	var state string
	var attempts int
	var nextRetry sql.NullInt64
	var lastErr sql.NullString
	err := db.Reader().QueryRow(
		"SELECT state, attempts, next_retry_at, last_error FROM content WHERE content_hash = 'h_1'").
		Scan(&state, &attempts, &nextRetry, &lastErr)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	// State must survive: the content is still chunked, its chunks are still
	// committed, and the retry resumes from there rather than re-reading and
	// re-chunking the file.
	if state != string(domain.ContentStateChunked) {
		t.Errorf("state = %q, want it left at %q", state, domain.ContentStateChunked)
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

func TestRescheduleUnknownContentIsLoud(t *testing.T) {
	store, _ := newTestStore(t)
	err := store.Reschedule(t.Context(), "h_missing", 1, time.Unix(1500, 0), "boom")
	if !errors.Is(err, domain.ErrContentGone) {
		t.Errorf("Reschedule on an unknown content = %v, want domain.ErrContentGone", err)
	}
}

func TestResetStaleRequeuesOutdatedContents(t *testing.T) {
	store, db := newTestStore(t)

	current := map[string]string{
		domain.StageChunker:       "c2",
		domain.StageEmbedding:     "fp2",
		domain.StageEmbeddingDims: "768",
		domain.StageVecMetric:     domain.VectorMetric,
	}
	fresh := `{"chunker":"c2","embedding":"fp2","embedding_dims":"768","vec_metric":"cosine"}`
	oldModel := `{"chunker":"c2","embedding":"fp1","embedding_dims":"768","vec_metric":"cosine"}`

	seedQueueContent(t, db, "h_current", domain.ContentStateIndexed, 100, nil, fresh)
	seedQueueContent(t, db, "h_stale", domain.ContentStateIndexed, 200, nil, oldModel)
	// A content indexed before a stage key existed reads as NULL through
	// json_extract — the oldest contents, and exactly the ones a <>
	// comparison would silently leave behind.
	seedQueueContent(t, db, "h_ancient", domain.ContentStateIndexed, 300, nil, `{"chunker":"c2"}`)
	// Failed rows are swept too: the configuration may have been the problem.
	seedQueueContent(t, db, "h_failed_stale", domain.ContentStateFailed, 400, nil, oldModel)
	// Mid-pipeline rows are already queued; the sweep is about terminal ones.
	seedQueueContent(t, db, "h_inflight", domain.ContentStateChunked, 500, nil, oldModel)

	moved, err := store.ResetStale(t.Context(), current)
	if err != nil {
		t.Fatalf("ResetStale: %v", err)
	}
	if moved != 3 {
		t.Errorf("moved = %d, want 3 (stale indexed, ancient, stale failed)", moved)
	}

	for hash, want := range map[string]domain.ContentState{
		"h_current":      domain.ContentStateIndexed,
		"h_stale":        domain.ContentStateDiscovered,
		"h_ancient":      domain.ContentStateDiscovered,
		"h_failed_stale": domain.ContentStateDiscovered,
		"h_inflight":     domain.ContentStateChunked,
	} {
		if got := contentState(t, db, hash); got != string(want) {
			t.Errorf("%s state = %q, want %q", hash, got, want)
		}
	}
}

func TestResetStaleClearsRetryColumnsAndPreservesOrdering(t *testing.T) {
	store, db := newTestStore(t)

	current := map[string]string{domain.StageEmbedding: "fp2"}
	seedQueueContent(t, db, "h_1", domain.ContentStateFailed, 4242, int64(9999), `{"embedding":"fp1"}`)
	if _, err := db.Writer().Exec(
		"UPDATE content SET attempts = 5, last_error = 'gave up' WHERE content_hash = 'h_1'"); err != nil {
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
		"SELECT attempts, next_retry_at, last_error, updated_at FROM content WHERE content_hash = 'h_1'").
		Scan(&attempts, &nextRetry, &lastErr, &updatedAt)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	// A content that exhausted its attempts under the old configuration gets
	// a full budget under the new one, and is due immediately.
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
	// moment every content is queued at once.
	if updatedAt != 4242 {
		t.Errorf("updated_at = %d, want it preserved at 4242", updatedAt)
	}
}

// A stage that has not run yet contributes no key. If an absent key counted
// as a mismatch, enabling conversion or summaries later would re-embed the
// entire corpus for nothing.
func TestResetStaleIgnoresStagesNotInCurrent(t *testing.T) {
	store, db := newTestStore(t)

	seedQueueContent(t, db, "h_1", domain.ContentStateIndexed, 100, nil,
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
	store, db := newTestStore(t)
	seedQueueContent(t, db, "h_1", domain.ContentStateIndexed, 100, nil, `{"embedding":"fp1"}`)

	moved, err := store.ResetStale(t.Context(), nil)
	if err != nil {
		t.Fatalf("ResetStale: %v", err)
	}
	if moved != 0 {
		t.Errorf("moved = %d, want 0", moved)
	}
	if got := contentState(t, db, "h_1"); got != string(domain.ContentStateIndexed) {
		t.Errorf("state = %q, want it untouched", got)
	}
}

// Swept contents must be visible to the very next claim — that round trip is
// the whole mechanism by which a model change re-embeds the corpus.
func TestResetStaleMakesContentClaimable(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	seedQueueContent(t, db, "h_1", domain.ContentStateIndexed, 100, nil, `{"embedding":"fp1"}`)

	before, err := store.ClaimBatch(ctx, time.Unix(1000, 0), 8)
	if err != nil {
		t.Fatalf("ClaimBatch before: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("claimed %v before the sweep; an indexed row must be invisible", claimedHashes(t, before))
	}

	if _, err := store.ResetStale(ctx, map[string]string{domain.StageEmbedding: "fp2"}); err != nil {
		t.Fatalf("ResetStale: %v", err)
	}

	after, err := store.ClaimBatch(ctx, time.Unix(1000, 0), 8)
	if err != nil {
		t.Fatalf("ClaimBatch after: %v", err)
	}
	if len(after) != 1 || after[0].Hash != "h_1" {
		t.Errorf("claimed %v after the sweep, want [h_1]", claimedHashes(t, after))
	}
}

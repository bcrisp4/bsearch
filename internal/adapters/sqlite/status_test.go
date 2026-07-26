package sqlite

import (
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
)

// The split is the whole point of the query: a backlog draining normally and a
// backlog where everything is in backoff have identical totals.
func TestQueueDepthSplitsDueFromBackoff(t *testing.T) {
	store, db := newTestStore(t)
	now := time.Unix(1000, 0)

	seedQueueContent(t, db, "h_due_null", domain.ContentStateDiscovered, 100, nil, "")
	seedQueueContent(t, db, "h_due_past", domain.ContentStateChunked, 100, int64(999), "")
	seedQueueContent(t, db, "h_due_now", domain.ContentStateChunked, 100, int64(1000), "")
	seedQueueContent(t, db, "h_backoff", domain.ContentStateChunked, 100, int64(1001), "")

	depth, err := store.QueueDepth(t.Context(), now)
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth.Pending != 3 {
		t.Errorf("pending = %d, want 3 (NULL, past and exactly-now are all due)", depth.Pending)
	}
	if depth.Retrying != 1 {
		t.Errorf("retrying = %d, want 1", depth.Retrying)
	}
}

// Depth counts work, not history: a fully indexed corpus reports an empty
// queue however many contents it holds.
func TestQueueDepthIgnoresTerminalStates(t *testing.T) {
	store, db := newTestStore(t)

	for _, state := range domain.TerminalContentStates {
		seedQueueContent(t, db, "h_"+string(state), state, 100, nil, "")
	}

	depth, err := store.QueueDepth(t.Context(), time.Unix(1000, 0))
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth != (domain.QueueDepth{}) {
		t.Errorf("depth = %+v, want zero — every row is terminal", depth)
	}
}

func TestFailureReasonsGroupsLargestFirst(t *testing.T) {
	store, db := newTestStore(t)
	ctx := t.Context()

	fail := func(hash, path, reason string) {
		t.Helper()
		seedQueueContent(t, db, hash, domain.ContentStateDiscovered, 100, nil, "")
		seedQueuePath(t, db, hash, path, 0)
		if err := store.MarkFailed(ctx, hash, reason); err != nil {
			t.Fatalf("MarkFailed %s: %v", hash, err)
		}
	}
	fail("h_utf1", "/tmp/utf1.md", "not valid UTF-8")
	fail("h_utf2", "/tmp/utf2.md", "not valid UTF-8")
	fail("h_big", "/tmp/big.md", "too large to convert")
	// A live content must not appear among the failures.
	seedQueueContent(t, db, "h_working", domain.ContentStateChunked, 100, nil, "")
	seedQueuePath(t, db, "h_working", "/tmp/working.md", 0)

	groups, err := store.FailureReasons(ctx, 5)
	if err != nil {
		t.Fatalf("FailureReasons: %v", err)
	}
	want := []domain.FailureGroup{
		{Reason: "not valid UTF-8", Contents: 2, ExamplePath: "/tmp/utf1.md"},
		{Reason: "too large to convert", Contents: 1, ExamplePath: "/tmp/big.md"},
	}
	if len(groups) != len(want) {
		t.Fatalf("groups = %+v, want %+v", groups, want)
	}
	for i := range want {
		if groups[i] != want[i] {
			t.Errorf("group %d = %+v, want %+v", i, groups[i], want[i])
		}
	}
}

// The documents join fans out per referencing path; the count must not. Two
// copies of one undecodable file are one failed content, not two.
func TestFailureReasonsCountsDistinctContents(t *testing.T) {
	store, db := newTestStore(t)
	ctx := t.Context()

	seedQueueContent(t, db, "h_dup", domain.ContentStateDiscovered, 100, nil, "")
	seedQueuePath(t, db, "h_dup", "/tmp/b-copy.md", 0)
	seedQueuePath(t, db, "h_dup", "/tmp/a-copy.md", 0)
	if err := store.MarkFailed(ctx, "h_dup", "not valid UTF-8"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	groups, err := store.FailureReasons(ctx, 5)
	if err != nil {
		t.Fatalf("FailureReasons: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want one", groups)
	}
	if groups[0].Contents != 1 {
		t.Errorf("Contents = %d, want 1 — the join fan-out must not inflate the count", groups[0].Contents)
	}
	if groups[0].ExamplePath != "/tmp/a-copy.md" {
		t.Errorf("ExamplePath = %q, want the stable min(path) pick", groups[0].ExamplePath)
	}
}

// Failed content whose every path is gone (sweep pending) still has to
// report — dropping it would make the failure total disagree with the state
// counts. There is no path to show, so the example is empty.
func TestFailureReasonsReportsOrphanedFailedContent(t *testing.T) {
	store, db := newTestStore(t)

	seedQueueContent(t, db, "h_orphan", domain.ContentStateDiscovered, 100, nil, "")
	if err := store.MarkFailed(t.Context(), "h_orphan", "converter: boom"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	groups, err := store.FailureReasons(t.Context(), 5)
	if err != nil {
		t.Fatalf("FailureReasons: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want one", groups)
	}
	want := domain.FailureGroup{Reason: "converter: boom", Contents: 1, ExamplePath: ""}
	if groups[0] != want {
		t.Errorf("group = %+v, want %+v", groups[0], want)
	}
}

// The cap is what keeps status readable on a corpus that failed in fifty
// different ways; it must take the largest groups, not the first ones found.
func TestFailureReasonsRespectsLimit(t *testing.T) {
	store, db := newTestStore(t)
	ctx := t.Context()

	// Seeded smallest-first so a query that ignored the ordering would return
	// the wrong two.
	for i, reason := range []string{"one", "two", "three"} {
		for n := 0; n <= i; n++ {
			hash := "h_" + reason + "_" + string(rune('a'+n))
			seedQueueContent(t, db, hash, domain.ContentStateDiscovered, 100, nil, "")
			if err := store.MarkFailed(ctx, hash, reason); err != nil {
				t.Fatalf("MarkFailed %s: %v", hash, err)
			}
		}
	}

	groups, err := store.FailureReasons(ctx, 2)
	if err != nil {
		t.Fatalf("FailureReasons: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}
	if groups[0].Reason != "three" || groups[0].Contents != 3 {
		t.Errorf("first group = %+v, want the largest ('three', 3)", groups[0])
	}
	if groups[1].Reason != "two" || groups[1].Contents != 2 {
		t.Errorf("second group = %+v, want ('two', 2)", groups[1])
	}
}

// A row that reached failed without a recorded reason still has to be
// counted: dropping it would make the failure total disagree with the state
// counts, and "2 failed" with an empty list is the least useful status there
// is.
func TestFailureReasonsCountsRowsWithNoReason(t *testing.T) {
	store, db := newTestStore(t)

	seedQueueContent(t, db, "h_silent", domain.ContentStateFailed, 100, nil, "")

	groups, err := store.FailureReasons(t.Context(), 5)
	if err != nil {
		t.Fatalf("FailureReasons: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("got %+v, want one group", groups)
	}
	if groups[0].Reason != "" || groups[0].Contents != 1 {
		t.Errorf("group = %+v, want an empty reason counted once", groups[0])
	}
}

func TestFailureReasonsEmptyWhenNothingFailed(t *testing.T) {
	store, _ := newTestStore(t)

	groups, err := store.FailureReasons(t.Context(), 5)
	if err != nil {
		t.Fatalf("FailureReasons: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("groups = %+v, want none", groups)
	}
}

package sqlite

import (
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
)

// The split is the whole point of the query: a backlog draining normally and a
// backlog where everything is in backoff have identical totals.
func TestQueueDepthSplitsDueFromBackoff(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	now := time.Unix(1000, 0)

	seedQueueDoc(t, db, "d_due_null", domain.DocStateDiscovered, 100, nil, "")
	seedQueueDoc(t, db, "d_due_past", domain.DocStateChunked, 100, int64(999), "")
	seedQueueDoc(t, db, "d_due_now", domain.DocStateChunked, 100, int64(1000), "")
	seedQueueDoc(t, db, "d_backoff", domain.DocStateChunked, 100, int64(1001), "")

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
// queue however many documents it holds.
func TestQueueDepthIgnoresTerminalStates(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)

	for _, state := range domain.TerminalDocStates {
		seedQueueDoc(t, db, "d_"+string(state), state, 100, nil, "")
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
	db := openTestDB(t)
	store := NewStore(db)
	ctx := t.Context()

	fail := func(id, reason string) {
		t.Helper()
		seedQueueDoc(t, db, id, domain.DocStateDiscovered, 100, nil, "")
		if err := store.MarkFailed(ctx, id, reason); err != nil {
			t.Fatalf("MarkFailed %s: %v", id, err)
		}
	}
	fail("d_utf1", "not valid UTF-8")
	fail("d_utf2", "not valid UTF-8")
	fail("d_big", "too large to convert")
	// A live document must not appear among the failures.
	seedQueueDoc(t, db, "d_working", domain.DocStateChunked, 100, nil, "")

	groups, err := store.FailureReasons(ctx, 5)
	if err != nil {
		t.Fatalf("FailureReasons: %v", err)
	}
	want := []domain.FailureGroup{
		{Reason: "not valid UTF-8", Documents: 2, ExamplePath: "/tmp/d_utf1.md"},
		{Reason: "too large to convert", Documents: 1, ExamplePath: "/tmp/d_big.md"},
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

// The cap is what keeps status readable on a corpus that failed in fifty
// different ways; it must take the largest groups, not the first ones found.
func TestFailureReasonsRespectsLimit(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := t.Context()

	// Seeded smallest-first so a query that ignored the ordering would return
	// the wrong two.
	for i, reason := range []string{"one", "two", "three"} {
		for n := 0; n <= i; n++ {
			id := reason + "_" + string(rune('a'+n))
			seedQueueDoc(t, db, id, domain.DocStateDiscovered, 100, nil, "")
			if err := store.MarkFailed(ctx, id, reason); err != nil {
				t.Fatalf("MarkFailed %s: %v", id, err)
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
	if groups[0].Reason != "three" || groups[0].Documents != 3 {
		t.Errorf("first group = %+v, want the largest ('three', 3)", groups[0])
	}
	if groups[1].Reason != "two" || groups[1].Documents != 2 {
		t.Errorf("second group = %+v, want ('two', 2)", groups[1])
	}
}

// A row that reached failed without a recorded reason still has to be
// counted: dropping it would make the failure total disagree with the state
// counts, and "2 failed" with an empty list is the least useful status there
// is.
func TestFailureReasonsCountsRowsWithNoReason(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)

	seedQueueDoc(t, db, "d_silent", domain.DocStateFailed, 100, nil, "")

	groups, err := store.FailureReasons(t.Context(), 5)
	if err != nil {
		t.Fatalf("FailureReasons: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("got %+v, want one group", groups)
	}
	if groups[0].Reason != "" || groups[0].Documents != 1 {
		t.Errorf("group = %+v, want an empty reason counted once", groups[0])
	}
}

func TestFailureReasonsEmptyWhenNothingFailed(t *testing.T) {
	store := NewStore(openTestDB(t))

	groups, err := store.FailureReasons(t.Context(), 5)
	if err != nil {
		t.Fatalf("FailureReasons: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("groups = %+v, want none", groups)
	}
}

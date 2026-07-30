package sqlite

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/bcrisp4/bsearch/internal/domain"
)

// seedPaths upserts one readable document per path, each with its own
// content, and returns them sorted — the order ListPaths must produce.
func seedPaths(t *testing.T, store *Store, paths ...string) []string {
	t.Helper()
	docs := make([]domain.Document, len(paths))
	for i, path := range paths {
		docs[i] = testDoc(fmt.Sprintf("hash-%02d", i), path)
	}
	if err := store.UpsertDocuments(context.Background(), docs); err != nil {
		t.Fatalf("seed paths: %v", err)
	}
	sorted := slices.Clone(paths)
	slices.Sort(sorted)
	return sorted
}

// listAll pages through the whole catalog with the cursor the caller would
// use, and returns what it saw.
func listAll(t *testing.T, store *Store, limit int) []string {
	t.Helper()
	var seen []string
	cursor := ""
	for {
		batch, err := store.ListPaths(context.Background(), cursor, limit)
		if err != nil {
			t.Fatalf("ListPaths after %q: %v", cursor, err)
		}
		if len(batch) == 0 {
			return seen
		}
		seen = append(seen, batch...)
		cursor = batch[len(batch)-1]
	}
}

func TestListPathsPagesInPathOrder(t *testing.T) {
	store, _ := newTestStore(t)
	want := seedPaths(t, store, "/a/two.md", "/a/one.md", "/b/three.md", "/a/sub/four.md")
	want = append(want, seedUnreadPath)
	slices.Sort(want)

	for _, limit := range []int{1, 2, 100} {
		if got := listAll(t, store, limit); !slices.Equal(got, want) {
			t.Errorf("limit %d: paths = %v, want %v", limit, got, want)
		}
	}
}

// The unread row must be enumerated. A denied or dataless file is still a
// file with a catalog row, and a `WHERE content_hash IS NOT NULL` added here
// one day would make every such row unpurgeable forever with nothing to
// signal it — the reason newTestStore always plants one.
func TestListPathsIncludesUnreadRows(t *testing.T) {
	store, _ := newTestStore(t)
	seedPaths(t, store, "/a/one.md")

	if got := listAll(t, store, 100); !slices.Contains(got, seedUnreadPath) {
		t.Errorf("paths = %v, want the NULL-hash row %s enumerated", got, seedUnreadPath)
	}
}

func TestListPathsCursorIsExclusive(t *testing.T) {
	store, _ := newTestStore(t)
	seedPaths(t, store, "/a/one.md", "/a/two.md")

	got, err := store.ListPaths(context.Background(), "/a/one.md", 100)
	if err != nil {
		t.Fatalf("ListPaths: %v", err)
	}
	if slices.Contains(got, "/a/one.md") {
		t.Errorf("paths = %v, want the cursor itself excluded", got)
	}
}

// The caller deletes rows as it pages. That is safe because the cursor is a
// value rather than a row reference and every deleted path sorts at or before
// it — this pins the property rather than the reasoning.
func TestListPathsSkipsNothingWhenRowsAreDeletedBehindTheCursor(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	seedPaths(t, store, "/p/1.md", "/p/2.md", "/p/3.md", "/p/4.md")

	var seen []string
	cursor := ""
	for {
		batch, err := store.ListPaths(ctx, cursor, 2)
		if err != nil {
			t.Fatalf("ListPaths: %v", err)
		}
		if len(batch) == 0 {
			break
		}
		seen = append(seen, batch...)
		cursor = batch[len(batch)-1]
		if _, err := store.DeleteByPaths(ctx, batch); err != nil {
			t.Fatalf("DeleteByPaths: %v", err)
		}
	}

	want := []string{"/p/1.md", "/p/2.md", "/p/3.md", "/p/4.md", seedUnreadPath}
	slices.Sort(want)
	slices.Sort(seen)
	if !slices.Equal(seen, want) {
		t.Errorf("saw %v, want every row exactly once: %v", seen, want)
	}
	if n := countRows(t, store.db, "SELECT count(*) FROM documents"); n != 0 {
		t.Errorf("documents = %d, want the catalog emptied", n)
	}
}

// The property that distinguishes this from DeleteByPathPrefix, and the whole
// reason both exist: /a/b must not take /a/bc with it.
func TestDeleteByPathsRemovesExactlyThose(t *testing.T) {
	store, _ := newTestStore(t)
	seedPaths(t, store, "/a/b", "/a/bc", "/a/b/child.md")

	removed, err := store.DeleteByPaths(context.Background(), []string{"/a/b"})
	if err != nil {
		t.Fatalf("DeleteByPaths: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	got := listAll(t, store, 100)
	for _, want := range []string{"/a/bc", "/a/b/child.md"} {
		if !slices.Contains(got, want) {
			t.Errorf("paths = %v, want %s untouched — nothing beneath or beside is implied", got, want)
		}
	}
}

// Deletion removes the documents row and nothing else. Content survives while
// another path still holds it, and survives its last path too until the sweep
// collects it — search stops serving it either way, because result assembly
// inner-joins documents (ADR 0015).
func TestDeleteByPathsLeavesContentForTheSweep(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()
	const hash = "shared"
	seedChunked(t, store, testDoc(hash, "/dup/one.md"), testChunks("alpha", "beta"))
	if err := store.UpsertDocuments(ctx, []domain.Document{testDoc(hash, "/dup/two.md")}); err != nil {
		t.Fatalf("second path: %v", err)
	}

	if _, err := store.DeleteByPaths(ctx, []string{"/dup/one.md"}); err != nil {
		t.Fatalf("DeleteByPaths: %v", err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM chunks WHERE content_hash = ?", hash); n != 2 {
		t.Fatalf("chunks = %d, want 2 — the other path still holds this content", n)
	}
	if removed, err := store.SweepOrphans(ctx, domain.SweepScopeAll); err != nil || removed != 0 {
		t.Fatalf("SweepOrphans = %d, %v; want nothing collected while a path references it", removed, err)
	}

	if _, err := store.DeleteByPaths(ctx, []string{"/dup/two.md"}); err != nil {
		t.Fatalf("DeleteByPaths: %v", err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM content WHERE content_hash = ?", hash); n != 1 {
		t.Errorf("content = %d, want it still present — deletion never waits on the sweep", n)
	}
	if removed, err := store.SweepOrphans(ctx, domain.SweepScopeAll); err != nil || removed != 1 {
		t.Fatalf("SweepOrphans = %d, %v; want the orphan collected", removed, err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM chunks WHERE content_hash = ?", hash); n != 0 {
		t.Errorf("chunks = %d, want them cascaded away", n)
	}
}

func TestDeleteByPathsRefusesAnEmptyPath(t *testing.T) {
	store, _ := newTestStore(t)
	seedPaths(t, store, "/a/one.md")

	if _, err := store.DeleteByPaths(context.Background(), []string{"/a/one.md", ""}); err == nil {
		t.Fatal("DeleteByPaths = nil, want an error for an empty path")
	}
	if got := listAll(t, store, 100); !slices.Contains(got, "/a/one.md") {
		t.Errorf("paths = %v, want the batch rejected whole — no partial delete", got)
	}
}

func TestDeleteByPathsEmptySliceIsANoop(t *testing.T) {
	store, _ := newTestStore(t)
	removed, err := store.DeleteByPaths(context.Background(), nil)
	if err != nil || removed != 0 {
		t.Fatalf("DeleteByPaths(nil) = %d, %v; want 0, nil", removed, err)
	}
}

// One call carrying more paths than SQLite allows variables must still work:
// the caller batches by page, but nothing in the port says it has to.
func TestDeleteByPathsChunksLargeInput(t *testing.T) {
	store, db := newTestStore(t)
	const n = 1000
	paths := make([]string, n)
	for i := range n {
		paths[i] = fmt.Sprintf("/bulk/%04d.md", i)
	}
	seedPaths(t, store, paths...)

	removed, err := store.DeleteByPaths(context.Background(), paths)
	if err != nil {
		t.Fatalf("DeleteByPaths: %v", err)
	}
	if removed != n {
		t.Errorf("removed = %d, want %d", removed, n)
	}
	if got := countRows(t, db, "SELECT count(*) FROM documents"); got != 1 {
		t.Errorf("documents = %d, want only the unread seed left", got)
	}
}

func TestKnownMountsRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	got, remembered, err := store.KnownMounts(ctx)
	if err != nil || len(got) != 0 || remembered {
		t.Fatalf("KnownMounts on a fresh database = %v, %v; want empty", got, err)
	}

	want := []string{"/", "/Volumes/Archive"}
	if err := store.SetKnownMounts(ctx, want); err != nil {
		t.Fatalf("SetKnownMounts: %v", err)
	}
	if got, _, err := store.KnownMounts(ctx); err != nil || !slices.Equal(got, want) {
		t.Fatalf("KnownMounts = %v, %v; want %v", got, err, want)
	}

	// Replaces rather than merges, and an empty set is a legal value — a
	// volume forgotten has to actually be forgotten.
	if err := store.SetKnownMounts(ctx, nil); err != nil {
		t.Fatalf("SetKnownMounts(nil): %v", err)
	}
	if got, _, err := store.KnownMounts(ctx); err != nil || len(got) != 0 {
		t.Errorf("KnownMounts = %v, %v; want empty", got, err)
	}
}

// Paths are stored verbatim, including the characters that would need
// escaping in a LIKE pattern — the reason neither delete path uses one.
func TestDeleteByPathsHandlesAwkwardPaths(t *testing.T) {
	store, _ := newTestStore(t)
	awkward := []string{`/odd/100% sure.md`, `/odd/under_score.md`, `/odd/quote'name.md`}
	seedPaths(t, store, awkward...)

	removed, err := store.DeleteByPaths(context.Background(), awkward)
	if err != nil {
		t.Fatalf("DeleteByPaths: %v", err)
	}
	if removed != len(awkward) {
		t.Errorf("removed = %d, want %d", removed, len(awkward))
	}
	if got := listAll(t, store, 100); len(got) != 1 || !strings.Contains(got[0], "denied") {
		t.Errorf("paths = %v, want only the unread seed left", got)
	}
}

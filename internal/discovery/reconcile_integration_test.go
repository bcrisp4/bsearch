package discovery

// Integration: a real Scanner over a real filesystem into a real SQLite
// store, asserting ADR 0015's general accounting invariant — the three
// populations `bsearch status` reports must reconcile. Any future
// accounting that drops a population (the naive port that counts contents
// and loses unread files, say) fails here rather than shipping as a corpus
// that looks healthy with twelve denied files.

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/bcrisp4/bsearch/internal/adapters/sqlite"
	"github.com/bcrisp4/bsearch/internal/domain"
)

// reconcile asserts the invariant: files = unread + files-with-content, and
// distinct contents = the content rows that remain once orphans are swept.
func reconcile(t *testing.T, store *sqlite.Store, wantFiles, wantUnread, wantContents int) domain.CatalogCounts {
	t.Helper()
	counts, err := store.CatalogCounts(context.Background())
	if err != nil {
		t.Fatalf("CatalogCounts: %v", err)
	}
	unread := 0
	for _, n := range counts.Unread {
		unread += n
	}
	contents := 0
	for _, n := range counts.Content {
		contents += n
	}
	if counts.Files != wantFiles || unread != wantUnread || contents != wantContents {
		t.Fatalf("counts = files %d, unread %d, contents %d; want %d, %d, %d (dedup gap = files−unread−contents)",
			counts.Files, unread, contents, wantFiles, wantUnread, wantContents)
	}
	return counts
}

func TestCatalogCountsReconcileThroughScanEditAndSweep(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 0 does not deny")
	}
	ctx := context.Background()
	root := t.TempDir()
	write := func(name, text string) string {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write("a.md", "alpha")
	write("b.md", "beta")
	write("dup1.md", "same bytes")
	write("dup2.md", "same bytes")
	denied := write("denied.md", "locked away")
	if err := os.Chmod(denied, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o600) })
	write("cloud.md", "never read")

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "bsearch.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := sqlite.NewStore(db)

	s := New(store, Options{Include: []string{root}})
	s.dataless = func(info fs.FileInfo) bool { return info.Name() == "cloud.md" }

	if _, err := s.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	// 6 files; denied + dataless unread; 3 distinct contents (dup pair
	// collapses) — the gap of one is what deduplication saved.
	counts := reconcile(t, store, 6, 2, 3)
	if counts.Unread[domain.UnreadDenied] != 1 || counts.Unread[domain.UnreadDataless] != 1 {
		t.Errorf("unread = %v, want denied 1 and dataless 1, never folded", counts.Unread)
	}
	if counts.Content[domain.ContentStateDiscovered] != 3 {
		t.Errorf("content = %v, want 3 discovered", counts.Content)
	}

	// An edit re-points b.md; the old hash is orphaned until swept, and the
	// invariant must hold again once it is.
	write("b.md", "beta, revised")
	if _, err := s.Scan(ctx); err != nil {
		t.Fatalf("Scan after edit: %v", err)
	}
	if removed, err := store.SweepOrphans(ctx); err != nil || removed != 1 {
		t.Fatalf("SweepOrphans = %d, %v; want 1 orphan (b.md's old hash)", removed, err)
	}
	reconcile(t, store, 6, 2, 3)

	// Granting access clears the denial with no file change: the unread row
	// becomes a readable one and its content enters the queue.
	if err := os.Chmod(denied, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx); err != nil {
		t.Fatalf("Scan after chmod: %v", err)
	}
	counts = reconcile(t, store, 6, 1, 4)
	if counts.Unread[domain.UnreadDenied] != 0 {
		t.Errorf("unread = %v, want the denial cleared", counts.Unread)
	}
}

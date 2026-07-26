package discovery

// Integration: a real Scanner over a real filesystem into a real SQLite
// store, pinning ADR 0015's accounting at the store layer — the three
// populations CatalogCounts reports must reconcile through a scan, an edit
// plus orphan sweep, and a re-granted denial. (The daemon's port of these
// counts onto the wire is covered by internal/daemon's status tests; this
// file is the only place the full filesystem sequence runs against a real
// store.)
//
// This import makes the discovery test binary cgo-dependent; the in-memory
// fakeStore keeps the rest of the package's tests fast, not isolated — see
// the note on fakeStore.

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/adapters/sqlite"
	"github.com/bcrisp4/bsearch/internal/domain"
)

// assertCounts pins all three populations and their reconciliation
// arithmetic: files − unread must equal the count of documents rows holding
// content, and the content rows that remain (orphans are invisible to
// CatalogCounts) are the distinct contents those files share — the gap
// between the two is deduplication's saving.
func assertCounts(t *testing.T, store *sqlite.Store, wantFiles, wantUnread, wantContents int) domain.CatalogCounts {
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
		t.Fatalf("counts = files %d, unread %d, contents %d; want %d, %d, %d",
			counts.Files, unread, contents, wantFiles, wantUnread, wantContents)
	}
	if readable := counts.Files - unread; readable < contents {
		t.Fatalf("files−unread = %d < contents %d — more distinct contents than files holding any",
			readable, contents)
	}
	return counts
}

func TestCatalogCountsReconcileThroughScanEditAndSweep(t *testing.T) {
	ctx := context.Background()
	root := tmpDir(t)
	writeFile := func(name, text string) string {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	writeFile("a.md", "alpha")
	writeFile("b.md", "beta")
	writeFile("dup1.md", "same bytes")
	writeFile("dup2.md", "same bytes")
	writeFile("cloud.md", "never read")

	// Only the denial fixtures need non-root; the dedup, sweep, and
	// io_error accounting run everywhere. A positive probe rather than
	// Geteuid alone would be stronger (CAP_DAC_OVERRIDE, ACLs), but the
	// supported platform is macOS where neither defeats chmod 0.
	denyWorks := os.Geteuid() != 0
	var denied string
	if denyWorks {
		denied = writeFile("denied.md", "locked away")
		if err := os.Chmod(denied, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(denied, 0o600) })
	}

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "bsearch.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := sqlite.NewStore(db)

	s := New(store, Options{Include: []string{root}})
	s.dataless = func(info fs.FileInfo) bool { return info.Name() == "cloud.md" }

	// An io_error row rides along from the start: no portable fixture makes
	// os.Open fail with a non-permission error, so it is seeded through the
	// same port discovery writes through. Without it, a future accounting
	// that folds io_error into denied reports every flaky-disk file as a
	// Full Disk Access problem and no test notices.
	ioErrPath := filepath.Join(root, "flaky.bin.md")
	if err := store.UpsertDocuments(ctx, []domain.Document{{
		Path: ioErrPath, UnreadReason: domain.UnreadIOError, Size: 9, MTime: time.Unix(1700000000, 0),
	}}); err != nil {
		t.Fatalf("seed io_error row: %v", err)
	}

	files, unread := 6, 2 // a, b, dup1, dup2, cloud(dataless) + flaky(io_error)
	if denyWorks {
		files, unread = 7, 3
	}

	if _, err := s.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	// Three distinct contents for four readable files — the gap of one is
	// the dup pair collapsing.
	counts := assertCounts(t, store, files, unread, 3)
	if counts.Unread[domain.UnreadDataless] != 1 || counts.Unread[domain.UnreadIOError] != 1 {
		t.Errorf("unread = %v, want dataless 1 and io_error 1 in their own buckets", counts.Unread)
	}
	if denyWorks && counts.Unread[domain.UnreadDenied] != 1 {
		t.Errorf("unread = %v, want denied 1, never folded into the other reasons", counts.Unread)
	}
	if counts.Content[domain.ContentStateDiscovered] != 3 {
		t.Errorf("content = %v, want 3 discovered", counts.Content)
	}

	// The by-state split is real, not an artifact of everything sitting at
	// discovered: advance one content and assert the buckets move.
	aHash, ok, err := store.GetByPath(ctx, filepath.Join(root, "a.md"))
	if err != nil || !ok {
		t.Fatalf("GetByPath(a.md) = %v, %v", ok, err)
	}
	if err := store.UpdateContentState(ctx, aHash.ContentHash, domain.ContentStateIndexed); err != nil {
		t.Fatalf("UpdateContentState: %v", err)
	}
	counts = assertCounts(t, store, files, unread, 3)
	if counts.Content[domain.ContentStateDiscovered] != 2 || counts.Content[domain.ContentStateIndexed] != 1 {
		t.Errorf("content = %v, want the discovered/indexed split 2/1", counts.Content)
	}

	// An edit re-points b.md; the old hash is orphaned. CatalogCounts is
	// blind to orphans by design, so the identity holds before AND after
	// the sweep collects it.
	writeFile("b.md", "beta, revised")
	if _, err := s.Scan(ctx); err != nil {
		t.Fatalf("Scan after edit: %v", err)
	}
	counts = assertCounts(t, store, files, unread, 3)
	if removed, err := store.SweepOrphans(ctx, domain.SweepScopeQueue); err != nil || removed != 1 {
		t.Fatalf("SweepOrphans = %d, %v; want 1 orphan (b.md's old hash, non-terminal)", removed, err)
	}
	assertCounts(t, store, files, unread, 3)

	if !denyWorks {
		return
	}
	// Granting access clears the denial with no file change: the unread row
	// becomes a readable one and its content enters the queue.
	if err := os.Chmod(denied, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx); err != nil {
		t.Fatalf("Scan after chmod: %v", err)
	}
	counts = assertCounts(t, store, files, unread-1, 4)
	if counts.Unread[domain.UnreadDenied] != 0 {
		t.Errorf("unread = %v, want the denial cleared", counts.Unread)
	}
}

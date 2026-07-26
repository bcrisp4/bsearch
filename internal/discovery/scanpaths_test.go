package discovery

import (
	"errors"
	gomaps "maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bcrisp4/bsearch/internal/pathutil"
)

// scanPaths runs ScanPaths and fails on a fatal error, mirroring the scan
// helper the walk tests use.
func scanPaths(t *testing.T, store *fakeStore, opts Options, paths ...string) Result {
	t.Helper()
	res, err := New(store, opts).ScanPaths(t.Context(), paths)
	if err != nil {
		t.Fatalf("ScanPaths(%v): %v", paths, err)
	}
	return res
}

func TestScanPathsNewFile(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, "a.md")
	write(t, path, "hello")
	store := newFakeStore()

	res := scanPaths(t, store, Options{Include: []string{dir}}, path)

	if res.Discovered != 1 || res.Deleted != 0 || len(res.PathErrors) != 0 {
		t.Fatalf("Result = %+v, want 1 discovered", res)
	}
	if len(store.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(store.upserts))
	}
	if doc := store.upserts[0]; doc.Path != path || doc.ContentHash != hashOf("hello") {
		t.Errorf("doc = %+v", doc)
	}
	if !store.content[hashOf("hello")] {
		t.Error("no content row created for the new hash")
	}
}

func TestScanPathsUnchangedNoWrites(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, "a.md")
	write(t, path, "hello")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scanPaths(t, store, opts, path)
	res := scanPaths(t, store, opts, path)

	if res.Unchanged != 1 || res.Discovered != 0 {
		t.Errorf("Result = %+v, want 1 unchanged", res)
	}
	if len(store.upserts) != 1 {
		t.Errorf("upserts = %d after re-reconcile, want 1", len(store.upserts))
	}
}

// Out-of-scope paths are counted, not recorded as errors: the watcher
// subscribes to whole roots, so a burst of events for excluded or
// uninteresting files is the normal case, not a problem to report. The count
// exists because the all-ignored case is not normal — it is what a root
// spelled differently from the paths the watcher reports looks like, and
// without a number it looks like a quiet machine.
func TestScanPathsIgnoresOutOfScope(t *testing.T) {
	dir := tmpDir(t)
	outside := tmpDir(t)
	write(t, filepath.Join(dir, "a.md"), "in scope")
	write(t, filepath.Join(dir, "skip", "b.md"), "excluded")
	write(t, filepath.Join(dir, "notes.pdf"), "not text yet")
	write(t, filepath.Join(outside, "c.md"), "out of scope")
	store := newFakeStore()

	res := scanPaths(t, store, Options{
		Include:  []string{dir},
		Excluded: func(p string) bool { return strings.Contains(p, string(os.PathSeparator)+"skip") },
	},
		filepath.Join(dir, "a.md"),
		filepath.Join(dir, "skip", "b.md"),
		filepath.Join(dir, "notes.pdf"),
		filepath.Join(outside, "c.md"),
		"relative/path.md",
	)

	if res.Discovered != 1 || len(res.PathErrors) != 0 {
		t.Fatalf("Result = %+v, want only the in-scope file", res)
	}
	// The excluded file, the file outside every root, and the relative path.
	// Not the .pdf: it is in scope, just not a format indexed yet.
	if res.Ignored != 3 {
		t.Errorf("Ignored = %d, want 3", res.Ignored)
	}
	if got := store.upserts[0].Path; got != filepath.Join(dir, "a.md") {
		t.Errorf("upserted %q, want the in-scope file", got)
	}
}

// A directory arriving in the same batch as the files inside it — what a
// `cp -R` or an unarchive delivers — is walked once, and the files under it
// are not then re-stat'ed and re-queried one by one.
func TestScanPathsDirectoryCoversItsOwnFiles(t *testing.T) {
	dir := tmpDir(t)
	sub := filepath.Join(dir, "sub")
	write(t, filepath.Join(sub, "a.md"), "one")
	write(t, filepath.Join(sub, "b.md"), "two")
	store := newFakeStore()

	res := scanPaths(t, store, Options{Include: []string{dir}},
		filepath.Join(sub, "b.md"), sub, filepath.Join(sub, "a.md"))

	if res.Discovered != 2 || res.Unchanged != 0 {
		t.Fatalf("Result = %+v, want 2 discovered and no second visit", res)
	}
	if len(store.pathLookups) != 2 {
		t.Errorf("GetByPath calls = %d, want 2 — each file looked up once", len(store.pathLookups))
	}
}

func TestScanPathsSymlinkIgnored(t *testing.T) {
	dir := tmpDir(t)
	target := filepath.Join(dir, "real.md")
	link := filepath.Join(dir, "link.md")
	write(t, target, "content")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()

	res := scanPaths(t, store, Options{Include: []string{dir}}, link)

	if res.Discovered != 0 || len(store.upserts) != 0 {
		t.Errorf("symlink was indexed: %+v", res)
	}
}

// A directory event has to be descended: FSEvents reports a directory being
// created or moved as one event for the directory, never one per file in it.
func TestScanPathsDirectoryIsWalked(t *testing.T) {
	dir := tmpDir(t)
	sub := filepath.Join(dir, "sub")
	write(t, filepath.Join(sub, "a.md"), "one")
	write(t, filepath.Join(sub, "deeper", "b.md"), "two")
	store := newFakeStore()

	res := scanPaths(t, store, Options{Include: []string{dir}}, sub)

	if res.Discovered != 2 {
		t.Fatalf("Result = %+v, want both files under the directory", res)
	}
}

func TestScanPathsDeletedFilePurged(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, "a.md")
	write(t, path, "hello")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scanPaths(t, store, opts, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	res := scanPaths(t, store, opts, path)

	if res.Deleted != 1 {
		t.Fatalf("Result = %+v, want 1 deleted", res)
	}
	if _, ok := store.docs[path]; ok {
		t.Error("catalog row survived the deletion")
	}
	// Deletion removes documents only; the orphaned content row is the
	// sweep's to collect, never discovery's.
	if !store.content[hashOf("hello")] {
		t.Error("content row deleted by discovery; the sweep owns that")
	}
}

// A vanished path does not say whether it was a file or a directory, so the
// purge covers everything beneath it — which is what makes `rm -r` land.
func TestScanPathsDeletedDirectoryPurgesSubtree(t *testing.T) {
	dir := tmpDir(t)
	sub := filepath.Join(dir, "sub")
	write(t, filepath.Join(sub, "a.md"), "one")
	write(t, filepath.Join(sub, "b.md"), "two")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scanPaths(t, store, opts, sub)
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	res := scanPaths(t, store, opts, sub)

	if res.Deleted != 2 {
		t.Fatalf("Result = %+v, want both documents purged", res)
	}
	if len(store.docs) != 0 {
		t.Errorf("catalog still holds %d rows", len(store.docs))
	}
}

// `rm -rf` arrives as the directory plus an event per file inside it. Once
// the directory has been answered for, the files under it have been too:
// they must not each go on to report an unreachable parent, or a handled
// deletion becomes a burst of warnings and the signal ErrParentUnreachable
// carries is worthless.
func TestScanPathsSubtreeDeleteAnswersItsDescendants(t *testing.T) {
	dir := tmpDir(t)
	sub := filepath.Join(dir, "sub")
	write(t, filepath.Join(sub, "a.md"), "one")
	write(t, filepath.Join(sub, "b.md"), "two")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scanPaths(t, store, opts, sub)
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	// The files listed before the directory, so the ordering cannot come
	// from the order the watcher happened to report them in.
	res := scanPaths(t, store, opts,
		filepath.Join(sub, "a.md"), filepath.Join(sub, "b.md"), sub)

	if res.Deleted != 2 {
		t.Fatalf("Result = %+v, want both documents purged", res)
	}
	if len(res.PathErrors) != 0 {
		t.Errorf("PathErrors = %v, want none: the subtree was purged, not declined", res.PathErrors)
	}
	if !slices.Equal(store.prefixDeletes, []string{sub}) {
		t.Errorf("prefixDeletes = %v, want just the directory", store.prefixDeletes)
	}
}

// The guard that keeps a permissions blip or an unmounted volume from
// reading as a mass deletion: if the parent is gone too, this is not one
// file disappearing, and the slower scan-side reconcile (issue #57) can have
// it instead.
func TestScanPathsVanishedParentIsNotADeletion(t *testing.T) {
	dir := tmpDir(t)
	sub := filepath.Join(dir, "sub")
	path := filepath.Join(sub, "a.md")
	write(t, path, "hello")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scanPaths(t, store, opts, path)
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	// Only the file's own event arrives — the directory's does not.
	res := scanPaths(t, store, opts, path)

	if res.Deleted != 0 {
		t.Fatalf("purged despite an unreachable parent: %+v", res)
	}
	if len(store.prefixDeletes) != 0 {
		t.Errorf("DeleteByPathPrefix called for %v", store.prefixDeletes)
	}
	if !slices.ContainsFunc(res.PathErrors, func(pe PathError) bool {
		return pe.Path == path && errors.Is(pe.Err, ErrParentUnreachable)
	}) {
		t.Errorf("PathErrors = %v, want an ErrParentUnreachable for %s", res.PathErrors, path)
	}
}

// A file rewritten inside the debounce window arrives as both a deletion and
// a creation; it is neither gone nor worth purging.
func TestScanPathsRecreatedFileNotPurged(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, "a.md")
	write(t, path, "first")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scanPaths(t, store, opts, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	write(t, path, "second")
	res := scanPaths(t, store, opts, path)

	if res.Deleted != 0 || res.Discovered != 1 {
		t.Fatalf("Result = %+v, want a re-discovery and no deletion", res)
	}
}

// A rename is a documents row appearing at the new path with the same hash
// while the old path purges (ADR 0015). The hash is already in the catalog,
// so no content row is created and no pipeline work is scheduled.
func TestScanPathsRenameRepointsPathNotContent(t *testing.T) {
	dir := tmpDir(t)
	old := filepath.Join(dir, "old.md")
	renamed := filepath.Join(dir, "new.md")
	write(t, old, "stable content")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scanPaths(t, store, opts, old)
	hash := store.upserts[0].ContentHash
	if err := os.Rename(old, renamed); err != nil {
		t.Fatal(err)
	}

	// Old path first in the batch: the outcome must not depend on the
	// order the watcher happened to report them in.
	res := scanPaths(t, store, opts, old, renamed)

	if res.Discovered != 1 || res.Changed != 0 || res.Deleted != 1 {
		t.Fatalf("Result = %+v, want 1 discovered / 0 changed / 1 deleted", res)
	}
	doc, ok := store.docs[renamed]
	if !ok {
		t.Fatalf("no catalog row at the new path; rows = %v", catalogPaths(store))
	}
	if doc.ContentHash != hash {
		t.Errorf("ContentHash = %q, want the original %q", doc.ContentHash, hash)
	}
	if _, stale := store.docs[old]; stale {
		t.Error("a row is still parked at the old path")
	}
	// Same bytes → the existing content row is reused, nothing new created.
	if len(store.content) != 1 {
		t.Errorf("content rows = %v, want just the original", store.content)
	}
}

// The same property for a directory move, which arrives as two paths for the
// directory and none for the files inside it.
func TestScanPathsDirectoryRenameRepointsPaths(t *testing.T) {
	dir := tmpDir(t)
	old := filepath.Join(dir, "notes")
	renamed := filepath.Join(dir, "archive")
	write(t, filepath.Join(old, "a.md"), "one")
	write(t, filepath.Join(old, "b.md"), "two")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scanPaths(t, store, opts, old)
	if err := os.Rename(old, renamed); err != nil {
		t.Fatal(err)
	}

	res := scanPaths(t, store, opts, old, renamed)

	if res.Discovered != 2 || res.Changed != 0 || res.Deleted != 2 {
		t.Fatalf("Result = %+v, want 2 discovered / 0 changed / 2 deleted", res)
	}
	for name, content := range map[string]string{"a.md": "one", "b.md": "two"} {
		doc, ok := store.docs[filepath.Join(renamed, name)]
		if !ok {
			t.Errorf("no catalog row for %s at the new path", name)
			continue
		}
		if doc.ContentHash != hashOf(content) {
			t.Errorf("%s: ContentHash = %q, want %q", name, doc.ContentHash, hashOf(content))
		}
	}
	for _, name := range []string{"a.md", "b.md"} {
		if _, stale := store.docs[filepath.Join(old, name)]; stale {
			t.Errorf("%s still parked at the old path", name)
		}
	}
	// Two files, two hashes, no new content minted by the move.
	if len(store.content) != 2 {
		t.Errorf("content rows = %v, want the original two", store.content)
	}
}

func TestScanPathsUnreadablePathRecorded(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not apply")
	}
	dir := tmpDir(t)
	locked := filepath.Join(dir, "locked")
	path := filepath.Join(locked, "a.md")
	write(t, path, "hello")
	store := newFakeStore()
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	res := scanPaths(t, store, Options{Include: []string{dir}}, path)

	// EPERM is not ENOENT: an unreadable file is a problem to report, never
	// a document to purge.
	if res.Deleted != 0 || len(store.prefixDeletes) != 0 {
		t.Fatalf("purged an unreadable path: %+v", res)
	}
	if len(res.PathErrors) != 1 || res.PathErrors[0].Path != path {
		t.Errorf("PathErrors = %v, want one for %s", res.PathErrors, path)
	}
}

func TestScanPathsStoreFailureIsFatal(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, "a.md")
	write(t, path, "hello")
	store := newFakeStore()
	store.failWith = errors.New("disk on fire")

	if _, err := New(store, Options{Include: []string{dir}}).ScanPaths(t.Context(), []string{path}); err == nil {
		t.Fatal("ScanPaths returned nil, want the store failure")
	}
}

// Roots is what the watcher subscribes to, so it has to agree with the walk:
// canonicalized, deduplicated, deny-list applied.
func TestRoots(t *testing.T) {
	dir := tmpDir(t)
	nested := filepath.Join(dir, "nested")
	denied := filepath.Join(dir, "denied")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(denied, 0o755); err != nil {
		t.Fatal(err)
	}

	roots, errs := New(newFakeStore(), Options{
		Include:  []string{dir, nested, denied, dir},
		Excluded: func(p string) bool { return p == denied },
	}).Roots()

	if !slices.Equal(roots, []string{dir}) {
		t.Errorf("Roots = %v, want just %s (nested and duplicate folded in)", roots, dir)
	}
	// The excluded root is folded into dir before the deny-list is
	// consulted, so no error is expected here; an explicitly excluded
	// top-level root is what ErrRootExcluded is for.
	if len(errs) != 0 {
		t.Errorf("PathErrors = %v, want none", errs)
	}
}

func TestRootsExcludedRootRecorded(t *testing.T) {
	dir := tmpDir(t)
	roots, errs := New(newFakeStore(), Options{
		Include:  []string{dir},
		Excluded: func(p string) bool { return p == dir },
	}).Roots()

	if len(roots) != 0 {
		t.Errorf("Roots = %v, want none", roots)
	}
	if len(errs) != 1 || !errors.Is(errs[0].Err, ErrRootExcluded) {
		t.Errorf("PathErrors = %v, want one ErrRootExcluded", errs)
	}
}

// catalogPaths returns the catalog's paths in order, for failure messages.
func catalogPaths(store *fakeStore) []string {
	return slices.Sorted(gomaps.Keys(store.docs))
}

// The macOS data-volume firmlink is not a symlink, so EvalSymlinks leaves
// that spelling of a root exactly as configured — while the FSEvents adapter
// folds it out of every event path. Both sides have to fold, or a root
// written that way matches no event and the watcher indexes nothing while
// reporting itself perfectly healthy.
func TestRootsFoldTheDataVolumeFirmlink(t *testing.T) {
	// A path under the firmlink that does not exist on this machine: the
	// fold is a spelling rule, applied whether or not the root resolves.
	root := pathutil.DataVolumeRoot + "/Users/nobody/bsearch-not-here"
	roots, _ := New(newFakeStore(), Options{Include: []string{root}}).Roots()

	want := "/Users/nobody/bsearch-not-here"
	if !slices.Contains(roots, want) {
		t.Errorf("Roots() = %v, want the firmlink spelling folded to %q", roots, want)
	}
}

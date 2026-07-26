package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
)

// fakeStore is an in-memory DocumentStore that records calls, keeping
// these unit tests independent of cgo/sqlite (the real-store integration
// lands with issue #6).
type fakeStore struct {
	docs    map[string]domain.Document // keyed by path
	content map[string]bool            // content rows, keyed by hash

	batches       [][]domain.Document // every UpsertDocuments call
	upserts       []domain.Document   // the batches, flattened
	pathLookups   []string            // paths passed to GetByPath
	prefixDeletes []string            // paths passed to DeleteByPathPrefix
	failWith      error               // returned by every method when set
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		docs:    map[string]domain.Document{},
		content: map[string]bool{},
	}
}

func (f *fakeStore) GetByPath(_ context.Context, path string) (domain.Document, bool, error) {
	if f.failWith != nil {
		return domain.Document{}, false, f.failWith
	}
	f.pathLookups = append(f.pathLookups, path)
	doc, ok := f.docs[path]
	return doc, ok, nil
}

func (f *fakeStore) UpsertDocuments(_ context.Context, docs []domain.Document) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.batches = append(f.batches, slices.Clone(docs))
	for _, doc := range docs {
		f.docs[doc.Path] = doc
		// Eager content creation (INSERT ... ON CONFLICT DO NOTHING):
		// a new hash gets a row, an existing one is left alone.
		if doc.ContentHash != "" {
			f.content[doc.ContentHash] = true
		}
		f.upserts = append(f.upserts, doc)
	}
	return nil
}

func (f *fakeStore) DeleteByPathPrefix(_ context.Context, dir string) (int, error) {
	if f.failWith != nil {
		return 0, f.failWith
	}
	f.prefixDeletes = append(f.prefixDeletes, dir)
	var removed int
	for path := range f.docs {
		if path == dir || strings.HasPrefix(path, dir+string(filepath.Separator)) {
			delete(f.docs, path)
			removed++
		}
	}
	return removed, nil
}

var _ domain.DocumentStore = (*fakeStore)(nil)

// tmpDir is t.TempDir resolved to its canonical path: Scan canonicalizes
// include roots (macOS temp dirs live behind the /var → /private/var
// alias), so tests comparing walked paths need the resolved form.
func tmpDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func scan(t *testing.T, store *fakeStore, opts Options) Result {
	t.Helper()
	res, err := New(store, opts).Scan(t.Context())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return res
}

// hashOf is the lowercase hex sha256 of content — what discovery stores.
func hashOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestScanNewFile(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "a.md"), "hello")
	store := newFakeStore()

	res := scan(t, store, Options{Include: []string{dir}})

	if res.Discovered != 1 || res.Unchanged != 0 || res.Changed != 0 || len(res.PathErrors) != 0 {
		t.Fatalf("Result = %+v, want 1 discovered", res)
	}
	if len(store.batches) != 1 || len(store.upserts) != 1 {
		t.Fatalf("batches = %d, upserts = %d, want 1/1", len(store.batches), len(store.upserts))
	}
	doc := store.upserts[0]
	if doc.Path != filepath.Join(dir, "a.md") || doc.Size != 5 {
		t.Errorf("doc = %+v", doc)
	}
	if doc.ContentHash != hashOf("hello") {
		t.Errorf("ContentHash = %q, want sha256 of the bytes", doc.ContentHash)
	}
	if doc.UnreadReason != "" {
		t.Errorf("UnreadReason = %q, want empty for a read file", doc.UnreadReason)
	}
	// The eager insert: a new hash gets a content row at discovered.
	if !store.content[doc.ContentHash] {
		t.Errorf("no content row for %q", doc.ContentHash)
	}
}

func TestScanUnchangedNoWrites(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "a.md"), "hello")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scan(t, store, opts)
	res := scan(t, store, opts)

	if res.Unchanged != 1 || res.Discovered != 0 || res.Changed != 0 {
		t.Errorf("rescan Result = %+v, want 1 unchanged", res)
	}
	if len(store.upserts) != 1 {
		t.Errorf("upserts after rescan = %d, want 1 (no writes)", len(store.upserts))
	}
}

func TestScanTouchedSameContentRefreshesStat(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, "a.md")
	write(t, path, "hello")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scan(t, store, opts)
	newTime := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, newTime, newTime); err != nil {
		t.Fatal(err)
	}
	res := scan(t, store, opts)

	if res.Unchanged != 1 || res.Discovered != 0 || res.Changed != 0 {
		t.Errorf("Result = %+v, want 1 unchanged", res)
	}
	// Touched-but-identical re-upserts so the next scan's cheap check hits.
	if len(store.upserts) != 2 {
		t.Fatalf("upserts = %d, want 2 (stat refresh is an upsert)", len(store.upserts))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stored := store.docs[path]
	if stored.MTime.UnixNano() != info.ModTime().UnixNano() {
		t.Errorf("stored MTime = %v, want refreshed to %v", stored.MTime, info.ModTime())
	}
	if stored.Size != 5 {
		t.Errorf("stored Size = %d, want 5", stored.Size)
	}
	// Same bytes → no new content row.
	if len(store.content) != 1 {
		t.Errorf("content rows = %d, want 1", len(store.content))
	}
}

func TestScanEditedRepointsPath(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, "a.md")
	write(t, path, "hello")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scan(t, store, opts)
	oldHash := store.upserts[0].ContentHash
	write(t, path, "hello, edited")
	res := scan(t, store, opts)

	if res.Discovered != 1 || res.Changed != 1 {
		t.Errorf("Result = %+v, want 1 discovered / 1 changed", res)
	}
	doc := store.docs[path]
	if doc.ContentHash != hashOf("hello, edited") {
		t.Errorf("ContentHash = %q, want the new hash", doc.ContentHash)
	}
	// The old content row is orphaned, not deleted: the sweep collects it.
	if !store.content[oldHash] || !store.content[doc.ContentHash] {
		t.Errorf("content rows = %v, want both old %q and new %q", store.content, oldHash, doc.ContentHash)
	}
}

func TestScanCopySchedulesNoNewContent(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "a.md"), "same content")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scan(t, store, opts)
	// Copy: a second path with identical bytes.
	write(t, filepath.Join(dir, "b.md"), "same content")
	res := scan(t, store, opts)

	if res.Discovered != 1 || res.Changed != 0 {
		t.Errorf("Result = %+v, want 1 discovered / 0 changed", res)
	}
	if len(store.docs) != 2 {
		t.Errorf("documents = %v, want a row per path", catalogPaths(store))
	}
	// One distinct content → one content row; the copy schedules no work.
	if len(store.content) != 1 {
		t.Errorf("content rows = %d, want 1", len(store.content))
	}
}

func TestScanExcludedDirPruned(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "keep.md"), "keep")
	write(t, filepath.Join(dir, "node_modules", "junk.md"), "junk")
	store := newFakeStore()

	excluded := func(p string) bool { return filepath.Base(p) == "node_modules" }
	res := scan(t, store, Options{Include: []string{dir}, Excluded: excluded})

	if res.Discovered != 1 {
		t.Errorf("Result = %+v, want 1 discovered", res)
	}
	// Pruning means the file inside was never even looked up.
	for _, p := range store.pathLookups {
		if strings.Contains(p, "node_modules") {
			t.Errorf("excluded subtree was statted: %s", p)
		}
	}
}

func TestScanExcludedRootRecordedNotScanned(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "a.md"), "hello")
	store := newFakeStore()

	res := scan(t, store, Options{
		Include:  []string{dir},
		Excluded: func(p string) bool { return p == dir || strings.HasPrefix(p, dir+string(os.PathSeparator)) },
	})

	if res.Discovered != 0 || len(store.pathLookups) != 0 {
		t.Errorf("excluded root scanned: %+v, lookups %v", res, store.pathLookups)
	}
	// Exclusions win, but an explicitly configured root that scans to
	// nothing must leave a trace.
	if len(res.PathErrors) != 1 || !errors.Is(res.PathErrors[0].Err, ErrRootExcluded) {
		t.Errorf("PathErrors = %+v, want one ErrRootExcluded for the root", res.PathErrors)
	}
}

func TestScanSymlinkRootFollowed(t *testing.T) {
	target := tmpDir(t)
	write(t, filepath.Join(target, "a.md"), "hello")
	link := filepath.Join(t.TempDir(), "notes")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()

	res := scan(t, store, Options{Include: []string{link}})

	if res.Discovered != 1 || len(res.PathErrors) != 0 {
		t.Fatalf("Result = %+v, want symlinked root followed", res)
	}
	// The upserted path carries the fully resolved location.
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(resolved, "a.md"); store.upserts[0].Path != want {
		t.Errorf("Path = %q, want %q", store.upserts[0].Path, want)
	}
}

func TestScanSymlinkRootResolvingToIncludedRootVisitsOnce(t *testing.T) {
	target := tmpDir(t)
	write(t, filepath.Join(target, "a.md"), "hello")
	link := filepath.Join(t.TempDir(), "notes")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()

	// Symlink and its target both included: dedup must happen after
	// resolution, or the target is walked twice.
	res := scan(t, store, Options{Include: []string{link, target}})

	if res.Discovered != 1 || res.Unchanged != 0 || len(store.upserts) != 1 {
		t.Errorf("Result = %+v with %d upserts, want file visited once", res, len(store.upserts))
	}
}

func TestScanExcludedFile(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "server.pem.md"), "not really a cert but excluded")
	write(t, filepath.Join(dir, "keep.md"), "keep")
	store := newFakeStore()

	excluded := func(p string) bool { return strings.Contains(filepath.Base(p), "pem") }
	res := scan(t, store, Options{Include: []string{dir}, Excluded: excluded})

	if res.Discovered != 1 || store.upserts[0].Path != filepath.Join(dir, "keep.md") {
		t.Errorf("Result = %+v, upserts = %+v", res, store.upserts)
	}
}

func TestScanSkipsNonTextAndSymlinks(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "a.md"), "hello")
	write(t, filepath.Join(dir, "b.pdf"), "%PDF")
	write(t, filepath.Join(dir, "noext"), "text")
	if err := os.Symlink(filepath.Join(dir, "a.md"), filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	// Symlinked dir cycle: must not loop or index through it.
	if err := os.Symlink(dir, filepath.Join(dir, "cycle")); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()

	res := scan(t, store, Options{Include: []string{dir}})

	if res.Discovered != 1 {
		t.Errorf("Result = %+v, want only a.md discovered", res)
	}
	if len(store.upserts) != 1 || store.upserts[0].Path != filepath.Join(dir, "a.md") {
		t.Errorf("upserts = %+v", store.upserts)
	}
}

func TestScanTextExtensions(t *testing.T) {
	dir := tmpDir(t)
	for _, name := range []string{"a.md", "b.markdown", "c.txt", "D.MD"} {
		write(t, filepath.Join(dir, name), "content of "+name)
	}
	store := newFakeStore()

	res := scan(t, store, Options{Include: []string{dir}})

	if res.Discovered != 4 {
		t.Errorf("Discovered = %d, want 4 (.md/.markdown/.txt, case-insensitive)", res.Discovered)
	}
}

func TestScanPermissionError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not apply")
	}
	dir := tmpDir(t)
	locked := filepath.Join(dir, "locked")
	write(t, filepath.Join(locked, "secret.md"), "cannot read")
	write(t, filepath.Join(dir, "open.md"), "readable")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	store := newFakeStore()

	res := scan(t, store, Options{Include: []string{dir}})

	if res.Discovered != 1 {
		t.Errorf("Result = %+v, want sibling still discovered", res)
	}
	if len(res.PathErrors) != 1 || res.PathErrors[0].Path != locked {
		t.Fatalf("PathErrors = %+v, want one for %s", res.PathErrors, locked)
	}
}

func TestScanMissingRoot(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "a.md"), "hello")
	// Sibling of dir, not nested under it — a nested missing root would be
	// deduped away by root normalization before it could error.
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	store := newFakeStore()

	res := scan(t, store, Options{Include: []string{missing, dir}})

	if res.Discovered != 1 {
		t.Errorf("Result = %+v, want other root scanned", res)
	}
	if len(res.PathErrors) != 1 || res.PathErrors[0].Path != missing {
		t.Errorf("PathErrors = %+v, want one for missing root", res.PathErrors)
	}
}

func TestScanOverlappingRootsVisitOnce(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "sub", "a.md"), "hello")
	store := newFakeStore()

	res := scan(t, store, Options{Include: []string{dir, filepath.Join(dir, "sub"), dir}})

	if res.Discovered != 1 || len(store.upserts) != 1 {
		t.Errorf("Result = %+v with %d upserts, want file visited once", res, len(store.upserts))
	}
}

func TestScanDatalessSkipped(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "cloud.md"), "placeholder")
	write(t, filepath.Join(dir, "local.md"), "on disk")
	store := newFakeStore()

	s := New(store, Options{Include: []string{dir}})
	s.dataless = func(info os.FileInfo) bool { return info.Name() == "cloud.md" }
	res, err := s.Scan(t.Context())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if res.Dataless != 1 || res.Discovered != 1 {
		t.Errorf("Result = %+v, want 1 dataless / 1 discovered", res)
	}
	// The placeholder must never be opened or looked up.
	for _, p := range store.pathLookups {
		if filepath.Base(p) == "cloud.md" {
			t.Errorf("dataless file was processed: %s", p)
		}
	}
}

func TestScanStoreFailureIsFatal(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "a.md"), "hello")
	store := newFakeStore()
	store.failWith = errors.New("disk full")

	_, err := New(store, Options{Include: []string{dir}}).Scan(t.Context())
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("Scan = %v, want fatal store error", err)
	}
}

func TestScanContextCancelled(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "a.md"), "hello")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := New(newFakeStore(), Options{Include: []string{dir}}).Scan(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Scan = %v, want context.Canceled", err)
	}
}

func TestNormalizeRoots(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "dedup", in: []string{"/a", "/a"}, want: []string{"/a"}},
		{name: "nested dropped", in: []string{"/a/b", "/a"}, want: []string{"/a"}},
		{name: "boundary kept", in: []string{"/a", "/ab"}, want: []string{"/a", "/ab"}},
		{name: "non-adjacent nested", in: []string{"/a", "/a!", "/a/b"}, want: []string{"/a", "/a!"}},
		{name: "trailing slash cleaned", in: []string{"/a/", "/a/b"}, want: []string{"/a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeRoots(tt.in); !slices.Equal(got, tt.want) {
				t.Errorf("normalizeRoots(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

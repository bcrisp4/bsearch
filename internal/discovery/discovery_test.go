package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
)

// fakeStore is an in-memory DocumentStore that records calls, keeping
// these unit tests independent of cgo/sqlite (the real-store integration
// lands with issue #6).
type fakeStore struct {
	docs map[string]domain.Document // keyed by path

	upserts     []domain.Document
	statUpdates []string // doc IDs
	pathLookups []string // paths passed to GetByPath
	failWith    error    // returned by every method when set
}

func newFakeStore() *fakeStore {
	return &fakeStore{docs: map[string]domain.Document{}}
}

func (f *fakeStore) UpsertDocument(_ context.Context, doc domain.Document, _ []domain.Chunk) ([]int64, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}
	// Displace any other row holding the path (mirrors the sqlite store).
	for path, d := range f.docs {
		if d.ID == doc.ID && path != doc.Path {
			delete(f.docs, path)
		}
	}
	f.docs[doc.Path] = doc
	f.upserts = append(f.upserts, doc)
	return nil, nil
}

func (f *fakeStore) GetByPath(_ context.Context, path string) (domain.Document, bool, error) {
	if f.failWith != nil {
		return domain.Document{}, false, f.failWith
	}
	f.pathLookups = append(f.pathLookups, path)
	doc, ok := f.docs[path]
	return doc, ok, nil
}

func (f *fakeStore) GetByContentHash(_ context.Context, hash string) ([]domain.Document, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}
	var docs []domain.Document
	for _, d := range f.docs {
		if d.ContentHash == hash {
			docs = append(docs, d)
		}
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].ID < docs[j].ID })
	return docs, nil
}

func (f *fakeStore) UpdateDocumentStat(_ context.Context, docID string, size int64, mtime time.Time) error {
	if f.failWith != nil {
		return f.failWith
	}
	for path, d := range f.docs {
		if d.ID == docID {
			d.Size, d.MTime = size, mtime
			f.docs[path] = d
			f.statUpdates = append(f.statUpdates, docID)
			return nil
		}
	}
	return errors.New("no such document")
}

func (f *fakeStore) DeleteDocument(_ context.Context, docID string) error {
	if f.failWith != nil {
		return f.failWith
	}
	for path, d := range f.docs {
		if d.ID == docID {
			delete(f.docs, path)
		}
	}
	return nil
}

var _ domain.DocumentStore = (*fakeStore)(nil)

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

var docIDRe = regexp.MustCompile(`^d_[0-9a-f]{16}$`)

func TestScanNewFile(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.md"), "hello")
	store := newFakeStore()

	res := scan(t, store, Options{Include: []string{dir}})

	if res.Discovered != 1 || res.Unchanged != 0 || len(res.PathErrors) != 0 {
		t.Fatalf("Result = %+v, want 1 discovered", res)
	}
	if len(store.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(store.upserts))
	}
	doc := store.upserts[0]
	if doc.State != domain.DocStateDiscovered {
		t.Errorf("State = %q, want discovered", doc.State)
	}
	if !docIDRe.MatchString(doc.ID) {
		t.Errorf("ID = %q, want match %v", doc.ID, docIDRe)
	}
	if doc.Path != filepath.Join(dir, "a.md") || doc.Size != 5 {
		t.Errorf("doc = %+v", doc)
	}
	if doc.StageVersions != nil {
		t.Errorf("StageVersions = %v, want nil on discovery", doc.StageVersions)
	}
}

func TestScanUnchangedNoWrites(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.md"), "hello")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scan(t, store, opts)
	res := scan(t, store, opts)

	if res.Unchanged != 1 || res.Discovered != 0 {
		t.Errorf("rescan Result = %+v, want 1 unchanged", res)
	}
	if len(store.upserts) != 1 || len(store.statUpdates) != 0 {
		t.Errorf("writes after rescan: upserts=%d statUpdates=%d, want 1/0",
			len(store.upserts), len(store.statUpdates))
	}
}

func TestScanTouchedSameContent(t *testing.T) {
	dir := t.TempDir()
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

	if res.Unchanged != 1 || res.Discovered != 0 {
		t.Errorf("Result = %+v, want 1 unchanged", res)
	}
	if len(store.upserts) != 1 {
		t.Errorf("upserts = %d, want 1 (no re-upsert on touch)", len(store.upserts))
	}
	if len(store.statUpdates) != 1 {
		t.Errorf("statUpdates = %d, want 1", len(store.statUpdates))
	}
}

func TestScanEditedKeepsID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	write(t, path, "hello")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scan(t, store, opts)
	firstID, firstHash := store.upserts[0].ID, store.upserts[0].ContentHash
	write(t, path, "hello, edited")
	res := scan(t, store, opts)

	if res.Discovered != 1 || res.Renamed != 0 {
		t.Errorf("Result = %+v, want 1 discovered", res)
	}
	if len(store.upserts) != 2 {
		t.Fatalf("upserts = %d, want 2", len(store.upserts))
	}
	second := store.upserts[1]
	if second.ID != firstID {
		t.Errorf("edit minted new id %q, want %q kept", second.ID, firstID)
	}
	if second.ContentHash == firstHash {
		t.Error("edit kept the old content hash")
	}
	if second.State != domain.DocStateDiscovered {
		t.Errorf("State = %q, want discovered", second.State)
	}
}

func TestScanExcludedDirPruned(t *testing.T) {
	dir := t.TempDir()
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

func TestScanExcludedRootScansNothing(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.md"), "hello")
	store := newFakeStore()

	res := scan(t, store, Options{
		Include:  []string{dir},
		Excluded: func(p string) bool { return p == dir || strings.HasPrefix(p, dir+string(os.PathSeparator)) },
	})

	if res.Discovered != 0 || len(store.pathLookups) != 0 {
		t.Errorf("excluded root scanned: %+v, lookups %v", res, store.pathLookups)
	}
}

func TestScanExcludedFile(t *testing.T) {
	dir := t.TempDir()
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
	dir := t.TempDir()
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
	dir := t.TempDir()
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
	dir := t.TempDir()
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
	dir := t.TempDir()
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
	dir := t.TempDir()
	write(t, filepath.Join(dir, "sub", "a.md"), "hello")
	store := newFakeStore()

	res := scan(t, store, Options{Include: []string{dir, filepath.Join(dir, "sub"), dir}})

	if res.Discovered != 1 || len(store.upserts) != 1 {
		t.Errorf("Result = %+v with %d upserts, want file visited once", res, len(store.upserts))
	}
}

func TestScanRenameKeepsID(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.md")
	write(t, old, "stable content")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scan(t, store, opts)
	id := store.upserts[0].ID
	if err := os.Rename(old, filepath.Join(dir, "new.md")); err != nil {
		t.Fatal(err)
	}
	res := scan(t, store, opts)

	if res.Discovered != 1 || res.Renamed != 1 {
		t.Errorf("Result = %+v, want 1 discovered / 1 renamed", res)
	}
	moved := store.upserts[len(store.upserts)-1]
	if moved.ID != id {
		t.Errorf("rename minted new id %q, want %q kept", moved.ID, id)
	}
	if moved.Path != filepath.Join(dir, "new.md") {
		t.Errorf("Path = %q", moved.Path)
	}
}

func TestScanCopyMintsNewID(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.md"), "same content")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scan(t, store, opts)
	// Copy: original still on disk → hash match must NOT merge.
	write(t, filepath.Join(dir, "b.md"), "same content")
	res := scan(t, store, opts)

	if res.Renamed != 0 {
		t.Errorf("copy detected as rename: %+v", res)
	}
	ids := map[string]bool{}
	for _, d := range store.docs {
		ids[d.ID] = true
	}
	if len(ids) != 2 {
		t.Errorf("ids = %v, want two distinct ids for duplicate content", ids)
	}
}

func TestScanAmbiguousRenameMintsNewID(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	// Two rows share a hash (copy), then both files vanish.
	write(t, filepath.Join(dir, "a.md"), "same content")
	scan(t, store, opts)
	write(t, filepath.Join(dir, "b.md"), "same content")
	scan(t, store, opts)
	var oldIDs []string
	for _, d := range store.docs {
		oldIDs = append(oldIDs, d.ID)
	}
	for _, name := range []string{"a.md", "b.md"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(dir, "c.md"), "same content")
	res := scan(t, store, opts)

	if res.Renamed != 0 {
		t.Errorf("ambiguous rename resolved: %+v", res)
	}
	final := store.upserts[len(store.upserts)-1]
	if slices.Contains(oldIDs, final.ID) {
		t.Errorf("ambiguous candidate id %q reused; want fresh id", final.ID)
	}
}

func TestScanDatalessSkipped(t *testing.T) {
	dir := t.TempDir()
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
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.md"), "hello")
	store := newFakeStore()
	store.failWith = errors.New("disk full")

	_, err := New(store, Options{Include: []string{dir}}).Scan(t.Context())
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("Scan = %v, want fatal store error", err)
	}
}

func TestScanContextCancelled(t *testing.T) {
	dir := t.TempDir()
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

package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
)

// fakeStore is an in-memory DocumentStore that records calls, keeping the
// unit tests fast and their assertions precise (which batches, in what
// order). Not isolation: reconcile_integration_test.go links the real
// sqlite store into this same test binary, so the package's tests are
// cgo-built regardless.
type fakeStore struct {
	docs    map[string]domain.Document // keyed by path
	content map[string]bool            // content rows, keyed by hash

	batches       [][]domain.Document // every UpsertDocuments call
	upserts       []domain.Document   // the batches, flattened
	pathLookups   []string            // paths passed to GetByPath
	prefixDeletes []string            // paths passed to DeleteByPathPrefix
	pathDeletes   [][]string          // every DeleteByPaths call, as batched
	knownMounts   []string            // the ScanState half
	// mountsRemembered is whether anything has ever been written. False is
	// the fresh-database state, in which the deletion pass declines
	// everything for one pass rather than deleting with no volume evidence.
	mountsRemembered bool
	// knownMountsErr fails only the memory read — the seam for a corrupt
	// meta value, which must decline rather than fail the scan.
	knownMountsErr error
	// listPathsErr fails the catalog enumeration — the seam for a store
	// failure inside the reconcile, which must decline rather than fail Scan.
	listPathsErr error
	ops          []string // write calls in order: "upsert", "delete", "delete_paths"
	failWith     error    // returned by every method when set

	// onGetByPath, when set, runs at the top of every lookup — the seam for
	// injecting mid-scan cancellation or filesystem races at the exact point
	// between the walk's stat and the read.
	onGetByPath func(path string)
	// failUpsert, when set, is consulted with the 1-based UpsertDocuments
	// call number; a non-nil return fails that flush before anything is
	// recorded — the seam for testing that counts track committed batches.
	failUpsert func(call int) error
}

// newFakeStore starts in the steady state: mounts have been observed at least
// once, which is true of every daemon past its first scan. The first-run
// state (nothing ever written, so no volume evidence at all) is its own test.
func newFakeStore() *fakeStore {
	return &fakeStore{
		docs:             map[string]domain.Document{},
		content:          map[string]bool{},
		mountsRemembered: true,
	}
}

func (f *fakeStore) GetByPath(_ context.Context, path string) (domain.Document, bool, error) {
	if f.onGetByPath != nil {
		f.onGetByPath(path)
	}
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
	if f.failUpsert != nil {
		if err := f.failUpsert(len(f.batches) + 1); err != nil {
			return err
		}
	}
	f.batches = append(f.batches, slices.Clone(docs))
	f.ops = append(f.ops, "upsert")
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
	f.ops = append(f.ops, "delete")
	var removed int
	for path := range f.docs {
		if path == dir || strings.HasPrefix(path, dir+string(filepath.Separator)) {
			delete(f.docs, path)
			removed++
		}
	}
	return removed, nil
}

func (f *fakeStore) DeleteByPaths(_ context.Context, paths []string) (int, error) {
	if f.failWith != nil {
		return 0, f.failWith
	}
	if len(paths) == 0 {
		return 0, nil
	}
	f.pathDeletes = append(f.pathDeletes, slices.Clone(paths))
	f.ops = append(f.ops, "delete_paths")
	var removed int
	for _, path := range paths {
		if _, ok := f.docs[path]; ok {
			delete(f.docs, path)
			removed++
		}
	}
	return removed, nil
}

// ListPaths pages the catalog in path order with an exclusive cursor, the
// contract the real store's index range scan gives.
func (f *fakeStore) ListPaths(_ context.Context, after string, limit int) ([]string, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}
	if f.listPathsErr != nil {
		return nil, f.listPathsErr
	}
	paths := slices.Sorted(maps.Keys(f.docs))
	var out []string
	for _, path := range paths {
		if path > after {
			out = append(out, path)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) KnownMounts(context.Context) ([]string, bool, error) {
	if f.failWith != nil {
		return nil, false, f.failWith
	}
	if f.knownMountsErr != nil {
		return nil, false, f.knownMountsErr
	}
	return slices.Clone(f.knownMounts), f.mountsRemembered, nil
}

func (f *fakeStore) SetKnownMounts(_ context.Context, mounts []string) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.knownMounts = slices.Clone(mounts)
	f.mountsRemembered = true
	return nil
}

var (
	_ domain.DocumentStore = (*fakeStore)(nil)
	_ domain.ScanState     = (*fakeStore)(nil)
)

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

// newScanner builds a Scanner with a fixed mount table. Tests must not read
// the machine's real one: the deletion pass declines everything beneath a
// remembered-but-absent mount, so a scan's behaviour would otherwise depend
// on what happens to be plugged in. "/" is what every temp-dir root sits
// under, and the volume tests override this with their own table.
func newScanner(store *fakeStore, opts Options) *Scanner {
	sc := New(store, store, opts)
	sc.mounts = func() ([]string, bool, error) { return []string{"/"}, true, nil }
	return sc
}

func scan(t *testing.T, store *fakeStore, opts Options) Result {
	t.Helper()
	res, err := newScanner(store, opts).Scan(t.Context())
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

// upsertsFor returns every upsert written for path, in order — the way to
// assert steady state ("no second write") for one file among many.
func upsertsFor(store *fakeStore, path string) []domain.Document {
	var out []domain.Document
	for _, d := range store.upserts {
		if d.Path == path {
			out = append(out, d)
		}
	}
	return out
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
	// An unreadable directory is a walkErr: the file inside was never even
	// statted, so there is nothing to persist an unread row for.
	if res.Unread != 0 || len(store.docs) != 1 {
		t.Errorf("Unread = %d, docs = %v, want the walk error only", res.Unread, catalogPaths(store))
	}
}

// An unreadable file — stat succeeds, open fails — persists a denied row, so
// `bsearch status` reports the TCC denial in steady state, not just in the
// scan that met it.
func TestScanUnreadableFilePersistsDeniedRow(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not apply")
	}
	dir := tmpDir(t)
	path := filepath.Join(dir, "secret.md")
	write(t, path, "cannot read")
	write(t, filepath.Join(dir, "open.md"), "readable")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	store := newFakeStore()

	res := scan(t, store, Options{Include: []string{dir}})

	if res.Unread != 1 || res.Discovered != 1 {
		t.Fatalf("Result = %+v, want 1 unread / 1 discovered", res)
	}
	if len(res.PathErrors) != 1 || res.PathErrors[0].Path != path {
		t.Fatalf("PathErrors = %+v, want one for %s", res.PathErrors, path)
	}
	doc, ok := store.docs[path]
	if !ok {
		t.Fatalf("no unread row persisted; docs = %v", catalogPaths(store))
	}
	if doc.UnreadReason != domain.UnreadDenied || doc.ContentHash != "" {
		t.Errorf("doc = %+v, want reason denied and no hash", doc)
	}
	if doc.Size != int64(len("cannot read")) {
		t.Errorf("Size = %d, want the stat size", doc.Size)
	}
	// Bytes never obtained → no content row for this path.
	if len(store.content) != 1 || !store.content[hashOf("readable")] {
		t.Errorf("content rows = %v, want only the readable sibling's", store.content)
	}
}

// Steady state: the denial is counted every scan, but recorded only once —
// the second scan sees the same reason, size and mtime and writes nothing.
func TestScanUnreadableFileSteadyState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not apply")
	}
	dir := tmpDir(t)
	path := filepath.Join(dir, "secret.md")
	write(t, path, "cannot read")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scan(t, store, opts)
	res := scan(t, store, opts)

	if res.Unread != 1 {
		t.Errorf("rescan Unread = %d, want the denial counted again", res.Unread)
	}
	if got := upsertsFor(store, path); len(got) != 1 {
		t.Errorf("upserts for the denied path = %d, want 1 (no steady-state rewrite)", len(got))
	}
}

// A file that became unreadable after its bytes were obtained keeps its row
// and its hash: unread_reason is for bytes never obtained (ADR 0015), and
// de-referencing content a re-grant would only re-embed helps no one.
func TestScanUnreadableFileKeepsExistingHash(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not apply")
	}
	dir := tmpDir(t)
	path := filepath.Join(dir, "a.md")
	write(t, path, "hello")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scan(t, store, opts)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	// Bump mtime so the cheap check cannot answer and the read is attempted.
	newTime := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, newTime, newTime); err != nil {
		t.Fatal(err)
	}
	res := scan(t, store, opts)

	if res.Unread != 0 {
		t.Errorf("Unread = %d, want 0: the bytes were obtained before the denial", res.Unread)
	}
	if len(res.PathErrors) != 1 || res.PathErrors[0].Path != path {
		t.Errorf("PathErrors = %+v, want the read failure reported", res.PathErrors)
	}
	doc := store.docs[path]
	if doc.ContentHash != hashOf("hello") || doc.UnreadReason != "" {
		t.Errorf("doc = %+v, want the hash kept and no reason", doc)
	}
	if got := upsertsFor(store, path); len(got) != 1 {
		t.Errorf("upserts = %d, want 1 (no unread rewrite over a hashed row)", len(got))
	}
}

// unread→readable: an unread row is re-tried every scan (no cheap check
// without a hash), so granting access clears the denial without the file
// having to change.
func TestScanDeniedFileBecomesReadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not apply")
	}
	dir := tmpDir(t)
	path := filepath.Join(dir, "secret.md")
	write(t, path, "now visible")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scan(t, store, opts)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	res := scan(t, store, opts)

	if res.Unread != 0 || res.Discovered != 1 || len(res.PathErrors) != 0 {
		t.Fatalf("Result = %+v, want the denial cleared and the file discovered", res)
	}
	doc := store.docs[path]
	if doc.ContentHash != hashOf("now visible") || doc.UnreadReason != "" {
		t.Errorf("doc = %+v, want hash set and reason cleared", doc)
	}
	if !store.content[hashOf("now visible")] {
		t.Error("no content row created once the bytes were obtained")
	}
}

// Result arithmetic: denied and io_error land in Unread; dataless stays in
// Dataless only — the two must never double-count a path.
func TestScanUnreadCountsExcludeDataless(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not apply")
	}
	dir := tmpDir(t)
	denied := filepath.Join(dir, "denied.md")
	write(t, denied, "no access")
	write(t, filepath.Join(dir, "cloud.md"), "placeholder")
	write(t, filepath.Join(dir, "local.md"), "on disk")
	if err := os.Chmod(denied, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o644) })
	store := newFakeStore()

	s := newScanner(store, Options{Include: []string{dir}})
	s.dataless = func(info os.FileInfo) bool { return info.Name() == "cloud.md" }
	res, err := s.Scan(t.Context())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if res.Unread != 1 || res.Dataless != 1 || res.Discovered != 1 {
		t.Errorf("Result = %+v, want 1 unread / 1 dataless / 1 discovered", res)
	}
}

// A scan larger than one batch lands in multiple transactions, none over
// flushEvery, with nothing dropped between flushes.
func TestScanFlushesInBatches(t *testing.T) {
	dir := tmpDir(t)
	const n = flushEvery + 44
	for i := range n {
		write(t, filepath.Join(dir, fmt.Sprintf("f%03d.md", i)), fmt.Sprintf("content %d", i))
	}
	store := newFakeStore()

	res := scan(t, store, Options{Include: []string{dir}})

	if res.Discovered != n {
		t.Fatalf("Discovered = %d, want %d", res.Discovered, n)
	}
	if len(store.batches) < 2 {
		t.Errorf("batches = %d, want the scan split across transactions", len(store.batches))
	}
	var total int
	for i, b := range store.batches {
		if len(b) > flushEvery {
			t.Errorf("batch %d holds %d docs, want at most %d", i, len(b), flushEvery)
		}
		total += len(b)
	}
	if total != n || len(store.docs) != n {
		t.Errorf("rows: %d upserted, %d stored, want %d — a flush lost documents", total, len(store.docs), n)
	}
}

// The committed-state property under cancellation: counts coupled to
// buffered rows must not survive the buffer being abandoned. The watcher
// records a Result before it looks at the error, so an overcount here
// becomes a cumulative lie in `bsearch status`.
func TestScanCountsMatchCommittedRowsOnCancel(t *testing.T) {
	dir := tmpDir(t)
	for i := range 5 {
		write(t, filepath.Join(dir, fmt.Sprintf("f%d.md", i)), fmt.Sprintf("content %d", i))
	}
	store := newFakeStore()
	ctx, cancel := context.WithCancel(t.Context())
	var lookups int
	store.onGetByPath = func(string) {
		if lookups++; lookups == 3 {
			cancel()
		}
	}

	res, err := newScanner(store, Options{Include: []string{dir}}).Scan(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Scan = %v, want context.Canceled", err)
	}
	if committed := len(store.upserts); res.Discovered != committed {
		t.Errorf("Discovered = %d but %d rows committed — counts must describe committed state", res.Discovered, committed)
	}
	if res.Discovered != 0 {
		t.Errorf("Discovered = %d, want 0: nothing was flushed before the cancel", res.Discovered)
	}
}

// The same property when a flush itself fails: batches committed before the
// failure stay counted, the batch that never landed does not.
func TestScanCountsMatchCommittedRowsOnFlushFailure(t *testing.T) {
	dir := tmpDir(t)
	const n = flushEvery + 10
	for i := range n {
		write(t, filepath.Join(dir, fmt.Sprintf("f%03d.md", i)), fmt.Sprintf("content %d", i))
	}
	store := newFakeStore()
	store.failUpsert = func(call int) error {
		if call == 2 {
			return errors.New("disk full")
		}
		return nil
	}

	res, err := newScanner(store, Options{Include: []string{dir}}).Scan(t.Context())

	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("Scan = %v, want the second flush's failure", err)
	}
	if committed := len(store.upserts); res.Discovered != committed || committed != flushEvery {
		t.Errorf("Discovered = %d, committed = %d, want both %d — the durable batch counted, the failed one not",
			res.Discovered, committed, flushEvery)
	}
}

// A file deleted between the walk's stat and the open is the same race the
// walk filters at d.Info(): no PathError, and never an io_error row — that
// would be a phantom documents row for a path no walk can ever purge.
func TestScanDeletedBetweenStatAndOpenLeavesNoTrace(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, "a.md")
	write(t, path, "here and gone")
	store := newFakeStore()
	store.onGetByPath = func(p string) {
		if p == path {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
	}

	res := scan(t, store, Options{Include: []string{dir}})

	if res.Unread != 0 || res.Discovered != 0 || len(res.PathErrors) != 0 {
		t.Errorf("Result = %+v, want the race swallowed", res)
	}
	if len(store.docs) != 0 || len(store.upserts) != 0 {
		t.Errorf("docs = %v, want no row for a deleted path", catalogPaths(store))
	}
}

// io_error is the one unread reason whose retry is not free: the last
// attempt died mid-read after hashing every byte up to the failure point.
// While the stat is unchanged the row is counted without re-reading; the
// stat moving is what re-arms the attempt.
func TestScanIOErrorRowRetriedOnlyWhenStatMoves(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, "big.md")
	write(t, path, "readable now")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	store.docs[path] = domain.Document{
		Path: path, UnreadReason: domain.UnreadIOError,
		Size: info.Size(), MTime: info.ModTime(),
	}
	opts := Options{Include: []string{dir}}

	res := scan(t, store, opts)

	// Steady stat → counted, never opened: no PathError (the file was not
	// touched), no write, and the row untouched even though a read would
	// now succeed.
	if res.Unread != 1 || len(res.PathErrors) != 0 || len(store.upserts) != 0 {
		t.Fatalf("Result = %+v with %d upserts, want the row counted without a read", res, len(store.upserts))
	}
	if store.docs[path].UnreadReason != domain.UnreadIOError {
		t.Errorf("doc = %+v, want the io_error row untouched", store.docs[path])
	}

	// The stat moving re-arms the read, which succeeds and clears the row.
	newTime := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, newTime, newTime); err != nil {
		t.Fatal(err)
	}
	res = scan(t, store, opts)

	if res.Unread != 0 || res.Discovered != 1 {
		t.Fatalf("Result = %+v, want the retry to succeed once the stat moved", res)
	}
	doc := store.docs[path]
	if doc.ContentHash != hashOf("readable now") || doc.UnreadReason != "" {
		t.Errorf("doc = %+v, want hash set and reason cleared", doc)
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

func TestScanDatalessPersistsUnreadRow(t *testing.T) {
	dir := tmpDir(t)
	cloud := filepath.Join(dir, "cloud.md")
	write(t, cloud, "placeholder")
	write(t, filepath.Join(dir, "local.md"), "on disk")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	s := newScanner(store, opts)
	s.dataless = func(info os.FileInfo) bool { return info.Name() == "cloud.md" }
	res, err := s.Scan(t.Context())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if res.Dataless != 1 || res.Discovered != 1 {
		t.Errorf("Result = %+v, want 1 dataless / 1 discovered", res)
	}
	// Dataless is not Unread: the split keeps "broken" and "working as
	// designed" from reporting as one number.
	if res.Unread != 0 {
		t.Errorf("Unread = %d, want 0 for a placeholder", res.Unread)
	}
	doc, ok := store.docs[cloud]
	if !ok {
		t.Fatalf("no unread row for the placeholder; docs = %v", catalogPaths(store))
	}
	if doc.UnreadReason != domain.UnreadDataless || doc.ContentHash != "" {
		t.Errorf("doc = %+v, want reason dataless and no hash", doc)
	}
	// The file was never opened: no hash was ever computed for its bytes.
	if len(store.content) != 1 || !store.content[hashOf("on disk")] {
		t.Errorf("content rows = %v, want only the local file's", store.content)
	}

	// Steady state: rescan counts it again, writes nothing new.
	res, err = s.Scan(t.Context())
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if res.Dataless != 1 {
		t.Errorf("rescan Dataless = %d, want 1", res.Dataless)
	}
	if got := upsertsFor(store, cloud); len(got) != 1 {
		t.Errorf("upserts for the placeholder = %d, want 1 (no steady-state rewrite)", len(got))
	}
}

// A file evicted after its bytes were indexed keeps its hash: the content is
// still what the file holds, only the local copy is gone.
func TestScanDatalessEvictedAfterIndexingKeepsHash(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, "a.md")
	write(t, path, "hello")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scan(t, store, opts) // indexed while local

	s := newScanner(store, opts)
	s.dataless = func(info os.FileInfo) bool { return true } // now evicted
	res, err := s.Scan(t.Context())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if res.Dataless != 1 {
		t.Errorf("Result = %+v, want 1 dataless", res)
	}
	doc := store.docs[path]
	if doc.ContentHash != hashOf("hello") || doc.UnreadReason != "" {
		t.Errorf("doc = %+v, want the hash kept over the eviction", doc)
	}
	if got := upsertsFor(store, path); len(got) != 1 {
		t.Errorf("upserts = %d, want 1 (no rewrite over a hashed row)", len(got))
	}
}

// The keep-hash rule holds only while the stat holds: a placeholder whose
// size or mtime moved was edited remotely after eviction, and keeping the
// hash would serve the old version's chunks indefinitely with no record
// that the file changed.
func TestScanDatalessEvictedThenRemoteEditDropsHash(t *testing.T) {
	dir := tmpDir(t)
	path := filepath.Join(dir, "a.md")
	write(t, path, "hello")
	store := newFakeStore()
	opts := Options{Include: []string{dir}}

	scan(t, store, opts) // indexed while local

	// Evicted, then edited on another device: the placeholder's stat moves
	// while the file stays dataless.
	write(t, path, "hello, edited remotely")
	s := newScanner(store, opts)
	s.dataless = func(info os.FileInfo) bool { return true }
	res, err := s.Scan(t.Context())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if res.Dataless != 1 {
		t.Errorf("Result = %+v, want 1 dataless", res)
	}
	doc := store.docs[path]
	if doc.UnreadReason != domain.UnreadDataless || doc.ContentHash != "" {
		t.Errorf("doc = %+v, want the hash dropped for a dataless row", doc)
	}
	// The old content row is orphaned for the sweep, never deleted here.
	if !store.content[hashOf("hello")] {
		t.Error("old content row deleted by discovery; the sweep owns that")
	}
}

func TestScanStoreFailureIsFatal(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "a.md"), "hello")
	store := newFakeStore()
	store.failWith = errors.New("disk full")

	_, err := newScanner(store, Options{Include: []string{dir}}).Scan(t.Context())
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("Scan = %v, want fatal store error", err)
	}
}

func TestScanContextCancelled(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "a.md"), "hello")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := newScanner(newFakeStore(), Options{Include: []string{dir}}).Scan(ctx)
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

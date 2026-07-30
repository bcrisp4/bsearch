package discovery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
	"github.com/bcrisp4/bsearch/internal/pathutil"
)

// seedDoc plants a catalog row for a path directly, bypassing the walk. It is
// how a test says "this was indexed earlier" — including for paths that no
// longer exist, which is the whole subject here and which no scan can create.
func seedDoc(store *fakeStore, path string) {
	store.docs[path] = domain.Document{
		Path:        path,
		ContentHash: hashOf(path),
		Size:        1,
		MTime:       time.Unix(1700000000, 0),
	}
	store.content[hashOf(path)] = true
}

// withMounts fixes the scanner's view of the mount table.
func withMounts(sc *Scanner, mounts ...string) *Scanner {
	sc.mounts = func() ([]string, bool, error) { return mounts, true, nil }
	return sc
}

func scanWith(t *testing.T, sc *Scanner) Result {
	t.Helper()
	res, err := sc.Scan(t.Context())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return res
}

// paths returns the catalog's paths, sorted — what survived.
func remaining(store *fakeStore) []string {
	out := make([]string, 0, len(store.docs))
	for path := range store.docs {
		out = append(out, path)
	}
	slices.Sort(out)
	return out
}

// A file deleted while the daemon was not running is the first of ADR 0013's
// three gaps: no event ever named it, so only a catalog-side pass can notice.
func TestScanPurgesRowWhoseFileIsGone(t *testing.T) {
	dir := tmpDir(t)
	kept := filepath.Join(dir, "kept.md")
	write(t, kept, "still here")
	store := newFakeStore()
	gone := filepath.Join(dir, "gone.md")
	seedDoc(store, gone)

	res := scan(t, store, Options{Include: []string{dir}})

	if res.Deleted != 1 || res.Pruned != 0 || res.Unverified != 0 {
		t.Fatalf("Result = %+v, want 1 deleted", res)
	}
	if got := remaining(store); !slices.Equal(got, []string{kept}) {
		t.Errorf("catalog = %v, want only %s", got, kept)
	}
	// Never by prefix. A prefix delete is the right answer to an event, which
	// cannot enumerate what was under a path; the catalog already has the
	// enumeration, so a wrong answer here must cost one row and not a
	// subtree. This assertion is the guard against reintroducing collapse.
	if len(store.prefixDeletes) != 0 {
		t.Errorf("prefixDeletes = %v, want none — the catalog pass deletes per path", store.prefixDeletes)
	}
}

// The third gap: a subtree that vanished with no directory event of its own.
// The event path declines this deliberately (purge's parent-must-exist guard),
// so the walk is the only thing that closes it.
func TestScanPurgesVanishedSubtree(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "keep.md"), "x")
	sub := filepath.Join(dir, "project")
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		write(t, filepath.Join(sub, name), name)
	}
	store := newFakeStore()

	scan(t, store, Options{Include: []string{dir}})
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	store.pathDeletes = nil
	res := scan(t, store, Options{Include: []string{dir}})

	if res.Deleted != 3 {
		t.Fatalf("Result = %+v, want 3 deleted", res)
	}
	if got := remaining(store); !slices.Equal(got, []string{filepath.Join(dir, "keep.md")}) {
		t.Errorf("catalog = %v, want only keep.md", got)
	}
	if len(store.pathDeletes) != 1 || len(store.pathDeletes[0]) != 3 {
		t.Errorf("pathDeletes = %v, want the three rows in one batch", store.pathDeletes)
	}
}

// The blip that must never wipe a corpus. A subtree the walk could not read
// is declined wholesale, and — the part guard ordering buys — the decline
// costs no stat and adds no second round of path errors, while the rest of
// the corpus is reconciled normally.
func TestScanDeclinesUnderUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root defeats chmod 0")
	}
	dir := tmpDir(t)
	denied := filepath.Join(dir, "denied")
	write(t, filepath.Join(denied, "a.md"), "a")
	write(t, filepath.Join(denied, "b.md"), "b")
	open := filepath.Join(dir, "open")
	write(t, filepath.Join(open, "c.md"), "c")
	store := newFakeStore()

	scan(t, store, Options{Include: []string{dir}})
	if err := os.Remove(filepath.Join(open, "c.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(denied, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o755) })

	res := scan(t, store, Options{Include: []string{dir}})

	if res.Deleted != 1 || res.Unverified != 2 {
		t.Fatalf("Result = %+v, want 1 deleted (c.md) and 2 unverified (the denied pair)", res)
	}
	want := []string{filepath.Join(denied, "a.md"), filepath.Join(denied, "b.md")}
	if got := remaining(store); !slices.Equal(got, want) {
		t.Errorf("catalog = %v, want the denied pair intact", got)
	}
	// One error, from the walk. The reconcile recognised the subtree without
	// stat-ing into it, so it neither doubled the report nor paid the
	// syscalls — the reason the coverage guard runs before the stat.
	if len(res.PathErrors) != 1 || res.PathErrors[0].Path != denied {
		t.Errorf("PathErrors = %v, want exactly the walk's one report of %s", res.PathErrors, denied)
	}
}

// The case the mount table exists for: a volume mounted inside a root goes
// away. Every path beneath it reads as ENOENT, the walk sees no error at all,
// and nothing else in the design would decline.
func TestScanDeclinesBeneathAnUnmountedVolume(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "local.md"), "local")
	volume := filepath.Join(dir, "volume")
	store := newFakeStore()
	for _, name := range []string{"one.md", "two.md"} {
		seedDoc(store, filepath.Join(volume, name))
	}
	store.knownMounts = []string{"/", volume}

	// The volume's mount point is gone entirely — how macOS leaves /Volumes
	// after an eject.
	res := scanWith(t, withMounts(newScanner(store, Options{Include: []string{dir}}), "/"))

	if res.Deleted != 0 || res.Unverified != 2 {
		t.Fatalf("Result = %+v, want nothing deleted and 2 unverified", res)
	}
	if !slices.Equal(res.Unmounted, []string{volume}) {
		t.Errorf("Unmounted = %v, want %s named so status can say which drive", res.Unmounted, volume)
	}
	if len(remaining(store)) != 3 {
		t.Errorf("catalog = %v, want all three rows intact", remaining(store))
	}
	// Still remembered: the drive is unplugged, not retired, and forgetting
	// it here is what would let the next scan purge the corpus.
	if !slices.Contains(store.knownMounts, volume) {
		t.Errorf("knownMounts = %v, want %s kept while rows remain beneath it", store.knownMounts, volume)
	}
}

// The nastier shape of the same thing: the mount point survives the unmount
// as an ordinary empty directory, so the walk descends it cleanly and reports
// nothing wrong. Only the mount table can tell this from an emptied folder.
func TestScanDeclinesBeneathAnUnmountedVolumeWithSurvivingMountPoint(t *testing.T) {
	dir := tmpDir(t)
	volume := filepath.Join(dir, "volume")
	if err := os.MkdirAll(volume, 0o755); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	seedDoc(store, filepath.Join(volume, "one.md"))
	store.knownMounts = []string{"/", volume}

	res := scanWith(t, withMounts(newScanner(store, Options{Include: []string{dir}}), "/"))

	if res.Deleted != 0 || res.Unverified != 1 || len(res.PathErrors) != 0 {
		t.Fatalf("Result = %+v, want 1 unverified and no errors — the walk saw nothing wrong", res)
	}
}

// The counterpart: a directory genuinely emptied, on a volume that is still
// mounted, purges. Without this the mechanism would be indistinguishable from
// a blanket refusal to ever delete.
func TestScanPurgesEmptiedDirectoryOnAMountedVolume(t *testing.T) {
	dir := tmpDir(t)
	volume := filepath.Join(dir, "volume")
	if err := os.MkdirAll(volume, 0o755); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	seedDoc(store, filepath.Join(volume, "one.md"))
	store.knownMounts = []string{"/", volume}

	res := scanWith(t, withMounts(newScanner(store, Options{Include: []string{dir}}), "/", volume))

	if res.Deleted != 1 || res.Unverified != 0 {
		t.Fatalf("Result = %+v, want the row purged — the volume is mounted and the file is gone", res)
	}
}

// A root that is not there resolves to itself and fails the walk, so the pass
// has no evidence for anything beneath it. This is the unmounted-volume-as-a-
// root shape, and it declines without needing the mount table at all.
func TestScanDeclinesWhenRootIsMissing(t *testing.T) {
	parent := tmpDir(t)
	root := filepath.Join(parent, "root")
	write(t, filepath.Join(root, "a.md"), "a")
	store := newFakeStore()

	scan(t, store, Options{Include: []string{root}})
	if err := os.Rename(root, root+".away"); err != nil {
		t.Fatal(err)
	}

	res := scan(t, store, Options{Include: []string{root}})

	if res.Deleted != 0 || res.Pruned != 0 {
		t.Fatalf("Result = %+v, want nothing removed when the root itself is gone", res)
	}
	if len(remaining(store)) != 1 {
		t.Errorf("catalog = %v, want the row intact", remaining(store))
	}
}

// A file bsearch is no longer configured to index has vanished as far as the
// index is concerned, whether it left the corpus or the corpus left it.
func TestScanPrunesRowsOutOfScope(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "kept.md"), "kept")
	archive := filepath.Join(dir, "archive")
	write(t, filepath.Join(archive, "old.md"), "old")
	elsewhere := filepath.Join(tmpDir(t), "other.md")
	write(t, elsewhere, "other")
	store := newFakeStore()

	scan(t, store, Options{Include: []string{dir}})
	seedDoc(store, elsewhere) // indexed under a root since removed from config

	res := scan(t, store, Options{
		Include: []string{dir},
		// Prefix-matching, like config.ExcludeSet.Match: a rule naming a
		// directory excludes what is inside it. The walk only ever asks
		// about the directory, but the deletion pass asks about files.
		Excluded: func(p string) bool { return pathutil.Within(p, archive) },
	})

	// Both files still exist; neither is in scope any more.
	if res.Pruned != 2 || res.Deleted != 0 {
		t.Fatalf("Result = %+v, want 2 pruned and nothing deleted", res)
	}
	if got := remaining(store); !slices.Equal(got, []string{filepath.Join(dir, "kept.md")}) {
		t.Errorf("catalog = %v, want only the in-scope file", got)
	}
}

// Scope pruning is licensed by the config, so it is suspended whenever the
// config could not be read out fully. A root that fails to resolve is dropped
// from the root set, and rows under a symlinked root carry the *resolved*
// spelling — so they would read as out of scope and be pruned on a blip.
func TestScanSuspendsPruningWhenARootFailsToResolve(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root defeats chmod 0")
	}
	base := tmpDir(t)
	working := filepath.Join(base, "working")
	write(t, filepath.Join(working, "kept.md"), "kept")

	// A symlinked root — ~/notes → ~/Dropbox/notes, the setup canonicalRoot
	// exists for. The catalog stores what the walk visited, so its rows carry
	// the RESOLVED spelling while the root is configured as the link.
	vault := filepath.Join(base, "vault")
	target := filepath.Join(vault, "data")
	stranded := filepath.Join(target, "x.md")
	write(t, stranded, "x")
	link := filepath.Join(base, "notes")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	store := newFakeStore()
	scan(t, store, Options{Include: []string{working, link}})
	if !slices.Contains(remaining(store), stranded) {
		t.Fatalf("catalog = %v, want the symlinked root's file stored under its resolved path", remaining(store))
	}

	// Now EvalSymlinks cannot traverse to the target: EACCES, not ENOENT, so
	// the root is dropped rather than passed through. The PathError it leaves
	// is spelled with the LINK, which covers none of the rows — they are
	// spelled under the target. Nothing but the suspension saves them.
	if err := os.Chmod(vault, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(vault, 0o755) })

	res := scan(t, store, Options{Include: []string{working, link}})

	if res.Pruned != 0 {
		t.Fatalf("Result = %+v, want no pruning while a root would not resolve", res)
	}
	if res.Ignored != 1 {
		t.Errorf("Ignored = %d, want the stranded row counted rather than silently kept", res.Ignored)
	}
	if !slices.Contains(remaining(store), stranded) {
		t.Errorf("catalog = %v, want %s intact", remaining(store), stranded)
	}
}

// Deletions ride the same batching discipline as upserts: short transactions,
// and counts that describe rows that actually went.
func TestScanBatchesDeletions(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "keep.md"), "keep")
	store := newFakeStore()
	const phantoms = 300
	for i := range phantoms {
		seedDoc(store, filepath.Join(dir, "gone", "f"+string(rune('a'+i/26))+string(rune('a'+i%26))+".md"))
	}

	res := scan(t, store, Options{Include: []string{dir}})

	if res.Deleted != phantoms {
		t.Fatalf("Deleted = %d, want %d", res.Deleted, phantoms)
	}
	if len(store.pathDeletes) < 2 {
		t.Fatalf("pathDeletes = %d batches, want more than one for %d rows", len(store.pathDeletes), phantoms)
	}
	for i, batch := range store.pathDeletes {
		if len(batch) > flushEvery {
			t.Errorf("batch %d holds %d paths, want at most %d", i, len(batch), flushEvery)
		}
	}
}

// An unread row is still a row. Filtering NULL-hash rows out of the catalog
// walk would make denied and dataless files unpurgeable forever.
func TestScanPurgesUnreadRowWhoseFileIsGone(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "keep.md"), "keep")
	store := newFakeStore()
	gone := filepath.Join(dir, "was-denied.md")
	store.docs[gone] = domain.Document{
		Path: gone, UnreadReason: domain.UnreadDenied, Size: 1, MTime: time.Unix(1700000000, 0),
	}

	res := scan(t, store, Options{Include: []string{dir}})

	if res.Deleted != 1 {
		t.Fatalf("Result = %+v, want the unread row purged", res)
	}
}

// Reached answers "did the scan get to any files", which arms the Full Disk
// Access alarm. Deletions are not files reached, and folding them in would
// silence the alarm for a pass that only removed rows.
func TestScanDeletionsDoNotCountAsReached(t *testing.T) {
	dir := tmpDir(t)
	store := newFakeStore()
	seedDoc(store, filepath.Join(dir, "gone.md"))

	res := scan(t, store, Options{Include: []string{dir}})

	if res.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1", res.Deleted)
	}
	if res.Reached() != 0 {
		t.Errorf("Reached() = %d, want 0 — an empty directory reached no files", res.Reached())
	}
}

// The remembered set accumulates so it survives a restart with the drive
// detached, and self-prunes so a retired volume does not protect an empty
// prefix forever.
func TestScanRemembersMountsInScopeAndForgetsEmptyOnes(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "a.md"), "a")
	volume := filepath.Join(dir, "volume")
	store := newFakeStore()
	// Remembered from a previous run, no longer mounted, and nothing left
	// beneath it — the retired drive.
	store.knownMounts = []string{"/", volume}

	scanWith(t, withMounts(newScanner(store, Options{Include: []string{dir}}), "/", "/elsewhere"))

	if slices.Contains(store.knownMounts, volume) {
		t.Errorf("knownMounts = %v, want %s forgotten once no rows remain beneath it", store.knownMounts, volume)
	}
	if !slices.Contains(store.knownMounts, "/") {
		t.Errorf("knownMounts = %v, want / kept — it contains the root", store.knownMounts)
	}
	// Out of scope entirely: neither inside a root nor holding one. Asserted
	// against a mount that HAS rows beneath it, or forgetEmptyMounts would
	// drop it anyway and the assertion would pass with inScope hardwired to
	// true — a regression remembering every mount on the machine, so that any
	// unrelated eject becomes a corpus-wide decline-prefix, would ship green.
	if slices.Contains(store.knownMounts, "/elsewhere") {
		t.Errorf("knownMounts = %v, want unrelated mounts left out", store.knownMounts)
	}
}

// inScope on its own: a mount is remembered when it sits inside a root or
// holds one, and not otherwise. Pinned directly because the round-trip test
// above cannot distinguish "inScope rejected it" from "forgetEmptyMounts
// dropped it for having no rows".
func TestInScopeMatchesMountsInsideOrHoldingARoot(t *testing.T) {
	roots := []string{"/home/ben/notes"}
	for mount, want := range map[string]bool{
		"/home/ben/notes":      true,  // the root itself
		"/home/ben/notes/ext":  true,  // mounted inside it
		"/home/ben":            true,  // holds it
		"/":                    true,  // holds it
		"/home/ben/notesuffix": false, // component boundary, not a prefix
		"/home/ben/other":      false,
		"/Volumes/Unrelated":   false,
	} {
		if got := inScope(mount, roots); got != want {
			t.Errorf("inScope(%q) = %v, want %v", mount, got, want)
		}
	}
}

// A mount table that cannot be read is a decline, not a licence: without it
// an unmounted volume and a deleted subtree are indistinguishable.
func TestScanDeclinesEverythingWhenTheMountTableIsUnreadable(t *testing.T) {
	dir := tmpDir(t)
	store := newFakeStore()
	seedDoc(store, filepath.Join(dir, "gone.md"))

	sc := newScanner(store, Options{Include: []string{dir}})
	sc.mounts = func() ([]string, bool, error) { return nil, true, os.ErrPermission }
	res := scanWith(t, sc)

	if res.Deleted != 0 || res.Unverified != 1 {
		t.Fatalf("Result = %+v, want nothing deleted and the row counted unverified", res)
	}
	// Named as a decline reason, not as a PathError: a database or mount-table
	// failure is not a per-path filesystem problem, and status renders
	// PathErrors under "Unreadable paths", whose documented meaning is a
	// missing Full Disk Access grant.
	if len(res.PathErrors) != 0 {
		t.Errorf("PathErrors = %v, want none — this is not a per-path problem", res.PathErrors)
	}
	if len(res.DeclineReasons) != 1 {
		t.Errorf("DeclineReasons = %v, want the reason named once", res.DeclineReasons)
	}
}

// The corpus-wipe this design exists to prevent, in the shape that defeated
// every guard: a symlinked include root — canonicalRoot's own doc calls
// ~/notes → ~/Dropbox/notes "a common setup" — whose target is on a volume
// that gets unplugged.
//
// Nothing reports a problem. EvalSymlinks fails ENOENT, which canonicalRoot
// passes through as the *configured* spelling; WalkDir lstats the now-dangling
// link and the symlink guard skips it without error. So there is no walk
// error, no unresolved root, and no unmounted mount to recognise — while the
// catalog rows are spelled under the *resolved* path and read as out of scope.
// Before the root-verification fix this pruned the entire external corpus and
// reported it as a config change.
func TestScanDoesNotPruneWhenASymlinkedRootStopsResolving(t *testing.T) {
	base := tmpDir(t)
	volume := filepath.Join(base, "volume")
	notes := filepath.Join(volume, "notes")
	write(t, filepath.Join(notes, "a.md"), "alpha")
	link := filepath.Join(base, "link")
	if err := os.Symlink(notes, link); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()

	scan(t, store, Options{Include: []string{link}})
	stored := remaining(store)
	if len(stored) != 1 {
		t.Fatalf("catalog = %v, want the file stored under its resolved path", stored)
	}

	if err := os.RemoveAll(volume); err != nil {
		t.Fatal(err)
	}
	res := scan(t, store, Options{Include: []string{link}})

	if res.Pruned != 0 || res.Deleted != 0 {
		t.Fatalf("Result = %+v, want nothing removed — the root did not resolve", res)
	}
	if got := remaining(store); !slices.Equal(got, stored) {
		t.Errorf("catalog = %v, want %v intact", got, stored)
	}
	if res.Ignored != 1 {
		t.Errorf("Ignored = %d, want the decline counted rather than silent", res.Ignored)
	}
}

// An include root covered by the exclude rules is a deliberate configuration,
// not a failure — so it must not become a decline-prefix. Folding every
// PathError into coverage made its rows permanently unprunable and printed a
// standing "deletions are not being noticed" line for a state the user chose.
func TestScanPrunesRowsUnderAnExcludedRoot(t *testing.T) {
	base := tmpDir(t)
	keep := filepath.Join(base, "keep")
	write(t, filepath.Join(keep, "a.md"), "a")
	dropped := filepath.Join(base, "dropped")
	write(t, filepath.Join(dropped, "b.md"), "b")
	store := newFakeStore()

	scan(t, store, Options{Include: []string{keep, dropped}})
	res := scan(t, store, Options{
		Include:  []string{keep, dropped},
		Excluded: func(p string) bool { return pathutil.Within(p, dropped) },
	})

	if res.Pruned != 1 || res.Unverified != 0 {
		t.Fatalf("Result = %+v, want the excluded root's row pruned, not declined", res)
	}
	if got := remaining(store); !slices.Equal(got, []string{filepath.Join(keep, "a.md")}) {
		t.Errorf("catalog = %v, want only the kept root's file", got)
	}
}

// A single unreadable *file* is not an opaque subtree. Its own path used to
// land in the coverage set, so the row was skipped without a stat — forever,
// since a denied file is retried every scan — giving every daemon without Full
// Disk Access a permanent "not reconciled" line and a Warn every cycle.
func TestScanReconcilesAlongsideAnUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root defeats chmod 0")
	}
	dir := tmpDir(t)
	locked := filepath.Join(dir, "locked.md")
	write(t, locked, "locked")
	write(t, filepath.Join(dir, "gone.md"), "gone")
	store := newFakeStore()

	scan(t, store, Options{Include: []string{dir}})
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	if err := os.Remove(filepath.Join(dir, "gone.md")); err != nil {
		t.Fatal(err)
	}

	res := scan(t, store, Options{Include: []string{dir}})

	if res.Unverified != 0 {
		t.Errorf("Unverified = %d, want 0 — the file was stat-able, only unreadable", res.Unverified)
	}
	if res.Deleted != 1 {
		t.Errorf("Deleted = %d, want the genuinely deleted file purged", res.Deleted)
	}
	if !slices.Contains(remaining(store), locked) {
		t.Errorf("catalog = %v, want the unreadable file's row kept", remaining(store))
	}
}

// "Cannot tell" is a decline, and a decline that no counter records is a
// silent skip. A row whose parent is a regular file is the portable fixture:
// the stat returns ENOTDIR, and nothing about the parent puts it in coverage,
// so the row reaches the stat guard rather than being declined earlier.
func TestScanCountsRowsItCouldNotStat(t *testing.T) {
	dir := tmpDir(t)
	file := filepath.Join(dir, "notes.md")
	write(t, file, "a real file")
	store := newFakeStore()
	for _, name := range []string{"one.md", "two.md", "three.md"} {
		seedDoc(store, filepath.Join(file, name))
	}

	res := scan(t, store, Options{Include: []string{dir}})

	if res.Deleted != 0 {
		t.Fatalf("Deleted = %d, want nothing deleted on an unreadable stat", res.Deleted)
	}
	if res.Unverified != 3 {
		t.Errorf("Unverified = %d, want all three declines counted", res.Unverified)
	}
	// One report for the shared directory, not one per row: a large subtree
	// failing this way must not flood the status sample or grow with the corpus.
	if len(res.PathErrors) != 1 {
		t.Errorf("PathErrors = %v, want one report for the shared directory", res.PathErrors)
	}
	// And the walk itself was fine, so the Full Disk Access alarm must stay
	// disarmed — it keys on WalkErrors, not on the whole slice.
	if res.WalkErrors != 0 {
		t.Errorf("WalkErrors = %d, want 0 — the walk hit no trouble", res.WalkErrors)
	}
}

// A symlink the walk skipped is a prefix it never looked through, so rows
// beneath it are declined without a stat — including a symlink cycle, which
// would otherwise reach the stat guard and report ELOOP per directory.
func TestScanDeclinesBeneathASkippedSymlink(t *testing.T) {
	dir := tmpDir(t)
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	for _, name := range []string{"one.md", "two.md"} {
		seedDoc(store, filepath.Join(a, name))
	}

	res := scan(t, store, Options{Include: []string{dir}})

	if res.Deleted != 0 || res.Unverified != 2 {
		t.Fatalf("Result = %+v, want both rows declined without a stat", res)
	}
	if len(res.PathErrors) != 0 {
		t.Errorf("PathErrors = %v, want none — the walk recognised the link rather than erroring", res.PathErrors)
	}
}

// A file replaced in place by a directory or a symlink still stats, but the
// walk will never index it again, so its old chunks and vectors would answer
// searches forever. The row is gone in every sense the index cares about.
func TestScanPurgesRowsWhoseFileWasReplaced(t *testing.T) {
	dir := tmpDir(t)
	asDir := filepath.Join(dir, "todir.md")
	asLink := filepath.Join(dir, "tolink.md")
	target := filepath.Join(dir, "target.md")
	write(t, asDir, "x")
	write(t, asLink, "y")
	write(t, target, "z")
	store := newFakeStore()

	scan(t, store, Options{Include: []string{dir}})
	if err := os.Remove(asDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(asLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, asLink); err != nil {
		t.Fatal(err)
	}

	res := scan(t, store, Options{Include: []string{dir}})

	if res.Deleted != 2 {
		t.Fatalf("Result = %+v, want both replaced rows purged", res)
	}
	if got := remaining(store); !slices.Equal(got, []string{target}) {
		t.Errorf("catalog = %v, want only the real file", got)
	}
}

// With no volume evidence ever recorded, a deletion would be made blind. The
// first pass learns and declines the deletion; the second acts.
func TestScanDeclinesDeletionsUntilMountsHaveBeenObservedOnce(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "keep.md"), "keep")
	store := newFakeStore()
	store.mountsRemembered = false
	seedDoc(store, filepath.Join(dir, "gone.md"))

	res := scan(t, store, Options{Include: []string{dir}})
	// Only the row that would have been deleted stands down. The one whose
	// file is present is reconciled normally — declining it would say nothing
	// and cost the loudest line in status.
	if res.Deleted != 0 || res.Unverified != 1 {
		t.Fatalf("Result = %+v, want the vanished row declined and nothing else", res)
	}
	if !store.mountsRemembered {
		t.Fatal("the first pass did not record what it observed, so the next is blind too")
	}

	res = scan(t, store, Options{Include: []string{dir}})
	if res.Deleted != 1 || res.Unverified != 0 {
		t.Errorf("Result = %+v, want the second pass to act", res)
	}
}

// A fresh install indexes its corpus and reconciles it in the same scan.
// Nothing is missing, so nothing should be declined — a whole-corpus decline
// here fires the product's loudest alarm, with no reason to attach to it, at
// a daemon doing exactly the right thing.
func TestScanOnAFreshInstallRaisesNoAlarm(t *testing.T) {
	dir := tmpDir(t)
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		write(t, filepath.Join(dir, name), name)
	}
	store := newFakeStore()
	store.mountsRemembered = false

	res := scan(t, store, Options{Include: []string{dir}})

	if res.Discovered != 3 {
		t.Fatalf("Result = %+v, want the corpus indexed", res)
	}
	if res.Unverified != 0 || res.Ignored != 0 {
		t.Errorf("Result = %+v, want nothing declined — no row is missing", res)
	}
}

// Off macOS the mount table is always empty, so a value-only comparison would
// never write, "remembered" would never become true, and deletions would be
// declined for ever. The observation is what gets recorded, not the value.
func TestScanRecordsTheObservationEvenWhenNoMountsAreVisible(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "keep.md"), "keep")
	store := newFakeStore()
	store.mountsRemembered = false
	seedDoc(store, filepath.Join(dir, "gone.md"))

	sc := newScanner(store, Options{Include: []string{dir}})
	sc.mounts = func() ([]string, bool, error) { return nil, false, nil } // the mounts_other.go answer
	if _, err := sc.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !store.mountsRemembered {
		t.Fatal("no observation recorded, so every future pass declines deletions for ever")
	}

	sc = newScanner(store, Options{Include: []string{dir}})
	sc.mounts = func() ([]string, bool, error) { return nil, false, nil }
	res := scanWith(t, sc)
	if res.Deleted != 1 {
		t.Errorf("Result = %+v, want the second pass to act", res)
	}
}

// A failed memory read must not let the learn step overwrite what it could
// not read. Replacing the record of an unplugged volume with a record that it
// does not exist is how a transient database error becomes a corpus wipe on
// the following pass.
func TestScanDoesNotOverwriteAMountMemoryItCouldNotRead(t *testing.T) {
	dir := tmpDir(t)
	volume := filepath.Join(dir, "volume")
	store := newFakeStore()
	store.knownMounts = []string{"/", volume}
	seedDoc(store, filepath.Join(volume, "one.md"))
	store.knownMountsErr = errors.New("database is locked")

	sc := newScanner(store, Options{Include: []string{dir}})
	if _, err := sc.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(store.knownMounts, volume) {
		t.Fatalf("knownMounts = %v, want %s still recorded — the read failed, the drive did not go away",
			store.knownMounts, volume)
	}
}

// A corrupt or unreadable memory is advisory data, not a reason to fail the
// scan: a fatal error here stops LastScan advancing, which makes every cycle
// due for a full walk for ever while indexing appears to work.
func TestScanSurvivesAnUnreadableMountMemory(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "keep.md"), "keep")
	store := newFakeStore()
	seedDoc(store, filepath.Join(dir, "gone.md"))
	store.knownMountsErr = errors.New("invalid character '}' looking for beginning of value")

	res, err := newScanner(store, Options{Include: []string{dir}}).Scan(t.Context())
	if err != nil {
		t.Fatalf("Scan = %v, want the corrupt memory declined rather than fatal", err)
	}
	if res.Deleted != 0 || res.Unverified != 1 {
		t.Errorf("Result = %+v, want the deletion declined for the pass", res)
	}
}

// An unreadable mount table must not report the root filesystem as unmounted.
func TestScanDoesNotClaimEveryRememberedVolumeIsUnmountedOnAMountTableFailure(t *testing.T) {
	dir := tmpDir(t)
	store := newFakeStore()
	store.knownMounts = []string{"/"}
	seedDoc(store, filepath.Join(dir, "gone.md"))

	sc := newScanner(store, Options{Include: []string{dir}})
	sc.mounts = func() ([]string, bool, error) { return nil, true, os.ErrPermission }
	res := scanWith(t, sc)

	if len(res.Unmounted) != 0 {
		t.Errorf("Unmounted = %v, want nothing claimed unmounted when the table could not be read", res.Unmounted)
	}
}

// A configured root nested under another *by spelling* can resolve somewhere
// else entirely. Dropping it before resolution meant its real location never
// entered the root set and never marked the pass unverified, so rows under it
// read as out of scope — a path spelled verbatim in paths.include reported as
// "no longer in the configured paths", and pruned.
func TestScanDoesNotPruneANestedSymlinkedRoot(t *testing.T) {
	base := tmpDir(t)
	vault := filepath.Join(base, "vault")
	write(t, filepath.Join(vault, "own.md"), "own")
	// vault/Sync is a symlink out to another tree, and is *also* configured.
	elsewhere := filepath.Join(base, "elsewhere")
	write(t, filepath.Join(elsewhere, "synced.md"), "synced")
	sync := filepath.Join(vault, "Sync")
	if err := os.Symlink(elsewhere, sync); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	opts := Options{Include: []string{vault, sync}}

	scan(t, store, opts)
	if !slices.Contains(remaining(store), filepath.Join(elsewhere, "synced.md")) {
		t.Fatalf("catalog = %v, want the nested symlinked root walked at its resolved location", remaining(store))
	}

	res := scan(t, store, opts)

	if res.Pruned != 0 {
		t.Fatalf("Result = %+v, want nothing pruned — both roots are in paths.include", res)
	}
	if len(remaining(store)) != 2 {
		t.Errorf("catalog = %v, want both files kept", remaining(store))
	}
}

// A directory replaced by a symlink is skipped by the walk without a word,
// but os.Lstat follows symlinked *ancestors* — so rows beneath it stat fine
// while the target is attached and vanish the moment it is not. The walk
// records the skipped link so the reconcile knows not to trust either answer.
func TestScanDeclinesBeneathASymlinkedDirectory(t *testing.T) {
	base := tmpDir(t)
	root := filepath.Join(base, "root")
	write(t, filepath.Join(root, "keep.md"), "keep")
	// notes was a real directory; it is now a link to a tree outside the root.
	target := filepath.Join(base, "target")
	write(t, filepath.Join(target, "a.md"), "a")
	notes := filepath.Join(root, "notes")
	if err := os.Symlink(target, notes); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	seedDoc(store, filepath.Join(notes, "a.md"))
	scan(t, store, Options{Include: []string{root}})

	// The target goes away — an unplugged volume, from the row's point of view.
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	res := scan(t, store, Options{Include: []string{root}})

	if res.Deleted != 0 {
		t.Fatalf("Result = %+v, want nothing deleted beneath a link the walk never looked through", res)
	}
	if res.Unverified != 1 {
		t.Errorf("Unverified = %d, want the decline counted", res.Unverified)
	}
}

// A missing include root is routine — ADR 0016's own remedy for the removable
// -volume risks is to give a drive its own root, which then ENOENTs on every
// scan while unplugged. Suspending scope pruning for that would switch off
// "editing paths.include takes effect" permanently. Its rows keep the
// configured spelling, so withinAny already covers them.
func TestScanStillPrunesWhenAPlainRootIsMissing(t *testing.T) {
	base := tmpDir(t)
	live := filepath.Join(base, "live")
	write(t, filepath.Join(live, "keep.md"), "keep")
	absent := filepath.Join(base, "absent")
	store := newFakeStore()
	stale := filepath.Join(base, "elsewhere", "old.md")
	write(t, stale, "old")
	seedDoc(store, stale)

	res := scan(t, store, Options{Include: []string{live, absent}})

	if res.Pruned != 1 {
		t.Fatalf("Result = %+v, want the out-of-scope row pruned despite a missing root", res)
	}
	if res.Ignored != 0 {
		t.Errorf("Ignored = %d, want scope pruning still active", res.Ignored)
	}
}

// An empty answer from a platform that can answer is not an answer: "/" is
// always mounted, so zero entries means the call was short-changed, and
// treating it as fact would report every remembered volume — including the
// root filesystem — as ejected.
func TestScanTreatsAnEmptyMountTableAsUnreadable(t *testing.T) {
	dir := tmpDir(t)
	store := newFakeStore()
	store.knownMounts = []string{"/"}
	seedDoc(store, filepath.Join(dir, "gone.md"))

	sc := newScanner(store, Options{Include: []string{dir}})
	sc.mounts = func() ([]string, bool, error) { return []string{}, true, nil }
	res := scanWith(t, sc)

	if len(res.Unmounted) != 0 {
		t.Errorf("Unmounted = %v, want nothing claimed ejected from a short answer", res.Unmounted)
	}
	if res.Deleted != 0 || res.Unverified != 1 {
		t.Errorf("Result = %+v, want the pass to decline", res)
	}
}

// A value that will never parse can only be repaired by overwriting it. Left
// alone it declines deletions for ever and needs the SQLite file hand-edited.
func TestScanRebuildsACorruptMountMemory(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "keep.md"), "keep")
	store := newFakeStore()
	seedDoc(store, filepath.Join(dir, "gone.md"))
	store.knownMountsErr = fmt.Errorf("%w: unexpected end of JSON input", domain.ErrScanStateCorrupt)

	res := scan(t, store, Options{Include: []string{dir}})
	if res.Deleted != 0 || res.Unverified != 1 {
		t.Fatalf("Result = %+v, want the pass to decline once", res)
	}
	if len(res.DeclineReasons) == 0 {
		t.Error("DeclineReasons is empty — the loudest line in the product would fire with no reason")
	}

	// Repaired: the next pass acts.
	store.knownMountsErr = nil
	res = scan(t, store, Options{Include: []string{dir}})
	if res.Deleted != 1 {
		t.Errorf("Result = %+v, want the rebuilt memory to let the next pass act", res)
	}
}

// The remembered set must converge. learnMounts adds an in-scope mount that
// holds no indexable files; forgetEmptyMounts used to drop it again, so the
// two wrote to the database on every scan for ever.
func TestScanMountMemoryConverges(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "a.md"), "a")
	store := newFakeStore()

	scan(t, store, Options{Include: []string{dir}})
	first := slices.Clone(store.knownMounts)
	writes := len(store.knownMounts)
	_ = writes
	scan(t, store, Options{Include: []string{dir}})

	if !slices.Equal(store.knownMounts, first) {
		t.Errorf("knownMounts oscillated: %v then %v", first, store.knownMounts)
	}
	if !slices.Contains(store.knownMounts, "/") {
		t.Errorf("knownMounts = %v, want the live in-scope mount kept", store.knownMounts)
	}
}

// A store failure inside the reconcile is a decline, never a fatal Scan. A
// fatal one stops LastScan advancing, which leaves every cycle due for a full
// walk for ever while indexing appears to work.
func TestScanSurvivesAStoreFailureInTheReconcile(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "keep.md"), "keep")
	store := newFakeStore()
	seedDoc(store, filepath.Join(dir, "gone.md"))
	store.listPathsErr = errors.New("database is locked")

	res, err := newScanner(store, Options{Include: []string{dir}}).Scan(t.Context())
	if err != nil {
		t.Fatalf("Scan = %v, want the store failure declined rather than fatal", err)
	}
	if res.Reconciled {
		t.Error("Reconciled = true, so zero declines would be read as a measurement")
	}
}

// The risk this closes: an include root typed in a case the filesystem does
// not use. It walks and indexes perfectly well — every row stored under the
// typed spelling — while the mount table reports the real one, so no volume
// is ever recognised as in scope and the unplugged-drive protection is
// silently disabled outright. Canonicalising the root at resolution keeps
// every downstream comparison byte-wise and makes all three agree.
func TestScanProtectsAVolumeUnderAMiscasedRoot(t *testing.T) {
	base := tmpDir(t)
	probe := filepath.Join(base, "CaseProbe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "caseprobe")); err != nil {
		t.Skip("filesystem is case-sensitive; the two spellings are two directories")
	}

	// On disk: lowercase. In the config: capitalised.
	root := filepath.Join(base, "notes")
	volume := filepath.Join(root, "ext")
	if err := os.MkdirAll(volume, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "local.md"), "local")
	typed := filepath.Join(base, "Notes")

	store := newFakeStore()
	// Two files on the "volume", already indexed, spelled as the filesystem
	// spells them — which is what a walk from the canonical root produces.
	for _, name := range []string{"one.md", "two.md"} {
		seedDoc(store, filepath.Join(volume, name))
	}
	store.knownMounts = []string{"/", volume}

	// The volume is not in the live table: unplugged.
	res := scanWith(t, withMounts(newScanner(store, Options{Include: []string{typed}}), "/"))

	if res.Deleted != 0 || res.Pruned != 0 {
		t.Fatalf("Result = %+v, want nothing removed — the volume is merely unplugged", res)
	}
	if !slices.Contains(res.Unmounted, volume) {
		t.Errorf("Unmounted = %v, want %s recognised despite the miscased root", res.Unmounted, volume)
	}
	if res.Unverified != 2 {
		t.Errorf("Unverified = %d, want both rows declined", res.Unverified)
	}
}

// Off macOS the mount table is empty because the platform cannot answer, not
// because nothing is mounted. Reading that as "every remembered volume was
// ejected" would decline the whole catalog for ever on any database carrying
// mounts over — the opposite of mounts_other.go's documented behaviour.
func TestScanDoesNotEjectRememberedVolumesWhereMountsAreUnsupported(t *testing.T) {
	dir := tmpDir(t)
	write(t, filepath.Join(dir, "keep.md"), "keep")
	volume := filepath.Join(dir, "volume")
	store := newFakeStore()
	store.knownMounts = []string{"/", volume}
	seedDoc(store, filepath.Join(dir, "gone.md"))

	sc := newScanner(store, Options{Include: []string{dir}})
	sc.mounts = func() ([]string, bool, error) { return nil, false, nil }
	res := scanWith(t, sc)

	if len(res.Unmounted) != 0 {
		t.Errorf("Unmounted = %v, want nothing claimed ejected — the platform cannot answer", res.Unmounted)
	}
	// Every other guard still applies, so a genuinely vanished file still goes.
	if res.Deleted != 1 || res.Unverified != 0 {
		t.Errorf("Result = %+v, want the deletion still noticed", res)
	}
}

// status renders the unmounted list as the *reason* for Unverified, so it has
// to be one. A volume that is absent but holds no rows explains none of the
// count, and naming it alongside a decline produced by something else — a
// directory the walk could not read — points the reader at a USB port for a
// Full Disk Access problem.
func TestScanNamesOnlyUnmountedVolumesThatHoldRows(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root defeats chmod 0")
	}
	dir := tmpDir(t)
	// The real cause: a subtree the walk cannot read, with rows under it.
	denied := filepath.Join(dir, "denied")
	write(t, filepath.Join(denied, "a.md"), "a")
	write(t, filepath.Join(denied, "b.md"), "b")
	store := newFakeStore()
	scan(t, store, Options{Include: []string{dir}})
	if err := os.Chmod(denied, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o755) })

	// A red herring: a volume remembered from an earlier scan, now unplugged,
	// whose rows have since gone.
	stick := filepath.Join(dir, "stick")
	store.knownMounts = []string{"/", stick}

	res := scanWith(t, withMounts(newScanner(store, Options{Include: []string{dir}}), "/"))

	if res.Unverified != 2 {
		t.Fatalf("Unverified = %d, want the two rows under the unreadable directory", res.Unverified)
	}
	if len(res.Unmounted) != 0 {
		t.Errorf("Unmounted = %v, want empty — that volume holds nothing and explains none of the count",
			res.Unmounted)
	}
}

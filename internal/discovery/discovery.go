// Package discovery keeps the catalog's view of the filesystem current: new
// and changed files become state=discovered rows for the pipeline, unchanged
// files are skipped cheaply (size/mtime, then content hash), renamed files
// keep their document IDs, and deleted files are purged. Per-path problems
// (permission errors — the macOS TCC constraint — unreadable files, missing
// roots) are collected in the result, never silently swallowed. See
// DESIGN.md (Indexing pipeline and queue; Change detection; Data retention;
// doc_id Closed issue).
//
// Two entry points, one body of rules. Scan walks every include root; it is
// the periodic backstop, and the only thing that notices a file nothing told
// us about. ScanPaths reconciles a named set of paths; it is what the
// FSEvents watcher drives, and it is the difference between a saved note
// being searchable in seconds and at the next walk. Both run the same
// per-file change detection, because two implementations of "has this file
// changed" would eventually disagree.
//
// Known limitation, by design ("cheap check"): an edit that preserves
// both size and mtime is invisible until either changes.
package discovery

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bcrisp4/bsearch/internal/domain"
	"github.com/bcrisp4/bsearch/internal/pathutil"
)

// ErrRootExcluded marks an include root skipped because it matches the
// exclude rules. Exclusions win over includes by design, but silently
// swallowing an explicitly configured root would make "why is nothing
// indexed" undiagnosable — so the skip is recorded as a PathError.
var ErrRootExcluded = errors.New("include root matches the exclude rules")

// ErrParentUnreachable marks a vanished path that was *not* purged because
// its parent directory had vanished too. One file disappearing is a
// deletion; a whole subtree disappearing at once is a volume unmounting or
// a permission grant being revoked, and treating that as a thousand
// deletions would destroy the corpus over a blip. The periodic scan's own
// reconciliation (issue #57) is the slower, safer path for those.
var ErrParentUnreachable = errors.New("parent directory is unreachable; not treating this as a deletion")

// Options configures a Scanner.
type Options struct {
	// Include are the absolute, tilde-expanded root directories to walk
	// (config [paths].include).
	Include []string
	// Excluded reports whether a path is deny-listed; exclusions win over
	// includes. Callers wire config.ExcludeRules().Match. Nil excludes
	// nothing.
	Excluded func(path string) bool
}

// PathError records a per-path problem encountered during a scan.
type PathError struct {
	Path string
	Err  error
}

// Result summarises one scan.
type Result struct {
	// Discovered counts new or content-changed files upserted as
	// state=discovered (renames included).
	Discovered int
	// Unchanged counts files skipped because the catalog is current
	// (size/mtime match, or content hash match after a touch).
	Unchanged int
	// Renamed counts moved files whose document ID was preserved
	// (a subset of Discovered).
	Renamed int
	// Dataless counts iCloud placeholder files skipped — indexing must
	// never trigger cloud downloads.
	Dataless int
	// Deleted counts catalog rows purged because their file is gone. Only
	// ScanPaths sets it: a walk sees what exists, so noticing an absence
	// needs either an event that named the path (here) or a catalog-side
	// reconcile (issue #57).
	Deleted int
	// Ignored counts named paths that were out of scope — outside every
	// include root, or matched by the deny-list. Only ScanPaths sets it: a
	// walk only ever visits paths that are in scope by construction.
	//
	// A count rather than a silent skip because the all-ignored case is the
	// one failure that looks exactly like success. A watch root spelled
	// differently from the paths the watcher reports (case, or the
	// /System/Volumes/Data firmlink) subscribes fine and delivers fine, and
	// every path it delivers lands here — a daemon that is watching
	// attentively and indexing nothing.
	Ignored int
	// PathErrors holds every per-path failure: permission errors,
	// unreadable files, missing include roots.
	PathErrors []PathError
}

// Scanner performs one-shot discovery over the include roots.
type Scanner struct {
	store domain.DocumentStore
	opts  Options

	// dataless is a seam for tests; production is the platform check.
	dataless func(fs.FileInfo) bool
}

// New returns a Scanner persisting through store.
func New(store domain.DocumentStore, opts Options) *Scanner {
	return &Scanner{store: store, opts: opts, dataless: isDataless}
}

// Scan walks every include root and reconciles the catalog. It returns an
// error only for fatal problems (store failure, context cancellation);
// per-path problems accumulate in Result.PathErrors.
func (s *Scanner) Scan(ctx context.Context) (Result, error) {
	var res Result
	for _, root := range s.roots(&res) {
		if err := s.walkTree(ctx, root, &res); err != nil {
			return res, err
		}
	}
	return res, nil
}

// ScanPaths reconciles the catalog for a named set of paths — what the
// FSEvents watcher delivers. Paths outside every include root, or matching
// the deny-list, are ignored; everything else gets the same change detection
// a walk would give it. As with Scan, the error return is for fatal problems
// only.
//
// The two passes are not a tidiness: their order is what makes a rename keep
// its document ID. A move arrives as the old path and the new path in one
// batch, and rename detection works by finding a catalog row whose content
// hash matches and whose path is gone from disk. Reconciling the paths that
// exist first lets the new path claim the old row; only then are the
// still-missing paths considered for deletion, by which point the renamed
// row has already moved out from under the old path and is not purged. In
// the other order every rename would purge and re-mint (DESIGN.md: doc_id).
func (s *Scanner) ScanPaths(ctx context.Context, paths []string) (Result, error) {
	var res Result
	// Root-resolution problems are the walk's to report, not this batch's:
	// they are a property of the configuration, they do not change between
	// reconciles, and folding them in here would re-report the same broken
	// root every debounce window — and fill the caller's per-reconcile error
	// budget before a single event-path problem got a look in.
	roots := s.roots(&Result{})
	excluded := s.excluded()

	// Sorted so an ancestor is always seen before its descendants, in both
	// passes. Copied rather than sorted in place: the caller's slice is not
	// ours to reorder.
	sorted := slices.Clone(paths)
	for i, p := range sorted {
		sorted[i] = filepath.Clean(p)
	}
	slices.Sort(sorted)

	var vanished []string
	// walked are the directories already descended into. A list rather than
	// just the most recent one: sorting puts a directory before its contents,
	// but a sibling can still sort between them ("/a", "/a!", "/a/x"), so the
	// most recent entry is not always the relevant ancestor.
	var walked []string
	for _, path := range sorted {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if !filepath.IsAbs(path) || !withinAny(path, roots) || excluded(path) {
			// Counted rather than dropped in silence. A watcher whose events
			// all land here is subscribed, healthy and useless — the shape a
			// root spelled differently from the paths FSEvents reports takes
			// — and without a number there is nothing to tell that apart from
			// a quiet machine.
			res.Ignored++
			continue
		}
		if withinAny(path, walked) {
			// Already reconciled by the walk of an ancestor: a directory
			// created or moved arrives with an event per file inside it too,
			// and re-statting every one of them would double the work of
			// every bulk copy.
			continue
		}
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			vanished = append(vanished, path)
			continue
		case err != nil:
			res.PathErrors = append(res.PathErrors, PathError{Path: path, Err: err})
			continue
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			// Never followed, never indexed — the walk's rule, applied here
			// so an event cannot smuggle content past the deny-list.
		case info.IsDir():
			// FSEvents reports a directory being moved or created as one
			// event for the directory, not one per file inside it, so the
			// contents are only seen by descending.
			if err := s.walkTree(ctx, path, &res); err != nil {
				return res, err
			}
			walked = append(walked, path)
		case !info.Mode().IsRegular() || !isTextFile(info.Name()):
		case s.dataless(info):
			res.Dataless++
		default:
			if err := s.processFile(ctx, path, info, &res); err != nil {
				return res, err
			}
		}
	}

	// Ancestor-before-descendant again, and for a sharper reason: one
	// `rm -rf` arrives as the directory plus an event per file inside it, and
	// once the directory has been answered for the files under it have been
	// answered too. Without that, every one of them would go on to find its
	// own parent missing and report an unreachable parent — turning a handled
	// deletion into a burst of warnings, and drowning the signal that error
	// exists to carry.
	var covered []string
	for _, path := range vanished {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if withinAny(path, covered) {
			continue
		}
		settled, err := s.purge(ctx, path, &res)
		if err != nil {
			return res, err
		}
		if settled {
			covered = append(covered, path)
		}
	}
	return res, nil
}

// purge removes the catalog rows for a path that is gone, and everything
// beneath it — a vanished path does not say whether it was a file or a
// directory, and both answers are the same call. It reports whether the
// path's absence was settled here, either by purging it or by declining to:
// either way the subtree has been answered for, and the caller uses that to
// stop the paths underneath asking the same question again.
//
// Two checks stand between an event and a delete, because deletion is the
// one operation here that destroys work. The path is re-stat'ed, since the
// first pass may have moved a renamed row onto it or the file may have been
// recreated inside the window. And the parent must still be there: an
// unmounted volume or a revoked Full Disk Access grant makes a whole subtree
// return ENOENT at once, which is a mount event wearing a deletion's
// clothes. Declining there costs freshness — the file stays indexed until
// the catalog-side reconcile of issue #57 lands to notice it, which today
// means until the user deletes it again with the daemon up — and that is the
// direction to be wrong in.
func (s *Scanner) purge(ctx context.Context, path string, res *Result) (settled bool, err error) {
	switch _, err := os.Lstat(path); {
	case err == nil:
		// Back already — recreated, or a renamed row moved onto it. Nothing
		// is settled: the paths beneath it are still their own question.
		return false, nil
	case !errors.Is(err, fs.ErrNotExist):
		// Only ENOENT is evidence of a deletion. EACCES from a grant revoked
		// since the first stat, EIO from a failing disk, ESTALE from a
		// network volume — all of them mean "cannot tell", and "cannot tell"
		// must never be spent on the one operation here that destroys work.
		res.PathErrors = append(res.PathErrors, PathError{Path: path, Err: err})
		return true, nil //nolint:nilerr // recorded in PathErrors; declining to delete is the outcome
	}
	if _, err := os.Lstat(filepath.Dir(path)); err != nil {
		res.PathErrors = append(res.PathErrors, PathError{
			Path: path,
			Err:  fmt.Errorf("%w: %s", ErrParentUnreachable, filepath.Dir(path)),
		})
		return true, nil //nolint:nilerr // recorded in PathErrors; declining to delete is the outcome
	}
	removed, err := s.store.DeleteByPathPrefix(ctx, path)
	if err != nil {
		return false, err
	}
	res.Deleted += removed
	return true, nil
}

// walkTree walks one directory tree, reconciling every file it is allowed to
// look at. Errors returned are fatal; per-path problems land in res.
func (s *Scanner) walkTree(ctx context.Context, root string, res *Result) error {
	excluded := s.excluded()
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			// Unreadable dir (TCC EPERM), vanished entry, missing
			// root: record and keep walking. WalkDir already skips
			// descent into a dir it could not read.
			res.PathErrors = append(res.PathErrors, PathError{Path: path, Err: walkErr})
			return nil //nolint:nilerr // recorded in PathErrors; keep walking
		}
		if d.Type()&fs.ModeSymlink != 0 {
			// Never follow or index symlinks: cycle-safe, and a link
			// cannot smuggle content past the deny-list.
			return nil
		}
		if d.IsDir() {
			if excluded(path) {
				return fs.SkipDir // prune: nothing under it is statted
			}
			return nil
		}
		if !d.Type().IsRegular() || !isTextFile(d.Name()) || excluded(path) {
			return nil
		}
		info, err := d.Info()
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil // deleted mid-walk
		case err != nil:
			res.PathErrors = append(res.PathErrors, PathError{Path: path, Err: err})
			return nil //nolint:nilerr // recorded in PathErrors; keep walking
		}
		if s.dataless(info) {
			res.Dataless++
			return nil
		}
		return s.processFile(ctx, path, info, res)
	})
}

// Roots returns the directories a scan would walk — canonicalized,
// deduplicated, deny-list applied — together with the per-root problems
// found resolving them. It exists so the watcher subscribes to exactly the
// roots the walk walks: two implementations of "which directories are in
// scope" would eventually disagree, and the disagreement would look like
// files that are watched but never indexed.
func (s *Scanner) Roots() ([]string, []PathError) {
	var res Result
	return s.roots(&res), res.PathErrors
}

// roots resolves the include roots, recording problems in res.
func (s *Scanner) roots(res *Result) []string {
	excluded := s.excluded()

	// Canonicalize roots first, then normalize again: a resolved root
	// may land on or under another include root (symlinked root, or an
	// aliased ancestor like macOS /var → /private/var), and only the
	// second pass can see that overlap.
	resolved := make([]string, 0, len(s.opts.Include))
	for _, root := range normalizeRoots(s.opts.Include) {
		if root, ok := canonicalRoot(root, res); ok {
			resolved = append(resolved, root)
		}
	}

	out := make([]string, 0, len(resolved))
	for _, root := range normalizeRoots(resolved) {
		if excluded(root) {
			res.PathErrors = append(res.PathErrors, PathError{Path: root, Err: ErrRootExcluded})
			continue
		}
		out = append(out, root)
	}
	return out
}

// excluded returns the deny-list predicate, defaulting to "excludes
// nothing" so callers never have to nil-check it.
func (s *Scanner) excluded() func(string) bool {
	if s.opts.Excluded == nil {
		return func(string) bool { return false }
	}
	return s.opts.Excluded
}

// withinAny reports whether path is equal to or under any of the roots.
func withinAny(path string, roots []string) bool {
	return slices.ContainsFunc(roots, func(root string) bool {
		return pathutil.Within(path, root)
	})
}

// processFile runs change detection for one candidate file and persists
// the outcome. Returned errors are fatal (store failures); read problems
// on the file itself become PathErrors.
func (s *Scanner) processFile(ctx context.Context, path string, info fs.FileInfo, res *Result) error {
	existing, known, err := s.store.GetByPath(ctx, path)
	if err != nil {
		return err
	}

	// Cheap check: size and mtime unchanged → catalog is current, no read.
	if known && existing.Size == info.Size() &&
		existing.MTime.UnixNano() == info.ModTime().UnixNano() {
		res.Unchanged++
		return nil
	}

	hash, err := hashFile(path)
	if err != nil {
		res.PathErrors = append(res.PathErrors, PathError{Path: path, Err: err})
		return nil //nolint:nilerr // recorded in PathErrors; keep walking
	}

	// Touched but content-identical → refresh stat, keep everything else.
	if known && existing.ContentHash == hash {
		switch err := s.store.UpdateDocumentStat(ctx, existing.ID, info.Size(), info.ModTime()); {
		case errors.Is(err, domain.ErrDocumentGone):
			// The row was purged between the read above and this write.
			//
			// That used to be ordinary: the watcher's reconcile ran on its own
			// goroutine and could see this path deleted while the walk was
			// mid-stride over it. Since ADR 0014 both run on the scheduler
			// goroutine, so nothing can purge between the two — reaching here
			// means the single-writer invariant broke, and swallowing it with
			// a bare `return nil` would absorb that silently: no log, no
			// PathError, and the document simply stops being counted.
			//
			// Recorded as a path error instead, which is the channel the rest
			// of this function already uses for "something was wrong with this
			// file" and which `bsearch status` surfaces (CLAUDE.md: never a
			// silent skip). Still not fatal: one document is not worth failing
			// a walk over, and the next pass rediscovers the file if it is
			// really still there.
			res.PathErrors = append(res.PathErrors, PathError{Path: path, Err: err})
			return nil //nolint:nilerr // recorded in PathErrors; keep walking
		case err != nil:
			return err
		}
		res.Unchanged++
		return nil
	}

	id, renamed := existing.ID, false
	if !known {
		if id, renamed, err = s.resolveID(ctx, hash, res); err != nil {
			return err
		}
	}

	doc := domain.Document{
		ID:          id,
		Path:        path,
		ContentHash: hash,
		Size:        info.Size(),
		MTime:       info.ModTime(),
		State:       domain.DocStateDiscovered,
	}
	if _, err := s.store.UpsertDocument(ctx, doc, nil); err != nil {
		return err
	}
	res.Discovered++
	if renamed {
		res.Renamed++
	}
	return nil
}

// resolveID decides the document ID for a path not in the catalog:
// rename detection per DESIGN.md's doc_id Closed issue. A catalog row
// with the same content hash whose path is gone from disk is a rename —
// reuse its ID. Anything ambiguous (old path still exists = copy; several
// candidate rows; stat failure on the old path) mints a fresh ID: prefer
// id churn over a false merge.
func (s *Scanner) resolveID(ctx context.Context, hash string, res *Result) (id string, renamed bool, err error) {
	candidates, err := s.store.GetByContentHash(ctx, hash)
	if err != nil {
		return "", false, err
	}
	var gone []domain.Document
	for _, c := range candidates {
		switch _, statErr := os.Lstat(c.Path); {
		case statErr == nil:
			// Old path still exists: a copy, not a rename.
		case errors.Is(statErr, fs.ErrNotExist):
			gone = append(gone, c)
		default:
			// Can't verify the old path is gone (e.g. TCC EPERM):
			// counted as still existing — prefer id churn over a false
			// merge — but recorded, or the churn is undiagnosable.
			res.PathErrors = append(res.PathErrors, PathError{Path: c.Path, Err: statErr})
		}
	}
	if len(gone) == 1 {
		return gone[0].ID, true, nil
	}
	id, err = newDocID()
	return id, false, err
}

// newDocID mints an opaque surrogate document ID: "d_" + 16 hex chars
// (64 random bits — collisions negligible at the 100k-doc target, which
// matters because the store's upsert would silently merge on collision).
func newDocID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint doc id: %w", err)
	}
	return "d_" + hex.EncodeToString(b[:]), nil
}

// hashFile returns the lowercase hex sha256 of the file contents.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// isTextFile reports whether the file is in M1's text/markdown corpus.
// Format routing moves behind the converter port with M6 (issue #21).
func isTextFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown", ".txt":
		return true
	}
	return false
}

// normalizeRoots cleans the include roots and drops duplicates and roots
// nested under another root, so overlapping includes never double-visit.
func normalizeRoots(include []string) []string {
	roots := make([]string, len(include))
	for i, r := range include {
		roots[i] = filepath.Clean(r)
	}
	slices.Sort(roots)
	var out []string
	for _, r := range roots {
		nested := slices.ContainsFunc(out, func(kept string) bool {
			return pathutil.Within(r, kept)
		})
		if !nested {
			out = append(out, r)
		}
	}
	return out
}

// canonicalRoot resolves an include root to its canonical on-disk path,
// following symlinks in the root and its ancestors. An explicitly
// configured symlinked root is user intent (~/notes → ~/Dropbox/notes is
// a common setup), unlike symlinks met during the walk, which are never
// followed — without this, WalkDir lstats the root, the symlink guard
// drops it, and the whole corpus silently scans to zero. Canonical
// ancestors matter too: aliases like macOS /var → /private/var would
// otherwise defeat duplicate-root detection. A root that fails to
// resolve is recorded and skipped; a missing root passes through so
// WalkDir reports it like any other unreadable root.
func canonicalRoot(root string, res *Result) (string, bool) {
	resolved, err := filepath.EvalSymlinks(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return pathutil.FoldDataVolume(root), true
	case err != nil:
		res.PathErrors = append(res.PathErrors, PathError{Path: root, Err: err})
		return "", false
	}
	// The data-volume firmlink is not a symlink, so EvalSymlinks leaves that
	// spelling of a root exactly as configured while the FSEvents adapter
	// folds it out of every event path. Folded here too, or the two would
	// never meet and every event under such a root would be ignored.
	return pathutil.FoldDataVolume(resolved), true
}

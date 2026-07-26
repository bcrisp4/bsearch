// Package discovery keeps the catalog's view of the filesystem current: it
// owns the documents table (ADR 0015). New and changed files become
// documents rows pointing at their content hash — creating the content row
// at state=discovered when the hash is new — unchanged files are skipped
// cheaply (size/mtime, then content hash), and deleted files are purged.
// Per-path problems (permission errors — the macOS TCC constraint —
// unreadable files, missing roots) are collected in the result, never
// silently swallowed. See DESIGN.md (Indexing pipeline and queue; Identity:
// path locates, content hash identifies; Data retention).
//
// There is deliberately no rename detection. A rename is a documents row
// appearing at a new path with the same hash while the old path purges;
// chunks, vectors and summaries were never addressed by path, so nothing
// needs repointing and no identity needs preserving (ADR 0015).
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
	// Discovered counts new or content-changed files upserted (the content
	// row is created at state=discovered when the hash is new to the
	// catalog).
	Discovered int
	// Unchanged counts files skipped because the catalog is current
	// (size/mtime match, or content hash match after a touch).
	Unchanged int
	// Changed counts known paths re-pointed to a different content hash —
	// an edit, not a new file (a subset of Discovered). The orphan sweep
	// keys on it: re-pointing a path is one of the two ways content is
	// orphaned, deletion being the other.
	Changed int
	// Dataless counts iCloud placeholder files skipped — indexing must
	// never trigger cloud downloads. Each is also persisted as an unread
	// documents row (reason dataless) unless the catalog already holds its
	// bytes from before the eviction and the stat still matches.
	Dataless int
	// Unread counts paths whose bytes could not be obtained this scan —
	// denied (TCC) or io_error — and that are recorded as unread rows.
	// Dataless placeholders are counted in Dataless, not here, so the two
	// never double-count in "how many paths did the scan reach". Paths
	// whose bytes the catalog already holds keep their hash and appear
	// only in PathErrors: unread_reason is for bytes never obtained
	// (ADR 0015).
	Unread int
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

// Reached reports how many paths the scan actually got an answer about —
// bytes read (Discovered, Unchanged) or deliberately left unread (Dataless).
// It is the shared "did the scan reach anything" predicate for the
// scheduler's Full Disk Access alarm and eval's empty-corpus guard, defined
// once so the two cannot drift.
//
// Unread is excluded on purpose: a denied file was *not* reached — its bytes
// were never obtained — and folding it in would silence the reached-nothing
// alarm for exactly the corpora it exists to flag (a listable root whose
// every file fails open with EPERM).
func (r Result) Reached() int {
	return r.Discovered + r.Unchanged + r.Dataless
}

// flushEvery is how many buffered document upserts ride in one transaction
// (#34). Large enough that a 100k-file first scan costs hundreds of
// transactions rather than 100k; small enough that each stays a short write
// under the busy-timeout discipline, and that a fatal mid-scan error loses
// at most one batch of not-yet-committed rows.
const flushEvery = 256

// Scanner performs one-shot discovery over the include roots.
type Scanner struct {
	store domain.DocumentStore
	opts  Options

	// pending buffers document upserts between flushes, so catalog writes
	// batch per transaction instead of paying one IMMEDIATE transaction per
	// file (#34). Emptied by flush; abandoned wholesale when a scan aborts —
	// the next scan rediscovers anything dropped, and prior batches are
	// already durable.
	pending []domain.Document

	// pendingRes stages the Result counts coupled to buffered rows
	// (Discovered, Changed, and the write-backed Unchanged/Unread/Dataless
	// increments). flush folds it into the caller's Result only after the
	// batch commits, so the counts a scan returns always describe committed
	// rows — the property the watcher's "recorded before the error is
	// considered" bookkeeping relies on. Counts with no write behind them
	// (cheap-check Unchanged, steady-state unread, Deleted, PathErrors) go
	// straight to the caller's Result. Discarded together with pending when
	// a scan aborts: rows that were never committed must never be counted,
	// and re-finding them next scan must not count them twice.
	pendingRes Result

	// dataless is a seam for tests; production is the platform check.
	dataless func(fs.FileInfo) bool
}

// New returns a Scanner persisting through store.
func New(store domain.DocumentStore, opts Options) *Scanner {
	return &Scanner{store: store, opts: opts, dataless: isDataless}
}

// buffer queues one document write, flushing into res when the batch is
// full. Callers stage the count for the row in pendingRes *before* calling
// buffer — the append can trigger the flush that folds it.
func (s *Scanner) buffer(ctx context.Context, doc domain.Document, res *Result) error {
	s.pending = append(s.pending, doc)
	if len(s.pending) >= flushEvery {
		return s.flush(ctx, res)
	}
	return nil
}

// flush commits the buffered batch in one transaction and, only then, folds
// the staged counts into res. Buffer and staged counts are surrendered
// before the write either way: a failed flush is fatal to the scan, and a
// Scanner outlives its scans, so rows — and counts — from an aborted one
// must never leak into the next.
func (s *Scanner) flush(ctx context.Context, res *Result) error {
	if len(s.pending) == 0 {
		return nil
	}
	batch := s.pending
	staged := s.pendingRes
	s.pending, s.pendingRes = nil, Result{}
	if err := s.store.UpsertDocuments(ctx, batch); err != nil {
		return err
	}
	res.Discovered += staged.Discovered
	res.Unchanged += staged.Unchanged
	res.Changed += staged.Changed
	res.Dataless += staged.Dataless
	res.Unread += staged.Unread
	return nil
}

// abandon drops any not-yet-committed rows and their staged counts — the
// deferred cleanup both entry points run so an aborted scan cannot leak
// either into the next one.
func (s *Scanner) abandon() {
	s.pending, s.pendingRes = nil, Result{}
}

// Scan walks every include root and reconciles the catalog. It returns an
// error only for fatal problems (store failure, context cancellation);
// per-path problems accumulate in Result.PathErrors.
//
// Writes are flushed at each root boundary, and write-coupled counts fold
// into the Result only as their batch commits — so Result's counts describe
// committed state however Scan returns, and a fatal error still leaves every
// prior root's batches durable and counted.
func (s *Scanner) Scan(ctx context.Context) (Result, error) {
	defer s.abandon()
	var res Result
	for _, root := range s.roots(&res) {
		if err := s.walkTree(ctx, root, &res); err != nil {
			return res, err
		}
		if err := s.flush(ctx, &res); err != nil {
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
// The two passes — paths that exist first, vanished paths second — order
// the work so purge's re-stat guard sees the world settled: a rename
// arrives as the old path and the new path in one batch, and reconciling
// the surviving paths first means the second pass stats the old path
// against a filesystem that is done moving. Nothing here preserves identity
// across the move any more (ADR 0015 retired rename detection); the order
// is purely about not purging on a stale view.
func (s *Scanner) ScanPaths(ctx context.Context, paths []string) (Result, error) {
	defer s.abandon()
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
	// Deduplicated because the write buffer makes duplicate input
	// non-idempotent: GetByPath reads the store, not pending, so the same
	// path twice in one call would be processed and counted twice — and a
	// readability change between the two visits would let the second row
	// clobber the first's hash inside one batch. The FSEvents adapter
	// cannot deliver duplicates, but ScanPaths is exported and must not
	// carry an undocumented precondition.
	sorted = slices.Compact(sorted)

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
		// Stat'ed before the walked-ancestor skip below: a walk reconciles
		// what exists, so it cannot have answered for a vanished descendant.
		// A directory deleted and recreated — or moved away and back —
		// inside one window arrives with events for children that no longer
		// exist, and skipping those unstatted would leave their rows serving
		// search hits forever.
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			vanished = append(vanished, path)
			continue
		case withinAny(path, walked):
			// Present, and already reconciled by the walk of an ancestor: a
			// directory created or moved arrives with an event per file
			// inside it too, and re-processing every one of them would
			// double the work of every bulk copy. A stat failure under a
			// walked ancestor lands here too — the walk already reported it.
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
			if err := s.noteDataless(ctx, path, info, &res); err != nil {
				return res, err
			}
		default:
			if err := s.processFile(ctx, path, info, &res); err != nil {
				return res, err
			}
		}
		// Flushed per event rather than once at the end: a debounce window
		// is nearly always far smaller than flushEvery, so one flush after
		// the loop would turn "lose at most one batch" into "lose the whole
		// reconcile" whenever shutdown or cancellation lands mid-pass.
		// Directory events still batch inside walkTree. This is also what
		// commits everything pass 1 learned before anything is deleted, so
		// the purge's re-stat guard runs against a settled catalog.
		if err := s.flush(ctx, &res); err != nil {
			return res, err
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
			return s.noteDataless(ctx, path, info, res)
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
	// Only for rows with content: an unread row (NULL hash) must be re-tried
	// every scan, so that granting Full Disk Access clears a denial without
	// the file having to change.
	if known && existing.ContentHash != "" && statMatches(existing, info) {
		res.Unchanged++
		return nil
	}

	// The one exception to the unconditional unread retry: io_error means
	// the last attempt got past open and died mid-read, after reading and
	// hashing every byte up to the failure point — retrying an unchanged
	// file re-pays that in full every scan (a large file on a failing disk
	// is gigabytes a day on a battery). denied stays unconditional, where
	// the retry earns its keep: open fails instantly, and the grant
	// appearing is exactly what it exists to notice.
	if known && existing.UnreadReason == domain.UnreadIOError && statMatches(existing, info) {
		res.Unread++
		return nil
	}

	hash, err := hashFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Deleted between the stat and the open — the same race the
			// walk filters at d.Info(). Not a problem to report and never
			// an io_error row: the path is gone, and persisting it would
			// leave a phantom row no walk can ever purge.
			return nil
		}
		// Reported both ways on purpose: the PathError is last-scan
		// transparency (what went wrong just now), the unread row is steady
		// state (`bsearch status` shows a denial however long ago the scan
		// that met it ran).
		res.PathErrors = append(res.PathErrors, PathError{Path: path, Err: err})
		reason := domain.UnreadIOError
		if errors.Is(err, fs.ErrPermission) {
			reason = domain.UnreadDenied
		}
		return s.noteUnread(ctx, path, info, reason, existing, known, res)
	}

	// Touched but content-identical → refresh stat. The same upsert as
	// below — a documents row carries nothing else worth protecting — but
	// counted as unchanged, because for the user nothing happened. The
	// unread→readable transition also lands here and below: the hash
	// filling in is what clears the reason.
	if known && existing.ContentHash == hash {
		s.pendingRes.Unchanged++
		return s.buffer(ctx, domain.Document{
			Path: path, ContentHash: hash,
			Size: info.Size(), MTime: info.ModTime(),
		}, res)
	}

	// New path, or a known path re-pointed to different bytes. The upsert
	// creates the content row at discovered when the hash is new; when it
	// is not — a duplicate, or a file restored to prior content — there is
	// simply no work to schedule, structurally (ADR 0015).
	s.pendingRes.Discovered++
	if known && existing.ContentHash != "" && existing.ContentHash != hash {
		s.pendingRes.Changed++
	}
	return s.buffer(ctx, domain.Document{
		Path: path, ContentHash: hash,
		Size: info.Size(), MTime: info.ModTime(),
	}, res)
}

// statMatches reports whether the catalog row still describes the file's
// size and mtime. UnixNano on both sides because that is the precision the
// adapter persists (mtime INTEGER — unix nanoseconds); one definition so no
// caller retypes the comparison with time.Equal and silently never matches.
func statMatches(existing domain.Document, info fs.FileInfo) bool {
	return existing.Size == info.Size() &&
		existing.MTime.UnixNano() == info.ModTime().UnixNano()
}

// noteDataless records an iCloud placeholder without ever opening the file,
// which could trigger a cloud download — the reason this cannot share
// processFile's read path and does its own lookup.
func (s *Scanner) noteDataless(ctx context.Context, path string, info fs.FileInfo, res *Result) error {
	existing, known, err := s.store.GetByPath(ctx, path)
	if err != nil {
		return err
	}
	return s.noteUnread(ctx, path, info, domain.UnreadDataless, existing, known, res)
}

// noteUnread is the one place the unread rules live — the package comment's
// "two implementations would eventually disagree" warning applies to this
// rule as much as to change detection, and the counting of its two callers
// had already diverged once. It records a path whose bytes were not obtained
// this scan: persisted as an unread row so `bsearch status` reports it in
// steady state, counted in Dataless for placeholders and Unread for
// everything else — a placeholder parked by design and a denial that needs
// fixing must never report as one number.
//
// A row whose bytes were obtained before the path stopped being readable
// keeps its hash — unread_reason is for bytes never obtained (ADR 0015) —
// with one carve-out per reason:
//   - unreadable file: kept unconditionally and counted by nothing;
//     the caller's PathError is the surface. Refreshing the stat here would
//     let the cheap check swallow the retry that notices a re-grant.
//   - dataless: kept only while the stat still matches. A placeholder whose
//     stat moved was edited remotely after eviction, and the indexed bytes
//     no longer describe the file — the hash is dropped for a dataless row,
//     because serving stale chunks indefinitely is worse than losing the
//     hit until the file is materialized.
func (s *Scanner) noteUnread(ctx context.Context, path string, info fs.FileInfo, reason domain.UnreadReason, existing domain.Document, known bool, res *Result) error {
	count := func(r *Result) {
		if reason == domain.UnreadDataless {
			r.Dataless++
		} else {
			r.Unread++
		}
	}
	if known && existing.ContentHash != "" {
		if reason != domain.UnreadDataless {
			return nil
		}
		if statMatches(existing, info) {
			count(res)
			return nil
		}
	}
	if known && existing.UnreadReason == reason && statMatches(existing, info) {
		// Steady state — recorded exactly like this already; count, no write.
		count(res)
		return nil
	}
	count(&s.pendingRes)
	return s.buffer(ctx, domain.Document{
		Path: path, UnreadReason: reason,
		Size: info.Size(), MTime: info.ModTime(),
	}, res)
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

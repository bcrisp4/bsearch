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
// deletions would destroy the corpus over a blip. The walk's own catalog-side
// reconcile is the slower, safer path for those: it stats each row rather than
// trusting one event, and recognises an unmounted volume from the mount table
// (ADR 0016).
var ErrParentUnreachable = errors.New("parent directory is unreachable; not treating this as a deletion")

// Options configures a Scanner.
type Options struct {
	// Include are the absolute, tilde-expanded root directories to walk
	// (config [paths].include).
	Include []string
	// Excluded reports whether a path is deny-listed; exclusions win over
	// includes. Callers wire config.ExcludeRules().Match. Nil excludes
	// nothing.
	//
	// It must answer for descendants, not just for the directory a rule
	// names: the walk prunes at the directory and never asks about what is
	// inside, but the deletion pass asks about catalog rows, which are files.
	// A predicate that matched only the named directory would leave every row
	// already indexed beneath a newly-excluded path in the index forever.
	// config.ExcludeSet.Match is prefix-based and satisfies this.
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
	// Deleted counts catalog rows purged because their file is gone. Set by
	// ScanPaths, from an event that named the path, and by Scan's
	// catalog-side reconcile, which enumerates rows and stats them — a walk
	// alone only ever sees what exists (ADR 0016).
	Deleted int
	// Pruned counts catalog rows removed because they are no longer in
	// scope: outside every include root, or newly deny-listed. Counted apart
	// from Deleted because the two are licensed by different evidence — the
	// filesystem for one, the config file for the other — and a config edit
	// that removes a thousand rows must be able to say so.
	Pruned int
	// Unverified counts catalog rows the reconcile deliberately left alone
	// because the walk could not vouch for their subtree: an unreadable
	// directory, a root that would not resolve, or a remembered mount point
	// that is not mounted right now.
	//
	// It is the number that tells "nothing was deleted because nothing was
	// deleted" apart from "nothing was deleted because the daemon is blind",
	// which is the difference between a healthy corpus and a missing Full
	// Disk Access grant.
	Unverified int
	// Ignored counts named paths that were out of scope — outside every
	// include root, or matched by the deny-list. Set by ScanPaths, and by
	// Scan's reconcile for out-of-scope rows in a pass where scope pruning
	// was suspended (a root failed to resolve): a walk itself only ever
	// visits paths that are in scope by construction.
	//
	// A count rather than a silent skip because the all-ignored case is the
	// one failure that looks exactly like success. A watch root spelled
	// differently from the paths the watcher reports (case, or the
	// /System/Volumes/Data firmlink) subscribes fine and delivers fine, and
	// every path it delivers lands here — a daemon that is watching
	// attentively and indexing nothing.
	Ignored int
	// Unmounted names the volumes that are not mounted where they were *and*
	// still hold catalog rows. It is the legible half of Unverified — "the
	// drive is unplugged" is something a user can act on, where "some
	// directory was unreadable" needs the PathErrors sample to mean anything.
	//
	// Restricted to volumes holding rows because status renders this as the
	// reason for Unverified: an absent volume with nothing beneath it explains
	// none of the count, and naming it would point at a USB port for a
	// permissions problem somewhere else entirely.
	Unmounted []string
	// Reconciled says the catalog-side deletion pass ran to completion. A pass
	// that never started (a fatal walk error returns first) or that stopped on
	// a store failure leaves the decline counters at zero, which is
	// indistinguishable from "nothing was declined" — so a consumer must not
	// treat those zeros as a measurement. Without it, an unrelated walk error
	// silently clears a standing "deletions are not being noticed" alarm.
	Reconciled bool
	// DeclineReasons are the reconcile's own explanations for standing down,
	// in the user's terms — a volume that is not mounted is already named in
	// Unmounted, so these are the ones nothing else can express: no volume
	// evidence yet, nothing in scope, a memory or a mount table that could not
	// be read.
	//
	// Deliberately not PathErrors. Those are per-path filesystem problems and
	// are rendered under "Unreadable paths", whose documented meaning is a
	// missing Full Disk Access grant; filing a corrupt SQLite value there
	// sends the reader to System Settings for a database problem.
	DeclineReasons []string
	// PathErrors holds every per-path failure: permission errors,
	// unreadable files, missing include roots.
	PathErrors []PathError
	// WalkErrors is how many of PathErrors the walk itself produced, before
	// the deletion reconcile appended any of its own. The Full Disk Access
	// alarm ("errors, and reached no files") keys on this rather than on the
	// slice length: a reconcile-side stat failure says nothing about whether
	// the walk could see the corpus, and folding it in would report a
	// permissions diagnosis for, say, a symlink loop.
	WalkErrors int
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

	// state persists the small facts a single walk cannot observe on its own
	// — currently which mount points have been seen inside the roots, which
	// is what lets an unmounted volume be told apart from a deletion.
	state domain.ScanState

	// dataless and mounts are seams for tests; production is the platform
	// check in each case.
	dataless func(fs.FileInfo) bool
	mounts   func() ([]string, bool, error)
}

// New returns a Scanner persisting through store, remembering across scans
// through state.
func New(store domain.DocumentStore, state domain.ScanState, opts Options) *Scanner {
	return &Scanner{store: store, state: state, opts: opts, dataless: isDataless, mounts: mountPoints}
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

	// Coverage is built as the walk goes, never reverse-engineered from
	// PathErrors afterwards. That slice mixes three different things — a
	// directory the walk was refused, a file it could stat but not open, and
	// a root deliberately excluded by config — and only the first is a prefix
	// the walk cannot vouch for. Reading them all as decline-prefixes made an
	// excluded root unprunable and a single unreadable file permanently
	// unreconcilable.
	cov := coverage{unknown: map[string]bool{}}
	roots, allVerified := s.resolveRoots(&res)
	cov.roots, cov.unresolved = roots, !allVerified

	for _, root := range cov.roots {
		if err := s.walkTree(ctx, root, &res, cov.unknown); err != nil {
			return res, err
		}
		if err := s.flush(ctx, &res); err != nil {
			return res, err
		}
	}
	// Snapshotted before the reconcile, which appends to the same slice: the
	// Full Disk Access alarm asks whether the *walk* hit trouble and reached
	// nothing, and a reconcile-side stat failure is not evidence of that.
	res.WalkErrors = len(res.PathErrors)

	// Sequenced rather than returned inline: `return res, s.reconcile(…, &res)`
	// reads res in the same statement as a call that mutates it through the
	// pointer, and Go leaves that ordering unspecified. It happens to work on
	// gc today; every reconcile count would silently return zero if it stopped.
	err := s.reconcileDeleted(ctx, cov, &res)
	return res, err
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
			// No coverage set: ScanPaths reconciles named paths and never
			// runs the catalog-side deletion pass, so it has nothing to
			// vouch for.
			if err := s.walkTree(ctx, path, &res, nil); err != nil {
				return res, err
			}
			walked = append(walked, path)
		case !admissible(info.Name(), info.Mode()):
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
// clothes. Declining there costs freshness — the file stays indexed until the
// walk's catalog-side reconcile notices it, up to a scan interval later — and
// that is the direction to be wrong in.
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
func (s *Scanner) walkTree(ctx context.Context, root string, res *Result, unknown map[string]bool) error {
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
			if unknown != nil && (d == nil || d.IsDir()) {
				// A directory whose contents were refused, or an entry that
				// could not be stat'ed at all (d is nil for the root itself).
				// Nothing beneath it is evidence of anything.
				unknown[path] = true
			}
			return nil //nolint:nilerr // recorded in PathErrors; keep walking
		}
		if d.Type()&fs.ModeSymlink != 0 {
			// Never follow or index symlinks: cycle-safe, and a link
			// cannot smuggle content past the deny-list.
			//
			// But a link the walk skipped is a prefix it did not look at, and
			// os.Lstat only declines to follow the *final* component — so a
			// directory replaced by a symlink leaves rows beneath it that
			// stat perfectly well through the link. Left out of coverage,
			// those rows are deleted the moment the link's target goes away,
			// which for a link to an external volume is every unplug.
			//
			// Recorded whatever the link points at, and without resolving it.
			// Asking whether it is a directory needs os.Stat, which fails
			// precisely when the target is missing — the case this exists for.
			// A link to a file costs one path in the set that no row should
			// occupy anyway, since the walk never indexes a symlink.
			//
			// Deny-listed links are skipped: an excluded tree holds no rows to
			// protect, and a $HOME with venvs, a Homebrew Cellar or a dotfiles
			// farm would otherwise put tens of thousands of paths in a set
			// that is held for the whole walk and consulted per catalog row.
			if unknown != nil && !excluded(path) {
				unknown[path] = true
			}
			return nil
		}
		if d.IsDir() {
			if excluded(path) {
				return fs.SkipDir // prune: nothing under it is statted
			}
			return nil
		}
		if !admissible(d.Name(), d.Type()) || excluded(path) {
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
	roots, _ := s.resolveRoots(res)
	return roots
}

// resolveRoots is roots plus the fact the deletion pass needs: whether every
// configured root resolved. An unresolved root is not merely absent from the
// returned set — catalog rows beneath it may be spelled in a way nothing in
// this pass can recognise, so scope pruning has to stand down (ADR 0016).
func (s *Scanner) resolveRoots(res *Result) (roots []string, allVerified bool) {
	excluded := s.excluded()
	allVerified = true

	// Every configured root is resolved, then the resolved set is normalized.
	// Deliberately not normalized first: dropping a root nested under another
	// *by its configured spelling* would skip resolving it, and a nested
	// entry that is a symlink can resolve somewhere else entirely
	// (~/vault/Sync → /Volumes/Ext/Sync). Dropped unresolved it never enters
	// the root set and never marks the pass unverified, so catalog rows under
	// its real location read as out of scope and are pruned — a root spelled
	// verbatim in paths.include reported as "no longer in the configured
	// paths". Cleaning and deduplicating identical spellings first is safe;
	// only the nesting rule has to wait.
	cleaned := make([]string, len(s.opts.Include))
	for i, root := range s.opts.Include {
		cleaned[i] = filepath.Clean(root)
	}
	slices.Sort(cleaned)
	resolved := make([]string, 0, len(cleaned))
	for _, root := range slices.Compact(cleaned) {
		root, keep, verified := canonicalRoot(root, res)
		if !verified {
			allVerified = false
		}
		if keep {
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
	return out, allVerified
}

// hasSymlinkAncestor reports whether path, or any directory above it, is a
// symlink. It is what separates "this root is simply not there" from "this
// root points somewhere that is not there" when EvalSymlinks answers ENOENT
// for both — and only the second can leave catalog rows spelled under a
// location this pass cannot name.
//
// A handful of lstats per unresolved root, and only on the ENOENT path.
func hasSymlinkAncestor(path string) bool {
	for p := path; ; {
		if info, err := os.Lstat(p); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}

// admissible reports whether a directory entry is the kind of thing the
// catalog holds: a regular file whose name says it carries text. One
// definition, called by the walk, by ScanPaths and by the deletion pass —
// three spellings of the same rule would drift, and the drift is silent in
// the worst direction. When M6 widens this for PDFs and office documents
// (issue #21), a reconcile that still believed the old rule would delete
// every newly-indexed binary document on its next pass as "replaced by
// something unindexable".
func admissible(name string, mode fs.FileMode) bool {
	return mode.IsRegular() && isTextFile(name)
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
	// simply no work to schedule, structurally (ADR 0015). For a restore
	// that holds for the life of the daemon process: the eager orphan sweep
	// spares terminal content precisely so an undo or a split-window rename
	// stays free, and only the startup sweep collects what is still
	// unreferenced by then (domain.SweepScope).
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
//
// verified reports whether the resolution actually happened. It is false for
// both failure modes, and the distinction matters only to the deletion pass:
// a root that did not resolve may hold catalog rows spelled under its
// *resolved* path, which the returned spelling does not cover. A dangling
// symlinked root is the dangerous shape — WalkDir lstats it, the symlink
// guard skips it, and nothing else in a scan would ever notice (ADR 0016).
func canonicalRoot(root string, res *Result) (resolved string, keep, verified bool) {
	resolved, err := filepath.EvalSymlinks(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// A missing root passes through so WalkDir reports it, and is
		// "verified" as long as no symlink is involved: the spelling returned
		// here is the one catalog rows under it already carry, so withinAny
		// covers them and scope pruning has nothing to get wrong.
		//
		// This matters because a missing root is routine, not exceptional —
		// ADR 0016's own remedy for the removable-volume risks is to give a
		// drive its own include root, which then ENOENTs on every scan while
		// it is unplugged. Suspending scope pruning for that would switch off
		// the "editing paths.include takes effect" behaviour permanently, and
		// config never requires an include root to exist.
		//
		// A dangling *symlinked* root is the dangerous shape, and the only
		// one: its rows are spelled under the resolved target, which nothing
		// in this pass can name.
		return pathutil.FoldDataVolume(pathutil.CanonicalCase(root)), true, !hasSymlinkAncestor(root)
	case err != nil:
		res.PathErrors = append(res.PathErrors, PathError{Path: root, Err: err})
		return "", false, false
	}
	// The data-volume firmlink is not a symlink, so EvalSymlinks leaves that
	// spelling of a root exactly as configured while the FSEvents adapter
	// folds it out of every event path. Folded here too, or the two would
	// never meet and every event under such a root would be ignored.
	//
	// Case is canonicalised too, and for the same reason: EvalSymlinks builds
	// its answer from the components it was given, so a root typed in the
	// wrong case resolves to the wrong case. Every comparison downstream is
	// byte-wise, so the spelling has to be right here or nowhere.
	return pathutil.FoldDataVolume(pathutil.CanonicalCase(resolved)), true, true
}

package discovery

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/bcrisp4/bsearch/internal/domain"
	"github.com/bcrisp4/bsearch/internal/pathutil"
)

// maxDeclineReports bounds how many distinct directories the reconcile will
// name when it cannot stat their contents. Enough to identify what is wrong;
// past that the count is the information.
const maxDeclineReports = 32

// coverage is what one completed walk can vouch for. Only Scan builds it,
// which is the point: the deletion pass is private and takes this, so
// "nothing deletes a row except on the evidence of a walk that just
// finished" is structural rather than a convention someone has to remember.
type coverage struct {
	// roots are the resolved include roots the walk covered.
	roots []string
	// unresolved records that some *configured* root failed to resolve this
	// pass — a different fact from a root that is deliberately excluded, and
	// the one that suspends scope pruning (see reconcileDeleted).
	unresolved bool
	// unknown are prefixes the walk cannot vouch for: directories it could
	// not read, roots it could not resolve, and remembered mount points that
	// are not mounted right now. A catalog row beneath one of these is left
	// strictly alone.
	unknown map[string]bool
}

// reconcileDeleted is the second phase of a walk: the walk found what exists,
// this finds what no longer does. It enumerates the catalog in path order and
// removes rows the filesystem has lost — closing the three gaps ADR 0013 left
// open (a file deleted while the daemon was down, a deletion lost to an event
// stream that overflowed, and a subtree that vanished without its own
// directory event).
//
// Deleting is the one thing here that destroys work, and it is asymmetric:
// removing a row is milliseconds, rebuilding what it referenced is
// battery-gated local inference over the corpus. So every step needs positive
// evidence and declines without it. Per row, the first guard that answers
// wins:
//
//  1. outside every root, or deny-listed → pruned, because a file bsearch is
//     no longer configured to index has vanished as far as the index is
//     concerned — unless a root failed to resolve this pass, when the same
//     rows are only counted. Scope is asked first because it is a question
//     about the configuration rather than the filesystem: an unreadable
//     directory or an absent volume has no bearing on it, and asked later a
//     de-scoped path under an unplugged drive could never be pruned at all;
//  2. beneath something the walk cannot vouch for → left alone, counted
//     Unverified, *without* even a stat, so a TCC-denied subtree is declined
//     for free and adds no second round of PathErrors — the walk already
//     reported the directory once;
//  3. stat says present, and what is there is still a regular indexable file
//     → kept. Present but replaced by a directory, a symlink or a non-text
//     file is a deletion as far as the index is concerned: the walk will
//     never index it again, so nothing could refresh the row;
//  4. stat fails with anything other than ENOENT → counted Unverified, never
//     deleted. EACCES, EIO, ESTALE and ELOOP all mean "cannot tell", and
//     "cannot tell" is never spent on a delete;
//  5. ENOENT → deleted, unless no volume evidence has ever been recorded, in
//     which case it too is counted Unverified.
//
// Rows are deleted individually rather than by prefix. A prefix delete is the
// right answer to an event, which names one path and cannot enumerate what was
// under it; here the catalog *is* that enumeration, so a vanished subtree
// arrives as N rows that each independently returned ENOENT and the blast
// radius of a wrong answer stays at one row.
func (s *Scanner) reconcileDeleted(ctx context.Context, cov coverage, res *Result) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	res.Reconciled = true
	if len(cov.roots) == 0 {
		// Nothing is in scope: every include root was excluded by the
		// deny-list, or none resolved, or the list is empty. Declining is
		// right — there is no walk behind this pass to license anything — but
		// declining in silence is not, so every row is counted and the reason
		// is named.
		res.DeclineReasons = append(res.DeclineReasons,
			"no include root is in scope — check paths.include and the exclude rules")
		return s.declineAll(ctx, res)
	}

	// A memory that cannot be read is a decline, never a fatal error: it is an
	// advisory, re-derivable cache, and failing the whole Scan over it would
	// stop LastScan advancing, leaving every cycle due for a full walk.
	//
	// A value that will never parse is a different thing from a database that
	// would not answer. The first can only be repaired by overwriting it, so
	// it is treated as no memory at all and the learn step below rewrites it;
	// the second is transient, and writing over it would replace the record of
	// an unplugged volume with a record that it does not exist.
	known, remembered, err := s.state.KnownMounts(ctx)
	memoryReadable := err == nil || errors.Is(err, domain.ErrScanStateCorrupt)
	switch {
	case errors.Is(err, domain.ErrScanStateCorrupt):
		res.DeclineReasons = append(res.DeclineReasons,
			"the remembered volume list was unreadable and has been rebuilt; deletions resume next scan")
		known, remembered = nil, false
	case err != nil:
		res.DeclineReasons = append(res.DeclineReasons,
			"the remembered volume list could not be read from the index database")
		known, remembered = nil, false
	}

	live, supported, err := s.mounts()
	// An empty answer from a platform that supports the question is not an
	// answer: "/" is always mounted, so zero entries means the call was
	// short-changed (a sandbox, a revoked entitlement) and every remembered
	// volume would read as ejected — including "/" itself.
	mountsReadable := err == nil && (!supported || len(live) > 0)
	if !mountsReadable {
		// Decline rather than proceed blind: an unmounted volume and a deleted
		// subtree look identical without the mount table, and only one of them
		// is recoverable.
		for _, root := range cov.roots {
			cov.unknown[root] = true
		}
		res.DeclineReasons = append(res.DeclineReasons,
			"the list of mounted volumes could not be read, so an unplugged drive cannot be told from a deletion")
		live = nil
	}
	if mountsReadable {
		for _, mount := range known {
			if !slices.Contains(live, mount) {
				// Remembered, and not mounted now. Everything beneath it is a
				// volume that went away, not a corpus that was deleted.
				cov.unknown[mount] = true
				res.Unmounted = append(res.Unmounted, mount)
			}
		}
	}

	// Learn before acting. A memory written after the deletions it was meant
	// to inform protects nothing on the pass that needed it.
	//
	// Only when BOTH sides are usable. With a memory that would not read,
	// `known` is empty because the read failed rather than because nothing is
	// remembered — writing the live table over it would replace the record of
	// an unplugged volume with a record that it does not exist, and the next
	// pass would delete everything on it. A transient read error must not be
	// able to destroy the evidence.
	// Captured before the learning below, which is the whole point: what
	// licenses a deletion is evidence recorded on an *earlier* pass. Reading
	// it afterwards would make the very first scan both learn the currently
	// mounted volumes and act on them, which is exactly the pre-existing
	// catalog with an unplugged drive that this exists to protect.
	blind := !remembered

	if mountsReadable && memoryReadable {
		next, err := s.learnMounts(ctx, cov, known, live, remembered)
		switch {
		case err != nil:
			// A write failure is recorded, never fatal — same reasoning as the
			// read. Returning it here would stop LastScan advancing, which
			// leaves every cycle due for a full walk for ever.
			res.DeclineReasons = append(res.DeclineReasons,
				"the observed volumes could not be recorded in the index database")
		default:
			known = next
		}
	}

	// blind means no volume evidence had ever been recorded when this pass
	// began, so an absent file and an absent volume are indistinguishable.
	// Deletions stand down; everything else proceeds.
	//
	// Deliberately narrower than declining the whole corpus. On a fresh
	// install every row was just written by this same walk, so nothing would
	// be deleted anyway — declining wholesale there raised the loudest alarm
	// in the product, with no reason to attach to it, for a daemon doing
	// exactly the right thing. What has to stand down is the destructive step,
	// and only that.
	//
	// This does NOT rescue a volume that was already detached the first time
	// the memory was written — it was never there to be observed. That case is
	// closed by giving an external volume its own entry in paths.include,
	// where a root that will not resolve declines on its own.

	// One deny-list match per row is the most expensive thing this pass does
	// besides the stat — prefix compares plus a basename glob per path
	// component, around 3 µs. Memoising on the parent directory does not help:
	// the port is a single opaque predicate over a whole path, so a memo miss
	// still has to run it in full, and a miss is the common case. At the
	// corpus scale DESIGN.md targets (~100k documents) this is a few hundred
	// milliseconds per walk, which the walk itself dwarfs.
	excluded := s.excluded()
	// occupied tracks which remembered mounts still have catalog rows beneath
	// them. Free to collect, since the pass enumerates every path anyway, and
	// it is what lets the remembered set self-prune: an entry with no rows
	// left is protecting nothing and can be forgotten.
	occupied := make(map[string]bool, len(known))
	// blindDeclines counts rows the missing volume evidence actually stood
	// down, so the reason is only recorded when it explains something. A fresh
	// install is blind and has nothing missing, so it must say nothing at all.
	blindDeclines := 0
	// declined collapses guard 4's reports to one per directory, and stops
	// growing at maxDeclineReports. Per-directory alone bounds nothing: a
	// stale network mount fails every stat in a corpus with a ten-to-one
	// file-to-directory ratio, which is still tens of thousands of entries
	// and PathError structs held for the whole scan. Unverified carries the
	// magnitude; the samples only have to identify the problem.
	declined := map[string]bool{}
	var vanished, outOfScope []string
	cursor := ""
	for {
		// Paged rather than read whole: memory stays bounded by the page, not
		// by the corpus. Safe to delete as we go because the cursor is a
		// value and every path deleted sorts at or before it.
		batch, err := s.store.ListPaths(ctx, cursor, flushEvery)
		if err != nil {
			// Not fatal. This file argues twice over that a store failure here
			// must not fail the Scan — it would stop LastScan advancing, and
			// leave every cycle due for a full walk for ever — and returning
			// the error straight out is exactly that mistake. Reconciled goes
			// false so the counts are not read as a measurement.
			res.Reconciled = false
			res.DeclineReasons = append(res.DeclineReasons,
				"the catalog could not be read to the end, so some deletions were not looked for")
			return nil //nolint:nilerr // recorded as a decline; failing the Scan would freeze LastScan
		}
		if len(batch) == 0 {
			break
		}
		cursor = batch[len(batch)-1]

		for _, path := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			for _, mount := range known {
				if pathutil.Within(path, mount) {
					occupied[mount] = true
				}
			}

			switch {
			case !withinAny(path, cov.roots) || excluded(path):
				// Scope is asked first, and deliberately before coverage: it
				// is a question about the configuration, not about the
				// filesystem, so an unreadable directory or an absent volume
				// has no bearing on it. Asked second, a de-scoped path under
				// an unplugged drive could never be pruned at all.
				if cov.unresolved {
					// A root that failed to resolve is dropped from the root
					// set, and rows under a *symlinked* root are stored under
					// the resolved spelling — so they would read as out of
					// scope rather than as unverified. Scope pruning is
					// suspended wholesale for the pass rather than trying to
					// match the two spellings back up: config-licensed
					// deletion is not worth a guess.
					res.Ignored++
					continue
				}
				outOfScope = append(outOfScope, path)
			case underAny(path, cov.unknown):
				res.Unverified++
			default:
				switch info, err := os.Lstat(path); {
				case err == nil && indexable(path, info):
					// Still there, and still something a walk would index.
				case err == nil:
					// Present, but replaced by a directory, a symlink or a
					// non-text file — `rm x.md && mkdir x.md`, or an `ln -sf`
					// in a dotfiles setup. The walk skips all of those, so it
					// can never refresh the row, and the old chunks and
					// vectors would go on answering searches for content that
					// is not there. The document at this path is gone in
					// every sense that matters to the index.
					vanished = append(vanished, path)
				case !errors.Is(err, fs.ErrNotExist):
					// "Cannot tell" — EACCES, EIO, ESTALE, ELOOP. Declined
					// like any other unverifiable row, and counted as one:
					// the gauge exists to say deletions are not being
					// noticed somewhere, and a decline it cannot see is a
					// silent skip. Reported once per directory, because a
					// whole subtree failing this way must not put a
					// PathError per row into the status sample and memory.
					res.Unverified++
					if dir := filepath.Dir(path); !declined[dir] && len(declined) < maxDeclineReports {
						declined[dir] = true
						res.PathErrors = append(res.PathErrors, PathError{Path: path, Err: err})
					}
				case blind:
					// Gone, and no volume evidence to say whether the file or
					// the filesystem left. Declined, and counted so the
					// decline is visible.
					res.Unverified++
					blindDeclines++
				default:
					vanished = append(vanished, path)
				}
			}
		}

		if err := s.flushDeletes(ctx, &vanished, &res.Deleted); err != nil {
			res.Reconciled = false
			res.DeclineReasons = append(res.DeclineReasons,
				"some rows could not be removed from the index database")
			return nil //nolint:nilerr // recorded as a decline; failing the Scan would freeze LastScan
		}
		if err := s.flushDeletes(ctx, &outOfScope, &res.Pruned); err != nil {
			res.Reconciled = false
			res.DeclineReasons = append(res.DeclineReasons,
				"some rows could not be removed from the index database")
			return nil //nolint:nilerr // recorded as a decline; failing the Scan would freeze LastScan
		}
		if len(batch) < flushEvery {
			break
		}
	}

	if blindDeclines > 0 {
		res.DeclineReasons = append(res.DeclineReasons,
			"no volumes had been recorded yet, so deletions stand down for this scan and resume on the next")
	}

	if !mountsReadable || !memoryReadable {
		// Nothing was learned about mounts this pass, so nothing is forgotten
		// either: a transient failure here must not quietly weaken the next
		// pass's evidence.
		return nil
	}
	if err := s.forgetEmptyMounts(ctx, known, live, occupied); err != nil {
		// Recorded, not fatal — as with every other write to this cache.
		res.DeclineReasons = append(res.DeclineReasons,
			"the observed volumes could not be recorded in the index database")
	}
	return nil
}

// declineAll counts every catalog row as unverified without touching one. It
// is what "no include root resolved" looks like from the reconcile's side: a
// corpus-wide decline that has to be a number rather than an early return,
// because a silent whole-corpus skip is indistinguishable from a healthy scan.
func (s *Scanner) declineAll(ctx context.Context, res *Result) error {
	cursor := ""
	for {
		batch, err := s.store.ListPaths(ctx, cursor, flushEvery)
		if err != nil {
			res.Reconciled = false
			return nil //nolint:nilerr // recorded via Reconciled; failing the Scan would freeze LastScan
		}
		if len(batch) == 0 {
			return nil
		}
		cursor = batch[len(batch)-1]
		res.Unverified += len(batch)
		if len(batch) < flushEvery {
			return nil
		}
	}
}

// flushDeletes commits one batch of doomed paths and empties the slice. The
// count folds into res only after the transaction commits, so the numbers a
// scan returns always describe rows that actually went — the same discipline
// flush applies to upserts.
func (s *Scanner) flushDeletes(ctx context.Context, paths *[]string, count *int) error {
	if len(*paths) == 0 {
		return nil
	}
	// removed is folded in even on failure. The store commits chunk by chunk
	// and reports how many rows actually went, so discarding the count on
	// error would report nothing removed while the catalog had genuinely
	// shrunk — and would leave the orphan sweep unarmed for content whose
	// last path is gone.
	removed, err := s.store.DeleteByPaths(ctx, *paths)
	*count += removed
	*paths = (*paths)[:0]
	return err
}

// learnMounts unions the mount points visible right now, inside or containing
// a root, into the remembered set and returns it. Purely additive: this runs
// before any deletion, and dropping an entry here would remove protection in
// the same pass that was supposed to gain it.
//
// The set accumulates rather than tracking the current mount table, and that
// is the whole point. A drive unplugged before the daemon started is invisible
// to a snapshot taken this pass; remembering it from when it *was* there is
// what survives a reboot with the drive detached.
// remembered says whether anything has been written before. When it is false
// the write happens even if the set is unchanged, because what is being
// recorded is the *observation*, not the value: off macOS the mount table is
// always empty, so a value-only comparison would never write, "remembered"
// would never become true, and the pass would decline deletions for ever —
// the opposite of the documented behaviour that every other guard still
// applies there.
func (s *Scanner) learnMounts(ctx context.Context, cov coverage, known, live []string, remembered bool) ([]string, error) {
	next := slices.Clone(known)
	for _, mount := range live {
		if inScope(mount, cov.roots) {
			next = append(next, mount)
		}
	}
	next = sortedSet(next)
	if remembered && slices.Equal(next, known) {
		return known, nil
	}
	if err := s.state.SetKnownMounts(ctx, next); err != nil {
		return known, err
	}
	return next, nil
}

// forgetEmptyMounts drops remembered entries with no catalog rows left
// beneath them. Free to determine, since the pass enumerates every path
// anyway, and it is what stops a permanently retired volume protecting an
// empty prefix — and declining deletions under it — for ever.
//
// Only safe after the enumeration completed: occupied is evidence of absence,
// so a pass that stopped early would forget volumes it simply never reached.
func (s *Scanner) forgetEmptyMounts(ctx context.Context, known, live []string, occupied map[string]bool) error {
	next := make([]string, 0, len(known))
	for _, mount := range known {
		// Kept if it still protects rows, OR if it is mounted right now —
		// otherwise this undoes what learnMounts just did on every scan where
		// an in-scope mount holds no indexable files ("/" on most machines),
		// and the pair oscillate forever at two write transactions a cycle.
		if occupied[mount] || slices.Contains(live, mount) {
			next = append(next, mount)
		}
	}
	if slices.Equal(next, known) {
		return nil
	}
	return s.state.SetKnownMounts(ctx, next)
}

// sortedSet sorts and dedupes mount points — deliberately not normalizeRoots,
// which drops entries nested under another. Nested mounts are exactly what has
// to stay distinct here: on macOS $HOME is under /System/Volumes/Data, which
// is under /, and a volume mounted inside a root is the case the whole
// mechanism exists for.
func sortedSet(mounts []string) []string {
	slices.Sort(mounts)
	return slices.Compact(mounts)
}

// indexable reports whether what is at path is still the kind of thing a walk
// would put in the catalog. Mirrors walkTree's own admission rules, so the
// two cannot drift into a row the reconcile keeps and the walk will never
// refresh.
func indexable(path string, info fs.FileInfo) bool {
	return admissible(filepath.Base(path), info.Mode())
}

// inScope reports whether a mount point is one this corpus cares about:
// mounted inside a root, or holding a root. "/" always matches the second
// arm and is always mounted, so remembering it costs a set entry and declines
// nothing.
func inScope(mount string, roots []string) bool {
	return slices.ContainsFunc(roots, func(root string) bool {
		return pathutil.Within(mount, root) || pathutil.Within(root, mount)
	})
}

// underAny reports whether path lies STRICTLY beneath any member of set, by
// walking path's ancestors from its parent up. Bounded by path depth rather
// than set size, and — unlike a binary search over a sorted prefix list — it
// cannot be tricked by a sibling that sorts between a directory and its
// contents ("/a." falls between "/a" and "/a/b").
//
// Strictly beneath, because every member is a directory or a skipped symlink
// and never a file row: a directory the walk was refused holds no row at its
// own path, so nothing is lost — while a *file* replaced by a symlink is a
// member whose own row must still be judged, and deleted, since the walk can
// never refresh it.
func underAny(path string, set map[string]bool) bool {
	if len(set) == 0 {
		return false
	}
	for p := filepath.Dir(path); ; {
		if set[p] {
			return true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}

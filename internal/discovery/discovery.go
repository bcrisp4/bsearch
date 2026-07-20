// Package discovery walks the configured include paths and keeps the
// catalog's view of the filesystem current: new and changed files become
// state=discovered rows for the pipeline, unchanged files are skipped
// cheaply (size/mtime, then content hash), and renamed files keep their
// document IDs. Per-path problems (permission errors — the macOS TCC
// constraint — unreadable files, missing roots) are collected in the
// result, never silently swallowed. See DESIGN.md (Indexing pipeline and
// queue; Change detection; doc_id Closed issue).
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
	excluded := s.opts.Excluded
	if excluded == nil {
		excluded = func(string) bool { return false }
	}

	// Canonicalize roots first, then normalize again: a resolved root
	// may land on or under another include root (symlinked root, or an
	// aliased ancestor like macOS /var → /private/var), and only the
	// second pass can see that overlap.
	resolved := make([]string, 0, len(s.opts.Include))
	for _, root := range normalizeRoots(s.opts.Include) {
		if root, ok := canonicalRoot(root, &res); ok {
			resolved = append(resolved, root)
		}
	}

	for _, root := range normalizeRoots(resolved) {
		if excluded(root) {
			res.PathErrors = append(res.PathErrors, PathError{Path: root, Err: ErrRootExcluded})
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
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
			return s.processFile(ctx, path, info, &res)
		})
		if err != nil {
			return res, err
		}
	}
	return res, nil
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
		if err := s.store.UpdateDocumentStat(ctx, existing.ID, info.Size(), info.ModTime()); err != nil {
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
		return root, true
	case err != nil:
		res.PathErrors = append(res.PathErrors, PathError{Path: root, Err: err})
		return "", false
	}
	return resolved, true
}

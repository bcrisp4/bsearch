// Package fsevents is the macOS FSEvents adapter for domain.Watcher: the
// near-real-time half of change detection (DESIGN.md: Change detection;
// issue #13). It reports paths, and nothing else — whether a path is
// interesting, whether its content actually changed, and what to do about it
// are discovery's decisions, made with a stat and a hash.
//
// Two properties are worth stating because they shape the code:
//
// The event stream is advisory. FSEvents coalesces, can report a path that
// did not really change, and under pressure will say only "something under
// here moved" (kFSEventStreamEventFlagMustScanSubDirs). Every one of those
// cases is honest here rather than papered over — an overflow becomes
// WatchBatch.Rescan, which asks the caller for a full walk.
//
// Losing events costs a walk, never correctness. The periodic scan is the
// backstop, so this adapter is free to degrade to Rescan whenever it cannot
// be precise, and does so rather than dropping paths quietly.
package fsevents

import (
	"errors"

	"github.com/bcrisp4/bsearch/internal/domain"
)

var _ domain.Watcher = (*Watcher)(nil)

// ErrUnsupported means this platform has no FSEvents. The daemon treats it
// as "scan-only", not as a failure: a periodic walk is a working mode, and
// DESIGN.md's macOS-first constraint means a Linux port supplies its own
// adapter (inotify) rather than this one growing a second implementation.
var ErrUnsupported = errors.New("filesystem watching is only supported on macOS")

// Watcher subscribes to FSEvents. The zero value is ready; use New.
type Watcher struct{}

// New returns an FSEvents watcher.
func New() *Watcher { return &Watcher{} }

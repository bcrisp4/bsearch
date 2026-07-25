//go:build !darwin

package fsevents

import (
	"context"

	"github.com/bcrisp4/bsearch/internal/domain"
)

// Watch reports that this platform has no FSEvents. The caller falls back to
// the periodic scan (DESIGN.md: macOS-first; a Linux port brings an inotify
// adapter of its own).
func (w *Watcher) Watch(context.Context, []string) (<-chan domain.WatchBatch, error) {
	return nil, ErrUnsupported
}

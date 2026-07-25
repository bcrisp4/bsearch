//go:build darwin

package fsevents

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	// Aliased because this package shares its name: ours is the adapter,
	// fse is the cgo binding it wraps.
	fse "github.com/fsnotify/fsevents"

	"github.com/bcrisp4/bsearch/internal/domain"
	"github.com/bcrisp4/bsearch/internal/pathutil"
)

const (
	// latency is how long FSEvents holds an event before delivering it, so
	// the kernel coalesces a burst into one callback. Free temporal
	// batching; the caller debounces again on top for the mid-write case,
	// so this only has to be long enough to be worth the syscall.
	latency = time.Second
	// eventBuffer sizes the channel the cgo callback writes to. The
	// callback runs on a dispatch queue and blocks on that send, so a
	// buffer is what keeps a momentary stall on our side out of the
	// kernel's way.
	eventBuffer = 64
	// outBuffer sizes deliveries to the caller. Small: the caller's own
	// debounce window is the real buffer.
	outBuffer = 8
	// maxPendingPaths bounds a batch held back by a slow consumer. Past it
	// the batch collapses to Rescan — the list has stopped being cheaper
	// than a walk, and unbounded growth would be the one way this adapter
	// could hurt a machine it is supposed to stay out of the way of.
	maxPendingPaths = 8192
)

// rescanFlags are the event flags that mean the accompanying paths are not
// the whole story. Overflow (the kernel's own log wrapped, or our stream
// fell too far behind) and ground-moved events (a volume arrived or went
// away, a watch root was replaced) all have the same answer: stop trusting
// the list and walk.
const rescanFlags = fse.MustScanSubDirs | fse.KernelDropped | fse.UserDropped |
	fse.RootChanged | fse.Mount | fse.Unmount | fse.EventIDsWrapped

// Watch subscribes to changes under roots. See domain.Watcher.
func (w *Watcher) Watch(ctx context.Context, roots []string) (<-chan domain.WatchBatch, error) {
	if len(roots) == 0 {
		return nil, errors.New("fsevents: no roots to watch")
	}
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("fsevents: watch root %q is not absolute", root)
		}
	}

	es := &fse.EventStream{
		Paths:  slices.Clone(roots),
		Events: make(chan []fse.Event, eventBuffer),
		// FileEvents gives per-file paths; without it every event names a
		// directory and the caller has to re-walk to find out what changed.
		// NoDefer delivers the first event of a burst at the start of the
		// latency window rather than the end, which is the difference
		// between a saved note being seen now and in a second. WatchRoot
		// reports a root itself being moved or replaced, which is a Rescan.
		Flags:   fse.FileEvents | fse.NoDefer | fse.WatchRoot,
		Latency: latency,
	}
	if err := es.Start(); err != nil {
		return nil, fmt.Errorf("fsevents: watch %d root(s): %w", len(roots), err)
	}

	out := make(chan domain.WatchBatch, outBuffer)
	go pump(ctx, es, out)
	return out, nil
}

// pump moves events from the stream to the caller until ctx is done.
//
// It holds at most one batch back. Delivering a held batch and reading the
// stream are arms of the same select, so a caller that stops reading slows
// deliveries but never blocks the cgo callback — which matters more than it
// looks: that callback runs on a dispatch queue, and a blocked one stalls
// the whole stream rather than just this goroutine.
func pump(ctx context.Context, es *fse.EventStream, out chan<- domain.WatchBatch) {
	defer close(out)
	defer shutdown(es)

	var pending domain.WatchBatch
	for {
		// A nil channel blocks forever in a select, which is exactly the
		// behaviour wanted when there is nothing to deliver.
		var deliver chan<- domain.WatchBatch
		if pending.Rescan || len(pending.Paths) > 0 {
			deliver = out
		}
		select {
		case <-ctx.Done():
			return
		case deliver <- pending:
			pending = domain.WatchBatch{}
		case events, ok := <-es.Events:
			if !ok {
				return
			}
			absorb(&pending, events)
		}
	}
}

// shutdown stops the stream without deadlocking against its own callback.
//
// The callback delivers by sending on es.Events, so if that channel is full
// when we stop reading it, the callback is parked inside the dispatch queue
// that Stop has to tear down. Stop therefore runs on its own goroutine while
// this one keeps draining, until Stop reports the stream is gone and no
// further callback can arrive.
func shutdown(es *fse.EventStream) {
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		es.Stop()
	}()
	for {
		select {
		case <-stopped:
			return
		case <-es.Events:
		}
	}
}

// absorb folds one callback's events into the batch being assembled.
func absorb(batch *domain.WatchBatch, events []fse.Event) {
	for _, ev := range events {
		if ev.Flags&rescanFlags != 0 {
			// The paths collected so far are not wrong, but they are no
			// longer sufficient, and carrying them would suggest otherwise.
			batch.Rescan = true
			batch.Paths = nil
			continue
		}
		if batch.Rescan {
			continue
		}
		if p := normalize(ev.Path); p != "" {
			batch.Paths = append(batch.Paths, p)
		}
	}
	if !batch.Rescan && len(batch.Paths) > maxPendingPaths {
		batch.Rescan = true
		batch.Paths = nil
	}
}

// normalize cleans an event path and folds the data-volume spelling of it
// onto the one the caller's roots are written in. An empty result means the
// path is not usable and should be dropped.
func normalize(p string) string {
	if !filepath.IsAbs(p) {
		// Device is left zero on the stream, so paths are absolute. Anything
		// else is a shape we did not ask for and cannot resolve.
		return ""
	}
	// Shared with discovery's root canonicalization on purpose: the fold only
	// does any good if both sides of the eventual comparison agree on it.
	return pathutil.FoldDataVolume(filepath.Clean(p))
}

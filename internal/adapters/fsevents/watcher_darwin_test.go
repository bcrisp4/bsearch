//go:build darwin

package fsevents

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	fse "github.com/fsnotify/fsevents"

	"github.com/bcrisp4/bsearch/internal/domain"
	"github.com/bcrisp4/bsearch/internal/pathutil"
)

// eventDeadline is how long a test waits for the kernel. Generous on
// purpose: FSEvents holds events for the stream's latency window and the
// machine may be busy, and a watcher test that flakes gets deleted rather
// than fixed.
const eventDeadline = 20 * time.Second

// watchTemp subscribes to a temp directory and returns its canonical path
// plus the event channel. The path is resolved because macOS temp
// directories live behind the /var → /private/var alias and FSEvents
// reports the resolved spelling.
func watchTemp(t *testing.T) (string, <-chan domain.WatchBatch) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	events, err := New().Watch(ctx, []string{dir})
	if err != nil {
		t.Fatalf("Watch(%s): %v", dir, err)
	}
	return dir, events
}

// awaitPath waits for a batch naming want. Batches are coalesced and may
// carry unrelated paths, so this drains until it sees the one asked for.
func awaitPath(t *testing.T, events <-chan domain.WatchBatch, want string) {
	t.Helper()
	deadline := time.After(eventDeadline)
	for {
		select {
		case batch, ok := <-events:
			if !ok {
				t.Fatalf("event channel closed before %s arrived", want)
			}
			if batch.Rescan {
				// Legitimate on a busy machine, and not what this test is
				// about; keep waiting for the specific path.
				continue
			}
			for _, path := range batch.Paths {
				if path == want {
					return
				}
			}
		case <-deadline:
			t.Fatalf("no event for %s within %v", want, eventDeadline)
		}
	}
}

func TestWatchReportsCreateModifyDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("waits on the kernel's event stream")
	}
	dir, events := watchTemp(t)
	path := filepath.Join(dir, "note.md")

	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitPath(t, events, path)

	if err := os.WriteFile(path, []byte("hello again"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitPath(t, events, path)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	awaitPath(t, events, path)
}

// A rename must deliver both spellings, because that is what lets the
// caller recognise one document rather than a deletion and a creation.
func TestWatchReportsBothSidesOfARename(t *testing.T) {
	if testing.Short() {
		t.Skip("waits on the kernel's event stream")
	}
	dir, events := watchTemp(t)
	old := filepath.Join(dir, "old.md")
	renamed := filepath.Join(dir, "new.md")

	if err := os.WriteFile(old, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitPath(t, events, old)
	if err := os.Rename(old, renamed); err != nil {
		t.Fatal(err)
	}
	awaitPath(t, events, renamed)
}

// Cancelling the context must stop the stream and close the channel — the
// shutdown that has to survive a callback parked on a full buffer.
func TestWatchStopsOnContextCancel(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	events, err := New().Watch(ctx, []string{dir})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	cancel()
	deadline := time.After(eventDeadline)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("event channel was not closed after cancellation")
		}
	}
}

func TestWatchRejectsUnusableRoots(t *testing.T) {
	for name, roots := range map[string][]string{
		"none":     nil,
		"relative": {"notes"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New().Watch(t.Context(), roots); err == nil {
				t.Errorf("Watch(%v) = nil error, want a rejection", roots)
			}
		})
	}
}

func TestAbsorbTranslatesFlags(t *testing.T) {
	const dir = "/Users/ben/notes"

	t.Run("collects item paths", func(t *testing.T) {
		var batch domain.WatchBatch
		absorb(&batch, []fse.Event{
			{Path: dir + "/a.md", Flags: fse.ItemCreated | fse.ItemIsFile},
			{Path: dir + "/b.md", Flags: fse.ItemModified | fse.ItemIsFile},
		})
		if batch.Rescan {
			t.Error("ordinary item events asked for a rescan")
		}
		if len(batch.Paths) != 2 {
			t.Errorf("Paths = %v, want both", batch.Paths)
		}
	})

	// Every flag that means "the list is not the whole story" has the same
	// answer, and the paths collected so far go with it: keeping them would
	// suggest they were sufficient.
	for name, flag := range map[string]fse.EventFlags{
		"overflow":       fse.MustScanSubDirs,
		"kernel dropped": fse.KernelDropped,
		"user dropped":   fse.UserDropped,
		"root changed":   fse.RootChanged,
		"mount":          fse.Mount,
		"unmount":        fse.Unmount,
		"ids wrapped":    fse.EventIDsWrapped,
	} {
		t.Run(name+" forces a rescan", func(t *testing.T) {
			var batch domain.WatchBatch
			absorb(&batch, []fse.Event{
				{Path: dir + "/a.md", Flags: fse.ItemCreated},
				{Path: dir, Flags: flag},
				{Path: dir + "/b.md", Flags: fse.ItemCreated},
			})
			if !batch.Rescan {
				t.Error("Rescan = false")
			}
			if len(batch.Paths) != 0 {
				t.Errorf("Paths = %v, want none once a walk is needed", batch.Paths)
			}
		})
	}

	t.Run("collapses when the caller falls behind", func(t *testing.T) {
		events := make([]fse.Event, maxPendingPaths+1)
		for i := range events {
			events[i] = fse.Event{Path: dir + "/" + itoa(i) + ".md", Flags: fse.ItemCreated}
		}
		var batch domain.WatchBatch
		absorb(&batch, events)
		if !batch.Rescan || len(batch.Paths) != 0 {
			t.Errorf("batch = %+v, want a collapsed rescan", batch)
		}
	})
}

func TestNormalize(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"absolute":             {"/Users/ben/a.md", "/Users/ben/a.md"},
		"uncleaned":            {"/Users/ben/./sub/../a.md", "/Users/ben/a.md"},
		"trailing separator":   {"/Users/ben/sub/", "/Users/ben/sub"},
		"data volume firmlink": {pathutil.DataVolumeRoot + "/Users/ben/a.md", "/Users/ben/a.md"},
		// The firmlink root itself, and a directory that merely starts with
		// its name, are not the same thing and must survive intact.
		"data volume root": {pathutil.DataVolumeRoot, pathutil.DataVolumeRoot},
		"similar prefix":   {pathutil.DataVolumeRoot + "Extra/a.md", pathutil.DataVolumeRoot + "Extra/a.md"},
		"relative dropped": {"notes/a.md", ""},
		"empty dropped":    {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := normalize(tc.in); got != tc.want {
				t.Errorf("normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

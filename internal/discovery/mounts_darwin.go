package discovery

import (
	"fmt"
	"syscall"

	"github.com/bcrisp4/bsearch/internal/pathutil"
)

// mntNoWait is MNT_NOWAIT from <sys/mount.h>, not named in the stdlib
// syscall package on darwin. MNT_WAIT is 1 and is the wrong one: see below.
const mntNoWait = 2

// mountPoints returns the mount point of every filesystem mounted right now,
// folded through pathutil.FoldDataVolume so the spellings meet the ones roots
// and catalog paths are stored in — $HOME lives on /System/Volumes/Data, so
// without the fold the mount table and the corpus would never compare equal.
//
// supported is always true here: darwin can answer the question, so an empty
// result means the call was short-changed rather than that nothing is mounted.
//
// MNT_NOWAIT, never MNT_WAIT: the waiting form asks every filesystem to
// refresh its statistics first, which blocks on an unresponsive network mount
// — and an unresponsive network mount is one of the situations this exists to
// notice. Stale numbers are fine; the only field read is the mount point.
func mountPoints() ([]string, bool, error) {
	// Getfsstat fills the buffer and returns how many entries it wrote — it
	// does NOT report that more existed, so a buffer that came back exactly
	// full is indistinguishable from a truncated read. Volumes can be mounted
	// between the counting call and the reading one, and a truncated read
	// here silently drops a volume's protection, so grow and retry rather
	// than trusting one round.
	var buf []syscall.Statfs_t
	for size := 0; ; {
		n, err := syscall.Getfsstat(nil, mntNoWait)
		if err != nil {
			return nil, true, fmt.Errorf("count mounted filesystems: %w", err)
		}
		size = max(n+8, size*2)
		buf = make([]syscall.Statfs_t, size)
		n, err = syscall.Getfsstat(buf, mntNoWait)
		if err != nil {
			return nil, true, fmt.Errorf("read mounted filesystems: %w", err)
		}
		if n < len(buf) {
			buf = buf[:n]
			break
		}
	}

	mounts := make([]string, 0, len(buf))
	for _, fs := range buf {
		if on := cString(fs.Mntonname[:]); on != "" {
			mounts = append(mounts, pathutil.FoldDataVolume(on))
		}
	}
	return mounts, true, nil
}

// cString reads a NUL-terminated C string out of a fixed-size struct field.
// The field is []int8 because C's char is signed here; reinterpreting each
// element as a byte is the whole job, and a negative one is simply a byte at
// or above 0x80 — an ordinary UTF-8 continuation byte in a path, not an
// overflow.
func cString(raw []int8) string {
	b := make([]byte, 0, len(raw))
	for _, c := range raw {
		if c == 0 {
			break
		}
		b = append(b, byte(c)) //nolint:gosec // G115: signed char reinterpreted as the byte it always was
	}
	return string(b)
}

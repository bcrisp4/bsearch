package discovery

import (
	"io/fs"
	"syscall"
)

// sfDataless is SF_DATALESS from <sys/stat.h>: the file is an iCloud
// (Optimize Storage) placeholder whose content is not on disk. Not named
// in the stdlib syscall package.
const sfDataless = 0x40000000

// isDataless reports whether info describes a dataless placeholder file.
// Reading one would trigger a cloud download — indexing must never do
// that (DESIGN.md: enqueue).
func isDataless(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Flags&sfDataless != 0
}

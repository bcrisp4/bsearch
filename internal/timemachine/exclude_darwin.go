//go:build darwin

package timemachine

/*
#cgo LDFLAGS: -framework CoreServices
#include <CoreServices/CoreServices.h>
#include <string.h>

// Shims so the Go side never holds a CFURLRef and never has to get its
// lifetime right. Each creates the URL, makes the one call, and releases.
//
// The final argument to CFURLCreateFromFileSystemRepresentation is
// isDirectory: it decides whether the URL carries a trailing slash, and the
// caller stats the path rather than guessing, because a URL that disagrees
// with the filesystem is the kind of mistake that fails silently.

static OSStatus bsearch_tm_set(const char *path, int isDir, int exclude) {
	CFURLRef url = CFURLCreateFromFileSystemRepresentation(
		NULL, (const UInt8 *)path, (CFIndex)strlen(path), isDir ? true : false);
	if (url == NULL) {
		return coreFoundationUnknownErr;
	}
	OSStatus status = CSBackupSetItemExcluded(url, exclude ? true : false, false);
	CFRelease(url);
	return status;
}

static OSStatus bsearch_tm_get(const char *path, int isDir, int *excluded) {
	CFURLRef url = CFURLCreateFromFileSystemRepresentation(
		NULL, (const UInt8 *)path, (CFIndex)strlen(path), isDir ? true : false);
	if (url == NULL) {
		return coreFoundationUnknownErr;
	}
	*excluded = CSBackupIsItemExcluded(url, NULL) ? 1 : 0;
	CFRelease(url);
	return noErr;
}
*/
import "C"

import (
	"fmt"
	"os"
	"unsafe"
)

// Exclude marks path as excluded from Time Machine backups. Directories are
// excluded with everything beneath them, which is why the daemon excludes its
// data directory rather than the database file: the -wal and -shm files do not
// exist yet at startup, and neither will whatever a later milestone puts
// there.
//
// The exclusion is "sticky" (CSBackupSetItemExcluded's excludeByPath is false):
// it lives in an extended attribute on the item, so it needs no privileges,
// and it follows the directory if it moves. The path-based alternative writes
// to Time Machine's own preferences and wants root — a daemon running as the
// user cannot set it, and should not be able to.
//
// Calling it on an already-excluded path succeeds and changes nothing.
func Exclude(path string) error {
	isDir, err := statDir(path)
	if err != nil {
		return err
	}
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))

	if status := C.bsearch_tm_set(cPath, cBool(isDir), 1); status != C.noErr {
		return fmt.Errorf("exclude %s from Time Machine: CSBackupSetItemExcluded returned OSStatus %d", path, int(status))
	}
	return nil
}

// Excluded reports whether path is currently excluded from Time Machine
// backups. It exists so the exclusion can be verified without anything having
// to know how CoreServices stores it.
func Excluded(path string) (bool, error) {
	isDir, err := statDir(path)
	if err != nil {
		return false, err
	}
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))

	var excluded C.int
	if status := C.bsearch_tm_get(cPath, cBool(isDir), &excluded); status != C.noErr {
		return false, fmt.Errorf("read Time Machine exclusion for %s: CSBackupIsItemExcluded returned OSStatus %d", path, int(status))
	}
	return excluded != 0, nil
}

// statDir answers the isDirectory question the URL needs, and turns a missing
// path into os.ErrNotExist here rather than into an OSStatus number further
// down. Lstat, not Stat: a symlink is excluded as itself, since excluding what
// it happens to point at is not what the caller named.
func statDir(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

func cBool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

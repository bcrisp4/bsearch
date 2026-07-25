// Package pathutil holds small filesystem-path helpers shared by the
// config deny-list matcher and discovery's root handling, so subtle
// boundary logic lives in exactly one place.
package pathutil

import (
	"os"
	"strings"
)

// Within reports whether path equals prefix or lies beneath it. Both
// arguments must be absolute, cleaned paths. The component boundary is
// respected — /foo does not contain /foobar — and a prefix of "/"
// contains every absolute path.
//
// Comparison is byte-wise and case-sensitive; on a case-insensitive
// filesystem (default APFS) differently-cased spellings of the same
// directory do not match.
func Within(path, prefix string) bool {
	if path == prefix {
		return true
	}
	if prefix == string(os.PathSeparator) {
		return strings.HasPrefix(path, prefix)
	}
	return strings.HasPrefix(path, prefix+string(os.PathSeparator))
}

// DataVolumeRoot is the firmlink mount of the writable volume on macOS
// 10.15+, where /Users is really /System/Volumes/Data/Users.
const DataVolumeRoot = "/System/Volumes/Data"

// FoldDataVolume rewrites a path spelled through the macOS data-volume
// firmlink to the spelling everything else uses. Other paths are returned
// unchanged, as is the firmlink root itself — folding that would leave "",
// and a directory that merely starts with the same letters is a different
// directory.
//
// It lives here, and not in the FSEvents adapter that first needed it,
// because it only does any good applied to both sides of a comparison.
// Folding event paths alone and leaving watch roots in the other spelling
// makes every event fall outside every root — a watcher that subscribes,
// delivers, and indexes nothing.
func FoldDataVolume(path string) string {
	rest := strings.TrimPrefix(path, DataVolumeRoot)
	if rest == path || !strings.HasPrefix(rest, string(os.PathSeparator)) {
		return path
	}
	return rest
}

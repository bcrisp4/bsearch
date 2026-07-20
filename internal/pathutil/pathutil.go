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

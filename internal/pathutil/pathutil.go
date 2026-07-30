// Package pathutil holds small filesystem-path helpers shared by the
// config deny-list matcher and discovery's root handling, so subtle
// boundary logic lives in exactly one place.
package pathutil

import (
	"os"
	"path/filepath"
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

// CanonicalCase returns path with every component spelled the way the
// filesystem stores it. On a case-insensitive volume (default APFS)
// `~/Notes` and `~/notes` open the same directory but are different strings,
// and bsearch compares path strings in several places that must agree:
// include roots, the catalog rows a walk writes from those roots, the mount
// table, and the paths FSEvents delivers.
//
// Getting one of them in the wrong case is silent and total. A root typed
// `~/Notes` against an on-disk `~/notes` walks and indexes perfectly well —
// every row stored under the typed spelling — while the mount table reports
// the real one, so no volume is ever recognised as in scope and the
// unplugged-drive protection is disabled outright (ADR 0016). It is also why
// a watch root in the wrong case subscribes, delivers, and matches nothing
// (issue #64).
//
// Resolving it here, once, at the point a root is resolved, is the fix that
// keeps every comparison byte-wise. Making Within case-insensitive instead
// would be wrong on a case-sensitive volume, where two spellings really are
// two directories.
//
// Best effort: a component that cannot be listed (permissions, or it does not
// exist) is kept as written along with everything below it, which is exactly
// today's behaviour. An exact match always wins over a case-folded one, so a
// case-sensitive volume holding both `Notes` and `notes` is unaffected.
func CanonicalCase(path string) string {
	sep := string(os.PathSeparator)
	if !strings.HasPrefix(path, sep) {
		return path
	}
	parts := strings.Split(strings.TrimPrefix(path, sep), sep)
	out := ""
	for i, want := range parts {
		if want == "" {
			continue
		}
		parent := out
		if parent == "" {
			parent = sep
		}
		real, ok := onDiskName(parent, want)
		if !ok {
			return out + sep + strings.Join(parts[i:], sep)
		}
		out += sep + real
	}
	if out == "" {
		return sep
	}
	return out
}

// onDiskName finds how dir spells want. An exact match wins outright.
//
// A case-folded match is accepted only when the filesystem itself resolves
// the spelling it was given, which is the practical test for "this directory
// is case-insensitive". Without that check, a case-SENSITIVE volume holding
// `Notes` would answer `NOTES` with `Notes` — quietly rewriting an include
// root to a different directory that happens to exist, where the honest
// outcome is that the configured one does not. Two spellings really are two
// directories there, and correcting one into the other is the exact mistake
// this whole function exists to avoid making at comparison time.
func onDiskName(dir, want string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	folded := ""
	for _, e := range entries {
		name := e.Name()
		if name == want {
			return name, true
		}
		if folded == "" && strings.EqualFold(name, want) {
			folded = name
		}
	}
	if folded == "" {
		return "", false
	}
	if _, err := os.Lstat(filepath.Join(dir, want)); err != nil {
		// The typed spelling does not resolve, so this filesystem is case
		// -sensitive and `want` genuinely is not there. Leave it as written.
		return "", false
	}
	return folded, true
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

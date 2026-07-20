//go:build !darwin

package discovery

import "io/fs"

// isDataless is a no-op off macOS: dataless placeholders are an
// iCloud/APFS concept.
func isDataless(fs.FileInfo) bool {
	return false
}

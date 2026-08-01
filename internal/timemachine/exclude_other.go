//go:build !darwin

package timemachine

// Exclude reports that this platform has no Time Machine. The daemon logs the
// reason and carries on: bsearch is macOS-first (DESIGN.md: Non-goals), and a
// backup mechanism that does not exist is not a startup failure.
func Exclude(string) error {
	return ErrUnsupported
}

// Excluded reports that this platform has no Time Machine, for the same
// reason. It answers false rather than defaulting to true, so a caller that
// checks before acting is told the truth: nothing here is excluded, because
// nothing here is backed up by Time Machine in the first place.
func Excluded(string) (bool, error) {
	return false, ErrUnsupported
}

//go:build !darwin

package discovery

// mountPoints reports no mounts off macOS. Reading the mount table needs a
// per-platform syscall, and bsearch is macOS-first (DESIGN.md: Non-goals);
// a Linux port adds the /proc/self/mountinfo reader here.
//
// supported is false, which is what distinguishes "this platform cannot
// answer" from "the answer is that nothing is mounted". Darwin always reports
// at least "/", so an empty result there means the call was short-changed and
// must not be read as every volume having been ejected.
//
// The consequence is stated where it bites: with no mounts to remember, the
// deletion pass declines nothing on volume grounds, so an unmounted volume
// under an include root reads as a deletion. Every other guard still applies.
func mountPoints() ([]string, bool, error) {
	return nil, false, nil
}

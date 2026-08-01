// Package timemachine marks paths as excluded from Time Machine backups
// (DESIGN.md: Data retention — Backups; ADR 0017).
//
// The daemon excludes its data directory on every start. The index is derived
// data with nothing to preserve across a rebuild, and it concentrates the full
// text, summaries and embeddings of everything indexed into one file — which
// is Security threat 1, "the index is a honeypot", seen from the backup side.
// Excluding it is a mechanism, not a recommendation: it closes the leaked-
// backup half of that threat without asking the user to configure anything.
//
// This is not a port. Ports exist for the I/O boundaries domain logic depends
// on; this is a startup side-effect that no domain code knows about, so it is
// called directly from `bsearch serve` and nothing mocks it.
package timemachine

import "errors"

// ErrUnsupported means this platform has no Time Machine. The daemon logs it
// and carries on — the exclusion is best-effort everywhere, and off macOS
// there is nothing to exclude from (DESIGN.md: Non-goals — not cross-platform
// in v1).
var ErrUnsupported = errors.New("excluding paths from Time Machine backups is only supported on macOS")

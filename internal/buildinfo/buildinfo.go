// Package buildinfo carries version metadata stamped into the binary at build
// time (see the Makefile and, later, .goreleaser.yaml).
package buildinfo

// Version is the release version. Overridden at link time with
// -X github.com/bcrisp4/bsearch/internal/buildinfo.Version=<version>.
var Version = "dev"

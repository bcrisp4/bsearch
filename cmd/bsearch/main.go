// Command bsearch is the bsearch CLI and daemon: one binary, with `serve`
// running the indexing daemon and the remaining subcommands acting as clients
// over its unix socket. See DESIGN.md for the full design and the milestone
// order in which these subcommands land.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/bcrisp4/bsearch/internal/buildinfo"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "bsearch:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		_, err := fmt.Fprintf(out, "bsearch %s\n", buildinfo.Version)
		return err
	}
	return errors.New("no subcommands implemented yet — see DESIGN.md (milestone M1)")
}

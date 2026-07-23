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

// run dispatches subcommands. Each subcommand owns a stdlib flag.FlagSet
// (ADR 0006); dispatch itself stays a plain switch.
func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: bsearch <index|search|eval|version>")
	}
	switch args[0] {
	case "version", "--version":
		if len(args) != 1 {
			return fmt.Errorf("version takes no arguments (got %q)", args[1])
		}
		_, err := fmt.Fprintf(out, "bsearch %s\n", buildinfo.Version)
		return err
	case "index":
		return runIndex(args[1:], out)
	case "search":
		return runSearch(args[1:], out)
	case "eval":
		return runEval(args[1:], out)
	default:
		return fmt.Errorf("unknown command %q (usage: bsearch <index|search|eval|version>)", args[0])
	}
}

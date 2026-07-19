package main

import (
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version"} {
		t.Run(arg, func(t *testing.T) {
			var out strings.Builder
			if err := run([]string{arg}, &out); err != nil {
				t.Fatalf("run(%q) = %v, want nil", arg, err)
			}
			if got := out.String(); !strings.HasPrefix(got, "bsearch ") {
				t.Errorf("run(%q) printed %q, want a %q prefix", arg, got, "bsearch ")
			}
		})
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out strings.Builder
	if err := run([]string{"search", "heat pump"}, &out); err == nil {
		t.Fatal("run(search) = nil, want an error while subcommands are unimplemented")
	}
	if out.Len() != 0 {
		t.Errorf("run(search) wrote %q to stdout, want nothing", out.String())
	}
}

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

func TestRunVersionRejectsTrailingArgs(t *testing.T) {
	var out strings.Builder
	if err := run([]string{"version", "index"}, &out); err == nil {
		t.Fatal("run(version index) = nil, want error for trailing args")
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out strings.Builder
	err := run([]string{"frobnicate"}, &out)
	if err == nil {
		t.Fatal("run(frobnicate) = nil, want unknown-command error")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("run(frobnicate) = %v, want unknown-command error", err)
	}
	if out.Len() != 0 {
		t.Errorf("run(frobnicate) wrote %q to stdout, want nothing", out.String())
	}
}

//go:build darwin

package timemachine_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bcrisp4/bsearch/internal/timemachine"
)

// A fresh directory is not excluded, Exclude makes it so, and saying it twice
// is not an error. Idempotence is the property the daemon depends on: it sets
// the exclusion on every start, so the second start onwards must be a no-op
// rather than a warning in the log.
func TestExcludeDirectoryRoundTrip(t *testing.T) {
	dir := t.TempDir()

	excluded, err := timemachine.Excluded(dir)
	if err != nil {
		t.Fatalf("Excluded on a fresh directory: %v", err)
	}
	if excluded {
		t.Fatal("a fresh directory reports as already excluded")
	}

	if err := timemachine.Exclude(dir); err != nil {
		t.Fatalf("Exclude: %v", err)
	}
	if excluded, err = timemachine.Excluded(dir); err != nil {
		t.Fatalf("Excluded after Exclude: %v", err)
	}
	if !excluded {
		t.Fatal("directory is not excluded after Exclude")
	}

	if err := timemachine.Exclude(dir); err != nil {
		t.Fatalf("Exclude is not idempotent: %v", err)
	}
	if excluded, err = timemachine.Excluded(dir); err != nil {
		t.Fatalf("Excluded after the second Exclude: %v", err)
	}
	if !excluded {
		t.Fatal("the second Exclude cleared the exclusion")
	}
}

// Files work too. The daemon excludes a directory, but nothing in the API
// should depend on that — and the isDirectory flag the CoreServices call wants
// is exactly the kind of detail that goes wrong silently.
func TestExcludeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bsearch.db")
	if err := os.WriteFile(path, []byte("not really a database"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := timemachine.Exclude(path); err != nil {
		t.Fatalf("Exclude: %v", err)
	}
	excluded, err := timemachine.Excluded(path)
	if err != nil {
		t.Fatalf("Excluded: %v", err)
	}
	if !excluded {
		t.Fatal("file is not excluded after Exclude")
	}
}

// A path that does not exist is reported as such rather than as an opaque
// OSStatus. The daemon only logs this, so the message is the whole of what a
// user gets to act on.
func TestExcludeMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")

	err := timemachine.Exclude(missing)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Exclude on a missing path = %v, want os.ErrNotExist", err)
	}
	if _, err := timemachine.Excluded(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Excluded on a missing path = %v, want os.ErrNotExist", err)
	}
}

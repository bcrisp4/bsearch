package pathutil_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bcrisp4/bsearch/internal/pathutil"
)

func TestWithin(t *testing.T) {
	tests := []struct {
		path, prefix string
		want         bool
	}{
		{path: "/a/b", prefix: "/a/b", want: true},   // equal
		{path: "/a/b/c", prefix: "/a/b", want: true}, // beneath
		{path: "/a/bc", prefix: "/a/b", want: false}, // boundary
		{path: "/foobar", prefix: "/foo", want: false},
		{path: "/a", prefix: "/a/b", want: false}, // reversed
		{path: "/", prefix: "/", want: true},
		{path: "/anything", prefix: "/", want: true}, // root contains all
		{path: "/a/B", prefix: "/a/b", want: false},  // case-sensitive
	}
	for _, tt := range tests {
		t.Run(tt.path+" in "+tt.prefix, func(t *testing.T) {
			if got := pathutil.Within(tt.path, tt.prefix); got != tt.want {
				t.Errorf("Within(%q, %q) = %v, want %v", tt.path, tt.prefix, got, tt.want)
			}
		})
	}
}

func TestFoldDataVolume(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"firmlink spelling": {pathutil.DataVolumeRoot + "/Users/ben/a.md", "/Users/ben/a.md"},
		"already folded":    {"/Users/ben/a.md", "/Users/ben/a.md"},
		// The firmlink root itself would fold to "", and a directory that
		// merely starts with the same letters is a different directory.
		"the root itself": {pathutil.DataVolumeRoot, pathutil.DataVolumeRoot},
		"similar prefix":  {pathutil.DataVolumeRoot + "Extra/a.md", pathutil.DataVolumeRoot + "Extra/a.md"},
		"unrelated":       {"/tmp/x", "/tmp/x"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := pathutil.FoldDataVolume(tc.in); got != tc.want {
				t.Errorf("FoldDataVolume(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// caseInsensitive reports whether dir's filesystem folds case. macOS APFS
// does by default, but it can be formatted case-sensitive and Linux is, so
// the tests that need folding say so rather than assuming.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "CaseProbe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe) //nolint:errcheck // best effort in a temp dir
	_, err := os.Stat(filepath.Join(dir, "caseprobe"))
	return err == nil
}

func TestCanonicalCaseFixesASpellingTheFilesystemDoesNotUse(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !caseInsensitive(t, resolved) {
		t.Skip("filesystem is case-sensitive; the two spellings are two directories")
	}
	if err := os.MkdirAll(filepath.Join(resolved, "notes", "work"), 0o755); err != nil {
		t.Fatal(err)
	}

	typed := filepath.Join(resolved, "Notes", "WORK")
	got := pathutil.CanonicalCase(typed)
	want := filepath.Join(resolved, "notes", "work")
	if got != want {
		t.Errorf("pathutil.CanonicalCase(%q) = %q, want %q", typed, got, want)
	}
	// And the whole point: the corrected spelling now compares equal to what
	// the filesystem reports, which the typed one does not.
	if pathutil.Within(typed, want) {
		t.Error("the typed spelling already matched, so this test proves nothing")
	}
	if !pathutil.Within(got, want) {
		t.Errorf("Within(%q, %q) = false — canonicalising did not make them agree", got, want)
	}
}

func TestCanonicalCaseLeavesUnresolvableComponentsAlone(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A missing root is ordinary — an unplugged drive with its own include
	// entry. It must come back as written, not mangled or emptied.
	missing := filepath.Join(resolved, "nope", "deeper")
	if got := pathutil.CanonicalCase(missing); got != missing {
		t.Errorf("pathutil.CanonicalCase(%q) = %q, want it unchanged", missing, got)
	}
}

func TestCanonicalCasePrefersAnExactMatch(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if caseInsensitive(t, resolved) {
		t.Skip("need a case-sensitive filesystem to hold both spellings")
	}
	for _, name := range []string{"Notes", "notes"} {
		if err := os.Mkdir(filepath.Join(resolved, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	exact := filepath.Join(resolved, "Notes")
	if got := pathutil.CanonicalCase(exact); got != exact {
		t.Errorf("pathutil.CanonicalCase(%q) = %q, want the exact spelling kept", exact, got)
	}

	// And a spelling that exists in neither case comes back untouched. Folding
	// it onto whichever entry looks similar would rewrite a configured root to
	// a different directory — the mistake this function exists to avoid, made
	// at resolution time instead of comparison time.
	//
	// (Verified manually against a case-sensitive APFS image: before the
	// resolve-check in onDiskName, "…/NOTES" came back as "…/Notes".)
	absent := filepath.Join(resolved, "NOTES")
	if got := pathutil.CanonicalCase(absent); got != absent {
		t.Errorf("pathutil.CanonicalCase(%q) = %q, want it unchanged — a different directory here", absent, got)
	}
}

func TestCanonicalCaseLeavesRelativePathsAlone(t *testing.T) {
	for _, p := range []string{"", "relative/path", "."} {
		if got := pathutil.CanonicalCase(p); got != p {
			t.Errorf("pathutil.CanonicalCase(%q) = %q, want it unchanged", p, got)
		}
	}
}

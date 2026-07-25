package pathutil_test

import (
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

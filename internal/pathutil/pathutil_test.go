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

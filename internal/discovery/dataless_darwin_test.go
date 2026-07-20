package discovery

import (
	"io/fs"
	"syscall"
	"testing"
	"time"
)

// fakeInfo is an fs.FileInfo whose Sys() returns a fabricated Stat_t, so
// the dataless decode is testable without a real iCloud placeholder.
type fakeInfo struct {
	sys any
}

func (fakeInfo) Name() string       { return "x.md" }
func (fakeInfo) Size() int64        { return 0 }
func (fakeInfo) Mode() fs.FileMode  { return 0 }
func (fakeInfo) ModTime() time.Time { return time.Time{} }
func (fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any         { return f.sys }

func TestIsDataless(t *testing.T) {
	tests := []struct {
		name string
		info fs.FileInfo
		want bool
	}{
		{name: "dataless flag set", info: fakeInfo{sys: &syscall.Stat_t{Flags: sfDataless}}, want: true},
		{name: "flag among others", info: fakeInfo{sys: &syscall.Stat_t{Flags: sfDataless | 0x1}}, want: true},
		{name: "no flags", info: fakeInfo{sys: &syscall.Stat_t{}}, want: false},
		{name: "non-stat sys", info: fakeInfo{sys: 42}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDataless(tt.info); got != tt.want {
				t.Errorf("isDataless = %v, want %v", got, tt.want)
			}
		})
	}
}

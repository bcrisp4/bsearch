package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/config"
)

// writeConfig writes body to a config.toml inside a fresh temp dir and
// returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// setHome points $HOME at a temp dir so tilde expansion and defaults are
// deterministic, and returns it.
func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	home := setHome(t)

	cfg, err := config.Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err != nil {
		t.Fatalf("Load(missing) = %v, want nil (defaults)", err)
	}

	if want := []string{home}; !slices.Equal(cfg.Paths.Include, want) {
		t.Errorf("Paths.Include = %v, want %v", cfg.Paths.Include, want)
	}
	if len(cfg.Paths.Exclude) != 0 {
		t.Errorf("Paths.Exclude = %v, want empty", cfg.Paths.Exclude)
	}
	if want := "http://localhost:1234/v1"; cfg.Inference.Endpoint != want {
		t.Errorf("Inference.Endpoint = %q, want %q", cfg.Inference.Endpoint, want)
	}
	if cfg.Inference.EmbeddingModel != "" || cfg.Inference.SummaryModel != "" {
		t.Errorf("model defaults = %q/%q, want empty (M2 bake-off decides)",
			cfg.Inference.EmbeddingModel, cfg.Inference.SummaryModel)
	}
	if want := "http://localhost:18000"; cfg.Converter.Endpoint != want {
		t.Errorf("Converter.Endpoint = %q, want %q", cfg.Converter.Endpoint, want)
	}
	if want := filepath.Join(home, ".config/bsearch/bscribe-token"); cfg.Converter.TokenFile != want {
		t.Errorf("Converter.TokenFile = %q, want %q", cfg.Converter.TokenFile, want)
	}
	if want := (config.Interval{Duration: 5 * time.Minute}); cfg.Power.AC.IndexInterval != want {
		t.Errorf("Power.AC.IndexInterval = %+v, want %+v", cfg.Power.AC.IndexInterval, want)
	}
	if want := (config.Interval{Duration: 60 * time.Minute}); cfg.Power.Battery.IndexInterval != want {
		t.Errorf("Power.Battery.IndexInterval = %+v, want %+v", cfg.Power.Battery.IndexInterval, want)
	}
}

func TestLoadPartialFileMergesOverDefaults(t *testing.T) {
	home := setHome(t)
	path := writeConfig(t, `
[paths]
include = ["~/Notes"]
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if want := []string{filepath.Join(home, "Notes")}; !slices.Equal(cfg.Paths.Include, want) {
		t.Errorf("Paths.Include = %v, want %v", cfg.Paths.Include, want)
	}
	// Untouched sections keep their defaults.
	if want := "http://localhost:1234/v1"; cfg.Inference.Endpoint != want {
		t.Errorf("Inference.Endpoint = %q, want default %q", cfg.Inference.Endpoint, want)
	}
	if want := (config.Interval{Duration: 5 * time.Minute}); cfg.Power.AC.IndexInterval != want {
		t.Errorf("Power.AC.IndexInterval = %+v, want default %+v", cfg.Power.AC.IndexInterval, want)
	}
}

// TestLoadSampleConfig parses the full sample config from DESIGN.md
// field-for-field.
func TestLoadSampleConfig(t *testing.T) {
	home := setHome(t)
	path := writeConfig(t, `
[paths]
include = ["~"]
exclude = ["~/Archive/old-junk"]

[inference]
endpoint        = "http://localhost:1234/v1"
embedding_model = ""
summary_model   = ""

[converter]
endpoint   = "http://localhost:18000"
token_file = "~/.config/bsearch/bscribe-token"

[power]
ac.index_interval      = "5m"
battery.index_interval = "60m"
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if want := []string{home}; !slices.Equal(cfg.Paths.Include, want) {
		t.Errorf("Paths.Include = %v, want %v", cfg.Paths.Include, want)
	}
	if want := []string{filepath.Join(home, "Archive/old-junk")}; !slices.Equal(cfg.Paths.Exclude, want) {
		t.Errorf("Paths.Exclude = %v, want %v", cfg.Paths.Exclude, want)
	}
	if want := (config.Interval{Duration: 60 * time.Minute}); cfg.Power.Battery.IndexInterval != want {
		t.Errorf("Power.Battery.IndexInterval = %+v, want %+v", cfg.Power.Battery.IndexInterval, want)
	}
}

func TestLoadDeferInterval(t *testing.T) {
	setHome(t)
	path := writeConfig(t, `
[power]
battery.index_interval = "defer"
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if want := (config.Interval{Defer: true}); cfg.Power.Battery.IndexInterval != want {
		t.Errorf("Power.Battery.IndexInterval = %+v, want %+v", cfg.Power.Battery.IndexInterval, want)
	}
	if want := (config.Interval{Duration: 5 * time.Minute}); cfg.Power.AC.IndexInterval != want {
		t.Errorf("Power.AC.IndexInterval = %+v, want default %+v", cfg.Power.AC.IndexInterval, want)
	}
}

func TestLoadErrors(t *testing.T) {
	setHome(t)
	tests := []struct {
		name    string
		body    string
		wantSub string // substring the error must contain
	}{
		{
			name:    "unknown key",
			body:    "[inference]\nmodel = \"x\"\n",
			wantSub: "inference.model",
		},
		{
			name:    "unknown table",
			body:    "[inferense]\nendpoint = \"http://localhost:1234/v1\"\n",
			wantSub: "inferense",
		},
		{
			name:    "malformed toml",
			body:    "[paths\ninclude = [",
			wantSub: "",
		},
		{
			name:    "bad interval",
			body:    "[power]\nac.index_interval = \"banana\"\n",
			wantSub: "banana",
		},
		{
			name:    "negative interval",
			body:    "[power]\nac.index_interval = \"-5m\"\n",
			wantSub: "positive",
		},
		{
			name:    "zero interval",
			body:    "[power]\nac.index_interval = \"0s\"\n",
			wantSub: "positive",
		},
		{
			name:    "unparseable endpoint",
			body:    "[inference]\nendpoint = \"://nope\"\n",
			wantSub: "endpoint",
		},
		{
			name:    "non-http endpoint",
			body:    "[converter]\nendpoint = \"ftp://localhost:18000\"\n",
			wantSub: "endpoint",
		},
		{
			name:    "empty include",
			body:    "[paths]\ninclude = []\n",
			wantSub: "include",
		},
		{
			name:    "relative include",
			body:    "[paths]\ninclude = [\"notes\"]\n",
			wantSub: "notes",
		},
		{
			name:    "relative exclude with separator",
			body:    "[paths]\nexclude = [\"Archive/junk\"]\n",
			wantSub: "Archive/junk",
		},
		{
			name:    "malformed exclude glob",
			body:    "[paths]\nexclude = [\"[oops\"]\n",
			wantSub: "[oops",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(writeConfig(t, tt.body))
			if err == nil {
				t.Fatal("Load = nil error, want error")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("Load error %q does not mention %q", err, tt.wantSub)
			}
		})
	}
}

func TestLoadEmptyPathErrors(t *testing.T) {
	setHome(t)
	// os.ReadFile("") reports ErrNotExist, which must not be mistaken for
	// a missing config file: an empty path means the caller failed to
	// resolve one (e.g. DefaultPath() with no home dir) — fail loudly.
	if _, err := config.Load(""); err == nil {
		t.Error("Load(\"\") = nil error, want error")
	}
}

func TestLoadUnreadableFileErrors(t *testing.T) {
	setHome(t)
	// A directory at the config path is an I/O error, not "missing" —
	// must not be silently treated as first-run defaults.
	dir := t.TempDir()
	if _, err := config.Load(dir); err == nil {
		t.Error("Load(directory) = nil error, want error")
	}
}

func TestIntervalUnmarshalText(t *testing.T) {
	tests := []struct {
		in      string
		want    config.Interval
		wantErr bool
	}{
		{in: "5m", want: config.Interval{Duration: 5 * time.Minute}},
		{in: "1h30m", want: config.Interval{Duration: 90 * time.Minute}},
		{in: "defer", want: config.Interval{Defer: true}},
		{in: "banana", wantErr: true},
		{in: "", wantErr: true},
		{in: "-1m", wantErr: true},
		{in: "0s", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			var got config.Interval
			err := got.UnmarshalText([]byte(tt.in))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("UnmarshalText(%q) = nil error, want error", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalText(%q) = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("UnmarshalText(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestExcludeRulesBuiltins(t *testing.T) {
	home := setHome(t)

	cfg, err := config.Load(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	rules := cfg.ExcludeRules()

	for _, want := range []string{
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".gnupg"),
		filepath.Join(home, "Library"),
		filepath.Join(home, ".cache"),
		filepath.Join(home, "Library/Application Support/bsearch"),
	} {
		if !slices.Contains(rules.Prefixes, want) {
			t.Errorf("ExcludeRules().Prefixes missing %q", want)
		}
	}
	for _, want := range []string{
		".git", ".hg", ".svn", "node_modules", ".venv", "venv", "vendor",
		"__pycache__", ".env", ".env.*", "*.pem", "*.key", "id_rsa*",
		"id_ed25519*", "*.keychain*",
	} {
		if !slices.Contains(rules.Patterns, want) {
			t.Errorf("ExcludeRules().Patterns missing %q", want)
		}
	}
}

func TestExcludeRulesUserEntries(t *testing.T) {
	home := setHome(t)
	path := writeConfig(t, `
[paths]
exclude = ["~/Archive/old-junk", "/Volumes/scratch/", "*.bak"]
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	rules := cfg.ExcludeRules()

	// Absolute / tilde entries become prefixes; bare names become patterns.
	for _, want := range []string{filepath.Join(home, "Archive/old-junk"), "/Volumes/scratch"} {
		if !slices.Contains(rules.Prefixes, want) {
			t.Errorf("ExcludeRules().Prefixes missing user entry %q", want)
		}
	}
	// A trailing slash must not produce a dead prefix that never matches
	// cleaned walk paths.
	if !rules.Match("/Volumes/scratch/secret.md") {
		t.Error("Match missed a path under a user exclude prefix")
	}
	if !slices.Contains(rules.Patterns, "*.bak") {
		t.Errorf("ExcludeRules().Patterns missing user entry %q", "*.bak")
	}
	// Built-ins survive user additions.
	if !slices.Contains(rules.Patterns, ".git") {
		t.Error("ExcludeRules().Patterns lost built-in .git")
	}
}

func TestExcludeSetMatch(t *testing.T) {
	rules := config.ExcludeSet{
		Prefixes: []string{"/home/u/.ssh", "/foo"},
		Patterns: []string{".git", "node_modules", ".env.*", "*.pem"},
	}
	tests := []struct {
		path string
		want bool
	}{
		{path: "/home/u/.ssh", want: true},                // path equals prefix
		{path: "/home/u/.ssh/id_rsa", want: true},         // under prefix
		{path: "/home/u/.sshfs/x.md", want: false},        // prefix boundary
		{path: "/foobar/x.md", want: false},               // prefix boundary
		{path: "/foo/x.md", want: true},                   // under short prefix
		{path: "/home/u/proj/.git/config", want: true},    // pattern dir component
		{path: "/home/u/a/node_modules/b/c", want: true},  // pattern deep in tree
		{path: "/home/u/proj/.env.local", want: true},     // glob pattern
		{path: "/home/u/certs/server.pem", want: true},    // suffix glob
		{path: "/home/u/notes/todo.md", want: false},      // no match
		{path: "/home/u/gitstuff/readme.md", want: false}, // pattern must match whole component
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := rules.Match(tt.path); got != tt.want {
				t.Errorf("Match(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestDefaultPath(t *testing.T) {
	home := setHome(t)

	t.Run("xdg set", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "/custom/xdg")
		if got, want := config.DefaultPath(), "/custom/xdg/bsearch/config.toml"; got != want {
			t.Errorf("DefaultPath() = %q, want %q", got, want)
		}
	})
	t.Run("xdg unset", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "")
		if got, want := config.DefaultPath(), filepath.Join(home, ".config/bsearch/config.toml"); got != want {
			t.Errorf("DefaultPath() = %q, want %q", got, want)
		}
	})
}

func TestLoadErrorsAreNotFsNotExist(t *testing.T) {
	setHome(t)
	// Guard the missing-file-is-fine contract: a *parse* failure must not
	// masquerade as fs.ErrNotExist and get swallowed by callers.
	_, err := config.Load(writeConfig(t, "[paths\n"))
	if err == nil {
		t.Fatal("Load(malformed) = nil, want error")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load(malformed) error %v wraps ErrNotExist; must not", err)
	}
}

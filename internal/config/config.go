// Package config loads bsearch's TOML configuration
// (~/.config/bsearch/config.toml) and supplies built-in defaults and the
// privacy deny-list. See DESIGN.md (Sample config, Privacy).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/bcrisp4/bsearch/internal/domain"
	"github.com/bcrisp4/bsearch/internal/pathutil"
)

// Built-in deny-list (DESIGN.md Privacy). User [paths].exclude entries
// extend it; exclusions always win over includes. Browser profiles
// (Chrome, Firefox, Safari) live under ~/Library and are covered by that
// prefix.
var (
	// denyPrefixes are tilde-relative directory prefixes, expanded at Load.
	// ~/Library/Application Support/bsearch is redundant with ~/Library
	// today but listed explicitly so it survives a future Mail carve-in.
	denyPrefixes = []string{
		"~/.ssh",
		"~/.gnupg",
		"~/Library",
		"~/.cache",
		"~/Library/Application Support/bsearch",
	}

	// denyPatterns are basename globs matched anywhere in the tree:
	// VCS internals, dependency/cache dirs, and secret-bearing files.
	denyPatterns = []string{
		".git", ".hg", ".svn",
		"node_modules", ".venv", "venv", "vendor", "__pycache__",
		".env", ".env.*",
		"*.pem", "*.key", "id_rsa*", "id_ed25519*", "*.keychain*",
	}
)

// Interval is a positive duration or the literal "defer" (skip work
// entirely, e.g. heavy indexing on battery).
type Interval struct {
	Duration time.Duration
	Defer    bool
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (i *Interval) UnmarshalText(text []byte) error {
	s := string(text)
	if s == "defer" {
		*i = Interval{Defer: true}
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("interval %q: want a duration (\"5m\") or \"defer\"", s)
	}
	if d <= 0 {
		return fmt.Errorf("interval %q: must be positive", s)
	}
	*i = Interval{Duration: d}
	return nil
}

// Paths configures which directories are indexed.
type Paths struct {
	Include []string `toml:"include"`
	Exclude []string `toml:"exclude"`
}

// Inference configures the OpenAI-compatible inference endpoint. Model
// names default to empty until the M2 bake-off records defaults.
//
// The template/ceiling fields are per-field overrides of the built-in
// per-model registry (internal/embedding); empty/zero means "use the
// registry default for the configured model". They exist for models the
// registry doesn't know — BYO inference must never require registry
// membership.
type Inference struct {
	Endpoint       string `toml:"endpoint"`
	EmbeddingModel string `toml:"embedding_model"`
	SummaryModel   string `toml:"summary_model"`
	// QueryTemplate must contain {q} when set.
	QueryTemplate string `toml:"query_template"`
	// PassageTemplate must contain {d} when set; {t} optionally marks a
	// title slot filled by the chunk's heading-path breadcrumb.
	PassageTemplate string `toml:"passage_template"`
	// InputCeilingTokens is the embedding model's input limit; 0 defers
	// to the registry (unlimited for unknown models).
	InputCeilingTokens int `toml:"input_ceiling_tokens"`
}

// Converter configures the bscribe document-conversion service.
type Converter struct {
	Endpoint  string `toml:"endpoint"`
	TokenFile string `toml:"token_file"`
}

// PowerPolicy is the indexing policy for one power state.
type PowerPolicy struct {
	IndexInterval Interval `toml:"index_interval"`
}

// Power configures power-aware indexing behaviour.
type Power struct {
	AC      PowerPolicy `toml:"ac"`
	Battery PowerPolicy `toml:"battery"`
}

// Config is the loaded bsearch configuration. Construct with Load; the
// zero value has no defaults and an empty deny-list.
type Config struct {
	Paths     Paths     `toml:"paths"`
	Inference Inference `toml:"inference"`
	Converter Converter `toml:"converter"`
	Power     Power     `toml:"power"`

	// excludePrefixes holds the built-in deny prefixes, tilde-expanded
	// at Load so ExcludeRules never has to handle expansion failure.
	excludePrefixes []string
}

// ExcludeSet is the merged deny-list: built-in exclusions plus the user's
// [paths].exclude entries.
type ExcludeSet struct {
	// Prefixes are absolute path prefixes: the path and everything under
	// it are excluded.
	Prefixes []string
	// Patterns are basename globs (path.Match syntax) matched against
	// every file and directory name anywhere in the tree.
	Patterns []string
}

// Match reports whether p is excluded: equal to or under a deny prefix,
// or any path component matches a deny pattern. p must be an absolute,
// cleaned path, as produced by walking an absolute root. Matching is
// case-sensitive: on a case-insensitive filesystem (default APFS) a
// differently-cased spelling of a denied directory is not matched, so
// include roots should be written in on-disk casing (tilde-expanded
// paths always are).
func (e ExcludeSet) Match(p string) bool {
	for _, prefix := range e.Prefixes {
		if pathutil.Within(p, prefix) {
			return true
		}
	}
	for component := range strings.SplitSeq(strings.TrimPrefix(p, string(os.PathSeparator)), string(os.PathSeparator)) {
		for _, pattern := range e.Patterns {
			// Patterns are validated at Load; a bad built-in would be
			// a programming error and path.Match then reports no match.
			if ok, _ := path.Match(pattern, component); ok {
				return true
			}
		}
	}
	return false
}

// DefaultPath returns the config file location:
// $XDG_CONFIG_HOME/bsearch/config.toml, or ~/.config/bsearch/config.toml.
func DefaultPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "bsearch", "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "bsearch", "config.toml")
}

// Load reads the config file at path. A missing file is not an error:
// built-in defaults are returned (first-run experience). Unknown keys,
// malformed TOML, and invalid values are errors.
func Load(path string) (*Config, error) {
	// os.ReadFile("") reports ErrNotExist, which would silently pass for
	// first-run defaults; an empty path means the caller failed to resolve
	// one (e.g. DefaultPath() with no home directory).
	if path == "" {
		return nil, errors.New("config path is empty")
	}
	cfg := Config{
		Paths: Paths{Include: []string{"~"}},
		Inference: Inference{
			Endpoint: "http://localhost:1234/v1",
		},
		Converter: Converter{
			Endpoint:  "http://localhost:18000",
			TokenFile: "~/.config/bsearch/bscribe-token", // #nosec G101 -- path to a token file, not a credential
		},
		Power: Power{
			AC:      PowerPolicy{IndexInterval: Interval{Duration: 5 * time.Minute}},
			Battery: PowerPolicy{IndexInterval: Interval{Duration: 60 * time.Minute}},
		},
	}

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// First run: no file, pure defaults.
	case err != nil:
		return nil, fmt.Errorf("read config: %w", err)
	default:
		md, err := toml.Decode(string(data), &cfg)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if undecoded := md.Undecoded(); len(undecoded) > 0 {
			keys := make([]string, len(undecoded))
			for n, key := range undecoded {
				keys[n] = key.String()
			}
			return nil, fmt.Errorf("%s: unknown key(s): %s", path, strings.Join(keys, ", "))
		}
	}

	if err := cfg.expandPaths(); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

// ExcludeRules returns the merged deny-list. User exclude entries that
// are absolute (or were tilde-expanded to absolute) become prefixes;
// bare names become basename patterns. Absolute entries are cleaned
// here: a raw entry with a trailing slash or dot segment would
// otherwise never prefix-match a cleaned walk path — a silently dead
// deny rule.
func (c *Config) ExcludeRules() ExcludeSet {
	prefixes := slices.Clone(c.excludePrefixes)
	patterns := slices.Clone(denyPatterns)
	for _, entry := range c.Paths.Exclude {
		if strings.HasPrefix(entry, "/") {
			prefixes = append(prefixes, filepath.Clean(entry))
		} else {
			patterns = append(patterns, entry)
		}
	}
	return ExcludeSet{Prefixes: prefixes, Patterns: patterns}
}

// expandPaths tilde-expands every path-valued field, and materialises the
// built-in deny prefixes.
func (c *Config) expandPaths() error {
	for _, list := range [][]string{c.Paths.Include, c.Paths.Exclude} {
		for n, p := range list {
			expanded, err := expandTilde(p)
			if err != nil {
				return err
			}
			list[n] = expanded
		}
	}
	tokenFile, err := expandTilde(c.Converter.TokenFile)
	if err != nil {
		return err
	}
	c.Converter.TokenFile = tokenFile

	c.excludePrefixes = make([]string, len(denyPrefixes))
	for n, p := range denyPrefixes {
		if c.excludePrefixes[n], err = expandTilde(p); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) validate() error {
	if len(c.Paths.Include) == 0 {
		return errors.New("paths.include: must list at least one directory")
	}
	for _, p := range c.Paths.Include {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("paths.include: %q is not an absolute path (start with ~/ or /)", p)
		}
	}
	// Non-absolute exclude entries are basename globs; one with a path
	// separator matches nothing, silently, and a malformed glob would
	// silently never match — reject both at load.
	for _, e := range c.Paths.Exclude {
		if filepath.IsAbs(e) {
			continue
		}
		if strings.ContainsRune(e, '/') {
			return fmt.Errorf("paths.exclude: %q is neither an absolute path nor a basename glob", e)
		}
		if _, err := path.Match(e, "probe"); err != nil {
			return fmt.Errorf("paths.exclude: %q is not a valid glob pattern", e)
		}
	}
	if err := validateEndpoint("inference.endpoint", c.Inference.Endpoint); err != nil {
		return err
	}
	// A template without its placeholder would embed the same constant
	// string for every input — silent, total recall loss. An over-reserve
	// passage template composes past the input ceiling on full-size
	// chunks. Reject both at load.
	if t := c.Inference.QueryTemplate; t != "" && !strings.Contains(t, domain.PlaceholderQuery) {
		return fmt.Errorf("inference.query_template: %q does not contain %s", t, domain.PlaceholderQuery)
	}
	if t := c.Inference.PassageTemplate; t != "" && !strings.Contains(t, domain.PlaceholderPassage) {
		return fmt.Errorf("inference.passage_template: %q does not contain %s", t, domain.PlaceholderPassage)
	}
	if n := len(c.Inference.PassageTemplate); n > domain.TemplateReserveBytes {
		return fmt.Errorf("inference.passage_template: %d bytes exceeds the %d-byte reserve budgeted by the chunker",
			n, domain.TemplateReserveBytes)
	}
	if n := c.Inference.InputCeilingTokens; n < 0 {
		return fmt.Errorf("inference.input_ceiling_tokens: %d is negative", n)
	}
	return validateEndpoint("converter.endpoint", c.Converter.Endpoint)
}

func validateEndpoint(key, endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s: %q is not an http(s) URL", key, endpoint)
	}
	return nil
}

// expandTilde replaces a leading "~" or "~/" with the home directory.
// Anything else (absolute paths, glob patterns) passes through untouched.
func expandTilde(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %q: %w", p, err)
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}

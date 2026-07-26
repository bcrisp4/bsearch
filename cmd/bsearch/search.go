package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"

	"github.com/bcrisp4/bsearch/internal/client"
	"github.com/bcrisp4/bsearch/internal/config"
	"github.com/bcrisp4/bsearch/internal/search"
)

// runSearch queries the daemon over its unix socket (ADR 0010). It holds no
// index and no inference configuration of its own: the daemon owns both, so
// there is one place where a query is validated, embedded, and matched
// against the index's embedding identity.
func runSearch(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(out)
	socketPath := fs.String("socket", config.DefaultSocketPath(), "daemon socket")
	limit := fs.Int("limit", search.DefaultLimit, "maximum documents to return")
	asJSON := fs.Bool("json", false, "emit JSON instead of human-readable output")
	fs.Usage = func() {
		fmt.Fprintln(out, `usage: bsearch search [--socket <path>] [--limit <n>] [--json] <query>`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() == 0 {
		return errors.New(`search requires a query, e.g. bsearch search "heat pump quote"`)
	}
	if fs.NArg() > 1 {
		// Also hit by flags placed after the query: stdlib flag stops
		// parsing at the first positional, so `search alpha --json` has
		// NArg 2 — the hint must cover both mistakes.
		return fmt.Errorf("search takes one query argument (got %d) — flags go before the query; quote multi-word queries", fs.NArg())
	}
	if *socketPath == "" {
		return errors.New("cannot resolve the default socket path (no home directory?) — pass --socket")
	}
	// A flag always has a value, so an explicit --limit 0 is a mistake; in
	// the JSON request an absent limit is legitimately 0 and means "use the
	// default". The rules genuinely differ, so the flag carries its own.
	if *limit < 1 || *limit > search.MaxLimit {
		return fmt.Errorf("--limit %d out of range [1, %d]", *limit, search.MaxLimit)
	}
	req := search.Request{Query: fs.Arg(0), Limit: *limit}
	// The daemon validates too — this is the same function, not a second
	// rule — but running it here means an obviously bad query doesn't need
	// a daemon to be rejected.
	if _, err := req.Validate(); err != nil {
		return errors.New(search.Message(err))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	body, err := client.New(*socketPath).Search(ctx, req)
	if err != nil {
		return err
	}

	if *asJSON {
		// Copied through rather than re-encoded: fields a newer daemon adds
		// reach consumers that understand them, even when this binary
		// doesn't.
		_, err := out.Write(body)
		return err
	}
	var resp search.Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("the daemon sent a response this version cannot read: %w", err)
	}
	writeSearchHuman(out, resp)
	return nil
}

func writeSearchHuman(out io.Writer, resp search.Response) {
	if len(resp.Hits) == 0 {
		fmt.Fprintln(out, "no results")
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	for i, h := range resp.Hits {
		if i > 0 {
			fmt.Fprintln(out)
		}
		// Paths are untrusted display text too: macOS filenames may
		// contain any byte but NUL and '/', including ESC and newlines.
		fmt.Fprintf(out, "%s  (distance %.3f)\n", stripControl(tildePath(home, h.Path)), h.Distance)
		// Heading paths come from indexed documents — untrusted, like
		// chunk text; the preview sanitizes both.
		if hp := search.Preview(h.HeadingPath, search.PreviewRunes); hp != "" {
			fmt.Fprintf(out, "    %s\n", hp)
		}
		fmt.Fprintf(out, "    %s\n", search.Preview(h.ChunkPreview, search.PreviewRunes))
		// Duplicate copies fold into one hit (ADR 0015); without these
		// lines the collapse would read as results silently going missing.
		for _, p := range h.AlsoAt {
			fmt.Fprintf(out, "    also at: %s\n", stripControl(tildePath(home, p)))
		}
	}
}

// tildePath abbreviates the home directory prefix to ~ for display.
// JSON output keeps absolute paths — machine consumers need usable paths.
func tildePath(home, path string) string {
	if home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return "~" + string(filepath.Separator) + rest
	}
	return path
}

// stripControl drops control runes (ESC, newlines, tabs, …) so untrusted
// display text can't drive the terminal or break the one-line-per-field
// output format. Unlike search.Preview it neither collapses spaces nor
// truncates — paths need to stay verbatim and complete.
func stripControl(s string) string {
	// Fast path: control characters are rare in real paths.
	if !strings.ContainsFunc(s, unicode.IsControl) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !unicode.IsControl(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

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
	"time"
	"unicode"

	"github.com/bcrisp4/bsearch/internal/adapters/sqlite"
	"github.com/bcrisp4/bsearch/internal/config"
	"github.com/bcrisp4/bsearch/internal/search"
)

// searchTimeout bounds the whole query interactively. The embedder's shared
// HTTP client allows 60s (sized for bulk index batches); 30s still tolerates
// an inference server cold-loading the embedding model on the first query
// (DESIGN.md: Constraints).
const searchTimeout = 30 * time.Second

// runSearch is the one-shot semantic search command (DESIGN.md: Milestone
// M1): embed the query with the model's query prefix, brute-force KNN over
// the current vector table, collapse to best-chunk-per-document, print.
// The query path itself lives in internal/search, shared with the daemon.
func runSearch(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", config.DefaultPath(), "config file")
	dbPath := fs.String("db", config.DefaultDBPath(), "index database file")
	limit := fs.Int("limit", search.DefaultLimit, "maximum documents to return")
	asJSON := fs.Bool("json", false, "emit JSON instead of human-readable output")
	fs.Usage = func() {
		fmt.Fprintln(out, `usage: bsearch search [--config <path>] [--db <path>] [--limit <n>] [--json] <query>`)
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
	// A flag always has a value, so an explicit --limit 0 is a mistake; in
	// the JSON request an absent limit is legitimately 0 and means "use the
	// default". The rules genuinely differ, so the flag carries its own.
	if *limit < 1 || *limit > search.MaxLimit {
		return fmt.Errorf("--limit %d out of range [1, %d]", *limit, search.MaxLimit)
	}
	req := search.Request{Query: fs.Arg(0), Limit: *limit}
	// Reject a bad request before loading config and opening the index:
	// "the query is empty" should not arrive behind "no index database".
	// Search re-validates — this is the same check, not a second one.
	if _, err := req.Validate(); err != nil {
		return errors.New(search.Message(err))
	}

	if *dbPath == "" {
		return errors.New("cannot resolve the default database path (no home directory?) — pass --db")
	}
	_, embedder, err := loadInference(*configPath)
	if err != nil {
		return err
	}
	// Also before opening the index: an over-long query is the caller's
	// mistake and shouldn't be reported behind a missing-database error.
	if err := req.CheckLength(embedder.Spec()); err != nil {
		return errors.New(search.Message(err))
	}

	// Search is read-only: OpenExisting refuses to bring an empty index into
	// existence as a side effect, so a missing database is reported as one.
	db, err := sqlite.OpenExisting(*dbPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no index database at %s — run 'bsearch index' first", *dbPath)
		}
		return err
	}
	defer db.Close()
	store := sqlite.NewStore(db)

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(sigCtx, searchTimeout)
	defer cancel()

	resp, err := search.New(store, embedder).Search(ctx, req)
	if err != nil {
		return errors.New(search.Message(err))
	}

	if *asJSON {
		return json.NewEncoder(out).Encode(resp)
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
		fmt.Fprintf(out, "    %s\n", h.ChunkPreview)
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

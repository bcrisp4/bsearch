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
	"github.com/bcrisp4/bsearch/internal/domain"
)

const (
	// searchTimeout bounds the whole query interactively. The embedder's
	// shared HTTP client allows 60s (sized for bulk index batches); 30s
	// still tolerates an inference server cold-loading the embedding model
	// on the first query (DESIGN.md: Constraints).
	searchTimeout = 30 * time.Second

	// overFetchFactor over-fetches chunk hits before collapsing to
	// best-chunk-per-document, so documents with many matching chunks
	// can't crowd others out of the top --limit. Echoes DESIGN.md's
	// top-k×8 rescore factor; brute-force KNN scans every vector
	// regardless of k, so a larger k is nearly free.
	overFetchFactor = 8

	// maxKNNK is sqlite-vec's KNN k ceiling (SQLITE_VEC_VEC0_K_MAX) —
	// a k above it fails the query outright, so over-fetch clamps here.
	// Above maxKNNK/overFetchFactor documents the over-fetch factor
	// degrades gracefully instead of erroring.
	maxKNNK = 4096

	// maxLimit bounds --limit; must not exceed maxKNNK or the clamped k
	// could return fewer chunks than documents requested.
	maxLimit = 1000

	// previewRunes is the chunk-preview length (DESIGN.md: ~150 chars).
	previewRunes = 150
)

// runSearch is the one-shot semantic search command (DESIGN.md: Milestone
// M1): embed the query with the model's query prefix, brute-force KNN over
// the current vector table, collapse to best-chunk-per-document, print.
func runSearch(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", config.DefaultPath(), "config file")
	dbPath := fs.String("db", config.DefaultDBPath(), "index database file")
	limit := fs.Int("limit", 10, "maximum documents to return")
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
	query := fs.Arg(0)
	if strings.TrimSpace(query) == "" {
		return errors.New("query is empty")
	}
	if *limit < 1 || *limit > maxLimit {
		return fmt.Errorf("--limit %d out of range [1, %d]", *limit, maxLimit)
	}

	if *dbPath == "" {
		return errors.New("cannot resolve the default database path (no home directory?) — pass --db")
	}
	_, embedder, err := loadInference(*configPath)
	if err != nil {
		return err
	}
	spec := embedder.Spec()

	// The embedder enforces the input ceiling too, but its error is
	// phrased for indexing bugs ("composed"); a pasted-in over-long query
	// deserves a message that names the actual problem. Same heuristic
	// budget as the embedder's guard (BytesPerToken).
	if spec.CeilingTokens > 0 {
		if composed := spec.ComposeQuery(query); len(composed) > spec.CeilingTokens*domain.BytesPerToken {
			return fmt.Errorf("query is too long for the embedding model (limit %d tokens ≈ %d bytes; query composes to %d bytes) — shorten it",
				spec.CeilingTokens, spec.CeilingTokens*domain.BytesPerToken, len(composed))
		}
	}

	// Search is read-only: without this check, sqlite.Open would create and
	// migrate an empty database as a side effect, then fail on the missing
	// vec table anyway.
	if _, err := os.Stat(*dbPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no index database at %s — run 'bsearch index' first", *dbPath)
		}
		return err
	}

	db, err := sqlite.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	store := sqlite.NewStore(db)

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(sigCtx, searchTimeout)
	defer cancel()

	start := time.Now()

	// Fail fast (before the embedding HTTP call) when the index was built
	// under a different embedding identity: dims alone can't catch a model
	// swapped to another of equal dimensions, which would silently search
	// the wrong vector space. Whole-struct comparison so a future identity
	// field can't be silently skipped.
	indexed, dims, err := store.CurrentVecSpec(ctx)
	if err != nil {
		if errors.Is(err, sqlite.ErrNoVecTable) {
			return errors.New("nothing indexed yet — run 'bsearch index' first")
		}
		return err
	}
	want := spec
	want.CeilingTokens = 0 // excluded from vector-space identity (see CurrentVecSpec)
	if indexed != want {
		if indexed.Model != want.Model {
			return fmt.Errorf("the index was built with model %q but config now says %q — run 'bsearch index' to re-embed",
				indexed.Model, want.Model)
		}
		return fmt.Errorf("the index was built with a different embedding configuration for model %q (prefix templates changed?) — run 'bsearch index' to re-embed",
			want.Model)
	}

	vec, err := embedder.EmbedQuery(ctx, query)
	if err != nil {
		return err
	}
	// Same model name, different dimensions: the server's model changed
	// under us. SearchVectors would reject this too, but with a raw table
	// error; name the remedy.
	if len(vec) != dims {
		return fmt.Errorf("the embedding server returned %d dimensions but the index was built with %d — the model behind %q changed; run 'bsearch index' to re-embed",
			len(vec), dims, want.Model)
	}

	hits, err := store.SearchVectors(ctx, vec, knnK(*limit))
	if err != nil {
		return err
	}
	docs := domain.CollapseBestPerDoc(hits, *limit)
	took := time.Since(start).Milliseconds()

	if *asJSON {
		return writeSearchJSON(out, docs, took)
	}
	writeSearchHuman(out, docs)
	return nil
}

// knnK is the chunk over-fetch for a document limit, clamped to sqlite-vec's
// k ceiling. maxLimit <= maxKNNK keeps k >= limit after clamping.
func knnK(limit int) int {
	return min(limit*overFetchFactor, maxKNNK)
}

type searchHitJSON struct {
	DocID        string    `json:"doc_id"`
	Path         string    `json:"path"`
	Distance     float64   `json:"distance"`
	ChunkPreview string    `json:"chunk_preview"`
	HeadingPath  string    `json:"heading_path,omitempty"`
	Modified     time.Time `json:"modified"`
}

type searchJSON struct {
	Hits   []searchHitJSON `json:"hits"`
	TookMS int64           `json:"took_ms"`
}

// writeSearchJSON emits the DESIGN.md search response shape, with two M1
// differences: `distance` (raw KNN distance, lower = better, uncalibrated)
// instead of `score` — that name is reserved for fused ranking — and no
// `summary` until the summarizer lands.
func writeSearchJSON(out io.Writer, docs []domain.Hit, tookMS int64) error {
	resp := searchJSON{Hits: []searchHitJSON{}, TookMS: tookMS}
	for _, h := range docs {
		resp.Hits = append(resp.Hits, searchHitJSON{
			DocID:        h.Doc.ID,
			Path:         h.Doc.Path,
			Distance:     h.Distance,
			ChunkPreview: preview(h.Chunk.Text, previewRunes),
			HeadingPath:  h.Chunk.HeadingPath,
			Modified:     h.Doc.MTime.UTC(),
		})
	}
	return json.NewEncoder(out).Encode(resp)
}

func writeSearchHuman(out io.Writer, docs []domain.Hit) {
	if len(docs) == 0 {
		fmt.Fprintln(out, "no results")
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	for i, h := range docs {
		if i > 0 {
			fmt.Fprintln(out)
		}
		// Paths are untrusted display text too: macOS filenames may
		// contain any byte but NUL and '/', including ESC and newlines.
		fmt.Fprintf(out, "%s  (distance %.3f)\n", stripControl(tildePath(home, h.Doc.Path)), h.Distance)
		// Heading paths come from indexed documents — untrusted, like
		// chunk text; preview() sanitizes both.
		if hp := preview(h.Chunk.HeadingPath, previewRunes); hp != "" {
			fmt.Fprintf(out, "    %s\n", hp)
		}
		fmt.Fprintf(out, "    %s\n", preview(h.Chunk.Text, previewRunes))
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
// output format. Unlike preview it neither collapses spaces nor truncates —
// paths need to stay verbatim and complete.
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

// preview renders untrusted document text for one line of output: whitespace
// runs collapse to single spaces, control characters are dropped (a crafted
// document must not drive the terminal via escape sequences), and the result
// is truncated to maxRunes runes with an ellipsis. Truncation is
// rune-boundary safe (grapheme clusters may still split; acceptable for a
// preview). Single pass — never allocates proportional to the full text.
func preview(text string, maxRunes int) string {
	var b strings.Builder
	runes := 0
	pendingSpace := false
	for _, r := range text {
		if unicode.IsSpace(r) {
			pendingSpace = runes > 0
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		need := 1
		if pendingSpace {
			need = 2
		}
		if runes+need > maxRunes {
			return b.String() + "…"
		}
		if pendingSpace {
			b.WriteByte(' ')
			runes++
			pendingSpace = false
		}
		b.WriteRune(r)
		runes++
	}
	return b.String()
}

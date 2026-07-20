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

	"github.com/bcrisp4/bsearch/internal/adapters/openai"
	"github.com/bcrisp4/bsearch/internal/adapters/sqlite"
	"github.com/bcrisp4/bsearch/internal/config"
	"github.com/bcrisp4/bsearch/internal/domain"
	"github.com/bcrisp4/bsearch/internal/embedding"
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

	// maxLimit bounds --limit so k = limit × overFetchFactor stays sane.
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
		return fmt.Errorf("search takes one query argument (got %d) — quote multi-word queries", fs.NArg())
	}
	query := fs.Arg(0)
	if strings.TrimSpace(query) == "" {
		return errors.New("query is empty")
	}
	if *limit < 1 || *limit > maxLimit {
		return fmt.Errorf("--limit %d out of range [1, %d]", *limit, maxLimit)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.Inference.EmbeddingModel == "" {
		return fmt.Errorf("inference.embedding_model is not set — add it to %s (the M2 bake-off records recommended defaults in DESIGN.md)", *configPath)
	}
	if *dbPath == "" {
		return fmt.Errorf("cannot resolve the default database path (no home directory?) — pass --db")
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

	spec := embedding.ResolveSpec(
		cfg.Inference.EmbeddingModel,
		cfg.Inference.QueryTemplate,
		cfg.Inference.PassageTemplate,
		cfg.Inference.InputCeilingTokens,
	)
	embedder, err := openai.NewEmbedder(openai.EmbedderConfig{
		Endpoint: cfg.Inference.Endpoint,
		Spec:     spec,
	})
	if err != nil {
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
	// the wrong vector space.
	indexed, _, err := store.CurrentVecSpec(ctx)
	if err != nil {
		if errors.Is(err, sqlite.ErrNoVecTable) {
			return errors.New("nothing indexed yet — run 'bsearch index' first")
		}
		return err
	}
	current := embedder.Spec()
	if indexed.Model != current.Model {
		return fmt.Errorf("the index was built with model %q but config now says %q — run 'bsearch index' to re-embed",
			indexed.Model, current.Model)
	}
	if indexed.QueryTemplate != current.QueryTemplate || indexed.PassageTemplate != current.PassageTemplate {
		return fmt.Errorf("the index was built with different prefix templates for model %q — run 'bsearch index' to re-embed",
			current.Model)
	}

	vec, err := embedder.EmbedQuery(ctx, query)
	if err != nil {
		return err
	}
	hits, err := store.SearchVectors(ctx, vec, *limit*overFetchFactor)
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
	for i, h := range docs {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "%s  (distance %.3f)\n", tildePath(h.Doc.Path), h.Distance)
		if h.Chunk.HeadingPath != "" {
			fmt.Fprintf(out, "    %s\n", h.Chunk.HeadingPath)
		}
		fmt.Fprintf(out, "    %s\n", preview(h.Chunk.Text, previewRunes))
	}
}

// tildePath abbreviates the user's home directory prefix to ~ for display.
// JSON output keeps absolute paths — machine consumers need usable paths.
func tildePath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
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

// preview collapses all whitespace runs to single spaces and truncates to
// maxRunes runes, appending an ellipsis when truncated. Truncation is
// rune-boundary safe (grapheme clusters may still split; acceptable for a
// preview).
func preview(text string, maxRunes int) string {
	s := strings.Join(strings.Fields(text), " ")
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}

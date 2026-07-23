package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bcrisp4/bsearch/internal/adapters/openai"
	"github.com/bcrisp4/bsearch/internal/adapters/sqlite"
	"github.com/bcrisp4/bsearch/internal/config"
	"github.com/bcrisp4/bsearch/internal/discovery"
	"github.com/bcrisp4/bsearch/internal/pipeline"
)

// runIndex is the one-shot indexing command (DESIGN.md: Milestone M1):
// scan the configured include paths, then chunk → embed → store every new,
// changed, or stale document. Idempotent — re-runs skip up-to-date files.
func runIndex(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", config.DefaultPath(), "config file")
	dbPath := fs.String("db", config.DefaultDBPath(), "index database file")
	fs.Usage = func() {
		fmt.Fprintln(out, "usage: bsearch index [--config <path>] [--db <path>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("index takes no arguments (got %q) — indexing scope is [paths].include in the config", fs.Arg(0))
	}

	if *dbPath == "" {
		return errors.New("cannot resolve the default database path (no home directory?) — pass --db")
	}
	cfg, embedder, err := loadInference(*configPath)
	if err != nil {
		return err
	}

	db, err := sqlite.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	store := sqlite.NewStore(db)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	start := time.Now()
	scanner := discovery.New(store, discovery.Options{
		Include:  cfg.Paths.Include,
		Excluded: cfg.ExcludeRules().Match,
	})
	scanRes, err := scanner.Scan(ctx)
	if err != nil {
		return err
	}
	// TCC and other per-path failures are first-class state, never a silent
	// skip (DESIGN.md: Constraints).
	for _, pe := range scanRes.PathErrors {
		fmt.Fprintf(out, "warning: %s: %v\n", pe.Path, pe.Err)
	}
	fmt.Fprintf(out, "scanned: %d new/changed, %d unchanged, %d renamed, %d skipped (iCloud placeholder)\n",
		scanRes.Discovered, scanRes.Unchanged, scanRes.Renamed, scanRes.Dataless)
	// Every root errored and nothing was reachable: that is a permissions
	// problem (likely missing Full Disk Access), not a successful index of
	// an empty corpus — fail loudly (DESIGN.md: TCC is first-class state).
	if len(scanRes.PathErrors) > 0 &&
		scanRes.Discovered+scanRes.Unchanged+scanRes.Renamed+scanRes.Dataless == 0 {
		return errors.New("scan reached no files — check the warnings above (missing Full Disk Access?)")
	}

	ix, err := pipeline.New(pipeline.Options{
		Store:     store,
		Vectors:   store,
		Embedder:  embedder,
		Transient: openai.Transient,
		Progress:  out,
	})
	if err != nil {
		return err
	}
	docs, err := store.ListIndexable(ctx)
	if err != nil {
		return err
	}
	sum, runErr := ix.Run(ctx, docs)

	line := fmt.Sprintf("done: %d indexed, %d up to date, %d failed", sum.Indexed, sum.UpToDate, sum.Failed)
	if sum.Skipped > 0 {
		line += fmt.Sprintf(", %d skipped", sum.Skipped)
	}
	if sum.Warnings > 0 {
		line += fmt.Sprintf(", %d warnings", sum.Warnings)
	}
	fmt.Fprintf(out, "%s (%.1fs)\n", line, time.Since(start).Seconds())
	if runErr != nil {
		return runErr
	}
	// Per-document failures must be machine-visible: an unattended run
	// (cron, launchd) that silently drops documents from the index while
	// exiting 0 is indistinguishable from a clean run.
	if sum.Failed > 0 {
		return fmt.Errorf("%d document(s) failed to index — see output above", sum.Failed)
	}
	return nil
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/bcrisp4/bsearch/internal/config"
	"github.com/bcrisp4/bsearch/internal/daemon"
	"github.com/bcrisp4/bsearch/internal/server"
	"github.com/bcrisp4/bsearch/internal/socket"
)

// runServe runs the daemon: an HTTP+JSON API over a unix socket
// (DESIGN.md: Process model, API). Indexing still belongs to the one-shot
// `bsearch index` command until the scheduler and watcher land (#12, #13).
func runServe(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", config.DefaultPath(), "config file")
	dbPath := fs.String("db", config.DefaultDBPath(), "index database file")
	socketPath := fs.String("socket", config.DefaultSocketPath(), "unix socket to listen on")
	lockPath := fs.String("lock", config.DefaultLockPath(), "single-instance lock file")
	logLevel := fs.String("log-level", "info", "log level (debug|info|warn|error)")
	fs.Usage = func() {
		fmt.Fprintln(out, `usage: bsearch serve [--config <path>] [--db <path>] [--socket <path>] [--lock <path>] [--log-level <level>]`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("serve takes no arguments (got %q)", fs.Arg(0))
	}
	if *socketPath == "" || *dbPath == "" {
		return errors.New("cannot resolve the default socket or database path (no home directory?) — pass --socket and --db")
	}
	level, err := parseLogLevel(*logLevel)
	if err != nil {
		return err
	}
	// Logs go to stderr so stdout stays free for anything a future
	// foreground mode wants to print; launchd captures both.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// A malformed config is a startup failure: the daemon would be running
	// with settings the user did not write. A *missing embedding model* is
	// not — see newEmbedder.
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	opts := daemon.Options{DBPath: *dbPath}
	if cfg.Inference.EmbeddingModel == "" {
		opts.NotReady = fmt.Sprintf("inference.embedding_model is not set in %s (%s)", *configPath, missingModelHint)
		log.Warn("no embedding model configured; search will be unavailable until it is set",
			"config", *configPath)
	} else {
		embedder, err := newEmbedder(cfg)
		if err != nil {
			return err
		}
		opts.Embedder = embedder
	}

	back := daemon.New(opts)
	// Closed after the server has drained, never before: handlers are still
	// running during the drain.
	defer func() {
		if err := back.Close(); err != nil {
			log.Error("close index", "error", err)
		}
	}()

	ln, err := socket.Listen(*socketPath, *lockPath)
	if err != nil {
		if errors.Is(err, socket.ErrAlreadyRunning) {
			// Expected under launchd double-starts and a user running
			// serve twice; report it as a fact, not a crash.
			return fmt.Errorf("%w — stop it first, or pass a different --socket and --lock", err)
		}
		return err
	}
	defer ln.Close() //nolint:errcheck // Serve's shutdown closes it; this is the failure path

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("listening",
		"socket", *socketPath,
		"db", *dbPath,
		"config", *configPath,
		"pid", os.Getpid(),
	)
	return server.New(server.Options{
		Backend: back,
		Socket:  *socketPath,
		DBPath:  *dbPath,
		Logger:  log,
	}).Serve(ctx, ln)
}

func parseLogLevel(name string) (slog.Level, error) {
	var level slog.Level
	// UnmarshalText accepts the canonical names case-insensitively and
	// reports the valid set on failure.
	if err := level.UnmarshalText([]byte(name)); err != nil {
		return 0, fmt.Errorf("--log-level %q: want debug, info, warn, or error", name)
	}
	return level, nil
}

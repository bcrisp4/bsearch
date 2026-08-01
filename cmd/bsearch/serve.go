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
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/bcrisp4/bsearch/internal/adapters/fsevents"
	"github.com/bcrisp4/bsearch/internal/adapters/openai"
	"github.com/bcrisp4/bsearch/internal/adapters/sqlite"
	"github.com/bcrisp4/bsearch/internal/config"
	"github.com/bcrisp4/bsearch/internal/daemon"
	"github.com/bcrisp4/bsearch/internal/discovery"
	"github.com/bcrisp4/bsearch/internal/domain"
	"github.com/bcrisp4/bsearch/internal/pipeline"
	"github.com/bcrisp4/bsearch/internal/scheduler"
	"github.com/bcrisp4/bsearch/internal/server"
	"github.com/bcrisp4/bsearch/internal/socket"
	"github.com/bcrisp4/bsearch/internal/timemachine"
)

// runServe runs the daemon: an HTTP+JSON API over a unix socket, plus the
// background indexing loop that keeps the index current (DESIGN.md: Process
// model, API; ADR 0011, ADR 0012, ADR 0013).
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
	if *socketPath == "" || *dbPath == "" || *lockPath == "" {
		return errors.New("cannot resolve the default socket, lock, or database path (no home directory?) — pass --socket, --lock and --db")
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

	// Done here, holding the single-instance lock, because Listen has just
	// created the data directory and no second daemon can be racing us for it.
	excludeDataDirFromBackups(*dbPath, log)

	// The indexing side, built only once the single-instance lock is held:
	// opening the writer creates and migrates the index (ADR 0012), and a
	// second `bsearch serve` must not take the write lock on a running
	// daemon's database — or apply a migration underneath it — before it
	// discovers it lost the race.
	//
	// It is set up defensively: every way it can fail to start leaves the
	// daemon serving /v1/status, because a LaunchAgent that exits non-zero is
	// a crash-loop with nothing able to explain itself, and "why is nothing
	// being indexed" is exactly the question status exists to answer.
	sched, closeIndexer, indexingOff := newScheduler(cfg, opts.Embedder, *dbPath, log)
	defer closeIndexer()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("listening",
		"socket", *socketPath,
		"db", *dbPath,
		"config", *configPath,
		"pid", os.Getpid(),
	)

	// The scheduler gets its own cancellation rather than sharing the signal
	// context, so shutdown is ordered: drain the HTTP server first, and only
	// then stop indexing. The other order would abandon a document mid-write
	// while requests were still arriving.
	var wg sync.WaitGroup
	indexCtx, stopIndexing := context.WithCancel(context.Background())
	defer stopIndexing()
	if sched != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sched.Run(indexCtx); err != nil {
				log.Error("indexing scheduler stopped", "error", err)
			}
		}()
	}

	serveErr := server.New(server.Options{
		Backend:  back,
		Indexing: func() server.IndexingStatus { return indexingStatus(sched, indexingOff) },
		Socket:   *socketPath,
		DBPath:   *dbPath,
		Logger:   log,
	}).Serve(ctx, ln)

	stopIndexing()
	wg.Wait()
	return serveErr
}

// excludeDataDirFromBackups keeps the index out of Time Machine (DESIGN.md:
// Data retention — Backups; ADR 0017). The index is derived data with nothing
// to preserve, and it concentrates the text of everything indexed into one
// file, so a backup of it is Security threat 1 with a copy made.
//
// Never fatal, for the reason newScheduler below is never fatal: a LaunchAgent
// that exits non-zero is a crash-loop with nothing able to explain why. A
// missed exclusion costs backup space and leaks an index into a backup the
// user already has; refusing to start costs them search entirely.
//
// It excludes the data directory, and only when the database is in it. Never
// filepath.Dir(dbPath): --db is an arbitrary path, and excluding its parent
// would take `--db ~/notes.db` and quietly drop the user's whole home
// directory from their backups. Excluding the *default* directory regardless
// would be the mirror-image mistake — marking a directory the daemon is not
// using while the index it is using stays in the backup. So a database
// somewhere else is reported and nothing is touched.
func excludeDataDirFromBackups(dbPath string, log *slog.Logger) {
	dir := config.DataDir()
	if dir == "" {
		// No home directory, so no default data directory to speak of. The
		// caller has already rejected the paths that matter.
		return
	}
	if filepath.Dir(dbPath) != dir {
		log.Info("index is outside the data directory, so it is not excluded from Time Machine backups",
			"db", dbPath, "data_dir", dir)
		return
	}
	if err := timemachine.Exclude(dir); err != nil {
		log.Warn("could not exclude the data directory from Time Machine backups",
			"dir", dir, "error", err)
	}
}

// newScheduler builds the indexing loop, returning nil when it cannot be
// built. Every failure here is reported and survived rather than returned:
// the daemon's job when something is misconfigured is to keep answering
// status, not to exit.
//
// Which is why the reason is returned as well as logged. "Why is nothing
// being indexed" is the question status exists to answer, and a daemon that
// answers it only in a log file it cannot point at has not answered it.
//
// The returned close function releases the writer database and must run after
// the scheduler has stopped; the reason is empty when the scheduler was built.
func newScheduler(cfg *config.Config, embedder domain.Embedder, dbPath string, log *slog.Logger) (*scheduler.Scheduler, func(), string) {
	noop := func() {}
	if embedder == nil {
		// The caller has already logged this and put the fuller explanation
		// in the index half of status; this is the indexing half's echo of it.
		return nil, noop, "no embedding model is configured"
	}

	// Open, not OpenExisting: the daemon owns the writer role now, so it
	// creates and migrates the index rather than waiting for something else
	// to (ADR 0012). The query path keeps its own lazily-opened read handle.
	db, err := sqlite.Open(dbPath)
	if err != nil {
		log.Error("indexing disabled: cannot open the index for writing", "db", dbPath, "error", err)
		return nil, noop, "the index could not be opened for writing: " + err.Error()
	}
	closeDB := func() {
		if err := db.Close(); err != nil {
			log.Error("close index writer", "error", err)
		}
	}

	store := sqlite.NewStore(db)
	indexer, err := pipeline.New(pipeline.Options{
		Store:     store,
		Vectors:   store,
		Embedder:  embedder,
		Transient: openai.Transient,
		// Progress lines are for a human watching a one-shot command. The
		// daemon reports through the log, which has levels and structure.
		Progress: io.Discard,
	})
	if err != nil {
		log.Error("indexing disabled", "error", err)
		closeDB()
		return nil, noop, "the indexing pipeline could not be built: " + err.Error()
	}

	sched, err := scheduler.New(scheduler.Options{
		Queue:   store,
		Indexer: indexer,
		Scanner: discovery.New(store, store, discovery.Options{
			Include:  cfg.Paths.Include,
			Excluded: cfg.ExcludeRules().Match,
		}),
		// Constructing the watcher subscribes to nothing yet; the scheduler
		// starts it, and a platform or a grant that will not allow it ends
		// as scan-only with a reason in status, never as a failed startup.
		Watcher: fsevents.New(),
		Logger:  log,
		// Always the AC policy for now: macOS power detection lands with M7.
		// Assuming AC is the safe half of the guess — assuming battery would
		// silently stop indexing on a desktop, and the failure would look
		// like a bug rather than a default.
		Power: func() config.PowerPolicy { return cfg.Power.AC },
	})
	if err != nil {
		log.Error("indexing disabled", "error", err)
		closeDB()
		return nil, noop, "the indexing scheduler could not be built: " + err.Error()
	}
	return sched, closeDB, ""
}

// indexingStatus maps the scheduler's snapshot onto the wire type. It lives
// here rather than in internal/server so the transport keeps knowing nothing
// about the indexing side, exactly as it knows nothing about storage.
//
// A nil scheduler is not an error state to hide: it is the answer to "why is
// nothing being indexed", so it is reported with the reason that produced it.
func indexingStatus(sched *scheduler.Scheduler, offReason string) server.IndexingStatus {
	if sched == nil {
		if offReason == "" {
			offReason = "indexing is not running"
		}
		// The always-present slices are still always present: a consumer
		// measuring `.indexing.scan_unmounted | length` must not meet null
		// just because the indexing loop never started.
		return server.IndexingStatus{
			Running:            false,
			Reason:             offReason,
			ScanUnmounted:      []string{},
			ScanDeclineReasons: []string{},
		}
	}
	snap := sched.Snapshot()
	status := server.IndexingStatus{
		Running:            true,
		Gate:               snap.Gate,
		Deferring:          snap.Deferring,
		LastError:          snap.LastError,
		LastScan:           optionalTime(snap.LastScan),
		LastCycle:          optionalTime(snap.LastCycle),
		LastProgress:       optionalTime(snap.LastProgress),
		ScanErrors:         snap.ScanErrs,
		ScanReachedNothing: snap.ScanReachedNothing,
		ScanDeleted:        snap.ScanDeleted,
		ScanPruned:         snap.ScanPruned,
		ScanUnverified:     snap.ScanUnverified,
		ScanIgnored:        snap.ScanIgnored,
		ScanUnmounted:      nonNil(snap.ScanUnmounted),
		ScanDeclineReasons: nonNil(snap.ScanDeclineReasons),
		Watch: &server.WatchStatus{
			Running:    snap.Watching,
			Reason:     snap.WatchReason,
			Roots:      snap.WatchRoots,
			LastEvent:  optionalTime(snap.LastWatchEvent),
			Reconciled: snap.WatchReconciled,
			Deleted:    snap.WatchDeleted,
			Rescans:    snap.WatchRescans,
			Ignored:    snap.WatchIgnored,
		},
		Totals: server.IndexingTotals{
			Indexed:    snap.Indexed,
			Failed:     snap.Failed,
			Skipped:    snap.Skipped,
			Retried:    snap.Retried,
			Swept:      snap.Swept,
			Collected:  snap.Collected,
			Changed:    snap.Changed,
			Superseded: snap.Superseded,
		},
	}
	for _, pe := range snap.PathErrors {
		status.PathErrors = append(status.PathErrors, server.PathError{Path: pe.Path, Error: pe.Err})
	}
	return status
}

// optionalTime reports a timestamp only once it has happened. The zero Time
// marshals to a date in year 1, which reads as an answer rather than as the
// absence of one.
func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
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

// nonNil renders an empty slice as [] rather than null. The status document is
// a contract (DESIGN.md: Interfaces) and its examples show [], so a consumer
// indexing or measuring the field must not meet null on every healthy daemon.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

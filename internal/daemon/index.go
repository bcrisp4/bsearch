// Package daemon owns the index handle behind the HTTP API: opening it
// lazily, noticing when it is replaced, and reporting what state it is in.
// It is the one place that knows both the storage adapter and the query
// service, which keeps the transport (internal/server) free of either.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/bcrisp4/bsearch/internal/adapters/sqlite"
	"github.com/bcrisp4/bsearch/internal/domain"
	"github.com/bcrisp4/bsearch/internal/search"
	"github.com/bcrisp4/bsearch/internal/server"
)

// idleConnTimeout retires idle reader connections. A daemon that answers a
// handful of searches a day should not hold a page cache open between them.
const idleConnTimeout = 5 * time.Minute

// Options configures a Daemon.
type Options struct {
	// DBPath is the index database. It need not exist yet.
	DBPath string
	// Embedder embeds queries. Nil means the daemon is running without a
	// usable inference configuration: it still serves status, so the user
	// can find out why searching fails.
	Embedder domain.Embedder
	// NotReady explains a nil Embedder, in the user's terms.
	NotReady string
}

// Daemon answers the server's Backend port. Safe for concurrent use.
type Daemon struct {
	dbPath   string
	embedder domain.Embedder
	notReady string

	// gate serialises access to the handle below. A channel rather than a
	// sync.Mutex because it can be acquired under a context: a mutex would
	// let one slow open blow every waiting request's deadline, and the
	// clients would see dead connections instead of a timeout they can read.
	gate chan struct{}

	db    *sqlite.DB
	store *sqlite.Store
	// opened identifies the file the handle refers to, for detecting a
	// replaced database (drop-and-reindex creates a new inode).
	opened os.FileInfo
	closed bool

	closeOnce sync.Once
	closeErr  error
}

var _ server.Backend = (*Daemon)(nil)

// New builds a Daemon. Nothing is opened until the first request — the
// daemon must start (and answer status) whether or not an index exists.
func New(opts Options) *Daemon {
	return &Daemon{
		dbPath:   opts.DBPath,
		embedder: opts.Embedder,
		notReady: opts.NotReady,
		gate:     make(chan struct{}, 1),
	}
}

// Search answers a query against the current index.
func (d *Daemon) Search(ctx context.Context, req search.Request) (search.Response, error) {
	if d.embedder == nil {
		return search.Response{}, fmt.Errorf("%w: %s", search.ErrNotIndexed, d.unavailableReason())
	}
	store, err := d.acquire(ctx)
	if err != nil {
		return search.Response{}, err
	}
	return search.New(store, d.embedder).Search(ctx, req)
}

// Status reports what the daemon can see. It answers as fully as it can at
// every level of brokenness: no database, a database with no vectors, or an
// index built under a different model all produce counts where counts exist
// and a reason where they don't.
func (d *Daemon) Status(ctx context.Context) (server.IndexStatus, error) {
	store, err := d.acquire(ctx)
	if err != nil {
		if errors.Is(err, search.ErrNotIndexed) {
			// A missing model outranks a missing index: indexing wouldn't
			// fix it, and the next status call surfaces the index once the
			// configuration is right.
			if d.embedder == nil {
				return server.IndexStatus{Ready: false, Reason: d.unavailableReason()}, nil
			}
			return server.IndexStatus{Ready: false, Reason: search.Message(err)}, nil
		}
		return server.IndexStatus{}, err
	}

	status := server.IndexStatus{Documents: map[string]int{}}
	counts, err := store.CountsByState(ctx)
	if err != nil {
		return server.IndexStatus{}, err
	}
	for state, n := range counts {
		status.Documents[string(state)] = n
	}

	indexed, dims, err := store.CurrentVecSpec(ctx)
	if err != nil {
		if errors.Is(err, domain.ErrNoVecTable) {
			status.Reason = "nothing embedded yet — run 'bsearch index'"
			return status, nil
		}
		return server.IndexStatus{}, err
	}
	status.Model = indexed.Model
	status.Dims = dims

	if d.embedder == nil {
		status.Reason = d.unavailableReason()
		return status, nil
	}
	// Report the same disagreement search would refuse on, rather than
	// claiming readiness and letting every query fail with a 409.
	configured := d.embedder.Spec()
	configured.CeilingTokens = 0 // a chunking budget, not part of the vector space
	if indexed != configured {
		status.Reason = fmt.Sprintf("the index was built for model %q but the daemon is configured for %q — run 'bsearch index' to re-embed, or restart the daemon if you changed its config",
			indexed.Model, configured.Model)
		return status, nil
	}
	status.Ready = true
	return status, nil
}

// Close releases the index handle and stops later requests reopening it.
// Called after the server has drained, never before: handlers are still
// running during the drain.
func (d *Daemon) Close() error {
	d.closeOnce.Do(func() {
		d.gate <- struct{}{}
		defer func() { <-d.gate }()
		d.closed = true
		if d.db != nil {
			d.closeErr = d.db.Close()
			d.db, d.store, d.opened = nil, nil, nil
		}
	})
	return d.closeErr
}

// acquire returns a store over the current index, opening it if needed.
//
// Two behaviours matter more than they look. Failures are never cached: a
// daemon started before the first `bsearch index` has to start working when
// the index appears, without a restart. And the file is re-checked every
// time: `bsearch index` runs in another process, so a drop-and-reindex
// replaces the inode underneath us, and a cached handle would go on serving
// an unlinked file with no error to give it away.
func (d *Daemon) acquire(ctx context.Context) (*sqlite.Store, error) {
	select {
	case d.gate <- struct{}{}:
		defer func() { <-d.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if d.closed {
		return nil, errors.New("the daemon is shutting down")
	}

	fi, err := os.Stat(d.dbPath)
	if err != nil {
		if os.IsNotExist(err) {
			d.dropHandle()
			return nil, fmt.Errorf("%w: no index database at %s — run 'bsearch index' first", search.ErrNotIndexed, d.dbPath)
		}
		return nil, fmt.Errorf("check index: %w", err)
	}

	if d.store != nil {
		if os.SameFile(fi, d.opened) {
			return d.store, nil
		}
		d.dropHandle() // replaced on disk; the open handle points at a ghost
	}

	db, err := sqlite.OpenExisting(d.dbPath)
	if err != nil {
		return nil, err
	}
	// A daemon holds its connections for weeks. The DSN gives every
	// connection a 64 MiB page cache, so idle connections are the
	// difference between a resident footprint that decays and one that only
	// grows (DESIGN.md: near-zero when idle).
	db.Reader().SetMaxIdleConns(1)
	db.Reader().SetConnMaxIdleTime(idleConnTimeout)
	// The daemon never writes; keep no writer connection warm.
	db.Writer().SetMaxIdleConns(0)

	d.db, d.store, d.opened = db, sqlite.NewStore(db), fi
	return d.store, nil
}

// dropHandle closes the current handle, ignoring the close error: the caller
// is already moving on to a fresh open, and a failure to close a database
// that no longer exists is not actionable.
func (d *Daemon) dropHandle() {
	if d.db == nil {
		return
	}
	_ = d.db.Close()
	d.db, d.store, d.opened = nil, nil, nil
}

func (d *Daemon) unavailableReason() string {
	if d.notReady != "" {
		return d.notReady
	}
	return "the daemon has no embedding model configured"
}

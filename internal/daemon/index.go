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

	// gate serialises swapping the handle below. A channel rather than a
	// sync.Mutex because it can be acquired under a context: a mutex would
	// let one slow open blow every waiting request's deadline, and the
	// clients would see dead connections instead of a timeout they can read.
	gate chan struct{}

	current *handle
	closed  bool

	closeOnce sync.Once
	closeErr  error
}

// handle is one open database, reference-counted so it outlives a swap.
//
// Requests hold a reference for the whole of their work, which spans several
// queries. Without the count, a concurrent request that noticed a replaced
// database would close the handle mid-search and the first request would die
// with "sql: database is closed" — an unclassifiable error, so the user would
// see an opaque 500 for something they did nothing to cause.
type handle struct {
	db    *sqlite.DB
	store *sqlite.Store
	// file identifies the database this handle refers to, for detecting a
	// replacement (drop-and-reindex creates a new inode).
	file os.FileInfo

	mu      sync.Mutex
	refs    int
	retired bool
}

// ref claims the handle for one request's duration.
func (h *handle) ref() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refs++
}

// release drops a request's claim, closing the database if this was the last
// one and the handle has been retired. A close error is dropped here: the
// last request out is not the one that can act on it, and by then the caller
// has its answer.
func (h *handle) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refs--
	_ = h.closeIfDone()
}

// retire marks the handle superseded, closing it if nothing is using it. When
// requests are still in flight the close is deferred to the last of them —
// which is the point: a swap must never pull the file out from under a query
// that is already running.
func (h *handle) retire() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.retired = true
	return h.closeIfDone()
}

// closeIfDone closes the database once nothing is using it.
func (h *handle) closeIfDone() error {
	if !h.retired || h.refs > 0 || h.db == nil {
		return nil
	}
	err := h.db.Close()
	h.db = nil
	return err
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
	h, err := d.acquire(ctx)
	if err != nil {
		return search.Response{}, err
	}
	defer h.release()
	return search.New(h.store, d.embedder).Search(ctx, req)
}

// Status reports what the daemon can see. It answers as fully as it can at
// every level of brokenness: no database, a database with no vectors, or an
// index built under a different model all produce counts where counts exist
// and a reason where they don't.
func (d *Daemon) Status(ctx context.Context) (server.IndexStatus, error) {
	h, err := d.acquire(ctx)
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
	defer h.release()

	status := server.IndexStatus{Documents: map[string]int{}}
	counts, err := h.store.CountsByState(ctx)
	if err != nil {
		return server.IndexStatus{}, err
	}
	for state, n := range counts {
		status.Documents[string(state)] = n
	}

	indexed, dims, err := h.store.CurrentVecSpec(ctx)
	if err != nil {
		if errors.Is(err, domain.ErrNoVecTable) {
			status.Reason = "nothing embedded yet — the daemon indexes in the background"
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
		status.Reason = fmt.Sprintf("the index was built for model %q but the daemon is configured for %q — restart the daemon, which re-embeds the corpus in the background under the configured model",
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
		d.closeErr = d.retireCurrent()
	})
	return d.closeErr
}

// acquire returns a store over the current index, opening it if needed.
//
// Two behaviours matter more than they look. Failures are never cached: the
// query path opens the database read-only and never creates it, so a daemon
// whose scheduler has not yet built the index has to start working the moment
// it appears, without a restart. And the file is re-checked every time,
// because a drop-and-reindex replaces the inode rather than the contents —
// `bsearch reindex` (#24) and a user restoring a backup both do it — and a
// cached handle would go on serving an unlinked file with no error to give it
// away.
func (d *Daemon) acquire(ctx context.Context) (*handle, error) {
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
			_ = d.retireCurrent()
			return nil, fmt.Errorf("%w: no index database at %s yet — the daemon creates it once an embedding model is configured", search.ErrNotIndexed, d.dbPath)
		}
		return nil, fmt.Errorf("check index: %w", err)
	}

	if d.current != nil {
		if os.SameFile(fi, d.current.file) {
			d.current.ref()
			return d.current, nil
		}
		_ = d.retireCurrent() // replaced on disk; the open handle points at a ghost
	}

	db, err := sqlite.OpenExisting(d.dbPath)
	if err != nil {
		// Every reason this can fail — the file vanished between the stat
		// and the open, a schema this build is too old or too new for — is
		// a statement about the index, not a daemon fault, and each carries
		// text worth reading. Without the sentinel they would all arrive as
		// an opaque 500 telling the user to check a log.
		return nil, fmt.Errorf("%w: %w", search.ErrNotIndexed, err)
	}
	// A daemon holds its connections for weeks. The DSN gives every
	// connection a 64 MiB page cache, so idle connections are the
	// difference between a resident footprint that decays and one that only
	// grows (DESIGN.md: near-zero when idle).
	db.Reader().SetMaxIdleConns(1)
	db.Reader().SetConnMaxIdleTime(idleConnTimeout)
	// The daemon never writes; keep no writer connection warm.
	db.Writer().SetMaxIdleConns(0)

	d.current = &handle{db: db, store: sqlite.NewStore(db), file: fi, refs: 1}
	return d.current, nil
}

// retireCurrent supersedes the open handle, reporting the close error when
// the close happened here. With requests in flight the close is deferred to
// the last of them, so a nil return means "closed cleanly, or not yet".
func (d *Daemon) retireCurrent() error {
	if d.current == nil {
		return nil
	}
	err := d.current.retire()
	d.current = nil
	return err
}

func (d *Daemon) unavailableReason() string {
	if d.notReady != "" {
		return d.notReady
	}
	return "the daemon has no embedding model configured"
}

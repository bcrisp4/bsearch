// Package server is the daemon's HTTP transport: routing, request decoding,
// the JSON error envelope, and the timeouts that keep a slow inference server
// from becoming a hung client. It holds no storage or inference detail — the
// Backend port supplies both — so handlers are testable without a database.
//
// It also knows nothing about how its listener was made, which is what lets a
// TCP listener (with an auth story) be added later without touching this
// package (ADR 0009).
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/bcrisp4/bsearch/internal/search"
)

const (
	// MaxRequestBytes caps a request body. A search request is a query and
	// a few numbers; anything larger is a mistake or an attack.
	MaxRequestBytes = 64 << 10

	// requestTimeout bounds one request's work. It sits well below
	// writeTimeout so the handler's own deadline always fires first and the
	// client receives a 504 with a message, rather than a dead connection.
	// The embedding server may be cold-loading a model, so it is generous.
	requestTimeout = 30 * time.Second

	// Connection timeouts. writeTimeout covers the whole handler, so it must
	// exceed requestTimeout with room to spare.
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 45 * time.Second
	idleTimeout       = 120 * time.Second

	// drainTimeout is how long shutdown waits for in-flight requests. Kept
	// under launchd's default 20s ExitTimeOut so a restart is never killed
	// mid-drain, and under requestTimeout so a stuck search cannot hold the
	// daemon open for its full deadline.
	drainTimeout = 10 * time.Second

	// defaultMaxInFlight bounds concurrent searches. Brute-force KNN is
	// CPU-bound and holds one of a small number of reader connections;
	// unbounded concurrency turns a latency SLO into a queue and burns
	// battery for no throughput (DESIGN.md: Constraints).
	defaultMaxInFlight = 2
)

// Backend is everything the transport needs from the daemon. Implementations
// own the index lifecycle (including opening it lazily), so a handler never
// has to reason about whether there is a database yet.
type Backend interface {
	// Search answers a query, or reports why it cannot. Failures should
	// wrap the internal/search sentinels so they classify correctly.
	Search(ctx context.Context, req search.Request) (search.Response, error)
	// Status reports index readiness and per-state document counts. An
	// error is reported to the caller as the reason the index is not
	// ready, never as a failed request.
	Status(ctx context.Context) (IndexStatus, error)
}

// Options configures a Server.
type Options struct {
	Backend Backend
	// Socket and DBPath are reported by /v1/status so a user who reaches
	// the daemon can see which files it is actually using.
	Socket string
	DBPath string
	// Indexing reports the background indexing loop's state. Nil means this
	// daemon has no indexing side to report on, and the field is omitted.
	//
	// A function rather than a port: it reads an in-memory snapshot, so it
	// cannot fail and has nothing to cancel. Taking the mapping as a closure
	// is also what keeps the transport from importing the scheduler — the
	// same separation that keeps it free of storage.
	Indexing func() IndexingStatus
	// Logger receives operational events only — never query text or
	// document content, at any level (DESIGN.md: Privacy).
	Logger *slog.Logger
	// MaxInFlight bounds concurrent searches; zero means the default.
	MaxInFlight int
	// Started is the daemon's start time, for uptime. Zero means now.
	Started time.Time
}

// Server is the daemon's HTTP transport.
type Server struct {
	backend  Backend
	indexing func() IndexingStatus
	socket   string
	dbPath   string
	log      *slog.Logger
	started  time.Time
	// slots bounds concurrent searches. A buffered channel rather than a
	// semaphore type: the zero-dependency version of the same thing.
	slots chan struct{}
}

// New builds a Server. It does not listen; pass a listener to Serve.
func New(opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	inFlight := opts.MaxInFlight
	if inFlight <= 0 {
		inFlight = defaultMaxInFlight
	}
	started := opts.Started
	if started.IsZero() {
		started = time.Now()
	}
	return &Server{
		backend:  opts.Backend,
		indexing: opts.Indexing,
		socket:   opts.Socket,
		dbPath:   opts.DBPath,
		log:      log,
		started:  started,
		slots:    make(chan struct{}, inFlight),
	}
}

// Handler returns the routed handler with middleware applied. Exposed for
// tests; Serve wires it into a configured http.Server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Each route is registered twice: the method pattern serves it, and the
	// bare pattern catches every other verb so a 405 carries the JSON
	// envelope instead of net/http's plain text.
	mux.HandleFunc("POST /v1/search", s.handleSearch)
	mux.HandleFunc("/v1/search", s.methodNotAllowed(http.MethodPost))
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("/v1/status", s.methodNotAllowed(http.MethodGet))
	mux.HandleFunc("/", s.notFound)

	// Logging is outermost so a panicking request still produces a request
	// line, and so the recover handler sees the context carrying the ID the
	// client was given — an ID nothing can be correlated with is no ID.
	return s.logRequests(s.recoverPanics(mux))
}

// Serve runs until ctx is cancelled, then drains. Ordering matters: stop
// accepting and let in-flight requests finish, force the stragglers when the
// drain budget runs out, and only then let the caller close the index —
// handlers are still running during the drain.
//
// The listener is never unlinked here. Go's UnixListener unlinks on Close,
// and a second unlink can delete a successor daemon's socket when launchd
// restarts us (ADR 0009).
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	// Handlers inherit this context, so cancelling it aborts work that
	// outlived the drain. http.Server.Shutdown does not cancel handler
	// contexts by itself.
	baseCtx, cancelHandlers := context.WithCancel(context.Background())
	defer cancelHandlers()

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    http.DefaultMaxHeaderBytes,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	s.log.Info("shutting down", "drain_timeout", drainTimeout)
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), drainTimeout)
	defer cancelDrain()
	if err := srv.Shutdown(drainCtx); err != nil {
		s.log.Warn("drain did not complete; closing connections", "error", err)
		cancelHandlers()
		if err := srv.Close(); err != nil {
			return err
		}
	}
	<-serveErr // Serve always returns once the listener is closed.
	return nil
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	req, ok := s.decodeSearch(w, r)
	if !ok {
		return
	}

	// Bound concurrency without queueing: a caller told "busy" immediately
	// can decide what to do, where one parked behind a full-corpus scan just
	// experiences the daemon as slow.
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		writeError(w, s.log, http.StatusServiceUnavailable, codeBusy,
			"the daemon is already running as many searches as it will run at once — try again shortly")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	resp, err := s.backend.Search(ctx, req)
	if err != nil {
		status, code, message := classify(err)
		if status == http.StatusInternalServerError {
			// The detail is withheld from the client, so it has to reach
			// the log or it is lost entirely.
			s.log.Error("search failed", "error", err)
		}
		writeError(w, s.log, status, code, message)
		return
	}
	writeJSON(w, s.log, resp)
}

// decodeSearch reads the request body strictly: unknown fields are a 400, not
// a silent no-op, so a misspelled field is loud (ADR 0009).
func (s *Server) decodeSearch(w http.ResponseWriter, r *http.Request) (search.Request, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
	dec := newStrictDecoder(r.Body)

	var req search.Request
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, s.log, http.StatusRequestEntityTooLarge, codeTooLarge,
				"the request body is too large")
			return search.Request{}, false
		}
		// The decoder's message names the offending field or offset, which
		// is what makes a typo fixable. It describes the request, never its
		// contents, so it is safe to return.
		writeError(w, s.log, http.StatusBadRequest, codeBadRequest,
			"could not read the request: "+err.Error())
		return search.Request{}, false
	}
	if !expectEOF(dec) {
		writeError(w, s.log, http.StatusBadRequest, codeBadRequest,
			"the request body has trailing content after the JSON object")
		return search.Request{}, false
	}
	return req, true
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, s.log, http.StatusNotFound, codeNotFound,
		"no such endpoint: "+r.URL.Path)
}

func (s *Server) methodNotAllowed(allowed string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allowed)
		writeError(w, s.log, http.StatusMethodNotAllowed, codeMethodNotAllowed,
			r.Method+" is not allowed here; use "+allowed)
	}
}

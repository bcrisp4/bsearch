package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/search"
	"github.com/bcrisp4/bsearch/internal/server"
)

type fakeBackend struct {
	resp      search.Response
	err       error
	status    server.IndexStatus
	statusErr error

	mu      sync.Mutex
	gotReq  search.Request
	release chan struct{} // when non-nil, Search blocks until it is closed
	entered chan struct{} // signalled once per Search entry
}

func (f *fakeBackend) Search(ctx context.Context, req search.Request) (search.Response, error) {
	f.mu.Lock()
	f.gotReq = req
	f.mu.Unlock()
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return search.Response{}, ctx.Err()
		}
	}
	if errors.Is(f.err, errPanic) {
		panic(f.err)
	}
	return f.resp, f.err
}

func (f *fakeBackend) Status(context.Context) (server.IndexStatus, error) {
	return f.status, f.statusErr
}

func (f *fakeBackend) request() search.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotReq
}

// newTestServer serves the handler over TCP; the unix socket is exercised
// end-to-end in the command's tests.
func newTestServer(t *testing.T, backend *fakeBackend) *httptest.Server {
	t.Helper()
	return newTestServerWithIndexing(t, backend, nil)
}

// newTestServerWithIndexing is newTestServer with an indexing reporter, for
// the status tests that care about the background loop's half of the payload.
func newTestServerWithIndexing(t *testing.T, backend *fakeBackend, indexing func() server.IndexingStatus) *httptest.Server {
	t.Helper()
	srv := server.New(server.Options{
		Backend:  backend,
		Indexing: indexing,
		Socket:   "/tmp/test.sock",
		DBPath:   "/tmp/test.db",
		// Discard: the request log is asserted separately.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// reply is a finished response: status, headers, and the body already read
// and closed. Helpers return this rather than *http.Response so no test has
// to remember to close a body (and so bodyclose can see that none leak).
type reply struct {
	status int
	header http.Header
	body   []byte
}

func do(t *testing.T, resp *http.Response, err error) reply {
	t.Helper()
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // response fully read below
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: data}
}

func post(t *testing.T, ts *httptest.Server, path, body string) reply {
	t.Helper()
	resp, err := ts.Client().Post(ts.URL+path, "application/json", strings.NewReader(body)) //nolint:bodyclose // do closes it
	return do(t, resp, err)
}

func get(t *testing.T, ts *httptest.Server, path string) reply {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + path) //nolint:bodyclose // do closes it
	return do(t, resp, err)
}

// decodeError asserts the response carries the error envelope and returns it.
func decodeError(t *testing.T, body []byte) (code, message string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body %q is not the error envelope: %v", body, err)
	}
	if env.Error.Code == "" || env.Error.Message == "" {
		t.Fatalf("envelope %q is missing code or message", body)
	}
	return env.Error.Code, env.Error.Message
}

func TestSearchReturnsHits(t *testing.T) {
	backend := &fakeBackend{resp: search.Response{
		Hits:   []search.Hit{{DocID: "d_1", Path: "/notes/a.md", Distance: 0.25}},
		TookMS: 12,
	}}
	ts := newTestServer(t, backend)

	rep := post(t, ts, "/v1/search", `{"query":"heat pump","limit":5}`)
	if rep.status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rep.status, rep.body)
	}
	if got := rep.header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var out search.Response
	if err := json.Unmarshal(rep.body, &out); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rep.body)
	}
	if len(out.Hits) != 1 || out.Hits[0].DocID != "d_1" {
		t.Errorf("hits = %+v, want the backend's single hit", out.Hits)
	}
	if got := backend.request(); got.Query != "heat pump" || got.Limit != 5 {
		t.Errorf("backend saw %+v, want the decoded request", got)
	}
}

func TestSearchAcceptsPublishedFields(t *testing.T) {
	// DESIGN.md publishes mode/summary_level/min_score. A caller written
	// against the design doc must not be rejected for using them, even
	// though this build ignores the last two (ADR 0009).
	backend := &fakeBackend{resp: search.Response{Hits: []search.Hit{}}}
	ts := newTestServer(t, backend)

	rep := post(t, ts, "/v1/search",
		`{"query":"q","limit":3,"mode":"hybrid","summary_level":16,"min_score":0.2}`)
	if rep.status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rep.status, rep.body)
	}
	if got := backend.request(); got.Mode != "hybrid" || got.SummaryLevel != 16 {
		t.Errorf("backend saw %+v, want the published fields passed through", got)
	}
}

func TestSearchRejectsUnknownFields(t *testing.T) {
	// Strict decoding is what makes a misspelled field loud instead of
	// silently ignored (ADR 0009).
	ts := newTestServer(t, &fakeBackend{})

	rep := post(t, ts, "/v1/search", `{"query":"q","limt":3}`)
	if rep.status != http.StatusBadRequest {
		t.Fatalf("status = %d (%s), want 400", rep.status, rep.body)
	}
	code, msg := decodeError(t, rep.body)
	if code != "bad_request" {
		t.Errorf("code = %q, want bad_request", code)
	}
	if !strings.Contains(msg, "limt") {
		t.Errorf("message %q does not name the offending field", msg)
	}
}

func TestSearchRejectsMalformedJSON(t *testing.T) {
	ts := newTestServer(t, &fakeBackend{})

	rep := post(t, ts, "/v1/search", `{"query":`)
	if rep.status != http.StatusBadRequest {
		t.Fatalf("status = %d (%s), want 400", rep.status, rep.body)
	}
	if code, _ := decodeError(t, rep.body); code != "bad_request" {
		t.Errorf("code = %q, want bad_request", code)
	}
}

func TestSearchRejectsOversizedBody(t *testing.T) {
	ts := newTestServer(t, &fakeBackend{})

	huge := fmt.Sprintf(`{"query":%q}`, strings.Repeat("x", server.MaxRequestBytes+1))
	rep := post(t, ts, "/v1/search", huge)
	if rep.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d (%s), want 413", rep.status, rep.body)
	}
	if code, _ := decodeError(t, rep.body); code != "payload_too_large" {
		t.Errorf("code = %q, want payload_too_large", code)
	}
}

func TestSearchErrorMapping(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantSlug string
	}{
		{"invalid", fmt.Errorf("%w: the query is empty", search.ErrInvalidRequest), http.StatusBadRequest, "bad_request"},
		{"not indexed", fmt.Errorf("%w: nothing indexed yet", search.ErrNotIndexed), http.StatusServiceUnavailable, "not_indexed"},
		{"mismatch", fmt.Errorf("%w: model changed", search.ErrIndexMismatch), http.StatusConflict, "index_mismatch"},
		{"embedder", fmt.Errorf("%w: connection refused", search.ErrEmbedder), http.StatusBadGateway, "embedder_unavailable"},
		{"timeout", context.DeadlineExceeded, http.StatusGatewayTimeout, "timeout"},
		{"unclassified", errors.New("disk gone"), http.StatusInternalServerError, "internal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t, &fakeBackend{err: tt.err})

			rep := post(t, ts, "/v1/search", `{"query":"q"}`)
			if rep.status != tt.wantCode {
				t.Fatalf("status = %d (%s), want %d", rep.status, rep.body, tt.wantCode)
			}
			code, msg := decodeError(t, rep.body)
			if code != tt.wantSlug {
				t.Errorf("code = %q, want %q", code, tt.wantSlug)
			}
			// The classifier prefix is for transports, not for people.
			if strings.HasPrefix(msg, "invalid search request:") {
				t.Errorf("message %q still carries the sentinel prefix", msg)
			}
		})
	}
}

func TestInternalErrorsAreNotLeaked(t *testing.T) {
	// An unclassified error may carry paths or driver internals; the client
	// gets a generic message and the detail goes to the log.
	ts := newTestServer(t, &fakeBackend{err: errors.New("disk gone at /Users/ben/secret.db")})

	rep := post(t, ts, "/v1/search", `{"query":"q"}`)
	if _, msg := decodeError(t, rep.body); strings.Contains(msg, "secret.db") {
		t.Errorf("message %q leaks internal detail", msg)
	}
}

func TestStatusReportsDaemonAndIndex(t *testing.T) {
	backend := &fakeBackend{status: server.IndexStatus{
		Ready:     true,
		Model:     "test-model",
		Dims:      768,
		Documents: map[string]int{"indexed": 192, "failed": 0},
	}}
	ts := newTestServer(t, backend)

	rep := get(t, ts, "/v1/status")
	if rep.status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rep.status, rep.body)
	}
	var out struct {
		Version string             `json:"version"`
		PID     int                `json:"pid"`
		UptimeS int64              `json:"uptime_s"`
		Socket  string             `json:"socket"`
		DBPath  string             `json:"db_path"`
		Index   server.IndexStatus `json:"index"`
	}
	if err := json.Unmarshal(rep.body, &out); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rep.body)
	}
	if out.Version == "" || out.PID == 0 {
		t.Errorf("status = %+v, want version and pid populated", out)
	}
	if out.UptimeS < 0 {
		t.Errorf("uptime_s = %d, want >= 0", out.UptimeS)
	}
	if out.Socket != "/tmp/test.sock" || out.DBPath != "/tmp/test.db" {
		t.Errorf("status = %+v, want the configured paths", out)
	}
	if !out.Index.Ready || out.Index.Model != "test-model" || out.Index.Dims != 768 {
		t.Errorf("index = %+v, want the backend's report", out.Index)
	}
	if out.Index.Documents["indexed"] != 192 {
		t.Errorf("documents = %v, want the backend's counts", out.Index.Documents)
	}
}

func TestStatusAnswersWhenNothingIsIndexed(t *testing.T) {
	ts := newTestServer(t, &fakeBackend{status: server.IndexStatus{
		Ready:  false,
		Reason: "no index database at /tmp/test.db",
	}})

	rep := get(t, ts, "/v1/status")
	// Status is the diagnostic of last resort: it must answer even when
	// nothing else can.
	if rep.status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200 even with no index", rep.status, rep.body)
	}
	var out struct {
		Index server.IndexStatus `json:"index"`
	}
	if err := json.Unmarshal(rep.body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Index.Ready {
		t.Error("index.ready = true, want false")
	}
	if out.Index.Reason == "" {
		t.Error("index.reason is empty; want it to say why")
	}
}

// The queue, failure and footprint fields the CLI renders have to survive the
// round trip; a status that reports counts but not the backlog cannot tell a
// draining queue from a stuck one.
func TestStatusReportsQueueFailuresAndFootprint(t *testing.T) {
	ts := newTestServer(t, &fakeBackend{status: server.IndexStatus{
		Ready:     true,
		Documents: map[string]int{"indexed": 192, "failed": 2},
		Queue:     &server.QueueStatus{Pending: 37, Retrying: 2},
		Failures: []server.FailureGroup{
			{Reason: "not valid UTF-8", Documents: 2, ExamplePath: "/tmp/legacy.txt"},
		},
		Disk: &server.DiskUsage{DBBytes: 400, WALBytes: 12, TotalBytes: 412},
	}})

	rep := get(t, ts, "/v1/status")
	if rep.status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rep.status, rep.body)
	}
	var out struct {
		Index server.IndexStatus `json:"index"`
	}
	if err := json.Unmarshal(rep.body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Index.Queue == nil || *out.Index.Queue != (server.QueueStatus{Pending: 37, Retrying: 2}) {
		t.Errorf("queue = %+v, want the backend's backlog", out.Index.Queue)
	}
	if len(out.Index.Failures) != 1 || out.Index.Failures[0].Documents != 2 {
		t.Errorf("failures = %+v, want the backend's one group", out.Index.Failures)
	}
	if out.Index.Disk == nil || out.Index.Disk.TotalBytes != 412 {
		t.Errorf("disk = %+v, want the backend's footprint", out.Index.Disk)
	}
}

func TestStatusReportsIndexingActivity(t *testing.T) {
	scanned := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	ts := newTestServerWithIndexing(t, &fakeBackend{}, func() server.IndexingStatus {
		return server.IndexingStatus{
			Running:            true,
			Gate:               "deferred: on battery",
			Deferring:          true,
			LastScan:           &scanned,
			ScanErrors:         2,
			ScanReachedNothing: true,
			PathErrors:         []server.PathError{{Path: "/Users/ben/Documents", Error: "operation not permitted"}},
			Totals:             server.IndexingTotals{Indexed: 12, Retried: 1},
		}
	})

	rep := get(t, ts, "/v1/status")
	if rep.status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rep.status, rep.body)
	}
	var out struct {
		Indexing *server.IndexingStatus `json:"indexing"`
	}
	if err := json.Unmarshal(rep.body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Indexing == nil {
		t.Fatalf("no indexing half in %s", rep.body)
	}
	if out.Indexing.Gate != "deferred: on battery" || !out.Indexing.Deferring {
		t.Errorf("indexing = %+v, want the gate reported verbatim", out.Indexing)
	}
	if out.Indexing.LastScan == nil || !out.Indexing.LastScan.Equal(scanned) {
		t.Errorf("last_scan = %v, want %v", out.Indexing.LastScan, scanned)
	}
	if len(out.Indexing.PathErrors) != 1 || !out.Indexing.ScanReachedNothing {
		t.Errorf("indexing = %+v, want the scan's path errors; a count alone is not a diagnosis", out.Indexing)
	}
	if out.Indexing.Totals.Indexed != 12 {
		t.Errorf("totals = %+v, want this process's counters", out.Indexing.Totals)
	}
}

// A timestamp that has not happened must be absent, not the zero Time: year 1
// reads as an answer rather than as the absence of one.
func TestStatusOmitsTimestampsThatHaveNotHappened(t *testing.T) {
	ts := newTestServerWithIndexing(t, &fakeBackend{}, func() server.IndexingStatus {
		return server.IndexingStatus{Running: true}
	})

	rep := get(t, ts, "/v1/status")
	for _, field := range []string{"last_scan", "last_cycle", "last_progress"} {
		if strings.Contains(string(rep.body), field) {
			t.Errorf("body contains %q before it has happened: %s", field, rep.body)
		}
	}
}

// "Nothing is indexed" and "nothing is indexing" are different problems. A
// daemon whose scheduler never started has to say so where the user is
// looking, not only in its log.
func TestStatusReportsIndexingThatIsNotRunning(t *testing.T) {
	ts := newTestServerWithIndexing(t, &fakeBackend{}, func() server.IndexingStatus {
		return server.IndexingStatus{Running: false, Reason: "no embedding model is configured"}
	})

	rep := get(t, ts, "/v1/status")
	var out struct {
		Indexing *server.IndexingStatus `json:"indexing"`
	}
	if err := json.Unmarshal(rep.body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Indexing == nil || out.Indexing.Running {
		t.Fatalf("indexing = %+v, want running=false", out.Indexing)
	}
	if out.Indexing.Reason != "no embedding model is configured" {
		t.Errorf("reason = %q, want the reason indexing is off", out.Indexing.Reason)
	}
}

func TestStatusOmitsIndexingWhenThereIsNone(t *testing.T) {
	ts := newTestServer(t, &fakeBackend{})

	rep := get(t, ts, "/v1/status")
	if strings.Contains(string(rep.body), "indexing") {
		t.Errorf("body mentions indexing with no reporter wired: %s", rep.body)
	}
}

func TestStatusReportsBackendFailureAsAReason(t *testing.T) {
	// Same principle: a status endpoint that 500s when the index is broken
	// withholds exactly the information the user needs.
	ts := newTestServer(t, &fakeBackend{statusErr: errors.New("database is locked")})

	rep := get(t, ts, "/v1/status")
	if rep.status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rep.status, rep.body)
	}
	var out struct {
		Index server.IndexStatus `json:"index"`
	}
	if err := json.Unmarshal(rep.body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Index.Ready || !strings.Contains(out.Index.Reason, "database is locked") {
		t.Errorf("index = %+v, want ready=false and the failure as the reason", out.Index)
	}
}

func TestUnknownRouteReturnsJSON(t *testing.T) {
	// net/http's own 404 is plain text, which would break the envelope the
	// client parses.
	ts := newTestServer(t, &fakeBackend{})

	rep := get(t, ts, "/v1/nope")
	if rep.status != http.StatusNotFound {
		t.Fatalf("status = %d (%s), want 404", rep.status, rep.body)
	}
	if code, _ := decodeError(t, rep.body); code != "not_found" {
		t.Errorf("code = %q, want not_found", code)
	}
}

func TestWrongMethodReturnsJSON(t *testing.T) {
	ts := newTestServer(t, &fakeBackend{})

	rep := get(t, ts, "/v1/search")
	if rep.status != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d (%s), want 405", rep.status, rep.body)
	}
	if code, _ := decodeError(t, rep.body); code != "method_not_allowed" {
		t.Errorf("code = %q, want method_not_allowed", code)
	}
	if got := rep.header.Get("Allow"); got == "" {
		t.Error("405 response has no Allow header")
	}
}

func TestConcurrentSearchesAreBounded(t *testing.T) {
	// Brute-force KNN is CPU-bound and holds a reader connection; unbounded
	// concurrency blows both the latency SLO and the idle-power constraint.
	backend := &fakeBackend{
		release: make(chan struct{}),
		entered: make(chan struct{}, 8),
	}
	srv := server.New(server.Options{
		Backend:     backend,
		MaxInFlight: 1,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	go func() {
		resp, err := ts.Client().Post(ts.URL+"/v1/search", "application/json", strings.NewReader(`{"query":"q"}`))
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-backend.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first search never reached the backend")
	}

	rep := post(t, ts, "/v1/search", `{"query":"q"}`)
	close(backend.release)
	if rep.status != http.StatusServiceUnavailable {
		t.Fatalf("second concurrent search: status = %d (%s), want 503", rep.status, rep.body)
	}
	if code, _ := decodeError(t, rep.body); code != "busy" {
		t.Errorf("code = %q, want busy", code)
	}
}

func TestStatusIsNotBoundedByTheSearchLimit(t *testing.T) {
	// Status must stay answerable while every search slot is occupied —
	// "the daemon is saturated" is precisely when you ask it what's wrong.
	backend := &fakeBackend{release: make(chan struct{}), entered: make(chan struct{}, 8)}
	srv := server.New(server.Options{
		Backend:     backend,
		MaxInFlight: 1,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	go func() {
		resp, err := ts.Client().Post(ts.URL+"/v1/search", "application/json", strings.NewReader(`{"query":"q"}`))
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-backend.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first search never reached the backend")
	}

	rep := get(t, ts, "/v1/status")
	close(backend.release)
	if rep.status != http.StatusOK {
		t.Fatalf("status while saturated = %d (%s), want 200", rep.status, rep.body)
	}
}

func TestPanicBecomesAnEnvelope(t *testing.T) {
	// net/http recovers per-connection panics but the client sees a bare
	// EOF; a daemon owes it an answer.
	ts := newTestServer(t, &fakeBackend{err: errPanic})

	rep := post(t, ts, "/v1/search", `{"query":"q"}`)
	if rep.status != http.StatusInternalServerError {
		t.Fatalf("status = %d (%s), want 500", rep.status, rep.body)
	}
	if code, _ := decodeError(t, rep.body); code != "internal" {
		t.Errorf("code = %q, want internal", code)
	}
}

// errPanic makes the fake backend panic instead of returning.
var errPanic = errors.New("panic sentinel")

func TestRequestLogNeverCarriesQueryText(t *testing.T) {
	// DESIGN.md Privacy: query text is never logged, at any level.
	var logged strings.Builder
	srv := server.New(server.Options{
		Backend: &fakeBackend{resp: search.Response{Hits: []search.Hit{}}},
		Logger:  slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := ts.Client().Post(ts.URL+"/v1/search", "application/json",
		strings.NewReader(`{"query":"my most private question"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if strings.Contains(logged.String(), "private question") {
		t.Errorf("request log contains query text:\n%s", logged.String())
	}
	if !strings.Contains(logged.String(), "/v1/search") {
		t.Errorf("request log does not record the route:\n%s", logged.String())
	}
}

func TestPanicIsLoggedWithTheRequestIDTheClientSees(t *testing.T) {
	// The correlation ID exists so a user reporting a failure can point at
	// a log line. If the recover handler sits outside the logging
	// middleware it sees the pre-ID request and logs an empty one, and the
	// panicking request produces no request line at all.
	var logged strings.Builder
	srv := server.New(server.Options{
		Backend: &fakeBackend{err: errPanic},
		Logger:  slog.New(slog.NewTextHandler(&logged, nil)),
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := ts.Client().Post(ts.URL+"/v1/search", "application/json", strings.NewReader(`{"query":"q"}`))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Header.Get("X-Request-Id")
	_ = resp.Body.Close()
	if id == "" {
		t.Fatal("no X-Request-Id header")
	}
	if !strings.Contains(logged.String(), id) {
		t.Errorf("log does not contain the request ID %q the client was given:\n%s", id, logged.String())
	}
	// The request line must exist too, and record the 500.
	if !strings.Contains(logged.String(), "status=500") {
		t.Errorf("panicking request produced no request log line:\n%s", logged.String())
	}
}

func TestClientDisconnectIsNotAnInternalError(t *testing.T) {
	// Ctrl-C on the CLI cancels the request context. Treating that as a
	// server fault fills the daemon's log with ERROR lines for something
	// entirely routine.
	var logged strings.Builder
	srv := server.New(server.Options{
		Backend: &fakeBackend{err: context.Canceled},
		Logger:  slog.New(slog.NewTextHandler(&logged, nil)),
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	rep := post(t, ts, "/v1/search", `{"query":"q"}`)
	if rep.status == http.StatusInternalServerError {
		t.Errorf("status = 500 for a cancelled request; want it distinguished")
	}
	if strings.Contains(logged.String(), "level=ERROR") {
		t.Errorf("a client disconnect was logged at ERROR:\n%s", logged.String())
	}
}

func TestSearchRejectsTrailingContent(t *testing.T) {
	// Strict decoding is the contract, so a body that isn't exactly one JSON
	// object is a 400. json.Decoder.More() catches a second document and
	// trailing garbage, but returns false for a stray closing brace — the
	// check has to be "the next decode is EOF", not "no more elements".
	for _, body := range []string{
		`{"query":"q"} {"query":"two"}`,
		`{"query":"q"} garbage`,
		`{"query":"q"}}`,
		`{"query":"q"}]`,
	} {
		t.Run(body, func(t *testing.T) {
			ts := newTestServer(t, &fakeBackend{})

			rep := post(t, ts, "/v1/search", body)
			if rep.status != http.StatusBadRequest {
				t.Fatalf("status = %d (%s), want 400", rep.status, rep.body)
			}
			if code, _ := decodeError(t, rep.body); code != "bad_request" {
				t.Errorf("code = %q, want bad_request", code)
			}
		})
	}
}

func TestSearchAcceptsTrailingWhitespace(t *testing.T) {
	// Curl and shell heredocs add a trailing newline; that is not garbage.
	ts := newTestServer(t, &fakeBackend{resp: search.Response{Hits: []search.Hit{}}})

	rep := post(t, ts, "/v1/search", "{\"query\":\"q\"}\n\n")
	if rep.status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rep.status, rep.body)
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/config"
	"github.com/bcrisp4/bsearch/internal/timemachine"
)

// shortSocketPath returns a socket path in a very short temp directory.
// t.TempDir() is unusable here: $TMPDIR alone is ~49 bytes on macOS and the
// test name is appended, which overruns sockaddr_un's 104-byte sun_path and
// fails bind() with a bare EINVAL.
func shortSocketPath(t *testing.T) (socketPath, lockPath string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bs") //nolint:usetesting // t.TempDir() is too long for sun_path
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("RemoveAll: %v", err)
		}
	})
	return filepath.Join(dir, "s.sock"), filepath.Join(dir, "s.lock")
}

// socketClient dials the daemon's unix socket over HTTP. The host in the URL
// is ignored by the transport but must be present for a valid request.
func socketClient(path string) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			},
		},
	}
}

// daemonFixture starts a daemon in the background and returns a client for
// it plus its database path. The daemon is stopped when the test ends.
type daemonFixture struct {
	client     *http.Client
	dbPath     string
	socketPath string
	lockPath   string
	cfgPath    string

	done    chan error
	wg      *sync.WaitGroup
	stopped bool
}

// stop shuts the daemon down and waits for it. Idempotent, so a test can stop
// a daemon early — restarting one against the same database is the only way
// to exercise what a configuration change does — and the cleanup still runs.
//
// Only one daemon may be running at a time: shutdown signals this process, so
// a second daemon would be caught by the same signal. That is a property of
// running the daemon in-process (issue #54), not of the daemon.
func (f *daemonFixture) stop(t *testing.T) {
	t.Helper()
	if f.stopped {
		return
	}
	f.stopped = true
	// SIGTERM is the shutdown path launchd uses; exercise that one.
	if err := signalSelf(); err != nil {
		t.Errorf("signal self: %v", err)
	}
	select {
	case err := <-f.done:
		if err != nil {
			t.Errorf("serve returned %v, want a clean shutdown", err)
		}
	case <-time.After(20 * time.Second):
		t.Error("serve did not shut down")
	}
	f.wg.Wait()
}

// waitForIndexed blocks until the daemon has indexed at least n contents by
// itself. Nothing runs an indexing command — that the daemon gets there on
// its own is the property under test.
func (f *daemonFixture) waitForIndexed(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		status, body := f.get(t, "/v1/status")
		if status == http.StatusOK {
			var out struct {
				Index struct {
					Content map[string]int `json:"content"`
				} `json:"index"`
			}
			if err := json.Unmarshal(body, &out); err == nil {
				if out.Index.Content["indexed"] >= n {
					return
				}
			}
			last = string(body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("daemon did not index %d contents on its own; last status was %s", n, last)
}

// waitForReady blocks until the daemon reports a usable index. Distinct from
// waitForIndexed: an empty corpus becomes ready without ever indexing
// anything.
func (f *daemonFixture) waitForReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		status, body := f.get(t, "/v1/status")
		if status == http.StatusOK {
			var out struct {
				Index struct {
					Ready bool `json:"ready"`
				} `json:"index"`
			}
			if err := json.Unmarshal(body, &out); err == nil && out.Index.Ready {
				return
			}
			last = string(body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("daemon never became ready; last status was %s", last)
}

// TestMain keeps SIGTERM from killing the test binary. The daemon tests signal
// this process to exercise the production shutdown path (signal.NotifyContext
// in runServe) rather than a test-only hook, and a signal that arrives while
// no daemon is running would otherwise terminate the run.
//
// Signalling the test process itself is the compromise of running the daemon
// in-process; issue #54 covers driving a real child process instead, which
// would retire this.
func TestMain(m *testing.M) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM)
	os.Exit(m.Run())
}

// startDaemon runs a daemon over a one-document corpus. The daemon indexes it
// by itself; a test that needs that to have happened calls waitForIndexed.
func startDaemon(t *testing.T) *daemonFixture {
	t.Helper()
	dir := t.TempDir()
	corpus := writeTestCorpus(t, dir, map[string]string{"alpha.md": "# Alpha\n\nalpha document body\n"})
	srv := fakeEmbeddingsServer(t, contentVec)
	return startDaemonWith(t, writeTestConfig(t, dir, corpus, srv.URL), filepath.Join(dir, "data", "bsearch.db"))
}

// startDaemonOverAnEmptyCorpus runs a daemon with nothing to index, which is
// the only way to observe a running daemon that has no vectors now that
// indexing is not something the user triggers.
func startDaemonOverAnEmptyCorpus(t *testing.T) *daemonFixture {
	t.Helper()
	dir := t.TempDir()
	corpus := filepath.Join(dir, "corpus")
	if err := os.MkdirAll(corpus, 0o700); err != nil {
		t.Fatalf("make empty corpus: %v", err)
	}
	srv := fakeEmbeddingsServer(t, contentVec)
	return startDaemonWith(t, writeTestConfig(t, dir, corpus, srv.URL), filepath.Join(dir, "data", "bsearch.db"))
}

// startDaemonWith runs a daemon over an explicit config and database, so a
// test can point the daemon at a configuration that disagrees with the index.
func startDaemonWith(t *testing.T, cfgPath, dbPath string) *daemonFixture {
	t.Helper()
	socketPath, lockPath := shortSocketPath(t)

	full := []string{
		"serve",
		"--config", cfgPath,
		"--db", dbPath,
		"--socket", socketPath,
		"--lock", lockPath,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	done := make(chan error, 1)
	go func() {
		defer wg.Done()
		done <- run(full, io.Discard)
	}()

	client := socketClient(socketPath)
	f := &daemonFixture{
		client: client, dbPath: dbPath, socketPath: socketPath,
		lockPath: lockPath, cfgPath: cfgPath, done: done, wg: &wg,
	}
	t.Cleanup(func() { f.stop(t) })

	waitForDaemon(t, client)
	return f
}

// waitForDaemon blocks until /v1/status answers.
func waitForDaemon(t *testing.T, client *http.Client) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://bsearch/v1/status") //nolint:bodyclose // closed below
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("daemon never started listening")
}

func (f *daemonFixture) get(t *testing.T, path string) (int, []byte) {
	t.Helper()
	resp, err := f.client.Get("http://bsearch" + path) //nolint:bodyclose // closed below
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // fully read below
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body
}

func (f *daemonFixture) post(t *testing.T, path, payload string) (int, []byte) {
	t.Helper()
	resp, err := f.client.Post("http://bsearch"+path, "application/json", strings.NewReader(payload)) //nolint:bodyclose // closed below
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // fully read below
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body
}

func TestServeSocketPermissions(t *testing.T) {
	f := startDaemon(t)

	// 0600 on the socket is the entire access-control story in v1.
	fi, err := os.Lstat(f.socketPath)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("socket mode = %o, want 600", got)
	}
}

// The daemon keeps its index out of Time Machine (DESIGN.md: Data retention;
// ADR 0017). $HOME is redirected so the assertion is about a directory this
// test owns, and so that running the suite never touches the real one.
//
// The data directory is deliberately *not* created first. On a first run
// nothing has necessarily made it — the socket lives elsewhere here, exactly
// as it does whenever --socket is overridden, and sqlite.Open would not reach
// it until later. Pre-creating it would test a machine that has already run
// the daemon once, which is the one case that was never in doubt.
func TestServeExcludesItsDataDirFromBackups(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := config.DataDir()

	dir := t.TempDir()
	corpus := writeTestCorpus(t, dir, map[string]string{"alpha.md": "# Alpha\n\nbody\n"})
	srv := fakeEmbeddingsServer(t, contentVec)
	// The database inside the data directory is what arms the exclusion: the
	// daemon marks the directory it is actually using, never an arbitrary one.
	f := startDaemonWith(t, writeTestConfig(t, dir, corpus, srv.URL), filepath.Join(dataDir, "bsearch.db"))
	f.waitForReady(t)

	excluded, err := timemachine.Excluded(dataDir)
	if err != nil {
		t.Fatalf("Excluded(%s): %v", dataDir, err)
	}
	if !excluded {
		t.Errorf("data directory %s is not excluded from Time Machine backups", dataDir)
	}
}

// A database somewhere other than the data directory leaves backups alone. The
// alternative — excluding the default directory anyway — marks one the daemon
// is not using while the index it *is* using stays in the backup, and
// excluding the database's own parent would turn `--db ~/notes.db` into
// dropping the whole home directory from Time Machine.
func TestServeDoesNotExcludeAnythingForADatabaseElsewhere(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := config.DataDir()

	dir := t.TempDir()
	corpus := writeTestCorpus(t, dir, map[string]string{"alpha.md": "# Alpha\n\nbody\n"})
	srv := fakeEmbeddingsServer(t, contentVec)
	f := startDaemonWith(t, writeTestConfig(t, dir, corpus, srv.URL), filepath.Join(dir, "elsewhere", "bsearch.db"))
	f.waitForReady(t)

	// Not even created: the daemon has no business making a directory it has
	// decided not to use, and its absence is the cleanest evidence that the
	// exclusion path returned before doing anything.
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat(%s) = %v, want the directory not to have been created", dataDir, err)
	}

	for _, path := range []string{filepath.Join(dir, "elsewhere"), dir, home} {
		excluded, err := timemachine.Excluded(path)
		if err != nil {
			t.Fatalf("Excluded(%s): %v", path, err)
		}
		if excluded {
			t.Errorf("%s was excluded from Time Machine backups; nothing should have been", path)
		}
	}
}

// An empty corpus is ready, not broken. The daemon proves the embedding
// endpoint works and establishes the vector space on its first cycle, so
// "ready with nothing in it" is an accurate description of a machine with no
// documents — and it is what lets a dims change on the inference server be
// noticed and repaired later, which needs a probe to detect at all.
func TestServeOverAnEmptyCorpusIsReadyAndEmpty(t *testing.T) {
	f := startDaemonOverAnEmptyCorpus(t)
	f.waitForReady(t)

	status, body := f.get(t, "/v1/status")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", status, body)
	}
	var out struct {
		Version string `json:"version"`
		PID     int    `json:"pid"`
		Index   struct {
			Ready   bool           `json:"ready"`
			Reason  string         `json:"reason"`
			Files   int            `json:"files"`
			Content map[string]int `json:"content"`
		} `json:"index"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if out.Version == "" || out.PID == 0 {
		t.Errorf("status = %+v, want version and pid", out)
	}
	if out.Index.Files != 0 || out.Index.Content["indexed"] != 0 {
		t.Errorf("index = %+v, want nothing discovered or indexed", out.Index)
	}
	// The daemon owns the writer role, so it creates and migrates the
	// database itself (ADR 0012) — there is no longer anything else that
	// could.
	if _, err := os.Stat(f.dbPath); err != nil {
		t.Errorf("serve did not create %s (stat err %v)", f.dbPath, err)
	}
}

// The headline of this change: nothing is run, and the corpus gets indexed.
func TestServeIndexesTheCorpusWithoutAnyCommand(t *testing.T) {
	f := startDaemon(t)
	f.waitForIndexed(t, 1)

	status, body := f.post(t, "/v1/search", `{"query":"alpha"}`)
	if status != http.StatusOK {
		t.Fatalf("search after indexing: status = %d (%s), want 200", status, body)
	}
	var out struct {
		Hits []struct {
			Path     string  `json:"path"`
			Distance float64 `json:"distance"`
		} `json:"hits"`
		TookMS int64 `json:"took_ms"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(out.Hits) == 0 {
		t.Fatal("no hits after indexing")
	}
	if !strings.HasSuffix(out.Hits[0].Path, "alpha.md") {
		t.Errorf("first hit = %q, want alpha.md", out.Hits[0].Path)
	}

	statusCode, statusBody := f.get(t, "/v1/status")
	if statusCode != http.StatusOK {
		t.Fatalf("status = %d (%s)", statusCode, statusBody)
	}
	var st struct {
		Index struct {
			Ready   bool           `json:"ready"`
			Model   string         `json:"model"`
			Dims    int            `json:"dims"`
			Files   int            `json:"files"`
			Content map[string]int `json:"content"`
		} `json:"index"`
	}
	if err := json.Unmarshal(statusBody, &st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !st.Index.Ready || st.Index.Model != "test-model" || st.Index.Dims == 0 {
		t.Errorf("index = %+v, want ready with model and dims", st.Index)
	}
	if st.Index.Files == 0 || st.Index.Content["indexed"] == 0 {
		t.Errorf("index = %+v, want at least one file with indexed content", st.Index)
	}
}

func TestServeRejectsASecondInstance(t *testing.T) {
	f := startDaemon(t)

	err := run([]string{
		"serve",
		"--config", f.cfgPath,
		"--db", f.dbPath,
		"--socket", f.socketPath,
		"--lock", f.lockPath,
	}, io.Discard)
	if err == nil {
		t.Fatal("second serve succeeded; want a single-instance error")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error %q does not say another daemon is running", err)
	}
}

func TestServeBadFlag(t *testing.T) {
	var out strings.Builder
	if err := run([]string{"serve", "--nope"}, &out); err == nil {
		t.Fatal("run(serve --nope) = nil, want a flag error")
	}
}

func TestServeRejectsArguments(t *testing.T) {
	var out strings.Builder
	err := run([]string{"serve", "extra"}, &out)
	if err == nil || !strings.Contains(err.Error(), "no arguments") {
		t.Fatalf("run(serve extra) = %v, want a no-arguments error", err)
	}
}

func TestServeHelp(t *testing.T) {
	var out strings.Builder
	if err := run([]string{"serve", "-h"}, &out); err != nil {
		t.Fatalf("run(serve -h) = %v, want nil (help is not a failure)", err)
	}
	if !strings.Contains(out.String(), "usage: bsearch serve") {
		t.Errorf("run(serve -h) printed %q, want usage text", out.String())
	}
}

func TestServeRejectsBadLogLevel(t *testing.T) {
	var out strings.Builder
	err := run([]string{"serve", "--log-level", "shouty"}, &out)
	if err == nil || !strings.Contains(err.Error(), "log-level") {
		t.Fatalf("run(serve --log-level shouty) = %v, want a log-level error", err)
	}
}

func TestServeStartsWithoutAnEmbeddingModel(t *testing.T) {
	// A LaunchAgent installed before the user has configured a model must
	// not crash-loop: status is the only thing that can explain the problem.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[paths]\ninclude = [\"~\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	socketPath, lockPath := shortSocketPath(t)
	dbPath := filepath.Join(dir, "data", "bsearch.db")

	done := make(chan error, 1)
	go func() {
		done <- run([]string{
			"serve", "--config", cfgPath, "--db", dbPath,
			"--socket", socketPath, "--lock", lockPath,
		}, io.Discard)
	}()

	client := socketClient(socketPath)
	waitForDaemon(t, client)

	resp, err := client.Get("http://bsearch/v1/status") //nolint:bodyclose // closed below
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read: %v", readErr)
	}
	var out struct {
		Index struct {
			Ready  bool   `json:"ready"`
			Reason string `json:"reason"`
		} `json:"index"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if out.Index.Ready || !strings.Contains(out.Index.Reason, "embedding_model") {
		t.Errorf("index = %+v, want not-ready naming the missing setting", out.Index)
	}

	if err := signalSelf(); err != nil {
		t.Fatalf("signal self: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve returned %v, want a clean shutdown", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not shut down")
	}
}

func TestServeUnknownRouteIsJSON(t *testing.T) {
	f := startDaemon(t)

	status, body := f.get(t, "/v1/nope")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d (%s), want 404", status, body)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body %q is not the error envelope: %v", body, err)
	}
	if env.Error.Code != "not_found" {
		t.Errorf("code = %q, want not_found", env.Error.Code)
	}
}

func TestServeCleansUpItsSocket(t *testing.T) {
	socketPath, lockPath := shortSocketPath(t)
	dir := t.TempDir()
	corpus := writeTestCorpus(t, dir, map[string]string{"alpha.md": "alpha"})
	srv := fakeEmbeddingsServer(t, contentVec)
	cfgPath := writeTestConfig(t, dir, corpus, srv.URL)

	done := make(chan error, 1)
	go func() {
		done <- run([]string{
			"serve", "--config", cfgPath, "--db", filepath.Join(dir, "data", "bsearch.db"),
			"--socket", socketPath, "--lock", lockPath,
		}, io.Discard)
	}()
	waitForDaemon(t, socketClient(socketPath))

	if err := signalSelf(); err != nil {
		t.Fatalf("signal self: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not shut down")
	}
	// A leftover socket makes the next start take the stale-recovery path
	// for no reason; the clean path must leave nothing behind.
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Errorf("socket still present after shutdown (stat err %v)", err)
	}
}

// errNoSignal reports a platform without self-signalling, which cannot happen
// on the unix targets this daemon supports.
var errNoSignal = errors.New("cannot signal this process")

// signalSelf sends SIGTERM to the test binary, which is how the daemon under
// test is asked to shut down: runServe installs the same handler a real daemon
// uses, so this exercises the production shutdown path — and SIGTERM
// specifically, because that is the signal launchd sends.
func signalSelf() error {
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		return fmt.Errorf("%w: %w", errNoSignal, err)
	}
	return p.Signal(syscall.SIGTERM)
}

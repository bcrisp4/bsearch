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
	"testing"
	"time"
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
}

// TestMain keeps SIGINT from killing the test binary. The daemon tests signal
// this process to exercise the production shutdown path (signal.NotifyContext
// in runServe) rather than a test-only hook, and a signal that arrives while
// no daemon is running would otherwise terminate the run.
func TestMain(m *testing.M) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	os.Exit(m.Run())
}

// startDaemon runs a daemon over a one-document corpus with nothing indexed.
func startDaemon(t *testing.T) *daemonFixture {
	t.Helper()
	dir := t.TempDir()
	corpus := writeTestCorpus(t, dir, map[string]string{"alpha.md": "# Alpha\n\nalpha document body\n"})
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
	t.Cleanup(func() {
		// SIGTERM is the shutdown path launchd uses; exercise that one.
		if err := syscallKillSelf(); err != nil {
			t.Errorf("signal self: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve returned %v, want a clean shutdown", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("serve did not shut down")
		}
		wg.Wait()
	})

	client := socketClient(socketPath)
	waitForDaemon(t, client)
	return &daemonFixture{client: client, dbPath: dbPath, socketPath: socketPath, lockPath: lockPath, cfgPath: cfgPath}
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

func TestServeStatusBeforeAnythingIsIndexed(t *testing.T) {
	f := startDaemon(t)

	status, body := f.get(t, "/v1/status")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", status, body)
	}
	var out struct {
		Version string `json:"version"`
		PID     int    `json:"pid"`
		Index   struct {
			Ready  bool   `json:"ready"`
			Reason string `json:"reason"`
		} `json:"index"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if out.Version == "" || out.PID == 0 {
		t.Errorf("status = %+v, want version and pid", out)
	}
	if out.Index.Ready || out.Index.Reason == "" {
		t.Errorf("index = %+v, want not-ready with a reason", out.Index)
	}
	// Starting the daemon must not manufacture an empty index.
	if _, err := os.Stat(f.dbPath); !os.IsNotExist(err) {
		t.Errorf("serve created %s (stat err %v); a reader must not", f.dbPath, err)
	}
}

func TestServeSearchBecomesAvailableAfterIndexing(t *testing.T) {
	f := startDaemon(t)

	status, body := f.post(t, "/v1/search", `{"query":"alpha"}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("search before indexing: status = %d (%s), want 503", status, body)
	}

	// Index in-process, exactly as a user would in another terminal.
	if err := run([]string{"index", "--config", f.cfgPath, "--db", f.dbPath}, io.Discard); err != nil {
		t.Fatalf("index: %v", err)
	}

	// The daemon must notice without a restart.
	status, body = f.post(t, "/v1/search", `{"query":"alpha"}`)
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
			Ready     bool           `json:"ready"`
			Model     string         `json:"model"`
			Dims      int            `json:"dims"`
			Documents map[string]int `json:"documents"`
		} `json:"index"`
	}
	if err := json.Unmarshal(statusBody, &st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !st.Index.Ready || st.Index.Model != "test-model" || st.Index.Dims == 0 {
		t.Errorf("index = %+v, want ready with model and dims", st.Index)
	}
	if st.Index.Documents["indexed"] == 0 {
		t.Errorf("documents = %v, want at least one indexed", st.Index.Documents)
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

	if err := syscallKillSelf(); err != nil {
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

	if err := syscallKillSelf(); err != nil {
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

// syscallKillSelf sends SIGTERM to the test binary, which is how the daemon
// under test is asked to shut down: run() installs the same handler a real
// daemon uses, so this exercises the production shutdown path rather than a
// test-only hook.
func syscallKillSelf() error {
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		return fmt.Errorf("%w: %w", errNoSignal, err)
	}
	return p.Signal(os.Interrupt)
}

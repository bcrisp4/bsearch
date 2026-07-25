package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bcrisp4/bsearch/internal/client"
	"github.com/bcrisp4/bsearch/internal/search"
)

// shortSocketPath returns a socket path short enough for sun_path (104 bytes
// on macOS); t.TempDir() plus a test name overruns it and bind() then fails
// with a bare EINVAL.
func shortSocketPath(t *testing.T) string {
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
	return filepath.Join(dir, "s.sock")
}

// serveStub runs handler on a unix socket and returns a client for it.
func serveStub(t *testing.T, handler http.HandlerFunc) (*client.Client, string) {
	t.Helper()
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 0}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return client.New(path), path
}

func TestSearchSendsTheRequestAndReturnsTheBodyVerbatim(t *testing.T) {
	const canned = `{"hits":[{"doc_id":"d_1","path":"/notes/a.md","distance":0.5,"score":9,"summary":"future"}],"took_ms":7}`
	var got search.Request
	c, _ := serveStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/search" {
			t.Errorf("got %s %s, want POST /v1/search", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(canned))
	})

	body, err := c.Search(context.Background(), search.Request{Query: "heat pump", Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got.Query != "heat pump" || got.Limit != 5 {
		t.Errorf("daemon saw %+v, want the request", got)
	}
	// Verbatim matters: fields this build has never heard of (score,
	// summary) must survive to consumers that understand them.
	if string(body) != canned {
		t.Errorf("body = %s, want it byte-identical to the daemon's", body)
	}
}

func TestSearchReportsTheDaemonsError(t *testing.T) {
	c, _ := serveStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"not_indexed","message":"nothing indexed yet — run 'bsearch index' first"}}`))
	})

	_, err := c.Search(context.Background(), search.Request{Query: "q"})
	if err == nil {
		t.Fatal("Search = nil error, want the daemon's error")
	}
	var apiErr *client.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not a *client.Error", err)
	}
	if apiErr.Code != "not_indexed" || apiErr.Status != http.StatusServiceUnavailable {
		t.Errorf("error = %+v, want the envelope's code and status", apiErr)
	}
	// The daemon's prose is what the user sees; the client never rewords it.
	if !strings.Contains(err.Error(), "run 'bsearch index' first") {
		t.Errorf("error %q does not carry the daemon's message", err)
	}
}

func TestNonEnvelopeErrorDegradesToTheStatus(t *testing.T) {
	// A proxy, a future version, or an escaped panic can answer with
	// something else; dumping an unknown body at the user helps nobody.
	c, _ := serveStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>nginx</html>"))
	})

	_, err := c.Search(context.Background(), search.Request{Query: "q"})
	if err == nil {
		t.Fatal("Search = nil error, want an error")
	}
	if strings.Contains(err.Error(), "nginx") {
		t.Errorf("error %q dumps the unknown body", err)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error %q does not name the status", err)
	}
}

func TestMissingSocketSaysTheDaemonIsNotRunning(t *testing.T) {
	c := client.New(shortSocketPath(t)) // never created

	_, err := c.Search(context.Background(), search.Request{Query: "q"})
	if err == nil {
		t.Fatal("Search against a missing socket = nil error")
	}
	if !strings.Contains(err.Error(), "not running") || !strings.Contains(err.Error(), "bsearch serve") {
		t.Errorf("error %q should say the daemon is not running and how to start it", err)
	}
}

func TestStaleSocketIsDistinguishedFromNoSocket(t *testing.T) {
	// A socket left by a daemon that died needs a different fix than no
	// socket at all, and the two are indistinguishable without saying so.
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err = client.New(path).Search(context.Background(), search.Request{Query: "q"})
	if err == nil {
		t.Fatal("Search against a stale socket = nil error")
	}
	if !strings.Contains(err.Error(), "nothing is listening") {
		t.Errorf("error %q does not identify a stale socket", err)
	}
}

func TestStatusFetchesTheStatusDocument(t *testing.T) {
	const canned = `{"version":"test","pid":1,"index":{"ready":true}}`
	c, _ := serveStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/status" {
			t.Errorf("got %s %s, want GET /v1/status", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(canned))
	})

	body, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if string(body) != canned {
		t.Errorf("body = %s, want it verbatim", body)
	}
}

func TestContextCancellationIsHonoured(t *testing.T) {
	c, _ := serveStub(t, func(http.ResponseWriter, *http.Request) {})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Search(ctx, search.Request{Query: "q"}); err == nil {
		t.Error("Search with a cancelled context = nil error")
	}
}

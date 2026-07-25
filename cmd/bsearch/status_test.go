package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/server"
)

// stubStatusDaemon serves one fixed body on a unix socket, so the CLI's
// rendering and passthrough can be tested against payloads a real daemon
// cannot easily be pushed into — including one from a future version.
func stubStatusDaemon(t *testing.T, body string) string {
	t.Helper()
	socketPath, _ := shortSocketPath(t)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return socketPath
}

func runStatusCommand(t *testing.T, args ...string) string {
	t.Helper()
	var out strings.Builder
	if err := run(append([]string{"status"}, args...), &out); err != nil {
		t.Fatalf("status %v: %v", args, err)
	}
	return out.String()
}

// A healthy daemon's report: every section present, counts readable, paths
// abbreviated to ~.
func TestStatusRendersAHealthyDaemon(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	scan := now.Add(-2 * time.Minute)
	progress := now.Add(-4 * time.Minute)
	resp := server.StatusResponse{
		Version: "v0.2.0", PID: 4312, UptimeS: 90421,
		Socket: "/home/bsearch.sock",
		DBPath: "/home/data/bsearch.db",
		Index: server.IndexStatus{
			Ready: true, Model: "embeddinggemma-300m", Dims: 768,
			Documents: map[string]int{"indexed": 1204, "discovered": 35, "chunked": 2},
			Queue:     &server.QueueStatus{Pending: 37},
			Disk:      &server.DiskUsage{DBBytes: 432013312, TotalBytes: 432013312},
		},
		Indexing: &server.IndexingStatus{
			Running: true, Gate: "idle — nothing to index",
			LastScan: &scan, LastProgress: &progress,
			Totals: server.IndexingTotals{Indexed: 1204},
		},
	}

	var out strings.Builder
	writeStatusHuman(&out, resp, "/home", now)
	got := out.String()

	for _, want := range []string{
		"bsearch v0.2.0 — running (pid 4312, up 1d 1h)",
		"~/bsearch.sock",
		"~/data/bsearch.db  (412 MiB)",
		"ready", "yes",
		"embeddinggemma-300m (768d)",
		"1,204 indexed",
		"pending  37",
		"discovered 35 · chunked 2",
		"idle — nothing to index",
		"last scan      2m ago",
		"last progress  4m ago",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
	// Nothing failed and nothing was unreadable: those sections must not
	// appear at all rather than as empty headings.
	for _, unwanted := range []string{"Failures", "Unreadable", "retrying"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("report mentions %q with nothing to report:\n%s", unwanted, got)
		}
	}
}

// The two questions status exists to answer are "is anything indexed" and "is
// anything indexing". They fail independently and must be reported
// independently.
func TestStatusRendersADaemonThatIsNotIndexing(t *testing.T) {
	resp := server.StatusResponse{
		Version: "v0.2.0", PID: 1,
		Index: server.IndexStatus{
			Ready:  false,
			Reason: "no index database at /home/data/bsearch.db yet",
		},
		Indexing: &server.IndexingStatus{
			Running: false, Reason: "no embedding model is configured",
		},
	}

	var out strings.Builder
	writeStatusHuman(&out, resp, "/home", time.Now())
	got := out.String()

	if !strings.Contains(got, "no index database at /home/data/bsearch.db yet") {
		t.Errorf("report does not say why the index is not ready:\n%s", got)
	}
	if !strings.Contains(got, "not running — no embedding model is configured") {
		t.Errorf("report does not say why indexing is off:\n%s", got)
	}
}

// A scan that reached nothing is the signature of a missing Full Disk Access
// grant, and the sample of paths is what makes it diagnosable.
func TestStatusRendersUnreadablePaths(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	scan := now.Add(-90 * time.Second)
	resp := server.StatusResponse{
		Version: "v0.2.0", PID: 1,
		Index: server.IndexStatus{Ready: true},
		Indexing: &server.IndexingStatus{
			Running: true, Gate: "files could not be read — check Full Disk Access",
			LastScan: &scan, ScanErrors: 8, ScanReachedNothing: true,
			PathErrors: []server.PathError{
				{Path: "/home/Documents", Error: "operation not permitted"},
				{Path: "/home/Desktop", Error: "operation not permitted"},
			},
		},
	}

	var out strings.Builder
	writeStatusHuman(&out, resp, "/home", now)
	got := out.String()

	for _, want := range []string{
		"check Full Disk Access",
		"1m 30s ago — 8 path errors, reached no files",
		"Unreadable paths (8)",
		"~/Documents  operation not permitted",
		"… 6 more",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
}

func TestStatusRendersFailureGroups(t *testing.T) {
	resp := server.StatusResponse{
		Version: "v0.2.0", PID: 1,
		Index: server.IndexStatus{
			Ready:     true,
			Documents: map[string]int{"failed": 3},
			Failures: []server.FailureGroup{
				{Reason: "file is not valid UTF-8", Documents: 2, ExamplePath: "/home/notes/legacy.txt"},
				{Reason: "", Documents: 1, ExamplePath: "/home/notes/mystery.md"},
			},
		},
	}

	var out strings.Builder
	writeStatusHuman(&out, resp, "/home", time.Now())
	got := out.String()

	for _, want := range []string{
		"Failures (3)",
		"2  file is not valid UTF-8",
		"~/notes/legacy.txt",
		// A failed row with no recorded reason still has to be listed, or the
		// count and the list disagree.
		"1  no reason recorded",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
}

// A path or a reason is untrusted display text — macOS filenames may hold any
// byte but NUL and '/', and a failure reason can quote a document.
func TestStatusStripsControlCharactersFromUntrustedText(t *testing.T) {
	resp := server.StatusResponse{
		Version: "v0.2.0", PID: 1,
		DBPath: "/home/data/\x1b[31mbsearch.db",
		Index: server.IndexStatus{
			Documents: map[string]int{"failed": 1},
			Failures: []server.FailureGroup{
				{Reason: "bad\x1b[2Jreason", Documents: 1, ExamplePath: "/home/a\nb.md"},
			},
		},
	}

	var out strings.Builder
	writeStatusHuman(&out, resp, "/home", time.Now())
	if got := out.String(); strings.ContainsAny(got, "\x1b") {
		t.Errorf("report carries an escape sequence to the terminal:\n%q", got)
	}
}

// --json is a promise to its callers: the daemon's bytes, not this build's
// understanding of them. A field a newer daemon adds has to survive.
func TestStatusJSONIsAPassthrough(t *testing.T) {
	body := `{"version":"v9.9.9","pid":7,"uptime_s":1,"socket":"/s","db_path":"/db",` +
		`"index":{"ready":true},"future_field":{"nested":[1,2,3]}}`
	socketPath := stubStatusDaemon(t, body)

	got := runStatusCommand(t, "--socket", socketPath, "--json")
	if got != body {
		t.Errorf("--json output = %s, want the daemon's bytes verbatim", got)
	}
	if !json.Valid([]byte(got)) {
		t.Error("--json output is not valid JSON")
	}
}

// The human path must not choke on a payload from a newer daemon either.
func TestStatusHumanIgnoresUnknownFields(t *testing.T) {
	socketPath := stubStatusDaemon(t, `{"version":"v9.9.9","pid":7,"uptime_s":1,`+
		`"index":{"ready":true},"future_field":1}`)

	if got := runStatusCommand(t, "--socket", socketPath); !strings.Contains(got, "bsearch v9.9.9") {
		t.Errorf("output = %q, want a rendered report", got)
	}
}

// Not reaching the daemon is the one failure that exits non-zero, and the
// message has to say what to do about it.
func TestStatusWithoutADaemonFails(t *testing.T) {
	// Short, because a path over sockaddr_un's 104 bytes fails with EINVAL
	// before the socket's absence is ever discovered.
	socketPath, _ := shortSocketPath(t)

	err := run([]string{"status", "--socket", socketPath}, io.Discard)
	if err == nil {
		t.Fatal("status with no daemon succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "not running") {
		t.Errorf("error = %q, want it to say the daemon is not running", err)
	}
}

func TestStatusRejectsArguments(t *testing.T) {
	if err := run([]string{"status", "extra"}, io.Discard); err == nil {
		t.Error("status with a positional argument succeeded, want an error")
	}
}

func TestStatusHelp(t *testing.T) {
	var out strings.Builder
	if err := run([]string{"status", "-h"}, &out); err != nil {
		t.Fatalf("status -h: %v", err)
	}
	if !strings.Contains(out.String(), "usage: bsearch status") {
		t.Errorf("help = %q, want a usage line", out.String())
	}
}

// End to end over the real socket: the daemon indexes a one-document corpus by
// itself, and status reports it.
func TestStatusAgainstARunningDaemon(t *testing.T) {
	f := startDaemon(t)
	f.waitForIndexed(t, 1)

	got := runStatusCommand(t, "--socket", f.socketPath)
	for _, want := range []string{"bsearch ", "Index", "ready", "1 indexed", "Indexing"} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{900 * time.Millisecond, "0s"},
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m 30s"},
		{2 * time.Hour, "2h"},
		{90421 * time.Second, "1d 1h"},
	}
	for _, c := range cases {
		if got := humanDuration(c.in); got != c.want {
			t.Errorf("humanDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KiB"},
		{432013312, "412 MiB"},
		{5 << 30, "5.0 GiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCountSeparatesThousands(t *testing.T) {
	cases := map[int]string{0: "0", 999: "999", 1204: "1,204", 1048576: "1,048,576"}
	for in, want := range cases {
		if got := count(in); got != want {
			t.Errorf("count(%d) = %q, want %q", in, got, want)
		}
	}
}

// A clock that moved backwards must not produce a negative age presented as a
// fact about indexing.
func TestAgoHandlesFutureTimestamps(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	if got := ago(now.Add(time.Hour), now); !strings.Contains(got, "future") {
		t.Errorf("ago(future) = %q, want it called out", got)
	}
	if got := ago(now, now); got != "just now" {
		t.Errorf("ago(now) = %q, want %q", got, "just now")
	}
}

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
			Files:   1268,
			Content: map[string]int{"indexed": 1204, "discovered": 35, "chunked": 2, "failed": 2},
			Unread:  map[string]int{"denied": 0, "dataless": 0, "io_error": 0},
			Queue:   &server.QueueStatus{Pending: 37},
			Disk:    &server.DiskUsage{DBBytes: 432013312, TotalBytes: 432013312},
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
		// Two lines for two populations (ADR 0015): paths and distinct
		// contents rendered side by side would read as a comparable triple.
		"files     1,268",
		"contents  1,204 indexed · 2 failed",
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
	// No failure groups reported and nothing was unread: those sections and
	// lines must not appear at all rather than as empty headings — and the
	// retired "deleted" state must never resurface (ADR 0015: a deleted file
	// is a removed row, not a state).
	for _, unwanted := range []string{"Failures", "Unreadable", "retrying", "unread", "deleted"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("report mentions %q with nothing to report:\n%s", unwanted, got)
		}
	}
}

// A pre-identity-split daemon reports the retired `documents` map and none of
// the fields this build reads. Rendering zeros would report a healthy-looking
// empty index on the one command reached for when something already seems
// wrong — the CLI must say what is actually happening instead.
func TestStatusWarnsWhenTheDaemonIsOlder(t *testing.T) {
	resp := server.StatusResponse{
		Version: "v0.1.0", PID: 1,
		Index: server.IndexStatus{
			Ready:     true,
			Documents: map[string]int{"indexed": 1204, "failed": 2},
		},
	}

	var out strings.Builder
	writeStatusHuman(&out, resp, "/home", time.Now())
	got := out.String()
	if !strings.Contains(got, "older than this CLI") || !strings.Contains(got, "bsearch serve") {
		t.Errorf("report does not warn about the old daemon:\n%s", got)
	}
	// The retired counts must not be rendered as if this build understood
	// them, and there are no new-style counts to show.
	if strings.Contains(got, "contents") {
		t.Errorf("report renders a contents line an old daemon never sent:\n%s", got)
	}
}

// The changed total only earns a line when it is non-zero: it is the visible
// trace of a claim abandoned because the file changed under it.
func TestStatusRendersTheChangedTotalOnlyWhenNonZero(t *testing.T) {
	resp := server.StatusResponse{
		Version: "v0.2.0", PID: 1,
		Index: server.IndexStatus{Ready: true},
		Indexing: &server.IndexingStatus{
			Running: true,
			Totals:  server.IndexingTotals{Indexed: 7, Changed: 3},
		},
	}

	var out strings.Builder
	writeStatusHuman(&out, resp, "/home", time.Now())
	if got := out.String(); !strings.Contains(got, "7 indexed · 0 failed · 0 skipped · 0 retried · 3 changed") {
		t.Errorf("since-start line does not carry the changed total:\n%s", got)
	}

	resp.Indexing.Totals.Changed = 0
	out.Reset()
	writeStatusHuman(&out, resp, "/home", time.Now())
	if got := out.String(); strings.Contains(got, "changed") {
		t.Errorf("since-start line mentions changed with nothing to report:\n%s", got)
	}
}

// The collected total only earns a line when it is non-zero — like the
// re-queued line, it is the daemon explaining background work, and here it
// is the one visible trace of the orphan sweep. A corpus where files keep
// being deleted while this number never moves is the sweep silently failing.
func TestStatusRendersTheCollectedTotalOnlyWhenNonZero(t *testing.T) {
	resp := server.StatusResponse{
		Version: "v0.2.0", PID: 1,
		Index: server.IndexStatus{Ready: true},
		Indexing: &server.IndexingStatus{
			Running: true,
			Totals:  server.IndexingTotals{Indexed: 7, Collected: 5},
		},
	}

	var out strings.Builder
	writeStatusHuman(&out, resp, "/home", time.Now())
	if got := out.String(); !strings.Contains(got, "5 (orphaned content)") || !strings.Contains(got, "collected") {
		t.Errorf("report does not carry the collected line:\n%s", got)
	}

	resp.Indexing.Totals.Collected = 0
	out.Reset()
	writeStatusHuman(&out, resp, "/home", time.Now())
	if got := out.String(); strings.Contains(got, "collected") || strings.Contains(got, "orphaned") {
		t.Errorf("report mentions the sweep with nothing collected:\n%s", got)
	}
}

// Unread files are invisible to search and the counts are the only place that
// says so. All three reasons render whenever any is non-zero: denied is the
// Full Disk Access signal and must never fold into dataless, which is an
// iCloud placeholder skipped by design (ADR 0015).
func TestStatusRendersUnreadFiles(t *testing.T) {
	resp := server.StatusResponse{
		Version: "v0.2.0", PID: 1,
		Index: server.IndexStatus{
			Ready:   true,
			Files:   14,
			Content: map[string]int{"indexed": 11},
			Unread:  map[string]int{"denied": 2, "dataless": 1, "io_error": 0},
		},
	}

	var out strings.Builder
	writeStatusHuman(&out, resp, "/home", time.Now())
	if got := out.String(); !strings.Contains(got, "denied 2 · dataless 1 · io_error 0") {
		t.Errorf("report does not break unread files down by reason:\n%s", got)
	}
}

// The watcher is the difference between "searchable in seconds" and
// "searchable in five minutes", so its state is on the report either way —
// running, off with a reason, or subscribed but never told anything.
func TestStatusRendersTheWatcher(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	event := now.Add(-30 * time.Second)

	for name, tc := range map[string]struct {
		watch *server.WatchStatus
		want  string
	}{
		"running": {
			watch: &server.WatchStatus{
				Running: true, Roots: 1, LastEvent: &event, Reconciled: 12, Deleted: 3,
			},
			want: "1 path watched, last change 30s ago · 12 queued · 3 removed",
		},
		"running but silent": {
			watch: &server.WatchStatus{Running: true, Roots: 2},
			want:  "2 paths watched — no changes seen yet",
		},
		"lost events": {
			watch: &server.WatchStatus{
				Running: true, Roots: 1, LastEvent: &event, Rescans: 2,
			},
			want: "2 full rescans (events were lost)",
		},
		"off": {
			watch: &server.WatchStatus{
				Reason: "filesystem watching is only supported on macOS",
			},
			want: "off — filesystem watching is only supported on macOS (relying on the periodic scan)",
		},
	} {
		t.Run(name, func(t *testing.T) {
			resp := server.StatusResponse{
				Version: "v0.2.0", PID: 1,
				Index:    server.IndexStatus{Ready: true},
				Indexing: &server.IndexingStatus{Running: true, Watch: tc.watch},
			}
			var out strings.Builder
			writeStatusHuman(&out, resp, "/home", now)
			if got := out.String(); !strings.Contains(got, tc.want) {
				t.Errorf("report is missing %q:\n%s", tc.want, got)
			}
		})
	}
}

// An older daemon does not report a watcher at all, and the line must then
// be absent rather than invented.
func TestStatusOmitsTheWatcherWhenUnreported(t *testing.T) {
	resp := server.StatusResponse{
		Version: "v0.2.0", PID: 1,
		Index:    server.IndexStatus{Ready: true},
		Indexing: &server.IndexingStatus{Running: true},
	}
	var out strings.Builder
	writeStatusHuman(&out, resp, "/home", time.Now())
	if got := out.String(); strings.Contains(got, "watching") {
		t.Errorf("report claims something about watching:\n%s", got)
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
			Ready:   true,
			Content: map[string]int{"failed": 3},
			Failures: []server.FailureGroup{
				{Reason: "file is not valid UTF-8", Contents: 2, ExamplePath: "/home/notes/legacy.txt"},
				{Reason: "", Contents: 1, ExamplePath: "/home/notes/mystery.md"},
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

// A grown WAL is a symptom of its own — a reader holding a snapshot open, or
// a checkpoint that has not run — so it must not disappear into the database
// figure in the command meant to diagnose it.
func TestStatusCallsOutAGrownWAL(t *testing.T) {
	resp := server.StatusResponse{
		Version: "v0.2.0", PID: 1, DBPath: "/home/data/bsearch.db",
		Index: server.IndexStatus{
			Disk: &server.DiskUsage{DBBytes: 4096, WALBytes: 98912, TotalBytes: 103008},
		},
	}

	var out strings.Builder
	writeStatusHuman(&out, resp, "/home", time.Now())
	if got := out.String(); !strings.Contains(got, "(101 KiB, 96.6 KiB WAL)") {
		t.Errorf("report does not call out the WAL:\n%s", got)
	}
}

// The daemon reports only the largest few groups. Without a truncation marker
// the list reads as disagreeing with the total above it.
func TestStatusMarksTruncatedFailureGroups(t *testing.T) {
	resp := server.StatusResponse{
		Version: "v0.2.0", PID: 1,
		Index: server.IndexStatus{
			Content: map[string]int{"failed": 50},
			Failures: []server.FailureGroup{
				{Reason: "not valid UTF-8", Contents: 20},
				{Reason: "too large", Contents: 5},
			},
		},
	}

	var out strings.Builder
	writeStatusHuman(&out, resp, "/home", time.Now())
	got := out.String()
	if !strings.Contains(got, "Failures (50)") {
		t.Errorf("heading does not report the whole total:\n%s", got)
	}
	if !strings.Contains(got, "… 25 more") {
		t.Errorf("report does not mark the groups it left out:\n%s", got)
	}
}

// One unreadable directory is one path error.
func TestStatusSingularisesASinglePathError(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	scan := now.Add(-time.Minute)
	resp := server.StatusResponse{
		Version: "v0.2.0", PID: 1,
		Indexing: &server.IndexingStatus{
			Running: true, LastScan: &scan, ScanErrors: 1,
			PathErrors: []server.PathError{{Path: "/home/Documents", Error: "operation not permitted"}},
		},
	}

	var out strings.Builder
	writeStatusHuman(&out, resp, "/home", now)
	if got := out.String(); !strings.Contains(got, "1 path error,") && !strings.Contains(got, "1 path error\n") {
		t.Errorf("report says %q:\n%s", "1 path errors", got)
	}
}

// A path or a reason is untrusted display text — macOS filenames may hold any
// byte but NUL and '/', and a failure reason can quote a document.
func TestStatusStripsControlCharactersFromUntrustedText(t *testing.T) {
	resp := server.StatusResponse{
		Version: "v0.2.0", PID: 1,
		DBPath: "/home/data/\x1b[31mbsearch.db",
		Index: server.IndexStatus{
			Content: map[string]int{"failed": 1},
			Failures: []server.FailureGroup{
				{Reason: "bad\x1b[2Jreason", Contents: 1, ExamplePath: "/home/a\nb.md"},
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
		// A remainder too small to fill the next unit down must not be
		// rendered as a zero of it.
		{88200 * time.Second, "1d"},
		{7205 * time.Second, "2h"},
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

// The two reasons a row leaves the catalog are reported apart. "Gone from
// disk" and "no longer in the configured paths" are both removals, but only
// one of them is something the user did on purpose, and a count that dropped
// is unreadable without knowing which.
func TestStatusSplitsDeletedFromPruned(t *testing.T) {
	scan := time.Now().Add(-time.Minute)
	resp := server.StatusResponse{
		Version: "v0.2.0", PID: 1,
		Index:    server.IndexStatus{Ready: true},
		Indexing: &server.IndexingStatus{Running: true, LastScan: &scan},
	}

	for name, tc := range map[string]struct {
		deleted, pruned int
		want, notWant   string
	}{
		"both":    {3, 4, "3 deleted from disk · 4 no longer in the configured paths", ""},
		"deleted": {3, 0, "3 deleted from disk", "configured paths"},
		"pruned":  {0, 4, "4 no longer in the configured paths", "deleted from disk"},
		"neither": {0, 0, "", "removed"},
	} {
		t.Run(name, func(t *testing.T) {
			resp.Indexing.ScanDeleted, resp.Indexing.ScanPruned = tc.deleted, tc.pruned
			var out strings.Builder
			writeStatusHuman(&out, resp, "/home", time.Now())
			got := out.String()
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("report is missing %q:\n%s", tc.want, got)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Errorf("report mentions %q with nothing to report:\n%s", tc.notWant, got)
			}
		})
	}
}

// The line that carries the whole point of declining. Every other number in
// the report says the scan succeeded and nothing was deleted; without this,
// a corpus whose deletions are silently not being noticed looks identical to
// a healthy one. An unmounted volume is named, because that is a state the
// user can undo.
func TestStatusReportsRowsTheScanDeclinedToReconcile(t *testing.T) {
	scan := time.Now().Add(-time.Minute)
	resp := server.StatusResponse{
		Version: "v0.2.0", PID: 1,
		Index: server.IndexStatus{Ready: true},
		Indexing: &server.IndexingStatus{
			Running: true, LastScan: &scan,
			ScanUnverified: 412000,
			ScanUnmounted:  []string{"/Volumes/Archive"},
		},
	}

	var out strings.Builder
	writeStatusHuman(&out, resp, "/home", time.Now())
	got := out.String()
	for _, want := range []string{
		"not reconciled",
		"412,000 files — deletions are not being noticed there",
		"not mounted: /Volumes/Archive",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}

	// No volume to blame: point at the section that has the detail.
	resp.Indexing.ScanUnmounted = nil
	resp.Indexing.ScanErrors = 2
	resp.Indexing.PathErrors = []server.PathError{{Path: "/home/Documents", Error: "operation not permitted"}}
	out.Reset()
	writeStatusHuman(&out, resp, "/home", time.Now())
	if got := out.String(); !strings.Contains(got, "see unreadable paths below") {
		t.Errorf("report does not point at the unreadable-paths section:\n%s", got)
	}

	// Nothing declined: no line at all. A "not reconciled 0" on every healthy
	// daemon trains the eye to skip the one that matters.
	resp.Indexing.ScanUnverified = 0
	out.Reset()
	writeStatusHuman(&out, resp, "/home", time.Now())
	if got := out.String(); strings.Contains(got, "not reconciled") {
		t.Errorf("report carries the declined line with nothing declined:\n%s", got)
	}
}

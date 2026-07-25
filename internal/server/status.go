package server

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/bcrisp4/bsearch/internal/buildinfo"
)

// The GET /v1/status payload (DESIGN.md: HTTP API). Two halves, from two
// sources that fail independently: `index` is what the database says, supplied
// by the Backend; `indexing` is what the background loop is doing, supplied by
// Options.Indexing and readable even when there is no database at all. That
// split is the point of the endpoint — "nothing is indexed" and "nothing is
// indexing" are different problems with different fixes.
//
// Every field is additive and omitted when absent, so a newer daemon can grow
// the document without breaking a client that copies it through.

// StatusResponse is the GET /v1/status payload.
type StatusResponse struct {
	Version string      `json:"version"`
	PID     int         `json:"pid"`
	UptimeS int64       `json:"uptime_s"`
	Socket  string      `json:"socket"`
	DBPath  string      `json:"db_path"`
	Index   IndexStatus `json:"index"`
	// Indexing is absent when the daemon has no indexing loop to report on.
	Indexing *IndexingStatus `json:"indexing,omitempty"`
}

// IndexStatus is the index half. Ready, Model and Dims describe the vector
// generation search would use; Documents is the catalog breakdown, present
// whenever the database can be read at all — a database full of
// discovered-but-unindexed rows is exactly when the counts matter most, so
// they do not depend on Ready.
type IndexStatus struct {
	Ready     bool           `json:"ready"`
	Reason    string         `json:"reason,omitempty"`
	Model     string         `json:"model,omitempty"`
	Dims      int            `json:"dims,omitempty"`
	Documents map[string]int `json:"documents,omitempty"`
	// Queue is the dispatchable backlog. Absent when the database could not
	// be read.
	Queue *QueueStatus `json:"queue,omitempty"`
	// Failures are the largest groups of permanently-failed documents,
	// largest first. Absent when nothing has failed.
	Failures []FailureGroup `json:"failures,omitempty"`
	// Disk is what the index costs on disk (DESIGN.md: footprint is reported
	// in status).
	Disk *DiskUsage `json:"disk,omitempty"`
}

// QueueStatus splits the backlog into work due now and work in backoff. The
// split is what distinguishes a queue that is draining from one where every
// remaining document is failing.
type QueueStatus struct {
	Pending  int `json:"pending"`
	Retrying int `json:"retrying"`
}

// FailureGroup is one reason documents were given up on, with a count and one
// path to reproduce it with. Reason is untrusted display text.
type FailureGroup struct {
	Reason      string `json:"reason"`
	Documents   int    `json:"documents"`
	ExamplePath string `json:"example_path,omitempty"`
}

// DiskUsage is the index's footprint. The WAL is reported separately because
// a large one means something else — a reader holding a snapshot open, or a
// checkpoint that has not run — than a large database.
type DiskUsage struct {
	DBBytes    int64 `json:"db_bytes"`
	WALBytes   int64 `json:"wal_bytes"`
	TotalBytes int64 `json:"total_bytes"`
}

// IndexingStatus is the background loop's half: why it last did nothing, when
// it last did something, and what it has got through since the daemon started.
type IndexingStatus struct {
	// Running is false when the loop could not be started at all, in which
	// case Reason says why and the rest is zero.
	Running bool   `json:"running"`
	Reason  string `json:"reason,omitempty"`
	// Gate is why the last cycle did no work, in the user's terms; empty
	// means it worked normally. Deferring distinguishes a policy decision
	// (on battery) from an obstacle.
	Gate      string `json:"gate,omitempty"`
	Deferring bool   `json:"deferring"`
	// LastError is the most recent failure that stopped a cycle.
	LastError string `json:"last_error,omitempty"`
	// Timestamps are pointers so a daemon that has never scanned reports
	// nothing rather than a date in year 1. LastProgress against LastCycle is
	// what tells a slow queue from a stuck one.
	LastScan     *time.Time `json:"last_scan,omitempty"`
	LastCycle    *time.Time `json:"last_cycle,omitempty"`
	LastProgress *time.Time `json:"last_progress,omitempty"`
	// ScanErrors counts per-path problems in the last scan; PathErrors is a
	// capped sample of them. ScanReachedNothing means the scan errored and
	// reached no files at all — the signature of a missing Full Disk Access
	// grant, and the one outcome that leaves the daemon looking healthy while
	// indexing nothing (DESIGN.md: TCC is first-class state).
	ScanErrors         int         `json:"scan_errors"`
	ScanReachedNothing bool        `json:"scan_reached_nothing"`
	PathErrors         []PathError `json:"path_errors,omitempty"`
	// Watch is the filesystem watcher's half of freshness. Absent when the
	// daemon has no indexing loop to report on at all; present and not
	// running when the loop is scan-only, which is a working mode rather
	// than a fault (DESIGN.md: Change detection).
	Watch *WatchStatus `json:"watch,omitempty"`
	// Totals are cumulative since the daemon started, not since the index was
	// created: they describe this process, and the catalog counts describe the
	// corpus.
	Totals IndexingTotals `json:"totals"`
}

// WatchStatus is what the filesystem watcher is doing. When Running is
// false, Reason says why and the periodic scan is carrying freshness alone.
//
// LastEvent is the field worth reading: a watcher that is running but has
// never delivered anything is what a missing Full Disk Access grant looks
// like from here — subscribed, healthy, and told nothing.
type WatchStatus struct {
	Running   bool       `json:"running"`
	Reason    string     `json:"reason,omitempty"`
	Roots     int        `json:"roots,omitempty"`
	LastEvent *time.Time `json:"last_event,omitempty"`
	// Reconciled counts documents queued from events, Deleted counts
	// documents purged because their file went away, and Rescans counts
	// full walks forced by an event stream that lost events.
	Reconciled int `json:"reconciled"`
	Deleted    int `json:"deleted"`
	Rescans    int `json:"rescans"`
}

// PathError is one path the last scan could not read.
type PathError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// IndexingTotals is what this daemon process has got through.
type IndexingTotals struct {
	Indexed int `json:"indexed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
	Retried int `json:"retried"`
	Swept   int `json:"swept"`
}

// handleStatus answers even when nothing works: a status endpoint that fails
// when the index is broken withholds exactly the information being asked for
// (DESIGN.md: a stalled queue must always be distinguishable from a deferred
// one). A Backend failure becomes the index half's reason, never a non-2xx.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	// Bounded like any other request: reading status touches the database,
	// and on an unresponsive mount an unbounded handler would blow past the
	// write timeout and hand the client a dead connection instead of the
	// document this endpoint promises to always produce.
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	index, err := s.backend.Status(ctx)
	if err != nil {
		s.log.Warn("status", "error", err)
		index = IndexStatus{Ready: false, Reason: err.Error()}
	}
	resp := StatusResponse{
		Version: buildinfo.Version,
		PID:     os.Getpid(),
		UptimeS: int64(time.Since(s.started).Seconds()),
		Socket:  s.socket,
		DBPath:  s.dbPath,
		Index:   index,
	}
	if s.indexing != nil {
		indexing := s.indexing()
		resp.Indexing = &indexing
	}
	writeJSON(w, s.log, resp)
}

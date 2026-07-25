package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bcrisp4/bsearch/internal/client"
	"github.com/bcrisp4/bsearch/internal/config"
	"github.com/bcrisp4/bsearch/internal/domain"
	"github.com/bcrisp4/bsearch/internal/search"
	"github.com/bcrisp4/bsearch/internal/server"
)

const (
	// maxProseRunes bounds the daemon's own explanations. Generous, because
	// these sentences name paths and models and are the entire answer when
	// something is wrong — truncating one is truncating the diagnosis.
	maxProseRunes = 300
	// maxReasonRunes bounds a per-document failure reason, which comes from a
	// parser or an HTTP service and is occasionally a paragraph. Enough to
	// identify which failure this is; the file itself has the rest.
	maxReasonRunes = 120
)

// runStatus reports what the daemon is doing. It is a client like
// every other subcommand (ADR 0010): the daemon owns the index, so it is the
// only thing that can honestly describe it.
//
// Exit status is 0 for any answer the daemon gave, however unhappy. Reporting
// a stalled queue as a command failure would make `bsearch status` unusable in
// exactly the situation it exists for; a non-zero exit means the daemon could
// not be reached at all.
func runStatus(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(out)
	socketPath := fs.String("socket", config.DefaultSocketPath(), "daemon socket")
	asJSON := fs.Bool("json", false, "emit JSON instead of human-readable output")
	fs.Usage = func() {
		fmt.Fprintln(out, `usage: bsearch status [--socket <path>] [--json]`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("status takes no arguments (got %q)", fs.Arg(0))
	}
	if *socketPath == "" {
		return errors.New("cannot resolve the default socket path (no home directory?) — pass --socket")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	body, err := client.New(*socketPath).Status(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		// Copied through rather than re-encoded, like search: fields a newer
		// daemon reports still reach consumers that understand them.
		_, err := out.Write(body)
		return err
	}
	var resp server.StatusResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("the daemon sent a status this version cannot read: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	writeStatusHuman(out, resp, home, time.Now())
	return nil
}

// writeStatusHuman renders the report. now is a parameter so the relative
// timestamps are testable.
func writeStatusHuman(out io.Writer, resp server.StatusResponse, home string, now time.Time) {
	fmt.Fprintf(out, "bsearch %s — running (pid %d, up %s)\n",
		resp.Version, resp.PID, humanDuration(time.Duration(resp.UptimeS)*time.Second))
	dbLine := stripControl(tildePath(home, resp.DBPath))
	if d := resp.Index.Disk; d != nil {
		// The WAL is called out separately when it has grown, because it
		// means something else than a large database: a reader holding a
		// snapshot open, or a checkpoint that has not run. Folding it into
		// one total would hide the one number that is a symptom.
		dbLine += "  (" + humanBytes(d.TotalBytes)
		if d.WALBytes > 0 {
			dbLine += ", " + humanBytes(d.WALBytes) + " WAL"
		}
		dbLine += ")"
	}
	writeFields(out, []field{
		{"socket", stripControl(tildePath(home, resp.Socket))},
		{"db", dbLine},
	})

	writeIndexSection(out, resp.Index)
	writeQueueSection(out, resp.Index)
	writeIndexingSection(out, resp.Indexing, now)
	writeUnreadableSection(out, resp.Indexing, home)
	writeFailuresSection(out, resp.Index, home)
}

func writeIndexSection(out io.Writer, index server.IndexStatus) {
	fmt.Fprintln(out, "\nIndex")
	fields := []field{{"ready", yesNo(index.Ready)}}
	if index.Reason != "" {
		fields = append(fields, field{"reason", search.Preview(index.Reason, maxProseRunes)})
	}
	if index.Model != "" {
		model := stripControl(index.Model)
		if index.Dims > 0 {
			model += fmt.Sprintf(" (%dd)", index.Dims)
		}
		fields = append(fields, field{"model", model})
	}
	if index.Documents != nil {
		fields = append(fields, field{"documents", fmt.Sprintf("%s indexed · %s failed · %s deleted",
			count(index.Documents[string(domain.DocStateIndexed)]),
			count(index.Documents[string(domain.DocStateFailed)]),
			count(index.Documents[string(domain.DocStateDeleted)]))})
	}
	writeFields(out, fields)
}

// writeQueueSection reports the backlog. The per-state breakdown is what says
// where a stuck backlog is stuck — everything sitting at discovered is a
// different problem from everything sitting at chunked.
func writeQueueSection(out io.Writer, index server.IndexStatus) {
	if index.Queue == nil {
		return
	}
	fmt.Fprintln(out, "\nQueue")
	fields := []field{{"pending", count(index.Queue.Pending)}}
	if index.Queue.Retrying > 0 {
		// Only when it is happening: a line reading "retrying 0" on every
		// healthy daemon trains the eye to skip the one that matters.
		fields = append(fields, field{"retrying", count(index.Queue.Retrying)})
	}
	var states []string
	for _, state := range domain.DocStates {
		if state.Terminal() {
			continue
		}
		if n := index.Documents[string(state)]; n > 0 {
			states = append(states, fmt.Sprintf("%s %s", state, count(n)))
		}
	}
	if len(states) > 0 {
		fields = append(fields, field{"states", strings.Join(states, " · ")})
	}
	writeFields(out, fields)
}

func writeIndexingSection(out io.Writer, indexing *server.IndexingStatus, now time.Time) {
	if indexing == nil {
		return
	}
	fmt.Fprintln(out, "\nIndexing")
	if !indexing.Running {
		reason := indexing.Reason
		if reason == "" {
			reason = "no reason given"
		}
		writeFields(out, []field{{"status", "not running — " + search.Preview(reason, maxProseRunes)}})
		return
	}

	gate := indexing.Gate
	if gate == "" {
		gate = "working"
	}
	fields := []field{{"gate", stripControl(gate)}}
	if indexing.LastScan != nil {
		fields = append(fields, field{"last scan", ago(*indexing.LastScan, now) + scanTrouble(indexing)})
	}
	if indexing.LastProgress != nil {
		fields = append(fields, field{"last progress", ago(*indexing.LastProgress, now)})
	}
	if indexing.LastCycle != nil {
		fields = append(fields, field{"last cycle", ago(*indexing.LastCycle, now)})
	}
	t := indexing.Totals
	fields = append(fields, field{"since start", fmt.Sprintf("%s indexed · %s failed · %s skipped · %s retried",
		count(t.Indexed), count(t.Failed), count(t.Skipped), count(t.Retried))})
	if t.Swept > 0 {
		// Only worth a line when it happened: it is the daemon explaining why
		// an indexed corpus is being worked through again.
		fields = append(fields, field{"re-queued", count(t.Swept) + " (superseded pipeline stage)"})
	}
	if indexing.LastError != "" {
		fields = append(fields, field{"last error", search.Preview(indexing.LastError, maxProseRunes)})
	}
	writeFields(out, fields)
}

// writeUnreadableSection lists the paths the last scan could not read. Its own
// section rather than a footnote to the scan line: on macOS this is usually a
// missing Full Disk Access grant, which is the single most likely reason a
// healthy-looking daemon indexes nothing (DESIGN.md: TCC is first-class
// state; issue #14 adds the remediation).
func writeUnreadableSection(out io.Writer, indexing *server.IndexingStatus, home string) {
	if indexing == nil || len(indexing.PathErrors) == 0 {
		return
	}
	fmt.Fprintf(out, "\nUnreadable paths (%s)\n", count(indexing.ScanErrors))
	fields := make([]field, 0, len(indexing.PathErrors))
	for _, pe := range indexing.PathErrors {
		fields = append(fields, field{
			stripControl(tildePath(home, pe.Path)),
			search.Preview(pe.Error, maxReasonRunes),
		})
	}
	writeFields(out, fields)
	if more := indexing.ScanErrors - len(indexing.PathErrors); more > 0 {
		fmt.Fprintf(out, "  … %s more\n", count(more))
	}
}

// scanTrouble annotates the last-scan line. Reaching no files at all is called
// out separately from the error count: it is the signature of a missing Full
// Disk Access grant, and the one outcome where a daemon looks healthy while
// indexing nothing (issue #14 covers the remediation).
func scanTrouble(indexing *server.IndexingStatus) string {
	if indexing.ScanErrors == 0 {
		return ""
	}
	noun := "path errors"
	if indexing.ScanErrors == 1 {
		noun = "path error"
	}
	trouble := fmt.Sprintf(" — %s %s", count(indexing.ScanErrors), noun)
	if indexing.ScanReachedNothing {
		trouble += ", reached no files"
	}
	return trouble
}

// writeFailuresSection lists why documents were given up on. Grouped, because
// a corpus fails in a handful of ways and the count plus one path is what can
// be acted on.
func writeFailuresSection(out io.Writer, index server.IndexStatus, home string) {
	if len(index.Failures) == 0 {
		return
	}
	failed := index.Documents[string(domain.DocStateFailed)]
	fmt.Fprintf(out, "\nFailures (%s)\n", count(failed))
	listed := 0
	for _, f := range index.Failures {
		reason := f.Reason
		if reason == "" {
			reason = "no reason recorded"
		}
		fmt.Fprintf(out, "  %s  %s\n", count(f.Documents), search.Preview(reason, maxReasonRunes))
		if f.ExamplePath != "" {
			fmt.Fprintf(out, "     %s\n", stripControl(tildePath(home, f.ExamplePath)))
		}
		listed += f.Documents
	}
	// The daemon reports only the largest few groups, so on a corpus that
	// failed in many ways the list accounts for less than the heading. Saying
	// so is the difference between a truncated list and a list that appears
	// to disagree with its own total.
	if more := failed - listed; more > 0 {
		fmt.Fprintf(out, "  … %s more in reasons not shown\n", count(more))
	}
}

// field is one label/value line. Values are rendered aligned within their
// section rather than across the whole report: the sections are read one at a
// time, and a single global width leaves "ready" stranded eleven columns from
// its value.
type field struct {
	label string
	value string
}

func writeFields(out io.Writer, fields []field) {
	width := 0
	for _, f := range fields {
		if len(f.label) > width {
			width = len(f.label)
		}
	}
	for _, f := range fields {
		fmt.Fprintf(out, "  %-*s  %s\n", width, f.label, f.value)
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// ago renders a past timestamp relative to now, which is what a reader
// actually wants from status: "4m ago" answers "is it keeping up" where a
// wall-clock time makes them do the subtraction. A timestamp in the future
// (clock change, a daemon on another machine's clock) is reported as such
// rather than as a negative age.
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 0:
		return "in the future (" + t.Local().Format(time.RFC3339) + ")"
	case d < time.Second:
		return "just now"
	}
	return humanDuration(d) + " ago"
}

// humanDuration renders a duration to two units at most: "1d 1h", "4m 12s".
// Precision below that is noise for uptimes and staleness alike.
func humanDuration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	units := []struct {
		size time.Duration
		name string
	}{
		{24 * time.Hour, "d"},
		{time.Hour, "h"},
		{time.Minute, "m"},
		{time.Second, "s"},
	}
	var parts []string
	for _, u := range units {
		n := d / u.size
		if n == 0 {
			// Leading zeroes are not the answer; a zero *second* unit means
			// the first one was the whole answer — "2h", never "2h 0m", and
			// never "1d 0h" for a day and a half hour.
			if len(parts) == 0 {
				continue
			}
			break
		}
		d -= n * u.size
		parts = append(parts, strconv.FormatInt(int64(n), 10)+u.name)
		// Two units at most.
		if len(parts) == 2 {
			break
		}
	}
	return strings.Join(parts, " ")
}

// humanBytes renders a footprint in binary units, dropping the decimal once
// the number is large enough not to need it.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	value, exp := float64(n), 0
	for value >= unit && exp < 4 {
		value /= unit
		exp++
	}
	suffix := [...]string{"KiB", "MiB", "GiB", "TiB"}[exp-1]
	if value >= 100 {
		return fmt.Sprintf("%.0f %s", value, suffix)
	}
	return fmt.Sprintf("%.1f %s", value, suffix)
}

// count renders a document count with thousands separators. A corpus is
// counted in the hundreds of thousands, where "104829" is unreadable.
func count(n int) string {
	s := strconv.Itoa(n)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return sign + b.String()
}

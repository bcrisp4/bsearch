package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
)

// StatusSnapshot reads everything `bsearch status` reports — catalog counts,
// queue depth, and failure groups — in ONE read transaction. The three
// cross-reference each other on the rendered report (queue pending against
// the non-terminal state breakdown, the failure heading against the failed
// count), and separate reads land on separate WAL snapshots: mid-drain, the
// indexer commits between them and status contradicts itself on the one
// surface a user reaches for when something already looks wrong.
//
// The failure groups are only queried when the counts show failed > 0: the
// common case is a corpus with no failures at all, and status runs on every
// `bsearch status`. Inside the transaction, so the gate and the query agree
// by construction.
func (s *Store) StatusSnapshot(ctx context.Context, now time.Time, failLimit int) (domain.CatalogCounts, domain.QueueDepth, []domain.FailureGroup, error) {
	tx, err := s.db.Reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.CatalogCounts{}, domain.QueueDepth{}, nil, fmt.Errorf("status snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only

	counts, err := catalogCountsTx(ctx, tx)
	if err != nil {
		return domain.CatalogCounts{}, domain.QueueDepth{}, nil, err
	}
	depth, err := queueDepthTx(ctx, tx, now)
	if err != nil {
		return domain.CatalogCounts{}, domain.QueueDepth{}, nil, err
	}
	var groups []domain.FailureGroup
	if counts.Content[domain.ContentStateFailed] > 0 {
		if groups, err = failureReasonsTx(ctx, tx, failLimit); err != nil {
			return domain.CatalogCounts{}, domain.QueueDepth{}, nil, err
		}
	}
	return counts, depth, groups, nil
}

// CatalogCounts reads the three populations `bsearch status` reports —
// files, contents by state, unread by reason — in one read transaction, so
// they reconcile exactly rather than racily (ADR 0015): files − sum(unread)
// is the files that have content, sum(content) is how many distinct
// contents those are, and the gap is what deduplication saved.
func (s *Store) CatalogCounts(ctx context.Context) (domain.CatalogCounts, error) {
	tx, err := s.db.Reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.CatalogCounts{}, fmt.Errorf("catalog counts: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only
	return catalogCountsTx(ctx, tx)
}

// catalogCountsTx is CatalogCounts inside a caller-owned transaction.
//
// The content count is scoped to referenced rows: an edit or a delete
// orphans the old hash's row until the sweep collects it, and counting
// orphans would break the reconciliation identity above the moment either
// happens — five edits of one file would read as five contents of one
// document. EXISTS rather than IN, because the shared test seed keeps a
// NULL content_hash in documents and NOT IN/IN over a set with NULLs is a
// three-valued-logic trap (see newTestStore).
//
// Every state and reason is present, zero included: a reporting consumer
// must never have to tell "none here" apart from "this build didn't know
// about it" (DESIGN.md: status is observable). A state in the database this
// build doesn't know is still reported — dropping it would hide exactly the
// drift worth seeing.
func catalogCountsTx(ctx context.Context, tx *sql.Tx) (domain.CatalogCounts, error) {
	counts := domain.CatalogCounts{
		Content: make(map[domain.ContentState]int, len(domain.ContentStates)),
		Unread:  make(map[domain.UnreadReason]int, len(domain.UnreadReasons)),
	}
	for _, state := range domain.ContentStates {
		counts.Content[state] = 0
	}
	for _, reason := range domain.UnreadReasons {
		counts.Unread[reason] = 0
	}

	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM documents").Scan(&counts.Files); err != nil {
		return domain.CatalogCounts{}, fmt.Errorf("count files: %w", err)
	}

	if err := groupCount(ctx, tx, `
		SELECT state, count(*) FROM content
		WHERE EXISTS (SELECT 1 FROM documents d WHERE d.content_hash = content.content_hash)
		GROUP BY state`,
		func(key string, n int) { counts.Content[domain.ContentState(key)] = n }); err != nil {
		return domain.CatalogCounts{}, fmt.Errorf("count content by state: %w", err)
	}

	if err := groupCount(ctx, tx,
		"SELECT unread_reason, count(*) FROM documents WHERE content_hash IS NULL GROUP BY unread_reason",
		func(key string, n int) { counts.Unread[domain.UnreadReason(key)] = n }); err != nil {
		return domain.CatalogCounts{}, fmt.Errorf("count unread by reason: %w", err)
	}

	return counts, nil
}

// groupCount runs a two-column (key, count) GROUP BY and hands each row to
// record.
func groupCount(ctx context.Context, q queryer, query string, record func(key string, n int)) error {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err below reports failures

	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return err
		}
		record(key, n)
	}
	return rows.Err()
}

// QueueDepth counts the dispatchable backlog as of now: how many non-terminal
// contents the scheduler would claim, and how many are waiting out a backoff.
func (s *Store) QueueDepth(ctx context.Context, now time.Time) (domain.QueueDepth, error) {
	return queueDepthTx(ctx, s.db.Reader(), now)
}

// queueDepthTx is QueueDepth over a caller-chosen queryer (pool or
// transaction).
//
// The predicate is ClaimBatch's, deliberately — a depth that counted rows the
// claim would not return would report a queue that never drains. Terminal
// states are spliced in as a literal for the same reason as there: a bound
// parameter is opaque to the partial-index analysis and would demote this to a
// full scan of the corpus rather than of the backlog (see queue.go).
func queueDepthTx(ctx context.Context, q queryer, now time.Time) (domain.QueueDepth, error) {
	var depth domain.QueueDepth
	//nolint:gosec // G202: the spliced text is domain.TerminalContentStates, not input
	err := q.QueryRowContext(ctx, `
		SELECT
			count(*) FILTER (WHERE next_retry_at IS NULL OR next_retry_at <= ?),
			count(*) FILTER (WHERE next_retry_at > ?)
		FROM content
		WHERE state NOT IN (`+terminalStatesSQL()+`)`,
		now.Unix(), now.Unix()).Scan(&depth.Pending, &depth.Retrying)
	if err != nil {
		return domain.QueueDepth{}, fmt.Errorf("queue depth: %w", err)
	}
	return depth, nil
}

// FailureReasons returns the most common reasons content was given up on,
// largest group first, at most limit of them.
func (s *Store) FailureReasons(ctx context.Context, limit int) ([]domain.FailureGroup, error) {
	return failureReasonsTx(ctx, s.db.Reader(), limit)
}

// failureReasonsTx is FailureReasons over a caller-chosen queryer (pool or
// transaction).
//
// Grouped rather than listed: a corpus fails in a handful of ways — an
// unreachable converter, one undecodable encoding — and the reason with a
// single example is what a user can act on, where a thousand paths is not.
//
// The count is distinct contents, not joined rows: the documents join fans
// out per referencing path, and counting rows would inflate a duplicated
// failure. The join is inner, matching CatalogCounts' EXISTS scope: failed
// content no document references (orphaned, sweep pending) is invisible to
// the counts, so reporting it here would make the failure list disagree
// with its own heading — and there is nothing a user could act on anyway.
// Every group therefore always carries an example path; min(d.path) picks
// it only because it is stable, any row would do.
func failureReasonsTx(ctx context.Context, q queryer, limit int) ([]domain.FailureGroup, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT coalesce(c.last_error, ''), count(DISTINCT c.content_hash), min(d.path)
		FROM content c
		JOIN documents d ON d.content_hash = c.content_hash
		WHERE c.state = ?
		GROUP BY coalesce(c.last_error, '')
		ORDER BY count(DISTINCT c.content_hash) DESC, min(d.path)
		LIMIT ?`, string(domain.ContentStateFailed), limit)
	if err != nil {
		return nil, fmt.Errorf("failure reasons: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err below reports failures

	var groups []domain.FailureGroup
	for rows.Next() {
		var g domain.FailureGroup
		if err := rows.Scan(&g.Reason, &g.Contents, &g.ExamplePath); err != nil {
			return nil, fmt.Errorf("failure reasons: %w", err)
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failure reasons: %w", err)
	}
	return groups, nil
}

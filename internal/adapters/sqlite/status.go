package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
)

// QueueDepth counts the dispatchable backlog as of now: how many non-terminal
// documents the scheduler would claim, and how many are waiting out a backoff.
//
// The predicate is ClaimBatch's, deliberately — a depth that counted rows the
// claim would not return would report a queue that never drains. Terminal
// states are spliced in as a literal for the same reason as there: a bound
// parameter is opaque to the partial-index analysis and would demote this to a
// full scan of the corpus rather than of the backlog (see queue.go).
func (s *Store) QueueDepth(ctx context.Context, now time.Time) (domain.QueueDepth, error) {
	var depth domain.QueueDepth
	//nolint:gosec // G202: the spliced text is domain.TerminalDocStates, not input
	err := s.db.Reader().QueryRowContext(ctx, `
		SELECT
			count(*) FILTER (WHERE next_retry_at IS NULL OR next_retry_at <= ?),
			count(*) FILTER (WHERE next_retry_at > ?)
		FROM documents
		WHERE state NOT IN (`+terminalStatesSQL()+`)`,
		now.Unix(), now.Unix()).Scan(&depth.Pending, &depth.Retrying)
	if err != nil {
		return domain.QueueDepth{}, fmt.Errorf("queue depth: %w", err)
	}
	return depth, nil
}

// FailureReasons returns the most common reasons documents were given up on,
// largest group first, at most limit of them.
//
// Grouped rather than listed: a corpus fails in a handful of ways — an
// unreachable converter, one undecodable encoding — and the reason with a
// single example is what a user can act on, where a thousand paths is not.
// min(path) picks the example only because it is stable; any row would do.
func (s *Store) FailureReasons(ctx context.Context, limit int) ([]domain.FailureGroup, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.Reader().QueryContext(ctx, `
		SELECT coalesce(last_error, ''), count(*), min(path)
		FROM documents
		WHERE state = ?
		GROUP BY coalesce(last_error, '')
		ORDER BY count(*) DESC, min(path)
		LIMIT ?`, string(domain.DocStateFailed), limit)
	if err != nil {
		return nil, fmt.Errorf("failure reasons: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err below reports failures

	var groups []domain.FailureGroup
	for rows.Next() {
		var g domain.FailureGroup
		// min(path) is NULL-free here — path is NOT NULL and every group has
		// at least one row — but scanning through NullString costs nothing and
		// survives the day this query grows a LEFT JOIN.
		var path sql.NullString
		if err := rows.Scan(&g.Reason, &g.Documents, &path); err != nil {
			return nil, fmt.Errorf("failure reasons: %w", err)
		}
		g.ExamplePath = path.String
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failure reasons: %w", err)
	}
	return groups, nil
}

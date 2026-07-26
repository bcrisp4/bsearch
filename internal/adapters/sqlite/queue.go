package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
)

var _ domain.Queue = (*Store)(nil)

// agingShare is the fraction of every claimed batch reserved for the oldest
// due content rather than the newest. One in four: enough that a 100k
// initial backlog drains steadily while a file saved a moment ago still
// reaches the embedder in the same cycle (DESIGN.md: priority is recency
// ordering, with aging so the backlog and due retries can't be starved).
const agingShare = 4

// terminalStatesSQL renders domain.TerminalContentStates as a SQL literal
// list.
//
// It is not parameterised. SQLite can only use the partial claim index when
// the query's WHERE clause literally implies the index's, and a bound
// parameter is opaque to that analysis — an `IN (?, ?)` would silently
// demote every claim to a full table scan. TestClaimBatchUsesThePartialIndex
// is what keeps this honest.
func terminalStatesSQL() string {
	quoted := make([]string, len(domain.TerminalContentStates))
	for i, s := range domain.TerminalContentStates {
		quoted[i] = "'" + string(s) + "'"
	}
	return strings.Join(quoted, ", ")
}

// ClaimBatch returns up to limit content rows due for work at now.
//
// Nothing is reserved — the name is DESIGN.md's, but with one indexing worker
// in one process there is no contention to arbitrate, so this is a plain read
// and a crash needs no release path (ADR 0011).
//
// The batch is composed, not merely ordered: three quarters of it are the
// most recently changed contents and one quarter the least, so a save lands
// in the current cycle without a bulk backlog ever aging out of reach. The
// two slices are deduplicated, which is why a near-empty queue returns fewer
// than limit rows — there is genuinely no more work, not lost content.
func (s *Store) ClaimBatch(ctx context.Context, now time.Time, limit int) ([]domain.Content, error) {
	if limit <= 0 {
		return nil, nil
	}
	aging := limit / agingShare
	recent := limit - aging

	// content_hash is the tiebreak: updated_at has one-second resolution, so
	// a bulk discovery pass writes thousands of rows sharing a timestamp, and
	// without it the claim could return the same arbitrary subset forever
	// while the rest of that second's work never came up.
	query := "SELECT " + contentColumns + `
		FROM content
		WHERE state NOT IN (` + terminalStatesSQL() + `)
		  AND (next_retry_at IS NULL OR next_retry_at <= ?)
		ORDER BY updated_at %s, content_hash %s
		LIMIT ?`

	contents, err := s.queryContents(ctx, "claim batch",
		fmt.Sprintf(query, "DESC", "DESC"), now.Unix(), recent)
	if err != nil {
		return nil, err
	}
	if aging > 0 {
		old, err := s.queryContents(ctx, "claim batch (aging)",
			fmt.Sprintf(query, "ASC", "ASC"), now.Unix(), aging)
		if err != nil {
			return nil, err
		}
		seen := make(map[string]struct{}, len(contents))
		for _, c := range contents {
			seen[c.Hash] = struct{}{}
		}
		for _, c := range old {
			if _, dup := seen[c.Hash]; !dup {
				contents = append(contents, c)
			}
		}
	}
	return contents, nil
}

// GetWork re-reads one claimed content row immediately before working it and
// resolves every path referencing it, newest mtime first, tie-broken by
// path ascending — the same rule that picks a search hit's primary path
// (see SearchVectors), so "which path does bsearch mean by this content"
// has one answer everywhere. All paths, not just the primary: the bytes at
// any copy are the bytes (the pipeline verifies the hash), so an unreadable
// primary — an unmounted volume, a revoked grant — must not block a
// readable duplicate.
//
// ErrContentGone covers both "swept" and "no live path references it" —
// deleted while in flight, with the sweep still to run. Either way the
// content is not workable and the caller moves on.
func (s *Store) GetWork(ctx context.Context, hash string) (domain.WorkItem, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `
		SELECT `+prefixedContentColumns+`, d.path
		FROM content c
		JOIN documents d ON d.content_hash = c.content_hash
		WHERE c.content_hash = ?
		ORDER BY d.mtime DESC, d.path ASC`, hash)
	if err != nil {
		return domain.WorkItem{}, fmt.Errorf("get work %s: %w", hash, err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err below reports failures

	var (
		item domain.WorkItem
		raw  contentRow
	)
	for rows.Next() {
		var path string
		targets := append(raw.targets(&item.Content), &path)
		if err := rows.Scan(targets...); err != nil {
			return domain.WorkItem{}, fmt.Errorf("get work %s: %w", hash, err)
		}
		item.Paths = append(item.Paths, path)
	}
	if err := rows.Err(); err != nil {
		return domain.WorkItem{}, fmt.Errorf("get work %s: %w", hash, err)
	}
	if len(item.Paths) == 0 {
		return domain.WorkItem{}, fmt.Errorf("get work %s: %w", hash, domain.ErrContentGone)
	}
	if err := raw.finish(&item.Content); err != nil {
		return domain.WorkItem{}, err
	}
	item.Path = item.Paths[0]
	return item, nil
}

// Reschedule records a transient failure: the attempt count, when the
// content is next due, and why the last try failed.
//
// State is deliberately untouched. Content that failed while embedding is
// still chunked, and its chunks are still in the database — resetting it to
// discovered would throw away work that is fine and re-read, re-normalize and
// re-chunk the file for nothing.
func (s *Store) Reschedule(ctx context.Context, hash string, attempts int, at time.Time, reason string) error {
	return s.updateContent(ctx, "reschedule", hash,
		"UPDATE content SET attempts = ?, next_retry_at = ?, last_error = ?, updated_at = ? WHERE content_hash = ?",
		attempts, at.Unix(), reason, time.Now().Unix(), hash)
}

// ResetStale moves content whose derived data was produced by stage versions
// other than current back to discovered, and reports how many moved.
//
// This is what makes changing the embedding model re-embed the corpus with no
// command run. The dispatch predicate skips terminal states by design, so
// indexed content is invisible to the scheduler no matter how stale it is;
// without this sweep a model change would leave the daemon serving vectors
// from the old model indefinitely, with nothing in the logs to say so.
//
// Failed rows are swept too. failed is otherwise permanent by construction —
// content is immutable, so the same bytes fail the same way tomorrow — but a
// config change is precisely the variable that permanence conditions on:
// content that failed under one chunker or model deserves a fresh attempt
// under a different one.
//
// updated_at is *not* bumped, unlike every other write here. The sweep is a
// bulk administrative reset rather than a per-content event, and stamping
// the whole corpus with one timestamp would flatten the ordering signal the
// claim depends on at precisely the moment it matters most — the point where
// every content is queued at once and "which of these did I touch today"
// is the only thing distinguishing them.
func (s *Store) ResetStale(ctx context.Context, current map[string]string) (int, error) {
	if len(current) == 0 {
		return 0, nil
	}
	// Sorted so the statement is stable across calls (map iteration is not),
	// which keeps SQLite's statement cache warm and test failures readable.
	keys := make([]string, 0, len(current))
	for k := range current {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	// Both the path and the expected value are bound. Stage keys are domain
	// constants rather than user input, but a JSON path spliced into SQL is
	// the kind of thing that stops being true later.
	//
	// IS NOT, not <>: json_extract yields NULL for a key a content predates,
	// and <> evaluates NULL there — which would quietly exclude exactly the
	// oldest content, the rows most in need of the sweep.
	conds := make([]string, len(keys))
	args := make([]any, 0, 2*len(keys))
	for i, k := range keys {
		conds[i] = "json_extract(stage_versions, ?) IS NOT ?"
		args = append(args, "$."+k, current[k])
	}

	var moved int
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		//nolint:gosec // G202: the joined text is a fixed condition repeated
		// once per key; every value, including the JSON path, is bound.
		res, err := tx.ExecContext(ctx, `
			UPDATE content
			SET state = 'discovered', attempts = 0, next_retry_at = NULL, last_error = NULL
			WHERE state IN ('indexed', 'failed')
			  AND (`+strings.Join(conds, " OR ")+`)`, args...)
		if err != nil {
			return fmt.Errorf("reset stale: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("reset stale: %w", err)
		}
		moved = int(n)
		return nil
	})
	return moved, err
}

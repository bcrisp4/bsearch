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
// due documents rather than the newest. One in four: enough that a 100k
// initial backlog drains steadily while a file saved a moment ago still
// reaches the embedder in the same cycle (DESIGN.md: priority is recency
// ordering, with aging so the backlog and due retries can't be starved).
const agingShare = 4

// terminalStatesSQL renders domain.TerminalDocStates as a SQL literal list.
//
// It is not parameterised. SQLite can only use the partial claim index when
// the query's WHERE clause literally implies the index's, and a bound
// parameter is opaque to that analysis — an `IN (?, ?, ?)` would silently
// demote every claim to a full table scan. TestClaimBatchUsesThePartialIndex
// is what keeps this honest.
func terminalStatesSQL() string {
	quoted := make([]string, len(domain.TerminalDocStates))
	for i, s := range domain.TerminalDocStates {
		quoted[i] = "'" + string(s) + "'"
	}
	return strings.Join(quoted, ", ")
}

// ClaimBatch returns up to limit documents due for work at now.
//
// Nothing is reserved — the name is DESIGN.md's, but with one indexing worker
// in one process there is no contention to arbitrate, so this is a plain read
// and a crash needs no release path (ADR 0011).
//
// The batch is composed, not merely ordered: three quarters of it are the
// most recently changed documents and one quarter the least, so a save lands
// in the current cycle without a bulk backlog ever aging out of reach. The
// two slices are deduplicated, which is why a near-empty queue returns fewer
// than limit rows — there is genuinely no more work, not a lost document.
func (s *Store) ClaimBatch(ctx context.Context, now time.Time, limit int) ([]domain.Document, error) {
	if limit <= 0 {
		return nil, nil
	}
	aging := limit / agingShare
	recent := limit - aging

	// id is the tiebreak: updated_at has one-second resolution, so a bulk
	// discovery pass writes thousands of rows sharing a timestamp, and
	// without it the claim could return the same arbitrary subset forever
	// while the rest of that second's work never came up.
	query := "SELECT " + strings.Join(docColumns, ", ") + `
		FROM documents
		WHERE state NOT IN (` + terminalStatesSQL() + `)
		  AND (next_retry_at IS NULL OR next_retry_at <= ?)
		ORDER BY updated_at %s, id %s
		LIMIT ?`

	docs, err := s.queryDocs(ctx, "claim batch",
		fmt.Sprintf(query, "DESC", "DESC"), now.Unix(), recent)
	if err != nil {
		return nil, err
	}
	if aging > 0 {
		old, err := s.queryDocs(ctx, "claim batch (aging)",
			fmt.Sprintf(query, "ASC", "ASC"), now.Unix(), aging)
		if err != nil {
			return nil, err
		}
		seen := make(map[string]struct{}, len(docs))
		for _, d := range docs {
			seen[d.ID] = struct{}{}
		}
		for _, d := range old {
			if _, dup := seen[d.ID]; !dup {
				docs = append(docs, d)
			}
		}
	}
	return docs, nil
}

// Reschedule records a transient failure: the attempt count, when the
// document is next due, and why the last try failed.
//
// State is deliberately untouched. A document that failed while embedding is
// still chunked, and its chunks are still in the database — resetting it to
// discovered would throw away work that is fine and re-read, re-normalize and
// re-chunk the file for nothing.
func (s *Store) Reschedule(ctx context.Context, docID string, attempts int, at time.Time, reason string) error {
	return s.updateDoc(ctx, "reschedule", docID,
		"UPDATE documents SET attempts = ?, next_retry_at = ?, last_error = ?, updated_at = ? WHERE id = ?",
		attempts, at.Unix(), reason, time.Now().Unix(), docID)
}

// ResetStale moves documents whose derived data was produced by stage
// versions other than current back to discovered, and reports how many moved.
//
// This is what makes changing the embedding model re-embed the corpus with no
// command run. The dispatch predicate skips terminal states by design, so an
// indexed document is invisible to the scheduler no matter how stale it is;
// without this sweep a model change would leave the daemon serving vectors
// from the old model indefinitely, with nothing in the logs to say so.
//
// Failed rows are swept too, matching what the one-shot pipeline used to do:
// a document that failed under one configuration deserves a fresh attempt
// under a different one, because the configuration may well have been the
// problem.
//
// updated_at is *not* bumped, unlike every other write here. The sweep is a
// bulk administrative reset rather than a per-document event, and stamping
// the whole corpus with one timestamp would flatten the ordering signal the
// claim depends on at precisely the moment it matters most — the point where
// every document is queued at once and "which of these did I touch today"
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
	// IS NOT, not <>: json_extract yields NULL for a key a document predates,
	// and <> evaluates NULL there — which would quietly exclude exactly the
	// oldest documents, the ones most in need of the sweep.
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
			UPDATE documents
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

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
)

// queryer abstracts *sql.DB and *sql.Tx for reads.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// setMeta upserts one meta key — the single blessed write path for
// pipeline metadata.
func setMeta(ctx context.Context, tx *sql.Tx, key, value string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		return fmt.Errorf("set meta %s: %w", key, err)
	}
	return nil
}

// getMeta reads one meta key; ok is false when the key is absent.
func getMeta(ctx context.Context, q queryer, key string) (value string, ok bool, err error) {
	err = q.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&value)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("get meta %s: %w", key, err)
	}
	return value, true, nil
}

// documentColumns is the canonical documents column list, paired with
// documentRow. One definition so every query path hydrates identical
// domain.Documents. prefixedDocumentColumns is the same list under the
// alias every join in this package gives the documents table — the two
// must stay in documentRow.targets order together.
const (
	documentColumns         = "path, content_hash, unread_reason, size, mtime"
	prefixedDocumentColumns = "d.path, d.content_hash, d.unread_reason, d.size, d.mtime"
)

// documentRow holds the raw documents column values that need conversion
// after Scan: the nullable pair (content_hash XOR unread_reason — the
// schema's CHECK) and the nanosecond mtime.
type documentRow struct {
	contentHash  sql.NullString
	unreadReason sql.NullString
	mtimeNS      int64
}

// targets returns Scan destinations matching documentColumns order.
func (r *documentRow) targets(doc *domain.Document) []any {
	return []any{&doc.Path, &r.contentHash, &r.unreadReason, &doc.Size, &r.mtimeNS}
}

// finish converts the raw values into their domain form ("" is the Go
// spelling of NULL for both halves of the exclusive pair).
func (r *documentRow) finish(doc *domain.Document) {
	doc.ContentHash = r.contentHash.String
	doc.UnreadReason = domain.UnreadReason(r.unreadReason.String)
	doc.MTime = time.Unix(0, r.mtimeNS)
}

// contentColumns is the canonical content column list, paired with
// contentRow.
//
// The retry columns are projected here rather than only on the queue path so
// that "which content is this" has one answer. The hazard that buys is in
// the other direction: a queue write must never round-trip a Content through
// a blanket upsert that would reset attempts, next_retry_at or last_error —
// see queue.go.
const contentColumns = "content_hash, state, stage_versions, attempts, next_retry_at, last_error"

// contentRow holds the raw content column values that need conversion after
// Scan.
type contentRow struct {
	state     string
	stageRaw  string
	nextRetry sql.NullInt64  // unix seconds; NULL = not scheduled
	lastError sql.NullString // NULL = no failure recorded
}

// targets returns Scan destinations matching contentColumns order.
func (r *contentRow) targets(c *domain.Content) []any {
	return []any{&c.Hash, &r.state, &r.stageRaw, &c.Attempts, &r.nextRetry, &r.lastError}
}

// finish converts the raw values into their domain form.
func (r *contentRow) finish(c *domain.Content) error {
	c.State = domain.ContentState(r.state)
	// NULL next_retry_at means "due now", which is the zero Time: the
	// scheduler compares with !After(now), so zero is always due.
	if r.nextRetry.Valid {
		c.NextRetryAt = time.Unix(r.nextRetry.Int64, 0)
	} else {
		c.NextRetryAt = time.Time{}
	}
	c.LastError = r.lastError.String
	if r.stageRaw == "" || r.stageRaw == "{}" {
		c.StageVersions = nil
		return nil
	}
	if err := json.Unmarshal([]byte(r.stageRaw), &c.StageVersions); err != nil {
		return fmt.Errorf("corrupt stage_versions for %s: %w", c.Hash, err)
	}
	return nil
}

// marshalStageVersions renders the map for storage (nil → "{}").
func marshalStageVersions(sv map[string]string) (string, error) {
	if len(sv) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(sv)
	if err != nil {
		return "", fmt.Errorf("marshal stage_versions: %w", err)
	}
	return string(b), nil
}

// nullable renders "" as SQL NULL — the write-side counterpart of the
// documentRow pair.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

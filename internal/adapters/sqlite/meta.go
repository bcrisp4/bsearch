package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// docColumns is the canonical documents column list, paired with docRow.
// One definition so every query path (GetByPath, SearchVectors, future FTS)
// hydrates identical domain.Documents.
//
// The retry columns are projected here rather than only on the queue path so
// that "which document is this" has one answer. The hazard that buys is in
// the other direction: UpsertDocument resets attempts, next_retry_at and
// last_error by design (a file change earns a fresh budget), so a queue write
// must never round-trip a Document through it — see queue.go.
var docColumns = []string{
	"id", "path", "content_hash", "size", "mtime", "state", "stage_versions",
	"attempts", "next_retry_at", "last_error",
}

// prefixDocColumns renders docColumns with a table alias ("d.id, d.path, …").
func prefixDocColumns(alias string) string {
	cols := make([]string, len(docColumns))
	for i, c := range docColumns {
		cols[i] = alias + "." + c
	}
	return strings.Join(cols, ", ")
}

// docRow holds the raw column values that need conversion after Scan.
type docRow struct {
	mtimeNS   int64
	state     string
	stageRaw  string
	nextRetry sql.NullInt64  // unix seconds; NULL = not scheduled
	lastError sql.NullString // NULL = no failure recorded
}

// targets returns Scan destinations matching docColumns order.
func (r *docRow) targets(doc *domain.Document) []any {
	return []any{
		&doc.ID, &doc.Path, &doc.ContentHash, &doc.Size, &r.mtimeNS, &r.state, &r.stageRaw,
		&doc.Attempts, &r.nextRetry, &r.lastError,
	}
}

// finish converts the raw values into their domain form.
func (r *docRow) finish(doc *domain.Document) error {
	doc.MTime = time.Unix(0, r.mtimeNS)
	doc.State = domain.DocState(r.state)
	// NULL next_retry_at means "due now", which is the zero Time: the
	// scheduler compares with !After(now), so zero is always due.
	if r.nextRetry.Valid {
		doc.NextRetryAt = time.Unix(r.nextRetry.Int64, 0)
	} else {
		doc.NextRetryAt = time.Time{}
	}
	doc.LastError = r.lastError.String
	if r.stageRaw == "" || r.stageRaw == "{}" {
		doc.StageVersions = nil
		return nil
	}
	if err := json.Unmarshal([]byte(r.stageRaw), &doc.StageVersions); err != nil {
		return fmt.Errorf("corrupt stage_versions for %s: %w", doc.ID, err)
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

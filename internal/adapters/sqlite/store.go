package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
)

// Store implements the domain storage ports over one DB.
type Store struct {
	db *DB
}

// NewStore wraps an open DB in the storage ports.
func NewStore(db *DB) *Store { return &Store{db: db} }

var (
	_ domain.DocumentStore = (*Store)(nil)
	_ domain.ContentStore  = (*Store)(nil)
)

// withTx runs fn in one writer transaction (IMMEDIATE via the pool's
// _txlock), committing on nil and rolling back on error.
func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// UpsertDocuments writes the batch in one short IMMEDIATE transaction (#34):
// content rows first (created at discovered if absent, never touched if
// present — a second identical file schedules no work), then the documents
// rows, wholesale.
//
// The eager content insert is what keeps dispatch a plain predicate over
// content rather than an anti-join (ADR 0015). Its updated_at is the insert
// time, which is what makes a just-changed file hot in the claim's recency
// ordering.
//
// The documents upsert replaces the row completely: a rename is the same
// path-keyed write as an edit, an unread→readable transition is the hash
// filling in as the reason NULLs out, and there are no queue columns here to
// protect — the old UpsertDocument's displacement, liveness and retry-reset
// machinery all died with the surrogate id.
func (s *Store) UpsertDocuments(ctx context.Context, docs []domain.Document) error {
	if len(docs) == 0 {
		return nil
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()

		insContent, err := tx.PrepareContext(ctx, `
			INSERT INTO content (content_hash, state, created_at, updated_at)
			VALUES (?, 'discovered', ?, ?)
			ON CONFLICT (content_hash) DO NOTHING`)
		if err != nil {
			return err
		}
		defer insContent.Close()

		insDoc, err := tx.PrepareContext(ctx, `
			INSERT INTO documents (path, content_hash, unread_reason, size, mtime)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (path) DO UPDATE SET
				content_hash = excluded.content_hash,
				unread_reason = excluded.unread_reason,
				size = excluded.size,
				mtime = excluded.mtime`)
		if err != nil {
			return err
		}
		defer insDoc.Close()

		for _, doc := range docs {
			if doc.ContentHash != "" {
				if _, err := insContent.ExecContext(ctx, doc.ContentHash, now, now); err != nil {
					return fmt.Errorf("insert content %s: %w", doc.ContentHash, err)
				}
			}
			if _, err := insDoc.ExecContext(ctx,
				doc.Path, nullable(doc.ContentHash), nullable(string(doc.UnreadReason)),
				doc.Size, doc.MTime.UnixNano()); err != nil {
				return fmt.Errorf("upsert document %s: %w", doc.Path, err)
			}
		}
		return nil
	})
}

// GetByPath fetches the catalog row for a path; ok is false when the path
// has never been stored.
func (s *Store) GetByPath(ctx context.Context, path string) (domain.Document, bool, error) {
	var (
		doc domain.Document
		raw documentRow
	)
	err := s.db.Reader().QueryRowContext(ctx,
		"SELECT "+documentColumns+" FROM documents WHERE path = ?", path).
		Scan(raw.targets(&doc)...)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.Document{}, false, nil
	case err != nil:
		return domain.Document{}, false, fmt.Errorf("get by path %s: %w", path, err)
	}
	raw.finish(&doc)
	return doc, true, nil
}

// DeleteByPathPrefix removes the document stored at dir and every document
// stored beneath it, reporting how many rows went. One call covers both a
// deleted file and a deleted directory, which is what the caller needs:
// FSEvents reports a vanished path without saying which it was.
//
// Documents only. Content the removed rows referenced keeps its chunks and
// vectors until the orphan sweep collects it — and contributes nothing to
// search in the meantime, because result assembly inner-joins documents. A
// deletion takes effect the moment this returns, whenever the sweep runs.
//
// The range predicate rather than LIKE: '/' is 0x2f and '0' is 0x30, so
// everything under dir sorts between dir+"/" and dir+"0", which the primary
// key on path serves as a range scan and which needs no escaping.
func (s *Store) DeleteByPathPrefix(ctx context.Context, dir string) (int, error) {
	// A prefix delete is the one operation here that can empty the catalog,
	// so the degenerate inputs that would do it are refused rather than
	// obeyed. Trailing separators go first, or "//" would walk straight past
	// a guard written against "/". Neither spelling is reachable from a
	// cleaned absolute event path; this is about what happens when one day
	// it is.
	trimmed := strings.TrimRight(dir, "/")
	if trimmed == "" {
		return 0, fmt.Errorf("delete by path prefix: refusing to purge the catalog for prefix %q", dir)
	}
	dir = trimmed

	var removed int
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"DELETE FROM documents WHERE path = ? OR (path > ? AND path < ?)",
			dir, dir+"/", dir+"0")
		if err != nil {
			return fmt.Errorf("delete documents under %s: %w", dir, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete documents under %s: %w", dir, err)
		}
		removed = int(n)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// StoreChunks replaces c.Hash's chunks and records its state and stage
// versions in one short IMMEDIATE transaction, returning chunk IDs in
// ordinal order.
//
// It never creates the content row: discovery creates rows at discovered,
// and a write against a missing row means the content was swept while this
// work was in flight — resurrecting it would make content whose every
// source is gone permanently searchable, so it reports ErrContentGone
// instead.
//
// Retry columns are deliberately untouched. The pipeline commits chunks
// with state=chunked *before* the embed network call, so a reset here would
// park the row at attempts=0 for the whole duration of that call — a crash
// in the window would hand the content a fresh budget on restart, and
// content that can never be embedded would never reach the attempt cap.
func (s *Store) StoreChunks(ctx context.Context, c domain.Content, chunks []domain.Chunk) ([]int64, error) {
	stageJSON, err := marshalStageVersions(c.StageVersions)
	if err != nil {
		return nil, err
	}

	var ids []int64
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"UPDATE content SET state = ?, stage_versions = ?, updated_at = ? WHERE content_hash = ?",
			string(c.State), stageJSON, time.Now().Unix(), c.Hash)
		if err != nil {
			return fmt.Errorf("store chunks for %s: %w", c.Hash, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("store chunks for %s: %w", c.Hash, domain.ErrContentGone)
		}

		// Replace chunks wholesale — and their vectors first, while the old
		// chunk IDs still exist to find them by.
		if err := deleteVectorsTx(ctx, tx, c.Hash); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM chunks WHERE content_hash = ?", c.Hash); err != nil {
			return fmt.Errorf("delete old chunks for %s: %w", c.Hash, err)
		}

		ins, err := tx.PrepareContext(ctx, `
			INSERT INTO chunks (content_hash, ordinal, text, heading_path, byte_start, byte_end)
			VALUES (?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer ins.Close()

		ids = make([]int64, len(chunks))
		for i, ch := range chunks {
			res, err := ins.ExecContext(ctx, c.Hash, ch.Ordinal, ch.Text, ch.HeadingPath, ch.ByteStart, ch.ByteEnd)
			if err != nil {
				return fmt.Errorf("insert chunk %d of %s: %w", ch.Ordinal, c.Hash, err)
			}
			if ids[i], err = res.LastInsertId(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// UpdateContentState flips state (and updated_at) only. Unknown hash is a
// loud error — the caller just processed the row.
func (s *Store) UpdateContentState(ctx context.Context, hash string, state domain.ContentState) error {
	return s.updateContent(ctx, "update state", hash,
		"UPDATE content SET state = ?, updated_at = ? WHERE content_hash = ?",
		string(state), time.Now().Unix(), hash)
}

// MarkFailed sets state=failed and records the reason in last_error.
// Permanent by construction — content is immutable, so nothing resets it;
// a config change re-queues it via ResetStale, which is a different event.
func (s *Store) MarkFailed(ctx context.Context, hash, reason string) error {
	return s.updateContent(ctx, "mark failed", hash,
		"UPDATE content SET state = 'failed', last_error = ?, updated_at = ? WHERE content_hash = ?",
		reason, time.Now().Unix(), hash)
}

// updateContent runs one UPDATE on a single content row inside a writer
// transaction, erroring loudly when the row doesn't exist.
func (s *Store) updateContent(ctx context.Context, op, hash, query string, args ...any) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("%s for %s: %w", op, hash, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// Not a bug in the caller: the row can be swept between claiming
			// content and finishing with it, and the answer to that is to
			// stop working on it, not to fail the daemon.
			return fmt.Errorf("%s for %s: %w", op, hash, domain.ErrContentGone)
		}
		return nil
	})
}

// ListWorkItems returns one WorkItem per content row — the pipeline's
// one-shot equivalent of a full claim, ordered by path for stable output.
// Terminal rows are included; whether a row is stale against current stage
// versions, or failed and not worth another attempt, is the caller's call.
//
// The path is the content's primary: newest mtime, tie-broken by path
// ascending — GetWork's rule, and search's. Content no document references
// (orphaned, sweep pending) has no path to read from and is omitted.
func (s *Store) ListWorkItems(ctx context.Context) ([]domain.WorkItem, error) {
	return s.queryWorkItems(ctx, "list work items", `
		SELECT `+prefixedContentColumns+`, d.path
		FROM content c
		JOIN documents d ON d.content_hash = c.content_hash
		WHERE d.path = (
			SELECT d2.path FROM documents d2
			WHERE d2.content_hash = c.content_hash
			ORDER BY d2.mtime DESC, d2.path ASC LIMIT 1)
		ORDER BY d.path`)
}

// prefixedContentColumns is contentColumns under the alias every join in
// this package gives the content table.
const prefixedContentColumns = "c.content_hash, c.state, c.stage_versions, c.attempts, c.next_retry_at, c.last_error"

// queryWorkItems runs a SELECT projecting prefixedContentColumns + a path
// column and hydrates every row. op prefixes errors.
func (s *Store) queryWorkItems(ctx context.Context, op, query string, args ...any) ([]domain.WorkItem, error) {
	rows, err := s.db.Reader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err below reports failures

	var items []domain.WorkItem
	for rows.Next() {
		var (
			item domain.WorkItem
			raw  contentRow
		)
		targets := append(raw.targets(&item.Content), &item.Path)
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		if err := raw.finish(&item.Content); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return items, nil
}

// queryContents runs a content SELECT (projecting contentColumns) and
// hydrates every row. op prefixes errors.
func (s *Store) queryContents(ctx context.Context, op, query string, args ...any) ([]domain.Content, error) {
	rows, err := s.db.Reader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err below reports failures

	var contents []domain.Content
	for rows.Next() {
		var (
			c   domain.Content
			raw contentRow
		)
		if err := rows.Scan(raw.targets(&c)...); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		if err := raw.finish(&c); err != nil {
			return nil, err
		}
		contents = append(contents, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return contents, nil
}

// SweepOrphans deletes content no documents row references — the other half
// of deletion (DeleteByPathPrefix removes only the documents row) and of
// edits (discovery re-points a path and leaves the old hash behind). One
// transaction, vectors first across every generation while the chunk ids
// still resolve to find them by, then the content rows; chunks and
// summaries follow by FK cascade. Returns how many contents went.
//
// ONE transaction, never find-then-delete across two: a file restored to
// prior content in the gap (editor undo, `git checkout`, a sync client)
// would lose derived data its hash had started referencing again — issue
// #63's signature, rebuilt from new machinery.
//
// NOT EXISTS, never NOT IN. documents.content_hash is nullable (unread
// rows), and `hash NOT IN (SELECT content_hash FROM documents)` goes
// UNKNOWN for every candidate the moment one NULL is present — the sweep
// would delete zero rows, permanently, with no error and no log. The shared
// store-test seed keeps a NULL-hash row planted precisely so a rewrite in
// that direction fails on arrival (see newTestStore).
func (s *Store) SweepOrphans(ctx context.Context) (int, error) {
	var removed int
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		tables, err := listVecTables(ctx, tx)
		if err != nil {
			return err
		}
		for table := range tables {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
				DELETE FROM %s WHERE rowid IN (
					SELECT c.id FROM chunks c
					WHERE NOT EXISTS (SELECT 1 FROM documents d
						WHERE d.content_hash = c.content_hash))`, table)); err != nil {
				return fmt.Errorf("sweep orphan vectors from %s: %w", table, err)
			}
		}
		res, err := tx.ExecContext(ctx, `
			DELETE FROM content WHERE NOT EXISTS (
				SELECT 1 FROM documents d
				WHERE d.content_hash = content.content_hash)`)
		if err != nil {
			return fmt.Errorf("sweep orphan content: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("sweep orphan content: %w", err)
		}
		removed = int(n)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// deleteVectorsTx removes a content's vec rows from EVERY generation, not
// just the current one — content re-chunked while model B is current must
// not resurface as orphan rowids when the user switches back to model A's
// table. Generations are few (one per model tried), so the sweep is cheap.
func deleteVectorsTx(ctx context.Context, tx *sql.Tx, hash string) error {
	tables, err := listVecTables(ctx, tx)
	if err != nil {
		return err
	}
	for table := range tables {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			"DELETE FROM %s WHERE rowid IN (SELECT id FROM chunks WHERE content_hash = ?)", table),
			hash); err != nil {
			return fmt.Errorf("delete vectors for %s from %s: %w", hash, table, err)
		}
	}
	return nil
}

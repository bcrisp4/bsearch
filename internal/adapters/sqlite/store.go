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

var _ domain.DocumentStore = (*Store)(nil)

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

// UpsertDocument writes the document row and replaces its chunks in one
// short IMMEDIATE transaction. Chunk IDs are returned in ordinal order.
//
// Semantics: the document owns its path. Any other catalog row still holding
// the path (e.g. deleted-and-recreated file whose old row wasn't purged yet)
// is displaced — removed with its chunks and vectors — instead of failing
// the UNIQUE(path) constraint. Which document ID a path gets (rename/copy
// detection) is discovery's policy; by the time this is called, it has
// been decided.
//
// An upsert resets the retry columns (attempts, next_retry_at, last_error)
// only when the incoming state is `discovered`, which is discovery's marker
// for content that is new or has changed on disk — the case DESIGN.md's "A
// file change resets failed" is about.
//
// Every other state leaves them alone, and that distinction is load-bearing
// rather than tidiness. The pipeline commits chunks through this method with
// state=chunked *before* the embed network call, so an unconditional reset
// would park the row at attempts=0 for the whole duration of that call: a
// crash in the window would hand the document a fresh budget on restart, and
// a document that can never be embedded would never reach the attempt cap.
// The scheduler restores the count from the claimed Document afterwards, so
// the normal path was always correct — it was only the crash window that was
// not.
func (s *Store) UpsertDocument(ctx context.Context, doc domain.Document, chunks []domain.Chunk) ([]int64, error) {
	stageJSON, err := marshalStageVersions(doc.StageVersions)
	if err != nil {
		return nil, err
	}

	var ids []int64
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		// Displace any other row holding this path.
		var displaced string
		err := tx.QueryRowContext(ctx,
			"SELECT id FROM documents WHERE path = ? AND id != ?", doc.Path, doc.ID).Scan(&displaced)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// Path free (or already ours).
		case err != nil:
			return fmt.Errorf("check path owner: %w", err)
		default:
			if err := deleteDocumentTx(ctx, tx, displaced); err != nil {
				return fmt.Errorf("displace %s from %s: %w", displaced, doc.Path, err)
			}
		}

		now := time.Now().Unix()
		// Bound rather than compared in SQL against a literal, so the rule
		// reads off domain.DocStateDiscovered and cannot drift from it.
		freshFile := doc.State == domain.DocStateDiscovered

		// Only discovery creates rows. A pipeline write lands seconds after
		// the document was claimed, and in those seconds the file can be
		// deleted and the watcher's reconcile can purge it — at which point
		// this INSERT would put it back, and the pipeline would go on to
		// embed and finalize a document whose file is gone. Nothing would
		// ever notice: the row looks indexed, and search serves it.
		if !freshFile {
			var live int
			switch err := tx.QueryRowContext(ctx,
				"SELECT 1 FROM documents WHERE id = ?", doc.ID).Scan(&live); {
			case errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("upsert document %s (%s): %w", doc.ID, doc.Path, domain.ErrDocumentGone)
			case err != nil:
				return fmt.Errorf("check document %s still exists: %w", doc.ID, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO documents (id, path, content_hash, size, mtime, state, stage_versions, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				path = excluded.path,
				content_hash = excluded.content_hash,
				size = excluded.size,
				mtime = excluded.mtime,
				state = excluded.state,
				stage_versions = excluded.stage_versions,
				attempts = CASE WHEN ? THEN 0 ELSE documents.attempts END,
				next_retry_at = CASE WHEN ? THEN NULL ELSE documents.next_retry_at END,
				last_error = CASE WHEN ? THEN NULL ELSE documents.last_error END,
				updated_at = excluded.updated_at`,
			doc.ID, doc.Path, doc.ContentHash, doc.Size, doc.MTime.UnixNano(),
			string(doc.State), stageJSON, now, now,
			freshFile, freshFile, freshFile); err != nil {
			return fmt.Errorf("upsert document %s: %w", doc.ID, err)
		}

		// Replace chunks wholesale — and their vectors first, while the old
		// chunk IDs still exist to find them by.
		if err := deleteVectorsTx(ctx, tx, doc.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM chunks WHERE doc_id = ?", doc.ID); err != nil {
			return fmt.Errorf("delete old chunks for %s: %w", doc.ID, err)
		}

		ins, err := tx.PrepareContext(ctx, `
			INSERT INTO chunks (doc_id, ordinal, text, heading_path, byte_start, byte_end)
			VALUES (?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer ins.Close()

		ids = make([]int64, len(chunks))
		for i, c := range chunks {
			res, err := ins.ExecContext(ctx, doc.ID, c.Ordinal, c.Text, c.HeadingPath, c.ByteStart, c.ByteEnd)
			if err != nil {
				return fmt.Errorf("insert chunk %d of %s: %w", c.Ordinal, doc.ID, err)
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

// GetByPath fetches the catalog row for a path; ok is false when the path
// has never been stored.
func (s *Store) GetByPath(ctx context.Context, path string) (domain.Document, bool, error) {
	var (
		doc domain.Document
		raw docRow
	)
	err := s.db.Reader().QueryRowContext(ctx,
		"SELECT "+strings.Join(docColumns, ", ")+" FROM documents WHERE path = ?", path).
		Scan(raw.targets(&doc)...)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.Document{}, false, nil
	case err != nil:
		return domain.Document{}, false, fmt.Errorf("get by path %s: %w", path, err)
	}
	if err := raw.finish(&doc); err != nil {
		return domain.Document{}, false, err
	}
	return doc, true, nil
}

// GetByID fetches one catalog row, reporting domain.ErrDocumentGone when the
// id is not there.
func (s *Store) GetByID(ctx context.Context, docID string) (domain.Document, error) {
	var (
		doc domain.Document
		raw docRow
	)
	err := s.db.Reader().QueryRowContext(ctx,
		"SELECT "+strings.Join(docColumns, ", ")+" FROM documents WHERE id = ?", docID).
		Scan(raw.targets(&doc)...)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.Document{}, fmt.Errorf("get %s: %w", docID, domain.ErrDocumentGone)
	case err != nil:
		return domain.Document{}, fmt.Errorf("get by id %s: %w", docID, err)
	}
	if err := raw.finish(&doc); err != nil {
		return domain.Document{}, err
	}
	return doc, nil
}

// GetByContentHash returns every catalog row with this content hash, in
// stable id order — discovery's rename detection input.
func (s *Store) GetByContentHash(ctx context.Context, hash string) ([]domain.Document, error) {
	return s.queryDocs(ctx, "get by content hash",
		"SELECT "+strings.Join(docColumns, ", ")+" FROM documents WHERE content_hash = ? ORDER BY id", hash)
}

// queryDocs runs a documents SELECT (projecting docColumns) and hydrates
// every row. op prefixes errors.
func (s *Store) queryDocs(ctx context.Context, op, query string, args ...any) ([]domain.Document, error) {
	rows, err := s.db.Reader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()

	var docs []domain.Document
	for rows.Next() {
		var (
			doc domain.Document
			raw docRow
		)
		if err := rows.Scan(raw.targets(&doc)...); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		if err := raw.finish(&doc); err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return docs, nil
}

// UpdateDocumentStat refreshes size/mtime on an existing row, leaving
// state, chunks, stage versions, and retry columns untouched. Unknown
// docID is a loud error — the caller just read the row.
func (s *Store) UpdateDocumentStat(ctx context.Context, docID string, size int64, mtime time.Time) error {
	return s.updateDoc(ctx, "update stat", docID,
		"UPDATE documents SET size = ?, mtime = ?, updated_at = ? WHERE id = ?",
		size, mtime.UnixNano(), time.Now().Unix(), docID)
}

// ListIndexable returns every catalog row the pipeline may need to work on
// — every state except deleted — ordered by path. Indexed and failed rows
// are included; whether a row is stale (or still failed) against current
// stage versions is the caller's call.
func (s *Store) ListIndexable(ctx context.Context) ([]domain.Document, error) {
	return s.queryDocs(ctx, "list indexable",
		"SELECT "+strings.Join(docColumns, ", ")+
			" FROM documents WHERE state != 'deleted' ORDER BY path")
}

// CountsByState returns the number of catalog rows in each document state.
// Every state in domain.DocStates is present, zero included: a reporting
// consumer must never have to tell "no documents here" apart from "this
// build didn't know about the state" (DESIGN.md: status is observable).
// A state in the database that this build doesn't know is still reported —
// dropping it would hide exactly the drift worth seeing.
func (s *Store) CountsByState(ctx context.Context) (map[domain.DocState]int, error) {
	counts := make(map[domain.DocState]int, len(domain.DocStates))
	for _, state := range domain.DocStates {
		counts[state] = 0
	}

	rows, err := s.db.Reader().QueryContext(ctx,
		"SELECT state, count(*) FROM documents GROUP BY state")
	if err != nil {
		return nil, fmt.Errorf("count documents by state: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err below reports failures

	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, fmt.Errorf("count documents by state: %w", err)
		}
		counts[domain.DocState(state)] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count documents by state: %w", err)
	}
	return counts, nil
}

// UpdateDocumentState flips state (and updated_at) only. Unknown docID is a
// loud error — the caller just processed the row.
func (s *Store) UpdateDocumentState(ctx context.Context, docID string, state domain.DocState) error {
	return s.updateDoc(ctx, "update state", docID,
		"UPDATE documents SET state = ?, updated_at = ? WHERE id = ?",
		string(state), time.Now().Unix(), docID)
}

// MarkFailed sets state=failed and records the reason in last_error.
func (s *Store) MarkFailed(ctx context.Context, docID, reason string) error {
	return s.updateDoc(ctx, "mark failed", docID,
		"UPDATE documents SET state = 'failed', last_error = ?, updated_at = ? WHERE id = ?",
		reason, time.Now().Unix(), docID)
}

// updateDoc runs one UPDATE on a single documents row inside a writer
// transaction, erroring loudly when the row doesn't exist.
func (s *Store) updateDoc(ctx context.Context, op, docID, query string, args ...any) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("%s for %s: %w", op, docID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// Not a bug in the caller: the row can be purged between
			// claiming a document and finishing with it, and the answer to
			// that is to stop working on it, not to fail the daemon.
			return fmt.Errorf("%s for %s: %w", op, docID, domain.ErrDocumentGone)
		}
		return nil
	})
}

// DeleteDocument removes the document and everything derived from it.
func (s *Store) DeleteDocument(ctx context.Context, docID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		return deleteDocumentTx(ctx, tx, docID)
	})
}

// DeleteByPathPrefix removes the document stored at dir and every document
// stored beneath it, reporting how many rows went. One call covers both a
// deleted file and a deleted directory, which is what the caller needs:
// FSEvents reports a vanished path without saying which it was.
//
// The range predicate rather than LIKE: '/' is 0x2f and '0' is 0x30, so
// everything under dir sorts between dir+"/" and dir+"0", which the UNIQUE
// index on path serves as a range scan and which needs no escaping.
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

	// One statement per vec generation plus one for the documents, however
	// many rows are under dir. Deleting row by row would run a meta-table
	// scan per document and hold the single writer connection for the length
	// of the subtree — `rm -rf` on a large indexed folder is exactly the
	// input, and a write transaction that scales with it is what the
	// busy-timeout discipline exists to avoid.
	const under = "documents.path = ? OR (documents.path > ? AND documents.path < ?)"
	args := []any{dir, dir + "/", dir + "0"}

	var removed int
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		// Vectors first, while the chunk rows still exist to find them by,
		// and across every generation — a document deleted while model B is
		// current must not resurface as orphan rowids under model A.
		tables, err := listVecTables(ctx, tx)
		if err != nil {
			return err
		}
		for table := range tables {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
				DELETE FROM %s WHERE rowid IN (
					SELECT chunks.id FROM chunks
					JOIN documents ON documents.id = chunks.doc_id
					WHERE %s)`, table, under), args...); err != nil {
				return fmt.Errorf("delete vectors under %s from %s: %w", dir, table, err)
			}
		}

		// Chunks and summaries cascade via FK.
		res, err := tx.ExecContext(ctx,
			"DELETE FROM documents WHERE "+under, args...)
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

// deleteDocumentTx removes one document inside an open transaction. Chunks
// and summaries cascade via FK; vec rows have no FK (virtual table) and are
// deleted explicitly first, while the chunk rows still exist to find them by.
func deleteDocumentTx(ctx context.Context, tx *sql.Tx, docID string) error {
	if err := deleteVectorsTx(ctx, tx, docID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM documents WHERE id = ?", docID); err != nil {
		return fmt.Errorf("delete document %s: %w", docID, err)
	}
	return nil
}

// deleteVectorsTx removes a document's vec rows from EVERY generation, not
// just the current one — a doc deleted while model B is current must not
// resurface as orphan rowids when the user switches back to model A's
// table. Generations are few (one per model tried), so the sweep is cheap.
func deleteVectorsTx(ctx context.Context, tx *sql.Tx, docID string) error {
	tables, err := listVecTables(ctx, tx)
	if err != nil {
		return err
	}
	for table := range tables {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			"DELETE FROM %s WHERE rowid IN (SELECT id FROM chunks WHERE doc_id = ?)", table),
			docID); err != nil {
			return fmt.Errorf("delete vectors for %s from %s: %w", docID, table, err)
		}
	}
	return nil
}

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
// An upsert also resets the retry columns (attempts, next_retry_at,
// last_error): a changed file starts fresh (DESIGN.md: "A file change
// resets failed").
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
				attempts = 0,
				next_retry_at = NULL,
				last_error = NULL,
				updated_at = excluded.updated_at`,
			doc.ID, doc.Path, doc.ContentHash, doc.Size, doc.MTime.UnixNano(),
			string(doc.State), stageJSON, now, now); err != nil {
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

// DeleteDocument removes the document and everything derived from it.
func (s *Store) DeleteDocument(ctx context.Context, docID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		return deleteDocumentTx(ctx, tx, docID)
	})
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

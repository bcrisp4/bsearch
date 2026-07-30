package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	_ domain.ScanState     = (*Store)(nil)
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
// content rows first (created at discovered if absent), then the documents
// rows, wholesale.
//
// The eager content insert is what keeps dispatch a plain predicate over
// content rather than an anti-join (ADR 0015). A re-referenced hash splits
// on whether the row is terminal: non-terminal content gets its updated_at
// bumped, so an undo, a `git checkout`, or a copy of not-yet-indexed bytes
// is hot in the claim's recency ordering rather than stranded behind the
// aging slice; terminal content is never touched — a second identical file
// schedules no work, and re-discovering indexed or failed bytes must not
// drag them back through the pipeline.
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

		//nolint:gosec // G202: the spliced text is domain.TerminalContentStates, not input
		insContent, err := tx.PrepareContext(ctx, `
			INSERT INTO content (content_hash, state, created_at, updated_at)
			VALUES (?, 'discovered', ?, ?)
			ON CONFLICT (content_hash) DO UPDATE SET updated_at = excluded.updated_at
				WHERE state NOT IN (`+terminalStatesSQL()+`)`)
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

// pathBatch bounds one DeleteByPaths statement. Well under SQLite's variable
// limit, and the same size as every other batched write here, so a large
// purge is many short transactions rather than one long one.
const pathBatch = 256

// DeleteByPaths removes exactly the named documents, reporting how many rows
// went. Nothing beneath them — that is DeleteByPathPrefix's job, and the two
// are separate because their callers hold different evidence.
//
// A prefix delete answers an event, which names one path and cannot say
// whether it was a file or a directory or what was under it. This answers a
// catalog pass, which has already enumerated the children: a vanished subtree
// reaches here as N paths that each independently failed to stat, so the
// blast radius of a wrong answer stays at one row instead of scaling with
// whatever happened to sort beneath it (ADR 0016).
//
// Empty input is a no-op. An empty path is refused and nothing is deleted —
// it is not a legal document path, and the caller that produced one is
// confused about something worth failing loudly over.
func (s *Store) DeleteByPaths(ctx context.Context, paths []string) (int, error) {
	if len(paths) == 0 {
		return 0, nil
	}
	for _, path := range paths {
		if path == "" {
			return 0, errors.New("delete by paths: refusing a batch containing an empty path")
		}
	}

	// One transaction per chunk, not one around all of them: the point of
	// pathBatch is that each write stays short under the busy-timeout
	// discipline, and wrapping every chunk in a single transaction would
	// reinstate exactly the long write it exists to avoid. A caller passing
	// thousands of paths therefore gets several short transactions and a
	// partial delete if one fails — which is safe here, since every path
	// removed was independently judged gone and the next pass re-derives the
	// rest.
	var removed int
	for chunk := range slices.Chunk(paths, pathBatch) {
		// Counted after the commit, never inside the closure: a chunk whose
		// DELETE ran but whose commit failed removed nothing, and callers
		// treat this number as "rows that actually went".
		var n int
		err := s.withTx(ctx, func(tx *sql.Tx) error {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
			args := make([]any, len(chunk))
			for i, p := range chunk {
				args[i] = p
			}
			//nolint:gosec // G202: the spliced text is ?-placeholders, not input
			res, err := tx.ExecContext(ctx,
				"DELETE FROM documents WHERE path IN ("+placeholders+")", args...)
			if err != nil {
				return fmt.Errorf("delete %d documents: %w", len(chunk), err)
			}
			affected, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("delete %d documents: %w", len(chunk), err)
			}
			n = int(affected)
			return nil
		})
		if err != nil {
			return removed, err
		}
		removed += n
	}
	return removed, nil
}

// ListPaths returns up to limit document paths in ascending path order,
// starting strictly after `after` — the cursor discovery walks the catalog
// with when the question is which rows the filesystem no longer has.
//
// On the reader pool and outside any transaction: the caller stats every path
// it gets back, so holding a read transaction across the loop would pin the
// WAL for the length of a corpus-wide walk and hide the caller's own deletes
// from later pages.
//
// Paginating while the caller deletes is safe because the cursor is a value
// rather than a row reference, and every path the caller deletes sorts at or
// before it — a property that holds only because those deletes are per-path.
//
// Every row, INCLUDING unread rows whose content_hash is NULL. A denied or
// dataless file is still a file with a catalog row, and filtering them out
// here would make them unpurgeable forever with nothing to signal it.
func (s *Store) ListPaths(ctx context.Context, after string, limit int) ([]string, error) {
	rows, err := s.db.Reader().QueryContext(ctx,
		"SELECT path FROM documents WHERE path > ? ORDER BY path LIMIT ?", after, limit)
	if err != nil {
		return nil, fmt.Errorf("list paths after %q: %w", after, err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err below reports failures
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("list paths after %q: %w", after, err)
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list paths after %q: %w", after, err)
	}
	return paths, nil
}

// metaKnownMounts holds discovery's remembered mount points, a JSON array.
const metaKnownMounts = "discovery.mounts"

// KnownMounts returns the mount points discovery has observed inside the
// include roots, and whether anything has ever been written. An absent key
// means no volume evidence exists yet, which is a different situation from a
// set that is legitimately empty.
func (s *Store) KnownMounts(ctx context.Context) ([]string, bool, error) {
	raw, ok, err := getMeta(ctx, s.db.Reader(), metaKnownMounts)
	if err != nil || !ok {
		return nil, false, err
	}
	var mounts []string
	if err := json.Unmarshal([]byte(raw), &mounts); err != nil {
		// Wrapped as corrupt rather than as a plain failure: the caller has to
		// tell "this value will never parse, overwrite it" from "the database
		// would not answer, leave it alone".
		return nil, false, fmt.Errorf("%w: %w", domain.ErrScanStateCorrupt, err)
	}
	return mounts, true, nil
}

// SetKnownMounts replaces the remembered set.
func (s *Store) SetKnownMounts(ctx context.Context, mounts []string) error {
	if mounts == nil {
		mounts = []string{}
	}
	raw, err := json.Marshal(mounts)
	if err != nil {
		return fmt.Errorf("encode remembered mounts: %w", err)
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		return setMeta(ctx, tx, metaKnownMounts, string(raw))
	})
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

// MarkFailed sets state=failed, records the reason in last_error, and
// deletes the content's chunks and vectors (every generation) in the same
// transaction. Failed content must not serve stale chunks, and there are two
// routes to failed — the pipeline's normalize failure and the scheduler's
// attempt cap — which must leave identical state behind, or M4's FTS5
// external-content table would turn the cap route's leftovers into live
// BM25 hits.
//
// Permanent by construction — content is immutable, so nothing resets it;
// a config change re-queues it via ResetStale, which is a different event.
func (s *Store) MarkFailed(ctx context.Context, hash, reason string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"UPDATE content SET state = 'failed', last_error = ?, updated_at = ? WHERE content_hash = ?",
			reason, time.Now().Unix(), hash)
		if err != nil {
			return fmt.Errorf("mark failed for %s: %w", hash, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// Not a bug in the caller: the row can be swept between claiming
			// content and giving up on it — see updateContent.
			return fmt.Errorf("mark failed for %s: %w", hash, domain.ErrContentGone)
		}
		// Vectors first, while the chunk IDs still exist to find them by —
		// the same discipline as StoreChunks' replacement path.
		if err := deleteVectorsTx(ctx, tx, hash); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM chunks WHERE content_hash = ?", hash); err != nil {
			return fmt.Errorf("delete chunks of failed %s: %w", hash, err)
		}
		return nil
	})
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
		// Primary only: the one-shot path (pipeline.Run, eval) works
		// corpora with no duplicate files, so the fallback list is just
		// the primary.
		item.Paths = []string{item.Path}
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

// sweepBatch bounds one sweep transaction: enough that a big purge takes
// tens of transactions rather than thousands, small enough that each stays
// a short write under the busy-timeout discipline — the repo rule every
// other writer here follows, and an unbounded `rm -rf` sweep would not.
const sweepBatch = 256

// SweepOrphans deletes content no documents row references — the other half
// of deletion (DeleteByPathPrefix removes only the documents row) and of
// edits (discovery re-points a path and leaves the old hash behind). scope
// says whether terminal orphans are collected too (see domain.SweepScope).
// Returns how many contents went; chunks and summaries follow by FK
// cascade, vectors are deleted first, per hash, across every generation,
// while the chunk ids still resolve to find them by.
//
// Shaped for the common case: a cheap EXISTS probe on the reader answers
// "anything to do?" without touching the writer or scanning a vector
// table — the armed-but-empty sweep costs one indexed anti-join, not a
// full vec0 scan per generation. Real work is cursor-paginated batches of
// sweepBatch hashes, one short IMMEDIATE transaction each, and every batch
// RE-VERIFIES orphan-ness inside its own transaction — find-then-delete
// across transactions is exactly what would let a file restored to prior
// content in the gap (editor undo, `git checkout`, a sync client) lose
// derived data its hash had started referencing again (issue #63's
// signature), so the find inside the transaction is the one that counts.
//
// NOT EXISTS, never NOT IN. documents.content_hash is nullable (unread
// rows), and `hash NOT IN (SELECT content_hash FROM documents)` goes
// UNKNOWN for every candidate the moment one NULL is present — the sweep
// would delete zero rows, permanently, with no error and no log. The shared
// store-test seed keeps a NULL-hash row planted precisely so a rewrite in
// that direction fails on arrival (see newTestStore).
func (s *Store) SweepOrphans(ctx context.Context, scope domain.SweepScope) (int, error) {
	stateFilter := ""
	if scope == domain.SweepScopeQueue {
		//nolint:gosec // G202: the spliced text is domain.TerminalContentStates, not input
		stateFilter = "state NOT IN (" + terminalStatesSQL() + ") AND "
	}
	orphaned := `NOT EXISTS (
		SELECT 1 FROM documents d WHERE d.content_hash = content.content_hash)`

	var due bool
	if err := s.db.Reader().QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM content WHERE "+stateFilter+orphaned+")").
		Scan(&due); err != nil {
		return 0, fmt.Errorf("probe for orphaned content: %w", err)
	}
	if !due {
		return 0, nil
	}

	removed, cursor := 0, ""
	for {
		// Candidates are read outside the transaction and re-verified
		// inside it; the cursor advances over candidates regardless of the
		// verify's answer, so a hash re-referenced in the gap is skipped
		// without ever being re-collected into a spin.
		batch, err := s.sweepCandidates(ctx, stateFilter+orphaned, cursor)
		if err != nil {
			return removed, fmt.Errorf("collect orphaned content: %w", err)
		}
		if len(batch) == 0 {
			return removed, nil
		}
		cursor = batch[len(batch)-1]

		err = s.withTx(ctx, func(tx *sql.Tx) error {
			doomed, err := verifyOrphans(ctx, tx, stateFilter+orphaned, batch)
			if err != nil {
				return fmt.Errorf("verify orphaned content: %w", err)
			}

			for _, hash := range doomed {
				if err := deleteVectorsTx(ctx, tx, hash); err != nil {
					return err
				}
			}
			if len(doomed) == 0 {
				return nil
			}
			dp := strings.TrimSuffix(strings.Repeat("?,", len(doomed)), ",")
			da := make([]any, len(doomed))
			for i, h := range doomed {
				da[i] = h
			}
			//nolint:gosec // G202: the spliced text is ?-placeholders, not input
			res, err := tx.ExecContext(ctx,
				"DELETE FROM content WHERE content_hash IN ("+dp+")", da...)
			if err != nil {
				return fmt.Errorf("sweep orphaned content: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			removed += int(n)
			return nil
		})
		if err != nil {
			return removed, err
		}
		if len(batch) < sweepBatch {
			return removed, nil
		}
	}
}

// sweepCandidates reads the next batch of orphan candidates after cursor, in
// hash order. A read on the reader pool, deliberately outside any write
// transaction — SweepOrphans re-verifies each batch inside its own.
func (s *Store) sweepCandidates(ctx context.Context, predicate, cursor string) ([]string, error) {
	//nolint:gosec // G202: the spliced text is SweepOrphans' own predicate, not input
	rows, err := s.db.Reader().QueryContext(ctx, `
		SELECT content_hash FROM content
		WHERE content_hash > ? AND `+predicate+`
		ORDER BY content_hash LIMIT ?`, cursor, sweepBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err below reports failures
	var batch []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		batch = append(batch, hash)
	}
	return batch, rows.Err()
}

// verifyOrphans re-evaluates the orphan predicate over batch inside tx —
// the find that counts, because it holds the same lock as the delete.
func verifyOrphans(ctx context.Context, tx *sql.Tx, predicate string, batch []string) ([]string, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
	args := make([]any, len(batch))
	for i, h := range batch {
		args[i] = h
	}
	//nolint:gosec // G202: the spliced text is ?-placeholders and SweepOrphans' own predicate, not input
	rows, err := tx.QueryContext(ctx, `
		SELECT content_hash FROM content
		WHERE content_hash IN (`+placeholders+`) AND `+predicate, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err below reports failures
	var doomed []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		doomed = append(doomed, hash)
	}
	return doomed, rows.Err()
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

package sqlite

import (
	"database/sql"
	"fmt"
	"time"
)

// migrations are applied in order, each in one IMMEDIATE transaction, and
// tracked in schema_migrations. Version N is migrations[N-1]. No down
// migrations: the index is derived data and drop-and-reindex is the fallback
// of last resort (DESIGN.md: Storage).
//
// Static tables only. Vector tables are per-embedding-model and created
// lazily by the adapter once model+dims are known (see vec.go): vec0
// dimensions are fixed at CREATE, and the model is a config/M2 decision, so
// no static migration can create them.
var migrations = []string{
	// v1: catalog + chunks + summaries + meta (DESIGN.md: queue state
	// machine, chunking, pyramid summaries, pipeline metadata).
	`
CREATE TABLE documents (
	id             TEXT PRIMARY KEY,
	path           TEXT NOT NULL UNIQUE,
	content_hash   TEXT NOT NULL,
	size           INTEGER NOT NULL,
	mtime          INTEGER NOT NULL, -- unix nanoseconds
	state          TEXT NOT NULL CHECK (state IN
		('discovered','converted','chunked','embedded','indexed','failed','deleted')),
	attempts       INTEGER NOT NULL DEFAULT 0,
	next_retry_at  INTEGER,          -- unix seconds; NULL = not scheduled
	last_error     TEXT,
	stage_versions TEXT NOT NULL DEFAULT '{}', -- JSON, per-stage versions
	created_at     INTEGER NOT NULL, -- unix seconds
	updated_at     INTEGER NOT NULL  -- unix seconds
) STRICT;

-- Dispatch: SELECT ... WHERE state NOT IN (...) AND next_retry_at <= now.
CREATE INDEX idx_documents_dispatch ON documents (state, next_retry_at);

-- Rename detection: hash match against rows whose path is gone.
CREATE INDEX idx_documents_content_hash ON documents (content_hash);

CREATE TABLE chunks (
	-- AUTOINCREMENT: chunk IDs key vec-table rowids, so a freed ID must
	-- never be reused — a stale vector would silently attach to a new chunk.
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	doc_id       TEXT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
	ordinal      INTEGER NOT NULL,
	text         TEXT NOT NULL,
	heading_path TEXT NOT NULL DEFAULT '',
	byte_start   INTEGER NOT NULL,
	byte_end     INTEGER NOT NULL,
	UNIQUE (doc_id, ordinal) -- also serves as the doc_id lookup index
) STRICT;

CREATE TABLE summaries (
	doc_id TEXT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
	level  INTEGER NOT NULL CHECK (level IN (4, 16, 64)),
	text   TEXT NOT NULL,
	PRIMARY KEY (doc_id, level)
) STRICT;

-- Versioned pipeline metadata: current vec generation, model descriptors,
-- chunker version (DESIGN.md: Pipeline metadata and model migration).
CREATE TABLE meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) STRICT;
`,
}

// migrate brings the schema up to the current version.
func migrate(writer *sql.DB) error {
	if _, err := writer.Exec(
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		) STRICT`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var current int
	if err := writer.QueryRow(
		"SELECT coalesce(max(version), 0) FROM schema_migrations").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	for v := current + 1; v <= len(migrations); v++ {
		if err := applyMigration(writer, v); err != nil {
			return fmt.Errorf("migration %d: %w", v, err)
		}
	}
	return nil
}

func applyMigration(writer *sql.DB, version int) error {
	tx, err := writer.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	if _, err := tx.Exec(migrations[version-1]); err != nil {
		return err
	}
	if _, err := tx.Exec(
		"INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)",
		version, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	return tx.Commit()
}

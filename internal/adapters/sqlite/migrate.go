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
// ADR 0015 replaced this list wholesale rather than appending: the identity
// split was executed as drop-and-reindex while the project was pre-0.1.0,
// so the old shape is unreachable and re-deriving it migration-by-migration
// would reject nothing and document nothing. A database recording a higher
// version than this build carries is from before the rewrite (or from a
// newer build) and is refused by migrate() with the remedy.
//
// Static tables only. Vector tables are per-embedding-model and created
// lazily by the adapter once model+dims are known (see vec.go): vec0
// dimensions are fixed at CREATE, and the model is a config/M2 decision, so
// no static migration can create them.
var migrations = []string{
	// v1: catalog + content queue + chunks + summaries + meta (ADR 0015,
	// DESIGN.md: Identity — path locates, content hash identifies; Queue).
	//
	// documents mirrors the filesystem and keys on path; content is the work
	// queue and everything derived from the bytes, keyed on the content
	// hash. There is no doc_id.
	`
CREATE TABLE content (
	content_hash   TEXT PRIMARY KEY,
	state          TEXT NOT NULL CHECK (state IN
		('discovered','converted','chunked','embedded','indexed','failed')),
	stage_versions TEXT NOT NULL DEFAULT '{}', -- JSON, per-stage versions
	attempts       INTEGER NOT NULL DEFAULT 0,
	next_retry_at  INTEGER,          -- unix seconds; NULL = not scheduled
	last_error     TEXT,
	created_at     INTEGER NOT NULL, -- unix seconds
	updated_at     INTEGER NOT NULL  -- unix seconds
) STRICT;

CREATE TABLE documents (
	path          TEXT PRIMARY KEY,
	content_hash  TEXT REFERENCES content(content_hash),
	unread_reason TEXT CHECK (unread_reason IN ('denied','dataless','io_error')),
	size          INTEGER NOT NULL,
	mtime         INTEGER NOT NULL,  -- unix nanoseconds
	-- Exactly one: either we have the content, or we say why not.
	CHECK ((content_hash IS NULL) <> (unread_reason IS NULL))
) STRICT;

CREATE TABLE chunks (
	-- AUTOINCREMENT: chunk IDs key vec-table rowids (and the future FTS5
	-- external-content table), so a freed ID must never be reused — a stale
	-- vector would silently attach to a new chunk.
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	content_hash TEXT NOT NULL REFERENCES content(content_hash) ON DELETE CASCADE,
	ordinal      INTEGER NOT NULL,
	text         TEXT NOT NULL,
	heading_path TEXT NOT NULL DEFAULT '',
	byte_start   INTEGER NOT NULL,
	byte_end     INTEGER NOT NULL,
	UNIQUE (content_hash, ordinal) -- also serves as the content_hash lookup index
) STRICT;

CREATE TABLE summaries (
	content_hash TEXT NOT NULL REFERENCES content(content_hash) ON DELETE CASCADE,
	level        INTEGER NOT NULL CHECK (level IN (4, 16, 64)),
	text         TEXT NOT NULL,
	PRIMARY KEY (content_hash, level)
) STRICT;

-- Versioned pipeline metadata: current vec generation, model descriptors,
-- chunker version (DESIGN.md: Pipeline metadata and model migration).
CREATE TABLE meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) STRICT;

-- The claim index (ADR 0011). Partial: terminal rows are absent from the
-- B-tree entirely, so claim cost is bounded by the working set rather than
-- by history, and flipping a row terminal costs no index maintenance on
-- the hot path. updated_at leads because it is what the claim orders by:
-- forwards for the aging slice, backwards for the recency slice, one index
-- serving both.
--
-- The WHERE clause duplicates domain.TerminalContentStates. SQLite
-- requires a literal here, so the two are kept honest by a test rather
-- than by construction.
CREATE INDEX idx_content_queue ON content (updated_at)
	WHERE state NOT IN ('indexed', 'failed');

-- The join key for search-result assembly, GetWork's primary-path pick,
-- and the orphan sweep's NOT EXISTS probe.
CREATE INDEX idx_documents_content ON documents (content_hash);
`,
}

// schemaVersion is the schema this build writes and reads: the highest
// migration it carries.
var schemaVersion = len(migrations)

// checkSchema verifies that an existing database is one this build can read,
// without writing to it. Read-only openers (the daemon) use this in place of
// migrate: taking a write lock to discover there is nothing to migrate would
// contend with the indexer for no gain, and a schema the build doesn't
// understand must be reported, not silently served.
func checkSchema(reader *sql.DB) error {
	var current int
	if err := reader.QueryRow(
		"SELECT coalesce(max(version), 0) FROM schema_migrations").Scan(&current); err != nil {
		return fmt.Errorf("read schema version (not a bsearch index database?): %w", err)
	}
	switch {
	case current < schemaVersion:
		return fmt.Errorf("index schema is version %d but this build expects %d — restart the bsearch daemon, which migrates the index at startup",
			current, schemaVersion)
	case current > schemaVersion:
		return fmt.Errorf("index schema is version %d, newer than this build supports (%d) — %s", current, schemaVersion, schemaRemedy)
	}
	return nil
}

// schemaRemedy is what to do about a database recording a version this build
// does not carry. The index is derived data, so the remedy is deletion, not
// repair — but deleting is the user's call, never this code's: an old binary
// pointed at a genuinely newer database must refuse loudly, not destroy it.
const schemaRemedy = "if you have downgraded bsearch, upgrade it again; " +
	"otherwise the schema was rebuilt (ADR 0015) and there is no in-place " +
	"migration — delete the index database and let the daemon reindex"

// migrate brings the schema up to the current version.
//
// A recorded version *above* what this build carries is an error, not a
// no-op: the loop below would silently apply nothing and leave every query
// running against a shape this code has never seen. That is exactly the
// state an ADR 0015-style wholesale replacement leaves an old database in,
// so it must be loud.
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
	if current > len(migrations) {
		return fmt.Errorf("index schema is version %d, newer than this build supports (%d) — %s",
			current, len(migrations), schemaRemedy)
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

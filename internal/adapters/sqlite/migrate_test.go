package sqlite

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/bcrisp4/bsearch/internal/domain"
)

func TestOpenCreatesCurrentSchema(t *testing.T) {
	db := openTestDB(t)

	rows, err := db.Reader().Query(
		"SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	for _, want := range []string{"content", "documents", "chunks", "summaries", "meta", "schema_migrations"} {
		if !slices.Contains(tables, want) {
			t.Errorf("table %q missing; have %v", want, tables)
		}
	}

	for _, want := range []string{"idx_content_queue", "idx_documents_content"} {
		var n int
		if err := db.Reader().QueryRow(
			"SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?", want).Scan(&n); err != nil {
			t.Fatalf("check %s: %v", want, err)
		}
		if n != 1 {
			t.Errorf("index %q missing", want)
		}
	}

	var version int
	err = db.Reader().QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version)
	if err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("schema version = %d, want %d", version, schemaVersion)
	}
}

func TestOpenIsIdempotentAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bsearch.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer db.Close()

	var applied int
	err = db.Reader().QueryRow("SELECT count(*) FROM schema_migrations").Scan(&applied)
	if err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	if applied != schemaVersion {
		t.Errorf("applied migrations = %d, want %d (reopen must not re-apply)", applied, schemaVersion)
	}
}

// The partial claim index duplicates domain.TerminalContentStates as a SQL
// literal, because SQLite needs a literal there and a migration is frozen
// once it ships. If a state is ever added to the terminal set, the index and
// the claim predicate silently stop agreeing — this is what notices. "Names
// exactly": a state missing from the WHERE would bloat the index, an extra
// one would hide claimable rows from it.
func TestPartialClaimIndexMatchesTerminalStates(t *testing.T) {
	db := openTestDB(t)

	var ddl string
	err := db.Reader().QueryRow(
		"SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_content_queue'").Scan(&ddl)
	if err != nil {
		t.Fatalf("read idx_content_queue: %v", err)
	}

	m := regexp.MustCompile(`NOT IN\s*\(([^)]*)\)`).FindStringSubmatch(ddl)
	if m == nil {
		t.Fatalf("idx_content_queue is not a NOT IN partial index; DDL is %s", ddl)
	}
	var got []string
	for _, part := range strings.Split(m[1], ",") {
		got = append(got, strings.Trim(strings.TrimSpace(part), "'"))
	}
	slices.Sort(got)
	var want []string
	for _, state := range domain.TerminalContentStates {
		want = append(want, string(state))
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("idx_content_queue excludes %v, want exactly domain.TerminalContentStates %v; DDL is %s",
			got, want, ddl)
	}
}

func TestSchemaEnforcesIntegrity(t *testing.T) {
	db := openTestDB(t)

	// STRICT + CHECK: bad state value must be rejected.
	_, err := db.Writer().Exec(
		`INSERT INTO content (content_hash, state, created_at, updated_at)
		 VALUES ('h_bogus', 'bogus', 0, 0)`)
	if err == nil {
		t.Error("insert with state='bogus' succeeded, want CHECK violation")
	}

	// FK: chunk referencing missing content must be rejected.
	_, err = db.Writer().Exec(
		`INSERT INTO chunks (content_hash, ordinal, text, heading_path, byte_start, byte_end)
		 VALUES ('h_missing', 0, 'hello', '', 0, 5)`)
	if err == nil {
		t.Error("chunk insert with dangling content_hash succeeded, want FK violation")
	}

	// FK: a document cannot claim a content row that does not exist.
	_, err = db.Writer().Exec(
		`INSERT INTO documents (path, content_hash, size, mtime)
		 VALUES ('/tmp/x.md', 'h_missing', 1, 0)`)
	if err == nil {
		t.Error("document insert with dangling content_hash succeeded, want FK violation")
	}
}

// Every domain.ContentState constant must round-trip through the schema's
// CHECK constraint — the two lists are duplicated (Go and frozen migration
// SQL) and this is what turns drift into a test failure instead of a runtime
// CHECK violation. 'deleted' died with ADR 0015: a deleted file is a removed
// documents row, never a content state.
func TestContentStatesAcceptedBySchema(t *testing.T) {
	db := openTestDB(t)

	if len(domain.ContentStates) != 6 {
		t.Errorf("domain.ContentStates has %d states, want 6 — update the schema CHECK together with it", len(domain.ContentStates))
	}
	for i, state := range domain.ContentStates {
		_, err := db.Writer().Exec(
			"INSERT INTO content (content_hash, state, created_at, updated_at) VALUES (?, ?, 0, 0)",
			fmt.Sprintf("h_%d", i), string(state))
		if err != nil {
			t.Errorf("state %q rejected by schema: %v", state, err)
		}
	}

	if _, err := db.Writer().Exec(
		"INSERT INTO content (content_hash, state, created_at, updated_at) VALUES ('h_del', 'deleted', 0, 0)"); err == nil {
		t.Error("state 'deleted' accepted by schema; it does not exist under ADR 0015")
	}
}

// Exactly one of content_hash / unread_reason: either we have the content, or
// we say why not. Both and neither are contradictions the schema itself must
// refuse (ADR 0015).
func TestDocumentsRequireExactlyOneOfHashOrReason(t *testing.T) {
	db := openTestDB(t)

	// The hash cases need a content row to satisfy the FK.
	if _, err := db.Writer().Exec(
		"INSERT INTO content (content_hash, state, created_at, updated_at) VALUES ('h_1', 'discovered', 0, 0)"); err != nil {
		t.Fatalf("seed content: %v", err)
	}

	for _, tc := range []struct {
		name         string
		hash, reason any
		wantErr      bool
	}{
		{"hash only", "h_1", nil, false},
		{"reason only", nil, "denied", false},
		{"both set", "h_1", "denied", true},
		{"both NULL", nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.Writer().Exec(
				"INSERT INTO documents (path, content_hash, unread_reason, size, mtime) VALUES (?, ?, ?, 1, 0)",
				"/tmp/"+strings.ReplaceAll(tc.name, " ", "_")+".md", tc.hash, tc.reason)
			if tc.wantErr && err == nil {
				t.Errorf("insert (%s) succeeded, want CHECK violation", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("insert (%s) rejected: %v", tc.name, err)
			}
		})
	}
}

// The reason enum mirrors domain.UnreadReasons the same way the state CHECK
// mirrors domain.ContentStates.
func TestUnreadReasonsAcceptedBySchema(t *testing.T) {
	db := openTestDB(t)

	for i, reason := range domain.UnreadReasons {
		_, err := db.Writer().Exec(
			"INSERT INTO documents (path, unread_reason, size, mtime) VALUES (?, ?, 1, 0)",
			fmt.Sprintf("/tmp/%d.md", i), string(reason))
		if err != nil {
			t.Errorf("unread_reason %q rejected by schema: %v", reason, err)
		}
	}

	if _, err := db.Writer().Exec(
		"INSERT INTO documents (path, unread_reason, size, mtime) VALUES ('/tmp/bogus.md', 'bogus', 1, 0)"); err == nil {
		t.Error("unread_reason 'bogus' accepted, want CHECK violation")
	}
}

// ADR 0015 replaced the migration list wholesale, so a database recording a
// higher version than this build carries is either from before the rewrite or
// from a newer build. The loop would silently apply nothing and leave every
// query running against a shape this code has never seen — it must refuse
// loudly and name the remedy instead.
func TestMigrateRefusesADatabaseFromANewerBuild(t *testing.T) {
	db := openTestDB(t)

	if _, err := db.Writer().Exec(
		"INSERT INTO schema_migrations (version, applied_at) VALUES (99, '2026-01-01T00:00:00Z')"); err != nil {
		t.Fatalf("record future version: %v", err)
	}

	err := migrate(db.Writer())
	if err == nil {
		t.Fatal("migrate() on a newer-versioned database = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "newer than this build") {
		t.Errorf("error %q does not say the schema is newer than this build", err)
	}
	if !strings.Contains(err.Error(), "delete the index database") {
		t.Errorf("error %q does not name the remedy (delete the index database)", err)
	}

	// The read-only opener must refuse the same database the same way.
	if err := checkSchema(db.Reader()); err == nil {
		t.Error("checkSchema on a newer-versioned database = nil, want a refusal")
	} else if !strings.Contains(err.Error(), "newer than this build") {
		t.Errorf("checkSchema error %q does not say the schema is newer than this build", err)
	}
}

// Folding the old migration list into a single entry (ADR 0015) means a
// database created under the old schema's v1 records the same version number
// as the rebuilt schema — the version comparison alone accepts it, and the
// daemon would die on the first catalog touch with a raw "no such table:
// content" instead of the remedy. The content-table probe is what refuses it.
func TestMigrateRefusesAPreRewriteDatabaseWithTheSameVersion(t *testing.T) {
	// Hand-built old-v1 shape: doc_id-keyed documents, no content table,
	// schema_migrations recording version 1 — what 7e16a6a's migration left
	// behind.
	db, err := openPools(filepath.Join(t.TempDir(), "old-v1.db"))
	if err != nil {
		t.Fatalf("openPools: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, ddl := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL) STRICT`,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (1, '2026-01-01T00:00:00Z')`,
		`CREATE TABLE documents (id TEXT PRIMARY KEY, path TEXT NOT NULL UNIQUE,
			content_hash TEXT, state TEXT NOT NULL, size INTEGER, mtime INTEGER) STRICT`,
	} {
		if _, err := db.Writer().Exec(ddl); err != nil {
			t.Fatalf("build old-v1 shape: %v", err)
		}
	}

	err = migrate(db.Writer())
	if err == nil {
		t.Fatal("migrate() on an old-v1 database = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "delete the index database") {
		t.Errorf("error %q does not name the remedy (delete the index database)", err)
	}

	// The read-only opener must refuse the same database the same way.
	if err := checkSchema(db.Reader()); err == nil {
		t.Error("checkSchema on an old-v1 database = nil, want a refusal")
	} else if !strings.Contains(err.Error(), "delete the index database") {
		t.Errorf("checkSchema error %q does not name the remedy", err)
	}
}

func TestCheckSchemaAcceptsTheCurrentSchema(t *testing.T) {
	db := openTestDB(t)
	if err := checkSchema(db.Reader()); err != nil {
		t.Errorf("checkSchema on a freshly migrated database: %v", err)
	}
}

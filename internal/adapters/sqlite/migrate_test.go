package sqlite

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bcrisp4/bsearch/internal/domain"
)

func TestOpenCreatesSchemaV1(t *testing.T) {
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

	for _, want := range []string{"documents", "chunks", "summaries", "meta", "schema_migrations"} {
		if !slices.Contains(tables, want) {
			t.Errorf("table %q missing; have %v", want, tables)
		}
	}

	var version int
	err = db.Reader().QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version)
	if err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	if version != 1 {
		t.Errorf("schema version = %d, want 1", version)
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
	if applied != 1 {
		t.Errorf("applied migrations = %d, want 1 (reopen must not re-apply)", applied)
	}
}

func TestSchemaEnforcesIntegrity(t *testing.T) {
	db := openTestDB(t)

	// STRICT + CHECK: bad state value must be rejected.
	_, err := db.Writer().Exec(
		`INSERT INTO documents (id, path, content_hash, size, mtime, state, attempts, stage_versions, created_at, updated_at)
		 VALUES ('d_1', '/tmp/x.md', 'abc', 1, 0, 'bogus', 0, '{}', 0, 0)`)
	if err == nil {
		t.Error("insert with state='bogus' succeeded, want CHECK violation")
	}

	// FK: chunk referencing a missing document must be rejected.
	_, err = db.Writer().Exec(
		`INSERT INTO chunks (doc_id, ordinal, text, heading_path, byte_start, byte_end)
		 VALUES ('d_missing', 0, 'hello', '', 0, 5)`)
	if err == nil {
		t.Error("chunk insert with dangling doc_id succeeded, want FK violation")
	}
}

// Every domain.DocState constant must round-trip through the schema's CHECK
// constraint — the two lists are duplicated (Go and frozen migration SQL)
// and this is what turns drift into a test failure instead of a runtime
// CHECK violation.
func TestDocStatesAcceptedBySchema(t *testing.T) {
	db := openTestDB(t)
	states := []domain.DocState{
		domain.DocStateDiscovered, domain.DocStateConverted, domain.DocStateChunked,
		domain.DocStateEmbedded, domain.DocStateIndexed, domain.DocStateFailed,
		domain.DocStateDeleted,
	}
	for i, state := range states {
		_, err := db.Writer().Exec(
			`INSERT INTO documents (id, path, content_hash, size, mtime, state, created_at, updated_at)
			 VALUES (?, ?, 'h', 1, 0, ?, 0, 0)`,
			fmt.Sprintf("d_%d", i), fmt.Sprintf("/tmp/%d.md", i), string(state))
		if err != nil {
			t.Errorf("state %q rejected by schema: %v", state, err)
		}
	}
}

package sqlite

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// openTestDB opens a database in a fresh temp dir and closes it with the test.
func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "data", "bsearch.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db
}

func TestOpenAppliesProductionPragmas(t *testing.T) {
	db := openTestDB(t)

	checks := []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"},
		{"foreign_keys", "1"},
		{"busy_timeout", "5000"},
		{"synchronous", "1"}, // NORMAL
		{"temp_store", "2"},  // MEMORY
	}
	// Per-connection settings (foreign_keys, busy_timeout, synchronous,
	// temp_store) are not persistent, so both pools must carry them.
	pools := []struct {
		name string
		pool *sql.DB
	}{
		{"reader", db.Reader()},
		{"writer", db.Writer()},
	}
	for _, p := range pools {
		for _, c := range checks {
			var got string
			if err := p.pool.QueryRow("PRAGMA " + c.pragma).Scan(&got); err != nil {
				t.Fatalf("%s PRAGMA %s: %v", p.name, c.pragma, err)
			}
			if got != c.want {
				t.Errorf("%s PRAGMA %s = %q, want %q", p.name, c.pragma, got, c.want)
			}
		}
	}
}

func TestOpenLoadsSqliteVec(t *testing.T) {
	db := openTestDB(t)

	var version string
	if err := db.Reader().QueryRow("SELECT vec_version()").Scan(&version); err != nil {
		t.Fatalf("vec_version(): %v", err)
	}
	if version == "" {
		t.Error("vec_version() returned empty string")
	}
}

func TestOpenCreatesDirAndRestrictsPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "bsearch.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("db dir mode = %o, want 700", perm)
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat db file: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("db file mode = %o, want 600", perm)
	}
}

func TestReaderNotBlockedByOpenWriteTx(t *testing.T) {
	db := openTestDB(t)

	if _, err := db.Writer().Exec(
		`INSERT INTO meta (key, value) VALUES ('k', 'v')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Hold a write transaction open (IMMEDIATE: write lock taken now).
	tx, err := db.Writer().Begin()
	if err != nil {
		t.Fatalf("begin write tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES ('k2', 'v2')`); err != nil {
		t.Fatalf("write in tx: %v", err)
	}

	// WAL promise: a reader sees the last committed snapshot without waiting.
	done := make(chan error, 1)
	go func() {
		var v string
		done <- db.Reader().QueryRow(`SELECT value FROM meta WHERE key = 'k'`).Scan(&v)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("concurrent read: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader blocked behind open write transaction")
	}
}

func TestOpenHandlesSpecialCharPaths(t *testing.T) {
	// '?', '#', '%' in directory names must not derail file: URI parsing.
	dir := filepath.Join(t.TempDir(), "odd?dir #100%")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bsearch.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open with special chars: %v", err)
	}
	defer db.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db file not at requested path: %v", err)
	}
}

func TestSidecarFilesNotWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bsearch.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A write forces -wal and -shm into existence.
	if _, err := db.Writer().Exec("INSERT INTO meta (key, value) VALUES ('k', 'v')"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		st, err := os.Stat(f)
		if err != nil {
			t.Fatalf("stat %s: %v", f, err)
		}
		if perm := st.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s mode = %o, want no group/other access", filepath.Base(f), perm)
		}
	}
}

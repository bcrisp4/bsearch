// Package sqlite is the storage adapter: one SQLite database file holding
// catalog, chunks, summaries, and sqlite-vec vectors behind the domain
// storage ports (DESIGN.md: Storage, SQLite driver).
package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	sqlite3 "github.com/mattn/go-sqlite3"
)

// driverName is a mattn/go-sqlite3 driver registration that runs connPragmas
// on every new connection — a plain pool.Exec would only reach one pooled
// connection, silently leaving the rest untuned.
const driverName = "sqlite3_bsearch"

// connPragmas are settings with no DSN parameter in mattn/go-sqlite3.
var connPragmas = []string{
	"PRAGMA temp_store = MEMORY",
	// KNN scans should be RAM-bound (DESIGN.md: vector search levers).
	// 2147418112 = SQLITE_MAX_MMAP_SIZE, the compiled-in ceiling (~2 GiB);
	// anything higher is silently clamped to it.
	"PRAGMA mmap_size = 2147418112",
	"PRAGMA journal_size_limit = 67108864", // cap WAL growth at 64 MiB
}

func init() {
	// Register sqlite-vec on every future connection in this process
	// (sqlite3_auto_extension under the hood).
	sqlitevec.Auto()

	sql.Register(driverName, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			for _, pragma := range connPragmas {
				if _, err := conn.Exec(pragma, nil); err != nil {
					return fmt.Errorf("%s: %w", pragma, err)
				}
			}
			return nil
		},
	})
}

// DB holds the two connection pools over one database file: a single-connection
// writer (IMMEDIATE transactions) and a small reader pool. WAL keeps readers
// unblocked by the writer; a lone writer connection makes SQLite's
// single-writer model explicit instead of a lock-contention surprise.
type DB struct {
	writer *sql.DB
	reader *sql.DB
}

// dsnPragmas are applied to every connection via the DSN. Persistent settings
// (journal_mode) ride along harmlessly; the rest are per-connection and must
// be here. cache_size is per-connection heap (64 MiB × pool size) — sized
// with the small reader pool below in mind.
const dsnPragmas = "_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000&_foreign_keys=on&_cache_size=-64000"

// escapeURIPath percent-encodes the characters that break SQLite's file: URI
// parsing (mattn splits the DSN at the first '?'; SQLite decodes %XX).
func escapeURIPath(p string) string {
	return strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(p)
}

// Open opens (creating if needed) the database at path. The parent directory
// is created 0700 and the database file 0600 — the index concentrates
// document content (DESIGN.md: Security threat 1). The file is created (or
// tightened) before any connection opens, because SQLite creates the -wal
// and -shm sidecars copying the main file's permissions.
func Open(path string) (*DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	// MkdirAll is a no-op on an existing directory — tighten it explicitly.
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- directory: owner needs the execute bit
		return nil, fmt.Errorf("chmod db dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- caller-chosen db path
	if err != nil {
		return nil, fmt.Errorf("create db file: %w", err)
	}
	_ = f.Close()
	// O_CREATE doesn't touch the mode of a pre-existing file.
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("chmod db file: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?%s", escapeURIPath(path), dsnPragmas)

	writer, err := openPool(dsn+"&_txlock=immediate", 1, 1)
	if err != nil {
		return nil, fmt.Errorf("open writer: %w", err)
	}
	// Single user, occasional concurrent queries: a small pool bounds the
	// per-connection cache_size cost (idle-footprint constraint).
	reader, err := openPool(dsn, 4, 2)
	if err != nil {
		_ = writer.Close() // already failing; best-effort cleanup
		return nil, fmt.Errorf("open reader: %w", err)
	}
	db := &DB{writer: writer, reader: reader}

	// Sanity: the vec extension must be present or nothing downstream works.
	var vecVersion string
	if err := db.reader.QueryRow("SELECT vec_version()").Scan(&vecVersion); err != nil {
		_ = db.Close() // already failing; best-effort cleanup
		return nil, fmt.Errorf("sqlite-vec not loaded: %w", err)
	}

	if err := migrate(db.writer); err != nil {
		_ = db.Close() // already failing; best-effort cleanup
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// openPool opens one database/sql pool over the bsearch driver.
func openPool(dsn string, maxOpen, maxIdle int) (*sql.DB, error) {
	pool, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(maxOpen)
	pool.SetMaxIdleConns(maxIdle)
	// Force the first connection so DSN/hook failures surface at Open.
	if err := pool.Ping(); err != nil {
		_ = pool.Close() // already failing; best-effort cleanup
		return nil, err
	}
	return pool, nil
}

// Writer returns the single-connection write pool. Write transactions are
// IMMEDIATE (via _txlock) so busy_timeout governs lock waits cleanly.
func (d *DB) Writer() *sql.DB { return d.writer }

// Reader returns the read pool.
func (d *DB) Reader() *sql.DB { return d.reader }

// Close closes both pools.
func (d *DB) Close() error {
	return errors.Join(d.reader.Close(), d.writer.Close())
}

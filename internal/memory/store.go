// Package memory is the agent's durable state: a single SQLite database on
// the Fly volume, replicated to object storage by Litestream.
package memory

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registers "sqlite"; no cgo
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrations are applied in filename order. Never edit one that has shipped;
// add a new file.
var migrations = mustListMigrations()

func mustListMigrations() []string {
	names, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		panic(err)
	}
	sort.Strings(names)
	return names
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies pending
// migrations. WAL mode is required: Litestream only replicates WAL databases.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One writer avoids SQLITE_BUSY between our own connections.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name       TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	for _, name := range migrations {
		var exists int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&exists)
		if err != nil {
			return err
		}
		if exists > 0 {
			continue
		}
		body, err := migrationFS.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
			name, now()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

// RecordBoot logs a daemon start and returns the total number of boots,
// including this one.
func (s *Store) RecordBoot(ctx context.Context, version string) (int64, error) {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO boots (version, started_at) VALUES (?, ?)`, version, now()); err != nil {
		return 0, err
	}
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM boots`).Scan(&n)
	return n, err
}

// tsFormat is fixed-width so timestamps sort correctly as text. RFC3339Nano
// trims trailing zeros, so "…:05Z" would sort after "…:05.1Z".
const tsFormat = "2006-01-02T15:04:05.000000Z"

func ts(t time.Time) string { return t.UTC().Format(tsFormat) }

func parseTS(s string) (time.Time, error) { return time.Parse(tsFormat, s) }

func now() string { return ts(time.Now()) }

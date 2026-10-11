package memory

import (
	"context"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// Litestream only replicates databases in WAL mode.
func TestOpenUsesWAL(t *testing.T) {
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

// The boot counter is what the Phase 0 restore check reads: it must keep
// counting across process restarts against the same file.
func TestRecordBootPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	for want := int64(1); want <= 3; want++ {
		s := openTemp(t, path)
		got, err := s.RecordBoot(ctx, "test")
		if err != nil {
			t.Fatalf("RecordBoot: %v", err)
		}
		if got != want {
			t.Fatalf("boot %d: RecordBoot = %d", want, got)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	if err := s.migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(migrations) {
		t.Fatalf("schema_migrations rows = %d, want %d", n, len(migrations))
	}
}

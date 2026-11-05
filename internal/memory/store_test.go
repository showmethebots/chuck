package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"
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

// The deploy notice compares this boot's version with the one before it, so
// a restart of the same build stays quiet.
func TestPreviousBootVersion(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	steps := []struct{ boot, want string }{
		{"v1", ""}, // first boot ever: nothing before it
		{"v1", "v1"},
		{"v2", "v1"},
		{"v1", "v2"}, // rollback
	}
	for i, st := range steps {
		if _, err := s.RecordBoot(ctx, st.boot); err != nil {
			t.Fatalf("RecordBoot: %v", err)
		}
		got, err := s.PreviousBootVersion(ctx)
		if err != nil {
			t.Fatalf("PreviousBootVersion: %v", err)
		}
		if got != st.want {
			t.Errorf("boot %d (%s): PreviousBootVersion = %q, want %q", i+1, st.boot, got, st.want)
		}
	}
}

// Review focus: if timestamps did not sort as text, anything ordered or
// windowed by time would silently misorder.
func TestTimestampsSortAsText(t *testing.T) {
	base := time.Date(2026, 10, 13, 12, 0, 5, 0, time.UTC)
	times := []time.Time{base, base.Add(100 * time.Millisecond), base.Add(time.Second), base.Add(10 * time.Hour)}
	for i := 1; i < len(times); i++ {
		if a, b := ts(times[i-1]), ts(times[i]); a >= b {
			t.Errorf("ts %q does not sort before %q", a, b)
		}
	}
	est := time.FixedZone("EST", -5*3600)
	if got, want := ts(base.In(est)), ts(base); got != want {
		t.Errorf("ts in EST = %q, want %q", got, want)
	}
	in := base.Add(123456 * time.Microsecond)
	back, err := parseTS(ts(in))
	if err != nil || !back.Equal(in) {
		t.Errorf("round trip = %v, %v; want %v", back, err, in)
	}
}

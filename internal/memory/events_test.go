package memory

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestInsertEventDedupesDelivery(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	e := Event{Source: SourceGitHub, Kind: "push", DeliveryID: "d-1", Repo: "o/r", Payload: []byte(`{"a":1}`)}
	id, inserted, err := s.InsertEvent(ctx, e)
	if err != nil || !inserted || id == 0 {
		t.Fatalf("first insert: id=%d inserted=%v err=%v", id, inserted, err)
	}
	if _, inserted, err = s.InsertEvent(ctx, e); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if inserted {
		t.Fatal("redelivery inserted a second row")
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("events = %d, %v; want 1", n, err)
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

// Production already has 0001 applied with boot rows. Opening it must apply
// 0002 and keep them.
func TestOpenUpgradesPhase0Database(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	phase0, err := migrationFS.ReadFile("migrations/0001_boots.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`,
		string(phase0),
		`INSERT INTO schema_migrations VALUES ('migrations/0001_boots.sql', '2026-10-01T00:00:00Z')`,
		`INSERT INTO boots (version, started_at) VALUES ('v0', '2026-10-01T00:00:00Z')`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s := openTemp(t, path)
	defer func() { _ = s.Close() }()
	var boots, paused int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM boots`).Scan(&boots); err != nil || boots != 1 {
		t.Fatalf("boots = %d, %v; want 1", boots, err)
	}
	if err := s.db.QueryRow(`SELECT paused FROM control WHERE id = 1`).Scan(&paused); err != nil || paused != 0 {
		t.Fatalf("control.paused = %d, %v; want 0", paused, err)
	}
	var events int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&events); err != nil || events != 0 {
		t.Fatalf("events = %d, %v; want 0", events, err)
	}
}

// control.reason only ever holds "pause" or "kill" while paused, and NULL
// otherwise; the schema enforces that like every other enum-like column.
func TestControlReasonIsChecked(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	for _, ok := range []any{"pause", "kill", nil} {
		if _, err := s.db.ExecContext(ctx, `UPDATE control SET reason = ? WHERE id = 1`, ok); err != nil {
			t.Errorf("reason %v rejected: %v", ok, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE control SET reason = 'nap' WHERE id = 1`); err == nil {
		t.Error("reason 'nap' accepted; want a CHECK violation")
	}
}

package memory

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// insertEvent stores e with defaults filled in and fails the test unless it
// is a new row.
func insertEvent(t *testing.T, s *Store, e Event) int64 {
	t.Helper()
	if e.Source == "" {
		e.Source = SourceGitHub
	}
	if e.Kind == "" {
		e.Kind = "issues.opened"
	}
	if e.Payload == nil {
		e.Payload = []byte(`{}`)
	}
	id, inserted, err := s.InsertEvent(context.Background(), e)
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if !inserted {
		t.Fatal("InsertEvent: row not inserted")
	}
	return id
}

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
	if n, err := s.QueueDepth(ctx); err != nil || n != 1 {
		t.Fatalf("QueueDepth = %d, %v; want 1", n, err)
	}
}

func TestCronEventsAreNotQueued(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	insertEvent(t, s, Event{Source: SourceCron, Kind: "cron.daily"})
	insertEvent(t, s, Event{Source: SourceCron, Kind: "cron.daily"})
	if n, err := s.QueueDepth(ctx); err != nil || n != 0 {
		t.Fatalf("QueueDepth = %d, %v; want 0", n, err)
	}
	if e, err := s.NextUntriaged(ctx, time.Now().Add(time.Hour)); err != nil || e != nil {
		t.Fatalf("NextUntriaged = %+v, %v; want nil", e, err)
	}
}

func TestNextUntriagedOldestFirstAndHonoursRetryTime(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	first := insertEvent(t, s, Event{DeliveryID: "a", ReceivedAt: base})
	second := insertEvent(t, s, Event{DeliveryID: "b", ReceivedAt: base.Add(time.Second)})

	// Eligible from the moment it is received.
	got, err := s.NextUntriaged(ctx, base)
	if err != nil || got == nil || got.ID != first {
		t.Fatalf("NextUntriaged(base) = %+v, %v; want event %d", got, err, first)
	}
	if !got.ReceivedAt.Equal(base) {
		t.Errorf("ReceivedAt = %v, want %v", got.ReceivedAt, base)
	}

	// A first event waiting to retry does not block the second.
	if _, err := s.db.ExecContext(ctx, `UPDATE events SET next_attempt_at = ? WHERE id = ?`,
		ts(base.Add(time.Hour)), first); err != nil {
		t.Fatal(err)
	}
	got, err = s.NextUntriaged(ctx, base.Add(time.Minute))
	if err != nil || got == nil || got.ID != second {
		t.Fatalf("NextUntriaged(+1m) = %+v, %v; want event %d", got, err, second)
	}

	got, err = s.NextUntriaged(ctx, base.Add(time.Hour))
	if err != nil || got == nil || got.ID != first {
		t.Fatalf("NextUntriaged(+1h) = %+v, %v; want event %d", got, err, first)
	}
}

// Review focus: if timestamps did not sort as text, the queue and the daily
// spend window would silently misorder.
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

func TestLastEventAtIgnoresCron(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	if got, err := s.LastEventAt(ctx); err != nil || !got.IsZero() {
		t.Fatalf("empty LastEventAt = %v, %v; want zero", got, err)
	}
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	insertEvent(t, s, Event{DeliveryID: "a", ReceivedAt: base})
	insertEvent(t, s, Event{Source: SourceCron, Kind: "cron.daily", ReceivedAt: base.Add(time.Hour)})
	if got, err := s.LastEventAt(ctx); err != nil || !got.Equal(base) {
		t.Fatalf("LastEventAt = %v, %v; want %v", got, err, base)
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
	if n, err := s.QueueDepth(ctx); err != nil || n != 0 {
		t.Fatalf("QueueDepth = %d, %v", n, err)
	}
}

package memory

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Event sources.
const (
	SourceGitHub = "github"
	SourceCron   = "cron"
)

// Event is one raw input, stored verbatim: a GitHub delivery or a cron tick.
type Event struct {
	ID         int64
	Source     string
	Kind       string // "pull_request.opened", "push", "cron.daily"
	DeliveryID string // X-GitHub-Delivery; empty for cron
	Repo       string // owner/name; empty when the event has no repository
	Headers    []byte // JSON object of request headers, signature removed
	Payload    []byte
	ReceivedAt time.Time
	Attempts   int
}

// InsertEvent stores e and reports whether it was new. A DeliveryID seen
// before is a no-op, not an error: GitHub redelivers. Cron ticks are stored
// already triaged; they are a record, not work.
func (s *Store) InsertEvent(ctx context.Context, e Event) (id int64, inserted bool, err error) {
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = time.Now()
	}
	var delivery any
	if e.DeliveryID != "" {
		delivery = e.DeliveryID
	}
	headers := string(e.Headers)
	if headers == "" {
		headers = "{}"
	}
	at := ts(e.ReceivedAt)
	var triaged any
	if e.Source == SourceCron {
		triaged = at
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO events
		(source, kind, delivery_id, repo, headers, payload, received_at, next_attempt_at, triaged_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (delivery_id) DO NOTHING`,
		e.Source, e.Kind, delivery, e.Repo, headers, e.Payload, at, at, triaged)
	if err != nil {
		return 0, false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return 0, false, err
	}
	id, err = res.LastInsertId()
	return id, err == nil, err
}

const eventCols = `id, source, kind, COALESCE(delivery_id, ''), repo, headers, payload, received_at, attempts`

type scanner interface{ Scan(dest ...any) error }

func scanEvent(sc scanner) (Event, error) {
	var e Event
	var headers, received string
	if err := sc.Scan(&e.ID, &e.Source, &e.Kind, &e.DeliveryID, &e.Repo, &headers, &e.Payload, &received, &e.Attempts); err != nil {
		return Event{}, err
	}
	e.Headers = []byte(headers)
	t, err := parseTS(received)
	if err != nil {
		return Event{}, err
	}
	e.ReceivedAt = t
	return e, nil
}

// NextUntriaged returns the oldest event waiting for triage whose retry time
// has come, or nil when there is none.
func (s *Store) NextUntriaged(ctx context.Context, now time.Time) (*Event, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+eventCols+` FROM events
		WHERE triaged_at IS NULL AND next_attempt_at <= ?
		ORDER BY id LIMIT 1`, ts(now))
	e, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// QueueDepth counts events not yet triaged, including those waiting to retry.
func (s *Store) QueueDepth(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE triaged_at IS NULL`).Scan(&n)
	return n, err
}

// LastEventAt is when the latest GitHub event arrived, or the zero time.
func (s *Store) LastEventAt(ctx context.Context) (time.Time, error) {
	var last sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(received_at) FROM events WHERE source = ?`, SourceGitHub).Scan(&last); err != nil {
		return time.Time{}, err
	}
	if !last.Valid {
		return time.Time{}, nil
	}
	return parseTS(last.String)
}

package memory

import (
	"context"
	"time"
)

// SourceGitHub is the only event source left. The events table is dropped
// by the loop migration, which replaces it with webhooks.
const SourceGitHub = "github"

// Event is one GitHub delivery, stored verbatim.
type Event struct {
	ID         int64
	Source     string
	Kind       string // "pull_request.opened", "push"
	DeliveryID string // X-GitHub-Delivery
	Repo       string // owner/name; empty when the event has no repository
	Headers    []byte // JSON object of request headers, signature removed
	Payload    []byte
	ReceivedAt time.Time
}

// InsertEvent stores e and reports whether it was new. A DeliveryID seen
// before is a no-op, not an error: GitHub redelivers. Every event is stored
// as already handled: nothing reads the queue until the loop lands.
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
	res, err := s.db.ExecContext(ctx, `INSERT INTO events
		(source, kind, delivery_id, repo, headers, payload, received_at, next_attempt_at, triaged_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (delivery_id) DO NOTHING`,
		e.Source, e.Kind, delivery, e.Repo, headers, e.Payload, at, at, at)
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

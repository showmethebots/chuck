package memory

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Webhook is one GitHub delivery, stored verbatim.
type Webhook struct {
	ID          int64
	DeliveryID  string // X-GitHub-Delivery
	Event       string // X-GitHub-Event: "pull_request", "push"
	Action      string // payload action; "" when it has none
	Repo        string // owner/name; "" when the event has no repository
	SenderID    int64
	SenderLogin string
	Payload     []byte
	ReceivedAt  time.Time
	HandledAt   time.Time
	Result      string // what the loop did with it, or why it did nothing
}

// InsertWebhook stores w and reports whether it was new. A delivery id
// seen before is a no-op, not an error: GitHub redelivers. A non-zero
// HandledAt stores the delivery as already handled, with Result.
func (s *Store) InsertWebhook(ctx context.Context, w Webhook) (id int64, inserted bool, err error) {
	if w.ReceivedAt.IsZero() {
		w.ReceivedAt = time.Now()
	}
	err = s.db.QueryRowContext(ctx, `INSERT INTO webhooks
		(delivery_id, event, action, repo, sender_id, sender_login, payload, received_at, handled_at, result)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (delivery_id) DO NOTHING RETURNING id`,
		w.DeliveryID, w.Event, w.Action, w.Repo, w.SenderID, w.SenderLogin, w.Payload, ts(w.ReceivedAt),
		nullTS(w.HandledAt), w.Result).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// MarkWebhookHandled records what the loop did with a delivery.
func (s *Store) MarkWebhookHandled(ctx context.Context, id int64, result string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE webhooks SET handled_at = ?, result = ? WHERE id = ?`, ts(at), result, id)
	return oneRow(res, err)
}

// UnhandledWebhooks returns deliveries the loop has not handled, oldest
// first: on boot these are what a crash left behind.
func (s *Store) UnhandledWebhooks(ctx context.Context, limit int) ([]Webhook, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, delivery_id, event, action, repo, sender_id,
		sender_login, payload, received_at FROM webhooks WHERE handled_at IS NULL ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Webhook
	for rows.Next() {
		var w Webhook
		var at string
		if err := rows.Scan(&w.ID, &w.DeliveryID, &w.Event, &w.Action, &w.Repo, &w.SenderID,
			&w.SenderLogin, &w.Payload, &at); err != nil {
			return nil, err
		}
		if w.ReceivedAt, err = parseTS(at); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// DeleteWebhooksBefore applies webhook_retention: it deletes deliveries
// received before cutoff and returns how many went.
func (s *Store) DeleteWebhooksBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM webhooks WHERE received_at < ?`, ts(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// WebhookByID returns one delivery with how it was handled.
func (s *Store) WebhookByID(ctx context.Context, id int64) (Webhook, error) {
	var w Webhook
	var received string
	var handled sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id, delivery_id, event, action, repo, sender_id,
		sender_login, payload, received_at, handled_at, result FROM webhooks WHERE id = ?`, id).Scan(
		&w.ID, &w.DeliveryID, &w.Event, &w.Action, &w.Repo, &w.SenderID, &w.SenderLogin, &w.Payload,
		&received, &handled, &w.Result)
	if errors.Is(err, sql.ErrNoRows) {
		return w, ErrNotFound
	}
	if err != nil {
		return w, err
	}
	if w.ReceivedAt, err = parseTS(received); err != nil {
		return w, err
	}
	w.HandledAt, err = scanTS(handled)
	return w, err
}

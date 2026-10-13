package memory

import (
	"context"
	"time"
)

// Message is one Slack interaction or post: the audit trail of who asked
// chuck to do what, and whether the allowlist let them.
type Message struct {
	ID           int64
	Channel      string
	TS           string
	User         string
	Direction    string // "in" or "out"
	Kind         string // "command", "button", "post"
	Text         string
	Allowed      bool // the user held the role the interaction needs
	ChangedState bool
	CreatedAt    time.Time
}

func (s *Store) RecordMessage(ctx context.Context, m Message) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO messages
		(slack_channel, slack_ts, slack_user, direction, kind, text, allowed, changed_state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.Channel, m.TS, m.User, m.Direction, m.Kind, m.Text, m.Allowed, m.ChangedState, ts(m.CreatedAt))
	return err
}

// Messages returns every recorded message, oldest first.
func (s *Store) Messages(ctx context.Context) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, slack_channel, slack_ts, slack_user, direction, kind,
		text, allowed, changed_state, created_at FROM messages ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Message
	for rows.Next() {
		var m Message
		var created string
		if err := rows.Scan(&m.ID, &m.Channel, &m.TS, &m.User, &m.Direction, &m.Kind, &m.Text,
			&m.Allowed, &m.ChangedState, &created); err != nil {
			return nil, err
		}
		if m.CreatedAt, err = parseTS(created); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CreateApproval opens the approval request for a decision. Calling it again
// for the same decision is a no-op.
func (s *Store) CreateApproval(ctx context.Context, decisionID int64, request string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO approvals (decision_id, request) VALUES (?, ?)
		ON CONFLICT (decision_id) DO NOTHING`, decisionID, request)
	return err
}

package memory

import (
	"context"
	"time"
)

// Control is the pause switch. It lives in SQLite so a deploy or restart
// never silently resumes a paused chuck.
type Control struct {
	Paused    bool
	Reason    string // "pause" or "kill" while paused
	By        string // Slack user ID of whoever last changed it
	ChangedAt time.Time
}

func (s *Store) Control(ctx context.Context) (Control, error) {
	var c Control
	var changed string
	err := s.db.QueryRowContext(ctx, `SELECT paused, COALESCE(reason, ''), COALESCE(by_slack_user, ''),
		COALESCE(changed_at, '') FROM control WHERE id = 1`).Scan(&c.Paused, &c.Reason, &c.By, &changed)
	if err != nil || changed == "" {
		return c, err
	}
	c.ChangedAt, err = parseTS(changed)
	return c, err
}

func (s *Store) SetControl(ctx context.Context, c Control) error {
	reason := c.Reason
	if !c.Paused {
		reason = ""
	}
	_, err := s.db.ExecContext(ctx, `UPDATE control SET paused = ?, reason = NULLIF(?, ''),
		by_slack_user = NULLIF(?, ''), changed_at = ? WHERE id = 1`, c.Paused, reason, c.By, ts(c.ChangedAt))
	return err
}

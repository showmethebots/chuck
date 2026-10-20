package memory

import (
	"context"
	"time"
)

// Summary is the daily report: activity in [Since, Until), plus all-time
// grade totals that track the Phase 1 exit criterion.
type Summary struct {
	Since, Until   time.Time
	Outcomes       map[string]int // decisions by outcome, in window
	Categories     map[string]int // ok decisions by category, in window
	SpendUSD       float64        // in window
	GradedInWindow int            // grade clicks in window
	Graded         int            // decisions with a grade, all time
	Up             int            // of those, latest grade 👍
	AskFirstDown   int            // ask_first decisions whose latest grade is 👎
	PendingGrades  int            // posted for grading, never graded
	Rejected       int            // inbound Slack interactions refused by the allowlist, all time
	Violations     int            // refused interactions that changed state anyway; must stay 0
}

func (s *Store) countBy(ctx context.Context, query string, args ...any) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

func (s *Store) Summary(ctx context.Context, since, until time.Time) (Summary, error) {
	sum := Summary{Since: since, Until: until}
	from, to := ts(since), ts(until)
	var err error
	if sum.Outcomes, err = s.countBy(ctx, `SELECT outcome, COUNT(*) FROM decisions
		WHERE created_at >= ? AND created_at < ? GROUP BY outcome`, from, to); err != nil {
		return sum, err
	}
	if sum.Categories, err = s.countBy(ctx, `SELECT category, COUNT(*) FROM decisions
		WHERE created_at >= ? AND created_at < ? AND outcome = ? GROUP BY category`, from, to, OutcomeOK); err != nil {
		return sum, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd), 0) FROM decisions
		WHERE created_at >= ? AND created_at < ?`, from, to).Scan(&sum.SpendUSD); err != nil {
		return sum, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM grades
		WHERE graded_at >= ? AND graded_at < ?`, from, to).Scan(&sum.GradedInWindow); err != nil {
		return sum, err
	}
	if err := s.db.QueryRowContext(ctx, `
		WITH latest AS (
			SELECT g.decision_id, g.grade FROM grades g
			WHERE g.id = (SELECT MAX(id) FROM grades WHERE decision_id = g.decision_id)
		)
		SELECT COUNT(*),
		       COALESCE(SUM(latest.grade = 'up'), 0),
		       COALESCE(SUM(latest.grade = 'down' AND d.permission = 'ask_first'), 0)
		FROM latest JOIN decisions d ON d.id = latest.decision_id`).Scan(&sum.Graded, &sum.Up, &sum.AskFirstDown); err != nil {
		return sum, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM decisions d
		WHERE d.post_kind IS NOT NULL AND NOT EXISTS (SELECT 1 FROM grades g WHERE g.decision_id = d.id)`).Scan(&sum.PendingGrades); err != nil {
		return sum, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(changed_state), 0) FROM messages
		WHERE direction = 'in' AND allowed = 0`).Scan(&sum.Rejected, &sum.Violations)
	return sum, err
}

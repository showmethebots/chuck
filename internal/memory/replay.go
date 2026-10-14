package memory

import (
	"context"
	"time"
)

// Graded is a graded GitHub event with its decision and latest grade.
type Graded struct {
	Event    Event
	Decision Decision
	Grade    string
}

// GradedEvents returns graded decisions on GitHub events made at or after
// since, oldest first, each with its raw event.
func (s *Store) GradedEvents(ctx context.Context, since time.Time) ([]Graded, error) {
	views, err := s.views(ctx, `WHERE d.created_at >= ? AND e.source = ?
		AND EXISTS (SELECT 1 FROM grades g WHERE g.decision_id = d.id) ORDER BY d.id`, ts(since), SourceGitHub)
	if err != nil {
		return nil, err
	}
	out := make([]Graded, 0, len(views))
	for _, v := range views {
		e, err := scanEvent(s.db.QueryRowContext(ctx, `SELECT `+eventCols+` FROM events WHERE id = ?`, v.EventID))
		if err != nil {
			return nil, err
		}
		out = append(out, Graded{Event: e, Decision: v.Decision, Grade: v.Grade})
	}
	return out, nil
}

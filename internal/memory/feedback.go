package memory

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// IgnoredFeedback is one comment the trusted-text filter kept out of a
// run's prompt.
type IgnoredFeedback struct {
	ID          int64
	RunID       int64
	AuthorID    int64
	AuthorLogin string
	URL         string
	Reason      string
	CreatedAt   time.Time
}

// InsertIgnoredFeedback records what a run's prompt left out, in one
// transaction.
func (s *Store) InsertIgnoredFeedback(ctx context.Context, runID int64, fs []IgnoredFeedback, at time.Time) error {
	if len(fs) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, f := range fs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO ignored_feedback
				(run_id, author_id, author_login, url, reason, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
				runID, f.AuthorID, f.AuthorLogin, f.URL, f.Reason, ts(at)); err != nil {
				return err
			}
		}
		return nil
	})
}

// IgnoredFeedbackForItem lists what every run on an item left out, oldest
// first.
func (s *Store) IgnoredFeedbackForItem(ctx context.Context, itemID int64) ([]IgnoredFeedback, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT f.id, f.run_id, f.author_id, f.author_login, f.url,
		f.reason, f.created_at FROM ignored_feedback f JOIN runs r ON r.id = f.run_id
		WHERE r.item_id = ? ORDER BY f.id`, itemID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []IgnoredFeedback
	for rows.Next() {
		var f IgnoredFeedback
		var at string
		if err := rows.Scan(&f.ID, &f.RunID, &f.AuthorID, &f.AuthorLogin, &f.URL, &f.Reason, &at); err != nil {
			return nil, err
		}
		if f.CreatedAt, err = parseTS(at); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// InsertReviewFindings records the sha256 of a review run's findings
// comment, as chuck read it when it accepted the review verdict.
func (s *Store) InsertReviewFindings(ctx context.Context, runID, itemID int64, url, bodySHA256 string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO review_findings
		(run_id, item_id, comment_url, body_sha256, created_at) VALUES (?, ?, ?, ?, ?)`,
		runID, itemID, url, bodySHA256, ts(at))
	return err
}

// ReviewFindingsSHA is the newest recorded hash of the findings comment at
// url on an item, or ErrNotFound.
func (s *Store) ReviewFindingsSHA(ctx context.Context, itemID int64, url string) (string, error) {
	var h string
	err := s.db.QueryRowContext(ctx, `SELECT body_sha256 FROM review_findings
		WHERE item_id = ? AND comment_url = ? ORDER BY id DESC LIMIT 1`, itemID, url).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return h, err
}

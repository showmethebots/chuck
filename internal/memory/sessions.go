package memory

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Session is a dashboard login. The cookie carries a random token; only
// its hash is stored, so a copy of the database holds no usable session.
type Session struct {
	UserID    int64
	Login     string
	CSRF      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// CreateSession stores a session for token, expiring ttl from at.
func (s *Store) CreateSession(ctx context.Context, token string, sess Session, at time.Time, ttl time.Duration) (Session, error) {
	sess.CreatedAt, sess.ExpiresAt = at, at.Add(ttl)
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, user_id, login, csrf, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`, HashToken(token), sess.UserID, sess.Login, sess.CSRF, ts(at), ts(sess.ExpiresAt))
	return sess, err
}

// TouchSession returns the live session for token and slides its expiry to
// ttl from at. A revoked, expired or unknown token is ErrNotFound.
func (s *Store) TouchSession(ctx context.Context, token string, at time.Time, ttl time.Duration) (Session, error) {
	var sess Session
	var created, expires string
	err := s.db.QueryRowContext(ctx, `UPDATE sessions SET expires_at = ?
		WHERE id = ? AND revoked_at IS NULL AND expires_at > ?
		RETURNING user_id, login, csrf, created_at, expires_at`,
		ts(at.Add(ttl)), HashToken(token), ts(at)).Scan(&sess.UserID, &sess.Login, &sess.CSRF, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return sess, ErrNotFound
	}
	if err != nil {
		return sess, err
	}
	if sess.CreatedAt, err = parseTS(created); err != nil {
		return sess, err
	}
	sess.ExpiresAt, err = parseTS(expires)
	return sess, err
}

// RevokeSession ends a session (logout). Revoking an unknown or already
// revoked token is not an error.
func (s *Store) RevokeSession(ctx context.Context, token string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, ts(at), HashToken(token))
	return err
}

// DeleteExpiredSessions removes sessions that expired or were revoked
// before cutoff.
func (s *Store) DeleteExpiredSessions(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at < ? OR revoked_at < ?`, ts(cutoff), ts(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

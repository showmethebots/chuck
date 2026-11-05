package memory

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SettingAPIKeyExhaustedUntil holds the time, in the store's timestamp
// format, until which runs use the OAuth token because the API key is out
// of credit.
const SettingAPIKeyExhaustedUntil = "api_key_exhausted_until"

// Setting returns a setting's value, or ErrNotFound.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetSetting writes a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, ts(at))
	return err
}

// DeleteSetting removes a setting; a missing one is not an error.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	return err
}

// APIKeyExhaustedUntil returns when the API key is next usable, or the
// zero time if it is not marked exhausted.
func (s *Store) APIKeyExhaustedUntil(ctx context.Context) (time.Time, error) {
	v, err := s.Setting(ctx, SettingAPIKeyExhaustedUntil)
	if errors.Is(err, ErrNotFound) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return parseTS(v)
}

// SetAPIKeyExhaustedUntil marks the API key exhausted until t.
func (s *Store) SetAPIKeyExhaustedUntil(ctx context.Context, t, at time.Time) error {
	return s.SetSetting(ctx, SettingAPIKeyExhaustedUntil, ts(t), at)
}

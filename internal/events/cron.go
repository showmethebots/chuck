package events

import (
	"context"
	"fmt"
	"time"
)

// Daily is a once-a-day schedule in UTC.
type Daily struct{ hour, minute int }

// ParseDaily reads "HH:MM" in UTC.
func ParseDaily(at string) (Daily, error) {
	t, err := time.Parse("15:04", at)
	if err != nil {
		return Daily{}, fmt.Errorf("daily time %q: want HH:MM in UTC", at)
	}
	return Daily{hour: t.Hour(), minute: t.Minute()}, nil
}

// Next is the first occurrence strictly after t.
func (d Daily) Next(t time.Time) time.Time {
	u := t.UTC()
	next := time.Date(u.Year(), u.Month(), u.Day(), d.hour, d.minute, 0, 0, time.UTC)
	if !next.After(u) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

// RunDaily calls f at each occurrence of d until ctx ends. A run missed
// while the daemon was down is skipped, not caught up.
func RunDaily(ctx context.Context, d Daily, now func() time.Time, f func(ctx context.Context, at time.Time)) {
	for {
		next := d.Next(now())
		timer := time.NewTimer(next.Sub(now()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			f(ctx, next)
		}
	}
}

package triage

import (
	"errors"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go"
)

// Systemic reports whether an error from Decide is about the classifier
// rather than about the event being classified. A wrong API key (401, 403),
// a rate limit or conflict (408, 409, 429), an Anthropic outage (5xx, 529), a
// network failure, or a cancelled or expired context would fail every queued
// event the same way. Charging those to the event would burn the whole
// backlog through its retries in minutes and record it all triage_failed, so
// the triage loop backs off instead and leaves the event untouched.
//
// Any other API status (400, 404, 413, 422 ...) means the API rejected the
// request built for this event; retrying it later won't help, so it counts
// against the event's attempts.
func Systemic(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return true
	}
	switch s := apiErr.StatusCode; s {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout,
		http.StatusConflict, http.StatusTooManyRequests:
		return true
	default:
		return s >= http.StatusInternalServerError
	}
}

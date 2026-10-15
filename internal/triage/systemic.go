package triage

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared"
)

// spendLimitPrefix starts the message of the 400 the API returns once an
// organization or workspace spend limit set in the Console is reached
// ("You have reached your specified API usage limits", or "... specified
// workspace API usage limits").
const spendLimitPrefix = "You have reached your specified"

// Systemic reports whether an error from Decide is about the classifier
// rather than about the event being classified. A wrong API key (401, 403),
// a billing problem (402) or a Console spend limit (a 400 carrying the
// spend-limit message), a rate limit, tier spend cap or conflict (408, 409,
// 429), an Anthropic outage (5xx, 529), a network failure, or a cancelled or
// expired context would fail every queued event the same way. Charging those
// to the event would burn the whole backlog through its retries in minutes
// and record it all triage_failed, so the triage loop backs off instead and
// leaves the event untouched.
//
// Any other API status (other 400s, 404, 413, 422 ...) means the API
// rejected the request built for this event; retrying it later won't help,
// so it counts against the event's attempts.
func Systemic(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return true
	}
	switch s := apiErr.StatusCode; s {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden,
		http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return true
	case http.StatusBadRequest:
		return spendLimitReached(apiErr)
	default:
		return s >= http.StatusInternalServerError
	}
}

// spendLimitReached reports whether a 400 is the API refusing because a
// Console spend limit was reached, not because the request was bad.
func spendLimitReached(apiErr *anthropic.Error) bool {
	if apiErr.Type() != shared.ErrorTypeInvalidRequestError {
		return false
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(apiErr.RawJSON()), &body) != nil {
		return false
	}
	return strings.HasPrefix(body.Error.Message, spendLimitPrefix)
}

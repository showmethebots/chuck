package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
)

// decideErr runs Decide with a real Haiku client against a server that
// answers every request with status and an Anthropic-shaped error body of
// errType and msg, so the error is the one the SDK really produces, wrapped
// as Decide wraps it.
func decideErr(t *testing.T, status int, errType, msg string) error {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": msg},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	h := NewHaiku(testMenu(), option.WithBaseURL(srv.URL), option.WithAPIKey("x"), option.WithMaxRetries(0))
	_, err = Decide(context.Background(), "issues.opened", "orionsbelt-dev/demos", fixture(t, "issues_opened.json"), h, testMenu())
	if err == nil {
		t.Fatalf("status %d: Decide returned no error", status)
	}
	return err
}

func TestSystemic(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   bool
	}{
		{400, false}, {404, false}, {413, false}, {422, false},
		{401, true}, {403, true}, {408, true}, {409, true}, {429, true},
		{500, true}, {503, true}, {529, true},
	} {
		if got := Systemic(decideErr(t, tc.status, "api_error", fmt.Sprintf("status %d", tc.status))); got != tc.want {
			t.Errorf("Systemic(HTTP %d) = %v, want %v", tc.status, got, tc.want)
		}
	}

	// A Console spend limit answers 400 invalid_request_error, which must
	// pause triage rather than fail each event; any other 400 is the
	// event's own fault. Messages are the ones the API documents.
	for _, tc := range []struct {
		name    string
		status  int
		errType string
		msg     string
		want    bool
	}{
		{"org spend limit", 400, "invalid_request_error",
			"You have reached your specified API usage limits. You will regain access on 2026-11-01 at 00:00 UTC.", true},
		{"workspace spend limit", 400, "invalid_request_error",
			"You have reached your specified workspace API usage limits. You will regain access on 2026-11-01 at 00:00 UTC.", true},
		{"bad request", 400, "invalid_request_error", "max_tokens: Field required", false},
		{"spend-limit text, other type", 400, "api_error",
			"You have reached your specified API usage limits.", false},
		{"billing", 402, "billing_error", "Your credit balance is too low.", true},
	} {
		if got := Systemic(decideErr(t, tc.status, tc.errType, tc.msg)); got != tc.want {
			t.Errorf("Systemic(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"network", errors.New("dial tcp: refused"), true},
		{"canceled", fmt.Errorf("classify: %w", context.Canceled), true},
		{"deadline", context.DeadlineExceeded, true},
	} {
		if got := Systemic(tc.err); got != tc.want {
			t.Errorf("Systemic(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

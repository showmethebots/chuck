package triage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
)

// decideErr runs Decide with a real Haiku client against a server that
// answers every request with status and an Anthropic-shaped error body, so
// the error is the one the SDK really produces, wrapped as Decide wraps it.
func decideErr(t *testing.T, status int) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, fmt.Sprintf(`{"type":"error","error":{"type":"api_error","message":"status %d"}}`, status))
	}))
	t.Cleanup(srv.Close)
	h := NewHaiku(testPolicy(), option.WithBaseURL(srv.URL), option.WithAPIKey("x"), option.WithMaxRetries(0))
	_, err := Decide(context.Background(), "issues.opened", "orionsbelt-dev/demos", fixture(t, "issues_opened.json"), h, testPolicy())
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
		if got := Systemic(decideErr(t, tc.status)); got != tc.want {
			t.Errorf("Systemic(HTTP %d) = %v, want %v", tc.status, got, tc.want)
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

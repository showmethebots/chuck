package web

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newServer(out *syncBuffer) *Server {
	return &Server{
		Log:       slog.New(slog.NewJSONHandler(out, nil)),
		Interval:  10 * time.Millisecond,
		Version:   "test",
		BootCount: 7,
	}
}

func TestRunLogsHeartbeatsUntilCancelled(t *testing.T) {
	var out syncBuffer
	d := newServer(&out)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, ln) }()

	// Cancel once three beats are logged rather than racing a timer, so a
	// slow CI runner cannot make this flaky.
	deadline := time.After(5 * time.Second)
	for strings.Count(out.String(), `"msg":"heartbeat"`) < 3 {
		select {
		case <-deadline:
			t.Fatalf("fewer than 3 heartbeats in 5s:\n%s", out.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v, want nil on cancel", err)
	}

	var beats int
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if rec["msg"] == "heartbeat" {
			beats++
			if rec["boot_count"] != float64(7) {
				t.Errorf("heartbeat boot_count = %v, want 7", rec["boot_count"])
			}
		}
	}
	if beats < 3 {
		t.Fatalf("got %d heartbeats, want >= 3\n%s", beats, out.String())
	}
}

func TestRunServesHealthz(t *testing.T) {
	var out syncBuffer
	d := newServer(&out)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestHealthzBody(t *testing.T) {
	var out syncBuffer
	rec := httptest.NewRecorder()
	newServer(&out).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))

	var body struct {
		Status    string `json:"status"`
		Version   string `json:"version"`
		BootCount int64  `json:"boot_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if body.Status != "ok" || body.Version != "test" || body.BootCount != 7 {
		t.Fatalf("body = %+v", body)
	}
}

func TestWebhookRouteOnlyWhenConfigured(t *testing.T) {
	d := newServer(&syncBuffer{})
	rec := httptest.NewRecorder()
	d.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/webhooks/github", strings.NewReader("{}")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unconfigured webhook: status %d, want 404", rec.Code)
	}

	d.Webhook = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted) })
	rec = httptest.NewRecorder()
	d.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/webhooks/github", strings.NewReader("{}")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("configured webhook: status %d, want 202", rec.Code)
	}
}

func TestHealthzReportsIntegrations(t *testing.T) {
	d := newServer(&syncBuffer{})
	d.Integrations = []string{"github_webhook"}
	rec := httptest.NewRecorder()
	d.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))

	var body struct {
		Integrations []string `json:"integrations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Integrations) != 1 || body.Integrations[0] != "github_webhook" {
		t.Errorf("integrations = %v", body.Integrations)
	}
}

func TestHealthzListsNoIntegrationsAsEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	newServer(&syncBuffer{}).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if !strings.Contains(rec.Body.String(), `"integrations":[]`) {
		t.Fatalf("body = %s, want integrations: []", rec.Body.String())
	}
}

func TestRunStopsBackgroundJobs(t *testing.T) {
	d := newServer(&syncBuffer{})
	started := make(chan struct{})
	stopped := make(chan struct{})
	d.Background = []func(context.Context){func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(stopped)
	}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, ln) }()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("Run returned before the background job stopped")
	}
}

// A slow client must not hold a request (and its webhook body buffer) open
// indefinitely: the whole request, body included, has a deadline.
func TestServerBoundsRequestReadTime(t *testing.T) {
	srv := newServer(&syncBuffer{}).server()
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout=%v ReadTimeout=%v; both must be set", srv.ReadHeaderTimeout, srv.ReadTimeout)
	}
}

package daemon

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

func newDaemon(out *syncBuffer) *Daemon {
	return &Daemon{
		Log:       slog.New(slog.NewJSONHandler(out, nil)),
		Interval:  10 * time.Millisecond,
		Version:   "test",
		BootCount: 7,
	}
}

func TestRunLogsHeartbeatsUntilCancelled(t *testing.T) {
	var out syncBuffer
	d := newDaemon(&out)
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
	d := newDaemon(&out)
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
	newDaemon(&out).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))

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

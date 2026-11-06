package github

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestAllowlistBuiltinCoversV4AndV6(t *testing.T) {
	a := NewAllowlist()
	for ip, want := range map[string]bool{
		"140.82.112.5":        true,
		"::ffff:140.82.112.5": true,
		"2606:50c0::1":        true,
		"8.8.8.8":             false,
		"2001:db8::1":         false,
		"127.0.0.1":           false,
	} {
		if got := a.Contains(netip.MustParseAddr(ip)); got != want {
			t.Errorf("Contains(%s) = %v, want %v", ip, got, want)
		}
	}
	if !a.Builtin() {
		t.Error("new list is not marked built-in")
	}
}

func metaServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAllowlistRefreshReplacesTheList(t *testing.T) {
	srv := metaServer(t, 200, `{"hooks":["203.0.113.0/24","2001:db8::/32"],"web":["1.1.1.1/32"]}`)
	a := NewAllowlist()
	if _, err := a.Refresh(context.Background(), srv.Client(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if a.Builtin() || !a.Contains(netip.MustParseAddr("203.0.113.9")) || !a.Contains(netip.MustParseAddr("2001:db8::5")) {
		t.Error("refreshed ranges not used")
	}
	if a.Contains(netip.MustParseAddr("140.82.112.5")) || a.Contains(netip.MustParseAddr("1.1.1.1")) {
		t.Error("old or non-hooks ranges still allowed")
	}
}

func TestAllowlistRefreshReportsTheDiff(t *testing.T) {
	a := NewAllowlist()
	srv := metaServer(t, 200, `{"hooks":["192.30.252.0/22","185.199.108.0/22","140.82.112.0/20","143.55.64.0/20",`+
		`"2a0a:a440::/29","2606:50c0::/32","203.0.113.0/24"]}`)
	d, err := a.Refresh(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Changed() || len(d.Added) != 1 || d.Added[0] != "203.0.113.0/24" || len(d.Removed) != 0 {
		t.Errorf("diff = %+v, want one range added", d)
	}
	if d, _ := a.Refresh(context.Background(), srv.Client(), srv.URL); d.Changed() {
		t.Errorf("second refresh diff = %+v, want unchanged", d)
	}
	gone := metaServer(t, 200, `{"hooks":["203.0.113.0/24"]}`)
	if d, _ := a.Refresh(context.Background(), gone.Client(), gone.URL); len(d.Removed) != 6 || len(d.Added) != 0 {
		t.Errorf("diff = %+v, want six removed", d)
	}
}

// hooks.txt parses, and every entry is a range.
func TestBuiltinHooksParse(t *testing.T) {
	if len(builtinHooks) == 0 {
		t.Fatal("no built-in ranges")
	}
	for _, s := range builtinHooks {
		if _, err := netip.ParsePrefix(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
}

func TestAllowlistFailedRefreshKeepsTheList(t *testing.T) {
	good := metaServer(t, 200, `{"hooks":["203.0.113.0/24"]}`)
	a := NewAllowlist()
	if _, err := a.Refresh(context.Background(), good.Client(), good.URL); err != nil {
		t.Fatal(err)
	}
	for name, srv := range map[string]*httptest.Server{
		"server error": metaServer(t, 500, `{}`),
		"not JSON":     metaServer(t, 200, `<html>`),
		"no hooks":     metaServer(t, 200, `{"hooks":[]}`),
		"bad range":    metaServer(t, 200, `{"hooks":["203.0.113.0/24","nonsense"]}`),
		"too wide":     metaServer(t, 200, `{"hooks":["0.0.0.0/0"]}`),
		"too wide v6":  metaServer(t, 200, `{"hooks":["::/0"]}`),
	} {
		if _, err := a.Refresh(context.Background(), srv.Client(), srv.URL); err == nil {
			t.Errorf("%s: refresh succeeded", name)
		}
		if !a.Contains(netip.MustParseAddr("203.0.113.9")) || a.Contains(netip.MustParseAddr("8.8.8.8")) {
			t.Errorf("%s: list changed after a failed refresh", name)
		}
	}
	// A failed first fetch keeps the built-in list.
	b := NewAllowlist()
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.Run(ctx, slog.New(slog.NewTextHandler(&logs, nil)), http.DefaultClient, metaServer(t, 503, "").URL, 1)
	if !b.Builtin() || !b.Contains(netip.MustParseAddr("140.82.112.5")) {
		t.Error("failed first fetch dropped the built-in list")
	}
	if !strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("failed refresh not logged at Error: %s", logs.String())
	}
}

func TestWebhookChecksTheSenderBeforeReading(t *testing.T) {
	store := &fakeStore{}
	body := fixture(t, "issues_opened.json")
	sig := sign([]byte("s3cret"), body)
	send := func(h *Receiver, ips ...string) (int, *countingBody) {
		cb := &countingBody{r: bytes.NewReader(body)}
		r := httptest.NewRequest(http.MethodPost, "/webhooks/github", cb)
		r.Header.Set("X-GitHub-Event", "issues")
		r.Header.Set("X-GitHub-Delivery", "d-"+strings.Join(ips, "-"))
		r.Header.Set("X-Hub-Signature-256", sig)
		for _, ip := range ips {
			r.Header.Add("Fly-Client-IP", ip)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, cb
	}
	h := newHook(store, "s3cret")
	for name, ips := range map[string][]string{
		"outside the ranges": {"8.8.8.8"},
		"missing header":     nil,
		"not an IP":          {"github.com"},
		"two headers":        {"140.82.112.5", "8.8.8.8"},
	} {
		code, cb := send(h, ips...)
		if code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, code)
		}
		if cb.n != 0 {
			t.Errorf("%s: %d body bytes read before rejecting", name, cb.n)
		}
	}
	if code, _ := send(h, "2606:50c0::7"); code != http.StatusAccepted {
		t.Errorf("IPv6 inside the ranges: status %d", code)
	}
	// No allowlist and no override rejects everything.
	if code, _ := send(&Receiver{Secret: []byte("s3cret"), Store: store, Log: h.Log}, "140.82.112.5"); code != http.StatusForbidden {
		t.Errorf("nil allowlist: status %d", code)
	}
	// The dev override skips the range check, but not the signature.
	dev := &Receiver{Secret: []byte("s3cret"), Store: store, Log: h.Log, AllowAnyIP: true}
	if code, _ := send(dev, "8.8.8.8"); code != http.StatusAccepted {
		t.Errorf("dev override: status %d", code)
	}
	r := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	r.Header.Set("X-GitHub-Event", "issues")
	r.Header.Set("X-GitHub-Delivery", "unsigned")
	w := httptest.NewRecorder()
	dev.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("dev override, unsigned: status %d", w.Code)
	}
}

type countingBody struct {
	r io.Reader
	n int
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

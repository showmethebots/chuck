package events

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orionsbelt-dev/chuck/internal/memory"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "github", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

type fakeStore struct {
	events []memory.Event
	seen   map[string]bool
	err    error
}

func (f *fakeStore) InsertEvent(_ context.Context, e memory.Event) (int64, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if f.seen[e.DeliveryID] {
		return 0, false, nil
	}
	f.seen[e.DeliveryID] = true
	f.events = append(f.events, e)
	return int64(len(f.events)), true, nil
}

var fixedNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func newHook(store *fakeStore, secret string) *GitHub {
	return &GitHub{
		Secret: []byte(secret),
		Store:  store,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return fixedNow },
	}
}

func deliver(h http.Handler, event, delivery, sig string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	if event != "" {
		r.Header.Set("X-GitHub-Event", event)
	}
	if delivery != "" {
		r.Header.Set("X-GitHub-Delivery", delivery)
	}
	if sig != "" {
		r.Header.Set("X-Hub-Signature-256", sig)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestWebhookStoresSignedDelivery(t *testing.T) {
	store := &fakeStore{}
	body := fixture(t, "issues_opened.json")
	w := deliver(newHook(store, "s3cret"), "issues", "d-1", sign([]byte("s3cret"), body), body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %q", w.Code, w.Body.String())
	}
	if len(store.events) != 1 {
		t.Fatalf("stored %d events, want 1", len(store.events))
	}
	e := store.events[0]
	if e.Source != memory.SourceGitHub || e.Kind != "issues.opened" || e.Repo != "orionsbelt-dev/demos" || e.DeliveryID != "d-1" {
		t.Errorf("event = %+v", e)
	}
	if !bytes.Equal(e.Payload, body) {
		t.Error("payload not stored verbatim")
	}
	if !e.ReceivedAt.Equal(fixedNow) {
		t.Errorf("ReceivedAt = %v", e.ReceivedAt)
	}
	var headers map[string][]string
	if err := json.Unmarshal(e.Headers, &headers); err != nil {
		t.Fatalf("headers not JSON: %v", err)
	}
	if _, ok := headers["X-Hub-Signature-256"]; ok {
		t.Error("signature header was stored")
	}
	if headers["X-Github-Delivery"][0] != "d-1" {
		t.Errorf("delivery header missing: %v", headers)
	}
}

func TestWebhookKinds(t *testing.T) {
	cases := []struct{ event, file, kind, repo string }{
		{"push", "push.json", "push", "orionsbelt-dev/demos"},
		{"ping", "ping.json", "ping", ""},
		{"workflow_run", "workflow_run_completed.json", "workflow_run.completed", "orionsbelt-dev/demos"},
	}
	for _, c := range cases {
		store := &fakeStore{}
		body := fixture(t, c.file)
		w := deliver(newHook(store, "k"), c.event, "d-"+c.event, sign([]byte("k"), body), body)
		if w.Code != http.StatusAccepted || len(store.events) != 1 {
			t.Fatalf("%s: status %d, %d events", c.event, w.Code, len(store.events))
		}
		if got := store.events[0]; got.Kind != c.kind || got.Repo != c.repo {
			t.Errorf("%s: kind=%q repo=%q, want %q %q", c.event, got.Kind, got.Repo, c.kind, c.repo)
		}
	}
}

func TestWebhookRejectsBadSignatures(t *testing.T) {
	body := fixture(t, "issues_opened.json")
	cases := map[string]string{
		"missing":      "",
		"wrong secret": sign([]byte("other"), body),
		"not hex":      "sha256=zzzz",
		"sha1 prefix":  "sha1=" + sign([]byte("s3cret"), body)[len("sha256="):],
	}
	for name, sig := range cases {
		store := &fakeStore{}
		w := deliver(newHook(store, "s3cret"), "issues", "d-1", sig, body)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, w.Code)
		}
		if len(store.events) != 0 {
			t.Errorf("%s: stored an unsigned delivery", name)
		}
	}
}

// Review focus: an unset secret must lock the endpoint, not open it.
func TestWebhookRejectsEverythingWithEmptySecret(t *testing.T) {
	store := &fakeStore{}
	body := fixture(t, "issues_opened.json")
	w := deliver(newHook(store, ""), "issues", "d-1", sign(nil, body), body)
	if w.Code != http.StatusUnauthorized || len(store.events) != 0 {
		t.Fatalf("status = %d, stored %d; want 401 and nothing stored", w.Code, len(store.events))
	}
}

func TestWebhookRedeliveryIsAccepted(t *testing.T) {
	store := &fakeStore{}
	h := newHook(store, "k")
	body := fixture(t, "issues_opened.json")
	for i := 0; i < 2; i++ {
		if w := deliver(h, "issues", "same", sign([]byte("k"), body), body); w.Code != http.StatusAccepted {
			t.Fatalf("delivery %d: status %d", i, w.Code)
		}
	}
	if len(store.events) != 1 {
		t.Fatalf("stored %d, want 1", len(store.events))
	}
}

func TestWebhookRejectsMissingHeadersAndBadJSON(t *testing.T) {
	body := fixture(t, "issues_opened.json")
	sig := sign([]byte("k"), body)
	if w := deliver(newHook(&fakeStore{}, "k"), "", "d-1", sig, body); w.Code != http.StatusBadRequest {
		t.Errorf("missing event header: %d", w.Code)
	}
	if w := deliver(newHook(&fakeStore{}, "k"), "issues", "", sig, body); w.Code != http.StatusBadRequest {
		t.Errorf("missing delivery header: %d", w.Code)
	}
	junk := []byte("not json")
	if w := deliver(newHook(&fakeStore{}, "k"), "issues", "d-1", sign([]byte("k"), junk), junk); w.Code != http.StatusBadRequest {
		t.Errorf("non-JSON body: %d", w.Code)
	}
}

func TestWebhookRejectsOversizeBody(t *testing.T) {
	body := bytes.Repeat([]byte("a"), maxBody+1)
	w := deliver(newHook(&fakeStore{}, "k"), "issues", "d-1", sign([]byte("k"), body), body)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
}

func TestWebhookStoreFailureIs500(t *testing.T) {
	body := fixture(t, "issues_opened.json")
	w := deliver(newHook(&fakeStore{err: errors.New("disk full")}, "k"), "issues", "d-1", sign([]byte("k"), body), body)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

// blockingBody blocks reads until released, and reports when a read starts.
type blockingBody struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return 0, io.EOF
}

// Each body can be up to maxBody and is read before its signature, so only
// maxInFlight bodies are read at once; further requests wait their turn
// rather than being rejected (GitHub does not retry failed deliveries).
func TestWebhookLimitsBodiesInFlight(t *testing.T) {
	h := newHook(&fakeStore{}, "k")
	send := func(body io.Reader, done chan<- struct{}) {
		r := httptest.NewRequest(http.MethodPost, "/webhooks/github", body)
		h.ServeHTTP(httptest.NewRecorder(), r)
		close(done)
	}

	var held []*blockingBody
	var heldDone []chan struct{}
	for i := 0; i < maxInFlight; i++ {
		b := &blockingBody{started: make(chan struct{}), release: make(chan struct{})}
		done := make(chan struct{})
		go send(b, done)
		select {
		case <-b.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("request %d never started reading", i)
		}
		held, heldDone = append(held, b), append(heldDone, done)
	}

	extra := &blockingBody{started: make(chan struct{}), release: make(chan struct{})}
	close(extra.release)
	extraDone := make(chan struct{})
	go send(extra, extraDone)
	select {
	case <-extra.started:
		t.Fatal("a request beyond maxInFlight started reading its body")
	case <-time.After(100 * time.Millisecond):
	}

	close(held[0].release)
	<-heldDone[0]
	select {
	case <-extraDone:
	case <-time.After(2 * time.Second):
		t.Fatal("waiting request did not proceed once a slot freed")
	}
	for _, b := range held[1:] {
		close(b.release)
	}
	for _, d := range heldDone[1:] {
		<-d
	}
}

func TestWebhookGivesUpWaitingWhenClientLeaves(t *testing.T) {
	h := newHook(&fakeStore{}, "k")
	var releases []chan struct{}
	for i := 0; i < maxInFlight; i++ {
		b := &blockingBody{started: make(chan struct{}), release: make(chan struct{})}
		releases = append(releases, b.release)
		go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/webhooks/github", b))
		<-b.started
	}
	defer func() {
		for _, r := range releases {
			close(r)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader("{}")).WithContext(ctx))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for a client that gave up waiting", w.Code)
	}
}

// A body with a Content-Length is read into one exactly-sized buffer, not
// grown by io.ReadAll's repeated reallocation (which roughly doubles peak
// memory per body).
func TestWebhookReadsBodyWithOneAllocation(t *testing.T) {
	const size = 8 << 20
	body := bytes.Repeat([]byte("a"), size)
	h := newHook(&fakeStore{}, "k")
	r := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	r.Header.Set("X-Hub-Signature-256", "sha256=00") // rejected after the read
	w := httptest.NewRecorder()

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	h.ServeHTTP(w, r)
	runtime.ReadMemStats(&after)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > size*5/4 {
		t.Fatalf("allocated %d bytes reading a %d-byte body; want about one body", got, size)
	}
}

type errBody struct{ err error }

func (b errBody) Read([]byte) (int, error) { return 0, b.err }

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// Only an oversized body is a 413; a read that times out or fails is not.
func TestWebhookBodyReadErrorStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"timeout", timeoutErr{}, http.StatusRequestTimeout},
		{"other read error", errors.New("connection reset"), http.StatusBadRequest},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/webhooks/github", errBody{c.err})
		r.ContentLength = -1
		w := httptest.NewRecorder()
		newHook(&fakeStore{}, "k").ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, w.Code, c.want)
		}
	}
}

func TestWebhookRejectsDeclaredOversizeBeforeReading(t *testing.T) {
	b := &blockingBody{started: make(chan struct{}), release: make(chan struct{})}
	close(b.release)
	r := httptest.NewRequest(http.MethodPost, "/webhooks/github", b)
	r.ContentLength = maxBody + 1
	w := httptest.NewRecorder()
	newHook(&fakeStore{}, "k").ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
	select {
	case <-b.started:
		t.Fatal("read the body of a request that declared an oversized Content-Length")
	default:
	}
}

// Over a real connection, a sender that stalls mid-body loses its slot after
// BodyTimeout and gets a 408, so it cannot hold the slot until the server's
// overall read timeout.
func TestWebhookStalledSenderTimesOut(t *testing.T) {
	h := newHook(&fakeStore{}, "k")
	h.BodyTimeout = 200 * time.Millisecond
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// Declare 1000 bytes, send 10, then stall.
	_, err = fmt.Fprintf(conn, "POST /webhooks/github HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n0123456789")
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response to a stalled sender: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408", resp.StatusCode)
	}
}

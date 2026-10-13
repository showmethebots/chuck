package events

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

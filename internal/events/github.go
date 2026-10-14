// Package events turns outside activity into stored events: GitHub App
// webhook deliveries and the daemon's own cron ticks.
package events

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/orionsbelt-dev/chuck/internal/memory"
)

// maxBody is GitHub's own cap on a webhook payload.
const maxBody = 25 << 20

// maxInFlight bounds how many webhook bodies are read at once. The body must
// be read in full before its signature can be checked, each can be up to
// maxBody, and the Machine has 256 MB, so unbounded concurrent (or slow)
// requests could exhaust memory. Extra requests wait for a slot instead of
// failing, because GitHub does not retry failed App deliveries.
const maxInFlight = 2

// GitHubStore is the part of memory.Store the webhook needs.
type GitHubStore interface {
	InsertEvent(ctx context.Context, e memory.Event) (int64, bool, error)
}

// GitHub receives GitHub App webhook deliveries. It verifies the signature,
// stores the delivery verbatim, and returns 202; triage happens later.
type GitHub struct {
	Secret []byte
	Store  GitHubStore
	Log    *slog.Logger
	Now    func() time.Time // nil means time.Now

	slotsOnce sync.Once
	slots     chan struct{}
}

func (g *GitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.slotsOnce.Do(func() { g.slots = make(chan struct{}, maxInFlight) })
	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	case <-r.Context().Done():
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	if !validSignature(g.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		g.Log.Warn("github webhook rejected", "reason", "bad signature", "delivery", delivery)
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	name := r.Header.Get("X-GitHub-Event")
	if name == "" || delivery == "" {
		http.Error(w, "missing X-GitHub-Event or X-GitHub-Delivery", http.StatusBadRequest)
		return
	}
	var meta struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		http.Error(w, "payload is not JSON", http.StatusBadRequest)
		return
	}
	kind := name
	if meta.Action != "" {
		kind += "." + meta.Action
	}
	headers := r.Header.Clone()
	headers.Del("X-Hub-Signature")
	headers.Del("X-Hub-Signature-256")
	headerJSON, err := json.Marshal(headers)
	if err != nil {
		http.Error(w, "headers", http.StatusInternalServerError)
		return
	}

	id, inserted, err := g.Store.InsertEvent(r.Context(), memory.Event{
		Source:     memory.SourceGitHub,
		Kind:       kind,
		DeliveryID: delivery,
		Repo:       meta.Repository.FullName,
		Headers:    headerJSON,
		Payload:    body,
		ReceivedAt: g.now(),
	})
	if err != nil {
		g.Log.Error("store github event", "delivery", delivery, "err", err.Error())
		http.Error(w, "store failed", http.StatusInternalServerError)
		return
	}
	g.Log.Info("github event", "event_id", id, "kind", kind, "repo", meta.Repository.FullName,
		"delivery", delivery, "duplicate", !inserted)
	w.WriteHeader(http.StatusAccepted)
}

func (g *GitHub) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// validSignature checks GitHub's X-Hub-Signature-256 header. An empty secret
// rejects everything, so a missing secret locks the endpoint instead of
// opening it.
func validSignature(secret, body []byte, header string) bool {
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	if !ok || len(secret) == 0 {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

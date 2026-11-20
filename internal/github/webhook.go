package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/showmethebots/chuck/internal/memory"
)

// maxBody is GitHub's own cap on a webhook payload.
const maxBody = 25 << 20

// defaultBodyTimeout bounds how long a request may hold a read slot.
const defaultBodyTimeout = 10 * time.Second

// maxInFlight bounds how many webhook bodies are read at once. A body must
// be read in full before its signature can be checked, and each can be up
// to maxBody. Measured locally (macOS, Go 1.27, three concurrent unsigned
// 25 MB posts repeated six times), peak RSS was 140 MB before exact-size
// reads, 127 MB with them and 2 slots, and 100 MB with 1 slot. The Fly
// Machine has 512 MB shared with Litestream. Senders outside GitHub's hook
// ranges are turned away before they reach a slot (see Allowlist), so only
// GitHub can fill them. Extra requests wait for a slot instead of failing,
// because GitHub does not retry failed App deliveries.
const maxInFlight = 2

// ResultNoLoop is the result recorded on deliveries received while the
// loop is off.
const ResultNoLoop = "stored; the loop is off"

// GitHubStore is the part of memory.Store the webhook needs.
type GitHubStore interface {
	InsertWebhook(ctx context.Context, w memory.Webhook) (int64, bool, error)
}

// Receiver receives GitHub App webhook deliveries. It turns away senders
// outside GitHub's hook ranges before reading anything, verifies the
// signature, stores the delivery verbatim, and returns 202. The loop reads
// stored deliveries on its own.
type Receiver struct {
	Secret []byte
	Store  GitHubStore
	// Allow holds GitHub's hook ranges. A request is checked against it by
	// its Fly-Client-IP before it may take a read slot. Nil, with
	// AllowAnyIP unset, rejects every request.
	Allow *Allowlist
	// AllowAnyIP skips the range check, for local development through a
	// tunnel (CHUCK_WEBHOOK_ALLOW_ANY_IP=1). The signature is still checked.
	AllowAnyIP bool
	// Notify, when set, means the loop is on: deliveries are stored
	// unhandled for it, and Notify is called after each new one so the
	// loop reads it now rather than on its next tick. Nil stores each
	// delivery as already handled with ResultNoLoop, so turning the loop on
	// later does not replay what arrived while it was off.
	Notify func()
	Log    *slog.Logger
	Now    func() time.Time // nil means time.Now
	// BodyTimeout bounds how long a request may wait for a read slot and then
	// how long it may hold one; 0 means 10 s.
	BodyTimeout time.Duration

	slotsOnce sync.Once
	slots     chan struct{}
}

func (g *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Who is sending is known before the body is read, so anyone outside
	// GitHub's ranges costs neither memory nor a read slot. Fly's proxy
	// sets Fly-Client-IP; the header checks below (User-Agent, signature)
	// cannot stand in for this, since any sender can set them.
	if !g.AllowAnyIP {
		ips := r.Header.Values("Fly-Client-IP")
		var ip netip.Addr
		var err error
		if len(ips) == 1 {
			ip, err = netip.ParseAddr(strings.TrimSpace(ips[0]))
		}
		if len(ips) != 1 || err != nil || g.Allow == nil || !g.Allow.Contains(ip) {
			g.Log.Warn("github webhook rejected", "reason", "sender outside GitHub's hook ranges",
				"ip", strings.Join(ips, ","), "delivery", r.Header.Get("X-GitHub-Delivery"))
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	// Wait at most bodyTimeout for a slot. GitHub gives up on a delivery after
	// 10 s anyway, and the bound stops a queued connection from holding a
	// goroutine indefinitely. A client that disconnects while queued is not
	// noticed here: net/http only detects a disconnect once the body has been
	// read, so r.Context() does not end the wait.
	g.slotsOnce.Do(func() { g.slots = make(chan struct{}, maxInFlight) })
	wait := time.NewTimer(g.bodyTimeout())
	select {
	case g.slots <- struct{}{}:
		wait.Stop()
		defer func() { <-g.slots }()
	case <-wait.C:
		g.Log.Warn("github webhook rejected", "reason", "no read slot free", "delivery", r.Header.Get("X-GitHub-Delivery"))
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}

	// A sender that holds a slot must finish quickly: GitHub itself gives up
	// on a delivery after 10 s, so anything slower is not GitHub. This
	// deadline replaces the server's ReadTimeout for the body read. Test
	// recorders do not support deadlines; any other failure to set it is
	// logged, since it would leave the slot unbounded.
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(g.bodyTimeout())); err != nil && !errors.Is(err, http.ErrNotSupported) {
		g.Log.Warn("github webhook: could not set body read deadline", "err", err.Error())
	}

	body, err := readBody(w, r)
	if err != nil {
		var tooBig *http.MaxBytesError
		var netErr interface{ Timeout() bool }
		switch {
		case errors.As(err, &tooBig):
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		case errors.As(err, &netErr) && netErr.Timeout():
			http.Error(w, "body read timed out", http.StatusRequestTimeout)
		default:
			http.Error(w, "could not read body", http.StatusBadRequest)
		}
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
		Sender struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"sender"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		http.Error(w, "payload is not JSON", http.StatusBadRequest)
		return
	}
	// X-GitHub-Event is not signed, so a captured body could be replayed
	// under another event name. For the events the loop acts on, the
	// signed payload must have that event's shape.
	if !PayloadMatchesEvent(name, body) {
		g.Log.Warn("github webhook rejected", "reason", "payload does not match X-GitHub-Event", "event", name, "delivery", delivery)
		http.Error(w, "payload does not match X-GitHub-Event", http.StatusBadRequest)
		return
	}
	kind := name
	if meta.Action != "" {
		kind += "." + meta.Action
	}
	at := g.now()
	row := memory.Webhook{
		DeliveryID:  delivery,
		Event:       name,
		Action:      meta.Action,
		Repo:        meta.Repository.FullName,
		SenderID:    meta.Sender.ID,
		SenderLogin: meta.Sender.Login,
		Payload:     body,
		ReceivedAt:  at,
	}
	if g.Notify == nil {
		row.HandledAt, row.Result = at, ResultNoLoop
	}
	id, inserted, err := g.Store.InsertWebhook(r.Context(), row)
	if err != nil {
		g.Log.Error("store github webhook", "delivery", delivery, "err", err.Error())
		http.Error(w, "store failed", http.StatusInternalServerError)
		return
	}
	g.Log.Info("github webhook", "webhook_id", id, "kind", kind, "repo", meta.Repository.FullName,
		"delivery", delivery, "duplicate", !inserted)
	w.WriteHeader(http.StatusAccepted)
	if inserted && g.Notify != nil {
		g.Notify()
	}
}

// readBody reads the request body into a single buffer. With a
// Content-Length (GitHub always sends one) the buffer is allocated once at
// its exact size; io.ReadAll's repeated growth would roughly double the peak
// memory of each 25 MB body. A declared length over maxBody is rejected
// before anything is read.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	n := r.ContentLength
	if n > maxBody {
		return nil, &http.MaxBytesError{Limit: maxBody}
	}
	if n < 0 {
		return io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r.Body, body); err != nil {
		return nil, err
	}
	return body, nil
}

func (g *Receiver) bodyTimeout() time.Duration {
	if g.BodyTimeout > 0 {
		return g.BodyTimeout
	}
	return defaultBodyTimeout
}

func (g *Receiver) now() time.Time {
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

// eventShape is, for each event the loop acts on, the top-level keys its
// payload has and the ones it lacks. Other events are stored as they come;
// nothing acts on them.
var eventShape = map[string]struct{ has, lacks []string }{
	"issues":              {has: []string{"issue"}, lacks: []string{"comment", "pull_request"}},
	"issue_comment":       {has: []string{"issue", "comment"}},
	"pull_request":        {has: []string{"pull_request", "number"}, lacks: []string{"review", "comment"}},
	"pull_request_review": {has: []string{"pull_request", "review"}},
	"workflow_run":        {has: []string{"workflow_run"}},
}

// PayloadMatchesEvent reports whether a signed payload has the shape of
// the event its unsigned X-GitHub-Event header names. The loop checks it
// again before acting on a stored delivery.
func PayloadMatchesEvent(event string, body []byte) bool {
	shape, ok := eventShape[event]
	if !ok {
		return true
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return false
	}
	for _, k := range shape.has {
		if v, ok := top[k]; !ok || string(v) == "null" {
			return false
		}
	}
	for _, k := range shape.lacks {
		if _, ok := top[k]; ok {
			return false
		}
	}
	return true
}

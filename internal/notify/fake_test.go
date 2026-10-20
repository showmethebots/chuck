package notify

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/orionsbelt-dev/chuck/internal/memory"
)

var t0 = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type sent struct {
	channel, ts, user, text string
	blocks                  []slack.Block
}

type fakePoster struct {
	posts, updates, ephemerals []sent
	failPost                   error
	n                          int
}

func (f *fakePoster) Post(_ context.Context, channel, text string, blocks []slack.Block) (string, error) {
	if f.failPost != nil {
		return "", f.failPost
	}
	f.n++
	ts := fmt.Sprintf("1700000000.%06d", f.n)
	f.posts = append(f.posts, sent{channel: channel, ts: ts, text: text, blocks: blocks})
	return ts, nil
}

func (f *fakePoster) Update(_ context.Context, channel, ts, text string, blocks []slack.Block) error {
	f.updates = append(f.updates, sent{channel: channel, ts: ts, text: text, blocks: blocks})
	return nil
}

func (f *fakePoster) Ephemeral(_ context.Context, channel, user, text string) error {
	f.ephemerals = append(f.ephemerals, sent{channel: channel, user: user, text: text})
	return nil
}

func newStore(t *testing.T) *memory.Store {
	t.Helper()
	s, err := memory.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

var seeded int

// seedDecision stores an event and a decision for it, filling defaults.
func seedDecision(t *testing.T, s *memory.Store, d memory.Decision) int64 {
	t.Helper()
	ctx := context.Background()
	seeded++
	ev, _, err := s.InsertEvent(ctx, memory.Event{
		Source: memory.SourceGitHub, Kind: "pull_request.opened", DeliveryID: fmt.Sprintf("seed-%d", seeded),
		Repo: "orionsbelt-dev/demos", Payload: []byte(`{}`), ReceivedAt: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	d.EventID = ev
	if d.Stage == "" {
		d.Stage = memory.StageHaiku
	}
	if d.Outcome == "" {
		d.Outcome = memory.OutcomeOK
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = t0
	}
	id, err := s.RecordDecision(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

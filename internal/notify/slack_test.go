package notify

import (
	"context"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// The Socket Mode connection itself is exercised by the live checklist
// (docs/shadow-checklist.md); this pins the Poster contract at compile time.
var _ Poster = (*Slack)(nil)

type chanAcker chan string

func (a chanAcker) AckCtx(_ context.Context, envelopeID string, _ any) error {
	a <- envelopeID
	return nil
}

// blockingPoster's Post waits until release is closed, like a slow Slack.
type blockingPoster struct {
	fakePoster
	release chan struct{}
}

func (b *blockingPoster) Post(ctx context.Context, channel, text string, blocks []slack.Block) (string, error) {
	<-b.release
	return b.fakePoster.Post(ctx, channel, text, blocks)
}

// Slack shows operation_timeout unless a command is acked within 3 seconds,
// so a slow announcement mustn't hold up the ack.
func TestDispatchAcksCommandBeforeAnnouncing(t *testing.T) {
	h, s, _ := newInteractions(t)
	bp := &blockingPoster{release: make(chan struct{})}
	h.Poster = bp
	sc := &Slack{log: discardLog()}
	acks := make(chanAcker, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc.dispatch(context.Background(), acks, socketmode.Event{
			Type:    socketmode.EventTypeSlashCommand,
			Data:    slack.SlashCommand{UserID: member, ChannelID: "CMAIN", Text: "kill"},
			Request: &socketmode.Request{EnvelopeID: "env-1"},
		}, h)
	}()
	select {
	case id := <-acks:
		if id != "env-1" {
			t.Errorf("acked %q, want env-1", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("command not acked while the announcement was blocked")
	}
	if !paused(t, s).Paused {
		t.Error("kill was acked before triage was paused")
	}
	close(bp.release)
	<-done
	if len(bp.posts) != 1 {
		t.Errorf("announcements = %+v, want one after the ack", bp.posts)
	}
}

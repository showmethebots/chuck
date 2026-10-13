package notify

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/orionsbelt-dev/chuck/internal/memory"
)

// OutboxStore is the part of memory.Store the outbox uses.
type OutboxStore interface {
	PendingPosts(ctx context.Context, limit int) ([]memory.DecisionView, error)
	CreateApproval(ctx context.Context, decisionID int64, request string) error
	MarkPosted(ctx context.Context, id int64, channel, ts string, at time.Time) error
	RecordMessage(ctx context.Context, m memory.Message) error
}

// Outbox sends decision posts that are due. Triage never posts inline, so a
// Slack outage delays posts without losing them or blocking triage.
type Outbox struct {
	Store         OutboxStore
	Poster        Poster
	MainChannel   string // approval requests
	ShadowChannel string // grade posts
	Log           *slog.Logger
	Now           func() time.Time
}

func (o *Outbox) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Flush posts pending decisions oldest first and stops at the first Slack
// error, leaving the rest for the next flush.
func (o *Outbox) Flush(ctx context.Context) error {
	pending, err := o.Store.PendingPosts(ctx, 20)
	if err != nil {
		return err
	}
	for _, v := range pending {
		channel := o.ShadowChannel
		text := DecisionText(v)
		if v.PostKind == memory.PostApproval {
			channel = o.MainChannel
			if err := o.Store.CreateApproval(ctx, v.ID, text); err != nil {
				return err
			}
		}
		ts, err := o.Poster.Post(ctx, channel, text, DecisionBlocks(v))
		if err != nil {
			return fmt.Errorf("post decision %d: %w", v.ID, err)
		}
		at := o.now()
		if err := o.Store.MarkPosted(ctx, v.ID, channel, ts, at); err != nil {
			return err
		}
		if err := o.Store.RecordMessage(ctx, memory.Message{
			Channel: channel, TS: ts, Direction: "out", Kind: "post", Text: text, Allowed: true, CreatedAt: at,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Run flushes every interval until ctx ends.
func (o *Outbox) Run(ctx context.Context, every time.Duration) {
	for {
		if err := o.Flush(ctx); err != nil {
			o.Log.Warn("outbox flush", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

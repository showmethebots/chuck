// Package notify is chuck's Slack surface: decision posts, the outbox that
// sends them, /chuck commands and buttons behind the allowlist, and the
// daily summary.
package notify

import (
	"context"

	"github.com/slack-go/slack"
)

// Poster sends and edits Slack messages. *Slack implements it; tests fake it.
type Poster interface {
	Post(ctx context.Context, channel, text string, blocks []slack.Block) (ts string, err error)
	Update(ctx context.Context, channel, ts, text string, blocks []slack.Block) error
	Ephemeral(ctx context.Context, channel, user, text string) error
}

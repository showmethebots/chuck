package notify

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// Slack is chuck's Slack connection: a Poster over the Web API, and a
// Socket Mode client (an outbound websocket, so no public endpoint) that
// hands /chuck commands and button clicks to Interactions.
type Slack struct {
	api *slack.Client
	log *slog.Logger
}

func NewSlack(botToken, appToken string, log *slog.Logger) *Slack {
	return &Slack{api: slack.New(botToken, slack.OptionAppLevelToken(appToken)), log: log}
}

func msgOptions(text string, blocks []slack.Block) []slack.MsgOption {
	opts := []slack.MsgOption{slack.MsgOptionText(text, false)}
	if len(blocks) > 0 {
		opts = append(opts, slack.MsgOptionBlocks(blocks...))
	}
	return opts
}

func (s *Slack) Post(ctx context.Context, channel, text string, blocks []slack.Block) (string, error) {
	_, ts, err := s.api.PostMessageContext(ctx, channel, msgOptions(text, blocks)...)
	return ts, err
}

func (s *Slack) Update(ctx context.Context, channel, ts, text string, blocks []slack.Block) error {
	_, _, _, err := s.api.UpdateMessageContext(ctx, channel, ts, msgOptions(text, blocks)...)
	return err
}

func (s *Slack) Ephemeral(ctx context.Context, channel, user, text string) error {
	_, err := s.api.PostEphemeralContext(ctx, channel, user, slack.MsgOptionText(text, false))
	return err
}

// Run keeps a Socket Mode connection open until ctx ends, reconnecting after
// a drop.
func (s *Slack) Run(ctx context.Context, h *Interactions) {
	for ctx.Err() == nil {
		sm := socketmode.New(s.api)
		connCtx, cancel := context.WithCancel(ctx)
		go s.consume(connCtx, sm, h)
		err := sm.RunContext(connCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		s.log.Error("slack socket mode disconnected; reconnecting", "err", fmt.Sprint(err))
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
	}
}

func (s *Slack) consume(ctx context.Context, sm *socketmode.Client, h *Interactions) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-sm.Events:
			if !ok {
				return
			}
			s.dispatch(ctx, sm, evt, h)
		}
	}
}

func (s *Slack) dispatch(ctx context.Context, sm *socketmode.Client, evt socketmode.Event, h *Interactions) {
	switch evt.Type {
	case socketmode.EventTypeSlashCommand:
		cmd, ok := evt.Data.(slack.SlashCommand)
		if !ok || evt.Request == nil {
			return
		}
		reply := h.Command(ctx, cmd.UserID, cmd.ChannelID, cmd.Text)
		// Ack with ctx, not Ack's own context.TODO(): Ack blocks on the
		// client's response channel, and Run cancels connCtx (which ctx
		// derives from) as soon as RunContext returns, after the response
		// sender has stopped. An Ack reached after that point would
		// otherwise block forever and leak this goroutine.
		var payload any
		if reply != "" {
			payload = map[string]any{"response_type": "ephemeral", "text": reply}
		}
		if err := sm.AckCtx(ctx, evt.Request.EnvelopeID, payload); err != nil {
			s.log.Error("ack slash command", "err", err.Error())
		}
	case socketmode.EventTypeInteractive:
		cb, ok := evt.Data.(slack.InteractionCallback)
		if !ok || evt.Request == nil {
			return
		}
		if err := sm.AckCtx(ctx, evt.Request.EnvelopeID, nil); err != nil {
			s.log.Error("ack interaction", "err", err.Error())
		}
		if cb.Type != slack.InteractionTypeBlockActions {
			return
		}
		for _, a := range cb.ActionCallback.BlockActions {
			h.Action(ctx, cb.User.ID, cb.Channel.ID, cb.Message.Timestamp, a.ActionID, a.Value)
		}
	}
}

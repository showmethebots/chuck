package notify

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/orionsbelt-dev/chuck/internal/config"
	"github.com/orionsbelt-dev/chuck/internal/memory"
)

// InteractionStore is the part of memory.Store commands and buttons use.
type InteractionStore interface {
	Control(ctx context.Context) (memory.Control, error)
	SetControl(ctx context.Context, c memory.Control) error
	QueueDepth(ctx context.Context) (int, error)
	LastEventAt(ctx context.Context) (time.Time, error)
	SpendSince(ctx context.Context, since time.Time) (float64, error)
	DecisionView(ctx context.Context, id int64) (memory.DecisionView, error)
	RecordGrade(ctx context.Context, decisionID int64, grade, user string, at time.Time) error
	RespondApproval(ctx context.Context, decisionID int64, response, user string, at time.Time) (bool, error)
	RecordMessage(ctx context.Context, m memory.Message) error
}

// Interactions handles /chuck commands and post buttons. Every interaction
// is checked against the allowlist before anything else happens; users not
// on it get no reply and no effect, and every attempt is recorded.
type Interactions struct {
	Store       InteractionStore
	Allow       *config.Slack
	Poster      Poster
	MainChannel string
	HaikuBudget float64
	Version     string
	Log         *slog.Logger
	Now         func() time.Time
}

var commandRoles = map[string]config.Role{
	"status": config.RoleMember,
	"pause":  config.RoleMember,
	"kill":   config.RoleMember,
	"resume": config.RoleOwner,
}

var actionRoles = map[string]config.Role{
	ActionApprove:   config.RoleMember,
	ActionDeny:      config.RoleMember,
	ActionGradeUp:   config.RoleOwner,
	ActionGradeDown: config.RoleOwner,
}

const usage = "Usage: /chuck status | pause | kill | resume"

func (h *Interactions) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// Command handles "/chuck <text>" and returns the ephemeral reply, or ""
// when the user gets none.
func (h *Interactions) Command(ctx context.Context, user, channel, text string) string {
	sub := strings.ToLower(strings.TrimSpace(text))
	msg := memory.Message{Channel: channel, User: user, Direction: "in", Kind: "command",
		Text: strings.TrimSpace("/chuck " + text), CreatedAt: h.now()}
	role := h.Allow.RoleOf(user)
	if role == config.RoleNone {
		h.record(ctx, msg)
		return ""
	}
	need, known := commandRoles[sub]
	if !known {
		msg.Allowed = true
		h.record(ctx, msg)
		return usage
	}
	if role < need {
		h.record(ctx, msg)
		return fmt.Sprintf("`/chuck %s` needs the owner.", sub)
	}
	msg.Allowed = true
	reply, changed, err := h.run(ctx, sub, user)
	if err != nil {
		h.Log.Error("slack command", "command", sub, "err", err.Error())
		reply = "Something went wrong: " + err.Error()
	}
	msg.ChangedState = changed
	h.record(ctx, msg)
	return reply
}

func (h *Interactions) run(ctx context.Context, sub, user string) (string, bool, error) {
	switch sub {
	case "status":
		s, err := h.status(ctx)
		return s, false, err
	case "pause", "kill":
		if err := h.Store.SetControl(ctx, memory.Control{Paused: true, Reason: sub, By: user, ChangedAt: h.now()}); err != nil {
			return "", false, err
		}
		h.announce(ctx, fmt.Sprintf("<@%s> ran `/chuck %s`: triage is paused (Phase 1 has no workers to stop). Only the owner can resume.", user, sub))
		return "Paused.", true, nil
	case "resume":
		if err := h.Store.SetControl(ctx, memory.Control{Paused: false, By: user, ChangedAt: h.now()}); err != nil {
			return "", false, err
		}
		h.announce(ctx, fmt.Sprintf("<@%s> resumed chuck.", user))
		return "Resumed.", true, nil
	}
	return usage, false, nil
}

func (h *Interactions) status(ctx context.Context) (string, error) {
	ctl, err := h.Store.Control(ctx)
	if err != nil {
		return "", err
	}
	depth, err := h.Store.QueueDepth(ctx)
	if err != nil {
		return "", err
	}
	last, err := h.Store.LastEventAt(ctx)
	if err != nil {
		return "", err
	}
	n := h.now().UTC()
	spent, err := h.Store.SpendSince(ctx, time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC))
	if err != nil {
		return "", err
	}
	state := "running"
	if ctl.Paused {
		state = fmt.Sprintf("paused (%s by <@%s> at %s)", ctl.Reason, ctl.By, ctl.ChangedAt.Format(time.RFC3339))
	}
	lastS := "never"
	if !last.IsZero() {
		lastS = last.Format(time.RFC3339)
	}
	return fmt.Sprintf("chuck %s: %s\nqueue: %d untriaged · Haiku today: $%.2f of $%.2f · last GitHub event: %s",
		h.Version, state, depth, spent, h.HaikuBudget, lastS), nil
}

// Action handles a button click on the post at channel/ts.
func (h *Interactions) Action(ctx context.Context, user, channel, ts, actionID, value string) {
	msg := memory.Message{Channel: channel, TS: ts, User: user, Direction: "in", Kind: "button",
		Text: actionID + ":" + value, CreatedAt: h.now()}
	need, known := actionRoles[actionID]
	role := h.Allow.RoleOf(user)
	if !known || role == config.RoleNone {
		h.record(ctx, msg)
		return
	}
	if role < need {
		h.record(ctx, msg)
		h.ephemeral(ctx, channel, user, "Only the owner can grade decisions.")
		return
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		h.record(ctx, msg)
		return
	}
	msg.Allowed = true
	changed, note, err := h.act(ctx, actionID, id, user)
	if err != nil {
		h.Log.Error("slack button", "action", actionID, "decision_id", id, "err", err.Error())
		note = "Something went wrong: " + err.Error()
	}
	msg.ChangedState = changed
	h.record(ctx, msg)
	if note != "" {
		h.ephemeral(ctx, channel, user, note)
	}
	if changed {
		h.refresh(ctx, channel, ts, id)
	}
}

func (h *Interactions) act(ctx context.Context, actionID string, id int64, user string) (bool, string, error) {
	switch actionID {
	case ActionGradeUp, ActionGradeDown:
		grade := "up"
		if actionID == ActionGradeDown {
			grade = "down"
		}
		if err := h.Store.RecordGrade(ctx, id, grade, user, h.now()); err != nil {
			return false, "", err
		}
		return true, "", nil
	case ActionApprove, ActionDeny:
		response := "approve"
		if actionID == ActionDeny {
			response = "deny"
		}
		ok, err := h.Store.RespondApproval(ctx, id, response, user, h.now())
		if err != nil || !ok {
			return false, "This approval was already answered.", err
		}
		return true, "", nil
	}
	return false, "", nil
}

func (h *Interactions) refresh(ctx context.Context, channel, ts string, id int64) {
	v, err := h.Store.DecisionView(ctx, id)
	if err == nil {
		err = h.Poster.Update(ctx, channel, ts, DecisionText(v), DecisionBlocks(v))
	}
	if err != nil {
		h.Log.Error("refresh decision post", "decision_id", id, "err", err.Error())
	}
}

func (h *Interactions) announce(ctx context.Context, text string) {
	ts, err := h.Poster.Post(ctx, h.MainChannel, text, nil)
	if err != nil {
		h.Log.Error("slack announce", "err", err.Error())
		return
	}
	h.record(ctx, memory.Message{Channel: h.MainChannel, TS: ts, Direction: "out", Kind: "post",
		Text: text, Allowed: true, CreatedAt: h.now()})
}

func (h *Interactions) ephemeral(ctx context.Context, channel, user, text string) {
	if err := h.Poster.Ephemeral(ctx, channel, user, text); err != nil {
		h.Log.Error("slack ephemeral", "err", err.Error())
	}
}

func (h *Interactions) record(ctx context.Context, m memory.Message) {
	if err := h.Store.RecordMessage(ctx, m); err != nil {
		h.Log.Error("record slack message", "err", err.Error())
	}
}

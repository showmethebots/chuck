package notify

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orionsbelt-dev/chuck/internal/config"
	"github.com/orionsbelt-dev/chuck/internal/memory"
)

const (
	owner    = "UOWNER0001"
	member   = "UMEMBER001"
	stranger = "USTRANGE01"
)

func newInteractions(t *testing.T) (*Interactions, *memory.Store, *fakePoster) {
	t.Helper()
	s, p := newStore(t), &fakePoster{}
	return &Interactions{
		Store: s, Allow: &config.Slack{Owner: owner, Members: []string{member}}, Poster: p,
		MainChannel: "CMAIN", HaikuBudget: 5, Version: "test", Log: discardLog(),
		Now: func() time.Time { return t0 },
	}, s, p
}

func paused(t *testing.T, s *memory.Store) memory.Control {
	t.Helper()
	c, err := s.Control(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// noUnauthorizedChange is exit criterion 5: no interaction from a user
// without the required role changed anything.
func noUnauthorizedChange(t *testing.T, s *memory.Store) {
	t.Helper()
	msgs, err := s.Messages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if !m.Allowed && m.ChangedState {
			t.Errorf("unauthorized change recorded: %+v", m)
		}
	}
}

func TestCommandMatrix(t *testing.T) {
	cases := []struct {
		name        string
		startPaused bool
		user, text  string
		reply       string // substring; "" means no reply
		wantPaused  bool
		wantReason  string
		announced   bool
	}{
		{"owner status", false, owner, "status", "running", false, "", false},
		{"member status", false, member, "status", "queue: 0 untriaged", false, "", false},
		{"stranger status", false, stranger, "status", "", false, "", false},
		{"member pause", false, member, "pause", "Paused.", true, "pause", true},
		{"stranger pause", false, stranger, "pause", "", false, "", false},
		{"member kill", false, member, "kill", "Paused.", true, "kill", true},
		{"stranger kill", false, stranger, "kill", "", false, "", false},
		{"member resume refused", true, member, "resume", "needs the owner", true, "pause", false},
		{"stranger resume", true, stranger, "resume", "", true, "pause", false},
		{"owner resume", true, owner, "resume", "Resumed.", false, "", true},
		{"paused status", true, owner, "status", "paused (pause by <@UMEMBER001>", true, "pause", false},
		{"owner unknown", false, owner, "frobnicate", "Usage:", false, "", false},
		{"owner empty", false, owner, "", "Usage:", false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			h, s, p := newInteractions(t)
			if c.startPaused {
				if err := s.SetControl(ctx, memory.Control{Paused: true, Reason: "pause", By: member, ChangedAt: t0}); err != nil {
					t.Fatal(err)
				}
			}
			reply := h.Command(ctx, c.user, "CMAIN", c.text)
			if c.reply == "" && reply != "" {
				t.Errorf("reply = %q, want none", reply)
			}
			if c.reply != "" && !strings.Contains(reply, c.reply) {
				t.Errorf("reply = %q, want it to contain %q", reply, c.reply)
			}
			ctl := paused(t, s)
			if ctl.Paused != c.wantPaused || ctl.Reason != c.wantReason {
				t.Errorf("control = %+v, want paused=%v reason=%q", ctl, c.wantPaused, c.wantReason)
			}
			if got := len(p.posts) > 0; got != c.announced {
				t.Errorf("announced = %v, want %v (%+v)", got, c.announced, p.posts)
			}
			// An inbound command is Allowed exactly when the user is on the
			// allowlist and holds the role it needs; an unknown command from an
			// allowlisted user is Allowed because it changes nothing.
			msgs, _ := s.Messages(ctx)
			var in *memory.Message
			for i := range msgs {
				if msgs[i].Direction == "in" {
					in = &msgs[i]
					break
				}
			}
			if in == nil || in.User != c.user || in.Kind != "command" {
				t.Fatalf("command not recorded: %+v", msgs)
			}
			wantAllowed := c.user != stranger && !strings.Contains(c.reply, "needs the owner")
			if in.Allowed != wantAllowed {
				t.Errorf("allowed = %v, want %v", in.Allowed, wantAllowed)
			}
			noUnauthorizedChange(t, s)
		})
	}
}

func seedApproval(t *testing.T, s *memory.Store) int64 {
	t.Helper()
	id := seedDecision(t, s, memory.Decision{WouldDo: "merge", Permission: "ask_first", Target: "https://github.com/o/r/pull/8", PostKind: memory.PostApproval})
	if err := s.CreateApproval(context.Background(), id, "merge"); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestButtonMatrix(t *testing.T) {
	cases := []struct {
		name, user, action string
		wantGrade          string
		wantApproval       string
		ephemeral          string // substring; "" means none
		updated            bool
	}{
		{"owner grades up", owner, ActionGradeUp, "up", "", "", true},
		{"owner grades down", owner, ActionGradeDown, "down", "", "", true},
		{"member cannot grade", member, ActionGradeUp, "", "", "Only the owner", false},
		{"stranger cannot grade", stranger, ActionGradeUp, "", "", "", false},
		{"member approves", member, ActionApprove, "", "approve", "", true},
		{"owner denies", owner, ActionDeny, "", "deny", "", true},
		{"stranger cannot approve", stranger, ActionApprove, "", "", "", false},
		{"unknown action", owner, "launch_missiles", "", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			h, s, p := newInteractions(t)
			id := seedApproval(t, s)
			h.Action(ctx, c.user, "CMAIN", "1700000000.000001", c.action, strconv.FormatInt(id, 10))

			v, err := s.DecisionView(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if v.Grade != c.wantGrade || v.ApprovalResponse != c.wantApproval {
				t.Errorf("grade=%q approval=%q, want %q %q", v.Grade, v.ApprovalResponse, c.wantGrade, c.wantApproval)
			}
			if got := len(p.updates) > 0; got != c.updated {
				t.Errorf("updated = %v, want %v", got, c.updated)
			}
			if c.ephemeral == "" && len(p.ephemerals) > 0 {
				t.Errorf("unexpected ephemeral %+v", p.ephemerals)
			}
			if c.ephemeral != "" && (len(p.ephemerals) != 1 || !strings.Contains(p.ephemerals[0].text, c.ephemeral)) {
				t.Errorf("ephemerals = %+v, want %q", p.ephemerals, c.ephemeral)
			}
			noUnauthorizedChange(t, s)
		})
	}
}

// Review focus: two people answering one approval.
func TestApprovalFirstAnswerWins(t *testing.T) {
	ctx := context.Background()
	h, s, p := newInteractions(t)
	id := seedApproval(t, s)
	value := strconv.FormatInt(id, 10)
	h.Action(ctx, member, "CMAIN", "1.1", ActionApprove, value)
	h.Action(ctx, owner, "CMAIN", "1.1", ActionDeny, value)

	v, _ := s.DecisionView(ctx, id)
	if v.ApprovalResponse != "approve" || v.ApprovalBy != member {
		t.Fatalf("approval = %q by %q, want approve by member", v.ApprovalResponse, v.ApprovalBy)
	}
	if len(p.ephemerals) != 1 || !strings.Contains(p.ephemerals[0].text, "already answered") || p.ephemerals[0].user != owner {
		t.Fatalf("ephemerals = %+v", p.ephemerals)
	}
	if len(p.updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(p.updates))
	}
}

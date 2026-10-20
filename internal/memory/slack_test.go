package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestMessagesRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	in := Message{Channel: "C1", User: "USTRANGE01", Direction: "in", Kind: "command", Text: "/chuck pause", Allowed: false, CreatedAt: t0}
	if err := s.RecordMessage(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := s.Messages(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("Messages = %+v, %v", got, err)
	}
	g := got[0]
	if g.ID == 0 || !g.CreatedAt.Equal(in.CreatedAt) {
		t.Fatalf("id/created = %d %v", g.ID, g.CreatedAt)
	}
	g.ID, g.CreatedAt = 0, in.CreatedAt
	if g != in {
		t.Fatalf("got %+v, want %+v", g, in)
	}
}

// MarkPosted updates the decision and inserts the message row in one
// transaction: if the decision does not exist, neither write should land.
func TestMarkPostedRollsBackOnMissingDecision(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	const missing = int64(999)
	err := s.MarkPosted(ctx, missing, Message{
		Channel: "C1", TS: "111.222", Direction: "out", Kind: "post", Text: "posted", Allowed: true, CreatedAt: t0,
	})
	if err == nil {
		t.Fatal("MarkPosted on a missing decision did not error")
	}
	msgs, err := s.Messages(ctx)
	if err != nil || len(msgs) != 0 {
		t.Fatalf("Messages after failed MarkPosted = %+v, %v; want none", msgs, err)
	}
}

func TestCreateApprovalIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	id := recordDecision(t, s, Decision{EventID: insertEvent(t, s, Event{DeliveryID: "a"}), PostKind: PostApproval})
	for i := 0; i < 2; i++ {
		if err := s.CreateApproval(ctx, id, "merge https://github.com/o/r/pull/1"); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM approvals`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("approvals = %d, %v; want 1", n, err)
	}
}

func TestRespondApprovalFirstAnswerWins(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	id := recordDecision(t, s, Decision{EventID: insertEvent(t, s, Event{DeliveryID: "a"}), PostKind: PostApproval})
	if err := s.CreateApproval(ctx, id, "merge"); err != nil {
		t.Fatal(err)
	}
	ok, err := s.RespondApproval(ctx, id, "approve", "UMEMBER001", t0)
	if err != nil || !ok {
		t.Fatalf("first answer = %v, %v", ok, err)
	}
	ok, err = s.RespondApproval(ctx, id, "deny", "UOWNER0001", t0.Add(time.Second))
	if err != nil || ok {
		t.Fatalf("second answer = %v, %v; want false", ok, err)
	}
	v, _ := s.DecisionView(ctx, id)
	if v.ApprovalResponse != "approve" || v.ApprovalBy != "UMEMBER001" {
		t.Fatalf("approval = %q by %q", v.ApprovalResponse, v.ApprovalBy)
	}
}

func TestLatestGradeWins(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	id := recordDecision(t, s, Decision{EventID: insertEvent(t, s, Event{DeliveryID: "a"}), PostKind: PostGrade})
	if err := s.RecordGrade(ctx, id, "down", "UOWNER0001", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordGrade(ctx, id, "up", "UOWNER0001", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.DecisionView(ctx, id); v.Grade != "up" {
		t.Fatalf("grade = %q, want up", v.Grade)
	}
}

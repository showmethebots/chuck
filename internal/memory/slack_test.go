package memory

import (
	"context"
	"path/filepath"
	"testing"
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

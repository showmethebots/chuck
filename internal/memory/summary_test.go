package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSummary(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	n := 0
	add := func(d Decision) int64 {
		n++
		d.EventID = insertEvent(t, s, Event{DeliveryID: string(rune('a' + n))})
		return recordDecision(t, s, d)
	}
	upOK := add(Decision{Category: "new_issue", WouldDo: "comment_issue", PostKind: PostGrade, CostUSD: 0.01})
	downAsk := add(Decision{Category: "pr_lifecycle", WouldDo: "merge", Permission: "ask_first", PostKind: PostApproval, CostUSD: 0.02})
	add(Decision{Category: "noise", WouldDo: "none", CostUSD: 0.01})
	add(Decision{Stage: StageRule, Outcome: OutcomeDropped, Rationale: "ping"})
	add(Decision{Outcome: OutcomeFailed, CostUSD: 0.01})
	add(Decision{Category: "push", WouldDo: "create_issue", PostKind: PostGrade})                     // pending grade
	add(Decision{Category: "noise", WouldDo: "none", CostUSD: 5, CreatedAt: t0.Add(-48 * time.Hour)}) // outside window

	if err := s.RecordGrade(ctx, upOK, "down", "UOWNER0001", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordGrade(ctx, upOK, "up", "UOWNER0001", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordGrade(ctx, downAsk, "down", "UOWNER0001", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	sum, err := s.Summary(ctx, t0.Add(-time.Hour), t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Outcomes[OutcomeOK] != 4 || sum.Outcomes[OutcomeDropped] != 1 || sum.Outcomes[OutcomeFailed] != 1 {
		t.Errorf("outcomes = %v", sum.Outcomes)
	}
	if sum.Categories["noise"] != 1 || sum.Categories["new_issue"] != 1 {
		t.Errorf("categories = %v", sum.Categories)
	}
	if sum.SpendUSD < 0.0499 || sum.SpendUSD > 0.0501 {
		t.Errorf("spend = %v, want 0.05", sum.SpendUSD)
	}
	if sum.GradedInWindow != 3 || sum.Graded != 2 || sum.Up != 1 || sum.AskFirstDown != 1 || sum.PendingGrades != 1 {
		t.Errorf("grades: in window %d, graded %d, up %d, ask_first down %d, pending %d",
			sum.GradedInWindow, sum.Graded, sum.Up, sum.AskFirstDown, sum.PendingGrades)
	}
}

func TestSummaryAllowlistCounts(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()
	for _, m := range []Message{
		{Direction: "in", Kind: "command", Text: "/chuck pause", User: "USTRANGE01", CreatedAt: t0},
		{Direction: "in", Kind: "button", Text: "grade_up:1", User: "UMEMBER001", CreatedAt: t0},
		{Direction: "in", Kind: "command", Text: "/chuck pause", User: "UMEMBER001", Allowed: true, ChangedState: true, CreatedAt: t0},
		{Direction: "out", Kind: "post", Text: "posted", Allowed: true, CreatedAt: t0},
	} {
		if err := s.RecordMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := s.Summary(ctx, t0.Add(-time.Hour), t0.Add(time.Hour))
	if err != nil || sum.Rejected != 2 || sum.Violations != 0 {
		t.Fatalf("rejected %d violations %d, %v; want 2 and 0", sum.Rejected, sum.Violations, err)
	}
}

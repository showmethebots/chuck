package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func recordDecision(t *testing.T, s *Store, d Decision) int64 {
	t.Helper()
	if d.Stage == "" {
		d.Stage = StageHaiku
	}
	if d.Outcome == "" {
		d.Outcome = OutcomeOK
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = t0
	}
	id, err := s.RecordDecision(context.Background(), d)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	return id
}

func TestRecordDecisionTriagesEvent(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	ev := insertEvent(t, s, Event{DeliveryID: "a", Kind: "issues.opened", Repo: "o/r", ReceivedAt: t0})
	id := recordDecision(t, s, Decision{
		EventID: ev, Category: "new_issue", WouldDo: "comment_issue", Permission: "act_freely",
		Target: "https://github.com/o/r/issues/1", Confidence: 0.8, Rationale: "asks a question",
		Model: "claude-haiku-4-5", InputTokens: 1000, OutputTokens: 100, CostUSD: 0.0015, PostKind: PostGrade,
	})

	if n, _ := s.QueueDepth(ctx); n != 0 {
		t.Fatalf("QueueDepth = %d after decision, want 0", n)
	}
	v, err := s.DecisionView(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Repo != "o/r" || v.Kind != "issues.opened" || v.WouldDo != "comment_issue" || v.PostKind != PostGrade || v.CostUSD != 0.0015 {
		t.Errorf("view = %+v", v)
	}
	if !v.CreatedAt.Equal(t0) {
		t.Errorf("CreatedAt = %v", v.CreatedAt)
	}
	byEvent, err := s.DecisionForEvent(ctx, ev)
	if err != nil || byEvent.ID != id {
		t.Fatalf("DecisionForEvent = %+v, %v", byEvent, err)
	}
}

func TestFailAttemptCountsAndDelays(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	ev := insertEvent(t, s, Event{DeliveryID: "a", ReceivedAt: t0})
	for want := 1; want <= 2; want++ {
		n, err := s.FailAttempt(ctx, ev, t0.Add(time.Hour))
		if err != nil || n != want {
			t.Fatalf("FailAttempt = %d, %v; want %d", n, err, want)
		}
	}
	if e, _ := s.NextUntriaged(ctx, t0.Add(time.Minute)); e != nil {
		t.Fatalf("event eligible before its retry time")
	}
	e, err := s.NextUntriaged(ctx, t0.Add(time.Hour))
	if err != nil || e == nil || e.Attempts != 2 {
		t.Fatalf("NextUntriaged = %+v, %v; want attempts 2", e, err)
	}
}

func TestSpendSinceIsInclusiveOfStart(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	day := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	for i, at := range []time.Time{day.Add(-time.Microsecond), day, day.Add(23 * time.Hour)} {
		ev := insertEvent(t, s, Event{DeliveryID: string(rune('a' + i)), ReceivedAt: at})
		recordDecision(t, s, Decision{EventID: ev, CostUSD: 1, CreatedAt: at})
	}
	got, err := s.SpendSince(ctx, day)
	if err != nil || got != 2 {
		t.Fatalf("SpendSince = %v, %v; want 2", got, err)
	}
}

func TestOutboxQueries(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	grade := recordDecision(t, s, Decision{EventID: insertEvent(t, s, Event{DeliveryID: "a"}), PostKind: PostGrade})
	approval := recordDecision(t, s, Decision{EventID: insertEvent(t, s, Event{DeliveryID: "b"}), PostKind: PostApproval})
	quiet := recordDecision(t, s, Decision{EventID: insertEvent(t, s, Event{DeliveryID: "c"}), WouldDo: "none"})

	pending, err := s.PendingPosts(ctx, 10)
	if err != nil || len(pending) != 2 || pending[0].ID != grade || pending[1].ID != approval {
		t.Fatalf("PendingPosts = %+v, %v", pending, err)
	}
	msg := Message{Channel: "C1", TS: "111.222", Direction: "out", Kind: "post", Text: "posted", Allowed: true, CreatedAt: t0}
	if err := s.MarkPosted(ctx, grade, msg); err != nil {
		t.Fatal(err)
	}
	pending, _ = s.PendingPosts(ctx, 10)
	if len(pending) != 1 || pending[0].ID != approval {
		t.Fatalf("after MarkPosted: %+v", pending)
	}
	v, _ := s.DecisionView(ctx, grade)
	if v.SlackChannel != "C1" || v.SlackTS != "111.222" {
		t.Errorf("slack location = %q %q", v.SlackChannel, v.SlackTS)
	}
	msgs, err := s.Messages(ctx)
	if err != nil || len(msgs) != 1 || msgs[0].Channel != "C1" || msgs[0].TS != "111.222" {
		t.Fatalf("Messages after MarkPosted = %+v, %v", msgs, err)
	}

	// Queueing a grade post never overrides an approval post.
	if err := s.QueueGradePosts(ctx, []int64{quiet, approval}); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.DecisionView(ctx, quiet); v.PostKind != PostGrade {
		t.Errorf("quiet post kind = %q, want grade", v.PostKind)
	}
	if v, _ := s.DecisionView(ctx, approval); v.PostKind != PostApproval {
		t.Errorf("approval post kind = %q, want approval", v.PostKind)
	}
}

func TestSampleQuietPicksOnlyUnpostedQuietDecisionsInWindow(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	in := func(id string, d Decision) int64 {
		d.EventID = insertEvent(t, s, Event{DeliveryID: id})
		return recordDecision(t, s, d)
	}
	none := in("a", Decision{WouldDo: "none"})
	dropped := in("b", Decision{Stage: StageRule, Outcome: OutcomeDropped, Rationale: "ping"})
	in("c", Decision{WouldDo: "comment_issue", PostKind: PostGrade})       // already posted kind
	in("d", Decision{Outcome: OutcomeFailed})                              // failures are counted, not sampled
	in("e", Decision{WouldDo: "none", CreatedAt: t0.Add(-48 * time.Hour)}) // outside window

	ids, err := s.SampleQuiet(ctx, t0.Add(-time.Hour), t0.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if len(ids) != 2 || !got[none] || !got[dropped] {
		t.Fatalf("SampleQuiet = %v, want {%d, %d}", ids, none, dropped)
	}
	if ids, _ := s.SampleQuiet(ctx, t0.Add(-time.Hour), t0.Add(time.Hour), 1); len(ids) != 1 {
		t.Fatalf("limit not applied: %v", ids)
	}
}

func TestDecisionViewURLFromPayload(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = s.Close() }()

	fixture := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "testdata", "github", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, tc := range []struct {
		kind    string
		payload []byte
		want    string
	}{
		{"issues.opened", fixture("issues_opened.json"), "https://github.com/orionsbelt-dev/demos/issues/7"},
		{"issue_comment.created", fixture("issue_comment_created.json"), "https://github.com/orionsbelt-dev/demos/issues/7"},
		{"pull_request_review.submitted", fixture("pull_request_review_submitted.json"), "https://github.com/orionsbelt-dev/demos/pull/8"},
		{"workflow_run.completed", fixture("workflow_run_completed.json"), "https://github.com/orionsbelt-dev/demos/actions/runs/42"},
		{"push", fixture("push.json"), "https://github.com/orionsbelt-dev/demos/compare/abc123...def456"},
		{"ping", []byte(`{}`), ""},
	} {
		ev := insertEvent(t, s, Event{DeliveryID: tc.kind, Kind: tc.kind, Repo: "o/r", Payload: tc.payload, ReceivedAt: t0})
		id := recordDecision(t, s, Decision{EventID: ev, WouldDo: "none"})
		v, err := s.DecisionView(ctx, id)
		if err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		if v.URL != tc.want {
			t.Errorf("%s: URL = %q, want %q", tc.kind, v.URL, tc.want)
		}
	}
}

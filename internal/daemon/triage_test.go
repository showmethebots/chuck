package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orionsbelt-dev/chuck/internal/config"
	"github.com/orionsbelt-dev/chuck/internal/memory"
	"github.com/orionsbelt-dev/chuck/internal/triage"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testPolicy() config.Policy {
	return config.Policy{
		Permissions: config.Permissions{
			ActFreely:     []string{"read", "triage", "comment_issue"},
			ActThenNotify: []string{"create_issue"},
			AskFirst:      []string{"merge"},
		},
		Budgets: config.Budgets{DailyUSD: map[config.Tier]float64{
			config.TierHaiku: 1, config.TierSonnet: 1, config.TierOpus: 1,
		}},
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// scriptedClassifier returns outputs[i] or errs[i] on call i, then a
// "none" result.
type scriptedClassifier struct {
	outputs []string
	errs    []error
	calls   int
}

const noneResult = `{"category":"noise","would_do":"none","target":"","confidence":0.9,"rationale":"nothing to do"}`

func (s *scriptedClassifier) Classify(_ context.Context, _ triage.Input) (string, triage.Usage, error) {
	i := s.calls
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		return "", triage.Usage{}, s.errs[i]
	}
	out := noneResult
	if i < len(s.outputs) && s.outputs[i] != "" {
		out = s.outputs[i]
	}
	return out, triage.Usage{Model: triage.HaikuModel, InputTokens: 1000, OutputTokens: 100}, nil
}

func issueFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "github", "issues_opened.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var deliveries int

func addEvent(t *testing.T, s *memory.Store, kind, payload string, at time.Time) int64 {
	t.Helper()
	deliveries++
	id, _, err := s.InsertEvent(context.Background(), memory.Event{
		Source: memory.SourceGitHub, Kind: kind, DeliveryID: fmt.Sprintf("%s-%d", kind, deliveries),
		Repo: "orionsbelt-dev/demos", Payload: []byte(payload), ReceivedAt: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newTriageEnv(t *testing.T) (*Triage, *memory.Store, *clock, *scriptedClassifier) {
	t.Helper()
	s, err := memory.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c := &clock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	sc := &scriptedClassifier{}
	return &Triage{Store: s, Classifier: sc, Policy: testPolicy(), Log: discardLog(), Now: c.now}, s, c, sc
}

func step(t *testing.T, tr *Triage) bool {
	t.Helper()
	did, err := tr.Step(context.Background())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	return did
}

func TestTriageDropsNoiseByRule(t *testing.T) {
	tr, s, c, sc := newTriageEnv(t)
	ev := addEvent(t, s, "workflow_run.requested", `{}`, c.t)
	if !step(t, tr) {
		t.Fatal("no work done")
	}
	d, err := s.DecisionForEvent(context.Background(), ev)
	if err != nil || d.Outcome != memory.OutcomeDropped || d.PostKind != "" {
		t.Fatalf("decision = %+v, %v", d, err)
	}
	if sc.calls != 0 {
		t.Fatal("classifier called")
	}
}

func TestTriagePostKindFollowsPermission(t *testing.T) {
	tr, s, c, sc := newTriageEnv(t)
	sc.outputs = []string{
		`{"category":"new_issue","would_do":"comment_issue","target":"https://github.com/orionsbelt-dev/demos/issues/7","confidence":0.8,"rationale":"r"}`,
		`{"category":"pr_lifecycle","would_do":"merge","target":"https://github.com/orionsbelt-dev/demos/pull/8","confidence":0.8,"rationale":"r"}`,
		noneResult,
	}
	ids := []int64{
		addEvent(t, s, "issues.opened", issueFixture(t), c.t),
		addEvent(t, s, "issues.opened", issueFixture(t), c.t),
		addEvent(t, s, "issues.opened", issueFixture(t), c.t),
	}
	for range ids {
		step(t, tr)
	}
	want := []struct{ kind, perm string }{
		{memory.PostGrade, "act_freely"},
		{memory.PostApproval, "ask_first"},
		{"", ""},
	}
	for i, ev := range ids {
		d, err := s.DecisionForEvent(context.Background(), ev)
		if err != nil {
			t.Fatal(err)
		}
		if d.PostKind != want[i].kind || d.Permission != want[i].perm || d.CostUSD != 0.0015 {
			t.Errorf("event %d: post_kind=%q permission=%q cost=%v, want %+v", i, d.PostKind, d.Permission, d.CostUSD, want[i])
		}
	}
}

func TestTriageInvalidOutputIsFailedNotPosted(t *testing.T) {
	tr, s, c, sc := newTriageEnv(t)
	sc.outputs = []string{`{"category":"noise","would_do":"delete_repo","target":"https://github.com/o/r","confidence":0.5,"rationale":"x"}`}
	ev := addEvent(t, s, "issues.opened", issueFixture(t), c.t)
	step(t, tr)
	d, _ := s.DecisionForEvent(context.Background(), ev)
	if d.Outcome != memory.OutcomeFailed || d.PostKind != "" || !strings.Contains(d.RawOutput, "delete_repo") {
		t.Fatalf("decision = %+v", d)
	}
}

func TestTriageRetriesThenFails(t *testing.T) {
	tr, s, c, sc := newTriageEnv(t)
	boom := errors.New("529 overloaded")
	sc.errs = []error{boom, boom, boom}
	ev := addEvent(t, s, "issues.opened", issueFixture(t), c.t)

	if !step(t, tr) {
		t.Fatal("first attempt did no work")
	}
	if _, err := s.DecisionForEvent(context.Background(), ev); err == nil {
		t.Fatal("decision written after first failure")
	}
	if step(t, tr) {
		t.Fatal("retried before backoff elapsed")
	}
	c.t = c.t.Add(time.Minute)
	step(t, tr) // attempt 2
	c.t = c.t.Add(5 * time.Minute)
	step(t, tr) // attempt 3: gives up
	d, err := s.DecisionForEvent(context.Background(), ev)
	if err != nil || d.Outcome != memory.OutcomeFailed || !strings.Contains(d.Rationale, "529") {
		t.Fatalf("decision = %+v, %v", d, err)
	}
	if sc.calls != 3 {
		t.Fatalf("classifier calls = %d, want 3", sc.calls)
	}
}

func TestTriageWaitsWhilePaused(t *testing.T) {
	tr, s, c, _ := newTriageEnv(t)
	ctx := context.Background()
	if err := s.SetControl(ctx, memory.Control{Paused: true, Reason: "pause", By: "U0123456789", ChangedAt: c.t}); err != nil {
		t.Fatal(err)
	}
	addEvent(t, s, "issues.opened", issueFixture(t), c.t)
	if step(t, tr) {
		t.Fatal("triaged while paused")
	}
	if n, _ := s.QueueDepth(ctx); n != 1 {
		t.Fatalf("queue = %d; events must keep queueing while paused", n)
	}
	if err := s.SetControl(ctx, memory.Control{By: "U0123456789", ChangedAt: c.t}); err != nil {
		t.Fatal(err)
	}
	if !step(t, tr) {
		t.Fatal("did not resume")
	}
}

func TestTriageStopsAtBudgetOncePerDay(t *testing.T) {
	tr, s, c, _ := newTriageEnv(t)
	tr.Policy.Budgets.DailyUSD[config.TierHaiku] = 0.002 // two calls at $0.0015 cross it
	var hits int
	tr.OnBudgetHit = func(context.Context, float64, float64) { hits++ }
	for i := 0; i < 4; i++ {
		addEvent(t, s, "issues.opened", issueFixture(t), c.t)
	}
	first, second := step(t, tr), step(t, tr)
	if !first || !second {
		t.Fatal("first two calls should run")
	}
	third, fourth := step(t, tr), step(t, tr)
	if third || fourth {
		t.Fatal("ran past the budget")
	}
	if hits != 1 {
		t.Fatalf("OnBudgetHit called %d times, want 1", hits)
	}
	c.t = time.Date(2026, 10, 14, 0, 0, 1, 0, time.UTC)
	if !step(t, tr) {
		t.Fatal("did not resume the next UTC day")
	}
}

func TestTriageRunStopsOnCancel(t *testing.T) {
	tr, _, _, _ := newTriageEnv(t)
	tr.Idle = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tr.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"

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

// scriptedClassifier returns outputs[i] on call i, then a "none" result.
type scriptedClassifier struct {
	outputs []string
	calls   int
}

const noneResult = `{"category":"noise","would_do":"none","target":"","confidence":0.9,"rationale":"nothing to do"}`

func (s *scriptedClassifier) Classify(_ context.Context, _ triage.Input) (string, triage.Usage, error) {
	i := s.calls
	s.calls++
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
	tr, s, c, _ := newTriageEnv(t)
	api := newFakeAnthropic(t, http.StatusBadRequest)
	tr.Classifier = api.haiku()
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
	if err != nil || d.Outcome != memory.OutcomeFailed || !strings.Contains(d.Rationale, "400") {
		t.Fatalf("decision = %+v, %v", d, err)
	}
	if n := api.requests.Load(); n != 3 {
		t.Fatalf("classifier calls = %d, want 3", n)
	}
}

// fakeAnthropic is an httptest server standing in for the Messages API. It
// answers with status and an Anthropic-shaped error body, or with a "none"
// result when status is 200, so the daemon sees the errors the SDK really
// produces.
type fakeAnthropic struct {
	srv      *httptest.Server
	status   atomic.Int32
	requests atomic.Int32
}

func newFakeAnthropic(t *testing.T, status int) *fakeAnthropic {
	t.Helper()
	f := &fakeAnthropic{}
	f.status.Store(int32(status))
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		st := int(f.status.Load())
		if st != http.StatusOK {
			w.WriteHeader(st)
			_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key (HTTP %d)"}}`, st)
			return
		}
		text, _ := json.Marshal(noneResult)
		_, _ = fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5",
			"content":[{"type":"text","text":%s}],"stop_reason":"end_turn","stop_sequence":null,
			"usage":{"input_tokens":1000,"output_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`, text)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAnthropic) haiku() *triage.Haiku {
	return triage.NewHaiku(testPolicy(), option.WithBaseURL(f.srv.URL), option.WithAPIKey("x"), option.WithMaxRetries(0))
}

// failSpy counts FailAttempt calls, so a test can prove an error was not
// charged to any event.
type failSpy struct {
	*memory.Store
	fails int
}

func (f *failSpy) FailAttempt(ctx context.Context, eventID int64, next time.Time) (int, error) {
	f.fails++
	return f.Store.FailAttempt(ctx, eventID, next)
}

// assertUntouched checks no event was charged an attempt or decided.
func assertUntouched(t *testing.T, spy *failSpy, ids ...int64) {
	t.Helper()
	if spy.fails != 0 {
		t.Fatalf("FailAttempt called %d times for a systemic error", spy.fails)
	}
	ctx := context.Background()
	for _, id := range ids {
		if d, err := spy.DecisionForEvent(ctx, id); err == nil {
			t.Fatalf("event %d decided: %+v", id, d)
		}
	}
	ev, err := spy.NextUntriaged(ctx, time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || ev == nil || ev.ID != ids[0] || ev.Attempts != 0 {
		t.Fatalf("next untriaged = %+v, %v; want event %d with 0 attempts", ev, err, ids[0])
	}
}

func TestTriageBacksOffOnSystemicError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, 529} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			tr, s, c, _ := newTriageEnv(t)
			spy := &failSpy{Store: s}
			tr.Store = spy
			api := newFakeAnthropic(t, status)
			tr.Classifier = api.haiku()
			start := c.t
			ids := []int64{
				addEvent(t, s, "issues.opened", issueFixture(t), c.t),
				addEvent(t, s, "issues.opened", issueFixture(t), c.t),
			}

			if step(t, tr) {
				t.Fatal("systemic failure reported as work done")
			}
			assertUntouched(t, spy, ids...)
			if n := api.requests.Load(); n != 1 {
				t.Fatalf("requests = %d, want 1", n)
			}

			// Blocked for 1m: Step doesn't reach the queue or the API.
			for _, at := range []time.Duration{0, 59 * time.Second} {
				c.t = start.Add(at)
				if step(t, tr) {
					t.Fatalf("worked at +%v, inside the backoff", at)
				}
			}
			if n := api.requests.Load(); n != 1 {
				t.Fatalf("requests = %d during the backoff, want 1", n)
			}

			// Second consecutive failure backs off 5m.
			c.t = start.Add(time.Minute)
			if step(t, tr) {
				t.Fatal("second systemic failure reported as work done")
			}
			assertUntouched(t, spy, ids...)
			c.t = start.Add(time.Minute + 5*time.Minute - time.Second)
			if step(t, tr) {
				t.Fatal("worked inside the 5m backoff")
			}
			if n := api.requests.Load(); n != 2 {
				t.Fatalf("requests = %d, want 2", n)
			}

			// The API recovers: both events triage normally.
			api.status.Store(http.StatusOK)
			c.t = start.Add(6 * time.Minute)
			for range ids {
				if !step(t, tr) {
					t.Fatal("did not resume after the backoff")
				}
			}
			for _, id := range ids {
				d, err := s.DecisionForEvent(context.Background(), id)
				if err != nil || d.Outcome != memory.OutcomeOK || d.WouldDo != triage.WouldDoNone {
					t.Fatalf("event %d: decision = %+v, %v", id, d, err)
				}
			}
			if spy.fails != 0 {
				t.Fatalf("FailAttempt called %d times", spy.fails)
			}
		})
	}
}

func TestTriageSuccessResetsSystemicBackoff(t *testing.T) {
	tr, s, c, _ := newTriageEnv(t)
	api := newFakeAnthropic(t, http.StatusTooManyRequests)
	tr.Classifier = api.haiku()
	start := c.t
	for i := 0; i < 3; i++ {
		addEvent(t, s, "issues.opened", issueFixture(t), c.t)
	}
	step(t, tr) // 429: blocked 1m
	api.status.Store(http.StatusOK)
	c.t = start.Add(time.Minute)
	if !step(t, tr) {
		t.Fatal("did not resume")
	}
	api.status.Store(http.StatusTooManyRequests)
	step(t, tr) // first failure since the success: 1m again, not 5m
	c.t = start.Add(2 * time.Minute)
	api.status.Store(http.StatusOK)
	if !step(t, tr) {
		t.Fatal("backoff after a success was not reset to 1m")
	}
}

// cancellingClassifier cancels the daemon's context and then calls the real
// client, as a shutdown arriving mid-request would.
type cancellingClassifier struct {
	cancel context.CancelFunc
	inner  triage.Classifier
}

func (c cancellingClassifier) Classify(ctx context.Context, in triage.Input) (string, triage.Usage, error) {
	c.cancel()
	return c.inner.Classify(ctx, in)
}

func TestTriageShutdownDoesNotConsumeAttempt(t *testing.T) {
	tr, s, c, _ := newTriageEnv(t)
	spy := &failSpy{Store: s}
	tr.Store = spy
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr.Classifier = cancellingClassifier{cancel: cancel, inner: newFakeAnthropic(t, http.StatusOK).haiku()}
	id := addEvent(t, s, "issues.opened", issueFixture(t), c.t)

	did, err := tr.Step(ctx)
	if did || err != nil {
		t.Fatalf("Step = %v, %v; want false, nil", did, err)
	}
	assertUntouched(t, spy, id)
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

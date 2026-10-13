package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/orionsbelt-dev/chuck/internal/config"
	"github.com/orionsbelt-dev/chuck/internal/memory"
	"github.com/orionsbelt-dev/chuck/internal/triage"
)

// TriageStore is the part of memory.Store the triage loop uses.
type TriageStore interface {
	Control(ctx context.Context) (memory.Control, error)
	SpendSince(ctx context.Context, since time.Time) (float64, error)
	NextUntriaged(ctx context.Context, now time.Time) (*memory.Event, error)
	FailAttempt(ctx context.Context, eventID int64, next time.Time) (int, error)
	RecordDecision(ctx context.Context, d memory.Decision) (int64, error)
}

// maxAttempts is how many classifier errors an event gets before it is
// recorded as triage_failed.
const maxAttempts = 3

// retryAfter is the wait after the nth failed attempt.
func retryAfter(n int) time.Duration {
	if n <= 1 {
		return time.Minute
	}
	return 5 * time.Minute
}

// Triage takes untriaged events oldest-first and records a decision for
// each. It waits while paused or once the day's Haiku budget is spent.
type Triage struct {
	Store      TriageStore
	Classifier triage.Classifier
	Policy     config.Policy
	Log        *slog.Logger
	Now        func() time.Time // nil means time.Now
	Idle       time.Duration    // wait when there is nothing to do; 0 means 2s
	// OnBudgetHit runs the first time in a UTC day that triage stops for budget.
	OnBudgetHit func(ctx context.Context, spent, budget float64)

	budgetDay string
}

func (t *Triage) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func dayStart(now time.Time) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// Run triages until ctx ends.
func (t *Triage) Run(ctx context.Context) {
	idle := t.Idle
	if idle == 0 {
		idle = 2 * time.Second
	}
	for ctx.Err() == nil {
		did, err := t.Step(ctx)
		if err != nil {
			t.Log.Error("triage step", "err", err.Error())
		}
		if did && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(idle):
		}
	}
}

// Step triages at most one event and reports whether it did any work.
func (t *Triage) Step(ctx context.Context) (bool, error) {
	ctl, err := t.Store.Control(ctx)
	if err != nil || ctl.Paused {
		return false, err
	}
	now := t.now()
	start := dayStart(now)
	spent, err := t.Store.SpendSince(ctx, start)
	if err != nil {
		return false, err
	}
	if budget := t.Policy.Budgets.DailyUSD[config.TierHaiku]; spent >= budget {
		if day := start.Format(time.DateOnly); day != t.budgetDay {
			t.budgetDay = day
			t.Log.Warn("haiku daily budget reached; triage waits until 00:00 UTC",
				"spent_usd", spent, "budget_usd", budget)
			if t.OnBudgetHit != nil {
				t.OnBudgetHit(ctx, spent, budget)
			}
		}
		return false, nil
	}

	ev, err := t.Store.NextUntriaged(ctx, now)
	if err != nil || ev == nil {
		return false, err
	}
	v, err := triage.Decide(ctx, ev.Kind, ev.Repo, ev.Payload, t.Classifier, t.Policy)
	if err != nil {
		n, ferr := t.Store.FailAttempt(ctx, ev.ID, now.Add(retryAfter(ev.Attempts+1)))
		if ferr != nil {
			return false, ferr
		}
		if n < maxAttempts {
			t.Log.Warn("classifier failed; will retry", "event_id", ev.ID, "attempt", n, "err", err.Error())
			return true, nil
		}
		v = triage.Verdict{Stage: memory.StageHaiku, Outcome: memory.OutcomeFailed, Rationale: err.Error()}
	}

	d := memory.Decision{
		EventID: ev.ID, Stage: v.Stage, Outcome: v.Outcome, Category: v.Category,
		WouldDo: v.WouldDo, Permission: string(v.Permission), Target: v.Target,
		Confidence: v.Confidence, Rationale: v.Rationale, RawOutput: v.RawOutput,
		Model: v.Usage.Model, InputTokens: v.Usage.InputTokens, OutputTokens: v.Usage.OutputTokens,
		CostUSD: v.CostUSD, PostKind: postKind(v), CreatedAt: now,
	}
	id, err := t.Store.RecordDecision(ctx, d)
	if err != nil {
		return false, err
	}
	t.Log.Info("decision", "decision_id", id, "event_id", ev.ID, "kind", ev.Kind, "repo", ev.Repo,
		"stage", d.Stage, "outcome", d.Outcome, "category", d.Category, "would_do", d.WouldDo,
		"cost_usd", d.CostUSD)
	return true, nil
}

// postKind decides whether a decision gets a Slack post: an approval request
// for ask_first actions, a grade post for any other proposed action.
func postKind(v triage.Verdict) string {
	switch {
	case v.Outcome != memory.OutcomeOK || v.WouldDo == triage.WouldDoNone:
		return ""
	case v.Permission == config.AskFirst:
		return memory.PostApproval
	default:
		return memory.PostGrade
	}
}

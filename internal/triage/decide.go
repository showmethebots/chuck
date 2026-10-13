package triage

import (
	"context"
	"fmt"

	"github.com/orionsbelt-dev/chuck/internal/config"
	"github.com/orionsbelt-dev/chuck/internal/memory"
)

// Verdict is what triage concluded about one event. The daemon stores it as
// a decision; replay compares it with a stored one.
type Verdict struct {
	Stage      string
	Outcome    string
	Category   string
	WouldDo    string
	Permission config.Permission
	Target     string
	Confidence float64
	Rationale  string
	RawOutput  string
	Usage      Usage
	CostUSD    float64
}

// Decide runs rules, then the classifier, then validation. It returns an
// error only when the classifier call itself failed, which is worth
// retrying; every other failure is a triage_failed verdict.
func Decide(ctx context.Context, kind, repo string, payload []byte, c Classifier, p config.Policy) (Verdict, error) {
	if name, ok := MatchRule(kind); ok {
		return Verdict{Stage: memory.StageRule, Outcome: memory.OutcomeDropped, Rationale: name}, nil
	}
	v := Verdict{Stage: memory.StageHaiku}
	in, err := BuildInput(kind, repo, payload)
	if err != nil {
		v.Outcome, v.Rationale = memory.OutcomeFailed, "input: "+err.Error()
		return v, nil
	}
	raw, u, err := c.Classify(ctx, in)
	if err != nil {
		return v, fmt.Errorf("classify: %w", err)
	}
	cost, err := CostUSD(u)
	if err != nil {
		return v, err
	}
	v.RawOutput, v.Usage, v.CostUSD = raw, u, cost
	r, perm, err := ParseResult(raw, p)
	if err != nil {
		v.Outcome, v.Rationale = memory.OutcomeFailed, "output: "+err.Error()
		return v, nil
	}
	v.Outcome = memory.OutcomeOK
	v.Category, v.WouldDo, v.Permission = r.Category, r.WouldDo, perm
	v.Target, v.Confidence, v.Rationale = r.Target, r.Confidence, r.Rationale
	return v, nil
}

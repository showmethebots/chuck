package triage

import (
	"context"
	"errors"
	"testing"

	"github.com/orionsbelt-dev/chuck/internal/config"
	"github.com/orionsbelt-dev/chuck/internal/memory"
)

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

// stubClassifier returns raw (or err) and records the inputs it saw. model
// defaults to HaikuModel when unset, so existing callers that don't care
// about pricing need not set it.
type stubClassifier struct {
	raw    string
	model  string
	err    error
	calls  int
	inputs []Input
}

func (s *stubClassifier) Classify(_ context.Context, in Input) (string, Usage, error) {
	s.calls++
	s.inputs = append(s.inputs, in)
	if s.err != nil {
		return "", Usage{}, s.err
	}
	model := s.model
	if model == "" {
		model = HaikuModel
	}
	return s.raw, Usage{Model: model, InputTokens: 1000, OutputTokens: 100}, nil
}

func TestDecideDropsByRuleWithoutCallingModel(t *testing.T) {
	c := &stubClassifier{}
	v, err := Decide(context.Background(), "workflow_run.requested", "o/r", []byte(`{}`), c, testPolicy())
	if err != nil || v.Stage != memory.StageRule || v.Outcome != memory.OutcomeDropped || v.Rationale != "workflow_run_not_completed" {
		t.Fatalf("verdict = %+v, %v", v, err)
	}
	if c.calls != 0 {
		t.Fatal("classifier called for a rule-dropped event")
	}
}

func TestDecideOK(t *testing.T) {
	c := &stubClassifier{raw: `{"category":"new_issue","would_do":"comment_issue","target":"https://github.com/orionsbelt-dev/demos/issues/7","confidence":0.8,"rationale":"needs repro steps"}`}
	v, err := Decide(context.Background(), "issues.opened", "orionsbelt-dev/demos", fixture(t, "issues_opened.json"), c, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if v.Outcome != memory.OutcomeOK || v.WouldDo != "comment_issue" || v.Permission != config.ActFreely || v.CostUSD != 0.0015 || v.RawOutput != c.raw {
		t.Fatalf("verdict = %+v", v)
	}
	if c.inputs[0].Title != "Demo page 404s on refresh" {
		t.Errorf("classifier saw %+v", c.inputs[0])
	}
}

func TestDecideInvalidOutputIsFailedWithCost(t *testing.T) {
	c := &stubClassifier{raw: `{"category":"noise","would_do":"delete_repo","target":"https://github.com/o/r","confidence":0.5,"rationale":"x"}`}
	v, err := Decide(context.Background(), "issues.opened", "o/r", fixture(t, "issues_opened.json"), c, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if v.Outcome != memory.OutcomeFailed || v.RawOutput != c.raw || v.CostUSD == 0 || v.WouldDo != "" {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestDecideBadPayloadIsFailed(t *testing.T) {
	v, err := Decide(context.Background(), "issues.opened", "o/r", []byte("nope"), &stubClassifier{}, testPolicy())
	if err != nil || v.Outcome != memory.OutcomeFailed {
		t.Fatalf("verdict = %+v, %v", v, err)
	}
}

func TestDecideUnpricedModelIsFailedWithOutputPreserved(t *testing.T) {
	c := &stubClassifier{
		raw:   `{"category":"new_issue","would_do":"comment_issue","target":"https://github.com/orionsbelt-dev/demos/issues/7","confidence":0.8,"rationale":"needs repro steps"}`,
		model: "claude-unknown",
	}
	v, err := Decide(context.Background(), "issues.opened", "orionsbelt-dev/demos", fixture(t, "issues_opened.json"), c, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if v.Outcome != memory.OutcomeFailed {
		t.Fatalf("outcome = %q, want %q", v.Outcome, memory.OutcomeFailed)
	}
	if v.RawOutput != c.raw {
		t.Errorf("RawOutput = %q, want the paid call's output preserved", v.RawOutput)
	}
	if v.Usage.Model != "claude-unknown" || v.Usage.InputTokens != 1000 || v.Usage.OutputTokens != 100 {
		t.Errorf("Usage = %+v, want token counts preserved", v.Usage)
	}
	if v.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want 0 since pricing failed", v.CostUSD)
	}
}

func TestDecideClassifierErrorIsReturned(t *testing.T) {
	_, err := Decide(context.Background(), "issues.opened", "o/r", fixture(t, "issues_opened.json"), &stubClassifier{err: errors.New("529 overloaded")}, testPolicy())
	if err == nil {
		t.Fatal("classifier error swallowed")
	}
}

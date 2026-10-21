package triage

import (
	"context"
	"strings"
	"testing"

	"github.com/orionsbelt-dev/chuck/internal/memory"
)

func gradedIssue(t *testing.T, id int64, raw, grade string) memory.Graded {
	return memory.Graded{
		Event: memory.Event{ID: id, Source: memory.SourceGitHub, Kind: "issues.opened", Repo: "orionsbelt-dev/demos", Payload: fixture(t, "issues_opened.json")},
		Decision: memory.Decision{ID: id, Stage: memory.StageHaiku, Outcome: memory.OutcomeOK, Category: "new_issue",
			Task: "implement_issue", WouldDo: None, RawOutput: raw},
		Grade: grade,
	}
}

func TestReplayAgreementAndDivergence(t *testing.T) {
	items := []memory.Graded{
		gradedIssue(t, 1, implementRaw, "up"),
		gradedIssue(t, 2, implementRaw, "down"),
		{Event: memory.Event{ID: 3, Kind: "ping", Payload: []byte(`{}`)},
			Decision: memory.Decision{ID: 3, Stage: memory.StageRule, Outcome: memory.OutcomeDropped, Rationale: "github_app_housekeeping"},
			Grade:    "up"},
	}
	// The model now says "none" for issues: the 👍 issue disagrees, the 👎 one diverges.
	c := &stubClassifier{raw: `{"category":"new_issue","task":"none","would_do":"none","target":"","ask":"","confidence":0.6,"rationale":"r"}`}
	r := Replay(context.Background(), items, c, testMenu(), 0)
	if r.Items != 3 || r.Errors != 0 {
		t.Fatalf("items %d errors %d", r.Items, r.Errors)
	}
	issue := r.Rows["new_issue"]
	if issue == nil || issue.Up != 1 || issue.UpAgree != 0 || issue.Down != 1 || issue.DownDiverge != 1 {
		t.Fatalf("new_issue row = %+v", issue)
	}
	if rule := r.Rows[memory.OutcomeDropped]; rule == nil || rule.Up != 1 || rule.UpAgree != 1 {
		t.Fatalf("dropped row = %+v", rule)
	}
	out := r.String()
	if !strings.Contains(out, "👍 agreement: 50% (1/2)") || !strings.Contains(out, "👎 divergence: 100% (1/1)") {
		t.Fatalf("report:\n%s", out)
	}
}

func TestEchoClassifierReproducesStoredDecisions(t *testing.T) {
	items := []memory.Graded{gradedIssue(t, 1, implementRaw, "up")}
	r := Replay(context.Background(), items, NewEchoClassifier(items), testMenu(), 0)
	if !strings.Contains(r.String(), "👍 agreement: 100% (1/1)") {
		t.Fatalf("report:\n%s", r.String())
	}
}

// Each stub call costs 0.0015 (1000 input / 100 output tokens at Haiku
// pricing, as in TestDecideOK). A $0.002 cap should let two calls through
// (cumulative cost 0.0015, then 0.0030) and stop before the third.
func TestReplayStopsAtMaxUSD(t *testing.T) {
	items := []memory.Graded{
		gradedIssue(t, 1, implementRaw, "up"),
		gradedIssue(t, 2, implementRaw, "up"),
		gradedIssue(t, 3, implementRaw, "up"),
	}
	c := &stubClassifier{raw: implementRaw}
	r := Replay(context.Background(), items, c, testMenu(), 0.002)
	if r.Items != 2 || !r.Stopped {
		t.Fatalf("items %d stopped %v", r.Items, r.Stopped)
	}
	if c.calls != 2 {
		t.Fatalf("classifier called %d times, want 2", c.calls)
	}
	if !strings.Contains(r.String(), "stopped early") {
		t.Fatalf("report:\n%s", r.String())
	}
}

func TestSameVerdictComparesTask(t *testing.T) {
	d := memory.Decision{Outcome: memory.OutcomeOK, Category: "new_issue", Task: "implement_issue", WouldDo: None}
	if !sameVerdict(d, Verdict{Outcome: memory.OutcomeOK, Category: "new_issue", Task: "implement_issue", WouldDo: None}) {
		t.Fatal("identical verdict reported as different")
	}
	if sameVerdict(d, Verdict{Outcome: memory.OutcomeOK, Category: "new_issue", Task: "address_review", WouldDo: None}) {
		t.Fatal("different task reported as the same verdict")
	}
}

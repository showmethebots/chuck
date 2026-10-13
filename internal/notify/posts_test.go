package notify

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/orionsbelt-dev/chuck/internal/memory"
)

// render encodes blocks without HTML escaping, so "<@U…>" mentions appear
// as Slack will receive them rather than escaped. SetEscapeHTML(false) alone
// is not enough: slack-go's ContextElements has its own MarshalJSON that
// calls the package-level json.Marshal (always HTML-escaped) rather than
// honoring the encoder it was invoked from, so a context block's text (used
// for "Approved by <@…>" and grade status lines) stays escaped regardless.
// Undo that escaping afterward; Slack receives the identical value either
// way, since a JSON unicode escape and the literal character decode to the
// same string.
func render(t *testing.T, v memory.DecisionView) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(DecisionBlocks(v)); err != nil {
		t.Fatal(err)
	}
	unescape := strings.NewReplacer("\\u003c", "<", "\\u003e", ">", "\\u0026", "&")
	return unescape.Replace(buf.String())
}

func view(d memory.Decision) memory.DecisionView {
	d.ID = 42
	if d.Outcome == "" {
		d.Outcome = memory.OutcomeOK
	}
	return memory.DecisionView{Decision: d, Repo: "orionsbelt-dev/demos", Kind: "pull_request.opened"}
}

func TestApprovalPostOpen(t *testing.T) {
	v := view(memory.Decision{WouldDo: "merge", Permission: "ask_first", Target: "https://github.com/orionsbelt-dev/demos/pull/8",
		Category: "pr_lifecycle", Confidence: 0.8, Rationale: "approved and green", PostKind: memory.PostApproval})
	got := render(t, v)
	for _, want := range []string{
		"SHADOW — nothing will execute", "`merge`", "pull/8", "approved and green", "orionsbelt-dev/demos",
		`"action_id":"approve"`, `"action_id":"deny"`, `"action_id":"grade_up"`, `"action_id":"grade_down"`, `"value":"42"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("blocks lack %q:\n%s", want, got)
		}
	}
	if !strings.Contains(DecisionText(v), "SHADOW") {
		t.Errorf("fallback text = %q", DecisionText(v))
	}
}

func TestApprovalPostAnswered(t *testing.T) {
	v := view(memory.Decision{WouldDo: "merge", Permission: "ask_first", Target: "https://github.com/o/r/pull/8", PostKind: memory.PostApproval})
	v.ApprovalResponse, v.ApprovalBy = "approve", "UMEMBER001"
	got := render(t, v)
	if strings.Contains(got, `"action_id":"approve"`) || strings.Contains(got, `"action_id":"deny"`) {
		t.Error("answered approval still offers approve/deny")
	}
	if !strings.Contains(got, "Approved by <@UMEMBER001>") {
		t.Errorf("no answer line:\n%s", got)
	}
	if !strings.Contains(got, `"action_id":"grade_up"`) {
		t.Error("grade buttons removed after approval")
	}
}

func TestGradePostShowsGrade(t *testing.T) {
	v := view(memory.Decision{WouldDo: "comment_issue", Permission: "act_freely", Target: "https://github.com/o/r/issues/7", PostKind: memory.PostGrade})
	v.Grade = "up"
	got := render(t, v)
	if !strings.Contains(got, "Graded 👍") || strings.Contains(got, "SHADOW") || strings.Contains(got, `"action_id":"approve"`) {
		t.Errorf("grade post:\n%s", got)
	}
}

func TestQuietPosts(t *testing.T) {
	dropped := render(t, view(memory.Decision{Stage: memory.StageRule, Outcome: memory.OutcomeDropped, Rationale: "github_app_housekeeping", PostKind: memory.PostGrade}))
	if !strings.Contains(dropped, "Dropped by rule `github_app_housekeeping`") {
		t.Errorf("dropped:\n%s", dropped)
	}
	none := render(t, view(memory.Decision{WouldDo: "none", Category: "noise", PostKind: memory.PostGrade}))
	if !strings.Contains(none, "chuck would do nothing") {
		t.Errorf("none:\n%s", none)
	}
}

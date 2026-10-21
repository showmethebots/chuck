package notify

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/slack-go/slack"

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
		Category: "pr_lifecycle", Confidence: 0.8, Rationale: "approved and green", Ask: "approved and CI green", PostKind: memory.PostApproval})
	got := render(t, v)
	for _, want := range []string{
		"SHADOW — nothing will execute", "`merge`", "pull/8", "approved and green", "orionsbelt-dev/demos",
		"*Ask:* approved and CI green", "*Why:* approved and green",
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

func TestTaskPostLeadsWithTaskAndAsk(t *testing.T) {
	v := view(memory.Decision{Task: "address_review", WouldDo: "none", Target: "https://github.com/orionsbelt-dev/demos/pull/1",
		Ask: "reviewer says the backoff never resets", Rationale: "concrete bug with a suggested fix", PostKind: memory.PostGrade})
	got := render(t, v)
	for _, want := range []string{
		"chuck would hand this to a worker: `address_review`", "pull/1",
		"*Ask:* reviewer says the backoff never resets", "*Why:* concrete bug with a suggested fix",
		`"action_id":"grade_up"`, `"action_id":"grade_down"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("blocks lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "SHADOW") {
		t.Errorf("task post shows SHADOW banner:\n%s", got)
	}
	if !strings.Contains(DecisionText(v), "hand this to a worker") {
		t.Errorf("fallback text = %q", DecisionText(v))
	}
}

func TestPreChangeGradePostStillRenders(t *testing.T) {
	v := view(memory.Decision{Task: "", WouldDo: "comment_issue", Permission: "act_freely", PostKind: memory.PostGrade})
	v.Grade = "up"
	got := render(t, v)
	if !strings.Contains(got, "chuck would `comment_issue`") {
		t.Errorf("pre-change post lost its old headline:\n%s", got)
	}
	if !strings.Contains(got, "Graded 👍") {
		t.Errorf("pre-change post lost its grade:\n%s", got)
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
	none := render(t, view(memory.Decision{Task: "none", WouldDo: "none", Category: "noise", PostKind: memory.PostGrade}))
	if !strings.Contains(none, "chuck would do nothing") {
		t.Errorf("none:\n%s", none)
	}
	if strings.Contains(none, "*Ask:*") {
		t.Errorf("quiet post with no ask shows an Ask line:\n%s", none)
	}
}

func TestQuietPostsLinkToEvent(t *testing.T) {
	const url = "https://github.com/orionsbelt-dev/demos/issues/7"
	for name, d := range map[string]memory.Decision{
		"none":    {WouldDo: "none", Category: "noise", PostKind: memory.PostGrade},
		"dropped": {Stage: memory.StageRule, Outcome: memory.OutcomeDropped, Rationale: "github_app_housekeeping", PostKind: memory.PostGrade},
	} {
		v := view(d)
		v.URL = url
		if got := render(t, v); !strings.Contains(got, "<"+url+">") {
			t.Errorf("%s: blocks lack a link to the event:\n%s", name, got)
		}
		if got := DecisionText(v); !strings.Contains(got, url) {
			t.Errorf("%s: fallback text lacks the link: %q", name, got)
		}
	}
	if got := render(t, view(memory.Decision{WouldDo: "none", PostKind: memory.PostGrade})); strings.Contains(got, "<>") {
		t.Errorf("empty URL rendered as a link:\n%s", got)
	}
}

func TestPostBoundsLongFields(t *testing.T) {
	long := strings.Repeat("é", 5000)
	v := view(memory.Decision{WouldDo: "comment_issue", Permission: "act_freely", Target: "https://github.com/o/r/issues/7?" + long,
		Category: "new_issue", Confidence: 0.8, Rationale: long, Ask: long, PostKind: memory.PostApproval})
	v.URL = "https://github.com/o/r/issues/7#" + long
	for _, b := range DecisionBlocks(v) {
		switch b := b.(type) {
		case *slack.SectionBlock:
			if n := utf8.RuneCountInString(b.Text.Text); n > 3000 {
				t.Errorf("section text is %d chars, Slack's limit is 3000", n)
			}
		case *slack.ContextBlock:
			for _, e := range b.ContextElements.Elements {
				if txt, ok := e.(*slack.TextBlockObject); ok {
					if n := utf8.RuneCountInString(txt.Text); n > 2000 {
						t.Errorf("context text is %d chars, Slack's limit is 2000", n)
					}
				}
			}
		}
	}
	text := DecisionText(v)
	if n := utf8.RuneCountInString(text); n > 1500 || !utf8.ValidString(text) || !strings.Contains(text, "…") {
		t.Errorf("fallback text is %d runes, valid UTF-8 %v: %.80q", n, utf8.ValidString(text), text)
	}
	if got := render(t, v); !strings.Contains(got, strings.Repeat("é", 499)+"…") {
		t.Error("rationale not cut to 500 runes with an ellipsis")
	}
}

// Model output and payload text are untrusted: a prompt injection mustn't be
// able to ping the channel or hide a link's real URL behind a GitHub label.
func TestPostEscapesUntrustedText(t *testing.T) {
	const evil = "https://evil.example|github.com/orionsbelt-dev/chuck/pull/1"
	v := view(memory.Decision{WouldDo: "comment_issue", Permission: "act_freely",
		Target: "https://github.com/o/r/issues/7 <" + evil + ">", Category: "new_issue",
		Rationale: "urgent <!channel> <!here> & <" + evil + ">",
		Ask:       "urgent <!channel> <!here> & <" + evil + ">", PostKind: memory.PostApproval})
	v.URL = "https://github.com/o/r/issues/7><!channel"
	for name, got := range map[string]string{"blocks": render(t, v), "text": DecisionText(v)} {
		for _, bad := range []string{"<!channel>", "<!here>", "<" + evil + ">", "><!channel"} {
			if strings.Contains(got, bad) {
				t.Errorf("%s contains %q unescaped:\n%s", name, bad, got)
			}
		}
	}
	if got := render(t, v); !strings.Contains(got, "&lt;!channel&gt;") || !strings.Contains(got, " &amp; ") {
		t.Errorf("rationale not escaped:\n%s", got)
	}
}

// TestAskRendersAsOneLine guards against model text that embeds a fake
// "*Why:*" line inside Ask, which would otherwise read as a second,
// attacker-controlled field in the post.
func TestAskRendersAsOneLine(t *testing.T) {
	v := view(memory.Decision{Ask: "reviewer says fix this\n*Why:* fake", PostKind: memory.PostGrade})
	blocks := DecisionBlocks(v)
	var askBlock *slack.SectionBlock
	for _, b := range blocks {
		if sb, ok := b.(*slack.SectionBlock); ok && sb.Text != nil && strings.Contains(sb.Text.Text, "*Ask:*") {
			askBlock = sb
		}
	}
	if askBlock == nil {
		t.Fatal("no block with *Ask:* found")
	}
	lines := strings.Split(askBlock.Text.Text, "\n")
	if len(lines) != 1 {
		t.Fatalf("Ask block has %d lines, want 1: %q", len(lines), askBlock.Text.Text)
	}
	if !strings.Contains(lines[0], "*Ask:* reviewer says fix this *Why:* fake") {
		t.Errorf("Ask line = %q, want embedded newline replaced by a space", lines[0])
	}
}

func TestFieldNeverSplitsAnEntity(t *testing.T) {
	if got := field(strings.Repeat("&", 10), 12); got != "&amp;&amp;…" {
		t.Errorf("field = %q, want two whole entities and a cut mark", got)
	}
	if n := utf8.RuneCountInString(field(strings.Repeat("&", 5000), maxRationale)); n > maxRationale+1 {
		t.Errorf("escaped field is %d runes, want at most %d", n, maxRationale+1)
	}
	if got := field("a<b", 10); got != "a&lt;b" {
		t.Errorf("field = %q, want a&lt;b", got)
	}
}

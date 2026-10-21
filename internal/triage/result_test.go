package triage

import (
	"testing"

	"github.com/orionsbelt-dev/chuck/internal/config"
)

func TestParseResultAccepts(t *testing.T) {
	r, perm, err := ParseResult(`{"category":"review_feedback","task":"address_review","would_do":"none","target":"https://github.com/o/r/pull/5","ask":"reviewer says X","confidence":0.8,"rationale":"r"}`, testMenu())
	if err != nil {
		t.Fatal(err)
	}
	if r.Task != "address_review" || r.WouldDo != None || r.Ask != "reviewer says X" || perm != "" || r.Confidence != 0.8 {
		t.Fatalf("task: %+v, %q", r, perm)
	}
	r, perm, err = ParseResult(`{"category":"pr_lifecycle","task":"none","would_do":"merge","target":"https://github.com/o/r/pull/8","ask":"merge the approved PR","confidence":0.7,"rationale":"approved and green"}`, testMenu())
	if err != nil {
		t.Fatal(err)
	}
	if r.Task != None || r.WouldDo != "merge" || perm != config.AskFirst {
		t.Fatalf("merge: %+v, %q", r, perm)
	}
	r, perm, err = ParseResult(`{"category":"noise","task":"none","would_do":"none","target":"","ask":"","confidence":0.95,"rationale":"bot chatter"}`, testMenu())
	if err != nil || r.Task != None || r.WouldDo != None || perm != "" {
		t.Fatalf("none: %+v, %q, %v", r, perm, err)
	}
}

// Each case is valid except for the one thing its name says, so a missing
// check can't hide behind another one.
func TestParseResultRejects(t *testing.T) {
	cases := map[string]string{
		"empty":                           ``,
		"malformed JSON":                  `{"category":`,
		"unknown field":                   `{"category":"noise","task":"none","would_do":"none","target":"","ask":"","confidence":0.5,"rationale":"x","extra":1}`,
		"unknown category":                `{"category":"chores","task":"none","would_do":"none","target":"","ask":"","confidence":0.5,"rationale":"x"}`,
		"undeclared action":               `{"category":"noise","task":"none","would_do":"delete_repo","target":"https://github.com/o/r","ask":"x","confidence":0.5,"rationale":"x"}`,
		"read is not proposable":          `{"category":"noise","task":"none","would_do":"read","target":"https://github.com/o/r","ask":"x","confidence":0.5,"rationale":"x"}`,
		"triage is not proposable":        `{"category":"noise","task":"none","would_do":"triage","target":"https://github.com/o/r","ask":"x","confidence":0.5,"rationale":"x"}`,
		"comment_issue is not proposable": `{"category":"new_issue","task":"none","would_do":"comment_issue","target":"https://github.com/o/r/issues/7","ask":"x","confidence":0.5,"rationale":"x"}`,
		"internal task":                   `{"category":"noise","task":"triage","would_do":"none","target":"https://github.com/o/r","ask":"x","confidence":0.5,"rationale":"x"}`,
		"unknown task":                    `{"category":"new_issue","task":"refactor","would_do":"none","target":"https://github.com/o/r/issues/7","ask":"x","confidence":0.5,"rationale":"x"}`,
		"task and action both set":        `{"category":"review_feedback","task":"address_review","would_do":"merge","target":"https://github.com/o/r/pull/5","ask":"x","confidence":0.5,"rationale":"x"}`,
		"task without ask":                `{"category":"review_feedback","task":"address_review","would_do":"none","target":"https://github.com/o/r/pull/5","ask":"","confidence":0.5,"rationale":"x"}`,
		"action without ask":              `{"category":"pr_lifecycle","task":"none","would_do":"merge","target":"https://github.com/o/r/pull/8","ask":"","confidence":0.5,"rationale":"x"}`,
		"task without target":             `{"category":"review_feedback","task":"address_review","would_do":"none","target":"","ask":"x","confidence":0.5,"rationale":"x"}`,
		"action without target":           `{"category":"pr_lifecycle","task":"none","would_do":"merge","target":"","ask":"x","confidence":0.5,"rationale":"x"}`,
		"target not on github.com":        `{"category":"new_issue","task":"none","would_do":"merge","target":"https://evil.example/o/r/pull/1","ask":"x","confidence":0.5,"rationale":"x"}`,
		"lookalike host":                  `{"category":"new_issue","task":"none","would_do":"merge","target":"https://github.com.evil.example/o/r","ask":"x","confidence":0.5,"rationale":"x"}`,
		"confidence above 1":              `{"category":"noise","task":"none","would_do":"none","target":"","ask":"","confidence":1.5,"rationale":"x"}`,
		"confidence below 0":              `{"category":"noise","task":"none","would_do":"none","target":"","ask":"","confidence":-0.1,"rationale":"x"}`,
		"missing task field":              `{"category":"noise","would_do":"none","target":"","confidence":0.5,"rationale":"x"}`,
	}
	for name, raw := range cases {
		if _, _, err := ParseResult(raw, testMenu()); err == nil {
			t.Errorf("%s: accepted %s", name, raw)
		}
	}
}

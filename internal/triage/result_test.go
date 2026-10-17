package triage

import (
	"strings"
	"testing"

	"github.com/orionsbelt-dev/chuck/internal/config"
)

func TestProposableActionsExcludeReadAndTriage(t *testing.T) {
	got := ProposableActions(testPolicy())
	want := []string{"comment_issue", "create_issue", "merge"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ProposableActions = %v, want %v", got, want)
	}
}

func TestParseResultAccepts(t *testing.T) {
	r, perm, err := ParseResult(`{"category":"pr_lifecycle","would_do":"merge","target":"https://github.com/o/r/pull/8","confidence":0.7,"rationale":"approved and green"}`, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if r.WouldDo != "merge" || perm != config.AskFirst || r.Confidence != 0.7 {
		t.Fatalf("got %+v, %q", r, perm)
	}
	r, perm, err = ParseResult(`{"category":"noise","would_do":"none","target":"","confidence":0.95,"rationale":"bot chatter"}`, testPolicy())
	if err != nil || r.WouldDo != WouldDoNone || perm != "" {
		t.Fatalf("none: %+v, %q, %v", r, perm, err)
	}
}

func TestParseResultRejects(t *testing.T) {
	cases := map[string]string{
		"empty":                    ``,
		"malformed JSON":           `{"category":`,
		"unknown field":            `{"category":"noise","would_do":"none","target":"","confidence":0.5,"rationale":"x","extra":1}`,
		"unknown category":         `{"category":"chores","would_do":"none","target":"","confidence":0.5,"rationale":"x"}`,
		"undeclared action":        `{"category":"noise","would_do":"delete_repo","target":"https://github.com/o/r","confidence":0.5,"rationale":"x"}`,
		"read is not proposable":   `{"category":"noise","would_do":"read","target":"https://github.com/o/r","confidence":0.5,"rationale":"x"}`,
		"triage is not proposable": `{"category":"noise","would_do":"triage","target":"https://github.com/o/r","confidence":0.5,"rationale":"x"}`,
		"missing target":           `{"category":"new_issue","would_do":"comment_issue","target":"","confidence":0.5,"rationale":"x"}`,
		"target not on github.com": `{"category":"new_issue","would_do":"merge","target":"https://evil.example/o/r/pull/1","confidence":0.5,"rationale":"x"}`,
		"lookalike host":           `{"category":"new_issue","would_do":"merge","target":"https://github.com.evil.example/o/r","confidence":0.5,"rationale":"x"}`,
		"confidence above 1":       `{"category":"noise","would_do":"none","target":"","confidence":1.5,"rationale":"x"}`,
		"confidence below 0":       `{"category":"noise","would_do":"none","target":"","confidence":-0.1,"rationale":"x"}`,
	}
	for name, raw := range cases {
		if _, _, err := ParseResult(raw, testPolicy()); err == nil {
			t.Errorf("%s: accepted %s", name, raw)
		}
	}
}

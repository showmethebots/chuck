// Package triage decides what chuck would do about an event: deterministic
// noise rules first, then Haiku for everything else, with the model's output
// validated against policy.yaml before anything trusts it.
package triage

import (
	"slices"
	"strings"
)

// rule is a named noise filter over the event kind. A match is recorded as
// dropped_by_rule without calling Haiku, and the name goes in the decision's
// rationale so the rules themselves can be graded.
type rule struct {
	name  string
	match func(kind string) bool
}

// rules run in order; the first match names the decision. Phase 2's router
// moves rules like these into YAML.
var rules = []rule{
	{"workflow_run_not_completed", func(k string) bool {
		return strings.HasPrefix(k, "workflow_run.") && k != "workflow_run.completed"
	}},
	{"comment_edited_or_deleted", func(k string) bool {
		return slices.Contains([]string{
			"issue_comment.edited", "issue_comment.deleted",
			"pull_request_review_comment.edited", "pull_request_review_comment.deleted",
		}, k)
	}},
	{"pull_request_housekeeping", func(k string) bool {
		return slices.Contains([]string{
			"pull_request.labeled", "pull_request.unlabeled", "pull_request.assigned",
			"pull_request.unassigned", "pull_request.review_request_removed", "pull_request.edited",
		}, k)
	}},
	{"github_app_housekeeping", func(k string) bool {
		return k == "ping" ||
			k == "installation" || strings.HasPrefix(k, "installation.") ||
			k == "installation_repositories" || strings.HasPrefix(k, "installation_repositories.")
	}},
}

// MatchRule returns the name of the first noise rule matching kind.
func MatchRule(kind string) (string, bool) {
	for _, r := range rules {
		if r.match(kind) {
			return r.name, true
		}
	}
	return "", false
}

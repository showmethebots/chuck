package triage

import "testing"

func TestMatchRule(t *testing.T) {
	cases := []struct {
		kind string
		rule string // "" means no rule matches
	}{
		{"workflow_run.requested", "workflow_run_not_completed"},
		{"workflow_run.in_progress", "workflow_run_not_completed"},
		{"workflow_run.completed", ""},
		{"issue_comment.edited", "comment_edited_or_deleted"},
		{"issue_comment.deleted", "comment_edited_or_deleted"},
		{"pull_request_review_comment.edited", "comment_edited_or_deleted"},
		{"issue_comment.created", ""},
		{"pull_request_review_comment.created", ""},
		{"pull_request.labeled", "pull_request_housekeeping"},
		{"pull_request.unlabeled", "pull_request_housekeeping"},
		{"pull_request.assigned", "pull_request_housekeeping"},
		{"pull_request.unassigned", "pull_request_housekeeping"},
		{"pull_request.review_request_removed", "pull_request_housekeeping"},
		{"pull_request.edited", "pull_request_housekeeping"},
		{"pull_request.opened", ""},
		{"pull_request.synchronize", ""},
		{"pull_request.closed", ""},
		{"ping", "github_app_housekeeping"},
		{"installation.created", "github_app_housekeeping"},
		{"installation_repositories.added", "github_app_housekeeping"},
		{"issues.opened", ""},
		{"push", ""},
	}
	for _, c := range cases {
		got, ok := MatchRule(c.kind)
		if ok != (c.rule != "") || got != c.rule {
			t.Errorf("MatchRule(%q) = %q, %v; want %q", c.kind, got, ok, c.rule)
		}
	}
}

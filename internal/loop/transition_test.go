package loop

import (
	"fmt"
	"go/build"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/showmethebots/chuck/internal/config"
)

const (
	human   = int64(40615752)
	app     = int64(9000)
	head    = "1111111111111111111111111111111111111111"
	newHead = "2222222222222222222222222222222222222222"
	merge   = "3333333333333333333333333333333333333333"
)

var now = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

var repo = Repo{FullName: "showmethebots/demos", AppUserID: app, Humans: []int64{human}}

func loopCfg(mods ...func(*config.Loop)) *config.Loop {
	c := &config.Loop{
		DefaultBranch: "main", WIPLimit: 1, MergeMethod: "SQUASH",
		AutoAdvance: true, AutoMerge: true, AutoPromote: true, Ratchet: true,
		Limits:    config.Limits{BuildAttempts: 2, ReviewRounds: 3, StaleHours: 6},
		RiskPaths: []string{"db/migrations/**"},
		Deploy:    config.Deploy{DevURL: "https://dev.example.com", DevWorkflow: "deploy-dev", ProdWorkflow: "deploy-prod"},
		Validate:  config.ValidateConfig{E2ECommand: "npx playwright test", Smoke: []string{"home"}},
	}
	for _, m := range mods {
		m(c)
	}
	return c
}

func issue(n int, s State) *Item {
	return &Item{ID: int64(n), Kind: KindIssue, Number: n, State: s, StateSince: now.Add(-time.Hour)}
}

func pr(n, issueN int, s State) *Item {
	return &Item{ID: int64(100 + n), Kind: KindPR, Number: n, IssueNumber: issueN, State: s, StateSince: now.Add(-time.Hour)}
}

func openPR() *PRFacts {
	return &PRFacts{Number: 13, Open: true, HeadSHA: head, HeadRef: "loop/issue-12", AuthorID: app, ClosesIssue: true,
		ChangedFiles: []string{"src/app.ts"}}
}

func mergedPR() *PRFacts {
	p := openPR()
	p.Open, p.Merged, p.MergeSHA = false, true, merge
	return p
}

func webhook(event, action string, mod func(*WebhookInput)) Input {
	w := &WebhookInput{Event: event, Action: action, SenderID: human}
	if mod != nil {
		mod(w)
	}
	return Input{Kind: InputWebhook, Webhook: w}
}

func verdict(stage config.Stage, v *Verdict) Input {
	return Input{Kind: InputVerdict, Verdict: &VerdictInput{RunID: 1, Stage: stage, Attempt: 1, Verdict: v},
		Facts: Facts{OpenRun: &OpenRun{ID: 1, Stage: stage, Attempt: 1}}}
}

func dash(a DashboardAction, mod func(*DashboardInput)) Input {
	d := &DashboardInput{Action: a, Actor: "cwinters8", ActorID: human}
	if mod != nil {
		mod(d)
	}
	return Input{Kind: InputDashboard, Action: d}
}

// withFacts sets the facts. A verdict keeps its open run unless f names
// another.
func withFacts(in Input, f Facts) Input {
	if f.OpenRun == nil {
		f.OpenRun = in.Facts.OpenRun
	}
	in.Facts = f
	return in
}

func sweepReady(findings ...Finding) *Verdict {
	return &Verdict{Stage: config.StageSweep, Outcome: "ready", Sweep: &SweepVerdict{Outcome: "ready", HeadSHA: head, Findings: findings}}
}

func validatePass() *Verdict {
	return &Verdict{Stage: config.StageValidate, Outcome: "pass", Validate: &ValidateVerdict{
		Outcome: "pass", TestedSHA: merge,
		Criteria:    []Check{{ID: "AC-1", Result: "pass"}, {ID: "AC-2", Result: "pass"}},
		Regressions: []Check{{Name: "home", Result: "pass"}, {Name: "e2e", Result: "pass"}},
	}}
}

// moves renders a result's changes as "kind#n:to" for comparison.
func moves(r Result) []string {
	var out []string
	for _, c := range r.Changes {
		out = append(out, fmt.Sprintf("%s#%d:%s", c.Item.Kind, c.Item.Number, c.To))
	}
	return out
}

func kinds(r Result) []string {
	var out []string
	for _, e := range r.Effects {
		if e.Kind != EffectStateLabel && e.Kind != EffectMention {
			s := string(e.Kind)
			if e.Stage != "" {
				s += ":" + string(e.Stage)
			}
			if e.Status != "" {
				s += ":" + e.Status
			}
			out = append(out, s)
		}
	}
	return out
}

type row struct {
	name    string
	item    *Item
	in      Input
	cfg     *config.Loop
	moves   []string // nil means no change
	effects []string // non-label, non-mention effects, in order
	reason  string   // substring
}

func (r row) run(t *testing.T) Result {
	t.Helper()
	cfg := r.cfg
	if cfg == nil {
		cfg = loopCfg()
	}
	res := Transition(r.item, repo, cfg, r.in, now)
	if got := moves(res); !slices.Equal(got, r.moves) {
		t.Errorf("%s: moves = %v, want %v (reason %q)", r.name, got, r.moves, res.Reason)
	}
	if got := kinds(res); !slices.Equal(got, r.effects) {
		t.Errorf("%s: effects = %v, want %v", r.name, got, r.effects)
	}
	if r.reason != "" && !strings.Contains(res.Reason, r.reason) && !anyReason(res, r.reason) {
		t.Errorf("%s: reason %q lacks %q", r.name, res.Reason, r.reason)
	}
	return res
}

func anyReason(res Result, s string) bool {
	for _, c := range res.Changes {
		if strings.Contains(c.Reason, s) {
			return true
		}
	}
	return false
}

func runRows(t *testing.T, rows []row) {
	for _, r := range rows {
		r.run(t)
	}
}

// One test per row of the spec's transition table, in table order.
func TestTransitionTable(t *testing.T) {
	ac := Facts{IssueCriteria: []string{"AC-1", "AC-2"}}
	runRows(t, []row{
		{name: "intake label by a human", in: webhook("issues", "labeled", func(w *WebhookInput) { w.Label = LabelIntake; w.Number = 12 }),
			moves: []string{"issue#12:intake"}, effects: []string{"launch:intake"}},
		{name: "dashboard start intake", in: dash(ActionStartIntake, func(d *DashboardInput) { d.Number = 12 }),
			moves: []string{"issue#12:intake"}, effects: []string{"launch:intake"}},
		{name: "intake on an item already in the loop", item: issue(12, StateReady),
			in: webhook("issues", "labeled", func(w *WebhookInput) { w.Label = LabelIntake; w.Number = 12 }), reason: "already in the loop"},

		{name: "intake ready with criteria", item: issue(12, StateIntake),
			in:    withFacts(verdict(config.StageIntake, &Verdict{Intake: &IntakeVerdict{Outcome: "ready_for_approval", CriteriaIDs: []string{"AC-1"}}}), ac),
			moves: []string{"issue#12:awaiting-approval"}},
		{name: "intake ready with sub-issues", item: issue(12, StateIntake),
			in:    withFacts(verdict(config.StageIntake, &Verdict{Intake: &IntakeVerdict{Outcome: "ready_for_approval", SubIssues: []int{20, 21}}}), Facts{SubIssues: []int{20, 21}}),
			moves: []string{"issue#12:awaiting-approval", "issue#20:awaiting-approval", "issue#21:awaiting-approval"}},
		{name: "intake ready, sub-issue GitHub does not list", item: issue(12, StateIntake),
			in:    withFacts(verdict(config.StageIntake, &Verdict{Intake: &IntakeVerdict{Outcome: "ready_for_approval", SubIssues: []int{20, 3}}}), Facts{SubIssues: []int{20}}),
			moves: []string{"issue#12:needs-human"}, reason: "does not list under this issue: #3"},
		{name: "intake ready, sub-issue already in the loop", item: issue(12, StateIntake),
			in:    withFacts(verdict(config.StageIntake, &Verdict{Intake: &IntakeVerdict{Outcome: "ready_for_approval", SubIssues: []int{20, 21}}}), Facts{SubIssuesKnown: []int{20}, SubIssues: []int{20, 21}}),
			moves: []string{"issue#12:awaiting-approval", "issue#21:awaiting-approval"}},
		{name: "intake needs clarification", item: issue(12, StateIntake),
			in:    verdict(config.StageIntake, &Verdict{Intake: &IntakeVerdict{Outcome: "needs_clarification", Questions: []string{"Which page?"}}}),
			moves: []string{"issue#12:needs-human"}, reason: "Which page?"},
		{name: "intake ready but no criteria in the issue", item: issue(12, StateIntake),
			in:    verdict(config.StageIntake, &Verdict{Intake: &IntakeVerdict{Outcome: "ready_for_approval", CriteriaIDs: []string{"AC-1"}}}),
			moves: []string{"issue#12:needs-human"}, reason: "no AC-n"},

		{name: "ready label by a human", item: issue(12, StateAwaitingApproval),
			in:    withFacts(webhook("issues", "labeled", func(w *WebhookInput) { w.Label = LabelReady; w.Number = 12 }), Facts{ActiveCount: 1}),
			moves: []string{"issue#12:ready"}},
		{name: "dashboard approve", item: issue(12, StateAwaitingApproval), in: withFacts(dash(ActionApprove, nil), Facts{ActiveCount: 1}),
			moves: []string{"issue#12:ready"}},

		{name: "tick starts the oldest ready", item: issue(12, StateReady),
			in:    withFacts(Input{Kind: InputTick}, Facts{Ready: []Item{*issue(12, StateReady), *issue(14, StateReady)}}),
			moves: []string{"issue#12:building"}, effects: []string{"launch:build"}},
		{name: "tick with WIP full", item: issue(12, StateReady),
			in: withFacts(Input{Kind: InputTick}, Facts{ActiveCount: 1, Ready: []Item{*issue(12, StateReady)}}), reason: "nothing to do"},

		{name: "build opened a valid PR", item: issue(12, StateBuilding),
			in:    withFacts(verdict(config.StageBuild, &Verdict{Build: &BuildVerdict{Outcome: "pr_opened", PRNumber: ptr(13)}}), Facts{ActiveCount: 1, Branch: "loop/issue-12", PR: openPR()}),
			moves: []string{"issue#12:in-pr", "pr#13:review"}, effects: []string{"launch:review"}},
		{name: "build error under the cap", item: issue(12, StateBuilding),
			in: Input{Kind: InputVerdict, Verdict: &VerdictInput{RunID: 1, Stage: config.StageBuild, Err: "timeout"},
				Facts: Facts{ActiveCount: 1, Attempts: map[config.Stage]int{config.StageBuild: 1}, OpenRun: &OpenRun{ID: 1}}},
			// It leaves building, freeing its own slot, and is the next ready.
			moves: []string{"issue#12:building"}, effects: []string{"launch:build"}},
		{name: "build error at the cap", item: issue(12, StateBuilding),
			in: Input{Kind: InputVerdict, Verdict: &VerdictInput{RunID: 1, Stage: config.StageBuild, Err: "timeout"},
				Facts: Facts{ActiveCount: 1, Attempts: map[config.Stage]int{config.StageBuild: 2}, OpenRun: &OpenRun{ID: 1}}},
			moves: []string{"issue#12:needs-human"}, reason: "last attempt"},
		{name: "build verdict failing its schema escalates under the cap", item: issue(12, StateBuilding),
			in: Input{Kind: InputVerdict, Verdict: &VerdictInput{RunID: 1, Stage: config.StageBuild, Err: "invalid verdict: missing outcome", Escalate: true},
				Facts: Facts{ActiveCount: 1, Attempts: map[config.Stage]int{config.StageBuild: 1}, OpenRun: &OpenRun{ID: 1}}},
			moves: []string{"issue#12:needs-human"}, reason: "invalid verdict"},
		{name: "build blocked", item: issue(12, StateBuilding),
			in:    withFacts(verdict(config.StageBuild, &Verdict{Build: &BuildVerdict{Outcome: "blocked", Blockers: []string{"AC-2 is ambiguous"}}}), Facts{ActiveCount: 1}),
			moves: []string{"issue#12:needs-human"}, reason: "AC-2 is ambiguous"},
		{name: "build PR checks failed", item: issue(12, StateBuilding),
			in: withFacts(verdict(config.StageBuild, &Verdict{Build: &BuildVerdict{Outcome: "pr_opened", PRNumber: ptr(13)}}),
				Facts{ActiveCount: 1, Branch: "loop/issue-12", PR: func() *PRFacts { p := openPR(); p.ChecksFailed = true; return p }()}),
			moves: []string{"issue#12:needs-human"}, reason: "checks failed"},

		{name: "review posted", item: pr(13, 12, StateReview),
			in:    withFacts(verdict(config.StageReview, &Verdict{Review: &ReviewVerdict{Outcome: "posted", ReviewedSHA: head}}), Facts{ActiveCount: 1, PR: openPR()}),
			moves: []string{"pr#13:sweep"}, effects: []string{"launch:sweep"}},
		{name: "review of a sha that is not the head", item: pr(13, 12, StateReview),
			in:    withFacts(verdict(config.StageReview, &Verdict{Review: &ReviewVerdict{Outcome: "posted", ReviewedSHA: newHead}}), Facts{ActiveCount: 1, PR: openPR()}),
			moves: []string{"pr#13:review"}, effects: []string{"launch:review"}, reason: "but the head is"},
		{name: "review failed", item: pr(13, 12, StateReview),
			in:    withFacts(verdict(config.StageReview, &Verdict{Review: &ReviewVerdict{Outcome: "failed"}}), Facts{ActiveCount: 1}),
			moves: []string{"pr#13:needs-human"}},
		{name: "sweep pushed under the round cap", item: pr(13, 12, StateSweep),
			in:    withFacts(verdict(config.StageSweep, &Verdict{Sweep: &SweepVerdict{Outcome: "changes_pushed"}}), Facts{ActiveCount: 1, Attempts: map[config.Stage]int{config.StageReview: 2}}),
			moves: []string{"pr#13:review"}, effects: []string{"launch:review"}},
		{name: "sweep pushed at the round cap", item: pr(13, 12, StateSweep),
			in:    withFacts(verdict(config.StageSweep, &Verdict{Sweep: &SweepVerdict{Outcome: "changes_pushed"}}), Facts{ActiveCount: 1, Attempts: map[config.Stage]int{config.StageReview: 3}}),
			moves: []string{"pr#13:needs-human"}, reason: "review rounds"},

		{name: "sweep ready, clean, auto_merge", item: pr(13, 12, StateSweep),
			in:    withFacts(verdict(config.StageSweep, sweepReady()), Facts{ActiveCount: 1, PR: openPR(), ReviewedSHA: head}),
			moves: []string{"pr#13:merging"}, effects: []string{"set_status:success", "enable_auto_merge"}},
		{name: "sweep ready, auto_merge off", item: pr(13, 12, StateSweep), cfg: loopCfg(func(c *config.Loop) { c.AutoMerge = false }),
			in:    withFacts(verdict(config.StageSweep, sweepReady()), Facts{ActiveCount: 1, PR: openPR(), ReviewedSHA: head}),
			moves: []string{"pr#13:awaiting-merge"}, effects: []string{"set_status:success"}},
		{name: "sweep ready, open findings", item: pr(13, 12, StateSweep),
			in:    withFacts(verdict(config.StageSweep, sweepReady(Finding{Category: "tests", Severity: "medium", Status: "open", Summary: "no test"})), Facts{ActiveCount: 1, PR: openPR(), ReviewedSHA: head}),
			moves: []string{"pr#13:awaiting-merge"}, effects: []string{"set_status:failure"}, reason: "no test"},
		{name: "sweep ready, declined security finding", item: pr(13, 12, StateSweep),
			in:    withFacts(verdict(config.StageSweep, sweepReady(Finding{Category: "security", Severity: "low", Status: "declined", Summary: "csrf"})), Facts{ActiveCount: 1, PR: openPR(), ReviewedSHA: head}),
			moves: []string{"pr#13:awaiting-merge"}, effects: []string{"set_status:failure"}},
		{name: "sweep ready, head moved", item: pr(13, 12, StateSweep),
			in:    withFacts(verdict(config.StageSweep, sweepReady()), Facts{ActiveCount: 1, PR: func() *PRFacts { p := openPR(); p.HeadSHA = newHead; return p }(), ReviewedSHA: head}),
			moves: []string{"pr#13:review"}, effects: []string{"launch:review"}, reason: "head moved"},
		{name: "sweep cap reached", item: pr(13, 12, StateSweep),
			in:    withFacts(verdict(config.StageSweep, &Verdict{Sweep: &SweepVerdict{Outcome: "cap_reached"}}), Facts{ActiveCount: 1}),
			moves: []string{"pr#13:needs-human"}},
		{name: "sweep blocked", item: pr(13, 12, StateSweep),
			in:    withFacts(verdict(config.StageSweep, &Verdict{Sweep: &SweepVerdict{Outcome: "blocked", Blockers: []string{"schema change"}}}), Facts{ActiveCount: 1}),
			moves: []string{"pr#13:needs-human"}, reason: "schema change"},

		{name: "human push in awaiting-merge", item: pr(13, 12, StateAwaitingMerge),
			in:    withFacts(webhook("pull_request", "synchronize", func(w *WebhookInput) { w.SHA = newHead }), Facts{}),
			moves: []string{"pr#13:review"}, effects: []string{"launch:review", "set_status:pending"}},
		{name: "human push in sweep", item: pr(13, 12, StateSweep),
			in:    withFacts(webhook("pull_request", "synchronize", func(w *WebhookInput) { w.SHA = newHead }), Facts{ActiveCount: 1, OpenRun: &OpenRun{ID: 5, Stage: config.StageSweep, Attempt: 1}}),
			moves: []string{"pr#13:review"}, effects: []string{"destroy_machine", "launch:review", "set_status:pending"}},

		{name: "merged from merging", item: pr(13, 12, StateMerging),
			in:    withFacts(webhook("pull_request", "closed", func(w *WebhookInput) { w.Merged = true }), Facts{ActiveCount: 1}),
			moves: []string{"pr#13:merged"}},
		{name: "merged from awaiting-merge", item: pr(13, 12, StateAwaitingMerge),
			in:    withFacts(webhook("pull_request", "closed", func(w *WebhookInput) { w.Merged = true }), Facts{}),
			moves: []string{"pr#13:merged"}},
		{name: "merged outside the loop", item: pr(13, 12, StateSweep),
			in:    withFacts(webhook("pull_request", "closed", func(w *WebhookInput) { w.Merged = true }), Facts{ActiveCount: 1, Linked: issue(12, StateInPR)}),
			moves: []string{"pr#13:needs-human", "issue#12:needs-human"}, reason: "merged outside the loop"},
		{name: "closed without merging", item: pr(13, 12, StateReview),
			in:    withFacts(webhook("pull_request", "closed", nil), Facts{ActiveCount: 1, Linked: issue(12, StateInPR)}),
			moves: []string{"pr#13:closed", "issue#12:needs-human"}},

		{name: "deploy-dev success", item: pr(13, 12, StateMerged),
			in: withFacts(webhook("workflow_run", "completed", func(w *WebhookInput) {
				w.Workflow = "deploy-dev"
				w.SHA = merge
				w.Branch = "main"
				w.RunEvent = "push"
				w.SameRepo = true
				w.Conclusion = "success"
			}), Facts{ActiveCount: 1, PR: mergedPR()}),
			moves: []string{"pr#13:validating"}, effects: []string{"launch:validate"}},
		{name: "deploy-dev failure", item: pr(13, 12, StateMerged),
			in: withFacts(webhook("workflow_run", "completed", func(w *WebhookInput) {
				w.Workflow = "deploy-dev"
				w.SHA = merge
				w.Branch = "main"
				w.RunEvent = "push"
				w.SameRepo = true
				w.Conclusion = "failure"
			}), Facts{ActiveCount: 1, PR: mergedPR()}),
			moves: []string{"pr#13:needs-human"}},
		{name: "deploy-dev for another sha", item: pr(13, 12, StateMerged),
			in:     withFacts(webhook("workflow_run", "completed", func(w *WebhookInput) { w.Workflow = "deploy-dev"; w.SHA = head; w.Conclusion = "success" }), Facts{ActiveCount: 1, PR: mergedPR()}),
			reason: "not the merge"},

		{name: "validate clears, auto_promote", item: pr(13, 12, StateValidating),
			in:    withFacts(verdict(config.StageValidate, validatePass()), Facts{ActiveCount: 1, PR: mergedPR(), IssueCriteria: []string{"AC-1", "AC-2"}}),
			moves: []string{"pr#13:promoting"}, effects: []string{"dispatch_workflow"}},
		{name: "validate clears, auto_promote off", item: pr(13, 12, StateValidating), cfg: loopCfg(func(c *config.Loop) { c.AutoPromote = false }),
			in:    withFacts(verdict(config.StageValidate, validatePass()), Facts{ActiveCount: 1, PR: mergedPR(), IssueCriteria: []string{"AC-1", "AC-2"}}),
			moves: []string{"pr#13:awaiting-prod-approval"}},
		{name: "validate clears, risk path", item: pr(13, 12, StateValidating),
			in:    withFacts(verdict(config.StageValidate, validatePass()), Facts{ActiveCount: 1, PR: func() *PRFacts { p := mergedPR(); p.ChangedFiles = []string{"db/migrations/1.sql"}; return p }(), IssueCriteria: []string{"AC-1", "AC-2"}}),
			moves: []string{"pr#13:awaiting-prod-approval"}, reason: "risk paths"},
		{name: "validate misses a criterion", item: pr(13, 12, StateValidating),
			in:    withFacts(verdict(config.StageValidate, validatePass()), Facts{ActiveCount: 1, PR: mergedPR(), IssueCriteria: []string{"AC-1", "AC-2", "AC-3"}}),
			moves: []string{"pr#13:needs-human"}, reason: "AC-3 not reported"},
		{name: "validate undetermined", item: pr(13, 12, StateValidating),
			in: withFacts(verdict(config.StageValidate, func() *Verdict {
				v := validatePass()
				v.Validate.Outcome, v.Validate.Criteria[1].Result = "undetermined", "undetermined"
				return v
			}()), Facts{ActiveCount: 1, PR: mergedPR(), IssueCriteria: []string{"AC-1", "AC-2"}}),
			moves: []string{"pr#13:needs-human"}, reason: "AC-2 undetermined"},
		{name: "validate tested another sha", item: pr(13, 12, StateValidating),
			in: withFacts(verdict(config.StageValidate, func() *Verdict {
				v := validatePass()
				v.Validate.TestedSHA = head
				return v
			}()), Facts{ActiveCount: 1, PR: mergedPR(), IssueCriteria: []string{"AC-1", "AC-2"}}),
			moves: []string{"pr#13:needs-human"}, reason: "not the deployed"},
		{name: "validate skipped a smoke flow", item: pr(13, 12, StateValidating),
			in: withFacts(verdict(config.StageValidate, func() *Verdict {
				v := validatePass()
				v.Validate.Regressions = v.Validate.Regressions[1:]
				return v
			}()), Facts{ActiveCount: 1, PR: mergedPR(), IssueCriteria: []string{"AC-1", "AC-2"}}),
			moves: []string{"pr#13:needs-human"}, reason: "smoke flow home not reported"},

		{name: "approve-prod label by a human", item: pr(13, 12, StateAwaitingProdApproval),
			in:    withFacts(webhook("pull_request", "labeled", func(w *WebhookInput) { w.Label = LabelApproveProd }), Facts{PR: mergedPR()}),
			moves: []string{"pr#13:promoting"}, effects: []string{"dispatch_workflow"}},
		{name: "dashboard promote", item: pr(13, 12, StateAwaitingProdApproval), in: withFacts(dash(ActionPromote, nil), Facts{PR: mergedPR()}),
			moves: []string{"pr#13:promoting"}, effects: []string{"dispatch_workflow"}},

		{name: "deploy-prod success", item: pr(13, 12, StatePromoting),
			in: withFacts(webhook("workflow_run", "completed", func(w *WebhookInput) {
				w.Workflow = "deploy-prod"
				w.Branch = "main"
				w.RunEvent = "push"
				w.SameRepo = true
				w.SHA = merge
				w.Conclusion = "success"
			}), Facts{ActiveCount: 1, PR: mergedPR(), Linked: issue(12, StateInPR)}),
			moves: []string{"pr#13:done", "issue#12:done"}, effects: []string{"open_ratchet_issue"}},
		{name: "deploy-prod with no branch", item: pr(13, 12, StatePromoting),
			in:     withFacts(webhook("workflow_run", "completed", func(w *WebhookInput) { w.Workflow = "deploy-prod"; w.SHA = merge; w.Conclusion = "success" }), Facts{ActiveCount: 1, PR: mergedPR()}),
			reason: "workflow run on"},
		{name: "deploy-prod from a fork named main", item: pr(13, 12, StatePromoting),
			in: withFacts(webhook("workflow_run", "completed", func(w *WebhookInput) {
				w.Workflow = "deploy-prod"
				w.Branch = "main"
				w.RunEvent = "pull_request"
				w.SHA = merge
				w.Conclusion = "success"
			}), Facts{ActiveCount: 1, PR: mergedPR()}),
			reason: "not a push or dispatch"},
		{name: "deploy-dev pushed from another repository", item: pr(13, 12, StateMerged),
			in: withFacts(webhook("workflow_run", "completed", func(w *WebhookInput) {
				w.Workflow = "deploy-dev"
				w.Branch = "main"
				w.RunEvent = "push"
				w.SHA = merge
				w.Conclusion = "success"
			}), Facts{ActiveCount: 1, PR: mergedPR()}),
			reason: "not a push or dispatch in this repository"},
		{name: "deploy-prod failure", item: pr(13, 12, StatePromoting),
			in: withFacts(webhook("workflow_run", "completed", func(w *WebhookInput) {
				w.Workflow = "deploy-prod"
				w.Branch = "main"
				w.RunEvent = "push"
				w.SameRepo = true
				w.SHA = merge
				w.Conclusion = "failure"
			}), Facts{ActiveCount: 1, PR: mergedPR()}),
			moves: []string{"pr#13:needs-human"}},
		{name: "deploy-prod dispatched for this promotion", item: pr(13, 12, StatePromoting),
			in: withFacts(webhook("workflow_run", "completed", func(w *WebhookInput) {
				w.Workflow, w.Branch, w.RunEvent, w.SameRepo = "deploy-prod", "main", "workflow_dispatch", true
				w.SHA, w.Conclusion, w.RunCreated = merge, "success", now.Add(-time.Hour+time.Second)
			}), Facts{ActiveCount: 1, PR: mergedPR(), Linked: issue(12, StateInPR)}),
			moves: []string{"pr#13:done", "issue#12:done"}, effects: []string{"open_ratchet_issue"}},
		{name: "deploy-prod dispatched before the promotion", item: pr(13, 12, StatePromoting),
			in: withFacts(webhook("workflow_run", "completed", func(w *WebhookInput) {
				w.Workflow, w.Branch, w.RunEvent, w.SameRepo = "deploy-prod", "main", "workflow_dispatch", true
				w.SHA, w.Conclusion, w.RunCreated = merge, "failure", now.Add(-2*time.Hour)
			}), Facts{ActiveCount: 1, PR: mergedPR()}),
			reason: "dispatched run from"},

		{name: "tick on a stale active item", item: func() *Item { i := pr(13, 12, StateReview); i.StateSince = now.Add(-7 * time.Hour); return i }(),
			in: withFacts(Input{Kind: InputTick}, Facts{ActiveCount: 1}), moves: []string{"pr#13:needs-human"}, reason: "stale"},
		{name: "tick on a fresh active item", item: pr(13, 12, StateReview), in: withFacts(Input{Kind: InputTick}, Facts{ActiveCount: 1}), reason: "nothing to do"},

		{name: "re-queue", item: func() *Item { i := pr(13, 12, StateNeedsHuman); i.EscalatedFrom = StateSweep; return i }(),
			in: dash(ActionRequeue, nil), moves: []string{"pr#13:sweep"}, effects: []string{"launch:sweep"}},
		{name: "re-queue into a full WIP", item: func() *Item { i := pr(13, 12, StateNeedsHuman); i.EscalatedFrom = StateSweep; return i }(),
			in: withFacts(dash(ActionRequeue, nil), Facts{ActiveCount: 1}), reason: "WIP limit"},
		{name: "re-queue while a stale run is still open", item: func() *Item { i := issue(12, StateNeedsHuman); i.EscalatedFrom = StateBuilding; return i }(),
			in: withFacts(dash(ActionRequeue, nil), Facts{OpenRun: &OpenRun{ID: 8, Stage: config.StageBuild, Attempt: 1}}), reason: "run 8 is still open"},
		{name: "cancel the stale run from needs-human", item: func() *Item { i := issue(12, StateNeedsHuman); i.EscalatedFrom = StateBuilding; return i }(),
			in: withFacts(dash(ActionCancelRun, nil), Facts{OpenRun: &OpenRun{ID: 8, Stage: config.StageBuild, Attempt: 1}}), effects: []string{"destroy_machine"}},
		{name: "close out", item: issue(12, StateNeedsHuman), in: dash(ActionCloseOut, nil), moves: []string{"issue#12:closed"}},
		{name: "cancel run", item: pr(13, 12, StateReview),
			in:    withFacts(dash(ActionCancelRun, nil), Facts{ActiveCount: 1, OpenRun: &OpenRun{ID: 7, Stage: config.StageReview, Attempt: 1}}),
			moves: []string{"pr#13:needs-human"}, effects: []string{"destroy_machine"}, reason: "cancelled by cwinters8"},
		{name: "cancel with no run", item: pr(13, 12, StateReview), in: dash(ActionCancelRun, nil), reason: "no run is open"},

		{name: "anything else", item: issue(12, StateReady), in: webhook("issues", "edited", nil), reason: "no rule for issues.edited"},
	})
}

func ptr(n int) *int { return &n }

// A freed slot starts the oldest ready item in the same result.
func TestWIPReleaseStartsOldestReady(t *testing.T) {
	r := row{name: "deploy-prod frees a slot", item: pr(13, 12, StatePromoting),
		in: withFacts(webhook("workflow_run", "completed", func(w *WebhookInput) {
			w.Workflow = "deploy-prod"
			w.Branch = "main"
			w.RunEvent = "push"
			w.SameRepo = true
			w.SHA = merge
			w.Conclusion = "success"
		}),
			Facts{ActiveCount: 1, PR: mergedPR(), Linked: issue(12, StateInPR), Ready: []Item{*issue(20, StateReady), *issue(21, StateReady)}}),
		moves: []string{"pr#13:done", "issue#12:done", "issue#20:building"}, effects: []string{"open_ratchet_issue", "launch:build"}}
	r.run(t)
	// Approving with a free slot and nothing older waiting starts at once.
	res := row{name: "approve into a free slot", item: issue(12, StateAwaitingApproval), in: dash(ActionApprove, nil),
		moves: []string{"issue#12:building"}, effects: []string{"launch:build"}}.run(t)
	// The change carries the state the store still has, so the commit's
	// state check holds.
	if from := res.Changes[0].Item.State; from != StateAwaitingApproval {
		t.Errorf("change from %s; want awaiting-approval", from)
	}
	// With an older ready item, the older one goes first.
	row{name: "approve behind an older ready item", item: issue(12, StateAwaitingApproval),
		in:    withFacts(dash(ActionApprove, nil), Facts{Ready: []Item{*issue(9, StateReady)}}),
		moves: []string{"issue#12:ready", "issue#9:building"}, effects: []string{"launch:build"}}.run(t)
}

func TestSameAttemptRelaunchIgnoresCaps(t *testing.T) {
	row{name: "machine never started at the cap", item: issue(12, StateBuilding),
		in: Input{Kind: InputVerdict, Verdict: &VerdictInput{RunID: 4, Stage: config.StageBuild, Err: "machine never started", SameAttempt: true},
			Facts: Facts{ActiveCount: 1, Attempts: map[config.Stage]int{config.StageBuild: 2}, OpenRun: &OpenRun{ID: 4, Stage: config.StageBuild, Attempt: 3}}},
		moves: []string{"issue#12:building"}, effects: []string{"launch:build"}, reason: "same attempt"}.run(t)
}

func TestLabelsNeedAHumanAndTheRightState(t *testing.T) {
	stranger := func(w *WebhookInput) { w.SenderID = 77 }
	runRows(t, []row{
		{name: "intake label by a stranger", in: webhook("issues", "labeled", func(w *WebhookInput) { w.Label = LabelIntake; w.Number = 12; stranger(w) }), reason: "non-human"},
		{name: "intake label by chuck's App", in: webhook("issues", "labeled", func(w *WebhookInput) { w.Label = LabelIntake; w.Number = 12; w.SenderID = app }), reason: "non-human"},
		{name: "ready label by a stranger", item: issue(12, StateAwaitingApproval), in: webhook("issues", "labeled", func(w *WebhookInput) { w.Label = LabelReady; stranger(w) }), reason: "non-human"},
		{name: "ready label in the wrong state", item: issue(12, StateIntake), in: webhook("issues", "labeled", func(w *WebhookInput) { w.Label = LabelReady }), reason: "not awaiting-approval"},
		{name: "approve-prod by a stranger", item: pr(13, 12, StateAwaitingProdApproval), in: webhook("pull_request", "labeled", func(w *WebhookInput) { w.Label = LabelApproveProd; stranger(w) }), reason: "non-human"},
		{name: "approve-prod in the wrong state", item: pr(13, 12, StateValidating), in: webhook("pull_request", "labeled", func(w *WebhookInput) { w.Label = LabelApproveProd }), reason: "not awaiting-prod-approval"},
		{name: "push by a stranger", item: pr(13, 12, StateAwaitingMerge), in: webhook("pull_request", "synchronize", func(w *WebhookInput) { w.SHA = newHead; stranger(w) }), reason: "non-human"},
		{name: "dashboard by a non-human", item: issue(12, StateAwaitingApproval), in: dash(ActionApprove, func(d *DashboardInput) { d.ActorID = 77 }), reason: "non-human"},
	})
}

// The #24 hold: a risk-path PR waits at awaiting-merge with chuck/loop
// pending until a human approves the reviewed head.
func TestRiskPathHold(t *testing.T) {
	risky := func() *PRFacts {
		p := openPR()
		p.ChangedFiles = []string{"src/a.ts", "db/migrations/0002.sql"}
		return p
	}
	held := func() Facts {
		p := risky()
		p.LoopStatus = StatusPending
		return Facts{PR: p, ReviewedSHA: head}
	}
	// Held for findings: chuck/loop is failure, whether or not risk paths
	// are touched too.
	findings := func(files ...string) Facts {
		p := openPR()
		p.ChangedFiles = files
		p.LoopStatus = StatusFailure
		return Facts{PR: p, ReviewedSHA: head}
	}
	runRows(t, []row{
		{name: "risk-path sweep ready holds pending", item: pr(13, 12, StateSweep),
			in:    withFacts(verdict(config.StageSweep, sweepReady()), Facts{ActiveCount: 1, PR: risky(), ReviewedSHA: head}),
			moves: []string{"pr#13:awaiting-merge"}, effects: []string{"set_status:pending"}, reason: "waiting for human approval: risk paths db/migrations/0002.sql"},
		{name: "a rename out of a risk path counts", item: pr(13, 12, StateSweep),
			in: withFacts(verdict(config.StageSweep, sweepReady()), Facts{ActiveCount: 1, ReviewedSHA: head,
				PR: func() *PRFacts {
					p := openPR()
					p.ChangedFiles = []string{"old.sql"}
					p.PreviousNames = []string{"db/migrations/1.sql"}
					return p
				}()}),
			moves: []string{"pr#13:awaiting-merge"}, effects: []string{"set_status:pending"}},
		{name: "loop.json is always a risk path", item: pr(13, 12, StateSweep),
			in: withFacts(verdict(config.StageSweep, sweepReady()), Facts{ActiveCount: 1, ReviewedSHA: head,
				PR: func() *PRFacts { p := openPR(); p.ChangedFiles = []string{".claude/loop.json"}; return p }()}),
			moves: []string{"pr#13:awaiting-merge"}, effects: []string{"set_status:pending"}},
		{name: "human approval sets success and stays", item: pr(13, 12, StateAwaitingMerge),
			in:      withFacts(webhook("pull_request_review", "submitted", func(w *WebhookInput) { w.ReviewState = "approved"; w.SHA = head }), held()),
			effects: []string{"set_status:success"}, reason: "merge approved"},
		{name: "approve-merge label by a human", item: pr(13, 12, StateAwaitingMerge),
			in:      withFacts(webhook("pull_request", "labeled", func(w *WebhookInput) { w.Label = LabelApproveMerge; w.SHA = head }), held()),
			effects: []string{"set_status:success"}},
		{name: "dashboard approve merge", item: pr(13, 12, StateAwaitingMerge),
			in:      withFacts(dash(ActionApproveMerge, func(d *DashboardInput) { d.SHA = head }), held()),
			effects: []string{"set_status:success"}},
		{name: "approval of a PR held for findings", item: pr(13, 12, StateAwaitingMerge),
			in:     withFacts(dash(ActionApproveMerge, func(d *DashboardInput) { d.SHA = head }), findings("db/migrations/0002.sql")),
			reason: "not held for risk paths"},
		{name: "approval of a PR held for findings, no risk path", item: pr(13, 12, StateAwaitingMerge),
			in:     withFacts(webhook("pull_request_review", "submitted", func(w *WebhookInput) { w.ReviewState = "approved"; w.SHA = head }), findings("src/a.ts")),
			reason: "not held for risk paths"},
		{name: "approval with no risk path, pending status", item: pr(13, 12, StateAwaitingMerge),
			in: withFacts(dash(ActionApproveMerge, func(d *DashboardInput) { d.SHA = head }),
				Facts{PR: func() *PRFacts { p := openPR(); p.LoopStatus = StatusPending; return p }(), ReviewedSHA: head}),
			reason: "not held for risk paths"},
		{name: "approval by a non-human", item: pr(13, 12, StateAwaitingMerge),
			in:     withFacts(webhook("pull_request_review", "submitted", func(w *WebhookInput) { w.ReviewState = "approved"; w.SHA = head; w.SenderID = app }), held()),
			reason: "non-human"},
		{name: "approval for an older sha", item: pr(13, 12, StateAwaitingMerge),
			in:     withFacts(webhook("pull_request_review", "submitted", func(w *WebhookInput) { w.ReviewState = "approved"; w.SHA = newHead }), held()),
			reason: "not the head"},
		{name: "approval of an unreviewed head", item: pr(13, 12, StateAwaitingMerge),
			in: withFacts(webhook("pull_request_review", "submitted", func(w *WebhookInput) { w.ReviewState = "approved"; w.SHA = newHead }),
				Facts{PR: func() *PRFacts { p := risky(); p.HeadSHA = newHead; return p }(), ReviewedSHA: head}),
			reason: "not reviewed"},
		{name: "changes requested is not approval", item: pr(13, 12, StateAwaitingMerge),
			in:     withFacts(webhook("pull_request_review", "submitted", func(w *WebhookInput) { w.ReviewState = "changes_requested"; w.SHA = head }), held()),
			reason: "not an approval"},
		{name: "push after approval resets to pending", item: pr(13, 12, StateAwaitingMerge),
			in:    withFacts(webhook("pull_request", "synchronize", func(w *WebhookInput) { w.SHA = newHead }), held()),
			moves: []string{"pr#13:review"}, effects: []string{"launch:review", "set_status:pending"}},
	})
}

func TestRatchetItemOpensNoRatchet(t *testing.T) {
	ratchet := issue(12, StateInPR)
	ratchet.RatchetOf = 4
	row{name: "ratchet issue done", item: pr(13, 12, StatePromoting),
		in: withFacts(webhook("workflow_run", "completed", func(w *WebhookInput) {
			w.Workflow = "deploy-prod"
			w.Branch = "main"
			w.RunEvent = "push"
			w.SameRepo = true
			w.SHA = merge
			w.Conclusion = "success"
		}), Facts{ActiveCount: 1, PR: mergedPR(), Linked: ratchet}),
		moves: []string{"pr#13:done", "issue#12:done"}}.run(t)
	row{name: "ratchet off", item: pr(13, 12, StatePromoting), cfg: loopCfg(func(c *config.Loop) { c.Ratchet = false }),
		in: withFacts(webhook("workflow_run", "completed", func(w *WebhookInput) {
			w.Workflow = "deploy-prod"
			w.Branch = "main"
			w.RunEvent = "push"
			w.SameRepo = true
			w.SHA = merge
			w.Conclusion = "success"
		}), Facts{ActiveCount: 1, PR: mergedPR(), Linked: issue(12, StateInPR)}),
		moves: []string{"pr#13:done", "issue#12:done"}}.run(t)
	row{name: "OpenRatchetIssue starts intake as chuck", in: dash(ActionStartIntake, func(d *DashboardInput) { d.Number = 30; d.RatchetOf = 12; d.Actor, d.ActorID = "chuck", 0 }),
		moves: []string{"issue#30:intake"}, effects: []string{"launch:intake"}}.run(t)
}

func TestSupersedesTargetsMustExist(t *testing.T) {
	v := &Verdict{Intake: &IntakeVerdict{Outcome: "ready_for_approval", CriteriaIDs: []string{"AC-1"}}}
	runRows(t, []row{
		{name: "missing target", item: issue(12, StateIntake),
			in:    withFacts(verdict(config.StageIntake, v), Facts{IssueCriteria: []string{"AC-1"}, Supersedes: []string{"#4 AC-1", "#4 AC-9"}, SuiteTests: []string{"#4 AC-1"}}),
			moves: []string{"issue#12:needs-human"}, reason: "#4 AC-9"},
		{name: "existing target", item: issue(12, StateIntake),
			in:    withFacts(verdict(config.StageIntake, v), Facts{IssueCriteria: []string{"AC-1"}, Supersedes: []string{"#4 AC-1"}, SuiteTests: []string{"#4 AC-1"}}),
			moves: []string{"issue#12:awaiting-approval"}},
	})
}

func TestAutoAdvanceOffMakesPendingStages(t *testing.T) {
	off := loopCfg(func(c *config.Loop) { c.AutoAdvance = false })
	res := row{name: "review posted, manual", item: pr(13, 12, StateReview), cfg: off,
		in:    withFacts(verdict(config.StageReview, &Verdict{Review: &ReviewVerdict{Outcome: "posted", ReviewedSHA: head}}), Facts{ActiveCount: 1, PR: openPR()}),
		moves: []string{"pr#13:sweep"}}.run(t)
	if res.Changes[0].PendingStage != config.StageSweep {
		t.Errorf("pending stage = %q, want sweep", res.Changes[0].PendingStage)
	}
	pending := pr(13, 12, StateSweep)
	pending.PendingStage = config.StageSweep
	runRows(t, []row{
		{name: "run the pending stage", item: pending, cfg: off, in: dash(ActionRunStage, func(d *DashboardInput) { d.Stage = config.StageSweep }),
			moves: []string{"pr#13:sweep"}, effects: []string{"launch:sweep"}},
		{name: "run a stage that is not pending", item: pending, cfg: off, in: dash(ActionRunStage, func(d *DashboardInput) { d.Stage = config.StageBuild }), reason: "sweep is pending"},
		{name: "run while a run is open", item: pending, cfg: off, in: withFacts(dash(ActionRunStage, func(d *DashboardInput) { d.Stage = config.StageSweep }), Facts{OpenRun: &OpenRun{ID: 3}}), reason: "still open"},
	})
}

func TestStaleAndMismatchedVerdictsDoNothing(t *testing.T) {
	runRows(t, []row{
		{name: "verdict for another stage", item: pr(13, 12, StateSweep),
			in: verdict(config.StageReview, &Verdict{Review: &ReviewVerdict{Outcome: "posted"}}), reason: "review verdict, but the item is sweep"},
		{name: "verdict with no run open", item: pr(13, 12, StateReview),
			in: func() Input {
				in := verdict(config.StageReview, &Verdict{Review: &ReviewVerdict{Outcome: "posted"}})
				in.Facts.OpenRun = nil
				return in
			}(), reason: "no run is open"},
		{name: "verdict from an older run", item: pr(13, 12, StateReview),
			in: withFacts(verdict(config.StageReview, &Verdict{Review: &ReviewVerdict{Outcome: "posted"}}), Facts{OpenRun: &OpenRun{ID: 9}}), reason: "run 9 is open"},
	})
}

func TestNeedsHumanRemembersWhereItCameFrom(t *testing.T) {
	res := Transition(pr(13, 12, StateSweep), repo, loopCfg(),
		withFacts(verdict(config.StageSweep, &Verdict{Sweep: &SweepVerdict{Outcome: "cap_reached"}}), Facts{ActiveCount: 1}), now)
	if res.Changes[0].EscalatedFrom != StateSweep {
		t.Errorf("escalated_from = %q", res.Changes[0].EscalatedFrom)
	}
	// Closed without merging: the issue re-queues to ready, not in-pr.
	res = Transition(pr(13, 12, StateReview), repo, loopCfg(), withFacts(webhook("pull_request", "closed", nil), Facts{ActiveCount: 1, Linked: issue(12, StateInPR)}), now)
	if res.Changes[1].EscalatedFrom != StateReady {
		t.Errorf("issue escalated_from = %q, want ready", res.Changes[1].EscalatedFrom)
	}
}

// Every state the loop can produce is one the store's CHECK accepts.
func TestStatesMatchTheMigration(t *testing.T) {
	sql := readFile(t, "../memory/migrations/0004_loop.sql")
	for _, s := range States {
		if !strings.Contains(sql, "'"+string(s)+"'") {
			t.Errorf("state %s is not in 0004_loop.sql", s)
		}
	}
}

// Transition reads nothing and calls nothing.
func TestPackageImportsNoIO(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		for _, bad := range []string{"net/http", "internal/github", "internal/memory", "database/sql", "os"} {
			if imp == bad || strings.HasSuffix(imp, "/"+bad) {
				t.Errorf("internal/loop imports %s", imp)
			}
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLateMergeWebhookAfterReplayDoesNothing(t *testing.T) {
	for _, s := range []State{StateMerged, StateValidating, StateAwaitingProdApproval, StatePromoting} {
		res := Transition(pr(13, 12, s), repo, loopCfg(),
			webhook("pull_request", "closed", func(w *WebhookInput) { w.Merged = true }), now)
		if len(res.Changes) != 0 || !strings.Contains(res.Reason, "already merged") {
			t.Errorf("%s: changes %v, reason %q; want none, already merged", s, moves(res), res.Reason)
		}
	}
}

func TestTooManyFilesIsARiskPath(t *testing.T) {
	p := openPR()
	p.ChangedFiles, p.TooManyFiles = nil, true
	res := Transition(pr(13, 12, StateSweep), repo, loopCfg(),
		withFacts(verdict(config.StageSweep, sweepReady()), Facts{ActiveCount: 1, PR: p, ReviewedSHA: head}), now)
	if got := moves(res); !slices.Equal(got, []string{"pr#13:awaiting-merge"}) {
		t.Fatalf("moves %v; want awaiting-merge", got)
	}
	if got := kinds(res); !slices.Equal(got, []string{"set_status:pending"}) {
		t.Errorf("effects %v; want a pending hold", got)
	}
	if !strings.Contains(res.Reason, "3000") {
		t.Errorf("reason %q; want it to name the file limit", res.Reason)
	}
}

func TestEscalate(t *testing.T) {
	res := Escalate(*pr(13, 12, StateMerging), loopCfg(), "merge failed three ticks running")
	if got := moves(res); !slices.Equal(got, []string{"pr#13:needs-human"}) {
		t.Fatalf("moves %v", got)
	}
	if res.Changes[0].EscalatedFrom != StateMerging {
		t.Errorf("escalated_from %q; want merging", res.Changes[0].EscalatedFrom)
	}
	var mention bool
	for _, e := range res.Effects {
		mention = mention || e.Kind == EffectMention
	}
	if !mention {
		t.Error("no mention for the escalation")
	}
	for _, s := range []State{StateNeedsHuman, StateDone, StateClosed} {
		if res := Escalate(*pr(13, 12, s), loopCfg(), "x"); len(res.Changes) != 0 {
			t.Errorf("%s escalated: %v", s, moves(res))
		}
	}
}

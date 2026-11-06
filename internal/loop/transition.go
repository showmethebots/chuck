package loop

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/showmethebots/chuck/internal/config"
)

// Labels a human adds to drive the loop. chuck's own state labels mirror
// the state; a label event counts only when a human sent it.
const (
	LabelIntake       = "loop:intake"
	LabelReady        = "loop:ready"
	LabelApproveMerge = "loop:approve-merge"
	LabelApproveProd  = "loop:approve-prod"
)

// Transition decides what an input does to an item: the state changes, for
// this item and any other it moves (the linked issue or PR, sub-issues,
// the next ready item a freed WIP slot starts), and the effects the
// executor then runs. It reads nothing and calls nothing; everything it
// needs from GitHub or the store arrives in in.Facts.
//
// item is nil when the input names something not yet in the loop.
func Transition(item *Item, repo Repo, cfg *config.Loop, in Input, now time.Time) Result {
	t := &txn{repo: repo, cfg: cfg, in: in, f: in.Facts, now: now, active: in.Facts.ActiveCount}
	if item != nil {
		t.item = *item
	}
	switch in.Kind {
	case InputWebhook:
		if in.Webhook == nil {
			return noop("webhook input without a webhook")
		}
		t.webhook(item, *in.Webhook)
	case InputVerdict:
		if in.Verdict == nil || item == nil {
			return noop("verdict without a run or item")
		}
		t.verdict(*in.Verdict)
	case InputDashboard:
		if in.Action == nil {
			return noop("dashboard input without an action")
		}
		t.dashboard(item, *in.Action)
	case InputTick:
		if item == nil {
			return noop("tick without an item")
		}
		t.tick()
	default:
		return noop("unknown input kind " + string(in.Kind))
	}
	if len(t.res.Changes) > 0 || in.Kind == InputTick {
		t.startNext()
	}
	return t.res
}

func noop(reason string) Result { return Result{Reason: reason} }

type txn struct {
	item   Item
	repo   Repo
	cfg    *config.Loop
	in     Input
	f      Facts
	now    time.Time
	active int
	res    Result
}

func (t *txn) skip(format string, a ...any) {
	t.res.Reason = fmt.Sprintf(format, a...)
}

func (t *txn) effect(e Effect) { t.res.Effects = append(t.res.Effects, e) }

// move records a change and its standard effects: the state label, a
// mention at a human gate, and, when launch is set and the new state runs a
// stage, a launch (or a pending stage under auto_advance: false).
func (t *txn) move(it Item, to State, reason string, launch bool) {
	c := Change{Item: it, To: to, Reason: reason}
	if to == StateNeedsHuman {
		c.EscalatedFrom = it.State
		if it.State == StateNeedsHuman || it.State == "" {
			c.EscalatedFrom = it.EscalatedFrom
		}
	}
	if it.State.Active() {
		t.active--
	}
	if to.Active() {
		t.active++
	}
	key := it.Key()
	t.effect(Effect{Kind: EffectStateLabel, Item: key, State: to})
	if launch {
		if stage, ok := stageFor(to); ok {
			if t.cfg.AutoAdvance {
				t.effect(Effect{Kind: EffectLaunch, Item: key, Stage: stage})
			} else {
				c.PendingStage = stage
			}
		}
	}
	switch to {
	case StateNeedsHuman, StateAwaitingApproval, StateAwaitingMerge, StateAwaitingProdApproval:
		t.effect(Effect{Kind: EffectMention, Item: key, State: to, Text: reason})
	}
	t.res.Changes = append(t.res.Changes, c)
	if t.res.Reason == "" {
		t.res.Reason = reason
	}
}

func (t *txn) changed(k Key) bool {
	for _, c := range t.res.Changes {
		if c.Item.Key() == k {
			return true
		}
	}
	return false
}

// startNext fills free WIP slots with the oldest ready items, including any
// this result just made ready, which are newer than every queued one.
func (t *txn) startNext() {
	queue := []Item{}
	for _, r := range t.f.Ready {
		if r.State == StateReady && !t.changed(r.Key()) {
			queue = append(queue, r)
		}
	}
	if t.in.Kind == InputTick && t.item.State == StateReady && !t.changed(t.item.Key()) &&
		!slices.ContainsFunc(queue, func(r Item) bool { return r.Key() == t.item.Key() }) {
		queue = append(queue, t.item)
	}
	for _, c := range t.res.Changes {
		if c.To == StateReady {
			it := c.Item
			it.State = StateReady
			queue = append(queue, it)
		}
	}
	for _, r := range queue {
		if t.active >= t.cfg.WIPLimit {
			return
		}
		// An item already moved this round moves once; replace its change.
		t.dropChange(r.Key())
		t.move(r, StateBuilding, "WIP slot free", true)
	}
}

func (t *txn) dropChange(k Key) {
	for i, c := range t.res.Changes {
		if c.Item.Key() == k {
			// The dropped change's effects stay: its label is overwritten by
			// the next, and a ready item has no launch or mention to undo.
			t.res.Changes = append(t.res.Changes[:i], t.res.Changes[i+1:]...)
			return
		}
	}
}

// --- webhooks ---

func (t *txn) webhook(item *Item, w WebhookInput) {
	human := t.repo.isHuman(w.SenderID)
	switch {
	case w.Event == "issues" && w.Action == "labeled" && w.Label == LabelIntake:
		if !human {
			t.skip("%s added by a non-human", w.Label)
			return
		}
		t.startIntake(item, w.Number, 0)
	case (w.Event == "issues" && w.Action == "labeled" && w.Label == LabelReady):
		if !human {
			t.skip("%s added by a non-human", w.Label)
			return
		}
		t.approve(item)
	case w.Event == "pull_request" && w.Action == "labeled" && w.Label == LabelApproveProd:
		if !human {
			t.skip("%s added by a non-human", w.Label)
			return
		}
		t.promote(item)
	case w.Event == "pull_request" && w.Action == "labeled" && w.Label == LabelApproveMerge:
		if !human {
			t.skip("%s added by a non-human", w.Label)
			return
		}
		t.approveMerge(item, w.SHA)
	case w.Event == "pull_request_review" && w.Action == "submitted":
		if w.ReviewState != "approved" {
			t.skip("review %s is not an approval", w.ReviewState)
			return
		}
		if !human {
			t.skip("approval by a non-human")
			return
		}
		t.approveMerge(item, w.SHA)
	case w.Event == "pull_request" && w.Action == "synchronize":
		t.synchronize(item, w, human)
	case w.Event == "pull_request" && w.Action == "closed":
		t.prClosed(item, w)
	case w.Event == "workflow_run" && w.Action == "completed":
		t.workflowRun(item, w)
	default:
		t.skip("no rule for %s.%s", w.Event, w.Action)
	}
}

func (t *txn) startIntake(item *Item, number, ratchetOf int) {
	if item != nil {
		t.skip("#%d is already in the loop (%s)", number, item.State)
		return
	}
	if number <= 0 {
		t.skip("no issue number")
		return
	}
	t.move(Item{Kind: KindIssue, Number: number, RatchetOf: ratchetOf}, StateIntake, "intake started", true)
}

func (t *txn) approve(item *Item) {
	if !t.is(item, KindIssue, StateAwaitingApproval) {
		return
	}
	t.move(*item, StateReady, "approved", false)
}

func (t *txn) promote(item *Item) {
	if !t.is(item, KindPR, StateAwaitingProdApproval) {
		return
	}
	t.move(*item, StatePromoting, "promotion approved", false)
	t.dispatchProd()
}

// approveMerge is the #24 hold's release: a human approval for the head
// that was reviewed turns chuck/loop green. The item stays in
// awaiting-merge; a human merges.
func (t *txn) approveMerge(item *Item, sha string) {
	if !t.is(item, KindPR, StateAwaitingMerge) {
		return
	}
	pr := t.f.PR
	switch {
	case pr == nil || !pr.Open:
		t.skip("PR is not open")
	case sha == "" || sha != pr.HeadSHA:
		t.skip("approval is for %s, not the head %s", short(sha), short(pr.HeadSHA))
	case sha != t.f.ReviewedSHA:
		t.skip("approval is for %s, which was not reviewed", short(sha))
	// Approval clears a risk-path hold and nothing else: a PR held for
	// unfixed findings needs a new review, not a human's say-so.
	case pr.LoopStatus != StatusPending || len(t.riskPaths()) == 0:
		t.skip("PR is not held for risk paths (chuck/loop is %q)", pr.LoopStatus)
	default:
		t.effect(Effect{Kind: EffectSetStatus, Item: item.Key(), SHA: sha, Status: StatusSuccess,
			Text: "approved by a human"})
		t.res.Reason = "merge approved for " + short(sha)
	}
}

func (t *txn) synchronize(item *Item, w WebhookInput, human bool) {
	if item == nil || item.Kind != KindPR {
		t.skip("not a loop PR")
		return
	}
	switch item.State {
	case StateReview, StateSweep, StateAwaitingMerge:
	default:
		t.skip("push in %s", item.State)
		return
	}
	if !human {
		// The sweep worker's own pushes arrive here; its verdict moves the
		// item. Anyone else's push gets no status, so it cannot merge.
		t.skip("push by a non-human")
		return
	}
	// A review or sweep worker still running would act on the old head.
	if (item.State == StateReview || item.State == StateSweep) && t.f.OpenRun != nil {
		t.effect(Effect{Kind: EffectDestroyMachine, Item: item.Key(), RunID: t.f.OpenRun.ID})
	}
	t.move(*item, StateReview, "pushed by a human", true)
	t.effect(Effect{Kind: EffectSetStatus, Item: item.Key(), SHA: w.SHA, Status: StatusPending,
		Text: "waiting for review of the new head"})
}

func (t *txn) prClosed(item *Item, w WebhookInput) {
	if item == nil || item.Kind != KindPR {
		t.skip("not a loop PR")
		return
	}
	if item.State.Terminal() {
		t.skip("PR already %s", item.State)
		return
	}
	issue := t.f.Linked
	if !w.Merged {
		t.move(*item, StateClosed, "PR closed without merging", false)
		if issue != nil && !issue.State.Terminal() {
			it := *issue
			t.move(it, StateNeedsHuman, fmt.Sprintf("PR #%d was closed without merging", item.Number), false)
			t.fixEscalation(it.Key(), StateReady)
		}
		return
	}
	if item.State == StateMerging || item.State == StateAwaitingMerge {
		t.move(*item, StateMerged, "merged", false)
		return
	}
	t.move(*item, StateNeedsHuman, "merged outside the loop", false)
	if issue != nil && !issue.State.Terminal() {
		t.move(*issue, StateNeedsHuman, fmt.Sprintf("PR #%d was merged outside the loop", item.Number), false)
	}
}

// fixEscalation overrides where a needs-human item re-queues to.
func (t *txn) fixEscalation(k Key, to State) {
	for i := range t.res.Changes {
		if t.res.Changes[i].Item.Key() == k {
			t.res.Changes[i].EscalatedFrom = to
		}
	}
}

func (t *txn) workflowRun(item *Item, w WebhookInput) {
	if item == nil || item.Kind != KindPR {
		t.skip("no loop PR for %s", short(w.SHA))
		return
	}
	pr := t.f.PR
	if pr == nil || pr.MergeSHA == "" || w.SHA != pr.MergeSHA {
		t.skip("workflow run is for %s, not the merge %s", short(w.SHA), mergeSHA(pr))
		return
	}
	if w.Branch != t.cfg.DefaultBranch {
		t.skip("workflow run on %s", w.Branch)
		return
	}
	ok := w.Conclusion == "success"
	switch {
	case w.Workflow == t.cfg.Deploy.DevWorkflow && item.State == StateMerged:
		if ok {
			t.move(*item, StateValidating, "deployed to dev", true)
		} else {
			t.move(*item, StateNeedsHuman, "deploy-dev "+w.Conclusion, false)
		}
	case w.Workflow == t.cfg.Deploy.ProdWorkflow && item.State == StatePromoting:
		if !ok {
			t.move(*item, StateNeedsHuman, "deploy-prod "+w.Conclusion, false)
			return
		}
		t.move(*item, StateDone, "deployed to production", false)
		if issue := t.f.Linked; issue != nil && !issue.State.Terminal() {
			t.move(*issue, StateDone, "deployed to production", false)
			if t.cfg.Ratchet && issue.RatchetOf == 0 {
				t.effect(Effect{Kind: EffectOpenRatchetIssue, Item: issue.Key()})
			}
		}
	default:
		t.skip("workflow %s in %s", w.Workflow, item.State)
	}
}

func mergeSHA(pr *PRFacts) string {
	if pr == nil {
		return "none"
	}
	return short(pr.MergeSHA)
}

// --- verdicts ---

func (t *txn) verdict(v VerdictInput) {
	it := t.item
	want, ok := stageFor(it.State)
	if !ok || want != v.Stage {
		t.skip("%s verdict, but the item is %s", v.Stage, it.State)
		return
	}
	switch {
	case t.f.OpenRun == nil:
		t.skip("verdict from run %d, but no run is open", v.RunID)
		return
	case t.f.OpenRun.ID != v.RunID:
		t.skip("verdict from run %d, but run %d is open", v.RunID, t.f.OpenRun.ID)
		return
	}
	if v.SameAttempt {
		t.move(it, it.State, "relaunch as the same attempt: "+v.Err, true)
		return
	}
	if v.Verdict == nil {
		t.stageError(v)
		return
	}
	switch v.Stage {
	case config.StageIntake:
		t.intakeVerdict(v.Verdict.Intake)
	case config.StageBuild:
		t.buildVerdict(v.Verdict.Build)
	case config.StageReview:
		t.reviewVerdict(v.Verdict.Review)
	case config.StageSweep:
		t.sweepVerdict(v.Verdict.Sweep)
	case config.StageValidate:
		t.validateVerdict(v.Verdict.Validate)
	}
}

func (t *txn) stageError(v VerdictInput) {
	it := t.item
	if v.Stage == config.StageBuild {
		used, limit := t.f.Attempts[config.StageBuild], t.cfg.Limits.BuildAttempts
		if used < limit {
			t.move(it, StateReady, fmt.Sprintf("build attempt %d of %d failed: %s", used, limit, v.Err), false)
			return
		}
		t.move(it, StateNeedsHuman, fmt.Sprintf("build failed on its last attempt (%d of %d): %s", used, limit, v.Err), false)
		return
	}
	t.move(it, StateNeedsHuman, fmt.Sprintf("%s failed: %s", v.Stage, v.Err), false)
}

func (t *txn) intakeVerdict(v *IntakeVerdict) {
	it := t.item
	if v == nil {
		t.move(it, StateNeedsHuman, "intake verdict missing", false)
		return
	}
	if v.Outcome != "ready_for_approval" {
		t.move(it, StateNeedsHuman, "intake needs clarification: "+strings.Join(v.Questions, "; "), false)
		return
	}
	if len(t.f.IssueCriteria) == 0 && len(v.SubIssues) == 0 {
		t.move(it, StateNeedsHuman, "intake reported ready, but the issue has no AC-n criteria and no sub-issues", false)
		return
	}
	var missing []string
	for _, s := range t.f.Supersedes {
		if !slices.Contains(t.f.SuiteTests, s) {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		t.move(it, StateNeedsHuman, "Supersedes names tests that do not exist: "+strings.Join(missing, ", "), false)
		return
	}
	// Sub-issue numbers are the worker's word; only ones GitHub lists as
	// this issue's sub-issues may enter the loop.
	var unknown []string
	for _, n := range v.SubIssues {
		if !slices.Contains(t.f.SubIssues, n) {
			unknown = append(unknown, fmt.Sprintf("#%d", n))
		}
	}
	if len(unknown) > 0 {
		t.move(it, StateNeedsHuman, "intake reported sub-issues GitHub does not list under this issue: "+strings.Join(unknown, ", "), false)
		return
	}
	t.move(it, StateAwaitingApproval, "criteria written", false)
	for _, n := range v.SubIssues {
		if n == it.Number || slices.Contains(t.f.SubIssuesKnown, n) {
			continue
		}
		t.move(Item{Kind: KindIssue, Number: n}, StateAwaitingApproval, fmt.Sprintf("sub-issue of #%d", it.Number), false)
	}
}

func (t *txn) buildVerdict(v *BuildVerdict) {
	it := t.item
	if v == nil || v.Outcome != "pr_opened" {
		reason := "build blocked"
		if v != nil && len(v.Blockers) > 0 {
			reason += ": " + strings.Join(v.Blockers, "; ")
		}
		t.move(it, StateNeedsHuman, reason, false)
		return
	}
	pr := t.f.PR
	var why []string
	switch {
	case v.PRNumber == nil || pr == nil || pr.Number != *v.PRNumber:
		why = append(why, "the reported PR was not found")
	default:
		if !pr.Open {
			why = append(why, "the PR is not open")
		}
		if t.f.Branch == "" || pr.HeadRef != t.f.Branch {
			why = append(why, fmt.Sprintf("the PR is on %q, not %q", pr.HeadRef, t.f.Branch))
		}
		if t.repo.AppUserID == 0 || pr.AuthorID != t.repo.AppUserID {
			why = append(why, "the PR was not opened by chuck's App")
		}
		if !pr.ClosesIssue {
			why = append(why, fmt.Sprintf("the PR does not reference #%d", it.Number))
		}
		if pr.ChecksFailed {
			why = append(why, "the PR's checks failed")
		}
	}
	if len(why) > 0 {
		t.move(it, StateNeedsHuman, "build reported pr_opened, but "+strings.Join(why, "; "), false)
		return
	}
	t.move(it, StateInPR, fmt.Sprintf("PR #%d opened", pr.Number), false)
	t.move(Item{Kind: KindPR, Number: pr.Number, IssueNumber: it.Number}, StateReview, "PR opened", true)
}

func (t *txn) reviewVerdict(v *ReviewVerdict) {
	it := t.item
	if v == nil || v.Outcome != "posted" {
		t.move(it, StateNeedsHuman, "review failed", false)
		return
	}
	// The sha the review reports is checked against GitHub's head here, so
	// every later "head = reviewed sha" check rests on GitHub, not on the
	// worker's word.
	if pr := t.f.PR; pr == nil || v.ReviewedSHA != pr.HeadSHA {
		head := ""
		if pr != nil {
			head = pr.HeadSHA
		}
		t.reviewAgain(fmt.Sprintf("review was of %s, but the head is %s", short(v.ReviewedSHA), short(head)))
		return
	}
	t.move(it, StateSweep, fmt.Sprintf("review posted, %d findings", v.FindingsCount), true)
}

func (t *txn) sweepVerdict(v *SweepVerdict) {
	it := t.item
	if v == nil {
		t.move(it, StateNeedsHuman, "sweep verdict missing", false)
		return
	}
	switch v.Outcome {
	case "changes_pushed":
		t.reviewAgain("sweep pushed fixes")
		return
	case "cap_reached", "blocked":
		reason := "sweep " + strings.ReplaceAll(v.Outcome, "_", " ")
		if len(v.Blockers) > 0 {
			reason += ": " + strings.Join(v.Blockers, "; ")
		}
		t.move(it, StateNeedsHuman, reason, false)
		return
	}
	// ready
	pr := t.f.PR
	if pr == nil || !pr.Open {
		t.move(it, StateNeedsHuman, "sweep ready, but the PR is not open", false)
		return
	}
	if pr.HeadSHA != t.f.ReviewedSHA || v.HeadSHA != pr.HeadSHA {
		t.reviewAgain(fmt.Sprintf("head moved to %s since review of %s", short(pr.HeadSHA), short(t.f.ReviewedSHA)))
		return
	}
	var open []string
	for _, f := range v.Findings {
		if f.Status == "open" || (f.Category == "security" && f.Status != "fixed") {
			open = append(open, fmt.Sprintf("%s %s %s: %s", f.Severity, f.Category, f.Status, f.Summary))
		}
	}
	head := pr.HeadSHA
	if len(open) > 0 {
		reason := "findings left: " + strings.Join(open, "; ")
		t.move(it, StateAwaitingMerge, reason, false)
		t.effect(Effect{Kind: EffectSetStatus, Item: it.Key(), SHA: head, Status: StatusFailure, Text: "findings left"})
		return
	}
	if risk := t.riskPaths(); len(risk) > 0 {
		reason := "waiting for human approval: risk paths " + strings.Join(risk, ", ")
		t.move(it, StateAwaitingMerge, reason, false)
		t.effect(Effect{Kind: EffectSetStatus, Item: it.Key(), SHA: head, Status: StatusPending, Text: truncate(reason, 140)})
		return
	}
	if !t.cfg.AutoMerge {
		t.move(it, StateAwaitingMerge, "ready to merge; auto_merge is off", false)
		t.effect(Effect{Kind: EffectSetStatus, Item: it.Key(), SHA: head, Status: StatusSuccess, Text: "reviewed; a human merges"})
		return
	}
	t.move(it, StateMerging, "ready to merge", false)
	t.effect(Effect{Kind: EffectSetStatus, Item: it.Key(), SHA: head, Status: StatusSuccess, Text: "reviewed"})
	t.effect(Effect{Kind: EffectEnableAutoMerge, Item: it.Key(), SHA: head})
}

// reviewAgain sends the PR back to review, unless that would pass the
// review_rounds cap.
func (t *txn) reviewAgain(reason string) {
	it := t.item
	if rounds := t.f.Attempts[config.StageReview]; rounds >= t.cfg.Limits.ReviewRounds {
		t.move(it, StateNeedsHuman, fmt.Sprintf("%s, but %d of %d review rounds are used", reason, rounds, t.cfg.Limits.ReviewRounds), false)
		return
	}
	t.move(it, StateReview, reason, true)
}

func (t *txn) riskPaths() []string {
	if t.f.PR == nil {
		return nil
	}
	files := append(append([]string(nil), t.f.PR.ChangedFiles...), t.f.PR.PreviousNames...)
	return t.cfg.RiskPathsHit(files)
}

func (t *txn) validateVerdict(v *ValidateVerdict) {
	it := t.item
	if v == nil {
		t.move(it, StateNeedsHuman, "validate verdict missing", false)
		return
	}
	var why []string
	reported := map[string]string{}
	for _, c := range v.Criteria {
		reported[c.ID] = c.Result
	}
	if len(t.f.IssueCriteria) == 0 {
		why = append(why, "the issue has no AC-n criteria")
	}
	for _, id := range t.f.IssueCriteria {
		switch r, ok := reported[id]; {
		case !ok:
			why = append(why, id+" not reported")
		case r != "pass":
			why = append(why, id+" "+r)
		}
	}
	ran := map[string]bool{}
	for _, r := range v.Regressions {
		ran[r.Name] = true
		if r.Result != "pass" {
			why = append(why, "regression "+r.Name+" "+r.Result)
		}
	}
	for _, f := range t.cfg.Validate.Smoke {
		if !ran[f] {
			why = append(why, "smoke flow "+f+" not reported")
		}
	}
	if len(v.Regressions) < len(t.cfg.Validate.Smoke)+1 {
		why = append(why, "the e2e suite was not reported")
	}
	if v.Outcome != "pass" {
		why = append(why, "outcome "+v.Outcome)
	}
	if pr := t.f.PR; pr == nil || v.TestedSHA != pr.MergeSHA {
		why = append(why, fmt.Sprintf("tested %s, not the deployed %s", short(v.TestedSHA), mergeSHA(t.f.PR)))
	}
	if len(why) > 0 {
		t.move(it, StateNeedsHuman, "validation: "+strings.Join(dedupe(why), "; "), false)
		return
	}
	if risk := t.riskPaths(); len(risk) > 0 {
		t.move(it, StateAwaitingProdApproval, "criteria pass; risk paths "+strings.Join(risk, ", "), false)
		return
	}
	if !t.cfg.AutoPromote {
		t.move(it, StateAwaitingProdApproval, "criteria pass; auto_promote is off", false)
		return
	}
	t.move(it, StatePromoting, "criteria pass", false)
	t.dispatchProd()
}

func (t *txn) dispatchProd() {
	sha := ""
	if t.f.PR != nil {
		sha = t.f.PR.MergeSHA
	}
	t.effect(Effect{Kind: EffectDispatchWorkflow, Item: t.item.Key(), Workflow: t.cfg.Deploy.ProdWorkflow, SHA: sha})
}

// --- dashboard ---

func (t *txn) dashboard(item *Item, a DashboardInput) {
	if a.Action != ActionStartIntake || a.RatchetOf == 0 {
		// Only OpenRatchetIssue acts as chuck; every button needs a human.
		if !t.repo.isHuman(a.ActorID) {
			t.skip("%s by a non-human", a.Action)
			return
		}
	}
	switch a.Action {
	case ActionStartIntake:
		t.startIntake(item, a.Number, a.RatchetOf)
	case ActionApprove:
		t.approve(item)
	case ActionPromote:
		t.promote(item)
	case ActionApproveMerge:
		t.approveMerge(item, a.SHA)
	case ActionRunStage:
		t.runStage(item, a.Stage)
	case ActionRequeue:
		t.requeue(item)
	case ActionCloseOut:
		if !t.is(item, "", StateNeedsHuman) {
			return
		}
		t.move(*item, StateClosed, "closed out by "+a.Actor, false)
	case ActionCancelRun:
		if item == nil || t.f.OpenRun == nil {
			t.skip("no run is open")
			return
		}
		run := t.f.OpenRun
		t.effect(Effect{Kind: EffectDestroyMachine, Item: item.Key(), RunID: run.ID})
		t.verdict(VerdictInput{RunID: run.ID, Stage: run.Stage, Attempt: run.Attempt, Err: "cancelled by " + a.Actor})
	default:
		t.skip("unknown action %s", a.Action)
	}
}

func (t *txn) runStage(item *Item, stage config.Stage) {
	if item == nil || item.PendingStage == "" {
		t.skip("no stage is pending")
		return
	}
	if item.PendingStage != stage {
		t.skip("%s is pending, not %s", item.PendingStage, stage)
		return
	}
	if t.f.OpenRun != nil {
		t.skip("run %d is still open", t.f.OpenRun.ID)
		return
	}
	c := Change{Item: *item, To: item.State, Reason: "run " + string(stage)}
	t.res.Changes = append(t.res.Changes, c)
	t.effect(Effect{Kind: EffectLaunch, Item: item.Key(), Stage: stage})
	t.res.Reason = c.Reason
}

func (t *txn) requeue(item *Item) {
	if !t.is(item, "", StateNeedsHuman) {
		return
	}
	to := item.EscalatedFrom
	if to == "" || to == StateNeedsHuman || to.Terminal() {
		t.skip("nothing to re-queue to")
		return
	}
	// A stale tick escalates without stopping the Machine, so a worker can
	// still be running. Re-queueing then would start a second one on the
	// same branch; cancel the run first.
	if t.f.OpenRun != nil {
		t.skip("run %d is still open; cancel it before re-queueing", t.f.OpenRun.ID)
		return
	}
	if to.Active() && t.active >= t.cfg.WIPLimit {
		t.skip("WIP limit %d reached", t.cfg.WIPLimit)
		return
	}
	t.move(*item, to, "re-queued", true)
}

// --- tick ---

func (t *txn) tick() {
	it := t.item
	if it.State.Active() && t.cfg.Limits.StaleHours > 0 &&
		t.now.Sub(it.StateSince) > time.Duration(t.cfg.Limits.StaleHours)*time.Hour {
		t.move(it, StateNeedsHuman, fmt.Sprintf("stale: %s for more than %dh", it.State, t.cfg.Limits.StaleHours), false)
		return
	}
	t.skip("nothing to do")
}

// --- helpers ---

// is checks the item exists, has the kind (if given) and is in the state.
func (t *txn) is(item *Item, kind string, state State) bool {
	switch {
	case item == nil:
		t.skip("not in the loop")
	case kind != "" && item.Kind != kind:
		t.skip("not a loop %s", kind)
	case item.State != state:
		t.skip("item is %s, not %s", item.State, state)
	default:
		return true
	}
	return false
}

func short(sha string) string {
	if sha == "" {
		return "none"
	}
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func dedupe(xs []string) []string {
	var out []string
	for _, x := range xs {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

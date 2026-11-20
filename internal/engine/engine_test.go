package engine

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/showmethebots/chuck/internal/config"
	"github.com/showmethebots/chuck/internal/github"
	"github.com/showmethebots/chuck/internal/loop"
	"github.com/showmethebots/chuck/internal/memory"
)

func labeled(number int, label string, sender int64) string {
	return fmt.Sprintf(`"issue":{"number":%d,"title":"t"},"label":{"name":%q},"sender":{"id":%d,"login":"x"}`, number, label, sender)
}

func TestLabelWebhookStartsIntake(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	w := fx.deliver("issues", "labeled", labeled(12, loop.LabelIntake, human))
	if w.HandledAt.IsZero() || !strings.Contains(w.Result, "issue#12 → intake") {
		t.Fatalf("webhook result %q, handled %v", w.Result, w.HandledAt)
	}
	if got := fx.state("issue", 12); got != "intake" {
		t.Fatalf("state %q; want intake", got)
	}
	tr, rs := fx.latestEffects("issue", 12)
	if tr.InputKind != "webhook" || tr.InputRef != "d-1" || tr.ConfigSHA != "blob1" {
		t.Errorf("transition %+v", tr)
	}
	if got := statuses(rs); got != "state_label=ok launch=ok" {
		t.Errorf("effects %s", got)
	}
	if !slices.Equal(fx.lnch.launched, []string{"issue#12:intake"}) {
		t.Errorf("launched %v", fx.lnch.launched)
	}
}

func TestNonHumanLabelAndOtherInstallationDoNothing(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	if w := fx.deliver("issues", "labeled", labeled(12, loop.LabelIntake, stranger)); !strings.Contains(w.Result, "non-human") {
		t.Errorf("stranger's label: %q", w.Result)
	}
	payload := fmt.Sprintf(`{"action":"labeled","installation":{"id":99},"repository":{"id":%d,"full_name":"showmethebots/demos"},%s}`,
		repoGHID, labeled(12, loop.LabelIntake, human))
	if res, err := fx.e.handleWebhook(fx.ctx, memory.Webhook{Event: "issues", Action: "labeled", Payload: []byte(payload)}); err != nil ||
		!strings.Contains(res, "installation 99") {
		t.Errorf("other installation: %q, %v", res, err)
	}
	// The unsigned event name must match the signed payload's shape.
	if res, _ := fx.e.handleWebhook(fx.ctx, memory.Webhook{Event: "pull_request", Action: "labeled", Payload: []byte(payload)}); !strings.Contains(res, "does not match") {
		t.Errorf("mismatched event: %q", res)
	}
	if _, err := fx.store.ItemByNumber(fx.ctx, fx.repo.ID, "issue", 12); !errors.Is(err, memory.ErrNotFound) {
		t.Errorf("an item was created: %v", err)
	}
}

// Issue #26: "effect failure leaves committed state and the error on the
// row; retry on tick".
func TestEffectFailureKeepsStateAndTickRetries(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	fx.gh.setFail("AddLabel", errBoom)
	fx.deliver("issues", "labeled", labeled(12, loop.LabelIntake, human))
	if got := fx.state("issue", 12); got != "intake" {
		t.Fatalf("state %q; the failed label must not undo the committed state", got)
	}
	_, rs := fx.latestEffects("issue", 12)
	if got := statuses(rs); got != "state_label=failed launch=pending" {
		t.Fatalf("effects %s; want the failure recorded and the rest left pending", got)
	}
	if !strings.Contains(rs[0].Result.Error, "github is down") {
		t.Errorf("error %q not on the row", rs[0].Result.Error)
	}
	if len(fx.lnch.launched) != 0 {
		t.Error("an effect ran after the one that failed")
	}

	fx.gh.setFail("AddLabel", nil)
	fx.tick()
	_, rs = fx.latestEffects("issue", 12)
	if got := statuses(rs); got != "state_label=ok launch=ok" {
		t.Fatalf("after the tick: %s", got)
	}
	if !slices.Equal(fx.lnch.launched, []string{"issue#12:intake"}) {
		t.Errorf("launched %v; want one launch", fx.lnch.launched)
	}
}

// Issue #26: "escalation after three".
func TestEffectFailingThreeTicksEscalates(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	fx.lnch.err = errBoom
	fx.deliver("issues", "labeled", labeled(12, loop.LabelIntake, human))
	for i := 1; i <= EscalateAfter; i++ {
		if got := fx.state("issue", 12); got != "intake" {
			t.Fatalf("before tick %d: state %q", i, got)
		}
		fx.tick()
	}
	it := fx.item("issue", 12)
	if it.State != "needs-human" || it.EscalatedFrom != "intake" {
		t.Fatalf("state %q from %q; want needs-human from intake", it.State, it.EscalatedFrom)
	}
	trs, _ := fx.store.Transitions(fx.ctx, it.ID)
	last := trs[len(trs)-1]
	if !strings.Contains(last.Reason, "launch failed 3 ticks running") {
		t.Errorf("reason %q", last.Reason)
	}
	rs, _ := decodeRecords(trs[len(trs)-2].Effects)
	if got := statuses(rs); got != "state_label=ok launch=abandoned" {
		t.Errorf("the failing row: %s", got)
	}
	// The escalation mentions the human, once.
	comments := fx.gh.issue(12).comments
	if len(comments) != 1 || !strings.Contains(comments[0].Body, "@cwinters8") {
		t.Fatalf("comments %+v", comments)
	}
	fx.tick()
	if n := len(fx.gh.issue(12).comments); n != 1 {
		t.Errorf("%d comments after another tick; a retried mention must not post twice", n)
	}
}

func reviewRun(t *testing.T, fx *fixture, itemID int64, sha string) {
	t.Helper()
	r, err := fx.store.InsertRun(fx.ctx, memory.Run{ItemID: itemID, Stage: "review", Image: "i", Model: "m",
		Credential: "api_key", Prompt: "p", SchemaName: "review", Ref: "r", TokenHash: strings.Repeat("a", 64),
		DeadlineAt: fx.now.Add(time.Hour)}, fx.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.store.MarkRunRunning(fx.ctx, r.ID, fx.now); err != nil {
		t.Fatal(err)
	}
	v := fmt.Sprintf(`{"outcome":"posted","summary":"s","reviewed_sha":%q,"comment_url":"https://x","findings_count":0,"skills_not_run":[]}`, sha)
	if err := fx.store.EndRun(fx.ctx, r.ID, memory.RunEnd{Status: "finished", VerdictJSON: v}, fx.now); err != nil {
		t.Fatal(err)
	}
}

func openRun(t *testing.T, fx *fixture, itemID int64, stage string) memory.Run {
	t.Helper()
	r, err := fx.store.InsertRun(fx.ctx, memory.Run{ItemID: itemID, Stage: stage, Image: "i", Model: "m",
		Credential: "api_key", Prompt: "p", SchemaName: stage, Ref: "r", TokenHash: strings.Repeat("b", 64),
		DeadlineAt: fx.now.Add(time.Hour)}, fx.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.store.MarkRunRunning(fx.ctx, r.ID, fx.now); err != nil {
		t.Fatal(err)
	}
	return r
}

func sweepReady() *loop.Verdict {
	return &loop.Verdict{Stage: config.StageSweep, Outcome: "ready",
		Sweep: &loop.SweepVerdict{Outcome: "ready", HeadSHA: head}}
}

// Issue #26: "merge not retried when the head moved".
func TestMergeNotRetriedWhenHeadMoved(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	fx.seed("issue", 12, 0, "in-pr")
	pr := fx.seed("pr", 13, 12, "sweep")
	fx.gh.openPR(13, 12)
	reviewRun(t, fx, pr.ID, head)
	run := openRun(t, fx, pr.ID, "sweep")

	fx.gh.setFail("AutoMerge", errBoom)
	res, err := fx.e.Verdict(fx.ctx, loop.VerdictInput{RunID: run.ID, Verdict: sweepReady()}, memory.RunEnd{Status: "finished"})
	if err != nil {
		t.Fatal(err)
	}
	if got := fx.state("pr", 13); got != "merging" {
		t.Fatalf("state %q (%s); want merging", got, res.Reason)
	}
	_, rs := fx.latestEffects("pr", 13)
	if got := statuses(rs); got != "state_label=ok set_status=ok enable_auto_merge=failed" {
		t.Fatalf("effects %s", got)
	}
	if !slices.Equal(fx.gh.setStatus, []string{head + " success"}) {
		t.Errorf("statuses %v", fx.gh.setStatus)
	}

	// Someone pushes before the tick. The new head was never reviewed.
	fx.gh.setFail("AutoMerge", nil)
	fx.gh.prs[13].Head.SHA = newHead
	fx.tick()
	if n := fx.gh.count("AutoMerge"); n != 1 {
		t.Errorf("AutoMerge called %d times; the moved head must not be merged", n)
	}
	it := fx.item("pr", 13)
	if it.State != "needs-human" {
		t.Fatalf("state %q; want needs-human", it.State)
	}
	trs, _ := fx.store.Transitions(fx.ctx, it.ID)
	if r := trs[len(trs)-1].Reason; !strings.Contains(r, "head moved") {
		t.Errorf("reason %q", r)
	}
}

func TestMergeRetriedWhileHeadHolds(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	fx.seed("issue", 12, 0, "in-pr")
	pr := fx.seed("pr", 13, 12, "sweep")
	fx.gh.openPR(13, 12)
	reviewRun(t, fx, pr.ID, head)
	run := openRun(t, fx, pr.ID, "sweep")
	fx.gh.setFail("AutoMerge", errBoom)
	if _, err := fx.e.Verdict(fx.ctx, loop.VerdictInput{RunID: run.ID, Verdict: sweepReady()}, memory.RunEnd{Status: "finished"}); err != nil {
		t.Fatal(err)
	}
	fx.gh.setFail("AutoMerge", nil)
	fx.tick()
	if !slices.Equal(fx.gh.merged, []string{"#13 " + head + " SQUASH"}) {
		t.Errorf("merged %v", fx.gh.merged)
	}
	if got := fx.state("pr", 13); got != "merging" {
		t.Errorf("state %q", got)
	}
}

func TestSecondVerdictChangesNothing(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	fx.seed("issue", 12, 0, "in-pr")
	pr := fx.seed("pr", 13, 12, "sweep")
	fx.gh.openPR(13, 12)
	reviewRun(t, fx, pr.ID, head)
	run := openRun(t, fx, pr.ID, "sweep")
	in := loop.VerdictInput{RunID: run.ID, Verdict: sweepReady()}
	if _, err := fx.e.Verdict(fx.ctx, in, memory.RunEnd{Status: "finished"}); err != nil {
		t.Fatal(err)
	}
	before, _ := fx.store.Transitions(fx.ctx, pr.ID)
	if _, err := fx.e.Verdict(fx.ctx, in, memory.RunEnd{Status: "finished"}); !errors.Is(err, memory.ErrStale) && err != nil {
		t.Fatalf("second verdict: %v", err)
	}
	after, _ := fx.store.Transitions(fx.ctx, pr.ID)
	if len(after) != len(before) {
		t.Errorf("a second verdict wrote %d transitions", len(after)-len(before))
	}
	if r, _ := fx.store.RunByID(fx.ctx, run.ID); r.Status != "finished" {
		t.Errorf("run %s", r.Status)
	}
}

// Issue #26: "invalid loop.json launches nothing".
func TestInvalidLoopJSONLaunchesNothing(t *testing.T) {
	fx := newFixture(t)
	fx.gh.loopJSON = `{"default_branch": "main", "wip_limit": 0}`
	fx.discover()
	if fx.repo.Enabled {
		t.Fatal("a repo with an invalid loop.json is enabled")
	}
	if fx.repo.ConfigJSON == "" || fx.repo.ConfigSHA != "blob1" {
		t.Errorf("the invalid config is not recorded for the dashboard: %+v", fx.repo)
	}
	w := fx.deliver("issues", "labeled", labeled(12, loop.LabelIntake, human))
	if !strings.Contains(w.Result, "not in the loop") {
		t.Errorf("result %q", w.Result)
	}
	fx.gh.labelBy(12, loop.LabelIntake, human)
	fx.tick()
	if _, err := fx.store.ItemByNumber(fx.ctx, fx.repo.ID, "issue", 12); !errors.Is(err, memory.ErrNotFound) {
		t.Errorf("an item was created: %v", err)
	}
	if len(fx.lnch.launched) != 0 {
		t.Errorf("launched %v", fx.lnch.launched)
	}

	// A default_branch that is not the repo's is invalid too: deploy runs
	// would never match it.
	fx.gh.loopJSON = strings.Replace(validLoop, `"default_branch": "main"`, `"default_branch": "trunk"`, 1)
	fx.tick()
	if r, _ := fx.store.RepoByName(fx.ctx, "showmethebots", "demos"); r.Enabled {
		t.Error("enabled with a mismatched default_branch")
	}
}

func TestDeletingLoopJSONIsTheKillSwitch(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	if !fx.repo.Enabled {
		t.Fatal("not enabled with a valid loop.json")
	}
	fx.gh.loopJSON = ""
	fx.tick()
	r, _ := fx.store.RepoByName(fx.ctx, "showmethebots", "demos")
	if r.Enabled || r.ConfigJSON != "" {
		t.Errorf("after deleting loop.json: %+v", r)
	}
	// And a failed read changes nothing.
	fx.gh.loopJSON = validLoop
	fx.tick()
	fx.gh.setFail("ReadFile", errBoom)
	fx.tick()
	if r, _ := fx.store.RepoByName(fx.ctx, "showmethebots", "demos"); !r.Enabled {
		t.Error("a failed read of loop.json switched the loop off")
	}
}

// GitHub does not retry failed deliveries; the tick finds what they would
// have done.
func TestTickReplaysMissedLabels(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	fx.gh.labelBy(12, loop.LabelIntake, human)
	fx.gh.labelBy(14, loop.LabelIntake, stranger)
	fx.tick()
	if got := fx.state("issue", 12); got != "intake" {
		t.Errorf("#12 %q; want intake from the replayed label", got)
	}
	if _, err := fx.store.ItemByNumber(fx.ctx, fx.repo.ID, "issue", 14); !errors.Is(err, memory.ErrNotFound) {
		t.Error("a stranger's label was replayed")
	}
	tr, _ := fx.latestEffects("issue", 12)
	if tr.InputKind != "tick" || !strings.Contains(tr.InputRef, "replayed issues.labeled") {
		t.Errorf("transition %+v", tr)
	}

	// loop:ready on an awaiting-approval issue.
	fx.seed("issue", 20, 0, "awaiting-approval")
	fx.gh.labelBy(20, loop.LabelReady, human)
	fx.tick()
	if got := fx.state("issue", 20); got != "ready" && got != "building" {
		trs, _ := fx.store.Transitions(fx.ctx, fx.item("issue", 20).ID)
		t.Errorf("#20 %q; want it approved: %+v", got, trs)
	}
}

func TestTickReplaysMissedMergeAndDeploy(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	fx.seed("issue", 12, 0, "in-pr")
	pr := fx.seed("pr", 13, 12, "merging")
	reviewRun(t, fx, pr.ID, head)
	p := fx.gh.openPR(13, 12)
	p.State, p.Merged, p.MergeSHA = "closed", true, mergeSHA
	fx.tick()
	if got := fx.state("pr", 13); got != "merged" {
		t.Fatalf("state %q; want merged", got)
	}

	// A run named like the deploy but from another file proves nothing.
	fake := github.WorkflowRun{ID: 1, Name: "deploy-dev", Path: ".github/workflows/other.yml", HeadSHA: mergeSHA,
		HeadBranch: "main", Event: "push", Status: "completed", Conclusion: "success"}
	fake.HeadRepository.ID, fake.Repository.ID = repoGHID, repoGHID
	fx.gh.wfRuns[mergeSHA] = []github.WorkflowRun{fake}
	fx.tick()
	if got := fx.state("pr", 13); got != "merged" {
		t.Fatalf("state %q after a spoofed name; want merged", got)
	}

	real := fake
	real.ID, real.Path, real.Name = 2, ".github/workflows/deploy-dev.yml", "Deploy"
	fx.gh.wfRuns[mergeSHA] = append(fx.gh.wfRuns[mergeSHA], real)
	fx.tick()
	if got := fx.state("pr", 13); got != "validating" {
		t.Fatalf("state %q; want validating", got)
	}
	// The real webhook arriving late is not a second merge.
	w := fx.deliver("pull_request", "closed", `"number":13,"pull_request":{"number":13,"merged":true,"head":{"sha":"`+head+`"}},"sender":{"id":1}`)
	if got := fx.state("pr", 13); got != "validating" {
		t.Errorf("late merge webhook moved the PR to %q (%s)", got, w.Result)
	}
}

func TestFactsFailingThreeTicksEscalate(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	fx.seed("issue", 12, 0, "in-pr")
	fx.seed("pr", 13, 12, "awaiting-merge")
	fx.gh.openPR(13, 12)
	fx.gh.setFail("PullRequest", errBoom)
	for i := 0; i < EscalateAfter-1; i++ {
		fx.tick()
	}
	if got := fx.state("pr", 13); got != "awaiting-merge" {
		t.Fatalf("escalated early: %q", got)
	}
	fx.tick()
	if got := fx.state("pr", 13); got != "needs-human" {
		t.Fatalf("state %q after %d failing ticks", got, EscalateAfter)
	}
}

func TestStaleItemEscalatesOnTheTick(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	fx.seed("issue", 12, 0, "in-pr")
	fx.seed("pr", 13, 12, "review")
	fx.gh.openPR(13, 12)
	fx.now = fx.now.Add(7 * time.Hour)
	fx.tick()
	if got := fx.state("pr", 13); got != "needs-human" {
		t.Errorf("state %q; want a stale escalation", got)
	}
}

func TestTickStartsReadyIntoFreeSlot(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	fx.seed("issue", 12, 0, "ready")
	fx.tick()
	if got := fx.state("issue", 12); got != "building" {
		t.Fatalf("state %q; want building", got)
	}
	if !slices.Equal(fx.lnch.launched, []string{"issue#12:build"}) {
		t.Errorf("launched %v", fx.lnch.launched)
	}
}

func TestLabelDriftRepaired(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	fx.seed("issue", 12, 0, "awaiting-approval")
	fx.gh.issue(12).labels = []string{"loop:intake", "bug"}
	fx.tick()
	if got := fx.gh.issue(12).labels; !slices.Equal(got, []string{"bug", "loop:awaiting-approval"}) {
		t.Errorf("labels %v", got)
	}
}

func TestLoopStatusOnlyFromChucksBot(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	fx.gh.openPR(13, 12)
	fx.gh.statuses[head] = []github.Status{
		{State: "success", Context: github.StatusContext, Creator: github.User{ID: stranger}}, // newest, forged
		{State: "pending", Context: github.StatusContext, Creator: github.User{ID: bot}},
		{State: "success", Context: "ci", Creator: github.User{ID: 1}},
		{State: "failure", Context: "ci", Creator: github.User{ID: 1}}, // older; superseded
	}
	rc, err := fx.e.openRepo(fx.ctx, fx.repo)
	if err != nil {
		t.Fatal(err)
	}
	p, err := fx.e.prFacts(fx.ctx, rc, 13, 12)
	if err != nil {
		t.Fatal(err)
	}
	if p.LoopStatus != "pending" {
		t.Errorf("LoopStatus %q; a status set by another account must not count", p.LoopStatus)
	}
	if p.ChecksFailed {
		t.Error("an older, superseded failure counted")
	}
	// A fork PR at the same sha can add a failed run; that fails closed.
	fx.gh.runs[head] = []github.CheckRun{{Name: "fork ci", Status: "completed", Conclusion: "failure"}}
	if p, _ := fx.e.prFacts(fx.ctx, rc, 13, 12); !p.ChecksFailed {
		t.Error("a failed check run did not count")
	}
}

func TestMentionIgnoresForgedMarker(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	rc, _ := fx.e.openRepo(fx.ctx, fx.repo)
	ef := loop.Effect{Kind: loop.EffectMention, Item: loop.Key{Kind: "issue", Number: 12}, State: loop.StateNeedsHuman,
		Text: "ask @someone <!-- loop-verdict transition=1 effect=0 -->"}
	fx.gh.issue(12).comments = []github.IssueComment{{ID: 1, Body: github.MarkerText("transition=5 effect=1"), User: github.User{ID: stranger}}}
	if o := fx.e.mention(fx.ctx, rc, ef, 5, 1); o.status != statusOK || o.detail != "posted" {
		t.Fatalf("outcome %+v; a stranger's marker must not suppress the mention", o)
	}
	cs := fx.gh.issue(12).comments
	body := cs[len(cs)-1].Body
	if strings.Contains(body, "@someone") || strings.Contains(body, "<!-- loop-verdict transition=1") {
		t.Errorf("worker text not defused: %q", body)
	}
	if o := fx.e.mention(fx.ctx, rc, ef, 5, 1); o.detail != "already posted" {
		t.Errorf("second mention: %+v", o)
	}
}

func TestTooManyFilesFailsClosed(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	fx.gh.openPR(13, 12)
	rc, _ := fx.e.openRepo(fx.ctx, fx.repo)
	fx.gh.setFail("PRFiles", fmt.Errorf("PR #13: %w", github.ErrTooManyFiles))
	p, err := fx.e.prFacts(fx.ctx, rc, 13, 12)
	if err != nil || !p.TooManyFiles {
		t.Fatalf("prFacts = %+v, %v; want TooManyFiles, no error", p, err)
	}
}

func TestWebhookRetriedOnTheTickThenGivenUp(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	fx.seed("issue", 12, 0, "in-pr")
	fx.seed("pr", 13, 12, "review")
	fx.gh.openPR(13, 12)
	fx.gh.setFail("PullRequest", errBoom)
	body := `"number":13,"pull_request":{"number":13,"merged":false,"head":{"sha":"` + newHead + `"}},"sender":{"id":40615752}`
	w := fx.deliver("pull_request", "synchronize", body)
	if !w.HandledAt.IsZero() {
		t.Fatalf("a delivery whose facts failed was marked handled: %q", w.Result)
	}
	// A second delivery behind it waits, so the two are not reordered.
	w2 := fx.deliver("pull_request", "closed", `"number":13,"pull_request":{"number":13,"merged":false,"head":{"sha":"`+newHead+`"}},"sender":{"id":1}`)
	if !w2.HandledAt.IsZero() {
		t.Fatal("a later delivery jumped the queue")
	}
	for i := 0; i < EscalateAfter; i++ {
		fx.tick()
	}
	got, _ := fx.store.WebhookByID(fx.ctx, w.ID)
	if got.HandledAt.IsZero() || !strings.Contains(got.Result, "gave up") {
		t.Errorf("after %d ticks: %q", EscalateAfter, got.Result)
	}
}

func TestCancelRunEndsTheRunWithTheVerdict(t *testing.T) {
	fx := newFixture(t)
	fx.discover()
	fx.seed("issue", 12, 0, "in-pr")
	pr := fx.seed("pr", 13, 12, "review")
	fx.gh.openPR(13, 12)
	run := openRun(t, fx, pr.ID, "review")

	if res, err := fx.e.Act(fx.ctx, fx.repo.ID, "pr", 13, loop.DashboardInput{Action: loop.ActionCancelRun, Actor: "mallory", ActorID: stranger}); err != nil || len(res.Changes) != 0 {
		t.Fatalf("a non-human's cancel: %v, %v", res, err)
	}
	if r, _ := fx.store.RunByID(fx.ctx, run.ID); r.Status != "running" {
		t.Fatalf("a refused cancel ended the run: %s", r.Status)
	}
	if _, err := fx.e.Act(fx.ctx, fx.repo.ID, "pr", 13, loop.DashboardInput{Action: loop.ActionCancelRun, Actor: "cwinters8", ActorID: human}); err != nil {
		t.Fatal(err)
	}
	if r, _ := fx.store.RunByID(fx.ctx, run.ID); r.Status != "cancelled" {
		t.Errorf("run %s; want cancelled", r.Status)
	}
	if got := fx.state("pr", 13); got != "needs-human" {
		t.Errorf("state %q", got)
	}
	if !slices.Equal(fx.lnch.destroyed, []int64{run.ID}) {
		t.Errorf("destroyed %v", fx.lnch.destroyed)
	}
}

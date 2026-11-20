package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/showmethebots/chuck/internal/config"
	"github.com/showmethebots/chuck/internal/github"
	"github.com/showmethebots/chuck/internal/loop"
	"github.com/showmethebots/chuck/internal/memory"
)

// TickOnce runs one tick: discovery, the deliveries still waiting, then for
// every enabled repo the events GitHub never delivered, the tick input for
// every item, effect retries and label drift, and last webhook retention.
func (e *Engine) TickOnce(ctx context.Context) {
	e.init()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.discover(ctx)
	e.processWebhooks(ctx)
	repos, err := e.Store.Repos(ctx)
	if err != nil {
		e.Log.Error("tick: list repos", "err", err.Error())
		return
	}
	for _, row := range repos {
		if ctx.Err() != nil {
			return
		}
		rc, err := e.openRepo(ctx, row)
		if errors.Is(err, errRepoOff) {
			continue
		}
		if err != nil {
			e.Log.Error("tick: open repo", "repo", row.Owner+"/"+row.Name, "err", err.Error())
			continue
		}
		e.tickRepo(ctx, rc)
	}
	if e.Retention > 0 {
		n, err := e.Store.DeleteWebhooksBefore(ctx, e.now().Add(-e.Retention))
		if err != nil {
			e.Log.Error("webhook retention", "err", err.Error())
		} else if n > 0 {
			e.Log.Info("webhook retention", "deleted", n)
		}
	}
}

// discover finds every repo the Apps are installed on and reads each one's
// loop.json from its default branch. A repo is enabled exactly when that
// file exists and is valid, and this runs every tick, so the dashboard has
// no off switch that sticks: deleting loop.json is the kill switch.
func (e *Engine) discover(ctx context.Context) {
	claimed := map[string]bool{}
	for _, owner := range e.GitHub.Apps() {
		ins, err := e.GitHub.Installations(ctx, owner)
		if err != nil {
			// Nothing is disabled on a failed listing: a GitHub blip must
			// not switch the loop off.
			e.Log.Error("discover: list installations", "app", owner, "err", err.Error())
			continue
		}
		seen := map[string]bool{}
		for _, in := range ins {
			for _, r := range in.Repos {
				key := strings.ToLower(r.FullName)
				if claimed[key] {
					continue // an App earlier in config order acts on it
				}
				claimed[key], seen[key] = true, true
				e.discoverRepo(ctx, owner, in.ID, r)
			}
		}
		rows, err := e.Store.Repos(ctx)
		if err != nil {
			e.Log.Error("discover: list repos", "err", err.Error())
			continue
		}
		for _, row := range rows {
			if row.AppOwner == owner && row.Enabled && !seen[strings.ToLower(row.Owner+"/"+row.Name)] {
				if err := e.Store.SetRepoEnabled(ctx, row.ID, false); err != nil {
					e.Log.Error("discover: disable repo", "repo", row.Owner+"/"+row.Name, "err", err.Error())
					continue
				}
				e.Log.Warn("repo left the loop: the App no longer reaches it", "repo", row.Owner+"/"+row.Name)
			}
		}
	}
}

func (e *Engine) discoverRepo(ctx context.Context, owner string, installationID int64, r github.Repository) {
	row := memory.Repo{Owner: r.Owner.Login, Name: r.Name, InstallationID: installationID,
		AppOwner: owner, DefaultBranch: r.DefaultBranch}
	if prev, err := e.Store.RepoByName(ctx, row.Owner, row.Name); err == nil {
		row.Enabled = prev.Enabled
		row.ConfigJSON, row.ConfigSHA = prev.ConfigJSON, prev.ConfigSHA
	}
	if r.Archived {
		row.Enabled = false
	}
	id, err := e.Store.UpsertRepo(ctx, row)
	if err != nil {
		e.Log.Error("discover: record repo", "repo", r.FullName, "err", err.Error())
		return
	}
	row.ID = id
	if r.Archived || row.DefaultBranch == "" {
		return
	}
	if _, err := e.readConfig(ctx, row); err != nil {
		e.Log.Error("discover: read loop.json", "repo", r.FullName, "err", err.Error())
	}
}

// readConfig reads loop.json from the repo's default branch, records it,
// and turns the loop on or off for the repo to match. A read that fails
// changes nothing and returns the error.
func (e *Engine) readConfig(ctx context.Context, row memory.Repo) (memory.Repo, error) {
	api, err := e.GitHub.Repo(row)
	if err != nil {
		return row, err
	}
	name := row.Owner + "/" + row.Name
	now := e.now()
	blob, err := api.ReadFile(ctx, github.LoopConfigPath, row.DefaultBranch)
	var enable bool
	switch {
	case errors.Is(err, github.ErrNotFound):
		if row.Enabled {
			e.Log.Warn("loop.json is gone; the repo leaves the loop", "repo", name)
		}
		row.ConfigJSON, row.ConfigSHA = "", ""
	case err != nil:
		return row, err
	default:
		row.ConfigJSON, row.ConfigSHA = string(blob.Content), blob.SHA
		cfg, perr := config.ParseLoop(blob.Content)
		if perr == nil && cfg.DefaultBranch != row.DefaultBranch {
			perr = fmt.Errorf("default_branch is %q, but the repo's default branch is %q", cfg.DefaultBranch, row.DefaultBranch)
		}
		if perr != nil {
			// Recorded so the dashboard can show what is wrong, but off:
			// an invalid config launches nothing.
			e.Log.Warn("loop.json invalid; the repo is off until it is fixed", "repo", name, "sha", blob.SHA, "err", perr.Error())
		} else {
			enable = true
			if !row.Enabled {
				e.Log.Info("repo joined the loop", "repo", name, "sha", blob.SHA)
			}
		}
	}
	if err := e.Store.SetRepoConfig(ctx, row.ID, row.ConfigJSON, row.ConfigSHA, now); err != nil {
		return row, err
	}
	if enable != row.Enabled {
		if err := e.Store.SetRepoEnabled(ctx, row.ID, enable); err != nil {
			return row, err
		}
		row.Enabled = enable
	}
	row.ConfigReadAt = now
	return row, nil
}

// refreshConfig re-reads loop.json before an input acts, so a merged
// change takes effect on the next event rather than the next tick.
func (e *Engine) refreshConfig(ctx context.Context, rc *repoCtx) (*repoCtx, error) {
	row, err := e.readConfig(ctx, rc.row)
	if err != nil {
		return rc, err
	}
	if row.ConfigSHA == rc.row.ConfigSHA && row.Enabled {
		rc.row = row
		return rc, nil
	}
	return e.openRepo(ctx, row)
}

func (e *Engine) tickRepo(ctx context.Context, rc *repoCtx) {
	// Discovery read loop.json moments ago; rc carries what it found.
	name := rc.loop.FullName

	// Events GitHub never delivered. It does not retry a failed delivery,
	// so what a missed webhook would have done is found here instead.
	if err := e.reconcileIntake(ctx, rc); err != nil {
		e.Log.Error("tick: reconcile intake labels", "repo", name, "err", err.Error())
	}
	items, err := e.Store.Items(ctx, rc.row.ID)
	if err != nil {
		e.Log.Error("tick: list items", "repo", name, "err", err.Error())
		return
	}
	for i := range items {
		it := items[i]
		if loop.State(it.State).Terminal() {
			continue
		}
		if err := e.reconcileItem(ctx, rc, it); err != nil {
			e.factErrors[it.ID]++
			n := e.factErrors[it.ID]
			e.Log.Warn("tick: reconcile item", "repo", name, "item", it.Number, "kind", it.Kind, "ticks", n, "err", err.Error())
			if n >= EscalateAfter {
				delete(e.factErrors, it.ID)
				if cur, err := e.Store.ItemByID(ctx, it.ID); err == nil {
					e.escalate(ctx, rc, cur, fmt.Sprintf("chuck could not read this %s from GitHub for %d ticks: %v", kindName(it.Kind), n, err))
				}
			}
			continue
		}
		delete(e.factErrors, it.ID)
	}

	// The tick input: stale items, and ready items into free WIP slots.
	for i := range items {
		it, err := e.Store.ItemByID(ctx, items[i].ID)
		if err != nil || loop.State(it.State).Terminal() {
			continue
		}
		f, _, err := e.storeFacts(ctx, rc, &it)
		if err != nil {
			e.Log.Error("tick: store facts", "repo", name, "item", it.ID, "err", err.Error())
			continue
		}
		if _, err := e.apply(ctx, rc, &it, loop.Input{Kind: loop.InputTick, Facts: f}, meta{kind: "tick", actor: "chuck"}); err != nil {
			e.Log.Error("tick: apply", "repo", name, "item", it.ID, "err", err.Error())
		}
	}

	// Effects that failed, then labels that drifted.
	items, err = e.Store.Items(ctx, rc.row.ID)
	if err != nil {
		return
	}
	for _, it := range items {
		e.retryEffects(ctx, rc, it)
	}
	for _, it := range items {
		if cur, err := e.Store.ItemByID(ctx, it.ID); err == nil && !loop.State(cur.State).Terminal() {
			if err := syncLabel(ctx, rc.api, cur.Number, loop.State(cur.State)); err != nil {
				e.Log.Warn("tick: repair label", "repo", name, "item", cur.Number, "err", err.Error())
			}
		}
	}
}

func kindName(k string) string {
	if k == memory.KindPR {
		return "PR"
	}
	return "issue"
}

// lastLabeler is who most recently added label, from an item's events.
// Zero (no one) counts as a non-human.
func lastLabeler(events []github.IssueEvent, label string) int64 {
	ev, _ := lastLabeled(events, label)
	return ev.Actor.ID
}

func lastLabeled(events []github.IssueEvent, label string) (github.IssueEvent, bool) {
	var last github.IssueEvent
	found := false
	for _, ev := range events {
		if ev.Event == "labeled" && ev.Label.Name == label {
			last, found = ev, true
		}
	}
	return last, found
}

// approvalLabeler is who added an approval label to an item, but only if
// they added it after the item reached its state. A label left from an
// earlier visit to the state approved an earlier head or an earlier
// promotion, not this one, so replaying it would approve what no human
// saw. The comparison is strict and has no allowance for clock skew: the
// tick is the fallback for a missed delivery, and a person can add the
// label again.
func approvalLabeler(events []github.IssueEvent, label string, it memory.Item) (int64, bool) {
	ev, ok := lastLabeled(events, label)
	if !ok || !ev.CreatedAt.After(it.StateSince) {
		return 0, false
	}
	return ev.Actor.ID, true
}

func hasLabel(is github.Issue, label string) bool {
	for _, l := range is.Labels {
		if l.Name == label {
			return true
		}
	}
	return false
}

// replay applies an event the tick found on GitHub as the webhook it
// should have been. The transition table decides as it would have; the
// row records that the tick found it.
func (e *Engine) replay(ctx context.Context, rc *repoCtx, item *memory.Item, w loop.WebhookInput) error {
	ref := fmt.Sprintf("replayed %s.%s #%d", w.Event, w.Action, w.Number)
	res, err := e.input(ctx, rc, item, loop.Input{Kind: loop.InputWebhook, Webhook: &w}, meta{kind: "tick", ref: ref, actor: "chuck"})
	if err != nil {
		return err
	}
	if len(res.Changes) > 0 {
		e.Log.Info("tick replayed a missed event", "repo", rc.loop.FullName, "event", ref, "result", describe(res))
	}
	return nil
}

// reconcileIntake starts intake for open issues a human labeled
// loop:intake whose delivery never came.
func (e *Engine) reconcileIntake(ctx context.Context, rc *repoCtx) error {
	issues, err := rc.api.OpenIssuesWithLabel(ctx, loop.LabelIntake)
	if err != nil {
		return err
	}
	for _, is := range issues {
		if is.PullRequest != nil {
			continue
		}
		// chuck's own state label is loop:intake too; those are in the loop.
		if _, err := e.Store.ItemByNumber(ctx, rc.row.ID, memory.KindIssue, is.Number); err == nil {
			continue
		} else if !errors.Is(err, memory.ErrNotFound) {
			return err
		}
		events, err := rc.api.IssueEvents(ctx, is.Number)
		if err != nil {
			return err
		}
		w := loop.WebhookInput{Event: "issues", Action: "labeled", Label: loop.LabelIntake,
			Number: is.Number, SenderID: lastLabeler(events, loop.LabelIntake)}
		if err := e.replay(ctx, rc, nil, w); err != nil {
			return err
		}
	}
	return nil
}

// reconcileItem replays what a missed delivery would have done for one
// item: a human's approval label or review, a merge or close, a deploy run.
func (e *Engine) reconcileItem(ctx context.Context, rc *repoCtx, it memory.Item) error {
	state := loop.State(it.State)
	if it.Kind == memory.KindIssue {
		if state != loop.StateAwaitingApproval {
			return nil
		}
		is, err := rc.api.GetIssue(ctx, it.Number)
		if err != nil || !hasLabel(is, loop.LabelReady) {
			return err
		}
		events, err := rc.api.IssueEvents(ctx, it.Number)
		if err != nil {
			return err
		}
		sender, ok := approvalLabeler(events, loop.LabelReady, it)
		if !ok {
			return nil
		}
		return e.replay(ctx, rc, &it, loop.WebhookInput{Event: "issues", Action: "labeled", Label: loop.LabelReady,
			Number: it.Number, SenderID: sender})
	}

	pr, err := rc.api.PullRequest(ctx, it.Number)
	if err != nil {
		return err
	}
	if pr.State != "open" {
		closed := loop.WebhookInput{Event: "pull_request", Action: "closed", Number: it.Number, SHA: pr.Head.SHA, Merged: pr.Merged}
		switch {
		case !pr.Merged:
			// Closed unmerged: the PR goes to closed, which ends this.
			return e.replay(ctx, rc, &it, closed)
		case state == loop.StateNeedsHuman:
			// Already with a human; replaying "merged outside the loop"
			// every tick would only repeat the mention.
			return nil
		case state == loop.StateMerged, state == loop.StateValidating,
			state == loop.StateAwaitingProdApproval, state == loop.StatePromoting:
			// Merged, as the loop already knows; the deploys are next.
		default:
			return e.replay(ctx, rc, &it, closed)
		}
	}
	switch state {
	case loop.StateAwaitingMerge:
		if pr.State != "open" {
			return nil
		}
		is, err := rc.api.GetIssue(ctx, it.Number)
		if err != nil {
			return err
		}
		if hasLabel(is, loop.LabelApproveMerge) {
			events, err := rc.api.IssueEvents(ctx, it.Number)
			if err != nil {
				return err
			}
			// The label names no sha, so it stands for the head only if it
			// came after the item reached awaiting-merge, which is after
			// the reviewed head was pushed. The table still requires the
			// head to be the reviewed sha.
			if sender, ok := approvalLabeler(events, loop.LabelApproveMerge, it); ok {
				if err := e.replay(ctx, rc, &it, loop.WebhookInput{Event: "pull_request", Action: "labeled",
					Label: loop.LabelApproveMerge, Number: it.Number, SHA: pr.Head.SHA, SenderID: sender}); err != nil {
					return err
				}
			}
		}
		reviews, err := rc.api.Reviews(ctx, it.Number)
		if err != nil {
			return err
		}
		for _, r := range reviews {
			if strings.EqualFold(r.State, "approved") && r.CommitID == pr.Head.SHA {
				if err := e.replay(ctx, rc, &it, loop.WebhookInput{Event: "pull_request_review", Action: "submitted",
					ReviewState: "approved", Number: it.Number, SHA: r.CommitID, SenderID: r.User.ID}); err != nil {
					return err
				}
			}
		}
	case loop.StateAwaitingProdApproval:
		is, err := rc.api.GetIssue(ctx, it.Number)
		if err != nil || !hasLabel(is, loop.LabelApproveProd) {
			return err
		}
		events, err := rc.api.IssueEvents(ctx, it.Number)
		if err != nil {
			return err
		}
		sender, ok := approvalLabeler(events, loop.LabelApproveProd, it)
		if !ok {
			return nil
		}
		return e.replay(ctx, rc, &it, loop.WebhookInput{Event: "pull_request", Action: "labeled",
			Label: loop.LabelApproveProd, Number: it.Number, SHA: pr.Head.SHA, SenderID: sender})
	case loop.StateMerged, loop.StatePromoting:
		if !pr.Merged || pr.MergeSHA == "" {
			return nil
		}
		want := rc.cfg.Deploy.DevWorkflow
		if state == loop.StatePromoting {
			want = rc.cfg.Deploy.ProdWorkflow
		}
		runs, err := rc.api.WorkflowRuns(ctx, pr.MergeSHA)
		if err != nil {
			return err
		}
		if state == loop.StatePromoting {
			// A push run's head is the merge. The run chuck dispatched has
			// the branch tip as its head, whatever landed after the merge,
			// so it is listed by workflow, from when promoting began. chuck
			// dispatches the workflow by this name, so the API knows it.
			dispatched, err := rc.api.DispatchRuns(ctx, want, it.StateSince.Add(-loop.RunClockSkew))
			if err != nil {
				return err
			}
			runs = append(runs, dispatched...)
		}
		var latest *loop.WebhookInput
		var latestID int64
		for _, r := range runs {
			if r.Status != "completed" {
				continue
			}
			w := loop.WebhookInput{Event: "workflow_run", Action: "completed", Number: it.Number}
			fillRun(&w, r, r.Repository.ID, rc.cfg.Deploy.DevWorkflow, rc.cfg.Deploy.ProdWorkflow)
			// A run is only this repo's own if its head repository is; the
			// list is already the repo's, so its id is the reference.
			if w.Workflow == want && w.SHA == pr.MergeSHA && r.ID > latestID {
				latest, latestID = &w, r.ID
			}
		}
		if latest != nil {
			return e.replay(ctx, rc, &it, *latest)
		}
	}
	return nil
}

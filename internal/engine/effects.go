package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/showmethebots/chuck/internal/github"
	"github.com/showmethebots/chuck/internal/loop"
	"github.com/showmethebots/chuck/internal/memory"
)

// Effect result statuses, as they appear in transitions.effects_json.
const (
	statusPending    = "pending"    // not run yet: committed, or after an earlier failure
	statusOK         = "ok"         // done
	statusFailed     = "failed"     // tried and failed; the tick retries it
	statusSkipped    = "skipped"    // nothing to do, or the part of chuck that does it is not built yet
	statusAbandoned  = "abandoned"  // will not be retried: unsafe now, or failed EscalateAfter ticks
	statusSuperseded = "superseded" // the item moved on before it ran
)

// effectRecord is one effect and what became of it.
type effectRecord struct {
	loop.Effect
	Result effectResult `json:"result"`
}

type effectResult struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
	// Ticks counts failed retries on the tick; at EscalateAfter the item
	// escalates.
	Ticks int    `json:"ticks,omitempty"`
	At    string `json:"at,omitempty"`
}

func (r effectResult) open() bool { return r.Status == statusPending || r.Status == statusFailed }

func encodeRecords(rs []effectRecord) string {
	if len(rs) == 0 {
		return "[]"
	}
	b, err := json.Marshal(rs)
	if err != nil {
		// Every field is a string or a number.
		panic(err)
	}
	return string(b)
}

func decodeRecords(s string) ([]effectRecord, error) {
	var rs []effectRecord
	if err := json.Unmarshal([]byte(s), &rs); err != nil {
		return nil, err
	}
	return rs, nil
}

// outcome is what running an effect came to.
type outcome struct {
	status, detail string
	err            error
	escalate       string // non-empty: escalate the effect's item with this reason
}

func (o outcome) result(at time.Time, prev effectResult) effectResult {
	r := effectResult{Status: o.status, Detail: o.detail, Ticks: prev.Ticks, At: at.Format(time.RFC3339)}
	if o.err != nil {
		r.Status, r.Error = statusFailed, o.err.Error()
	}
	return r
}

func ok(detail string) outcome      { return outcome{status: statusOK, detail: detail} }
func failed(err error) outcome      { return outcome{err: err} }
func skipped(detail string) outcome { return outcome{status: statusSkipped, detail: detail} }

// run performs one effect. Every effect is safe to run again: labels and
// statuses converge, a mention checks for its own marker, a launch checks
// for an open run, and a merge rechecks the PR first.
func (e *Engine) run(ctx context.Context, rc *repoCtx, ef loop.Effect, transitionID int64, idx int) outcome {
	n := ef.Item.Number
	switch ef.Kind {
	case loop.EffectStateLabel:
		if err := syncLabel(ctx, rc.api, n, ef.State); err != nil {
			return failed(err)
		}
		return ok(loop.StateLabel(ef.State))
	case loop.EffectMention:
		return e.mention(ctx, rc, ef, transitionID, idx)
	case loop.EffectSetStatus:
		if ef.SHA == "" {
			return outcome{status: statusAbandoned, detail: "no sha to set chuck/loop on"}
		}
		if err := rc.api.SetStatus(ctx, ef.SHA, ef.Status, ef.Text, ""); err != nil {
			return failed(err)
		}
		return ok(ef.Status + " on " + short(ef.SHA))
	case loop.EffectEnableAutoMerge:
		return e.merge(ctx, rc, ef)
	case loop.EffectDispatchWorkflow:
		if ef.SHA == "" {
			return outcome{status: statusAbandoned, detail: "no merge sha to deploy", escalate: "deploy-prod has no merge sha to deploy"}
		}
		if err := rc.api.DispatchWorkflow(ctx, ef.Workflow, rc.cfg.DefaultBranch, map[string]string{"sha": ef.SHA}); err != nil {
			return failed(err)
		}
		return ok(ef.Workflow + " for " + short(ef.SHA))
	case loop.EffectLaunch:
		it, err := e.Store.ItemByNumber(ctx, rc.row.ID, ef.Item.Kind, n)
		if err != nil {
			return failed(err)
		}
		if run, err := e.Store.OpenRun(ctx, it.ID); err == nil {
			return ok(fmt.Sprintf("run %d is already open", run.ID))
		} else if !errors.Is(err, memory.ErrNotFound) {
			return failed(err)
		}
		if e.Launcher == nil {
			return failed(errors.New("no worker launcher in this build (#27)"))
		}
		if err := e.Launcher.Launch(ctx, LaunchRequest{Repo: rc.row, Config: rc.cfg, Item: it, Stage: ef.Stage}); err != nil {
			return failed(err)
		}
		return ok("launched " + string(ef.Stage))
	case loop.EffectDestroyMachine:
		if e.Launcher == nil {
			return failed(errors.New("no worker launcher in this build (#27)"))
		}
		if err := e.Launcher.Destroy(ctx, ef.RunID); err != nil {
			return failed(err)
		}
		return ok(fmt.Sprintf("run %d destroyed", ef.RunID))
	case loop.EffectOpenRatchetIssue:
		if e.Ratchet == nil {
			return skipped("ratchet issues are not built yet (#32)")
		}
		it, err := e.Store.ItemByNumber(ctx, rc.row.ID, ef.Item.Kind, n)
		if err != nil {
			return failed(err)
		}
		if err := e.Ratchet.Open(ctx, rc.row, it); err != nil {
			return failed(err)
		}
		return ok("ratchet issue opened")
	}
	return outcome{status: statusAbandoned, detail: "unknown effect " + string(ef.Kind)}
}

// syncLabel makes the item carry its state's loop:* label and no other
// state's. An approval label (loop:approve-merge, loop:approve-prod) stays
// only in the state it approves: once the item leaves that state the
// approval is spent or its head is gone, and a label left behind would
// read as approval of whatever comes next.
func syncLabel(ctx context.Context, api RepoAPI, number int, state loop.State) error {
	is, err := api.GetIssue(ctx, number)
	if err != nil {
		return err
	}
	want := loop.StateLabel(state)
	have := false
	for _, l := range is.Labels {
		if l.Name == want {
			have = true
			continue
		}
		if isStateLabel(l.Name) || staleApproval(l.Name, state) {
			if err := api.RemoveLabel(ctx, number, l.Name); err != nil {
				return err
			}
		}
	}
	if !have {
		return api.AddLabel(ctx, number, want)
	}
	return nil
}

func staleApproval(name string, state loop.State) bool {
	switch name {
	case loop.LabelApproveMerge:
		return state != loop.StateAwaitingMerge
	case loop.LabelApproveProd:
		return state != loop.StateAwaitingProdApproval
	}
	return false
}

func isStateLabel(name string) bool {
	return slices.ContainsFunc(loop.States, func(s loop.State) bool { return loop.StateLabel(s) == name })
}

// mention posts the comment that tells the humans an item needs them. Its
// marker names the transition and effect, so a retry finds the comment it
// already posted instead of posting twice. Only a comment by chuck's own
// bot counts: anyone can type a marker.
func (e *Engine) mention(ctx context.Context, rc *repoCtx, ef loop.Effect, transitionID int64, idx int) outcome {
	marker := fmt.Sprintf("transition=%d effect=%d", transitionID, idx)
	cs, err := rc.api.IssueComments(ctx, ef.Item.Number)
	if err != nil {
		return failed(err)
	}
	want := github.MarkerText(marker)
	for _, c := range cs {
		if c.User.ID == rc.bot && strings.Contains(c.Body, want) {
			return ok("already posted")
		}
	}
	var at []string
	for _, h := range e.Humans {
		login, err := e.login(ctx, rc, h)
		if err != nil {
			return failed(fmt.Errorf("look up human %d: %w", h, err))
		}
		at = append(at, "@"+login)
	}
	body := fmt.Sprintf("%s this is now **%s**.\n\n%s", strings.Join(at, " "), ef.State, quote(ef.Text))
	if _, err := rc.api.Comment(ctx, ef.Item.Number, body, marker); err != nil {
		return failed(err)
	}
	return ok("posted")
}

func (e *Engine) login(ctx context.Context, rc *repoCtx, id int64) (string, error) {
	if l, ok := e.logins[id]; ok {
		return l, nil
	}
	u, err := rc.api.UserByID(ctx, id)
	if err != nil {
		return "", err
	}
	if u.Login == "" || strings.ContainsAny(u.Login, " \n@") {
		return "", fmt.Errorf("user %d has login %q", id, u.Login)
	}
	e.logins[id] = u.Login
	return u.Login, nil
}

// quote renders a reason for a comment chuck posts as its App. Reasons
// can carry a worker's words (intake questions, build blockers), so
// mentions are defused, the hidden-marker opener is escaped (a forged
// marker could make a later mention look already posted), and the length
// is capped.
func quote(s string) string {
	s = strings.ReplaceAll(s, "@", "@\u200b")
	s = strings.ReplaceAll(s, "<!--", "&lt;!--")
	if len(s) > 4000 {
		s = s[:4000] + "…"
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

// merge enables auto-merge, or merges, but only while the PR is still open
// and unmerged at the sha the sweep cleared. A head that moved is never
// merged: the item escalates instead, since the new head was not reviewed.
func (e *Engine) merge(ctx context.Context, rc *repoCtx, ef loop.Effect) outcome {
	pr, err := rc.api.PullRequest(ctx, ef.Item.Number)
	if err != nil {
		return failed(err)
	}
	switch {
	case pr.Merged:
		return ok("already merged")
	case pr.State != "open":
		return outcome{status: statusAbandoned, detail: "PR is closed"}
	case ef.SHA == "" || pr.Head.SHA != ef.SHA:
		reason := fmt.Sprintf("not merged: the head moved from %s to %s after the sweep cleared it", short(ef.SHA), short(pr.Head.SHA))
		return outcome{status: statusAbandoned, detail: reason, escalate: reason}
	}
	merged, err := rc.api.AutoMerge(ctx, pr, ef.SHA, rc.cfg.MergeMethod)
	if err != nil {
		return failed(err)
	}
	if merged {
		return ok("merged " + short(ef.SHA))
	}
	return ok("auto-merge enabled at " + short(ef.SHA))
}

// supersede marks a transition's unfinished effects superseded: its item
// has a newer row, and only the newest row's effects are retried.
func (e *Engine) supersede(ctx context.Context, transitionID int64) {
	t, err := e.transitionByID(ctx, transitionID)
	if err != nil {
		return
	}
	rs, err := decodeRecords(t.Effects)
	if err != nil {
		return
	}
	changed := false
	for i := range rs {
		if rs[i].Result.open() {
			rs[i].Result.Status, changed = statusSuperseded, true
		}
	}
	if changed {
		if err := e.Store.RecordEffects(ctx, transitionID, encodeRecords(rs)); err != nil {
			e.Log.Error("supersede effects", "transition", transitionID, "err", err.Error())
		}
	}
}

func (e *Engine) transitionByID(ctx context.Context, id int64) (memory.Transition, error) {
	return e.Store.TransitionByID(ctx, id)
}

// retryEffects runs an item's newest row's unfinished effects again, in
// order, stopping at the first that still fails. One that has failed
// EscalateAfter ticks is abandoned and the item escalates.
func (e *Engine) retryEffects(ctx context.Context, rc *repoCtx, it memory.Item) {
	t, err := e.Store.LatestTransition(ctx, it.ID)
	if err != nil {
		return
	}
	rs, err := decodeRecords(t.Effects)
	if err != nil {
		e.Log.Error("decode effects", "transition", t.ID, "err", err.Error())
		return
	}
	now := e.now()
	for i := range rs {
		if !rs[i].Result.open() {
			continue
		}
		prev := rs[i].Result
		o := e.run(ctx, rc, rs[i].Effect, t.ID, i)
		rs[i].Result = o.result(now, prev)
		escalate := o.escalate
		if rs[i].Result.Status == statusFailed {
			rs[i].Result.Ticks++
			if rs[i].Result.Ticks >= EscalateAfter {
				rs[i].Result.Status = statusAbandoned
				escalate = fmt.Sprintf("%s failed %d ticks running: %s", rs[i].Kind, rs[i].Result.Ticks, rs[i].Result.Error)
			}
		}
		if err := e.Store.RecordEffects(ctx, t.ID, encodeRecords(rs)); err != nil {
			e.Log.Error("record effect", "transition", t.ID, "err", err.Error())
			return
		}
		if escalate != "" {
			e.escalateKey(ctx, rc, rs[i].Item, escalate)
			return
		}
		if rs[i].Result.Status == statusFailed {
			return
		}
	}
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	if sha == "" {
		return "none"
	}
	return sha
}

package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/showmethebots/chuck/internal/config"
	"github.com/showmethebots/chuck/internal/loop"
	"github.com/showmethebots/chuck/internal/memory"
)

// Verdict applies a run's verdict, or chuck's own error for a run that
// produced none, and ends the run in the same commit as the changes it
// causes. internal/worker's run API and watcher call it. A run that has
// already ended returns memory.ErrStale and changes nothing, so a second
// or late post cannot move the item.
func (e *Engine) Verdict(ctx context.Context, v loop.VerdictInput, end memory.RunEnd) (loop.Result, error) {
	e.init()
	e.mu.Lock()
	defer e.mu.Unlock()
	run, err := e.Store.RunByID(ctx, v.RunID)
	if err != nil {
		return loop.Result{}, err
	}
	if v.Stage == "" {
		v.Stage = config.Stage(run.Stage)
	}
	if v.Stage != config.Stage(run.Stage) || v.Attempt != 0 && v.Attempt != run.Attempt {
		return loop.Result{}, fmt.Errorf("verdict for %s attempt %d, but run %d is %s attempt %d",
			v.Stage, v.Attempt, run.ID, run.Stage, run.Attempt)
	}
	v.Attempt = run.Attempt
	item, err := e.Store.ItemByID(ctx, run.ItemID)
	if err != nil {
		return loop.Result{}, err
	}
	row, err := e.repoByID(ctx, item.RepoID)
	if err != nil {
		return loop.Result{}, err
	}
	rc, err := e.openRepo(ctx, row)
	if errors.Is(err, errRepoOff) {
		// The run still ends; nothing moves in a repo that left the loop.
		return loop.Result{Reason: err.Error()}, e.Store.EndRun(ctx, run.ID, end, e.now())
	}
	if err != nil {
		return loop.Result{}, err
	}
	in := loop.Input{Kind: loop.InputVerdict, Verdict: &v}
	if in.Facts, err = e.facts(ctx, rc, &item, in); err != nil {
		return loop.Result{}, fmt.Errorf("facts: %w", err)
	}
	res := loop.Transition(toLoop(&item), rc.loop, rc.cfg, in, e.now())
	err = e.commit(ctx, rc, res, meta{kind: "verdict", ref: fmt.Sprint(run.ID), actor: "chuck"},
		&verdictEnd{runID: run.ID, end: end})
	return res, err
}

// Act applies a dashboard press (#31) on an item, or on an issue not yet
// in the loop for Start intake. The transition table refuses an action
// that does not fit the item's state, so a stale page cannot double-launch.
func (e *Engine) Act(ctx context.Context, repoID int64, kind string, number int, a loop.DashboardInput) (loop.Result, error) {
	e.init()
	e.mu.Lock()
	defer e.mu.Unlock()
	if a.Actor == "" {
		return loop.Result{}, errors.New("a dashboard action needs an actor")
	}
	row, err := e.repoByID(ctx, repoID)
	if err != nil {
		return loop.Result{}, err
	}
	rc, err := e.openRepo(ctx, row)
	if err != nil {
		return loop.Result{}, err
	}
	var item *memory.Item
	switch it, err := e.Store.ItemByNumber(ctx, repoID, kind, number); {
	case err == nil:
		item = &it
	case !errors.Is(err, memory.ErrNotFound):
		return loop.Result{}, err
	}
	if a.Number == 0 {
		a.Number = number
	}
	in := loop.Input{Kind: loop.InputDashboard, Action: &a}
	if in.Facts, err = e.facts(ctx, rc, item, in); err != nil {
		return loop.Result{}, fmt.Errorf("facts: %w", err)
	}
	// Cancel run ends the run in the same commit as the error verdict it
	// stands for, as a worker's own verdict would.
	var ve *verdictEnd
	if a.Action == loop.ActionCancelRun && in.Facts.OpenRun != nil {
		ve = &verdictEnd{runID: in.Facts.OpenRun.ID,
			end: memory.RunEnd{Status: memory.RunCancelled, Reason: "cancelled by " + a.Actor}}
	}
	res := loop.Transition(toLoop(item), rc.loop, rc.cfg, in, e.now())
	if len(res.Changes) == 0 {
		ve = nil // the table refused the press; the run stays open
	}
	return res, e.commit(ctx, rc, res, meta{kind: "dashboard", ref: string(a.Action), actor: a.Actor}, ve)
}

func (e *Engine) repoByID(ctx context.Context, id int64) (memory.Repo, error) {
	repos, err := e.Store.Repos(ctx)
	if err != nil {
		return memory.Repo{}, err
	}
	for _, r := range repos {
		if r.ID == id {
			return r, nil
		}
	}
	return memory.Repo{}, memory.ErrNotFound
}

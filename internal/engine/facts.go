package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/showmethebots/chuck/internal/config"
	"github.com/showmethebots/chuck/internal/github"
	"github.com/showmethebots/chuck/internal/loop"
	"github.com/showmethebots/chuck/internal/memory"
)

// failedConclusions are the check-run conclusions that fail a build.
var failedConclusions = map[string]bool{
	"failure": true, "timed_out": true, "cancelled": true, "action_required": true, "startup_failure": true,
}

// storeFacts is what the store alone knows: WIP, the ready queue, and for
// an item its attempts, open run and linked item.
func (e *Engine) storeFacts(ctx context.Context, rc *repoCtx, item *memory.Item) (loop.Facts, []memory.Item, error) {
	var f loop.Facts
	items, err := e.Store.Items(ctx, rc.row.ID)
	if err != nil {
		return f, nil, err
	}
	for i := range items {
		it := &items[i]
		s := loop.State(it.State)
		if s.Active() {
			f.ActiveCount++
		}
		if s == loop.StateReady {
			f.Ready = append(f.Ready, *toLoop(it))
		}
	}
	if item == nil {
		return f, items, nil
	}
	f.Attempts = map[config.Stage]int{}
	for _, st := range config.Stages {
		n, err := e.Store.Attempts(ctx, item.ID, string(st))
		if err != nil {
			return f, nil, err
		}
		f.Attempts[st] = n
	}
	switch run, err := e.Store.OpenRun(ctx, item.ID); {
	case err == nil:
		f.OpenRun = &loop.OpenRun{ID: run.ID, Stage: config.Stage(run.Stage), Attempt: run.Attempt}
	case !errors.Is(err, memory.ErrNotFound):
		return f, nil, err
	}
	f.Linked = linked(items, item)
	return f, items, nil
}

// linked is the issue a PR serves, or the newest PR an issue follows.
func linked(items []memory.Item, item *memory.Item) *loop.Item {
	var best *memory.Item
	for i := range items {
		it := &items[i]
		switch {
		case item.Kind == memory.KindPR && it.Kind == memory.KindIssue && it.Number == item.IssueNumber:
			return toLoop(it)
		case item.Kind == memory.KindIssue && it.Kind == memory.KindPR && it.IssueNumber == item.Number:
			if best == nil || it.ID > best.ID {
				best = it
			}
		}
	}
	return toLoop(best)
}

// facts is everything Transition needs for an input on an item: the store
// facts plus what GitHub says now. It reads only what the item's state and
// the input can use.
func (e *Engine) facts(ctx context.Context, rc *repoCtx, item *memory.Item, in loop.Input) (loop.Facts, error) {
	f, items, err := e.storeFacts(ctx, rc, item)
	if err != nil || item == nil {
		return f, err
	}
	state := loop.State(item.State)

	if item.Kind == memory.KindIssue {
		switch state {
		case loop.StateIntake:
			is, err := rc.api.GetIssue(ctx, item.Number)
			if err != nil {
				return f, err
			}
			f.IssueCriteria = loop.IssueCriteria(is.Body)
			f.Supersedes = loop.SupersedesTargets(is.Body)
			// The suite's test names are not read yet: SuiteTests stays
			// empty, so any Supersedes line fails closed to needs-human
			// until the ratchet (#32) records the suite.
			if f.SubIssues, err = rc.api.SubIssues(ctx, item.Number); err != nil {
				return f, err
			}
			for _, it := range items {
				if it.Kind == memory.KindIssue && containsInt(f.SubIssues, it.Number) {
					f.SubIssuesKnown = append(f.SubIssuesKnown, it.Number)
				}
			}
		case loop.StateBuilding:
			f.Branch = loop.BuildBranch(item.Number)
			if v := in.Verdict; v != nil && v.Verdict != nil && v.Verdict.Build != nil && v.Verdict.Build.PRNumber != nil {
				n := *v.Verdict.Build.PRNumber
				if n > 0 {
					if f.PR, err = e.prFacts(ctx, rc, n, item.Number); err != nil {
						return f, err
					}
				}
			}
		}
		return f, nil
	}

	// A PR.
	if f.PR, err = e.prFacts(ctx, rc, item.Number, item.IssueNumber); err != nil {
		return f, err
	}
	switch v, err := e.Store.LatestVerdict(ctx, item.ID, string(config.StageReview)); {
	case err == nil:
		rv, perr := loop.ParseVerdict(config.StageReview, []byte(v))
		if perr != nil {
			return f, fmt.Errorf("stored review verdict: %w", perr)
		}
		f.ReviewedSHA = rv.Review.ReviewedSHA
	case !errors.Is(err, memory.ErrNotFound):
		return f, err
	}
	if state == loop.StateValidating && item.IssueNumber > 0 {
		is, err := rc.api.GetIssue(ctx, item.IssueNumber)
		if err != nil {
			return f, err
		}
		f.IssueCriteria = loop.IssueCriteria(is.Body)
	}
	return f, nil
}

// prFacts reads a PR as GitHub has it now.
func (e *Engine) prFacts(ctx context.Context, rc *repoCtx, number, issue int) (*loop.PRFacts, error) {
	pr, err := rc.api.PullRequest(ctx, number)
	if err != nil {
		return nil, err
	}
	p := &loop.PRFacts{
		Number: pr.Number, Open: pr.State == "open", Merged: pr.Merged,
		HeadSHA: pr.Head.SHA, HeadRef: pr.Head.Ref, MergeSHA: pr.MergeSHA, AuthorID: pr.User.ID,
		ClosesIssue: issue > 0 && loop.ClosesIssue(pr.Body, issue),
	}
	if !pr.Merged {
		p.MergeSHA = ""
	}
	files, err := rc.api.PRFiles(ctx, number)
	switch {
	case errors.Is(err, github.ErrTooManyFiles):
		// A partial list cannot clear a PR; it fails closed as a risk path.
		p.TooManyFiles = true
	case err != nil:
		return nil, err
	}
	for _, fl := range files {
		p.ChangedFiles = append(p.ChangedFiles, fl.Name)
		if fl.PreviousName != "" {
			p.PreviousNames = append(p.PreviousNames, fl.PreviousName)
		}
	}
	if p.HeadSHA == "" {
		return p, nil
	}
	// Check runs are listed by commit, so a fork PR at the same sha adds its
	// runs too and can fail a loop PR's build. That fails closed: the item
	// goes to a human, never to merge.
	runs, err := rc.api.CheckRuns(ctx, p.HeadSHA)
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		if r.Status == "completed" && failedConclusions[r.Conclusion] {
			p.ChecksFailed = true
		}
	}
	statuses, err := rc.api.Statuses(ctx, p.HeadSHA)
	if err != nil {
		return nil, err
	}
	loopSeen, latest := false, map[string]bool{}
	for _, s := range statuses { // newest first; each context's first is its current state
		if s.Context == github.StatusContext {
			// Only chuck's App may speak for chuck/loop. A status anyone
			// else set under that name is ignored, not trusted.
			if !loopSeen && s.Creator.ID == rc.bot {
				p.LoopStatus, loopSeen = s.State, true
			}
			continue
		}
		if latest[s.Context] {
			continue
		}
		latest[s.Context] = true
		if s.State == "failure" || s.State == "error" {
			p.ChecksFailed = true
		}
	}
	return p, nil
}

func containsInt(xs []int, x int) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

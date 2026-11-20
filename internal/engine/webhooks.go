package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/showmethebots/chuck/internal/github"
	"github.com/showmethebots/chuck/internal/loop"
	"github.com/showmethebots/chuck/internal/memory"
)

// payload is the part of a delivery the loop reads. It is signed: every
// identity and number comes from here, never from a header.
type payload struct {
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender github.User `json:"sender"`
	Label  *struct {
		Name string `json:"name"`
	} `json:"label"`
	Issue *struct {
		Number      int       `json:"number"`
		Title       string    `json:"title"`
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
	PullRequest *struct {
		Number int  `json:"number"`
		Merged bool `json:"merged"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
	Review *struct {
		State    string      `json:"state"`
		CommitID string      `json:"commit_id"`
		User     github.User `json:"user"`
	} `json:"review"`
	WorkflowRun *github.WorkflowRun `json:"workflow_run"`
}

// loopActions are the deliveries the transition table has rows for. Any
// other is recorded and left, before chuck spends a GitHub call on it.
var loopActions = map[string]bool{
	"issues.labeled":                true,
	"pull_request.labeled":          true,
	"pull_request.synchronize":      true,
	"pull_request.closed":           true,
	"pull_request_review.submitted": true,
	"workflow_run.completed":        true,
}

// errRetry wraps a failure worth retrying later: GitHub or the store
// could not be read. Anything else is final for the delivery.
type errRetry struct{ err error }

func (e errRetry) Error() string { return e.err.Error() }
func (e errRetry) Unwrap() error { return e.err }

func retry(err error) error { return errRetry{err} }

// processWebhooks handles stored deliveries in the order they arrived. A
// delivery that fails for a reason worth retrying stops the pass, so
// later deliveries for the same PR are not applied out of order; it is
// tried again on the next tick and given up after EscalateAfter tries.
func (e *Engine) processWebhooks(ctx context.Context) {
	for {
		hooks, err := e.Store.UnhandledWebhooks(ctx, 100)
		if err != nil {
			e.Log.Error("read unhandled webhooks", "err", err.Error())
			return
		}
		if len(hooks) == 0 {
			return
		}
		for _, w := range hooks {
			if f, ok := e.hookFails[w.ID]; ok && e.now().Before(f.next) {
				return
			}
			result, err := e.handleWebhook(ctx, w)
			var rerr errRetry
			if errors.As(err, &rerr) {
				f := e.hookFails[w.ID]
				f.n++
				if f.n < EscalateAfter {
					// Half a tick, so the next tick is past it however late
					// this pass ran.
					f.next = e.now().Add(e.tickEvery() / 2)
					e.hookFails[w.ID] = f
					e.Log.Warn("webhook not handled; retrying on the tick", "webhook_id", w.ID, "delivery", w.DeliveryID,
						"try", f.n, "err", err.Error())
					return
				}
				result = fmt.Sprintf("gave up after %d tries: %v", f.n, err)
			} else if err != nil {
				result = "error: " + err.Error()
			}
			delete(e.hookFails, w.ID)
			if err := e.Store.MarkWebhookHandled(ctx, w.ID, result, e.now()); err != nil {
				e.Log.Error("mark webhook handled", "webhook_id", w.ID, "err", err.Error())
				return
			}
			e.Log.Info("webhook handled", "webhook_id", w.ID, "delivery", w.DeliveryID,
				"kind", w.Event+"."+w.Action, "repo", w.Repo, "result", result)
		}
	}
}

func (e *Engine) tickEvery() time.Duration {
	if e.Tick > 0 {
		return e.Tick
	}
	return defaultTick
}

// handleWebhook maps one stored delivery to an input and applies it. The
// result is what the webhook row records.
func (e *Engine) handleWebhook(ctx context.Context, w memory.Webhook) (string, error) {
	if !loopActions[w.Event+"."+w.Action] {
		return "no rule for " + w.Event + "." + w.Action, nil
	}
	// The receiver already checked this; a stored row is checked again
	// before anything acts on its unsigned event name.
	if !github.PayloadMatchesEvent(w.Event, w.Payload) {
		return "payload does not match the event " + w.Event, nil
	}
	var p payload
	if err := json.Unmarshal(w.Payload, &p); err != nil {
		return "payload is not JSON", nil
	}
	owner, name, found := strings.Cut(p.Repository.FullName, "/")
	if !found {
		return "no repository", nil
	}
	row, err := e.Store.RepoByName(ctx, owner, name)
	if errors.Is(err, memory.ErrNotFound) {
		return "repo not in the loop", nil
	}
	if err != nil {
		return "", retry(err)
	}
	if p.Installation.ID != row.InstallationID {
		// Another App installed on the same repo delivers the same events.
		return fmt.Sprintf("delivered by installation %d, not the repo's %d", p.Installation.ID, row.InstallationID), nil
	}
	rc, err := e.openRepo(ctx, row)
	if errors.Is(err, errRepoOff) {
		return err.Error(), nil
	}
	if err != nil {
		return "", retry(err)
	}
	if rc, err = e.refreshConfig(ctx, rc); err != nil {
		if errors.Is(err, errRepoOff) {
			return err.Error(), nil
		}
		return "", retry(err)
	}

	in, kind, number, err := e.mapWebhook(ctx, rc, w.Event, p)
	if err != nil {
		return "", retry(err)
	}
	if in == nil {
		return kind, nil // kind holds why there is nothing to do
	}
	var item *memory.Item
	if number > 0 {
		it, err := e.Store.ItemByNumber(ctx, rc.row.ID, kind, number)
		switch {
		case err == nil:
			item = &it
		case !errors.Is(err, memory.ErrNotFound):
			return "", retry(err)
		}
	}
	res, err := e.input(ctx, rc, item, *in, meta{kind: "webhook", ref: w.DeliveryID, actor: "chuck"})
	if err != nil {
		return "", err
	}
	return describe(res), nil
}

// input gathers facts for an input and applies it. A failure to read facts
// is worth retrying; a failure to commit is not (the store refused it).
func (e *Engine) input(ctx context.Context, rc *repoCtx, item *memory.Item, in loop.Input, m meta) (loop.Result, error) {
	f, err := e.facts(ctx, rc, item, in)
	if err != nil {
		return loop.Result{}, retry(fmt.Errorf("facts: %w", err))
	}
	in.Facts = f
	return e.apply(ctx, rc, item, in, m)
}

func describe(res loop.Result) string {
	if len(res.Changes) == 0 {
		if len(res.Effects) > 0 {
			return res.Reason + " (effects only)"
		}
		return "no transition: " + res.Reason
	}
	var parts []string
	for _, c := range res.Changes {
		parts = append(parts, fmt.Sprintf("%s#%d → %s", c.Item.Kind, c.Item.Number, c.To))
	}
	return strings.Join(parts, ", ") + ": " + res.Reason
}

// mapWebhook turns a payload into a loop input and the item it is for.
// With a nil input, kind is the reason nothing applies.
func (e *Engine) mapWebhook(ctx context.Context, rc *repoCtx, event string, p payload) (*loop.Input, string, int, error) {
	wi := &loop.WebhookInput{Event: event, Action: p.Action, SenderID: p.Sender.ID}
	if p.Label != nil {
		wi.Label = p.Label.Name
	}
	switch event {
	case "issues":
		if p.Issue == nil || p.Issue.PullRequest != nil {
			return nil, "not an issue", 0, nil
		}
		wi.Number = p.Issue.Number
		return &loop.Input{Kind: loop.InputWebhook, Webhook: wi}, memory.KindIssue, wi.Number, nil
	case "pull_request":
		if p.PullRequest == nil {
			return nil, "no pull request", 0, nil
		}
		wi.Number, wi.SHA, wi.Merged = p.PullRequest.Number, p.PullRequest.Head.SHA, p.PullRequest.Merged
		return &loop.Input{Kind: loop.InputWebhook, Webhook: wi}, memory.KindPR, wi.Number, nil
	case "pull_request_review":
		if p.PullRequest == nil || p.Review == nil {
			return nil, "no review", 0, nil
		}
		// The reviewer is the one whose approval counts.
		wi.SenderID = p.Review.User.ID
		wi.Number, wi.SHA, wi.ReviewState = p.PullRequest.Number, p.Review.CommitID, strings.ToLower(p.Review.State)
		return &loop.Input{Kind: loop.InputWebhook, Webhook: wi}, memory.KindPR, wi.Number, nil
	case "workflow_run":
		r := p.WorkflowRun
		if r == nil {
			return nil, "no workflow run", 0, nil
		}
		fillRun(wi, *r, p.Repository.ID, rc.cfg.Deploy.DevWorkflow, rc.cfg.Deploy.ProdWorkflow)
		if wi.Workflow == "" {
			return nil, "not a deploy workflow: " + r.Path, 0, nil
		}
		n, err := e.prForMerge(ctx, rc, r.HeadSHA)
		if err != nil {
			return nil, "", 0, err
		}
		if n == 0 {
			return nil, "no loop PR merged at " + short(r.HeadSHA), 0, nil
		}
		return &loop.Input{Kind: loop.InputWebhook, Webhook: wi}, memory.KindPR, n, nil
	}
	return nil, "no rule for " + event, 0, nil
}

// fillRun maps a workflow run. The workflow is named by its file, which
// only a merged change can add or alter on the default branch, not by its
// display name, which any workflow can claim.
func fillRun(wi *loop.WebhookInput, r github.WorkflowRun, repoID int64, dev, prod string) {
	wi.SHA, wi.Branch, wi.Conclusion, wi.RunEvent = r.HeadSHA, r.HeadBranch, r.Conclusion, r.Event
	wi.SameRepo = repoID > 0 && r.HeadRepository.ID == repoID && r.Repository.ID == repoID
	if !strings.HasPrefix(r.Path, ".github/workflows/") {
		return
	}
	file := path.Base(r.Path)
	stem := strings.TrimSuffix(strings.TrimSuffix(file, ".yml"), ".yaml")
	for _, want := range []string{dev, prod} {
		if want != "" && (file == want || stem == want) {
			wi.Workflow = want
			return
		}
	}
}

// prForMerge finds the loop PR, waiting on a deploy, merged at sha.
func (e *Engine) prForMerge(ctx context.Context, rc *repoCtx, sha string) (int, error) {
	if sha == "" {
		return 0, nil
	}
	items, err := e.Store.Items(ctx, rc.row.ID)
	if err != nil {
		return 0, err
	}
	for _, it := range items {
		if it.Kind != memory.KindPR || (it.State != string(loop.StateMerged) && it.State != string(loop.StatePromoting)) {
			continue
		}
		pr, err := rc.api.PullRequest(ctx, it.Number)
		if err != nil {
			return 0, err
		}
		if pr.Merged && pr.MergeSHA == sha {
			return it.Number, nil
		}
	}
	return 0, nil
}

package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/showmethebots/chuck/internal/github"
	"github.com/showmethebots/chuck/internal/memory"
)

const (
	human    = int64(40615752)
	stranger = int64(666)
	bot      = int64(9000)
	instID   = int64(7)
	repoGHID = int64(500)
	head     = "1111111111111111111111111111111111111111"
	newHead  = "2222222222222222222222222222222222222222"
	mergeSHA = "3333333333333333333333333333333333333333"
)

var t0 = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

const validLoop = `{
  "default_branch": "main", "wip_limit": 1, "merge_method": "SQUASH",
  "auto_advance": true, "auto_merge": true, "auto_promote": false, "ratchet": false,
  "limits": {"build_attempts": 2, "review_rounds": 3, "stale_hours": 6},
  "models": {"default": "claude-sonnet-5-5"}, "max_turns": {"default": 60},
  "machines": {"default": "shared-cpu-2x:2048"},
  "risk_paths": ["db/migrations/**"],
  "deploy": {"dev_url": "https://dev.example.com", "dev_workflow": "deploy-dev", "prod_workflow": "deploy-prod.yml"},
  "validate": {"e2e_command": "npx playwright test", "smoke": []}
}`

type fakeIssue struct {
	body     string
	labels   []string
	comments []github.IssueComment
	events   []github.IssueEvent
	subs     []int
}

// fakeGitHub is one repository and one App, in memory. fail makes a
// method return an error until cleared; calls counts every method.
type fakeGitHub struct {
	mu       sync.Mutex
	loopJSON string // "" means no loop.json
	loopSHA  string
	issues   map[int]*fakeIssue
	prs      map[int]*github.PR
	files    map[int][]github.File
	reviews  map[int][]github.Review
	runs     map[string][]github.CheckRun
	statuses map[string][]github.Status
	wfRuns   map[string][]github.WorkflowRun
	// dispatchRuns are listed by workflow file, not head sha.
	dispatchRuns []github.WorkflowRun
	// clock dates the events labelBy records.
	clock     func() time.Time
	fail      map[string]error
	calls     map[string]int
	setStatus []string // "sha state"
	merged    []string
	dispatch  []string
	nextID    int64
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		loopJSON: validLoop, loopSHA: "blob1",
		issues: map[int]*fakeIssue{}, prs: map[int]*github.PR{}, files: map[int][]github.File{},
		reviews: map[int][]github.Review{}, runs: map[string][]github.CheckRun{},
		statuses: map[string][]github.Status{}, wfRuns: map[string][]github.WorkflowRun{},
		fail: map[string]error{}, calls: map[string]int{},
	}
}

func (f *fakeGitHub) enter(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
	return f.fail[name]
}

func (f *fakeGitHub) setFail(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.fail, name)
	} else {
		f.fail[name] = err
	}
}

func (f *fakeGitHub) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeGitHub) issue(n int) *fakeIssue {
	if f.issues[n] == nil {
		f.issues[n] = &fakeIssue{}
	}
	return f.issues[n]
}

// labelBy adds a label as a person would, with the event GitHub records.
func (f *fakeGitHub) labelBy(n int, label string, actor int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issue(n)
	is.labels = append(is.labels, label)
	f.nextID++
	ev := github.IssueEvent{ID: f.nextID, Event: "labeled", Actor: github.User{ID: actor}, CreatedAt: f.clock()}
	ev.Label.Name = label
	is.events = append(is.events, ev)
}

func (f *fakeGitHub) openPR(n, issue int) *github.PR {
	pr := &github.PR{Number: n, NodeID: fmt.Sprintf("PR_%d", n), State: "open", Body: fmt.Sprintf("Closes #%d", issue),
		User: github.User{ID: bot, Type: "Bot"}}
	pr.Head.SHA, pr.Head.Ref = head, fmt.Sprintf("loop/issue-%d", issue)
	f.prs[n] = pr
	f.files[n] = []github.File{{Name: "src/app.ts"}}
	f.issue(n)
	return pr
}

// Source.

func (f *fakeGitHub) Apps() []string { return []string{"showmethebots"} }
func (f *fakeGitHub) Installations(context.Context, string) ([]Installation, error) {
	if err := f.enter("Installations"); err != nil {
		return nil, err
	}
	r := github.Repository{ID: repoGHID, Name: "demos", FullName: "showmethebots/demos", DefaultBranch: "main"}
	r.Owner.Login = "showmethebots"
	return []Installation{{ID: instID, Repos: []github.Repository{r}}}, nil
}
func (f *fakeGitHub) Repo(memory.Repo) (RepoAPI, error)            { return f, nil }
func (f *fakeGitHub) BotID(context.Context, string) (int64, error) { return bot, nil }

// RepoAPI reads.

func (f *fakeGitHub) GetIssue(_ context.Context, n int) (github.Issue, error) {
	if err := f.enter("GetIssue"); err != nil {
		return github.Issue{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issue(n)
	out := github.Issue{Number: n, Body: is.body, State: "open"}
	for _, l := range is.labels {
		out.Labels = append(out.Labels, struct {
			Name string `json:"name"`
		}{l})
	}
	return out, nil
}

func (f *fakeGitHub) IssueComments(_ context.Context, n int) ([]github.IssueComment, error) {
	if err := f.enter("IssueComments"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]github.IssueComment(nil), f.issue(n).comments...), nil
}

func (f *fakeGitHub) IssueEvents(_ context.Context, n int) ([]github.IssueEvent, error) {
	if err := f.enter("IssueEvents"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]github.IssueEvent(nil), f.issue(n).events...), nil
}

func (f *fakeGitHub) SubIssues(_ context.Context, n int) ([]int, error) {
	if err := f.enter("SubIssues"); err != nil {
		return nil, err
	}
	return f.issue(n).subs, nil
}

func (f *fakeGitHub) OpenIssuesWithLabel(_ context.Context, label string) ([]github.Issue, error) {
	if err := f.enter("OpenIssuesWithLabel"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []github.Issue
	for n, is := range f.issues {
		for _, l := range is.labels {
			if l == label {
				g := github.Issue{Number: n}
				if f.prs[n] != nil {
					g.PullRequest = &struct{}{}
				}
				out = append(out, g)
			}
		}
	}
	return out, nil
}

func (f *fakeGitHub) PullRequest(_ context.Context, n int) (github.PR, error) {
	if err := f.enter("PullRequest"); err != nil {
		return github.PR{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, ok := f.prs[n]
	if !ok {
		return github.PR{}, &github.APIError{Status: 404, Message: "Not Found"}
	}
	return *pr, nil
}

func (f *fakeGitHub) PRFiles(_ context.Context, n int) ([]github.File, error) {
	if err := f.enter("PRFiles"); err != nil {
		return nil, err
	}
	return f.files[n], nil
}

func (f *fakeGitHub) Reviews(_ context.Context, n int) ([]github.Review, error) {
	if err := f.enter("Reviews"); err != nil {
		return nil, err
	}
	return f.reviews[n], nil
}

func (f *fakeGitHub) CheckRuns(_ context.Context, sha string) ([]github.CheckRun, error) {
	if err := f.enter("CheckRuns"); err != nil {
		return nil, err
	}
	return f.runs[sha], nil
}

func (f *fakeGitHub) Statuses(_ context.Context, sha string) ([]github.Status, error) {
	if err := f.enter("Statuses"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]github.Status(nil), f.statuses[sha]...), nil
}

func (f *fakeGitHub) WorkflowRuns(_ context.Context, sha string) ([]github.WorkflowRun, error) {
	if err := f.enter("WorkflowRuns"); err != nil {
		return nil, err
	}
	return f.wfRuns[sha], nil
}

func (f *fakeGitHub) DispatchRuns(_ context.Context, wf string, since time.Time) ([]github.WorkflowRun, error) {
	if err := f.enter("DispatchRuns"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []github.WorkflowRun
	for _, r := range f.dispatchRuns {
		if r.Event == "workflow_dispatch" && strings.HasSuffix(r.Path, "/"+wf) && !r.CreatedAt.Before(since) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeGitHub) ReadFile(_ context.Context, path, ref string) (github.Blob, error) {
	if err := f.enter("ReadFile"); err != nil {
		return github.Blob{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if path != github.LoopConfigPath || ref != "main" {
		return github.Blob{}, fmt.Errorf("read %s at %s", path, ref)
	}
	if f.loopJSON == "" {
		return github.Blob{}, github.ErrNotFound
	}
	return github.Blob{SHA: f.loopSHA, Content: []byte(f.loopJSON)}, nil
}

func (f *fakeGitHub) UserByID(_ context.Context, id int64) (github.User, error) {
	if err := f.enter("UserByID"); err != nil {
		return github.User{}, err
	}
	return github.User{ID: id, Login: "cwinters8"}, nil
}

// RepoAPI writes.

func (f *fakeGitHub) AddLabel(_ context.Context, n int, label string) error {
	if err := f.enter("AddLabel"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issue(n)
	is.labels = append(is.labels, label)
	return nil
}

func (f *fakeGitHub) RemoveLabel(_ context.Context, n int, label string) error {
	if err := f.enter("RemoveLabel"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issue(n)
	var keep []string
	for _, l := range is.labels {
		if l != label {
			keep = append(keep, l)
		}
	}
	is.labels = keep
	return nil
}

func (f *fakeGitHub) Comment(_ context.Context, n int, body, marker string) (string, error) {
	if err := f.enter("Comment"); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	is := f.issue(n)
	is.comments = append(is.comments, github.IssueComment{ID: f.nextID, Body: body + "\n\n" + github.MarkerText(marker),
		User: github.User{ID: bot}})
	return "", nil
}

func (f *fakeGitHub) SetStatus(_ context.Context, sha, state, _, _ string) error {
	if err := f.enter("SetStatus"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setStatus = append(f.setStatus, sha+" "+state)
	f.statuses[sha] = append([]github.Status{{State: state, Context: github.StatusContext, Creator: github.User{ID: bot}}}, f.statuses[sha]...)
	return nil
}

func (f *fakeGitHub) DispatchWorkflow(_ context.Context, wf, ref string, in map[string]string) error {
	if err := f.enter("DispatchWorkflow"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dispatch = append(f.dispatch, wf+"@"+ref+" "+in["sha"])
	return nil
}

func (f *fakeGitHub) AutoMerge(_ context.Context, pr github.PR, sha, method string) (bool, error) {
	if err := f.enter("AutoMerge"); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merged = append(f.merged, fmt.Sprintf("#%d %s %s", pr.Number, sha, method))
	return false, nil
}

// fakeLauncher records launches; with no open-run bookkeeping of its own,
// a test that needs the run inserts it.
type fakeLauncher struct {
	mu        sync.Mutex
	launched  []string
	destroyed []int64
	err       error
}

func (l *fakeLauncher) Launch(_ context.Context, r LaunchRequest) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.launched = append(l.launched, fmt.Sprintf("%s#%d:%s", r.Item.Kind, r.Item.Number, r.Stage))
	return nil
}

func (l *fakeLauncher) Destroy(_ context.Context, id int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.destroyed = append(l.destroyed, id)
	return l.err
}

type fixture struct {
	t     *testing.T
	ctx   context.Context
	store *memory.Store
	gh    *fakeGitHub
	lnch  *fakeLauncher
	e     *Engine
	now   time.Time
	repo  memory.Repo
	hooks int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	s, err := memory.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	fx := &fixture{t: t, ctx: ctx, store: s, gh: newFakeGitHub(), lnch: &fakeLauncher{}, now: t0}
	fx.gh.clock = func() time.Time { return fx.now }
	fx.e = &Engine{Store: s, GitHub: fx.gh, Launcher: fx.lnch, Humans: []int64{human},
		Tick: 30 * time.Minute, Retention: 720 * time.Hour,
		Log: slog.New(slog.NewTextHandler(logOut, nil)), Now: func() time.Time { return fx.now }}
	return fx
}

// discover runs discovery and loads the repo row.
func (fx *fixture) discover() {
	fx.t.Helper()
	fx.e.init()
	fx.e.discover(fx.ctx)
	r, err := fx.store.RepoByName(fx.ctx, "showmethebots", "demos")
	if err != nil {
		fx.t.Fatal(err)
	}
	fx.repo = r
}

func (fx *fixture) tick() {
	fx.t.Helper()
	fx.now = fx.now.Add(30 * time.Minute)
	fx.e.TickOnce(fx.ctx)
}

// seed puts an item in a state directly, as earlier transitions would.
func (fx *fixture) seed(kind string, number, issue int, state string) memory.Item {
	fx.t.Helper()
	c, err := fx.store.Commit(fx.ctx, fx.now, memory.Change{
		New: &memory.Item{RepoID: fx.repo.ID, Kind: kind, Number: number, IssueNumber: issue},
		To:  state, InputKind: "dashboard", Actor: "test",
	})
	if err != nil {
		fx.t.Fatal(err)
	}
	it, err := fx.store.ItemByID(fx.ctx, c[0].ItemID)
	if err != nil {
		fx.t.Fatal(err)
	}
	return it
}

func (fx *fixture) item(kind string, number int) memory.Item {
	fx.t.Helper()
	it, err := fx.store.ItemByNumber(fx.ctx, fx.repo.ID, kind, number)
	if err != nil {
		fx.t.Fatalf("%s#%d: %v", kind, number, err)
	}
	return it
}

func (fx *fixture) state(kind string, number int) string { return fx.item(kind, number).State }

// deliver stores a delivery as the receiver would and handles it.
func (fx *fixture) deliver(event, action string, body string) memory.Webhook {
	fx.t.Helper()
	fx.hooks++
	payload := fmt.Sprintf(`{"action":%q,"installation":{"id":%d},"repository":{"id":%d,"full_name":"showmethebots/demos"},%s}`,
		action, instID, repoGHID, body)
	id, _, err := fx.store.InsertWebhook(fx.ctx, memory.Webhook{DeliveryID: fmt.Sprintf("d-%d", fx.hooks),
		Event: event, Action: action, Repo: "showmethebots/demos", Payload: []byte(payload), ReceivedAt: fx.now})
	if err != nil {
		fx.t.Fatal(err)
	}
	fx.e.init()
	fx.e.mu.Lock()
	fx.e.processWebhooks(fx.ctx)
	fx.e.mu.Unlock()
	w, err := fx.store.WebhookByID(fx.ctx, id)
	if err != nil {
		fx.t.Fatal(err)
	}
	return w
}

// latestEffects decodes an item's newest transition's effect records.
func (fx *fixture) latestEffects(kind string, number int) (memory.Transition, []effectRecord) {
	fx.t.Helper()
	tr, err := fx.store.LatestTransition(fx.ctx, fx.item(kind, number).ID)
	if err != nil {
		fx.t.Fatal(err)
	}
	rs, err := decodeRecords(tr.Effects)
	if err != nil {
		fx.t.Fatal(err)
	}
	return tr, rs
}

func statuses(rs []effectRecord) string {
	var out []string
	for _, r := range rs {
		out = append(out, string(r.Kind)+"="+r.Result.Status)
	}
	return strings.Join(out, " ")
}

var logOut io.Writer = io.Discard

var errBoom = errors.New("github is down")

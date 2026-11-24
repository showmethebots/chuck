package worker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/showmethebots/chuck/internal/config"
	"github.com/showmethebots/chuck/internal/engine"
	"github.com/showmethebots/chuck/internal/github"
	"github.com/showmethebots/chuck/internal/loop"
	"github.com/showmethebots/chuck/internal/memory"
)

const (
	human    = int64(40615752)
	stranger = int64(666)
	bot      = int64(9000)
	mergeSHA = "3333333333333333333333333333333333333333"
)

var t0 = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

const validLoop = `{
  "default_branch": "main", "wip_limit": 1, "merge_method": "SQUASH",
  "auto_advance": true, "auto_merge": true, "auto_promote": false, "ratchet": false,
  "limits": {"build_attempts": 2, "review_rounds": 3, "stale_hours": 6, "wall_clock": {"build": "90m"}},
  "models": {"default": "claude-sonnet-5-5", "review": "claude-opus-5-5"}, "max_turns": {"default": 60, "build": 120},
  "machines": {"default": "shared-cpu-2x:2048", "validate": "performance-cpu-4x:8192"},
  "risk_paths": [],
  "deploy": {"dev_url": "https://dev.example.com", "dev_workflow": "deploy-dev", "prod_workflow": "deploy-prod.yml"},
  "validate": {"e2e_command": "npx playwright test", "smoke": ["sign in"]}
}`

// fakeAPI is the GitHub reads a launch makes. Anything else panics: the
// launcher must not call it.
type fakeAPI struct {
	engine.RepoAPI
	issueComments  map[int][]github.IssueComment
	reviews        map[int][]github.Review
	reviewComments map[int][]github.ReviewComment
	prs            map[int]github.PR
	err            error
}

// fakeRepos hands out fakeAPI as chuck's App's view of the repo.
type fakeRepos struct{ api *fakeAPI }

func (f fakeRepos) Repo(memory.Repo) (engine.RepoAPI, error)     { return f.api, nil }
func (f fakeRepos) BotID(context.Context, string) (int64, error) { return bot, nil }

func (f *fakeAPI) IssueComments(_ context.Context, n int) ([]github.IssueComment, error) {
	return f.issueComments[n], f.err
}
func (f *fakeAPI) Reviews(_ context.Context, n int) ([]github.Review, error) {
	return f.reviews[n], f.err
}
func (f *fakeAPI) ReviewComments(_ context.Context, n int) ([]github.ReviewComment, error) {
	return f.reviewComments[n], f.err
}
func (f *fakeAPI) PullRequest(_ context.Context, n int) (github.PR, error) {
	pr, ok := f.prs[n]
	if !ok {
		return pr, &github.APIError{Status: 404}
	}
	return pr, f.err
}

// fakeTokens mints the stage's set, or perms when set.
type fakeTokens struct {
	perms github.Permissions
	calls []string
}

func (f *fakeTokens) WorkerToken(_ context.Context, r memory.Repo, s config.Stage) (github.Token, error) {
	f.calls = append(f.calls, r.Name+":"+string(s))
	p := github.StagePermissions[s]
	if f.perms != nil {
		p = f.perms
	}
	return github.Token{Value: "ghs_" + string(s), ExpiresAt: t0.Add(time.Hour), Permissions: p}, nil
}

// fakeVerdicts ends the run as the engine would, and records the input.
type fakeVerdicts struct {
	store *memory.Store
	now   func() time.Time
	mu    sync.Mutex
	got   []loop.VerdictInput
	ends  []memory.RunEnd
	err   error
}

func (f *fakeVerdicts) Verdict(ctx context.Context, v loop.VerdictInput, e memory.RunEnd) (loop.Result, error) {
	if f.err != nil {
		return loop.Result{}, f.err
	}
	if err := f.store.EndRun(ctx, v.RunID, e, f.now()); err != nil {
		return loop.Result{}, err
	}
	f.mu.Lock()
	f.got, f.ends = append(f.got, v), append(f.ends, e)
	f.mu.Unlock()
	return loop.Result{}, nil
}

type fixture struct {
	t      *testing.T
	ctx    context.Context
	store  *memory.Store
	fake   *Fake
	tokens *fakeTokens
	vs     *fakeVerdicts
	api    *fakeAPI
	m      *Manager
	srv    *httptest.Server
	repo   memory.Repo
	cfg    *config.Loop
	now    time.Time
	env    map[string]string

	revMu   sync.Mutex
	revoked []string
	revErr  error
}

// revokedTokens is every GitHub token the Manager revoked, in order.
func (fx *fixture) revokedTokens() []string {
	fx.revMu.Lock()
	defer fx.revMu.Unlock()
	return append([]string(nil), fx.revoked...)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	s, err := memory.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	fx := &fixture{t: t, ctx: ctx, store: s, fake: &Fake{}, tokens: &fakeTokens{}, now: t0,
		api: &fakeAPI{issueComments: map[int][]github.IssueComment{}, reviews: map[int][]github.Review{},
			reviewComments: map[int][]github.ReviewComment{}, prs: map[int]github.PR{}},
		env: map[string]string{}}
	clock := func() time.Time { return fx.now }
	fx.vs = &fakeVerdicts{store: s, now: clock}
	fx.m = &Manager{Store: s, Launcher: fx.fake, Tokens: fx.tokens, Verdicts: fx.vs,
		URL: "https://chuck.example.com", Image: "registry.fly.io/chuck-workers:abc",
		WallClockCeiling: 4 * time.Hour, Spend: config.Spend{MonthlyUSD: 100, PerRepoMonthlyUSD: 60},
		Getenv: func(k string) string { return fx.env[k] },
		Log:    slog.New(slog.DiscardHandler), Now: clock}
	fx.m.Repos = fakeRepos{fx.api}
	// Never reach api.github.com from a test.
	fx.m.Revoke = func(_ context.Context, tok string) error {
		fx.revMu.Lock()
		defer fx.revMu.Unlock()
		if fx.revErr != nil {
			return fx.revErr
		}
		fx.revoked = append(fx.revoked, tok)
		return nil
	}
	fx.srv = httptest.NewServer(fx.m.Handler())
	t.Cleanup(fx.srv.Close)

	id, err := s.UpsertRepo(ctx, memory.Repo{Owner: "showmethebots", Name: "demos", InstallationID: 7,
		AppOwner: "showmethebots", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepoConfig(ctx, id, validLoop, "blob1", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepoEnabled(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if fx.repo, err = s.RepoByName(ctx, "showmethebots", "demos"); err != nil {
		t.Fatal(err)
	}
	if fx.cfg, err = config.ParseLoop([]byte(validLoop)); err != nil {
		t.Fatal(err)
	}
	return fx
}

// item creates an issue, or a PR serving issue 12, in state.
func (fx *fixture) item(kind string, number int, state string) memory.Item {
	fx.t.Helper()
	n := &memory.Item{RepoID: fx.repo.ID, Kind: kind, Number: number, Title: "t"}
	if kind == memory.KindPR {
		n.IssueNumber = 12
	}
	c, err := fx.store.Commit(fx.ctx, fx.now, memory.Change{New: n, To: state, InputKind: "tick", Actor: "chuck"})
	if err != nil {
		fx.t.Fatal(err)
	}
	it, err := fx.store.ItemByID(fx.ctx, c[0].ItemID)
	if err != nil {
		fx.t.Fatal(err)
	}
	return it
}

// reviewed records a finished review run on a PR item that reported sha,
// with its findings posted at url, reporting the hash of the comment's
// body as it is now in fx.api.
func (fx *fixture) reviewed(it memory.Item, sha, url string) {
	fx.t.Helper()
	posted := ""
	for _, c := range fx.api.issueComments[it.Number] {
		if c.HTMLURL == url {
			posted = c.Body
		}
	}
	fx.reviewedAs(it, sha, url, posted)
}

// reviewedAs is reviewed with the body the review worker says it posted.
func (fx *fixture) reviewedAs(it memory.Item, sha, url, posted string) {
	fx.t.Helper()
	r, err := fx.store.InsertRun(fx.ctx, memory.Run{ItemID: it.ID, Stage: "review", Image: "i", Model: "m",
		Credential: memory.CredentialAPIKey, Prompt: "p", SchemaName: "review", Ref: "r",
		TokenHash: memory.HashToken(fmt.Sprint("review-", it.ID)), DeadlineAt: fx.now.Add(time.Hour)}, fx.now)
	if err != nil {
		fx.t.Fatal(err)
	}
	_ = fx.store.MarkRunRunning(fx.ctx, r.ID, fx.now)
	v := `{"outcome":"posted","summary":"s","reviewed_sha":"` + sha + `","comment_url":"` + url + `","comment_sha256":"` + bodySHA(posted) + `","findings_count":1,"skills_not_run":[]}`
	// As the run API does on accepting the verdict.
	rv, err := loop.ParseVerdict(config.StageReview, []byte(v))
	if err != nil {
		fx.t.Fatal(err)
	}
	fx.m.snapshotFindings(fx.ctx, r, rv.Review)
	if err := fx.store.EndRun(fx.ctx, r.ID, memory.RunEnd{Status: memory.RunFinished, VerdictJSON: v}, fx.now); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *fixture) req(it memory.Item, stage config.Stage) engine.LaunchRequest {
	return engine.LaunchRequest{Repo: fx.repo, Config: fx.cfg, Item: it, Stage: stage, API: fx.api,
		BotID: bot, Humans: []int64{human}}
}

// launch runs a launch and returns its run and Machine.
func (fx *fixture) launch(it memory.Item, stage config.Stage) (memory.Run, FakeMachine) {
	fx.t.Helper()
	if err := fx.m.Launch(fx.ctx, fx.req(it, stage)); err != nil {
		fx.t.Fatalf("launch %s: %v", stage, err)
	}
	run, err := fx.store.OpenRun(fx.ctx, it.ID)
	if err != nil {
		fx.t.Fatal(err)
	}
	mc, ok := fx.fake.Get(run.MachineID)
	if !ok {
		fx.t.Fatalf("run %d has machine %q, which was never started", run.ID, run.MachineID)
	}
	return run, mc
}

func (fx *fixture) run(id int64) memory.Run {
	fx.t.Helper()
	r, err := fx.store.RunByID(fx.ctx, id)
	if err != nil {
		fx.t.Fatal(err)
	}
	return r
}

func (fx *fixture) call(method, path, token string, body io.Reader, ctype string) (int, []byte) {
	fx.t.Helper()
	req, err := http.NewRequest(method, fx.srv.URL+path, body)
	if err != nil {
		fx.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := fx.srv.Client().Do(req)
	if err != nil {
		fx.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (fx *fixture) getSpec(run memory.Run, token string) (int, Spec) {
	fx.t.Helper()
	code, b := fx.call(http.MethodGet, fmt.Sprintf("/api/runs/%d/spec", run.ID), token, nil, "")
	var s Spec
	if code == http.StatusOK {
		if err := json.Unmarshal(b, &s); err != nil {
			fx.t.Fatal(err)
		}
	}
	return code, s
}

// verdictBody builds the multipart upload; evidence nil sends no part.
func verdictBody(t *testing.T, post any, evidence []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	pw, err := w.CreateFormField("verdict")
	if err != nil {
		t.Fatal(err)
	}
	switch p := post.(type) {
	case string:
		_, _ = pw.Write([]byte(p))
	default:
		if err := json.NewEncoder(pw).Encode(p); err != nil {
			t.Fatal(err)
		}
	}
	if evidence != nil {
		ew, err := w.CreateFormFile("evidence", "evidence.tar")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = ew.Write(evidence)
	}
	_ = w.Close()
	return &buf, w.FormDataContentType()
}

func (fx *fixture) postVerdict(run memory.Run, token string, post any, evidence []byte) (int, string) {
	fx.t.Helper()
	body, ct := verdictBody(fx.t, post, evidence)
	code, b := fx.call(http.MethodPost, fmt.Sprintf("/api/runs/%d/verdict", run.ID), token, body, ct)
	return code, string(b)
}

func tarOf(t *testing.T, files map[string][]byte, order ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range order {
		b := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(b)
	}
	_ = tw.Close()
	return buf.Bytes()
}

func contains(s string, subs ...string) bool {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			return false
		}
	}
	return true
}

var buildOK = map[string]any{"structured_output": map[string]any{
	"outcome": "pr_opened", "summary": "did it", "pr_number": 13, "tests_added": []string{},
	"tests_retired": []string{}, "blockers": []string{}, "skills_not_run": []string{},
}, "exit_code": 0, "duration_ms": 1234, "log_tail": "done"}

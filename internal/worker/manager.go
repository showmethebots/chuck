package worker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/showmethebots/chuck/internal/config"
	"github.com/showmethebots/chuck/internal/engine"
	"github.com/showmethebots/chuck/internal/github"
	"github.com/showmethebots/chuck/internal/loop"
	"github.com/showmethebots/chuck/internal/memory"
)

// Tokens mints a worker run's GitHub token: the one repository, the
// stage's permission set. engine.AppSource implements it.
type Tokens interface {
	WorkerToken(ctx context.Context, repo memory.Repo, stage config.Stage) (github.Token, error)
}

// Verdicts applies a run's verdict. *engine.Engine implements it.
type Verdicts interface {
	Verdict(ctx context.Context, v loop.VerdictInput, end memory.RunEnd) (loop.Result, error)
}

// Manager launches, serves, watches and destroys stage workers. It is the
// engine's Launcher.
type Manager struct {
	Store    *memory.Store
	Launcher Launcher
	Tokens   Tokens
	Verdicts Verdicts // set once the engine exists
	// URL is chuck as a worker reaches it: the base of the run API and the
	// Claude proxy.
	URL              string
	Image            string
	WallClockCeiling time.Duration
	Spend            config.Spend
	// Getenv reads a repo's validate credentials (LOOP_ENV_<owner>_<repo>)
	// when its worker asks for its spec. They never go in the Machine
	// config or the runs row.
	Getenv func(string) string
	Log    *slog.Logger
	Now    func() time.Time

	// Watcher timings; zero means the spec's.
	Poll         time.Duration // 30 s
	StartTimeout time.Duration // how long a Machine may take to fetch its spec: 10 m
	Overrun      time.Duration // past the deadline before chuck stops it: 10 m
	StopGrace    time.Duration // SIGTERM to destroy: 60 s
	// ExitGrace is how long a Machine may outlive its verdict before the
	// watcher destroys it: 2 m.
	ExitGrace time.Duration

	// Revoke revokes a GitHub token a run was given; nil means
	// github.RevokeToken against api.github.com.
	Revoke func(ctx context.Context, token string) error

	mu     sync.Mutex
	minted map[int64][]string // run id → GitHub tokens served, until the run ends
}

var _ engine.Launcher = (*Manager)(nil)

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *Manager) log() *slog.Logger {
	if m.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return m.Log
}

// newToken is a run token: 32 random bytes. Only its hash is stored.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Launch starts a worker for one launch effect. The engine calls it with
// its lock held and only when the item has no open run. The order is the
// spec's: check spend, record the run with its token's hash and deadline,
// then create the Machine. A Machine that fails to start ends the run as
// failed before it started, so the tick's retry relaunches it as the same
// attempt.
func (m *Manager) Launch(ctx context.Context, req engine.LaunchRequest) error {
	now := m.now()
	if err := m.checkSpend(ctx, req.Repo.ID, now); err != nil {
		return err
	}
	stage := req.Stage
	attempt, err := m.Store.NextAttempt(ctx, req.Item.ID, string(stage))
	if err != nil {
		return err
	}
	p, err := m.prepare(ctx, req, attempt)
	if err != nil {
		return err
	}
	prompt, err := loop.Prompt(stage, p.context)
	if err != nil {
		return err
	}
	cred := memory.CredentialAPIKey
	switch until, err := m.Store.APIKeyExhaustedUntil(ctx); {
	case err != nil:
		return err
	case now.Before(until):
		cred = memory.CredentialOAuth
	}
	token, err := newToken()
	if err != nil {
		return err
	}
	run, err := m.Store.InsertRun(ctx, memory.Run{
		ItemID: req.Item.ID, Stage: string(stage), Image: m.Image, Model: req.Config.ModelFor(stage),
		Credential: cred, Prompt: prompt, SchemaName: loop.SchemaName(stage), Ref: p.ref,
		TokenHash: memory.HashToken(token), DeadlineAt: now.Add(req.Config.WallClockFor(stage, m.WallClockCeiling)),
	}, now)
	if err != nil {
		return err
	}
	if run.Attempt != attempt {
		// Nothing else launches while the engine holds its lock; refuse
		// rather than run a prompt that names the wrong attempt.
		_ = m.Store.EndRun(ctx, run.ID, memory.RunEnd{Status: memory.RunFailed, Reason: "attempt_moved"}, now)
		return fmt.Errorf("run %d is attempt %d, but its prompt was built for %d", run.ID, run.Attempt, attempt)
	}
	if err := m.Store.InsertIgnoredFeedback(ctx, run.ID, p.ignored, now); err != nil {
		m.log().Error("record ignored feedback", "run", run.ID, "err", err.Error())
	}
	id, region, err := m.Launcher.Start(ctx, Machine{
		RunID: run.ID, Image: m.Image, Size: req.Config.MachineFor(stage),
		Env: map[string]string{
			EnvURL:   m.URL,
			EnvRunID: strconv.FormatInt(run.ID, 10),
			EnvToken: token,
		},
	})
	if err != nil {
		if eerr := m.Store.EndRun(ctx, run.ID, memory.RunEnd{Status: memory.RunFailed, Reason: "launch_failed"}, m.now()); eerr != nil {
			m.log().Error("end run after a failed launch", "run", run.ID, "err", eerr.Error())
		}
		return fmt.Errorf("start machine for run %d: %w", run.ID, err)
	}
	if err := m.Store.SetRunMachine(ctx, run.ID, id, region); err != nil {
		// The watcher finds the run without a Machine and ends it after
		// StartTimeout; this Machine's own token dies with the run.
		m.log().Error("record machine", "run", run.ID, "machine", id, "err", err.Error())
		_ = m.Launcher.Destroy(ctx, id)
		return err
	}
	m.log().Info("worker launched", "run", run.ID, "stage", string(stage), "attempt", run.Attempt,
		"machine", id, "region", region, "credential", cred)
	return nil
}

// Destroy stops and destroys a run's Machine, for a cancelled run.
func (m *Manager) Destroy(ctx context.Context, runID int64) error {
	run, err := m.Store.RunByID(ctx, runID)
	if err != nil {
		return err
	}
	return m.destroy(ctx, run)
}

func (m *Manager) destroy(ctx context.Context, run memory.Run) error {
	m.revoke(ctx, run.ID)
	if run.MachineID == "" || !run.DestroyedAt.IsZero() {
		return nil
	}
	if err := m.Launcher.Destroy(ctx, run.MachineID); err != nil {
		return err
	}
	if err := m.Store.MarkRunDestroyed(ctx, run.ID, m.now()); err != nil && !errors.Is(err, memory.ErrStale) {
		return err
	}
	return nil
}

// remember keeps a GitHub token served to a run so it can be revoked when
// the run ends. It lives in memory only: a restart forgets it, and such a
// token lives out GitHub's one-hour TTL.
func (m *Manager) remember(runID int64, token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.minted == nil {
		m.minted = map[int64][]string{}
	}
	m.minted[runID] = append(m.minted[runID], token)
}

// revoke revokes every GitHub token served to a run that has ended, so a
// token a worker leaked stops working with its run, not an hour later. A
// token whose revocation fails stays listed for the watcher's next pass.
func (m *Manager) revoke(ctx context.Context, runID int64) {
	m.mu.Lock()
	tokens := m.minted[runID]
	delete(m.minted, runID)
	m.mu.Unlock()
	rev := m.Revoke
	if rev == nil {
		rev = func(ctx context.Context, t string) error {
			return github.RevokeToken(ctx, &http.Client{Timeout: 30 * time.Second}, "", t)
		}
	}
	var keep []string
	for _, t := range tokens {
		if err := rev(ctx, t); err != nil {
			m.log().Warn("revoke worker token", "run", runID, "err", err.Error())
			keep = append(keep, t)
		}
	}
	if len(keep) > 0 {
		m.mu.Lock()
		if m.minted == nil {
			m.minted = map[int64][]string{}
		}
		m.minted[runID] = append(m.minted[runID], keep...)
		m.mu.Unlock()
	}
}

// revokeEnded revokes the tokens of every run that is no longer open,
// whichever path ended it: a verdict, the watcher, a cancel, an escalation.
func (m *Manager) revokeEnded(ctx context.Context) {
	m.mu.Lock()
	ids := make([]int64, 0, len(m.minted))
	for id := range m.minted {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		r, err := m.Store.RunByID(ctx, id)
		if err != nil || r.Status == memory.RunLaunching || r.Status == memory.RunRunning {
			continue
		}
		m.revoke(ctx, id)
	}
}

// checkSpend refuses a launch once the month's spend reaches either cap.
// The item escalates with reason "budget": raising a cap is a PR to
// config/chuck.yaml, then Re-queue.
func (m *Manager) checkSpend(ctx context.Context, repoID int64, now time.Time) error {
	rows, err := m.Store.SpendForMonth(ctx, now)
	if err != nil {
		return err
	}
	var total, repo float64
	for _, r := range rows {
		total += r.USD
		if r.RepoID == repoID {
			repo += r.USD
		}
	}
	switch {
	case total >= m.Spend.MonthlyUSD:
		return &engine.Escalation{Reason: fmt.Sprintf("budget: $%.2f spent this month, at the $%.2f cap", total, m.Spend.MonthlyUSD)}
	case repo >= m.Spend.PerRepoMonthlyUSD:
		return &engine.Escalation{Reason: fmt.Sprintf("budget: $%.2f spent on this repo this month, at the $%.2f per-repo cap", repo, m.Spend.PerRepoMonthlyUSD)}
	}
	return nil
}

// prepared is what a launch reads before it records the run.
type prepared struct {
	context loop.Context
	ref     string
	ignored []memory.IgnoredFeedback
}

// prepare builds the prompt's Context and the ref the worker checks out.
// Build and sweep read the feedback that counts; everything else on the
// item is listed as ignored.
func (m *Manager) prepare(ctx context.Context, req engine.LaunchRequest, attempt int) (prepared, error) {
	it, cfg := req.Item, req.Config
	p := prepared{context: loop.Context{Repo: req.Repo.Owner + "/" + req.Repo.Name, Attempt: attempt}}
	if it.Kind == memory.KindIssue {
		p.context.Issue = it.Number
	} else {
		p.context.PR, p.context.Issue = it.Number, it.IssueNumber
	}
	base := cfg.DefaultBranch
	if base == "" {
		base = req.Repo.DefaultBranch
	}
	head := ""
	switch req.Stage {
	case config.StageIntake:
		p.ref = base
	case config.StageBuild:
		p.context.Branch = loop.BuildBranch(it.Number)
		p.ref = p.context.Branch
		earlier, err := m.earlierAttempts(ctx, it.ID, attempt)
		if err != nil {
			return p, err
		}
		p.context.EarlierAttempts = earlier
	case config.StageReview, config.StageSweep, config.StageValidate:
		if it.Kind != memory.KindPR {
			return p, fmt.Errorf("%s runs on a PR, not %s #%d", req.Stage, it.Kind, it.Number)
		}
		pr, err := req.API.PullRequest(ctx, it.Number)
		if err != nil {
			return p, err
		}
		p.ref, head = pr.Head.Ref, pr.Head.SHA
		if req.Stage == config.StageValidate {
			if !pr.Merged || pr.MergeSHA == "" {
				return p, fmt.Errorf("validate: PR #%d has no merge commit", it.Number)
			}
			p.ref, p.context.SHA = pr.MergeSHA, pr.MergeSHA
			p.context.DevURL = cfg.Deploy.DevURL
			p.context.Setup, p.context.E2ECommand = cfg.Validate.Setup, cfg.Validate.E2ECommand
			p.context.Smoke = cfg.Validate.Smoke
		}
	default:
		return p, fmt.Errorf("no stage %q", req.Stage)
	}
	if p.ref == "" {
		return p, fmt.Errorf("%s: no ref to check out", req.Stage)
	}
	if req.Stage == config.StageBuild || req.Stage == config.StageSweep {
		cs, err := comments(ctx, req.API, it.Kind, it.Number)
		if err != nil {
			return p, fmt.Errorf("read feedback: %w", err)
		}
		findings := ""
		if req.Stage == config.StageSweep {
			if findings, err = m.findingsURL(ctx, it.ID, head); err != nil {
				return p, err
			}
		}
		if findings != "" && !slices.ContainsFunc(cs, func(c Comment) bool {
			return c.AuthorID == req.BotID && c.Source == SourceConversation && c.URL == findings
		}) {
			// Fail closed: a sweep without the findings would triage
			// nothing and report the PR ready.
			return p, &engine.Escalation{Reason: "review findings: the review verdict's comment_url is not a comment of chuck's App on this PR"}
		}
		p.context.Feedback, p.ignored = Trusted(cs, req.Humans, req.BotID, findings)
	}
	return p, nil
}

// findingsURL is the comment the item's last review verdict reported its
// findings in, or "" when there is no review verdict or it was of a head
// other than head.
func (m *Manager) findingsURL(ctx context.Context, itemID int64, head string) (string, error) {
	v, err := m.Store.LatestVerdict(ctx, itemID, string(config.StageReview))
	if errors.Is(err, memory.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	rv, err := loop.ParseVerdict(config.StageReview, []byte(v))
	if err != nil {
		return "", fmt.Errorf("stored review verdict: %w", err)
	}
	if head == "" || rv.Review.ReviewedSHA != head {
		return "", nil
	}
	return rv.Review.CommentURL, nil
}

// earlierAttempts is what each earlier build attempt reported, from its
// runs. A same-attempt relaunch's failed run is left out: it never ran.
func (m *Manager) earlierAttempts(ctx context.Context, itemID int64, attempt int) ([]string, error) {
	runs, err := m.Store.RunsForItem(ctx, itemID, string(config.StageBuild))
	if err != nil {
		return nil, err
	}
	byAttempt := map[int]string{}
	for _, r := range runs {
		if r.Attempt >= attempt || r.StartedAt.IsZero() {
			continue
		}
		byAttempt[r.Attempt] = attemptSummary(r)
	}
	var out []string
	for a := 1; a < attempt; a++ {
		if s, ok := byAttempt[a]; ok {
			out = append(out, s)
		}
	}
	return out, nil
}

func attemptSummary(r memory.Run) string {
	if r.VerdictJSON != "" {
		if v, err := loop.ParseVerdict(config.StageBuild, []byte(r.VerdictJSON)); err == nil {
			s := v.Build.Outcome + ": " + v.Build.Summary
			if len(v.Build.Blockers) > 0 {
				s += "\nBlockers: " + strings.Join(v.Build.Blockers, "; ")
			}
			return s
		}
	}
	s := r.Status
	if r.Reason != "" {
		s += ": " + r.Reason
	}
	return s
}

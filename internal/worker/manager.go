package worker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
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
		p.ref = pr.Head.Ref
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
		p.context.Feedback, p.ignored = Trusted(cs, req.Humans, req.BotID)
	}
	return p, nil
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

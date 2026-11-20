package engine

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/showmethebots/chuck/internal/github"
	"github.com/showmethebots/chuck/internal/memory"
)

// RepoAPI is every GitHub call the engine makes on one repository.
// *github.Client implements it.
type RepoAPI interface {
	GetIssue(ctx context.Context, number int) (github.Issue, error)
	IssueComments(ctx context.Context, number int) ([]github.IssueComment, error)
	IssueEvents(ctx context.Context, number int) ([]github.IssueEvent, error)
	SubIssues(ctx context.Context, number int) ([]int, error)
	OpenIssuesWithLabel(ctx context.Context, label string) ([]github.Issue, error)
	PullRequest(ctx context.Context, number int) (github.PR, error)
	PRFiles(ctx context.Context, number int) ([]github.File, error)
	Reviews(ctx context.Context, number int) ([]github.Review, error)
	CheckRuns(ctx context.Context, sha string) ([]github.CheckRun, error)
	Statuses(ctx context.Context, sha string) ([]github.Status, error)
	WorkflowRuns(ctx context.Context, headSHA string) ([]github.WorkflowRun, error)
	DispatchRuns(ctx context.Context, workflow string, since time.Time) ([]github.WorkflowRun, error)
	ReadFile(ctx context.Context, path, ref string) (github.Blob, error)
	UserByID(ctx context.Context, id int64) (github.User, error)

	AddLabel(ctx context.Context, number int, label string) error
	RemoveLabel(ctx context.Context, number int, label string) error
	Comment(ctx context.Context, number int, body, marker string) (string, error)
	SetStatus(ctx context.Context, sha, state, description, targetURL string) error
	DispatchWorkflow(ctx context.Context, workflow, ref string, inputs map[string]string) error
	AutoMerge(ctx context.Context, pr github.PR, sha, method string) (bool, error)
}

// Installation is one installation of an App and the repositories it
// reaches.
type Installation struct {
	ID    int64
	Repos []github.Repository
}

// Source is where the engine gets its GitHub access. Today that is the
// App's private key, held by chuck (AppSource). A hosted chuck can put a
// token broker behind the same interface, so the engine never holds a key
// itself and never mints a token: it asks for a repository's client.
type Source interface {
	// Apps names the Apps chuck acts as, in config order.
	Apps() []string
	// Installations lists an App's installations and their repositories.
	Installations(ctx context.Context, appOwner string) ([]Installation, error)
	// Repo is the client for one repository, acting as the App that
	// reaches it.
	Repo(r memory.Repo) (RepoAPI, error)
	// BotID is the App's bot user id: the author of loop PRs and of
	// chuck's own comments and statuses.
	BotID(ctx context.Context, appOwner string) (int64, error)
}

// AppSource is a Source backed by App private keys in chuck's environment.
type AppSource struct {
	ByOwner map[string]*github.App
	Order   []string // config order; nil means sorted

	mu   sync.Mutex
	bots map[string]int64
}

func (s *AppSource) Apps() []string {
	if s.Order != nil {
		return s.Order
	}
	out := make([]string, 0, len(s.ByOwner))
	for k := range s.ByOwner {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *AppSource) app(owner string) (*github.App, error) {
	a, ok := s.ByOwner[owner]
	if !ok {
		return nil, fmt.Errorf("no App configured for %q", owner)
	}
	return a, nil
}

func (s *AppSource) Installations(ctx context.Context, owner string) ([]Installation, error) {
	a, err := s.app(owner)
	if err != nil {
		return nil, err
	}
	ins, err := a.Installations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Installation, 0, len(ins))
	for _, in := range ins {
		repos, err := a.InstallationRepos(ctx, in.ID)
		if err != nil {
			return nil, fmt.Errorf("installation %d: %w", in.ID, err)
		}
		out = append(out, Installation{ID: in.ID, Repos: repos})
	}
	return out, nil
}

func (s *AppSource) Repo(r memory.Repo) (RepoAPI, error) {
	a, err := s.app(r.AppOwner)
	if err != nil {
		return nil, err
	}
	return &github.Client{App: a, InstallationID: r.InstallationID, Owner: r.Owner, Repo: r.Name}, nil
}

func (s *AppSource) BotID(ctx context.Context, owner string) (int64, error) {
	s.mu.Lock()
	id, ok := s.bots[owner]
	s.mu.Unlock()
	if ok {
		return id, nil
	}
	a, err := s.app(owner)
	if err != nil {
		return 0, err
	}
	u, err := a.Bot(ctx)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	if s.bots == nil {
		s.bots = map[string]int64{}
	}
	s.bots[owner] = u.ID
	s.mu.Unlock()
	return u.ID, nil
}

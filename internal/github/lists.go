package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// maxPages bounds every paginated read at 5000 entries, so a runaway list
// cannot hold the loop. A list that reaches it is an error, never a
// silently partial answer.
const maxPages = 50

// pages reads 100-entry pages until a short one.
func pages[T any](fetch func(page int) ([]T, error)) ([]T, error) {
	var all []T
	for page := 1; page <= maxPages; page++ {
		got, err := fetch(page)
		if err != nil {
			return nil, err
		}
		all = append(all, got...)
		if len(got) < 100 {
			return all, nil
		}
	}
	return nil, fmt.Errorf("more than %d entries; refusing a partial list", maxPages*100)
}

// CheckRun is one check run on a commit.
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // queued, in_progress, completed
	Conclusion string `json:"conclusion"` // success, failure, neutral, cancelled, skipped, timed_out, action_required, stale
	App        struct {
		ID int64 `json:"id"`
	} `json:"app"`
}

// CheckRuns lists every check run on sha. GitHub lists runs by commit, not
// by PR: a fork PR at the same sha adds its runs here too.
func (c *Client) CheckRuns(ctx context.Context, sha string) ([]CheckRun, error) {
	return pages(func(page int) ([]CheckRun, error) {
		var out struct {
			CheckRuns []CheckRun `json:"check_runs"`
		}
		err := c.call(ctx, http.MethodGet, c.repoPath("/commits/%s/check-runs?per_page=100&page=%d", url.PathEscape(sha), page), nil, &out)
		return out.CheckRuns, err
	})
}

// Status is one commit status. Creator is who set it: only statuses chuck's
// App set are read as chuck/loop.
type Status struct {
	State   string `json:"state"` // error, failure, pending, success
	Context string `json:"context"`
	Creator User   `json:"creator"`
}

// Statuses lists sha's commit statuses, newest first.
func (c *Client) Statuses(ctx context.Context, sha string) ([]Status, error) {
	return pages(func(page int) ([]Status, error) {
		var out []Status
		err := c.call(ctx, http.MethodGet, c.repoPath("/commits/%s/statuses?per_page=100&page=%d", url.PathEscape(sha), page), nil, &out)
		return out, err
	})
}

// SubIssues lists the numbers of an issue's sub-issues.
func (c *Client) SubIssues(ctx context.Context, number int) ([]int, error) {
	issues, err := pages(func(page int) ([]Issue, error) {
		var out []Issue
		err := c.call(ctx, http.MethodGet, c.repoPath("/issues/%d/sub_issues?per_page=100&page=%d", number, page), nil, &out)
		return out, err
	})
	if err != nil {
		return nil, err
	}
	ns := make([]int, 0, len(issues))
	for _, is := range issues {
		ns = append(ns, is.Number)
	}
	return ns, nil
}

// IssueEvent is one entry of an issue's or PR's timeline of events.
type IssueEvent struct {
	ID    int64  `json:"id"`
	Event string `json:"event"` // labeled, unlabeled, ...
	Actor User   `json:"actor"`
	Label struct {
		Name string `json:"name"`
	} `json:"label"`
}

// IssueEvents lists an issue's or PR's events, oldest first.
func (c *Client) IssueEvents(ctx context.Context, number int) ([]IssueEvent, error) {
	return pages(func(page int) ([]IssueEvent, error) {
		var out []IssueEvent
		err := c.call(ctx, http.MethodGet, c.repoPath("/issues/%d/events?per_page=100&page=%d", number, page), nil, &out)
		return out, err
	})
}

// Review is one PR review.
type Review struct {
	ID       int64  `json:"id"`
	User     User   `json:"user"`
	State    string `json:"state"` // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, PENDING
	CommitID string `json:"commit_id"`
}

// Reviews lists a PR's reviews, oldest first.
func (c *Client) Reviews(ctx context.Context, number int) ([]Review, error) {
	return pages(func(page int) ([]Review, error) {
		var out []Review
		err := c.call(ctx, http.MethodGet, c.repoPath("/pulls/%d/reviews?per_page=100&page=%d", number, page), nil, &out)
		return out, err
	})
}

// WorkflowRun is one Actions run, with what the loop checks about where it
// came from.
type WorkflowRun struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Path           string `json:"path"` // .github/workflows/deploy-dev.yml
	HeadSHA        string `json:"head_sha"`
	HeadBranch     string `json:"head_branch"`
	Event          string `json:"event"`
	Status         string `json:"status"`
	Conclusion     string `json:"conclusion"`
	HeadRepository struct {
		ID int64 `json:"id"`
	} `json:"head_repository"`
	Repository struct {
		ID int64 `json:"id"`
	} `json:"repository"`
}

// WorkflowRuns lists the Actions runs for a head sha.
func (c *Client) WorkflowRuns(ctx context.Context, headSHA string) ([]WorkflowRun, error) {
	return pages(func(page int) ([]WorkflowRun, error) {
		var out struct {
			WorkflowRuns []WorkflowRun `json:"workflow_runs"`
		}
		err := c.call(ctx, http.MethodGet, c.repoPath("/actions/runs?head_sha=%s&per_page=100&page=%d", url.QueryEscape(headSHA), page), nil, &out)
		return out.WorkflowRuns, err
	})
}

// OpenIssuesWithLabel lists open issues and PRs carrying label.
func (c *Client) OpenIssuesWithLabel(ctx context.Context, label string) ([]Issue, error) {
	return pages(func(page int) ([]Issue, error) {
		var out []Issue
		err := c.call(ctx, http.MethodGet, c.repoPath("/issues?state=open&labels=%s&per_page=100&page=%d", url.QueryEscape(label), page), nil, &out)
		return out, err
	})
}

// UserByID reads a GitHub account by its numeric id, for mentioning a
// human chuck knows only by id.
func (c *Client) UserByID(ctx context.Context, id int64) (User, error) {
	var u User
	err := c.call(ctx, http.MethodGet, fmt.Sprintf("/user/%d", id), nil, &u)
	return u, err
}

// Installation is one installation of the App.
type Installation struct {
	ID      int64 `json:"id"`
	Account User  `json:"account"`
}

// Installations lists the App's installations.
func (a *App) Installations(ctx context.Context) ([]Installation, error) {
	return pages(func(page int) ([]Installation, error) {
		jwt, err := a.JWT()
		if err != nil {
			return nil, err
		}
		var out []Installation
		err = a.do(ctx, "Bearer "+jwt, http.MethodGet, fmt.Sprintf("/app/installations?per_page=100&page=%d", page), nil, &out)
		return out, err
	})
}

// Repository is one repository an installation can reach.
type Repository struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Owner         User   `json:"owner"`
	DefaultBranch string `json:"default_branch"`
	Archived      bool   `json:"archived"`
}

// discoveryPermissions is the token that lists an installation's
// repositories. It is the one token not scoped to a repository, since its
// job is to find them, so it carries metadata read and nothing else.
var discoveryPermissions = Permissions{"metadata": "read"}

// InstallationRepos lists every repository an installation can reach.
func (a *App) InstallationRepos(ctx context.Context, installationID int64) ([]Repository, error) {
	tok, err := a.mintAll(ctx, installationID, discoveryPermissions)
	if err != nil {
		return nil, err
	}
	return pages(func(page int) ([]Repository, error) {
		var out struct {
			Repositories []Repository `json:"repositories"`
		}
		err := a.do(ctx, "token "+tok.Value, http.MethodGet, fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page), nil, &out)
		return out.Repositories, err
	})
}

// mintAll mints a token for every repository the installation reaches. It
// accepts only discoveryPermissions, so no write token is ever unscoped.
func (a *App) mintAll(ctx context.Context, installationID int64, perms Permissions) (Token, error) {
	if perms.key() != discoveryPermissions.key() {
		return Token{}, errors.New("github app: an unscoped token may carry metadata read only")
	}
	jwt, err := a.JWT()
	if err != nil {
		return Token{}, err
	}
	var t Token
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	if err := a.do(ctx, "Bearer "+jwt, http.MethodPost, path, []byte(`{"permissions":{"metadata":"read"}}`), &t); err != nil {
		return Token{}, fmt.Errorf("mint discovery token: %w", err)
	}
	for k, v := range t.Permissions {
		if k != "metadata" || v != "read" {
			return Token{}, fmt.Errorf("mint discovery token: GitHub granted %s:%s, not metadata read only", k, v)
		}
	}
	return t, nil
}

// Bot is the App's bot account: the author of every PR a build opens and
// of chuck's own comments and statuses.
func (a *App) Bot(ctx context.Context) (User, error) {
	jwt, err := a.JWT()
	if err != nil {
		return User{}, err
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := a.do(ctx, "Bearer "+jwt, http.MethodGet, "/app", nil, &app); err != nil {
		return User{}, err
	}
	seg, err := segment(app.Slug)
	if err != nil || strings.ContainsAny(app.Slug, "/[]") {
		return User{}, fmt.Errorf("github app: slug %q", app.Slug)
	}
	// /users/{slug}[bot] is public and needs no token; the App's JWT is not
	// accepted there, so it goes without one.
	var u User
	if err := a.do(ctx, "", http.MethodGet, "/users/"+seg+"%5Bbot%5D", nil, &u); err != nil {
		return User{}, err
	}
	if u.ID <= 0 || u.Type != "Bot" {
		return User{}, fmt.Errorf("github app: %s[bot] is not a bot account", app.Slug)
	}
	return u, nil
}

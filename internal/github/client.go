package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Client makes chuck's own calls on one repository, with a token scoped
// to that repository and ChuckPermissions.
type Client struct {
	App            *App
	InstallationID int64
	Owner, Repo    string
}

// StatusContext is the commit status the default-branch ruleset requires.
const StatusContext = "chuck/loop"

// verdictMarker opens the hidden marker on chuck's comments. chuck never
// reads it back; it is for the audit trail on GitHub.
const verdictMarker = "<!-- loop-verdict "

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	tok, err := c.App.token(ctx, c.InstallationID, c.Repo, ChuckPermissions)
	if err != nil {
		return err
	}
	var body []byte
	if in != nil {
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	return c.App.do(ctx, "token "+tok, method, path, body, out)
}

// segment escapes one path segment, refusing ones a server could
// normalize away ("", ".", "..").
func segment(s string) (string, error) {
	if s == "" || s == "." || s == ".." {
		return "", fmt.Errorf("path segment %q", s)
	}
	return url.PathEscape(s), nil
}

func (c *Client) repoPath(format string, a ...any) string {
	return "/repos/" + url.PathEscape(c.Owner) + "/" + url.PathEscape(c.Repo) + fmt.Sprintf(format, a...)
}

// AddLabel adds a label to an issue or PR.
func (c *Client) AddLabel(ctx context.Context, number int, label string) error {
	return c.call(ctx, http.MethodPost, c.repoPath("/issues/%d/labels", number),
		map[string][]string{"labels": {label}}, nil)
}

// RemoveLabel removes a label; one that is not there is not an error.
func (c *Client) RemoveLabel(ctx context.Context, number int, label string) error {
	seg, err := segment(label)
	if err != nil {
		return err
	}
	err = c.call(ctx, http.MethodDelete, c.repoPath("/issues/%d/labels/%s", number, seg), nil, nil)
	if IsStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// Comment posts a comment ending in the hidden loop-verdict marker. The
// marker names what posted it, such as "run=12 stage=review".
func (c *Client) Comment(ctx context.Context, number int, body, marker string) (string, error) {
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	err := c.call(ctx, http.MethodPost, c.repoPath("/issues/%d/comments", number),
		map[string]string{"body": strings.TrimRight(body, "\n") + "\n\n" + MarkerText(marker)}, &out)
	return out.HTMLURL, err
}

// MarkerText is the hidden marker Comment appends for marker, so a caller
// can tell whether a comment it may have posted is already there.
func MarkerText(marker string) string {
	return verdictMarker + strings.ReplaceAll(marker, "--", "") + " -->"
}

// SetStatus sets the chuck/loop commit status on sha.
func (c *Client) SetStatus(ctx context.Context, sha, state, description, targetURL string) error {
	switch state {
	case "pending", "success", "failure", "error":
	default:
		return fmt.Errorf("commit status state %q", state)
	}
	if len(description) > 140 {
		description = description[:140]
	}
	in := map[string]string{"state": state, "context": StatusContext, "description": description}
	if targetURL != "" {
		in["target_url"] = targetURL
	}
	return c.call(ctx, http.MethodPost, c.repoPath("/statuses/%s", url.PathEscape(sha)), in, nil)
}

// PR is what chuck reads of a pull request.
type PR struct {
	Number   int    `json:"number"`
	NodeID   string `json:"node_id"`
	State    string `json:"state"`
	Merged   bool   `json:"merged"`
	MergeSHA string `json:"merge_commit_sha"`
	Body     string `json:"body"`
	User     User   `json:"user"`
	Head     struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// User is a GitHub account. ID is the identity; logins can change.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

// PullRequest reads one PR.
func (c *Client) PullRequest(ctx context.Context, number int) (PR, error) {
	var pr PR
	err := c.call(ctx, http.MethodGet, c.repoPath("/pulls/%d", number), nil, &pr)
	return pr, err
}

// File is one file a PR changes. PreviousName is set on a rename, so a
// file moved out of a risk path still counts as touching it.
type File struct {
	Name         string `json:"filename"`
	PreviousName string `json:"previous_filename"`
	Status       string `json:"status"`
}

// ErrTooManyFiles is a PR whose file list GitHub truncates at 3000.
var ErrTooManyFiles = errors.New("PR changes 3000 files or more; GitHub lists no more")

// PRFiles lists every file a PR changes, following pages. GitHub stops at
// 3000 files; a PR that reaches that is reported as ErrTooManyFiles rather
// than judged on a partial list.
func (c *Client) PRFiles(ctx context.Context, number int) ([]File, error) {
	var all []File
	for page := 1; ; page++ {
		var files []File
		if err := c.call(ctx, http.MethodGet, c.repoPath("/pulls/%d/files?per_page=100&page=%d", number, page), nil, &files); err != nil {
			return nil, err
		}
		all = append(all, files...)
		if len(files) < 100 {
			return all, nil
		}
		if len(all) >= 3000 {
			return nil, fmt.Errorf("PR #%d: %w", number, ErrTooManyFiles)
		}
	}
}

// Issue is what chuck reads of an issue.
type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	User   User   `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest *struct{} `json:"pull_request"`
}

// GetIssue reads one issue.
func (c *Client) GetIssue(ctx context.Context, number int) (Issue, error) {
	var is Issue
	err := c.call(ctx, http.MethodGet, c.repoPath("/issues/%d", number), nil, &is)
	return is, err
}

// IssueComment is one comment, with its author for the trusted-text filter.
type IssueComment struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	User    User   `json:"user"`
}

// IssueComments lists an issue's or PR's comments, oldest first.
func (c *Client) IssueComments(ctx context.Context, number int) ([]IssueComment, error) {
	var all []IssueComment
	for page := 1; ; page++ {
		var cs []IssueComment
		if err := c.call(ctx, http.MethodGet, c.repoPath("/issues/%d/comments?per_page=100&page=%d", number, page), nil, &cs); err != nil {
			return nil, err
		}
		all = append(all, cs...)
		if len(cs) < 100 {
			return all, nil
		}
	}
}

// DispatchWorkflow runs a workflow_dispatch workflow on ref.
func (c *Client) DispatchWorkflow(ctx context.Context, workflow, ref string, inputs map[string]string) error {
	in := map[string]any{"ref": ref}
	if len(inputs) > 0 {
		in["inputs"] = inputs
	}
	return c.call(ctx, http.MethodPost, c.repoPath("/actions/workflows/%s/dispatches", url.PathEscape(workflow)), in, nil)
}

// Merge merges a PR, but only if its head is still sha: GitHub refuses
// with 409 if it moved.
func (c *Client) Merge(ctx context.Context, number int, sha, method string) error {
	return c.call(ctx, http.MethodPut, c.repoPath("/pulls/%d/merge", number),
		map[string]string{"sha": sha, "merge_method": strings.ToLower(method)}, nil)
}

// ErrAutoMergeRefused is GitHub declining auto-merge, which it does when
// the PR can merge right now ("clean status").
var ErrAutoMergeRefused = errors.New("github refused auto-merge")

// EnableAutoMerge asks GitHub to merge the PR once its checks pass. method
// is MERGE, SQUASH or REBASE.
func (c *Client) EnableAutoMerge(ctx context.Context, nodeID, method string) error {
	q := `mutation($id: ID!, $m: PullRequestMergeMethod!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $id, mergeMethod: $m}) { clientMutationId }
}`
	var out struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.call(ctx, http.MethodPost, "/graphql", map[string]any{
		"query": q, "variables": map[string]string{"id": nodeID, "m": strings.ToUpper(method)},
	}, &out); err != nil {
		return err
	}
	if len(out.Errors) > 0 {
		msg := out.Errors[0].Message
		if strings.Contains(strings.ToLower(msg), "clean status") {
			return fmt.Errorf("%w: %s", ErrAutoMergeRefused, msg)
		}
		return fmt.Errorf("enable auto-merge: %s", msg)
	}
	return nil
}

// AutoMerge enables auto-merge on the PR, or merges it directly at sha when
// GitHub refuses auto-merge because the PR is already mergeable. It
// reports whether it merged.
func (c *Client) AutoMerge(ctx context.Context, pr PR, sha, method string) (merged bool, err error) {
	err = c.EnableAutoMerge(ctx, pr.NodeID, method)
	if errors.Is(err, ErrAutoMergeRefused) {
		if err := c.Merge(ctx, pr.Number, sha, method); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, err
}

// File content is read at a ref and identified by its blob sha, which the
// loop records so a later change to the file is noticed.
type Blob struct {
	SHA     string
	Content []byte
}

// ErrNotFound is a file that does not exist at the ref.
var ErrNotFound = errors.New("not found")

// LoopConfigPath is the per-repo loop config.
const LoopConfigPath = ".claude/loop.json"

// ReadFile reads a file at ref (a branch or sha) with its blob sha.
func (c *Client) ReadFile(ctx context.Context, path, ref string) (Blob, error) {
	var out struct {
		Type     string `json:"type"`
		Path     string `json:"path"`
		SHA      string `json:"sha"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		seg, err := segment(s)
		if err != nil {
			return Blob{}, fmt.Errorf("read %q: %w", path, err)
		}
		segs[i] = seg
	}
	err := c.call(ctx, http.MethodGet, c.repoPath("/contents/%s?ref=%s", strings.Join(segs, "/"), url.QueryEscape(ref)), nil, &out)
	if IsStatus(err, http.StatusNotFound) {
		return Blob{}, fmt.Errorf("%s at %s: %w", path, ref, ErrNotFound)
	}
	if err != nil {
		return Blob{}, err
	}
	if out.Type != "file" {
		return Blob{}, fmt.Errorf("%s at %s is a %s, not a file", path, ref, out.Type)
	}
	// For a symlink to a file, GitHub answers with the target's content and
	// path. The loop config must be the file itself, or edits to the
	// target would change it without touching a risk path.
	if out.Path != path {
		return Blob{}, fmt.Errorf("%s at %s resolves to %s; symlinks are not read", path, ref, out.Path)
	}
	// Over 1 MB the contents API returns no content; read the blob itself.
	if out.Encoding != "base64" {
		return c.ReadBlob(ctx, out.SHA)
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil {
		return Blob{}, fmt.Errorf("%s at %s: %w", path, ref, err)
	}
	return Blob{SHA: out.SHA, Content: b}, nil
}

// ReadBlob reads a blob by its sha.
func (c *Client) ReadBlob(ctx context.Context, sha string) (Blob, error) {
	var out struct {
		SHA      string `json:"sha"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := c.call(ctx, http.MethodGet, c.repoPath("/git/blobs/%s", url.PathEscape(sha)), nil, &out); err != nil {
		return Blob{}, err
	}
	if out.Encoding != "base64" {
		return Blob{}, fmt.Errorf("blob %s: encoding %q", sha, out.Encoding)
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil {
		return Blob{}, fmt.Errorf("blob %s: %w", sha, err)
	}
	if out.SHA != sha {
		return Blob{}, fmt.Errorf("blob %s: GitHub returned %s", sha, out.SHA)
	}
	return Blob{SHA: sha, Content: b}, nil
}

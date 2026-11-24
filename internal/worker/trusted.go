package worker

import (
	"context"
	"slices"
	"strings"

	"github.com/showmethebots/chuck/internal/engine"
	"github.com/showmethebots/chuck/internal/loop"
	"github.com/showmethebots/chuck/internal/memory"
)

// Ignore reasons, as ignored_feedback.reason.
const (
	IgnoredUntrusted    = "untrusted_author"
	IgnoredChuckMention = "chuck_mention"
	// An App comment outside sweep's review findings on the reviewed head.
	IgnoredAppComment = "app_comment"
)

// mentionMarker opens the hidden marker on the comments chuck's executor
// posts to tell the humans an item needs them (engine's mention effect).
const mentionMarker = "<!-- loop-verdict transition="

// Where a comment was left.
const (
	SourceConversation = "conversation" // an issue's or PR's comments
	SourceReview       = "review"       // a PR review's body
	SourceInline       = "inline"       // an inline comment on a PR's diff
)

// Comment is one piece of feedback as GitHub has it, from any of the
// places a PR or issue collects it.
type Comment struct {
	AuthorID    int64
	AuthorLogin string
	URL         string
	Body        string
	Source      string
	CommitID    string // reviews and inline comments: the commit they were left on
}

// Trusted splits feedback into what may reach a prompt and what is
// recorded as ignored. A comment counts only when its author is in humans,
// or is chuck's App in the one place the spec names: the review stage's
// findings, as a review or inline comment on reviewedSHA, the head the
// last review verdict reported. This is the defence against a stranger on
// a public repo steering sweep into pushing their commit, so it is decided
// here, in code, by numeric user id, and never by an instruction to the
// model.
//
// App comments are narrowed because every worker posts as the App: an
// intake worker reading a stranger's issue, or a review worker quoting a
// stranger's PR comment, could otherwise leave text that the next run
// trusts. The executor's mentions quote workers' words back too. So the
// App's conversation comments never count, and its reviews count only on
// the head that was reviewed. reviewedSHA "" (a build, which has no
// review) trusts no App comment at all.
func Trusted(cs []Comment, humans []int64, bot int64, reviewedSHA string) (kept []loop.Feedback, ignored []memory.IgnoredFeedback) {
	for _, c := range cs {
		switch {
		case c.AuthorID == 0:
			// No author id: never a human in config, and never the App.
			ignored = append(ignored, ignoredFrom(c, IgnoredUntrusted))
		case c.AuthorID == bot && strings.Contains(c.Body, mentionMarker):
			ignored = append(ignored, ignoredFrom(c, IgnoredChuckMention))
		case c.AuthorID == bot && (c.Source == SourceConversation || reviewedSHA == "" || c.CommitID != reviewedSHA):
			ignored = append(ignored, ignoredFrom(c, IgnoredAppComment))
		case c.AuthorID == bot || slices.Contains(humans, c.AuthorID):
			if strings.TrimSpace(c.Body) == "" {
				continue
			}
			kept = append(kept, loop.Feedback{Author: c.AuthorLogin, URL: c.URL, Body: c.Body})
		default:
			ignored = append(ignored, ignoredFrom(c, IgnoredUntrusted))
		}
	}
	return kept, ignored
}

func ignoredFrom(c Comment, reason string) memory.IgnoredFeedback {
	return memory.IgnoredFeedback{AuthorID: c.AuthorID, AuthorLogin: c.AuthorLogin, URL: c.URL, Reason: reason}
}

// comments gathers every place feedback lands on an item: the issue's or
// PR's conversation, and for a PR its reviews' bodies and inline comments.
func comments(ctx context.Context, api engine.RepoAPI, kind string, number int) ([]Comment, error) {
	var out []Comment
	ics, err := api.IssueComments(ctx, number)
	if err != nil {
		return nil, err
	}
	for _, c := range ics {
		out = append(out, Comment{AuthorID: c.User.ID, AuthorLogin: c.User.Login, URL: c.HTMLURL, Body: c.Body, Source: SourceConversation})
	}
	if kind != memory.KindPR {
		return out, nil
	}
	rs, err := api.Reviews(ctx, number)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		if r.State == "PENDING" {
			continue // a draft review no one else can see yet
		}
		out = append(out, Comment{AuthorID: r.User.ID, AuthorLogin: r.User.Login, URL: r.HTMLURL, Body: r.Body,
			Source: SourceReview, CommitID: r.CommitID})
	}
	rcs, err := api.ReviewComments(ctx, number)
	if err != nil {
		return nil, err
	}
	for _, c := range rcs {
		body := c.Body
		if c.Path != "" {
			body = "On " + c.Path + ":\n\n" + body
		}
		out = append(out, Comment{AuthorID: c.User.ID, AuthorLogin: c.User.Login, URL: c.HTMLURL, Body: body,
			Source: SourceInline, CommitID: c.CommitID})
	}
	return out, nil
}

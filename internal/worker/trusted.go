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
)

// mentionMarker opens the hidden marker on the comments chuck's executor
// posts to tell the humans an item needs them (engine's mention effect).
const mentionMarker = "<!-- loop-verdict transition="

// Comment is one piece of feedback as GitHub has it, from any of the
// places a PR or issue collects it.
type Comment struct {
	AuthorID    int64
	AuthorLogin string
	URL         string
	Body        string
}

// Trusted splits feedback into what may reach a prompt and what is
// recorded as ignored. A comment counts only when its author is in humans
// or is chuck's App. This is the whole defence against a stranger on a
// public repo steering sweep into pushing their commit, so it is decided
// here, in code, by numeric user id, and never by an instruction to the
// model.
//
// chuck's App posts two kinds of comment: the review stage's findings,
// which sweep must act on, and the executor's mentions, which quote a
// worker's own words back to the humans (intake's questions, build's
// blockers). A mention is kept out too: feeding a worker's text back as
// trusted feedback would let one run's injected text instruct the next.
// Its marker can be typed by anyone, but only the App's own comments are
// checked for it, so a forged one only drops the forger's comment, which
// was dropped anyway.
func Trusted(cs []Comment, humans []int64, bot int64) (kept []loop.Feedback, ignored []memory.IgnoredFeedback) {
	for _, c := range cs {
		switch {
		case c.AuthorID == 0:
			// No author id: never a human in config, and never the App.
			ignored = append(ignored, ignoredFrom(c, IgnoredUntrusted))
		case c.AuthorID == bot && strings.Contains(c.Body, mentionMarker):
			ignored = append(ignored, ignoredFrom(c, IgnoredChuckMention))
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
		out = append(out, Comment{c.User.ID, c.User.Login, c.HTMLURL, c.Body})
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
		out = append(out, Comment{r.User.ID, r.User.Login, r.HTMLURL, r.Body})
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
		out = append(out, Comment{c.User.ID, c.User.Login, c.HTMLURL, body})
	}
	return out, nil
}

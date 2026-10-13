package triage

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// MaxText caps the free text sent to the model, in runes.
const MaxText = 4000

// ActorAgent is the actor type for text an AI agent posted through the
// owner's GitHub account.
const ActorAgent = "Agent"

// agentMarkerAuthors are the GitHub accounts whose agent marker is trusted.
// Agent skills post through the owner's account, so the marker is honoured
// only from it; anyone else could add it to disguise a comment. Phase 3
// revisits this when agents get their own GitHub identity.
var agentMarkerAuthors = []string{"cwinters8"}

// agentMarker is the hidden HTML comment agent skills append to what they
// post: <!-- agent: review-sweep -->.
var agentMarker = regexp.MustCompile(`<!--\s*agent:\s*[a-z0-9-]+\s*-->`)

// Input is the compact, event-specific extract the classifier sees. Text is
// written by other people and bots; the prompt marks it as untrusted.
type Input struct {
	Repo      string
	Kind      string
	Actor     string
	ActorType string // "User" or "Bot"
	Title     string
	URL       string
	Text      string
}

type ghItem struct {
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	Merged  bool   `json:"merged"`
}

type ghPayload struct {
	Sender struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"sender"`
	Issue       *ghItem `json:"issue"`
	PullRequest *ghItem `json:"pull_request"`
	Comment     *struct {
		Body string `json:"body"`
		Path string `json:"path"`
	} `json:"comment"`
	Review *struct {
		State string `json:"state"`
		Body  string `json:"body"`
	} `json:"review"`
	WorkflowRun *struct {
		Name       string `json:"name"`
		Conclusion string `json:"conclusion"`
		HeadBranch string `json:"head_branch"`
		HTMLURL    string `json:"html_url"`
	} `json:"workflow_run"`
	Ref     string `json:"ref"`
	Compare string `json:"compare"`
	Commits []struct {
		Message string `json:"message"`
	} `json:"commits"`
}

// BuildInput extracts what triage needs from a raw GitHub payload.
func BuildInput(kind, repo string, payload []byte) (Input, error) {
	var p ghPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return Input{}, fmt.Errorf("payload: %w", err)
	}
	in := Input{Repo: repo, Kind: kind, Actor: p.Sender.Login, ActorType: p.Sender.Type}
	item := p.PullRequest
	if item == nil {
		item = p.Issue
	}
	if item != nil {
		in.Title, in.URL = item.Title, item.HTMLURL
	}

	var text, authored string // authored: what the sender wrote for this event
	switch {
	case p.Review != nil:
		authored = p.Review.Body
		text = fmt.Sprintf("review %s:\n%s", p.Review.State, p.Review.Body)
	case p.Comment != nil:
		authored = p.Comment.Body
		text = p.Comment.Body
		if p.Comment.Path != "" {
			text = fmt.Sprintf("on %s:\n%s", p.Comment.Path, text)
		}
	case p.WorkflowRun != nil:
		w := p.WorkflowRun
		text = fmt.Sprintf("workflow %q on branch %s concluded %s", w.Name, w.HeadBranch, w.Conclusion)
		in.URL = w.HTMLURL
	case p.Ref != "":
		var b strings.Builder
		fmt.Fprintf(&b, "%d commit(s) pushed to %s", len(p.Commits), p.Ref)
		for _, c := range p.Commits {
			first, _, _ := strings.Cut(c.Message, "\n")
			b.WriteString("\n- ")
			b.WriteString(first)
		}
		text = b.String()
		in.URL = p.Compare
	case item != nil:
		authored = item.Body
		text = item.Body
		if item.Merged {
			text = "(merged)\n" + text
		}
	}
	if slices.Contains(agentMarkerAuthors, p.Sender.Login) && agentMarker.MatchString(authored) {
		in.ActorType = ActorAgent
	}
	in.Text = truncate(text, MaxText)
	return in, nil
}

// truncate keeps the first n runes of s and marks the cut.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "\n[truncated]"
}

package notify

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/slack-go/slack"

	"github.com/orionsbelt-dev/chuck/internal/memory"
)

// Button action IDs. The button value is the decision ID.
const (
	ActionGradeUp   = "grade_up"
	ActionGradeDown = "grade_down"
	ActionApprove   = "approve"
	ActionDeny      = "deny"
)

const shadowBanner = "SHADOW — nothing will execute."

// Length bounds for model- and payload-supplied text. Slack rejects a section
// over 3000 chars or a context element over 2000, and the outbox retries a
// rejected post forever, holding up every post behind it.
const (
	maxRationale = 500
	maxRef       = 300 // a target or a URL
)

// field escapes s for Slack mrkdwn and keeps at most n runes of the result,
// marking a cut with "…". Escaping &, < and > means model or payload text
// can't form a mention like <!channel> or a link whose label hides its URL.
// The cut never splits an entity, so the bound holds after escaping.
func field(s string, n int) string {
	var b strings.Builder
	runes := 0
	for _, r := range s {
		e := string(r)
		switch r {
		case '&':
			e = "&amp;"
		case '<':
			e = "&lt;"
		case '>':
			e = "&gt;"
		}
		if runes += utf8.RuneCountInString(e); runes > n {
			return b.String() + "…"
		}
		b.WriteString(e)
	}
	return b.String()
}

func headline(v memory.DecisionView) string {
	switch {
	case v.Outcome == memory.OutcomeDropped:
		return fmt.Sprintf("Dropped by rule `%s`", field(v.Rationale, maxRationale))
	case v.Outcome == memory.OutcomeFailed:
		return "Triage failed"
	case v.WouldDo == "" || v.WouldDo == "none":
		return "chuck would do nothing"
	default:
		return fmt.Sprintf("chuck would `%s` %s (%s)", v.WouldDo, field(v.Target, maxRef), v.Permission)
	}
}

// DecisionText is a decision post's notification and fallback text.
func DecisionText(v memory.DecisionView) string {
	text := fmt.Sprintf("%s %s: %s", v.Repo, v.Kind, headline(v))
	if v.URL != "" {
		text += " " + field(v.URL, maxRef)
	}
	if v.PostKind == memory.PostApproval {
		text = shadowBanner + " " + text
	}
	return text
}

// DecisionBlocks renders a grade or approval post, reflecting the current
// grade and approval answer so it can be re-rendered after a click.
func DecisionBlocks(v memory.DecisionView) []slack.Block {
	md := func(s string) *slack.TextBlockObject {
		return slack.NewTextBlockObject(slack.MarkdownType, s, false, false)
	}
	button := func(action, label string) *slack.ButtonBlockElement {
		return slack.NewButtonBlockElement(action, strconv.FormatInt(v.ID, 10),
			slack.NewTextBlockObject(slack.PlainTextType, label, true, false))
	}

	head := headline(v)
	if v.PostKind == memory.PostApproval {
		head = "*" + shadowBanner + "*\n" + head
	}
	what := v.Category
	if v.Outcome != memory.OutcomeOK {
		what = v.Outcome
	}
	about := fmt.Sprintf("%s · %s · %s · confidence %.2f", v.Repo, v.Kind, what, v.Confidence)
	if v.URL != "" {
		about += fmt.Sprintf(" · <%s>", field(v.URL, maxRef))
	}
	blocks := []slack.Block{
		slack.NewSectionBlock(md(head), nil, nil),
		slack.NewContextBlock("", md(about)),
	}
	if v.Outcome == memory.OutcomeOK && v.Rationale != "" {
		blocks = append(blocks, slack.NewSectionBlock(md("> "+field(v.Rationale, maxRationale)), nil, nil))
	}

	var status []string
	switch v.ApprovalResponse {
	case "approve":
		status = append(status, fmt.Sprintf("Approved by <@%s>", v.ApprovalBy))
	case "deny":
		status = append(status, fmt.Sprintf("Denied by <@%s>", v.ApprovalBy))
	}
	switch v.Grade {
	case "up":
		status = append(status, "Graded 👍")
	case "down":
		status = append(status, "Graded 👎")
	}
	for _, s := range status {
		blocks = append(blocks, slack.NewContextBlock("", md(s)))
	}

	var buttons []slack.BlockElement
	if v.PostKind == memory.PostApproval && v.ApprovalResponse == "" {
		buttons = append(buttons,
			button(ActionApprove, "Approve").WithStyle(slack.StylePrimary),
			button(ActionDeny, "Deny").WithStyle(slack.StyleDanger))
	}
	buttons = append(buttons, button(ActionGradeUp, "👍"), button(ActionGradeDown, "👎"))
	return append(blocks, slack.NewActionBlock(fmt.Sprintf("decision-%d", v.ID), buttons...))
}

// Package loop is the delivery loop: the stage prompts and verdict schemas
// workers run with, and (from #23) the transition function and executor.
//
// See docs/superpowers/specs/2026-11-03-delivery-loop-design.md, "Prompts"
// and "The ratchet".
package loop

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/showmethebots/chuck/internal/config"
)

//go:embed prompts/*.md verdicts/*.json
var stageFS embed.FS

// Context is what chuck knows about a run and appends to the stage prompt.
// Feedback and SuiteTests hold text chuck does not control; they are
// fenced as data when rendered.
type Context struct {
	Repo    string // owner/name
	Issue   int
	PR      int
	Attempt int    // attempt, or review round for review and sweep
	Branch  string // build: the branch chuck names for the PR
	SHA     string // validate: the deployed commit

	// Validate.
	DevURL     string
	Setup      string
	E2ECommand string
	Smoke      []string

	// Intake: the e2e suite's test names, for the Supersedes check.
	SuiteTests []string

	// Build and sweep: the feedback chuck decided counts. Comments from
	// anyone else never reach the prompt.
	Feedback []Feedback

	// Build: what earlier attempts reported, from their runs' verdicts.
	EarlierAttempts []string
}

// Feedback is one comment from a trusted author.
type Feedback struct {
	Author string
	URL    string
	Body   string
}

// Prompt is the stage's prompt with the Context block appended.
func Prompt(s config.Stage, c Context) (string, error) {
	body, err := stageFS.ReadFile("prompts/" + string(s) + ".md")
	if err != nil {
		return "", fmt.Errorf("no prompt for stage %q", s)
	}
	var b strings.Builder
	b.Write(body)
	b.WriteString("\n## Context\n\n")
	line := func(k, v string) { fmt.Fprintf(&b, "- %s: %s\n", k, v) }
	line("Repository", c.Repo)
	if c.Issue > 0 {
		line("Issue", fmt.Sprintf("#%d", c.Issue))
	}
	if c.PR > 0 {
		line("Pull request", fmt.Sprintf("#%d", c.PR))
	}
	attempt := c.Attempt
	if attempt < 1 {
		attempt = 1
	}
	if s == config.StageReview || s == config.StageSweep {
		line("Review round", fmt.Sprint(attempt))
	} else {
		line("Attempt", fmt.Sprint(attempt))
	}
	switch s {
	case config.StageBuild:
		line("Branch", "`"+c.Branch+"`")
	case config.StageValidate:
		line("Dev deployment", fmt.Sprintf("%s (commit %s)", c.DevURL, c.SHA))
	}
	b.WriteString("- You are running in loop mode: no human is watching this session. " +
		"Do not schedule wake-ups, subscribe to PR activity, or wait for replies. " +
		"Your structured output is your report.\n")

	switch s {
	case config.StageIntake:
		b.WriteString("\n### End-to-end tests in the suite\n\n")
		if len(c.SuiteTests) == 0 {
			b.WriteString("None yet.\n")
		} else {
			b.WriteString(fence(strings.Join(c.SuiteTests, "\n")))
		}
	case config.StageValidate:
		b.WriteString("\n### Regression checks\n\n")
		if c.Setup != "" {
			b.WriteString("Setup command:\n\n" + fence(c.Setup))
		}
		b.WriteString("End-to-end command:\n\n" + fence(c.E2ECommand))
		b.WriteString("Smoke flows, one per line:\n\n")
		if len(c.Smoke) == 0 {
			b.WriteString("None.\n")
		} else {
			b.WriteString(fence(strings.Join(c.Smoke, "\n")))
		}
	case config.StageBuild, config.StageSweep:
		if s == config.StageBuild {
			b.WriteString("\n### Earlier attempts\n\n")
			if len(c.EarlierAttempts) == 0 {
				b.WriteString("None.\n")
			}
			for i, a := range c.EarlierAttempts {
				fmt.Fprintf(&b, "Attempt %d:\n\n%s\n", i+1, fence(a))
			}
		}
		b.WriteString("\n### Feedback to act on\n\n")
		if len(c.Feedback) == 0 {
			b.WriteString("None.\n")
		}
		for _, f := range c.Feedback {
			fmt.Fprintf(&b, "From @%s, %s:\n\n%s\n", f.Author, f.URL, fence(f.Body))
		}
	}
	return b.String(), nil
}

// fence wraps untrusted text in a code fence longer than any backtick run
// inside it, so it cannot close the fence and pose as prompt text.
func fence(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", max(3, longest+1))
	return f + "text\n" + strings.TrimRight(s, "\n") + "\n" + f + "\n"
}

// SchemaName is the name a run records for its verdict schema.
func SchemaName(s config.Stage) string { return string(s) }

// Schema is the stage's verdict schema as compact JSON, for
// `claude --json-schema`.
func Schema(s config.Stage) ([]byte, error) {
	raw, err := stageFS.ReadFile("verdicts/" + string(s) + ".json")
	if err != nil {
		return nil, fmt.Errorf("no verdict schema for stage %q", s)
	}
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

var (
	compileOnce sync.Once
	compiled    map[config.Stage]*jsonschema.Schema
	compileErr  error
)

func schemas() (map[config.Stage]*jsonschema.Schema, error) {
	compileOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		compiled = map[config.Stage]*jsonschema.Schema{}
		for _, s := range config.Stages {
			raw, err := Schema(s)
			if err != nil {
				compileErr = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				compileErr = fmt.Errorf("verdict schema %s: %w", s, err)
				return
			}
			url := "chuck:///verdicts/" + string(s) + ".json"
			if err := c.AddResource(url, doc); err != nil {
				compileErr = err
				return
			}
			if compiled[s], err = c.Compile(url); err != nil {
				compileErr = fmt.Errorf("verdict schema %s: %w", s, err)
				return
			}
		}
	})
	return compiled, compileErr
}

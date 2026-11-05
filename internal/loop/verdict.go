package loop

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/showmethebots/chuck/internal/config"
)

// Verdict is a worker's decoded report. Exactly one of the stage fields is
// set, matching Stage.
type Verdict struct {
	Stage    config.Stage
	Outcome  string
	Summary  string
	Intake   *IntakeVerdict
	Build    *BuildVerdict
	Review   *ReviewVerdict
	Sweep    *SweepVerdict
	Validate *ValidateVerdict
}

type IntakeVerdict struct {
	Outcome     string       `json:"outcome"`
	Summary     string       `json:"summary"`
	CriteriaIDs []string     `json:"criteria_ids"`
	SubIssues   []int        `json:"sub_issues"`
	Supersedes  []Supersedes `json:"supersedes"`
	Questions   []string     `json:"questions"`
}

// Supersedes is one "Supersedes: #N AC-n" line under a criterion.
type Supersedes struct {
	Criterion string `json:"criterion"` // AC-n in this issue
	Test      string `json:"test"`      // "#N AC-n", an existing suite test
}

type BuildVerdict struct {
	Outcome      string   `json:"outcome"`
	Summary      string   `json:"summary"`
	PRNumber     *int     `json:"pr_number"`
	TestsAdded   []string `json:"tests_added"`
	TestsRetired []string `json:"tests_retired"`
	Blockers     []string `json:"blockers"`
	SkillsNotRun []string `json:"skills_not_run"`
}

type ReviewVerdict struct {
	Outcome       string   `json:"outcome"`
	Summary       string   `json:"summary"`
	ReviewedSHA   string   `json:"reviewed_sha"`
	CommentURL    string   `json:"comment_url"`
	FindingsCount int      `json:"findings_count"`
	SkillsNotRun  []string `json:"skills_not_run"`
}

type SweepVerdict struct {
	Outcome  string    `json:"outcome"`
	Summary  string    `json:"summary"`
	HeadSHA  string    `json:"head_sha"`
	Findings []Finding `json:"findings"`
	Blockers []string  `json:"blockers"`
}

// Finding is one review finding the sweep triaged.
type Finding struct {
	Category string `json:"category"`
	Severity string `json:"severity"`
	Status   string `json:"status"` // fixed, declined, open
	Summary  string `json:"summary"`
	URL      string `json:"url,omitempty"`
}

type ValidateVerdict struct {
	Outcome     string   `json:"outcome"`
	Summary     string   `json:"summary"`
	TestedSHA   string   `json:"tested_sha"`
	Criteria    []Result `json:"criteria"`
	Regressions []Result `json:"regressions"`
}

// Result is one criterion or regression check. ID is set for criteria,
// Name for regressions.
type Result struct {
	ID       string   `json:"id,omitempty"`
	Name     string   `json:"name,omitempty"`
	Result   string   `json:"result"`
	Evidence []string `json:"evidence"`
	Notes    string   `json:"notes"`
}

// ParseVerdict checks a worker's structured output against the stage's
// schema again (the worker is not trusted to have used it), then the rules
// a schema cannot say, and decodes it. Every problem is reported together.
func ParseVerdict(s config.Stage, b []byte) (*Verdict, error) {
	all, err := schemas()
	if err != nil {
		return nil, err
	}
	sch, ok := all[s]
	if !ok {
		return nil, fmt.Errorf("no verdict schema for stage %q", s)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%s verdict: not JSON: %w", s, err)
	}
	if err := sch.Validate(inst); err != nil {
		return nil, fmt.Errorf("%s verdict: %w", s, err)
	}
	v := &Verdict{Stage: s}
	var target any
	switch s {
	case config.StageIntake:
		v.Intake = &IntakeVerdict{}
		target = v.Intake
	case config.StageBuild:
		v.Build = &BuildVerdict{}
		target = v.Build
	case config.StageReview:
		v.Review = &ReviewVerdict{}
		target = v.Review
	case config.StageSweep:
		v.Sweep = &SweepVerdict{}
		target = v.Sweep
	case config.StageValidate:
		v.Validate = &ValidateVerdict{}
		target = v.Validate
	}
	// The schema already refused unknown fields; this decode cannot fail
	// on shape, only on a number too large for its Go type.
	if err := json.Unmarshal(b, target); err != nil {
		return nil, fmt.Errorf("%s verdict: %w", s, err)
	}
	var meta struct{ Outcome, Summary string }
	_ = json.Unmarshal(b, &meta)
	v.Outcome, v.Summary = meta.Outcome, meta.Summary
	if err := v.check(); err != nil {
		return nil, fmt.Errorf("%s verdict: %w", s, err)
	}
	return v, nil
}

// check holds the rules the schema leaves out: those that depend on the
// outcome, and uniqueness.
func (v *Verdict) check() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	switch {
	case v.Intake != nil:
		in := v.Intake
		if in.Outcome == "ready_for_approval" && len(in.CriteriaIDs) == 0 && len(in.SubIssues) == 0 {
			bad("ready_for_approval needs criteria_ids or sub_issues")
		}
		if in.Outcome == "needs_clarification" && len(in.Questions) == 0 {
			bad("needs_clarification needs at least one question")
		}
		ids := dupes(in.CriteriaIDs)
		for _, d := range ids {
			bad("criteria_ids: %s is listed twice", d)
		}
		known := map[string]bool{}
		for _, id := range in.CriteriaIDs {
			known[id] = true
		}
		for i, sp := range in.Supersedes {
			if !known[sp.Criterion] {
				bad("supersedes[%d].criterion: %s is not in criteria_ids", i, sp.Criterion)
			}
		}
	case v.Build != nil:
		bv := v.Build
		if bv.Outcome == "pr_opened" && bv.PRNumber == nil {
			bad("pr_opened needs pr_number")
		}
		if bv.Outcome == "blocked" && len(bv.Blockers) == 0 {
			bad("blocked needs at least one blocker")
		}
	case v.Sweep != nil:
		if v.Sweep.Outcome == "blocked" && len(v.Sweep.Blockers) == 0 {
			bad("blocked needs at least one blocker")
		}
	case v.Validate != nil:
		ids := make([]string, len(v.Validate.Criteria))
		for i, c := range v.Validate.Criteria {
			ids[i] = c.ID
		}
		for _, d := range dupes(ids) {
			bad("criteria: %s is reported twice", d)
		}
		if v.Validate.Outcome == "pass" && len(v.Validate.Criteria) == 0 {
			bad("outcome pass with no criteria reported")
		}
		// e2e_command is required in loop.json, so a pass always has at
		// least the suite to report.
		if v.Validate.Outcome == "pass" && len(v.Validate.Regressions) == 0 {
			bad("outcome pass with no regressions reported")
		}
		if v.Validate.Outcome == "pass" {
			for _, r := range append(append([]Result(nil), v.Validate.Criteria...), v.Validate.Regressions...) {
				if r.Result != "pass" {
					bad("outcome pass, but %s is %s", strings.TrimSpace(r.ID+r.Name), r.Result)
				}
			}
		}
	}
	return errors.Join(errs...)
}

func dupes(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if seen[x] {
			out = append(out, x)
		}
		seen[x] = true
	}
	return out
}

package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// LoopFile is where a consumer repo keeps its loop config.
const LoopFile = ".claude/loop.json"

// Default wall clocks from the spec, used when loop.json leaves a stage out.
var defaultWallClock = map[Stage]time.Duration{
	StageBuild:    90 * time.Minute,
	StageValidate: 60 * time.Minute,
}

const defaultWallClockOther = 30 * time.Minute

// alwaysRisk are risk paths in every repo whatever loop.json says: the loop
// config itself and the workflows and rulesets around it. Listing them in
// loop.json is allowed and changes nothing; leaving them out cannot turn
// them off.
var alwaysRisk = []string{".claude/**", ".github/**"}

// Loop is a repo's .claude/loop.json.
type Loop struct {
	DefaultBranch string            `json:"default_branch"`
	WIPLimit      int               `json:"wip_limit"`
	MergeMethod   string            `json:"merge_method"`
	AutoAdvance   bool              `json:"auto_advance"`
	AutoMerge     bool              `json:"auto_merge"`
	AutoPromote   bool              `json:"auto_promote"`
	Ratchet       bool              `json:"ratchet"`
	Limits        Limits            `json:"limits"`
	Models        map[string]string `json:"models"`
	MaxTurns      map[string]int    `json:"max_turns"`
	Machines      map[string]string `json:"machines"`
	RiskPaths     []string          `json:"risk_paths"`
	Deploy        Deploy            `json:"deploy"`
	Validate      ValidateConfig    `json:"validate"`

	risk []*regexp.Regexp
}

// Limits caps attempts, rounds and time.
type Limits struct {
	BuildAttempts int                 `json:"build_attempts"`
	ReviewRounds  int                 `json:"review_rounds"`
	StaleHours    int                 `json:"stale_hours"`
	WallClock     map[string]Duration `json:"wall_clock"`
}

// Deploy names the repo's deploy workflows and dev environment.
type Deploy struct {
	DevURL       string `json:"dev_url"`
	DevWorkflow  string `json:"dev_workflow"`
	ProdWorkflow string `json:"prod_workflow"`
}

// ValidateConfig is what the validator runs.
type ValidateConfig struct {
	Setup      string   `json:"setup"`
	E2ECommand string   `json:"e2e_command"`
	Smoke      []string `json:"smoke"`
}

var (
	mergeMethods = map[string]bool{"MERGE": true, "SQUASH": true, "REBASE": true}
	machineRe    = regexp.MustCompile(`^(shared|performance)-cpu-[0-9]+x:[0-9]+$`)
	modelRe      = regexp.MustCompile(`^claude-[a-z0-9-]+$`)
	branchRe     = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

// ParseLoop decodes loop.json strictly and reports every problem at once.
// A repo whose file fails here shows as "config invalid" and launches
// nothing.
func ParseLoop(b []byte) (*Loop, error) {
	var l Loop
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("%s: %w", LoopFile, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: trailing data after the object", LoopFile)
	}
	if err := l.validate(); err != nil {
		return nil, err
	}
	return &l, nil
}

func (l *Loop) validate() error {
	var p problems
	if l.DefaultBranch == "" || !branchRe.MatchString(l.DefaultBranch) {
		p.add("default_branch: required, a branch name")
	}
	if l.WIPLimit < 1 {
		p.add("wip_limit: at least 1")
	}
	if !mergeMethods[l.MergeMethod] {
		p.add("merge_method: want MERGE, SQUASH or REBASE, got %q", l.MergeMethod)
	}
	if l.Limits.BuildAttempts < 1 {
		p.add("limits.build_attempts: at least 1")
	}
	if l.Limits.ReviewRounds < 1 {
		p.add("limits.review_rounds: at least 1")
	}
	if l.Limits.StaleHours < 1 {
		p.add("limits.stale_hours: at least 1")
	}
	for k, d := range l.Limits.WallClock {
		checkStageKey(&p, "limits.wall_clock", k)
		if d <= 0 {
			p.add("limits.wall_clock.%s: above zero", k)
		}
	}

	requireDefault(&p, "models", keys(l.Models))
	for k, m := range l.Models {
		checkStageKey(&p, "models", k)
		if !modelRe.MatchString(m) {
			p.add("models.%s: want a Claude model id, got %q", k, m)
		}
	}
	requireDefault(&p, "max_turns", keys(l.MaxTurns))
	for k, n := range l.MaxTurns {
		checkStageKey(&p, "max_turns", k)
		if n < 1 {
			p.add("max_turns.%s: at least 1", k)
		}
	}
	requireDefault(&p, "machines", keys(l.Machines))
	for k, m := range l.Machines {
		checkStageKey(&p, "machines", k)
		if !machineRe.MatchString(m) {
			p.add("machines.%s: want a Fly size like \"shared-cpu-2x:2048\", got %q", k, m)
		}
	}

	for _, g := range append(append([]string(nil), alwaysRisk...), l.RiskPaths...) {
		re, err := compileGlob(g)
		if err != nil {
			p.add("risk_paths: %v", err)
			continue
		}
		l.risk = append(l.risk, re)
	}

	if u, err := url.Parse(l.Deploy.DevURL); err != nil || u.Scheme != "https" || u.Host == "" {
		p.add("deploy.dev_url: want an https URL, got %q", l.Deploy.DevURL)
	}
	if l.Deploy.DevWorkflow == "" {
		p.add("deploy.dev_workflow: required")
	}
	if l.Deploy.ProdWorkflow == "" {
		p.add("deploy.prod_workflow: required")
	}
	if strings.TrimSpace(l.Validate.E2ECommand) == "" {
		p.add("validate.e2e_command: required")
	}
	for i, s := range l.Validate.Smoke {
		if strings.TrimSpace(s) == "" {
			p.add("validate.smoke[%d]: empty", i)
		}
	}
	return p.err()
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func checkStageKey(p *problems, field, k string) {
	if k != "default" && !validStage(Stage(k)) {
		p.add("%s.%s: not a stage; want default, intake, build, review, sweep or validate", field, k)
	}
}

func requireDefault(p *problems, field string, ks []string) {
	for _, k := range ks {
		if k == "default" {
			return
		}
	}
	p.add("%s.default: required", field)
}

// ModelFor is the model a stage runs on.
func (l *Loop) ModelFor(s Stage) string {
	if m, ok := l.Models[string(s)]; ok {
		return m
	}
	return l.Models["default"]
}

// MaxTurnsFor is the stage's --max-turns.
func (l *Loop) MaxTurnsFor(s Stage) int {
	if n, ok := l.MaxTurns[string(s)]; ok {
		return n
	}
	return l.MaxTurns["default"]
}

// MachineFor is the stage's Fly Machine size.
func (l *Loop) MachineFor(s Stage) string {
	if m, ok := l.Machines[string(s)]; ok {
		return m
	}
	return l.Machines["default"]
}

// WallClockFor is how long a stage's worker may run: the stage's own entry,
// else loop.json's default, else the spec's default for the stage (build
// 90m, validate 60m, others 30m), never above ceiling.
func (l *Loop) WallClockFor(s Stage, ceiling time.Duration) time.Duration {
	d, ok := l.Limits.WallClock[string(s)]
	if !ok {
		d, ok = l.Limits.WallClock["default"]
	}
	if !ok {
		d = Duration(defaultWallClockOther)
		if v := defaultWallClock[s]; v > 0 {
			d = Duration(v)
		}
	}
	if ceiling > 0 && d.Std() > ceiling {
		return ceiling
	}
	return d.Std()
}

// IsRiskPath reports whether a repo-relative path matches a risk path,
// including the ones every repo has.
func (l *Loop) IsRiskPath(path string) bool {
	path = strings.TrimPrefix(path, "/")
	for _, re := range l.risk {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// RiskPathsHit returns the changed paths that are risk paths, in order.
func (l *Loop) RiskPathsHit(paths []string) []string {
	var hit []string
	for _, p := range paths {
		if l.IsRiskPath(p) {
			hit = append(hit, p)
		}
	}
	return hit
}

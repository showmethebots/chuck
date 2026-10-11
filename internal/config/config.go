// Package config loads and validates the agent's YAML configuration:
// config/routes.yaml (routing rules) and config/policy.yaml (permission
// tiers, budgets, caps). Decoding is strict: unknown keys are errors, so a
// typo cannot silently disable a rule.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	RoutesFile = "routes.yaml"
	PolicyFile = "policy.yaml"
)

// Tier is who handles a piece of work.
type Tier string

const (
	TierHaiku  Tier = "haiku"
	TierSonnet Tier = "sonnet"
	TierOpus   Tier = "opus"
	TierHuman  Tier = "human"

	// DefaultHaikuClassify sends unmatched events to Haiku to classify.
	// Only valid as routes.yaml `default`.
	DefaultHaikuClassify Tier = "haiku_classify"
)

// ModelTiers are the tiers backed by a model, and so carry a budget.
var ModelTiers = []Tier{TierHaiku, TierSonnet, TierOpus}

func (t Tier) valid() bool {
	switch t {
	case TierHaiku, TierSonnet, TierOpus, TierHuman:
		return true
	}
	return false
}

// Permission is how freely the agent may take an action.
type Permission string

const (
	ActFreely     Permission = "act_freely"
	ActThenNotify Permission = "act_then_notify"
	AskFirst      Permission = "ask_first"
)

// Config is the full validated configuration.
type Config struct {
	Routes Routes
	Policy Policy
}

// Routes is config/routes.yaml.
type Routes struct {
	Routes               []Route `yaml:"routes"`
	Default              Tier    `yaml:"default"`
	PromoteAfterFailures int     `yaml:"promote_after_failures"`
}

// Route sends work matching Match to Tier. First match wins.
type Route struct {
	Match Match `yaml:"match"`
	Tier  Tier  `yaml:"tier"`
}

// Match is a conjunction: every key that is set must match. A list value
// matches if any element does.
type Match struct {
	Event       StringList `yaml:"event"`
	Task        StringList `yaml:"task"`
	Action      StringList `yaml:"action"`
	AttemptsRaw string     `yaml:"attempts"`

	// Attempts is AttemptsRaw parsed during validation; nil when unset.
	Attempts *Attempts `yaml:"-"`
}

// Attempts compares a task's failure count, e.g. "<2" or ">=2".
type Attempts struct {
	Op string
	N  int
}

// Matches reports whether a task with n failed attempts satisfies a.
func (a Attempts) Matches(n int) bool {
	switch a.Op {
	case "<":
		return n < a.N
	case "<=":
		return n <= a.N
	case ">":
		return n > a.N
	case ">=":
		return n >= a.N
	case "==":
		return n == a.N
	}
	return false
}

func parseAttempts(s string) (*Attempts, error) {
	s = strings.TrimSpace(s)
	// Two-character operators first so "<=" is not read as "<".
	for _, op := range []string{"<=", ">=", "==", "<", ">"} {
		if rest, ok := strings.CutPrefix(s, op); ok {
			n, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil || n < 0 {
				break
			}
			return &Attempts{Op: op, N: n}, nil
		}
	}
	return nil, fmt.Errorf("attempts %q: want <N, <=N, >N, >=N, or ==N with N >= 0", s)
}

// StringList accepts either a single string or a list of strings.
type StringList []string

func (l *StringList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*l = StringList{n.Value}
		return nil
	}
	var s []string
	if err := n.Decode(&s); err != nil {
		return err
	}
	*l = s
	return nil
}

// Policy is config/policy.yaml.
type Policy struct {
	Permissions Permissions `yaml:"permissions"`
	Budgets     Budgets     `yaml:"budgets"`
	Caps        Caps        `yaml:"caps"`
}

type Permissions struct {
	ActFreely     []string `yaml:"act_freely"`
	ActThenNotify []string `yaml:"act_then_notify"`
	AskFirst      []string `yaml:"ask_first"`
}

type Budgets struct {
	DailyUSD                  map[Tier]float64 `yaml:"daily_usd"`
	SpendApprovalThresholdUSD float64          `yaml:"spend_approval_threshold_usd"`
}

type Caps struct {
	TaskMaxIterations  int      `yaml:"task_max_iterations"`
	WorkerMaxWallClock Duration `yaml:"worker_max_wall_clock"`
	ReaperInterval     Duration `yaml:"reaper_interval"`
}

// Duration is a time.Duration written as a Go duration string ("2h", "5m").
// Parsing happens during validation so the error can name the field.
type Duration struct {
	time.Duration
	raw string
}

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	d.raw = n.Value
	return nil
}

func (d *Duration) parse() error {
	v, err := time.ParseDuration(d.raw)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// PermissionOf returns the permission tier an action is declared in, or ""
// if it is not declared.
func (p Policy) PermissionOf(action string) Permission {
	for perm, actions := range p.Permissions.byTier() {
		for _, a := range actions {
			if a == action {
				return perm
			}
		}
	}
	return ""
}

func (p Permissions) byTier() map[Permission][]string {
	return map[Permission][]string{
		ActFreely:     p.ActFreely,
		ActThenNotify: p.ActThenNotify,
		AskFirst:      p.AskFirst,
	}
}

// Load reads routes.yaml and policy.yaml from dir and validates them,
// reporting every problem found rather than stopping at the first.
func Load(dir string) (*Config, error) {
	var cfg Config
	if err := decodeFile(filepath.Join(dir, RoutesFile), &cfg.Routes); err != nil {
		return nil, err
	}
	if err := decodeFile(filepath.Join(dir, PolicyFile), &cfg.Policy); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func decodeFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

func (c *Config) validate() error {
	var errs []error
	routesErr := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf(RoutesFile+": "+format, a...))
	}
	policyErr := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf(PolicyFile+": "+format, a...))
	}

	// policy.yaml: each action in exactly one permission tier.
	seen := map[string]Permission{}
	for _, perm := range []Permission{ActFreely, ActThenNotify, AskFirst} {
		for _, a := range c.Policy.Permissions.byTier()[perm] {
			if a == "" {
				policyErr("permissions.%s: empty action", perm)
				continue
			}
			if prev, ok := seen[a]; ok {
				policyErr("action %q is in both %s and %s", a, prev, perm)
				continue
			}
			seen[a] = perm
		}
	}

	for k := range c.Policy.Budgets.DailyUSD {
		if k == TierHuman || !k.valid() {
			policyErr("budgets.daily_usd: unknown model tier %q", k)
		}
	}
	for _, t := range ModelTiers {
		v, ok := c.Policy.Budgets.DailyUSD[t]
		switch {
		case !ok:
			policyErr("budgets.daily_usd.%s is required", t)
		case v <= 0:
			policyErr("budgets.daily_usd.%s must be > 0", t)
		}
	}
	if c.Policy.Budgets.SpendApprovalThresholdUSD <= 0 {
		policyErr("budgets.spend_approval_threshold_usd must be > 0")
	}
	if c.Policy.Caps.TaskMaxIterations < 1 {
		policyErr("caps.task_max_iterations must be >= 1")
	}
	for _, d := range []struct {
		name string
		d    *Duration
	}{
		{"worker_max_wall_clock", &c.Policy.Caps.WorkerMaxWallClock},
		{"reaper_interval", &c.Policy.Caps.ReaperInterval},
	} {
		switch {
		case d.d.raw == "":
			policyErr("caps.%s must be > 0", d.name)
		case d.d.parse() != nil:
			policyErr("caps.%s: invalid duration %q", d.name, d.d.raw)
		case d.d.Duration <= 0:
			policyErr("caps.%s must be > 0", d.name)
		}
	}

	// routes.yaml.
	if c.Routes.Default != DefaultHaikuClassify && !c.Routes.Default.valid() {
		routesErr("default: unknown tier %q", c.Routes.Default)
	}
	if c.Routes.PromoteAfterFailures < 1 {
		routesErr("promote_after_failures must be >= 1")
	}
	for i := range c.Routes.Routes {
		r := &c.Routes.Routes[i]
		at := fmt.Sprintf("routes[%d]", i)
		if !r.Tier.valid() {
			routesErr("%s: unknown tier %q", at, r.Tier)
		}
		m := &r.Match
		if len(m.Event)+len(m.Task)+len(m.Action) == 0 {
			if m.AttemptsRaw == "" {
				routesErr("%s: match is empty", at)
			} else {
				routesErr("%s: attempts needs event, task, or action alongside it", at)
			}
		}
		if m.AttemptsRaw != "" {
			a, err := parseAttempts(m.AttemptsRaw)
			if err != nil {
				routesErr("%s: %v", at, err)
			}
			m.Attempts = a
		}
		for _, kv := range []struct {
			key  string
			list StringList
		}{{"event", m.Event}, {"task", m.Task}, {"action", m.Action}} {
			for _, v := range kv.list {
				if v == "" {
					routesErr("%s: match.%s has an empty value", at, kv.key)
				}
			}
		}
		for _, a := range m.Action {
			if a == "" {
				continue
			}
			perm, declared := seen[a]
			if !declared {
				routesErr("%s: action %q is not declared in %s permissions", at, a, PolicyFile)
				continue
			}
			// Clark stays the gate: an ask_first action can never be
			// routed to a model.
			if perm == AskFirst && r.Tier != TierHuman {
				routesErr("%s: ask_first action %q must route to human, not %q", at, a, r.Tier)
			}
		}
	}

	return errors.Join(errs...)
}

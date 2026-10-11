package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validRoutes = `
routes:
  - match: {event: ci_failed, attempts: "<2"}
    tier: sonnet
  - match: {action: [merge, deploy]}
    tier: human
default: haiku_classify
promote_after_failures: 2
`

const validPolicy = `
permissions:
  act_freely: [read, triage]
  act_then_notify: [create_issue]
  ask_first: [merge, deploy]
budgets:
  daily_usd: {haiku: 5, sonnet: 15, opus: 10}
  spend_approval_threshold_usd: 5
caps:
  task_max_iterations: 10
  worker_max_wall_clock: 2h
  reaper_interval: 5m
`

func writeConfig(t *testing.T, routes, policy string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "routes.yaml"), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "policy.yaml"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The config shipped in the repo is the golden case: if it stops loading,
// the daemon will not start in production.
func TestLoadShippedConfig(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config"))
	if err != nil {
		t.Fatalf("shipped config invalid: %v", err)
	}
	if len(cfg.Routes.Routes) == 0 {
		t.Fatal("shipped config has no routes")
	}
}

func TestLoadParsesValues(t *testing.T) {
	cfg, err := Load(writeConfig(t, validRoutes, validPolicy))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	r := cfg.Routes.Routes[0]
	if got := r.Match.Event; len(got) != 1 || got[0] != "ci_failed" {
		t.Errorf("event = %v, want [ci_failed]", got)
	}
	if r.Match.Attempts == nil || r.Match.Attempts.Op != "<" || r.Match.Attempts.N != 2 {
		t.Errorf("attempts = %+v, want <2", r.Match.Attempts)
	}
	if r.Tier != TierSonnet {
		t.Errorf("tier = %q, want sonnet", r.Tier)
	}
	if got := cfg.Routes.Routes[1].Match.Action; len(got) != 2 {
		t.Errorf("action list = %v, want 2 entries", got)
	}
	if cfg.Routes.Default != "haiku_classify" {
		t.Errorf("default = %q", cfg.Routes.Default)
	}
	if cfg.Policy.Caps.WorkerMaxWallClock.Duration != 2*time.Hour {
		t.Errorf("wall clock = %v, want 2h", cfg.Policy.Caps.WorkerMaxWallClock)
	}
	if cfg.Policy.Budgets.DailyUSD[TierOpus] != 10 {
		t.Errorf("opus budget = %v, want 10", cfg.Policy.Budgets.DailyUSD[TierOpus])
	}
	if got := cfg.Policy.PermissionOf("merge"); got != AskFirst {
		t.Errorf("PermissionOf(merge) = %q, want ask_first", got)
	}
}

func TestAttemptsMatches(t *testing.T) {
	cases := []struct {
		expr string
		n    int
		want bool
	}{
		{"<2", 1, true}, {"<2", 2, false},
		{"<=2", 2, true}, {">=2", 2, true}, {">=2", 1, false},
		{">2", 3, true}, {"==0", 0, true}, {"==0", 1, false},
	}
	for _, c := range cases {
		a, err := parseAttempts(c.expr)
		if err != nil {
			t.Fatalf("parseAttempts(%q): %v", c.expr, err)
		}
		if got := a.Matches(c.n); got != c.want {
			t.Errorf("%q.Matches(%d) = %v, want %v", c.expr, c.n, got, c.want)
		}
	}
}

func TestLoadRejectsInvalid(t *testing.T) {
	cases := []struct {
		name    string
		routes  string // replacement pair applied to validRoutes, "old|new"
		policy  string // replacement pair applied to validPolicy, "old|new"
		wantErr string
	}{
		{name: "unknown top-level key", routes: "default:|defualt_typo: x\ndefault:", wantErr: "defualt_typo"},
		{name: "unknown match key", routes: "event: ci_failed,|evnt: ci_failed,", wantErr: "evnt"},
		{name: "unknown tier", routes: "tier: sonnet|tier: gpt", wantErr: `unknown tier "gpt"`},
		{name: "unknown default", routes: "default: haiku_classify|default: whatever", wantErr: `default: unknown tier "whatever"`},
		{name: "empty match", routes: `match: {event: ci_failed, attempts: "<2"}|match: {}`, wantErr: "match is empty"},
		{name: "bad attempts", routes: `attempts: "<2"|attempts: "about 2"`, wantErr: `attempts "about 2"`},
		{name: "attempts alone", routes: `{event: ci_failed, attempts: "<2"}|{attempts: "<2"}`, wantErr: "attempts needs event, task, or action"},
		{name: "empty string in list", routes: "[merge, deploy]|[merge, \"\"]", wantErr: "empty value"},
		{name: "promote below 1", routes: "promote_after_failures: 2|promote_after_failures: 0", wantErr: "promote_after_failures must be >= 1"},
		{name: "no routes", routes: "routes:|routes: []\nold_routes:", wantErr: "old_routes"},
		{name: "undeclared action", routes: "[merge, deploy]|[merge, yeet]", wantErr: `action "yeet" is not declared`},
		{name: "ask_first not to human", routes: "tier: human|tier: opus", wantErr: `ask_first action "merge" must route to human`},
		{name: "action in two tiers", policy: "act_freely: [read, triage]|act_freely: [read, triage, merge]", wantErr: `action "merge" is in both`},
		{name: "missing tier budget", policy: "haiku: 5, |", wantErr: "budgets.daily_usd.haiku is required"},
		{name: "budget for human", policy: "opus: 10}|opus: 10, human: 1}", wantErr: `budgets.daily_usd: unknown model tier "human"`},
		{name: "negative budget", policy: "sonnet: 15|sonnet: -1", wantErr: "budgets.daily_usd.sonnet must be > 0"},
		{name: "zero threshold", policy: "spend_approval_threshold_usd: 5|spend_approval_threshold_usd: 0", wantErr: "spend_approval_threshold_usd must be > 0"},
		{name: "bad duration", policy: "worker_max_wall_clock: 2h|worker_max_wall_clock: two hours", wantErr: "worker_max_wall_clock"},
		{name: "missing cap", policy: "  reaper_interval: 5m\n|", wantErr: "caps.reaper_interval must be > 0"},
		{name: "zero iterations", policy: "task_max_iterations: 10|task_max_iterations: 0", wantErr: "caps.task_max_iterations must be >= 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			routes, policy := validRoutes, validPolicy
			if c.routes != "" {
				routes = replaceOnce(t, routes, c.routes)
			}
			if c.policy != "" {
				policy = replaceOnce(t, policy, c.policy)
			}
			_, err := Load(writeConfig(t, routes, policy))
			if err == nil {
				t.Fatalf("Load succeeded, want error containing %q", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v\nwant it to contain %q", err, c.wantErr)
			}
		})
	}
}

// Validation reports every problem at once, so one CI run shows them all.
func TestLoadReportsAllErrors(t *testing.T) {
	routes := replaceOnce(t, validRoutes, "tier: sonnet|tier: gpt")
	policy := replaceOnce(t, validPolicy, "sonnet: 15|sonnet: -1")
	_, err := Load(writeConfig(t, routes, policy))
	if err == nil {
		t.Fatal("Load succeeded, want errors")
	}
	for _, want := range []string{`unknown tier "gpt"`, "budgets.daily_usd.sonnet must be > 0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "routes.yaml") {
		t.Fatalf("error = %v, want it to name routes.yaml", err)
	}
}

func replaceOnce(t *testing.T, s, pair string) string {
	t.Helper()
	old, new, ok := strings.Cut(pair, "|")
	if !ok || strings.Count(s, old) != 1 {
		t.Fatalf("replacement %q must match exactly once", old)
	}
	return strings.Replace(s, old, new, 1)
}

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestValidateShippedConfig(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"validate"},
		env(map[string]string{"CHUCK_CONFIG_DIR": "../../config"}), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut.String())
	}
}

func TestValidateReportsBadConfig(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"routes.yaml": "routes: []\ndefault: nope\npromote_after_failures: 2\n",
		"policy.yaml": "permissions: {}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"validate"},
		env(map[string]string{"CHUCK_CONFIG_DIR": dir}), &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), `unknown tier "nope"`) {
		t.Fatalf("stderr does not name the problem: %s", errOut.String())
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"frobnicate"}, env(nil), &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

// run refuses to start the daemon on invalid config, before touching the DB.
func TestRunFailsFastOnBadConfig(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"run"}, env(map[string]string{
		"CHUCK_CONFIG_DIR": t.TempDir(),
		"CHUCK_DB_PATH":    dbPath,
	}), &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("database was created despite invalid config")
	}
}

func TestLoadIntegrations(t *testing.T) {
	in, err := loadIntegrations(env(nil))
	if err != nil || len(in.names()) != 0 {
		t.Fatalf("no env: %v, names %v; want none", err, in.names())
	}
	in, err = loadIntegrations(env(map[string]string{
		"GITHUB_APP_WEBHOOK_SECRET": "s",
		"ANTHROPIC_API_KEY":         "k",
	}))
	if err != nil || in.WebhookSecret != "s" || in.AnthropicKey != "k" {
		t.Fatalf("got %+v, %v", in, err)
	}
	if got := strings.Join(in.names(), ","); got != "github_webhook,triage" {
		t.Fatalf("names = %s", got)
	}
}

func slackEnvVars() map[string]string {
	return map[string]string{
		"SLACK_BOT_TOKEN":            "xoxb-1",
		"SLACK_APP_TOKEN":            "xapp-1",
		"CHUCK_SLACK_MAIN_CHANNEL":   "CMAIN",
		"CHUCK_SLACK_SHADOW_CHANNEL": "CSHADOW",
	}
}

func TestLoadIntegrationsSlack(t *testing.T) {
	in, err := loadIntegrations(env(slackEnvVars()))
	if err != nil || in.Slack == nil || in.Slack.MainChannel != "CMAIN" {
		t.Fatalf("full Slack env: %+v, %v", in.Slack, err)
	}
	if got := strings.Join(in.names(), ","); got != "slack" {
		t.Fatalf("names = %s", got)
	}

	partial := slackEnvVars()
	delete(partial, "SLACK_APP_TOKEN")
	delete(partial, "CHUCK_SLACK_SHADOW_CHANNEL")
	_, err = loadIntegrations(env(partial))
	if err == nil || !strings.Contains(err.Error(), "SLACK_APP_TOKEN") || !strings.Contains(err.Error(), "CHUCK_SLACK_SHADOW_CHANNEL") {
		t.Fatalf("partial Slack env: err = %v, want both missing names", err)
	}
}

func TestLoadIntegrationsSchedule(t *testing.T) {
	in, err := loadIntegrations(env(nil))
	if err != nil || in.GradeSample != 5 {
		t.Fatalf("defaults: %+v, %v", in, err)
	}
	if got := in.Daily.Next(time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)); got.Hour() != 13 {
		t.Fatalf("default daily time = %v, want 13:00", got)
	}
	for k, v := range map[string]string{"CHUCK_DAILY_AT": "1pm", "CHUCK_GRADE_SAMPLE": "-1"} {
		if _, err := loadIntegrations(env(map[string]string{k: v})); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%s: err = %v", k, v, err)
		}
	}
}

// Turning Slack on without an allowlist would leave chuck listening to
// nobody, or worse; it must refuse before touching the database.
func TestRunRefusesSlackWithoutAllowlist(t *testing.T) {
	// The shipped config gains slack.yaml later, so copy only the other two.
	cfgDir := t.TempDir()
	for _, name := range []string{"routes.yaml", "policy.yaml"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "config", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfgDir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dbPath := filepath.Join(t.TempDir(), "state.db")
	vars := slackEnvVars()
	vars["CHUCK_CONFIG_DIR"] = cfgDir
	vars["CHUCK_DB_PATH"] = dbPath
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"run"}, env(vars), &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(out.String(), "slack.yaml") {
		t.Fatalf("log does not name slack.yaml: %s", out.String())
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatal("database was created")
	}
}

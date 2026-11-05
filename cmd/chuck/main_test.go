package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// A config directory left over from the project-manager framing must not
// pass: its rules no longer hold, and someone may still expect them to.
func TestValidateRejectsRetiredConfig(t *testing.T) {
	dir := t.TempDir()
	for _, name := range retiredConfig {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"validate"},
		env(map[string]string{"CHUCK_CONFIG_DIR": dir}), &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	for _, name := range retiredConfig {
		if !strings.Contains(errOut.String(), name) {
			t.Errorf("stderr does not name %s: %s", name, errOut.String())
		}
	}
}

func TestValidateMissingDir(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"validate"},
		env(map[string]string{"CHUCK_CONFIG_DIR": filepath.Join(t.TempDir(), "nope")}), &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"replay"}, env(nil), &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

// run refuses to start on invalid config, before touching the DB.
func TestRunFailsFastOnBadConfig(t *testing.T) {
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "slack.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "state.db")
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"run"}, env(map[string]string{
		"CHUCK_CONFIG_DIR": cfgDir,
		"CHUCK_DB_PATH":    dbPath,
	}), &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("database was created despite invalid config")
	}
}

func TestIntegrations(t *testing.T) {
	if got := integrations(""); len(got) != 0 {
		t.Fatalf("no secret: %v, want none", got)
	}
	if got := strings.Join(integrations("s"), ","); got != "github_webhook" {
		t.Fatalf("with secret: %s", got)
	}
}

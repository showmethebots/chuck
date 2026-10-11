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
		env(map[string]string{"UNCHARLES_CONFIG_DIR": "../../config"}), &out, &errOut)
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
		env(map[string]string{"UNCHARLES_CONFIG_DIR": dir}), &out, &errOut)
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
		"UNCHARLES_CONFIG_DIR": t.TempDir(),
		"UNCHARLES_DB_PATH":    dbPath,
	}), &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("database was created despite invalid config")
	}
}

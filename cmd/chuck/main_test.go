package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
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

// A config directory left over from the project-manager framing must not
// pass: its rules no longer hold, and someone may still expect them to.
func TestValidateRejectsRetiredConfig(t *testing.T) {
	dir := copyShippedConfig(t)
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

// copyShippedConfig returns a temp directory holding the shipped
// chuck.yaml.
func copyShippedConfig(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../config/chuck.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "chuck.yaml"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestValidateReportsBadChuckConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "chuck.yaml"), []byte("humans: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"validate"},
		env(map[string]string{"CHUCK_CONFIG_DIR": dir}), &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "humans: at least one") {
		t.Fatalf("exit %d, stderr: %s", code, errOut.String())
	}
}

func TestValidateLoopFiles(t *testing.T) {
	good := "../../internal/config/testdata/loop/valid/spec-example.json"
	bad := "../../internal/config/testdata/loop/invalid/wip-zero.json"
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"validate", good, bad},
		env(map[string]string{"CHUCK_CONFIG_DIR": "../../config"}), &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(out.String(), "spec-example.json is valid") || !strings.Contains(errOut.String(), "wip_limit") {
		t.Fatalf("stdout: %s\nstderr: %s", out.String(), errOut.String())
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
	cfgDir := copyShippedConfig(t)
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

func TestLoopTurnsOnOnlyWhenEveryReferenceResolves(t *testing.T) {
	c, err := checkConfigDir("../../config")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	full := map[string]string{
		"GITHUB_APP_ID": "123", "GITHUB_APP_PRIVATE_KEY": pemKey,
		"GITHUB_OAUTH_CLIENT_ID": "Iv1.x", "GITHUB_OAUTH_CLIENT_SECRET": "secret-value",
		"CHUCK_WORKER_IMAGE": "registry.fly.io/chuck-workers:abc",
	}
	for missing := range full {
		partial := map[string]string{}
		for k, v := range full {
			if k != missing {
				partial[k] = v
			}
		}
		eng, err := newEngine(c, env(partial), nil, log)
		if err != nil || eng != nil {
			t.Errorf("without %s: engine %v, err %v; want the loop off", missing, eng != nil, err)
		}
	}
	if strings.Contains(logs.String(), "secret-value") || strings.Contains(logs.String(), "PRIVATE KEY") {
		t.Errorf("a secret reached the log: %s", logs.String())
	}
	eng, err := newEngine(c, env(full), nil, log)
	if err != nil || eng == nil {
		t.Fatalf("with everything set: %v, %v", eng, err)
	}
	if eng.Tick != 30*time.Minute || len(eng.Humans) != 1 || len(eng.GitHub.Apps()) != 1 {
		t.Errorf("engine %+v", eng)
	}
}

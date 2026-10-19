package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/joho/godotenv"
)

func TestLoadDotenvMissingFileIsNotAnError(t *testing.T) {
	if err := loadDotenv(filepath.Join(t.TempDir(), ".env")); err != nil {
		t.Fatalf("missing .env: %v", err)
	}
}

func TestLoadDotenvSetsUnsetVarsOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	body := "CHUCK_DOTENV_TEST_NEW=from-file\nCHUCK_DOTENV_TEST_SET=from-file\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHUCK_DOTENV_TEST_NEW", "")
	if err := os.Unsetenv("CHUCK_DOTENV_TEST_NEW"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHUCK_DOTENV_TEST_SET", "from-env")

	if err := loadDotenv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("CHUCK_DOTENV_TEST_NEW"); got != "from-file" {
		t.Errorf("unset var = %q, want from-file", got)
	}
	if got := os.Getenv("CHUCK_DOTENV_TEST_SET"); got != "from-env" {
		t.Errorf("set var = %q, want from-env: the environment must win over .env", got)
	}
}

func TestLoadDotenvReportsMalformedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("NOT A VALID LINE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadDotenv(path); err == nil {
		t.Fatal("malformed .env loaded without error")
	}
}

// TestEnvExampleMatchesDocComment keeps .env.example and the package doc
// comment listing the same variables, so neither drifts from the other.
func TestEnvExampleMatchesDocComment(t *testing.T) {
	example, err := godotenv.Read("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	doc, _, _ := strings.Cut(string(src), "\npackage main")
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^//\t([A-Z][A-Z0-9_]+) `).FindAllStringSubmatch(doc, -1) {
		documented[m[1]] = true
	}
	if len(documented) == 0 {
		t.Fatal("found no variables in the main.go doc comment")
	}
	for name := range documented {
		if _, ok := example[name]; !ok {
			t.Errorf("%s is in the main.go doc comment but not .env.example", name)
		}
	}
	for name := range example {
		if !documented[name] {
			t.Errorf("%s is in .env.example but not the main.go doc comment", name)
		}
	}
}

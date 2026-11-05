package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustLoop(t *testing.T, name string) *Loop {
	t.Helper()
	l, err := ParseLoop(readFixture(t, filepath.Join("testdata", "loop", "valid", name)))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return l
}

func TestValidFixturesParse(t *testing.T) {
	files, _ := filepath.Glob("testdata/loop/valid/*.json")
	if len(files) == 0 {
		t.Fatal("no valid fixtures")
	}
	for _, f := range files {
		if _, err := ParseLoop(readFixture(t, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestInvalidFixturesFail(t *testing.T) {
	files, _ := filepath.Glob("testdata/loop/invalid*/*.json")
	if len(files) == 0 {
		t.Fatal("no invalid fixtures")
	}
	for _, f := range files {
		if _, err := ParseLoop(readFixture(t, f)); err == nil {
			t.Errorf("%s: parsed without error", f)
		}
	}
}

// The shipped JSON Schema is for consumers' editors and CI; chuck's own
// validator is the one that decides. They must agree on every fixture, or
// a consumer's CI passes a file chuck then refuses (or the reverse).
// invalid-go-only holds what a schema cannot express (a repeated key,
// since a parsed instance has already dropped it); chuck alone rejects
// those.
func TestSchemaAgreesWithValidator(t *testing.T) {
	c := jsonschema.NewCompiler()
	sch, err := c.Compile("../../config/loop.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"valid", "invalid"} {
		files, _ := filepath.Glob(filepath.Join("testdata", "loop", dir, "*.json"))
		for _, f := range files {
			b := readFixture(t, f)
			inst, err := jsonschema.UnmarshalJSON(strings.NewReader(string(b)))
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			schemaOK := sch.Validate(inst) == nil
			_, goErr := ParseLoop(b)
			if schemaOK != (goErr == nil) {
				t.Errorf("%s: schema valid=%v, validator err=%v", f, schemaOK, goErr)
			}
		}
	}
}

func TestEveryProblemReportedTogether(t *testing.T) {
	b := []byte(`{
		"default_branch": "", "wip_limit": 0, "merge_method": "nope",
		"limits": {"build_attempts": 0, "review_rounds": 0, "stale_hours": 0},
		"models": {}, "max_turns": {}, "machines": {},
		"deploy": {"dev_url": "", "dev_workflow": "", "prod_workflow": ""},
		"validate": {"e2e_command": ""}
	}`)
	_, err := ParseLoop(b)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{
		"default_branch", "wip_limit", "merge_method", "build_attempts", "review_rounds",
		"stale_hours", "models.default", "max_turns.default", "machines.default",
		"dev_url", "dev_workflow", "prod_workflow", "e2e_command",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

// encoding/json alone would let "AUTO_MERGE": true or a second
// "auto_merge" override a visible "auto_merge": false.
func TestKeysAreExactAndUnique(t *testing.T) {
	for _, f := range []string{"invalid/case-variant-key.json", "invalid-go-only/duplicate-key.json"} {
		_, err := ParseLoop(readFixture(t, filepath.Join("testdata", "loop", f)))
		if err == nil {
			t.Errorf("%s: parsed", f)
		}
	}
	nested := `{"limits": {"build_attempts": 1, "Build_Attempts": 9}, "models": {"default": "claude-a", "default": "claude-b"}}`
	err := checkJSONKeys([]byte(nested), reflect.TypeOf(Loop{}))
	if err == nil || !strings.Contains(err.Error(), "limits.Build_Attempts: unknown field") ||
		!strings.Contains(err.Error(), "models.default: key appears more than once") {
		t.Fatalf("nested: %v", err)
	}
}

func TestTrailingDataRejected(t *testing.T) {
	b := append(readFixture(t, "testdata/loop/valid/minimal.json"), []byte(`{}`)...)
	if _, err := ParseLoop(b); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("err = %v, want trailing data", err)
	}
}

func TestPerStageLookups(t *testing.T) {
	l := mustLoop(t, "spec-example.json")
	if got := l.ModelFor(StageBuild); got != "claude-opus-5-5" {
		t.Errorf("build model = %s", got)
	}
	if got := l.ModelFor(StageReview); got != "claude-sonnet-5-5" {
		t.Errorf("review model = %s, want the default", got)
	}
	if got := l.MaxTurnsFor(StageSweep); got != 60 {
		t.Errorf("sweep max turns = %d", got)
	}
	if got := l.MachineFor(StageValidate); got != "shared-cpu-2x:4096" {
		t.Errorf("validate machine = %s", got)
	}
	if got := l.MachineFor(StageIntake); got != "shared-cpu-2x:2048" {
		t.Errorf("intake machine = %s", got)
	}
}

func TestWallClockDefaultsAndCeiling(t *testing.T) {
	l := mustLoop(t, "minimal.json") // no wall_clock at all
	for s, want := range map[Stage]time.Duration{
		StageBuild: 90 * time.Minute, StageValidate: 60 * time.Minute,
		StageIntake: 30 * time.Minute, StageReview: 30 * time.Minute, StageSweep: 30 * time.Minute,
	} {
		if got := l.WallClockFor(s, 4*time.Hour); got != want {
			t.Errorf("%s = %v, want %v", s, got, want)
		}
	}
	l.Limits.WallClock = map[string]Duration{"default": Duration(45 * time.Minute), "build": Duration(5 * time.Hour)}
	if got := l.WallClockFor(StageBuild, 4*time.Hour); got != 4*time.Hour {
		t.Errorf("build above ceiling = %v, want clamped to 4h", got)
	}
	if got := l.WallClockFor(StageValidate, 4*time.Hour); got != 45*time.Minute {
		t.Errorf("validate = %v, want loop.json default 45m", got)
	}
}

func TestRiskPaths(t *testing.T) {
	l := mustLoop(t, "spec-example.json")
	for path, want := range map[string]bool{
		".claude/loop.json":                 true,
		".github/workflows/deploy-prod.yml": true,
		"db/migrations/0001.sql":            true,
		"migrations/0001.sql":               true,
		"internal/auth/session.go":          true,
		"package.json":                      true,
		"web/package.json":                  false, // root-anchored
		"src/app.ts":                        false,
		"docs/migrations.md":                false,
		".claudeignore":                     false,
	} {
		if got := l.IsRiskPath(path); got != want {
			t.Errorf("IsRiskPath(%q) = %v, want %v", path, got, want)
		}
	}
	if hit := l.RiskPathsHit([]string{"src/a.ts", "package.json", ".github/x"}); len(hit) != 2 {
		t.Errorf("RiskPathsHit = %v", hit)
	}
}

// A repo that leaves .claude/** out of risk_paths must not be able to
// auto-merge a change to its own loop.json.
func TestLoopConfigIsAlwaysARiskPath(t *testing.T) {
	l := mustLoop(t, "minimal.json")
	if len(l.RiskPaths) != 0 {
		t.Fatal("fixture should list no risk paths")
	}
	for _, p := range []string{".claude/loop.json", ".github/workflows/ci.yml", ".github/CODEOWNERS"} {
		if !l.IsRiskPath(p) {
			t.Errorf("%s is not a risk path", p)
		}
	}
}

func TestCompileGlob(t *testing.T) {
	for _, bad := range []string{"", "/a", "a/**b", "a//b", "a/{b,c}", "a/[bc]", "./a", "a/../b"} {
		if _, err := compileGlob(bad); err == nil {
			t.Errorf("compileGlob(%q) accepted", bad)
		}
	}
	re, err := compileGlob("src/*.go")
	if err != nil {
		t.Fatal(err)
	}
	if !re.MatchString("src/a.go") || re.MatchString("src/x/a.go") || re.MatchString("src/a.go.bak") {
		t.Errorf("src/*.go matched wrongly")
	}
	re, _ = compileGlob("a?c")
	if !re.MatchString("abc") || re.MatchString("a/c") {
		t.Errorf("a?c matched wrongly")
	}
}

func TestDurationJSON(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"90m"`), &d); err != nil || d.Std() != 90*time.Minute {
		t.Fatalf("90m = %v, %v", d, err)
	}
	if err := json.Unmarshal([]byte(`90`), &d); err == nil {
		t.Fatal("a bare number was accepted")
	}
}

// A Loop built in code rather than by ParseLoop must not treat every path
// as safe.
func TestIsRiskPathFailsClosed(t *testing.T) {
	var l Loop
	if !l.IsRiskPath(".claude/loop.json") || !l.IsRiskPath(".github/workflows/ci.yml") {
		t.Error("zero Loop: always-on risk paths not matched")
	}
	if l.IsRiskPath("src/app.ts") {
		t.Error("zero Loop: ordinary path matched")
	}
	l.RiskPaths = []string{"db/[bad]"}
	if !l.IsRiskPath("src/app.ts") {
		t.Error("uncompilable pattern: want every path treated as a risk path")
	}
}

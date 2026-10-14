package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orionsbelt-dev/chuck/internal/memory"
)

func TestReplayFake(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := memory.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(filepath.Join("..", "..", "internal", "testdata", "github", "issues_opened.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	seed := func(kind, delivery string, payload []byte, d memory.Decision) {
		ev, _, err := s.InsertEvent(ctx, memory.Event{Source: memory.SourceGitHub, Kind: kind, DeliveryID: delivery,
			Repo: "orionsbelt-dev/demos", Payload: payload, ReceivedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		d.EventID, d.CreatedAt = ev, now
		id, err := s.RecordDecision(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.RecordGrade(ctx, id, "up", "UOWNER0001", now); err != nil {
			t.Fatal(err)
		}
	}
	seed("issues.opened", "d1", payload, memory.Decision{Stage: memory.StageHaiku, Outcome: memory.OutcomeOK,
		Category: "new_issue", WouldDo: "comment_issue", Permission: "act_freely",
		Target: "https://github.com/orionsbelt-dev/demos/issues/7", PostKind: memory.PostGrade,
		RawOutput: `{"category":"new_issue","would_do":"comment_issue","target":"https://github.com/orionsbelt-dev/demos/issues/7","confidence":0.8,"rationale":"r"}`})
	seed("ping", "d2", []byte(`{}`), memory.Decision{Stage: memory.StageRule, Outcome: memory.OutcomeDropped,
		Rationale: "github_app_housekeeping"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := run(ctx, []string{"replay", "--db", path, "--fake"},
		env(map[string]string{"CHUCK_CONFIG_DIR": "../../config"}), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "👍 agreement: 100% (2/2)") {
		t.Fatalf("report:\n%s", out.String())
	}
}

func TestReplayUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"replay"}, env(map[string]string{"CHUCK_CONFIG_DIR": "../../config"}), &out, &errOut); code != 2 {
		t.Fatalf("no --db: exit %d, want 2", code)
	}
	db := filepath.Join(t.TempDir(), "state.db")
	s, err := memory.Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	errOut.Reset()
	if code := run(context.Background(), []string{"replay", "--db", db}, env(map[string]string{"CHUCK_CONFIG_DIR": "../../config"}), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "ANTHROPIC_API_KEY") {
		t.Fatalf("no key: exit %d, stderr %q", code, errOut.String())
	}
}

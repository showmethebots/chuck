package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/orionsbelt-dev/chuck/internal/memory"
)

func TestSummaryText(t *testing.T) {
	sum := memory.Summary{
		Since: t0.Add(-24 * time.Hour), Until: t0,
		Outcomes:   map[string]int{"ok": 80, "dropped_by_rule": 35, "triage_failed": 5},
		Categories: map[string]int{"noise": 50, "ci_failure": 4},
		SpendUSD:   0.21, GradedInWindow: 14, Graded: 63, Up: 58, AskFirstDown: 0, PendingGrades: 9,
		Rejected: 3,
	}
	got := SummaryText(sum, memory.Control{}, 5, 120<<20, 1<<30)
	for _, want := range []string{
		"chuck daily summary", "2026-10-13 12:00 UTC", "Status: running",
		"Decisions: 120 (dropped_by_rule 35, ok 80, triage_failed 5)",
		"Categories: ci_failure 4, noise 50",
		"Haiku: $0.21 of $5.00 a day",
		"Grades: 14 given, 9 waiting",
		"Exit progress: 63/100 graded · 92% 👍 · 0 👎 on ask_first",
		"Allowlist: 3 rejected, 0 violations",
		"Disk: 120 MB of 1024 MB (11%)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "over half full") {
		t.Error("disk warning below 50%")
	}
}

func TestSummaryTextPausedNoGradesFullDisk(t *testing.T) {
	ctl := memory.Control{Paused: true, Reason: "kill", By: "UMEMBER001", ChangedAt: t0}
	got := SummaryText(memory.Summary{Until: t0}, ctl, 5, 600<<20, 1<<30)
	for _, want := range []string{"Status: paused (kill by <@UMEMBER001>", "0/100 graded · no grades yet", "over half full"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
}

func TestBudgetText(t *testing.T) {
	if got := BudgetText(5.01, 5); !strings.Contains(got, "$5.01 of $5.00") || !strings.Contains(got, "00:00 UTC") {
		t.Fatalf("BudgetText = %q", got)
	}
}

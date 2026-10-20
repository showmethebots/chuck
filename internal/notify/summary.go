package notify

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/orionsbelt-dev/chuck/internal/memory"
)

// exitGraded is the exit criterion's minimum number of graded decisions.
const exitGraded = 100

func joinCounts(m map[string]int) (string, int) {
	keys := make([]string, 0, len(m))
	total := 0
	for k, n := range m {
		keys = append(keys, k)
		total += n
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", "), total
}

// SummaryText is the daily summary posted to the main channel.
func SummaryText(sum memory.Summary, ctl memory.Control, haikuBudget float64, diskUsed, diskTotal uint64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*chuck daily summary* (24h to %s)\n", sum.Until.UTC().Format("2006-01-02 15:04 UTC"))
	status := "running"
	if ctl.Paused {
		status = fmt.Sprintf("paused (%s by <@%s> since %s)", ctl.Reason, ctl.By, ctl.ChangedAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "Status: %s\n", status)
	outcomes, total := joinCounts(sum.Outcomes)
	fmt.Fprintf(&b, "Decisions: %d (%s)\n", total, outcomes)
	categories, _ := joinCounts(sum.Categories)
	fmt.Fprintf(&b, "Categories: %s\n", categories)
	fmt.Fprintf(&b, "Haiku: $%.2f of $%.2f a day\n", sum.SpendUSD, haikuBudget)
	fmt.Fprintf(&b, "Grades: %d given, %d waiting\n", sum.GradedInWindow, sum.PendingGrades)
	rate := "no grades yet"
	if sum.Graded > 0 {
		rate = fmt.Sprintf("%d%% 👍", sum.Up*100/sum.Graded)
	}
	fmt.Fprintf(&b, "Exit progress: %d/%d graded · %s · %d 👎 on ask_first\n", sum.Graded, exitGraded, rate, sum.AskFirstDown)
	fmt.Fprintf(&b, "Allowlist: %d rejected, %d violations\n", sum.Rejected, sum.Violations)
	if diskTotal > 0 {
		pct := diskUsed * 100 / diskTotal
		fmt.Fprintf(&b, "Disk: %d MB of %d MB (%d%%)", diskUsed>>20, diskTotal>>20, pct)
		if pct > 50 {
			b.WriteString(" — over half full; plan retention or a bigger volume")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// BudgetText is the notice posted when triage stops for the day's budget.
func BudgetText(spent, budget float64) string {
	return fmt.Sprintf("Haiku reached its daily budget ($%.2f of $%.2f). Triage resumes at 00:00 UTC; events keep queueing.", spent, budget)
}

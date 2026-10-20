package triage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/orionsbelt-dev/chuck/internal/config"
	"github.com/orionsbelt-dev/chuck/internal/memory"
)

// ReplayRow counts, for one category, how replay compares with grades.
type ReplayRow struct {
	Up, UpAgree       int // 👍 decisions, and how many replay reproduced
	Down, DownDiverge int // 👎 decisions, and how many replay changed
}

type ReplayReport struct {
	Items   int
	Errors  int // classifier calls that failed; those items are skipped
	Total   int // graded items passed to Replay
	Stopped bool
	MaxUSD  float64 // the cap in effect when Stopped, for the report line
	Rows    map[string]*ReplayRow
	CostUSD float64
}

// Replay re-runs graded events through Decide with the current rules and
// prompt. A 👍 decision should come out the same; a 👎 one should not.
//
// maxUSD caps how much a real Haiku run may spend: once accumulated
// CostUSD reaches maxUSD, Replay stops before the next item and sets
// Stopped. maxUSD <= 0 means no cap, for tests and --fake, which cost
// nothing; chuck replay rejects it for a real Haiku run. AGENTS.md requires
// every model tier to have a daily budget; this is replay's.
func Replay(ctx context.Context, items []memory.Graded, c Classifier, p config.Policy, maxUSD float64) ReplayReport {
	r := ReplayReport{Rows: map[string]*ReplayRow{}, Total: len(items), MaxUSD: maxUSD}
	for _, it := range items {
		if maxUSD > 0 && r.CostUSD >= maxUSD {
			r.Stopped = true
			break
		}
		v, err := Decide(ctx, it.Event.Kind, it.Event.Repo, it.Event.Payload, c, p)
		if err != nil {
			r.Errors++
			continue
		}
		r.Items++
		r.CostUSD += v.CostUSD
		key := it.Decision.Category
		if it.Decision.Outcome != memory.OutcomeOK {
			key = it.Decision.Outcome
		}
		row := r.Rows[key]
		if row == nil {
			row = &ReplayRow{}
			r.Rows[key] = row
		}
		same := sameVerdict(it.Decision, v)
		switch it.Grade {
		case "up":
			row.Up++
			if same {
				row.UpAgree++
			}
		case "down":
			row.Down++
			if !same {
				row.DownDiverge++
			}
		}
	}
	return r
}

func sameVerdict(d memory.Decision, v Verdict) bool {
	if d.Outcome != v.Outcome {
		return false
	}
	switch d.Outcome {
	case memory.OutcomeOK:
		return d.Category == v.Category && d.WouldDo == v.WouldDo
	case memory.OutcomeDropped:
		return d.Rationale == v.Rationale
	}
	return true
}

func pct(n, d int) string {
	if d == 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", n*100/d)
}

func (r ReplayReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "replay: %d replayed, %d classifier errors, cost $%.4f\n", r.Items, r.Errors, r.CostUSD)
	if r.Stopped {
		fmt.Fprintf(&b, "stopped early: reached the --max-usd cap of $%.2f; %d of %d graded decisions not replayed\n",
			r.MaxUSD, r.Total-r.Items-r.Errors, r.Total)
	}
	fmt.Fprint(&b, "\n")
	keys := make([]string, 0, len(r.Rows))
	for k := range r.Rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "category\tup\tagree\tdown\tdiverge")
	var total ReplayRow
	for _, k := range keys {
		row := r.Rows[k]
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%s\n", k, row.Up, pct(row.UpAgree, row.Up), row.Down, pct(row.DownDiverge, row.Down))
		total.Up += row.Up
		total.UpAgree += row.UpAgree
		total.Down += row.Down
		total.DownDiverge += row.DownDiverge
	}
	_ = tw.Flush()
	fmt.Fprintf(&b, "\n👍 agreement: %s (%d/%d)\n👎 divergence: %s (%d/%d)\n",
		pct(total.UpAgree, total.Up), total.UpAgree, total.Up,
		pct(total.DownDiverge, total.Down), total.DownDiverge, total.Down)
	return b.String()
}

// EchoClassifier returns each event's stored model output instead of
// calling a model, so CI can exercise replay without an API key. It is keyed
// by Input, so when two graded events build the same input, the last one's
// output is replayed for both. That is harmless for CI.
type EchoClassifier map[Input]string

func NewEchoClassifier(items []memory.Graded) EchoClassifier {
	e := EchoClassifier{}
	for _, it := range items {
		if in, err := BuildInput(it.Event.Kind, it.Event.Repo, it.Event.Payload); err == nil {
			e[in] = it.Decision.RawOutput
		}
	}
	return e
}

func (e EchoClassifier) Classify(_ context.Context, in Input) (string, Usage, error) {
	raw, ok := e[in]
	if !ok {
		return "", Usage{}, fmt.Errorf("no stored output for %s %s", in.Repo, in.Kind)
	}
	return raw, Usage{Model: HaikuModel}, nil
}

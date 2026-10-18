package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/orionsbelt-dev/chuck/internal/config"
	"github.com/orionsbelt-dev/chuck/internal/memory"
	"github.com/orionsbelt-dev/chuck/internal/triage"
)

// runReplay re-runs graded events through the current rules and prompt and
// prints agreement with the grades. It opens the database read-only.
func runReplay(ctx context.Context, args []string, getenv func(string) string, configDir string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", "", "SQLite file to replay, opened read-only (required)")
	since := fs.String("since", "", "only decisions on or after this UTC date, YYYY-MM-DD")
	fake := fs.Bool("fake", false, "reuse stored model output instead of calling Haiku")
	maxUSD := fs.Float64("max-usd", 1.00, "stop once Haiku spend reaches this many dollars; must be above 0 unless --fake")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *db == "" {
		_, _ = fmt.Fprintln(stderr, "replay: --db is required")
		return 2
	}
	// Replay's Haiku spend always has a cap: AGENTS.md allows no
	// "unlimited" budget. --fake costs nothing, so it needs none.
	if !*fake && *maxUSD <= 0 {
		_, _ = fmt.Fprintln(stderr, "replay: --max-usd must be above 0")
		return 2
	}
	cfg, err := config.Load(configDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "replay: config: %v\n", err)
		return 1
	}
	var from time.Time
	if *since != "" {
		if from, err = time.Parse(time.DateOnly, *since); err != nil {
			_, _ = fmt.Fprintf(stderr, "replay: --since: %v\n", err)
			return 2
		}
	}
	store, err := memory.OpenReadOnly(ctx, *db)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "replay: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	items, err := store.GradedEvents(ctx, from)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "replay: %v\n", err)
		return 1
	}

	var c triage.Classifier
	spendCap := *maxUSD
	if *fake {
		c = triage.NewEchoClassifier(items)
		spendCap = 0 // stored output costs nothing to replay
	} else {
		key := getenv("ANTHROPIC_API_KEY")
		if key == "" {
			_, _ = fmt.Fprintln(stderr, "replay: set ANTHROPIC_API_KEY, or pass --fake")
			return 1
		}
		c = triage.NewHaiku(cfg.Policy, option.WithAPIKey(key))
	}
	_, _ = fmt.Fprint(stdout, triage.Replay(ctx, items, c, cfg.Policy, spendCap).String())
	return 0
}

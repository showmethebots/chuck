// Command chuck is the autonomous project manager daemon.
//
//	chuck run       start the daemon (default)
//	chuck validate  load and validate config, then exit
//	chuck replay    re-run graded events and report agreement (--db, --since, --fake, --max-usd)
//
// Environment:
//
//	CHUCK_CONFIG_DIR           directory holding routes.yaml and policy.yaml (default ./config)
//	CHUCK_DB_PATH              SQLite state file (default ./data/chuck.db)
//	CHUCK_HEARTBEAT            heartbeat interval as a Go duration (default 1m)
//	PORT                       HTTP listen port (default 8080)
//	GITHUB_APP_WEBHOOK_SECRET  turns on POST /webhooks/github; deliveries must be signed with it
//	ANTHROPIC_API_KEY          turns on Haiku triage
//	SLACK_BOT_TOKEN            Slack bot token (xoxb-); with the next three, turns on Slack
//	SLACK_APP_TOKEN            Slack app-level token (xapp-) for Socket Mode
//	CHUCK_SLACK_MAIN_CHANNEL   channel ID for approvals, the daily summary, and notices
//	CHUCK_SLACK_SHADOW_CHANNEL channel ID for grade posts
//	CHUCK_DAILY_AT             daily summary and grade sample time, HH:MM UTC (default 13:00)
//	CHUCK_GRADE_SAMPLE         quiet decisions sampled for grading each day (default 5)
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/orionsbelt-dev/chuck/internal/config"
	"github.com/orionsbelt-dev/chuck/internal/daemon"
	"github.com/orionsbelt-dev/chuck/internal/events"
	"github.com/orionsbelt-dev/chuck/internal/memory"
	"github.com/orionsbelt-dev/chuck/internal/notify"
	"github.com/orionsbelt-dev/chuck/internal/triage"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd := "run"
	if len(args) > 0 {
		cmd = args[0]
	}
	log := slog.New(slog.NewJSONHandler(stdout, nil)).With("app", "chuck")
	configDir := envOr(getenv, "CHUCK_CONFIG_DIR", "config")

	switch cmd {
	case "validate":
		if _, err := config.Load(configDir); err != nil {
			_, _ = fmt.Fprintf(stderr, "invalid config in %s:\n%v\n", configDir, err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "config in %s is valid\n", configDir)
		return 0
	case "run":
		if err := runDaemon(ctx, log, getenv, configDir); err != nil {
			log.Error("daemon failed", "err", err.Error())
			return 1
		}
		return 0
	case "replay":
		return runReplay(ctx, args[1:], getenv, configDir, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q; want run, validate, or replay\n", cmd)
		return 2
	}
}

func runDaemon(ctx context.Context, log *slog.Logger, getenv func(string) string, configDir string) error {
	cfg, err := config.Load(configDir)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log.Info("config loaded", "dir", configDir, "routes", len(cfg.Routes.Routes))

	interval, err := time.ParseDuration(envOr(getenv, "CHUCK_HEARTBEAT", "1m"))
	if err != nil {
		return fmt.Errorf("CHUCK_HEARTBEAT: %w", err)
	}

	in, err := loadIntegrations(getenv)
	if err != nil {
		return err
	}
	if in.Slack != nil && cfg.Slack == nil {
		return fmt.Errorf("slack is on but %s is missing from %s", config.SlackFile, configDir)
	}

	dbPath := envOr(getenv, "CHUCK_DB_PATH", "data/chuck.db")
	store, err := memory.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Error("close state db", "err", err.Error())
		}
	}()
	boots, err := store.RecordBoot(ctx, version)
	if err != nil {
		return fmt.Errorf("record boot: %w", err)
	}

	ln, err := net.Listen("tcp", ":"+envOr(getenv, "PORT", "8080"))
	if err != nil {
		return err
	}
	d := &daemon.Daemon{
		Log: log, Interval: interval, Version: version, BootCount: boots,
		Integrations: in.names(),
		QueueDepth:   store.QueueDepth,
	}
	if in.WebhookSecret != "" {
		d.Webhook = &events.GitHub{Secret: []byte(in.WebhookSecret), Store: store, Log: log}
	}

	haikuBudget := cfg.Policy.Budgets.DailyUSD[config.TierHaiku]
	var sc *notify.Slack
	if in.Slack != nil {
		sc = notify.NewSlack(in.Slack.BotToken, in.Slack.AppToken, log)
	}

	if in.AnthropicKey != "" {
		tr := &daemon.Triage{
			Store:      store,
			Classifier: triage.NewHaiku(cfg.Policy, option.WithAPIKey(in.AnthropicKey)),
			Policy:     cfg.Policy,
			Log:        log,
		}
		if sc != nil {
			tr.OnBudgetHit = func(ctx context.Context, spent, budget float64) {
				text := notify.BudgetText(spent, budget)
				ts, err := sc.Post(ctx, in.Slack.MainChannel, text, nil)
				if err != nil {
					log.Error("post budget notice", "err", err.Error())
					return
				}
				if err := store.RecordMessage(ctx, memory.Message{Channel: in.Slack.MainChannel, TS: ts,
					Direction: "out", Kind: "post", Text: text, Allowed: true, CreatedAt: time.Now()}); err != nil {
					log.Error("record budget notice", "err", err.Error())
				}
			}
		}
		d.Background = append(d.Background, tr.Run)
	}

	jobs := &daemon.DailyJobs{
		Store: store, Sample: in.GradeSample, HaikuBudget: haikuBudget, Log: log,
		Disk: func() (uint64, uint64, error) { return daemon.DiskUsage(filepath.Dir(dbPath)) },
	}
	if sc != nil {
		ob := &notify.Outbox{Store: store, Poster: sc, MainChannel: in.Slack.MainChannel,
			ShadowChannel: in.Slack.ShadowChannel, Log: log}
		ix := &notify.Interactions{Store: store, Allow: cfg.Slack, Poster: sc, MainChannel: in.Slack.MainChannel,
			HaikuBudget: haikuBudget, Version: version, Log: log}
		jobs.Poster, jobs.MainChannel = sc, in.Slack.MainChannel
		d.Background = append(d.Background,
			func(ctx context.Context) { ob.Run(ctx, 5*time.Second) },
			func(ctx context.Context) { sc.Run(ctx, ix) },
		)
	}
	d.Background = append(d.Background, func(ctx context.Context) {
		events.RunDaily(ctx, in.Daily, time.Now, func(ctx context.Context, at time.Time) {
			if err := jobs.Run(ctx, at); err != nil {
				log.Error("daily jobs", "err", err.Error())
			}
		})
	})
	return d.Run(ctx, ln)
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// integrations is what the environment switches on. Each turns on
// independently so Phase 1 can deploy in slices.
type integrations struct {
	WebhookSecret string
	AnthropicKey  string
	Daily         events.Daily
	GradeSample   int
	Slack         *slackEnv // nil when Slack is off
}

type slackEnv struct {
	BotToken, AppToken, MainChannel, ShadowChannel string
}

var slackVars = []string{"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "CHUCK_SLACK_MAIN_CHANNEL", "CHUCK_SLACK_SHADOW_CHANNEL"}

func loadIntegrations(getenv func(string) string) (integrations, error) {
	daily, err := events.ParseDaily(envOr(getenv, "CHUCK_DAILY_AT", "13:00"))
	if err != nil {
		return integrations{}, fmt.Errorf("CHUCK_DAILY_AT: %w", err)
	}
	sample, err := strconv.Atoi(envOr(getenv, "CHUCK_GRADE_SAMPLE", "5"))
	if err != nil || sample < 0 {
		return integrations{}, fmt.Errorf("CHUCK_GRADE_SAMPLE: want a whole number >= 0, got %q", getenv("CHUCK_GRADE_SAMPLE"))
	}
	in := integrations{
		WebhookSecret: getenv("GITHUB_APP_WEBHOOK_SECRET"),
		AnthropicKey:  getenv("ANTHROPIC_API_KEY"),
		Daily:         daily,
		GradeSample:   sample,
	}
	var set, missing []string
	for _, k := range slackVars {
		if getenv(k) == "" {
			missing = append(missing, k)
		} else {
			set = append(set, k)
		}
	}
	if len(set) > 0 && len(missing) > 0 {
		return in, fmt.Errorf("partial Slack setup: %s set but %s missing",
			strings.Join(set, ", "), strings.Join(missing, ", "))
	}
	if len(set) > 0 {
		in.Slack = &slackEnv{
			BotToken: getenv("SLACK_BOT_TOKEN"), AppToken: getenv("SLACK_APP_TOKEN"),
			MainChannel: getenv("CHUCK_SLACK_MAIN_CHANNEL"), ShadowChannel: getenv("CHUCK_SLACK_SHADOW_CHANNEL"),
		}
	}
	return in, nil
}

// names lists the integrations that are on, for logs and /healthz.
func (in integrations) names() []string {
	n := []string{}
	if in.WebhookSecret != "" {
		n = append(n, "github_webhook")
	}
	if in.AnthropicKey != "" {
		n = append(n, "triage")
	}
	if in.Slack != nil {
		n = append(n, "slack")
	}
	return n
}

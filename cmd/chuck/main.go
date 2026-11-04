// Command chuck runs the agent delivery loop. The design is
// docs/superpowers/specs/2026-11-03-delivery-loop-design.md; until the loop
// packages land, chuck serves /healthz and stores signed GitHub deliveries
// without acting on them.
//
//	chuck run       start the server (default)
//	chuck validate  check the config directory, then exit
//
// Environment, read from the process and then from ./.env if it exists. A
// variable already set in the process wins over the same one in .env.
//
//	CHUCK_CONFIG_DIR           config directory (default ./config)
//	CHUCK_DB_PATH              SQLite state file (default ./data/chuck.db)
//	CHUCK_HEARTBEAT            heartbeat interval as a Go duration (default 1m)
//	PORT                       HTTP listen port (default 8080)
//	GITHUB_APP_WEBHOOK_SECRET  turns on POST /webhooks/github; deliveries must be signed with it
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/orionsbelt-dev/chuck/internal/events"
	"github.com/orionsbelt-dev/chuck/internal/memory"
	"github.com/orionsbelt-dev/chuck/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := loadDotenv(".env"); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "load .env: %v\n", err)
		os.Exit(1)
	}
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// loadDotenv copies variables from the file at path into the process
// environment, leaving any already set alone. A missing file is not an error:
// production has none and takes its environment from Fly secrets.
func loadDotenv(path string) error {
	err := godotenv.Load(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
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
		if err := checkConfigDir(configDir); err != nil {
			_, _ = fmt.Fprintf(stderr, "invalid config in %s:\n%v\n", configDir, err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "config in %s is valid\n", configDir)
		return 0
	case "run":
		if err := runServer(ctx, log, getenv, configDir); err != nil {
			log.Error("server failed", "err", err.Error())
			return 1
		}
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q; want run or validate\n", cmd)
		return 2
	}
}

// retiredConfig names files from the project-manager framing. A config
// directory still carrying one was built for a chuck that no longer exists,
// so refusing it beats silently ignoring rules someone expects to hold.
var retiredConfig = []string{"routes.yaml", "policy.yaml", "slack.yaml"}

// checkConfigDir is all the validation there is until config/chuck.yaml
// lands: the directory exists and holds none of the retired files.
func checkConfigDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	var errs []error
	for _, name := range retiredConfig {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			errs = append(errs, fmt.Errorf("%s is retired with the project-manager framing; delete it", name))
		}
	}
	return errors.Join(errs...)
}

func runServer(ctx context.Context, log *slog.Logger, getenv func(string) string, configDir string) error {
	if err := checkConfigDir(configDir); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	interval, err := time.ParseDuration(envOr(getenv, "CHUCK_HEARTBEAT", "1m"))
	if err != nil {
		return fmt.Errorf("CHUCK_HEARTBEAT: %w", err)
	}
	secret := getenv("GITHUB_APP_WEBHOOK_SECRET")

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
	srv := &web.Server{
		Log: log, Interval: interval, Version: version, BootCount: boots,
		Integrations: integrations(secret),
	}
	if secret != "" {
		srv.Webhook = &events.GitHub{Secret: []byte(secret), Store: store, Log: log}
	}
	return srv.Run(ctx, ln)
}

// integrations names what the environment switched on, for logs and
// /healthz.
func integrations(webhookSecret string) []string {
	n := []string{}
	if webhookSecret != "" {
		n = append(n, "github_webhook")
	}
	return n
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

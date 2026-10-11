// Command chuck is the autonomous project manager daemon.
//
//	chuck run       start the daemon (default)
//	chuck validate  load and validate config, then exit
//
// Environment:
//
//	CHUCK_CONFIG_DIR  directory holding routes.yaml and policy.yaml (default ./config)
//	CHUCK_DB_PATH     SQLite state file (default ./data/chuck.db)
//	CHUCK_HEARTBEAT   heartbeat interval as a Go duration (default 1m)
//	PORT              HTTP listen port (default 8080)
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/orionsbelt-dev/chuck/internal/config"
	"github.com/orionsbelt-dev/chuck/internal/daemon"
	"github.com/orionsbelt-dev/chuck/internal/memory"
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
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q; want run or validate\n", cmd)
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
	d := &daemon.Daemon{Log: log, Interval: interval, Version: version, BootCount: boots}
	return d.Run(ctx, ln)
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// Command chuck runs the agent delivery loop. The design is
// docs/superpowers/specs/2026-11-03-delivery-loop-design.md. chuck serves
// /healthz and stores signed GitHub deliveries; the loop itself (the
// executor and the tick in internal/engine) turns on only when every
// ${VAR} config/chuck.yaml names is set. Until then deliveries are stored
// as handled and nothing acts on them.
//
//	chuck run       start the server (default)
//	chuck validate [loop.json ...]  check config/chuck.yaml and any loop.json files given, then exit
//
// Environment, read from the process and then from ./.env if it exists. A
// variable already set in the process wins over the same one in .env.
//
//	CHUCK_CONFIG_DIR           directory holding chuck.yaml (default ./config)
//	CHUCK_DB_PATH              SQLite state file (default ./data/chuck.db)
//	CHUCK_HEARTBEAT            heartbeat interval as a Go duration (default 1m)
//	PORT                       HTTP listen port (default 8080)
//	GITHUB_APP_WEBHOOK_SECRET  turns on POST /webhooks/github; deliveries must be signed with it
//	CHUCK_WEBHOOK_ALLOW_ANY_IP 1 accepts deliveries from any IP, for local development through a
//	                           tunnel; otherwise Fly-Client-IP must be in GitHub's hook ranges
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/showmethebots/chuck/internal/config"
	"github.com/showmethebots/chuck/internal/engine"
	"github.com/showmethebots/chuck/internal/github"
	"github.com/showmethebots/chuck/internal/memory"
	"github.com/showmethebots/chuck/internal/web"
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
		return runValidate(configDir, args[1:], stdout, stderr)
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

// checkConfigDir checks dir/chuck.yaml without resolving its ${VAR}
// references (CI has none of the secrets) and refuses a directory that
// still holds a retired file.
func checkConfigDir(dir string) (*config.Chuck, error) {
	var errs []error
	for _, name := range retiredConfig {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			errs = append(errs, fmt.Errorf("%s is retired with the project-manager framing; delete it", name))
		}
	}
	c, err := config.LoadChuck(dir)
	if err != nil {
		errs = append(errs, err)
	}
	return c, errors.Join(errs...)
}

// runValidate checks the config directory and each loop.json named, and
// reports every file's problems before exiting.
func runValidate(configDir string, loopFiles []string, stdout, stderr io.Writer) int {
	code := 0
	if _, err := checkConfigDir(configDir); err != nil {
		_, _ = fmt.Fprintf(stderr, "invalid config in %s:\n%v\n", configDir, err)
		code = 1
	} else {
		_, _ = fmt.Fprintf(stdout, "config in %s is valid\n", configDir)
	}
	for _, f := range loopFiles {
		b, err := os.ReadFile(f)
		if err == nil {
			_, err = config.ParseLoop(b)
		}
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "invalid loop config %s:\n%v\n", f, err)
			code = 1
			continue
		}
		_, _ = fmt.Fprintf(stdout, "loop config %s is valid\n", f)
	}
	return code
}

func runServer(ctx context.Context, log *slog.Logger, getenv func(string) string, configDir string) error {
	// Structure only: the secrets chuck.yaml names are resolved when the
	// loop starts using them, so a deploy before they are set still serves
	// /healthz and the webhook.
	chuckCfg, err := checkConfigDir(configDir)
	if err != nil {
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
	eng, err := newEngine(chuckCfg, getenv, store, log)
	if err != nil {
		return err
	}
	if eng != nil {
		srv.Background = append(srv.Background, eng.Run)
		srv.Integrations = append(srv.Integrations, "loop")
	}
	if secret != "" {
		rcv := &github.Receiver{Secret: []byte(secret), Store: store, Log: log}
		if eng != nil {
			rcv.Notify = eng.Notify
		}
		if getenv("CHUCK_WEBHOOK_ALLOW_ANY_IP") == "1" {
			rcv.AllowAnyIP = true
			log.Warn("CHUCK_WEBHOOK_ALLOW_ANY_IP=1: webhooks are accepted from ANY IP address, not only GitHub's. " +
				"Local development only; never set this in production.")
		} else {
			rcv.Allow = github.NewAllowlist()
			go rcv.Allow.Run(ctx, log, &http.Client{Timeout: 30 * time.Second}, github.MetaURL, 24*time.Hour)
		}
		srv.Webhook = rcv
	}
	return srv.Run(ctx, ln)
}

// newEngine builds the loop from config/chuck.yaml once every reference in
// it resolves, OAuth ones included, so the loop never runs half
// configured. With any unset or malformed it returns nil and logs which
// (never a value): /healthz and the webhook keep serving either way.
func newEngine(c *config.Chuck, getenv func(string) string, store *memory.Store, log *slog.Logger) (*engine.Engine, error) {
	r, err := c.Resolve(getenv)
	if err != nil {
		log.Warn("loop off: config/chuck.yaml is not fully resolved", "why", err.Error())
		return nil, nil
	}
	src := &engine.AppSource{ByOwner: map[string]*github.App{}}
	for _, a := range r.Apps {
		id, err := strconv.ParseInt(a.AppID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("app %s: app_id: %w", a.Owner, err)
		}
		key, err := github.ParsePrivateKey([]byte(a.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("app %s: %w", a.Owner, err)
		}
		src.ByOwner[a.Owner] = &github.App{ID: id, Key: key}
		src.Order = append(src.Order, a.Owner)
	}
	humans := make([]int64, 0, len(r.Humans))
	for _, h := range r.Humans {
		humans = append(humans, h.ID)
	}
	log.Info("loop on", "apps", src.Order, "tick", r.Tick.Std().String())
	return &engine.Engine{
		Store: store, GitHub: src, Humans: humans, Log: log,
		Tick: r.Tick.Std(), Retention: r.WebhookRetention.Std(),
	}, nil
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

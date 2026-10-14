// Package daemon is the long-running process: it serves /healthz and the
// GitHub webhook, logs a heartbeat, and runs background jobs (the triage
// loop, Slack, cron) for its lifetime.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

type Daemon struct {
	Log       *slog.Logger
	Interval  time.Duration
	Version   string
	BootCount int64 // boots recorded in the state DB, this one included

	// Webhook receives GitHub deliveries at POST /webhooks/github. Nil leaves
	// the route unmounted.
	Webhook http.Handler
	// Integrations names what the environment switched on, for /healthz.
	Integrations []string
	// QueueDepth, when set, is reported by /healthz.
	QueueDepth func(context.Context) (int, error)
	// Background jobs run for the daemon's lifetime and must return once
	// their context ends.
	Background []func(context.Context)

	started time.Time
}

// Run serves HTTP on ln, runs Background jobs, and logs a heartbeat every
// Interval until ctx is cancelled. It returns nil on a clean shutdown, after
// every background job has returned.
func (d *Daemon) Run(ctx context.Context, ln net.Listener) error {
	d.started = time.Now()
	srv := d.server()

	jobCtx, stopJobs := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer stopJobs()
	for _, job := range d.Background {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job(jobCtx)
		}()
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	d.Log.Info("daemon started", "addr", ln.Addr().String(), "version", d.Version,
		"boot_count", d.BootCount, "integrations", d.integrations())
	tick := time.NewTicker(d.Interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := srv.Shutdown(shutdownCtx)
			d.Log.Info("daemon stopped")
			return err
		case err := <-serveErr:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-tick.C:
			d.Log.Info("heartbeat",
				"boot_count", d.BootCount,
				"uptime_s", int64(time.Since(d.started).Seconds()))
		}
	}
}

func (d *Daemon) integrations() []string {
	if d.Integrations == nil {
		return []string{}
	}
	return d.Integrations
}

// server bounds how long a client may take to send a request. ReadTimeout
// covers the body too, so a slow sender cannot hold a webhook body buffer
// open indefinitely; GitHub itself gives up on a delivery after 10 s.
func (d *Daemon) server() *http.Server {
	return &http.Server{
		Handler:           d.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
	}
}

// Handler serves the daemon's HTTP endpoints.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"status":       "ok",
			"version":      d.Version,
			"boot_count":   d.BootCount,
			"integrations": d.integrations(),
		}
		if d.QueueDepth != nil {
			if n, err := d.QueueDepth(r.Context()); err == nil {
				body["queue_depth"] = n
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	if d.Webhook != nil {
		mux.Handle("POST /webhooks/github", d.Webhook)
	}
	return mux
}

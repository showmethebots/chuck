// Package web is chuck's HTTP surface and process lifetime: it serves
// /healthz and the GitHub webhook, logs a heartbeat, and runs background
// jobs for its lifetime. The dashboard and worker API mount here as they
// land.
package web

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

// Server is the long-running process.
type Server struct {
	Log       *slog.Logger
	Interval  time.Duration
	Version   string
	BootCount int64 // boots recorded in the state DB, this one included

	// Webhook receives GitHub deliveries at POST /webhooks/github. Nil leaves
	// the route unmounted.
	Webhook http.Handler
	// Integrations names what the environment switched on, for /healthz.
	Integrations []string
	// Background jobs run for the daemon's lifetime and must return once
	// their context ends.
	Background []func(context.Context)

	started time.Time
}

// Run serves HTTP on ln, runs Background jobs, and logs a heartbeat every
// Interval until ctx is cancelled. It returns nil on a clean shutdown, after
// every background job has returned.
func (d *Server) Run(ctx context.Context, ln net.Listener) error {
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

func (d *Server) integrations() []string {
	if d.Integrations == nil {
		return []string{}
	}
	return d.Integrations
}

// server bounds how long a client may take to send a request. ReadTimeout
// covers the body for every route except the GitHub webhook, which bounds
// the wait for a read slot and replaces the read deadline with its own
// 10 s once it holds one (see events.GitHub).
func (d *Server) server() *http.Server {
	return &http.Server{
		Handler:           d.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
	}
}

// Handler serves chuck's HTTP endpoints.
func (d *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"status":       "ok",
			"version":      d.Version,
			"boot_count":   d.BootCount,
			"integrations": d.integrations(),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	if d.Webhook != nil {
		mux.Handle("POST /webhooks/github", d.Webhook)
	}
	return mux
}

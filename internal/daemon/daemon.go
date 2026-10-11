// Package daemon is the long-running process: in Phase 0 it serves
// /healthz and logs a heartbeat. The event loop and scheduler land here in
// Phase 1.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

type Daemon struct {
	Log       *slog.Logger
	Interval  time.Duration
	Version   string
	BootCount int64 // boots recorded in the state DB, this one included

	started time.Time
}

// Run serves HTTP on ln and logs a heartbeat every Interval until ctx is
// cancelled. It returns nil on a clean shutdown.
func (d *Daemon) Run(ctx context.Context, ln net.Listener) error {
	d.started = time.Now()
	srv := &http.Server{Handler: d.Handler(), ReadHeaderTimeout: 10 * time.Second}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	d.Log.Info("daemon started", "addr", ln.Addr().String(), "version", d.Version, "boot_count", d.BootCount)
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

// Handler serves the daemon's HTTP endpoints.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":     "ok",
			"version":    d.Version,
			"boot_count": d.BootCount,
		})
	})
	return mux
}

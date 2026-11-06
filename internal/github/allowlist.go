package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// builtinHooks is GitHub's `hooks` list from https://api.github.com/meta,
// used only until the first fetch succeeds. A stale entry here costs at
// most a few deliveries between boot and that fetch.
var builtinHooks = []string{
	"192.30.252.0/22",
	"185.199.108.0/22",
	"140.82.112.0/20",
	"143.55.64.0/20",
	"2a0a:a440::/29",
	"2606:50c0::/32",
}

// Allowlist is the set of ranges GitHub sends webhooks from. It is safe
// for concurrent use; a refresh replaces the whole list at once.
type Allowlist struct {
	mu       sync.RWMutex
	prefixes []netip.Prefix
	builtin  bool
}

func builtinAllowlist() *Allowlist {
	a := &Allowlist{builtin: true}
	for _, s := range builtinHooks {
		a.prefixes = append(a.prefixes, netip.MustParsePrefix(s))
	}
	return a
}

// NewAllowlist starts from the built-in list. Call Refresh, or Run, to
// replace it with GitHub's current one.
func NewAllowlist() *Allowlist { return builtinAllowlist() }

// Contains reports whether ip is inside one of the ranges. An IPv4 address
// written as IPv4-mapped IPv6 is matched as IPv4.
func (a *Allowlist) Contains(ip netip.Addr) bool {
	ip = ip.Unmap()
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, p := range a.prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// Builtin reports whether the list is still the built-in one.
func (a *Allowlist) Builtin() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.builtin
}

// Refresh fetches GitHub's meta endpoint and replaces the list. On any
// failure, including an empty or malformed list, the current list stays:
// a failed refresh never widens or empties it.
func (a *Allowlist) Refresh(ctx context.Context, client *http.Client, metaURL string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("github meta: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github meta: %s", resp.Status)
	}
	var meta struct {
		Hooks []string `json:"hooks"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4<<20)).Decode(&meta); err != nil {
		return fmt.Errorf("github meta: %w", err)
	}
	if len(meta.Hooks) == 0 {
		return fmt.Errorf("github meta: no hooks ranges")
	}
	prefixes := make([]netip.Prefix, 0, len(meta.Hooks))
	for _, s := range meta.Hooks {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return fmt.Errorf("github meta: hooks range %q: %w", s, err)
		}
		// A range this wide is not GitHub's; refuse the whole list rather
		// than let a bad response open the endpoint.
		if (p.Addr().Is4() && p.Bits() < 8) || (p.Addr().Is6() && p.Bits() < 16) {
			return fmt.Errorf("github meta: hooks range %s is too wide", p)
		}
		prefixes = append(prefixes, p.Masked())
	}
	a.mu.Lock()
	a.prefixes, a.builtin = prefixes, false
	a.mu.Unlock()
	return nil
}

// MetaURL is where GitHub publishes its ranges.
const MetaURL = "https://api.github.com/meta"

// Run refreshes now and then every interval until ctx ends, logging a
// failure at Error and keeping the last good list.
func (a *Allowlist) Run(ctx context.Context, log *slog.Logger, client *http.Client, metaURL string, interval time.Duration) {
	refresh := func() {
		if err := a.Refresh(ctx, client, metaURL); err != nil {
			log.Error("github hook ranges: refresh failed; keeping the current list",
				"builtin", a.Builtin(), "err", err.Error())
			return
		}
		log.Info("github hook ranges refreshed")
	}
	refresh()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refresh()
		}
	}
}

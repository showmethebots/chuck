package github

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
)

// hooksFile is GitHub's `hooks` list from https://api.github.com/meta,
// used only until the first fetch succeeds. A weekly workflow
// (hook-ranges.yml) pushes an update to a branch and opens an issue when
// GitHub's list changes; a person opens the PR,
// so the list chuck boots with is reviewed, not fetched.
//
//go:embed hooks.txt
var hooksFile string

// builtinHooks is hooksFile without comments and blank lines.
var builtinHooks = func() []string {
	var out []string
	for _, l := range strings.Split(hooksFile, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}()

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

// Diff is what a refresh changed, as ranges added and removed.
type Diff struct {
	Added, Removed []string
}

// Changed reports whether the refresh changed anything.
func (d Diff) Changed() bool { return len(d.Added)+len(d.Removed) > 0 }

func diff(old, new []netip.Prefix) Diff {
	var d Diff
	for _, p := range new {
		if !slices.Contains(old, p) {
			d.Added = append(d.Added, p.String())
		}
	}
	for _, p := range old {
		if !slices.Contains(new, p) {
			d.Removed = append(d.Removed, p.String())
		}
	}
	return d
}

// Refresh fetches GitHub's meta endpoint, replaces the list, and returns
// what changed. On any failure, including an empty or malformed list, the
// current list stays: a failed refresh never widens or empties it.
func (a *Allowlist) Refresh(ctx context.Context, client *http.Client, metaURL string) (Diff, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	if err != nil {
		return Diff{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return Diff{}, fmt.Errorf("github meta: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Diff{}, fmt.Errorf("github meta: %s", resp.Status)
	}
	var meta struct {
		Hooks []string `json:"hooks"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4<<20)).Decode(&meta); err != nil {
		return Diff{}, fmt.Errorf("github meta: %w", err)
	}
	if len(meta.Hooks) == 0 {
		return Diff{}, fmt.Errorf("github meta: no hooks ranges")
	}
	prefixes := make([]netip.Prefix, 0, len(meta.Hooks))
	for _, s := range meta.Hooks {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return Diff{}, fmt.Errorf("github meta: hooks range %q: %w", s, err)
		}
		// A range this wide is not GitHub's; refuse the whole list rather
		// than let a bad response open the endpoint.
		if (p.Addr().Is4() && p.Bits() < 8) || (p.Addr().Is6() && p.Bits() < 16) {
			return Diff{}, fmt.Errorf("github meta: hooks range %s is too wide", p)
		}
		prefixes = append(prefixes, p.Masked())
	}
	a.mu.Lock()
	d := diff(a.prefixes, prefixes)
	a.prefixes, a.builtin = prefixes, false
	a.mu.Unlock()
	return d, nil
}

// MetaURL is where GitHub publishes its ranges.
const MetaURL = "https://api.github.com/meta"

// Run refreshes now and then every interval until ctx ends. A failure is
// logged at Error and keeps the last good list. A refresh that changes the
// list is logged at Warn with every range added and removed, so no change
// to who may send webhooks goes unseen; the first one after boot is a
// change from hooks.txt, which the weekly workflow will also propose.
func (a *Allowlist) Run(ctx context.Context, log *slog.Logger, client *http.Client, metaURL string, interval time.Duration) {
	refresh := func() {
		wasBuiltin := a.Builtin()
		d, err := a.Refresh(ctx, client, metaURL)
		switch {
		case err != nil:
			log.Error("github hook ranges: refresh failed; keeping the current list",
				"builtin", a.Builtin(), "err", err.Error())
		case d.Changed():
			log.Warn("github hook ranges changed", "from_builtin", wasBuiltin,
				"added", strings.Join(d.Added, ","), "removed", strings.Join(d.Removed, ","))
		default:
			log.Info("github hook ranges refreshed; unchanged", "from_builtin", wasBuiltin)
		}
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

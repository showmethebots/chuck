package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// FlyMachinesURL is Fly's Machines API.
const FlyMachinesURL = "https://api.machines.dev"

// Fly launches workers as Machines in their own Fly app (chuck-workers),
// so a worker never shares chuck's volume or secrets and survives chuck's
// deploys.
type Fly struct {
	App     string
	Token   string // an API token scoped to App; never given to a worker
	Region  string // "" lets Fly choose
	BaseURL string // "" means FlyMachinesURL
	HTTP    *http.Client
	// Poll is how often Stop rechecks the Machine; 0 means 2 s.
	Poll time.Duration
}

var sizeRe = regexp.MustCompile(`^(shared|performance)-cpu-([0-9]+)x:([0-9]+)$`)

type flyGuest struct {
	CPUKind  string `json:"cpu_kind"`
	CPUs     int    `json:"cpus"`
	MemoryMB int    `json:"memory_mb"`
}

func guest(size string) (flyGuest, error) {
	m := sizeRe.FindStringSubmatch(size)
	if m == nil {
		return flyGuest{}, fmt.Errorf("machine size %q: want \"shared-cpu-2x:2048\"", size)
	}
	cpus, _ := strconv.Atoi(m[2])
	mem, _ := strconv.Atoi(m[3])
	if cpus < 1 || mem < 256 {
		return flyGuest{}, fmt.Errorf("machine size %q: at least 1 cpu and 256 MB", size)
	}
	return flyGuest{CPUKind: m[1], CPUs: cpus, MemoryMB: mem}, nil
}

func (f *Fly) Start(ctx context.Context, m Machine) (string, string, error) {
	g, err := guest(m.Size)
	if err != nil {
		return "", "", err
	}
	body := map[string]any{
		"name": fmt.Sprintf("run-%d", m.RunID),
		"config": map[string]any{
			"image": m.Image,
			"env":   m.Env,
			"guest": g,
			// The worker exits after posting; the Machine destroys itself
			// then. A stopped Machine still bills.
			"auto_destroy": true,
			"restart":      map[string]any{"policy": "no"},
			"metadata":     map[string]string{"chuck_run": strconv.FormatInt(m.RunID, 10)},
		},
	}
	if f.Region != "" {
		body["region"] = f.Region
	}
	var out struct {
		ID     string `json:"id"`
		Region string `json:"region"`
	}
	if err := f.do(ctx, http.MethodPost, "/machines", body, &out); err != nil {
		if errors.Is(err, ErrNoMachine) {
			return "", "", fmt.Errorf("fly: app %q not found", f.App)
		}
		return "", "", err
	}
	if out.ID == "" {
		return "", "", errors.New("fly: created a machine with no id")
	}
	return out.ID, out.Region, nil
}

func (f *Fly) State(ctx context.Context, id string) (State, error) {
	var out struct {
		State string `json:"state"`
	}
	err := f.do(ctx, http.MethodGet, "/machines/"+url.PathEscape(id), nil, &out)
	if errors.Is(err, ErrNoMachine) {
		return Gone, nil
	}
	if err != nil {
		return Alive, err
	}
	return flyState(out.State), nil
}

// flyState maps a Machine state. Only states from which the worker can
// still run count as alive; anything else, including a state this code
// does not know, is gone, so an unknown state ends the run instead of
// leaving it open until its deadline.
func flyState(s string) State {
	switch s {
	case "created", "starting", "started", "replacing", "updating":
		return Alive
	}
	return Gone
}

func (f *Fly) Stop(ctx context.Context, id string, grace time.Duration) error {
	body := map[string]any{"signal": "SIGTERM", "timeout": grace.String()}
	if err := f.do(ctx, http.MethodPost, "/machines/"+url.PathEscape(id)+"/stop", body, nil); err != nil {
		if errors.Is(err, ErrNoMachine) {
			return nil
		}
		return err
	}
	poll := f.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(grace + 10*time.Second)
	for time.Now().Before(deadline) {
		st, err := f.State(ctx, id)
		if err == nil && st == Gone {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
	return fmt.Errorf("fly: machine %s still running %s after SIGTERM", id, grace)
}

func (f *Fly) Destroy(ctx context.Context, id string) error {
	err := f.do(ctx, http.MethodDelete, "/machines/"+url.PathEscape(id)+"?force=true", nil, nil)
	if errors.Is(err, ErrNoMachine) {
		return nil
	}
	return err
}

func (f *Fly) do(ctx context.Context, method, path string, in, out any) error {
	base := strings.TrimRight(f.BaseURL, "/")
	if base == "" {
		base = FlyMachinesURL
	}
	var rd io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method,
		base+"/v1/apps/"+url.PathEscape(f.App)+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+f.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := f.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNoMachine
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = http.StatusText(resp.StatusCode)
		}
		// The path names the app and machine, never the token.
		return fmt.Errorf("fly %s %s: %d %s", method, path, resp.StatusCode, e.Error)
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

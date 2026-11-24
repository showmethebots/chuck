package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type flyCall struct {
	method, path, auth string
	body               map[string]any
}

func flyServer(t *testing.T, handle func(c flyCall) (int, string)) (*Fly, *[]flyCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []flyCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := flyCall{method: r.Method, path: r.URL.RequestURI(), auth: r.Header.Get("Authorization")}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &c.body)
		}
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		code, out := handle(c)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(srv.Close)
	return &Fly{App: "chuck-workers", Token: "fly-token", BaseURL: srv.URL, Poll: time.Millisecond}, &calls
}

func TestFlyStartAsksForAnEphemeralMachine(t *testing.T) {
	f, calls := flyServer(t, func(c flyCall) (int, string) {
		return 200, `{"id":"e286de4f","region":"iad","state":"created"}`
	})
	f.Region = "iad"
	env := map[string]string{EnvURL: "https://c", EnvRunID: "7", EnvToken: "tok"}
	id, region, err := f.Start(context.Background(), Machine{RunID: 7, Image: "img:1", Size: "performance-cpu-4x:8192", Env: env})
	if err != nil || id != "e286de4f" || region != "iad" {
		t.Fatalf("Start = %q %q %v", id, region, err)
	}
	c := (*calls)[0]
	if c.method != "POST" || c.path != "/v1/apps/chuck-workers/machines" || c.auth != "Bearer fly-token" {
		t.Errorf("call %+v", c)
	}
	cfg := c.body["config"].(map[string]any)
	if cfg["auto_destroy"] != true || cfg["restart"].(map[string]any)["policy"] != "no" || cfg["image"] != "img:1" {
		t.Errorf("config %v", cfg)
	}
	g := cfg["guest"].(map[string]any)
	if g["cpu_kind"] != "performance" || g["cpus"] != float64(4) || g["memory_mb"] != float64(8192) {
		t.Errorf("guest %v", g)
	}
	gotEnv := cfg["env"].(map[string]any)
	if len(gotEnv) != 3 || gotEnv[EnvToken] != "tok" {
		t.Errorf("env %v", gotEnv)
	}
	if c.body["region"] != "iad" || c.body["name"] != "run-7" {
		t.Errorf("body %v", c.body)
	}
	if _, _, err := f.Start(context.Background(), Machine{Size: "huge"}); err == nil {
		t.Error("a bad size reached Fly")
	}
}

func TestFlyStateStopDestroy(t *testing.T) {
	state := "started"
	f, calls := flyServer(t, func(c flyCall) (int, string) {
		switch {
		case c.method == "GET" && strings.HasSuffix(c.path, "/gone"):
			return 404, `{"error":"not found"}`
		case c.method == "GET" && strings.HasSuffix(c.path, "/broken"):
			return 500, `{"error":"boom"}`
		case c.method == "GET":
			return 200, `{"state":"` + state + `"}`
		case c.method == "POST" && strings.HasSuffix(c.path, "/stop"):
			state = "stopped"
			return 200, `{"ok":true}`
		case c.method == "DELETE" && strings.Contains(c.path, "/gone"):
			return 404, `{}`
		case c.method == "DELETE":
			return 200, `{"ok":true}`
		}
		return 400, ""
	})
	ctx := context.Background()
	for s, want := range map[string]State{"started": Alive, "starting": Alive, "created": Alive, "stopped": Gone, "destroyed": Gone, "suspended": Gone, "weird": Gone} {
		state = s
		if got, err := f.State(ctx, "m1"); err != nil || got != want {
			t.Errorf("state %s → %v %v, want %v", s, got, err, want)
		}
	}
	if got, err := f.State(ctx, "gone"); err != nil || got != Gone {
		t.Errorf("404 → %v %v, want gone", got, err)
	}
	if _, err := f.State(ctx, "broken"); err == nil {
		t.Error("a 500 read as a state")
	}
	state = "started"
	if err := f.Stop(ctx, "m1", time.Second); err != nil {
		t.Errorf("Stop: %v", err)
	}
	var stop flyCall
	for _, c := range *calls {
		if strings.HasSuffix(c.path, "/stop") {
			stop = c
		}
	}
	if stop.body["signal"] != "SIGTERM" || stop.body["timeout"] != "1s" {
		t.Errorf("stop body %v", stop.body)
	}
	if err := f.Destroy(ctx, "m1"); err != nil {
		t.Errorf("Destroy: %v", err)
	}
	if err := f.Destroy(ctx, "gone"); err != nil {
		t.Errorf("Destroy of a gone machine: %v", err)
	}
	last := (*calls)[len(*calls)-2]
	if last.method != "DELETE" || !strings.HasSuffix(last.path, "/m1?force=true") {
		t.Errorf("destroy call %+v", last)
	}
	for _, c := range *calls {
		if strings.Contains(c.path, "fly-token") {
			t.Error("the token reached a URL")
		}
	}
}

func TestLocalRunsTheBinaryWithOnlyTheRunVariables(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")
	bin := filepath.Join(dir, "worker.sh")
	script := "#!/bin/sh\nenv > " + out + "\nsleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHUCK_SECRET_THING", "must-not-leak")
	l := &Local{Bin: bin}
	ctx := context.Background()
	id, _, err := l.Start(ctx, Machine{Env: map[string]string{EnvURL: "http://localhost:8080", EnvRunID: "1", EnvToken: "tok"}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var env []byte
	for time.Now().Before(deadline) {
		if env, _ = os.ReadFile(out); strings.Contains(string(env), EnvToken) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(string(env), "CHUCK_RUN_TOKEN=tok") || strings.Contains(string(env), "must-not-leak") {
		t.Errorf("child env:\n%s", env)
	}
	if st, _ := l.State(ctx, id); st != Alive {
		t.Error("running child not alive")
	}
	if err := l.Stop(ctx, id, 5*time.Second); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if st, _ := l.State(ctx, id); st != Gone {
		t.Error("stopped child still alive")
	}
	if err := l.Destroy(ctx, id); err != nil {
		t.Error(err)
	}
	if st, _ := l.State(ctx, "local-0"); st != Gone {
		t.Error("an unknown id is not gone")
	}
}

package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Local runs the worker binary as a child process, for a laptop with
// CHUCK_URL pointing at a local chuck. The child gets PATH and HOME from
// chuck's environment, so it can find git and claude, plus the three run
// variables; nothing else of chuck's environment.
type Local struct {
	Bin string

	mu    sync.Mutex
	procs map[string]*localProc
}

type localProc struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (l *Local) Start(_ context.Context, m Machine) (string, string, error) {
	cmd := exec.Command(l.Bin)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for k, v := range m.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	// Its own process group, so stopping the worker stops claude and
	// whatever else it started too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return "", "", err
	}
	p := &localProc{cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.done) }()
	id := "local-" + strconv.Itoa(cmd.Process.Pid)
	l.mu.Lock()
	if l.procs == nil {
		l.procs = map[string]*localProc{}
	}
	l.procs[id] = p
	l.mu.Unlock()
	return id, "local", nil
}

func (l *Local) proc(id string) *localProc {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.procs[id]
}

func (l *Local) State(_ context.Context, id string) (State, error) {
	p := l.proc(id)
	if p == nil {
		// A chuck restart forgets its children; they are gone to it.
		return Gone, nil
	}
	select {
	case <-p.done:
		return Gone, nil
	default:
		return Alive, nil
	}
}

func (l *Local) Stop(ctx context.Context, id string, grace time.Duration) error {
	p := l.proc(id)
	if p == nil {
		return nil
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(grace):
		return fmt.Errorf("local worker %s still running %s after SIGTERM", id, grace)
	}
}

func (l *Local) Destroy(_ context.Context, id string) error {
	p := l.proc(id)
	if p == nil {
		return nil
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	<-p.done
	l.mu.Lock()
	delete(l.procs, id)
	l.mu.Unlock()
	return nil
}

// Package worker runs stage workers: it launches a Machine with a run
// token, serves the worker its spec and short-lived GitHub tokens, accepts
// its verdict, watches it, and stops and destroys it. It also decides
// which feedback is trusted enough to reach a prompt.
//
// See docs/superpowers/specs/2026-11-03-delivery-loop-design.md, "Workers".
//
// What a worker holds is the run token and nothing else. Its Machine's
// environment is CHUCK_URL, CHUCK_RUN_ID and CHUCK_RUN_TOKEN, because the
// Machine config is readable by anything holding the workers app's token.
// The prompt, the GitHub token and the validate stage's test credentials
// come from the run API, which refuses the token once the run has ended.
// chuck's App key and the Claude credentials never leave chuck.
package worker

import (
	"context"
	"errors"
	"time"
)

// Env names the only variables a worker Machine is given.
const (
	EnvURL   = "CHUCK_URL"
	EnvRunID = "CHUCK_RUN_ID"
	EnvToken = "CHUCK_RUN_TOKEN"
)

// Machine is what a Launcher starts for one run.
type Machine struct {
	RunID int64
	Image string
	// Size is loop.json's machine size, "shared-cpu-2x:2048".
	Size string
	// Env is exactly EnvURL, EnvRunID and EnvToken.
	Env map[string]string
}

// State is what a Launcher knows of a Machine now.
type State int

const (
	// Alive covers created, starting and started: the worker may still
	// post.
	Alive State = iota
	// Gone covers stopped, destroyed and unknown: nothing will post.
	Gone
)

// Launcher starts and stops worker Machines: Fly in production, Local on
// a laptop, Fake in tests.
type Launcher interface {
	// Start creates and starts the Machine, set to destroy itself when the
	// worker exits and never to restart. It returns the Machine's id and
	// region.
	Start(ctx context.Context, m Machine) (id, region string, err error)
	// State reports whether the Machine can still post a verdict.
	State(ctx context.Context, id string) (State, error)
	// Stop sends SIGTERM and waits up to grace for the worker to exit.
	Stop(ctx context.Context, id string, grace time.Duration) error
	// Destroy removes the Machine. A Machine already gone is not an error.
	Destroy(ctx context.Context, id string) error
}

// ErrNoMachine means the Launcher has no such Machine.
var ErrNoMachine = errors.New("no such machine")

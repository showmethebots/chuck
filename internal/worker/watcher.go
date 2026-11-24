package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/showmethebots/chuck/internal/engine"
	"github.com/showmethebots/chuck/internal/loop"
	"github.com/showmethebots/chuck/internal/memory"
)

// MaxStartFailures is how many runs of one attempt may end without their
// Machine ever starting before the item goes to a human. It matches the
// executor's engine.EscalateAfter for a launch that fails outright.
const MaxStartFailures = engine.EscalateAfter

// neverStarted counts the earlier runs of run's attempt that ended without
// starting (failed launches and Machines that never fetched their spec)
// since the item entered its current state. A same-attempt relaunch keeps
// the state, so those add up; a human re-queue after the escalation moves
// it, so the re-queued item gets its own tries.
func (m *Manager) neverStarted(ctx context.Context, run memory.Run) (int, error) {
	it, err := m.Store.ItemByID(ctx, run.ItemID)
	if err != nil {
		return 0, err
	}
	runs, err := m.Store.RunsForItem(ctx, run.ItemID, run.Stage)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range runs {
		if r.ID != run.ID && r.Attempt == run.Attempt && r.Status == memory.RunFailed && r.StartedAt.IsZero() &&
			!r.CreatedAt.Before(it.StateSince) {
			n++
		}
	}
	return n, nil
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Watch checks every open run once now, which resumes the runs a restart
// left open, and then every Poll until ctx ends.
func (m *Manager) Watch(ctx context.Context) {
	t := time.NewTicker(orDefault(m.Poll, 30*time.Second))
	defer t.Stop()
	for {
		m.WatchOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// WatchOnce is one pass of the watcher. A run whose Machine never fetched
// its spec within StartTimeout fails as never started, which relaunches it
// as the same attempt. A running run whose Machine is gone without a
// verdict fails. One still running Overrun past its deadline (the worker's
// own deadline handling failed) is stopped with SIGTERM, destroyed, and
// times out. Then every ended run's leftover Machine is destroyed.
func (m *Manager) WatchOnce(ctx context.Context) {
	runs, err := m.Store.OpenRuns(ctx)
	if err != nil {
		m.log().Error("watcher: open runs", "err", err.Error())
		return
	}
	now := m.now()
	for _, run := range runs {
		if ctx.Err() != nil {
			return
		}
		m.check(ctx, run, now)
	}
	m.sweepMachines(ctx, now)
	m.revokeEnded(ctx)
}

func (m *Manager) check(ctx context.Context, run memory.Run, now time.Time) {
	alive := false
	if run.MachineID != "" {
		st, err := m.Launcher.State(ctx, run.MachineID)
		if err != nil {
			// Unknown is not gone: a Machines API outage must not fail
			// every run. The deadline still applies on a later pass.
			m.log().Warn("watcher: machine state", "run", run.ID, "machine", run.MachineID, "err", err.Error())
			return
		}
		alive = st == Alive
	}
	switch run.Status {
	case memory.RunLaunching:
		if alive && now.Sub(run.CreatedAt) < orDefault(m.StartTimeout, 10*time.Minute) {
			return
		}
		if !alive && run.MachineID == "" && now.Sub(run.CreatedAt) < orDefault(m.StartTimeout, 10*time.Minute) {
			return // Launch is still creating it
		}
		v := loop.VerdictInput{Err: "machine never started", SameAttempt: true}
		n, err := m.neverStarted(ctx, run)
		if err != nil {
			m.log().Error("watcher: count failed starts", "run", run.ID, "err", err.Error())
			return
		}
		if n+1 >= MaxStartFailures {
			// A worker that dies before its first spec fetch (a broken
			// image, chuck unreachable from the workers app) would
			// otherwise relaunch every pass forever.
			v = loop.VerdictInput{Err: fmt.Sprintf("machine never started, %d times running", n+1), Escalate: true}
		}
		m.end(ctx, run, v, memory.RunEnd{Status: memory.RunFailed, Reason: "machine_never_started"})
	case memory.RunRunning:
		switch {
		case !alive:
			m.end(ctx, run, loop.VerdictInput{Err: "machine gone without a verdict"},
				memory.RunEnd{Status: memory.RunFailed, Reason: "machine_gone"})
		case now.After(run.DeadlineAt.Add(orDefault(m.Overrun, 10*time.Minute))):
			grace := orDefault(m.StopGrace, 60*time.Second)
			if err := m.Launcher.Stop(ctx, run.MachineID, grace); err != nil {
				m.log().Warn("watcher: stop", "run", run.ID, "machine", run.MachineID, "err", err.Error())
			}
			m.end(ctx, run, loop.VerdictInput{Err: "timed out: no verdict ten minutes past the deadline"},
				memory.RunEnd{Status: memory.RunTimedOut, Reason: "timeout"})
		}
	}
}

// end commits chuck's own error verdict for a run, then destroys its
// Machine. A run that ended meanwhile (the worker's verdict landed first)
// is left as it is.
func (m *Manager) end(ctx context.Context, run memory.Run, v loop.VerdictInput, e memory.RunEnd) {
	v.RunID = run.ID
	// Re-read: a worker fetching its spec or posting its verdict while
	// this pass looked at its Machine has moved the run on.
	if now, err := m.Store.RunByID(ctx, run.ID); err != nil || now.Status != run.Status {
		return
	}
	if _, err := m.Verdicts.Verdict(ctx, v, e); err != nil {
		if !errors.Is(err, memory.ErrStale) {
			m.log().Error("watcher: end run", "run", run.ID, "err", err.Error())
			return // the next pass tries again
		}
	} else {
		m.log().Warn("watcher ended a run", "run", run.ID, "status", e.Status, "reason", e.Reason)
	}
	if err := m.destroy(ctx, run); err != nil {
		m.log().Warn("watcher: destroy", "run", run.ID, "machine", run.MachineID, "err", err.Error())
	}
}

// sweepMachines destroys what auto_destroy left behind: a stopped Machine
// still bills. A Machine may outlive its own verdict by ExitGrace while
// the worker exits; one chuck ended is destroyed at once.
func (m *Manager) sweepMachines(ctx context.Context, now time.Time) {
	runs, err := m.Store.UndestroyedRuns(ctx)
	if err != nil {
		m.log().Error("watcher: undestroyed runs", "err", err.Error())
		return
	}
	for _, run := range runs {
		if ctx.Err() != nil {
			return
		}
		st, err := m.Launcher.State(ctx, run.MachineID)
		if err != nil {
			continue
		}
		if st == Alive && !run.CallbackAt.IsZero() && now.Sub(run.CallbackAt) < orDefault(m.ExitGrace, 2*time.Minute) {
			continue
		}
		if st == Gone {
			if err := m.Store.MarkRunDestroyed(ctx, run.ID, now); err != nil && !errors.Is(err, memory.ErrStale) {
				m.log().Error("watcher: mark destroyed", "run", run.ID, "err", err.Error())
			}
			continue
		}
		if err := m.destroy(ctx, run); err != nil {
			m.log().Warn("watcher: destroy leftover", "run", run.ID, "machine", run.MachineID, "err", err.Error())
		}
	}
}

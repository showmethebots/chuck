package worker

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Fake is an in-memory Launcher for tests: Start records the Machine and
// leaves it alive until the test calls Exit or the code under test stops
// or destroys it.
type Fake struct {
	mu         sync.Mutex
	next       int
	machines   map[string]*FakeMachine
	StartErr   error
	StateErr   error
	DestroyErr error
	// KeepRunning makes Stop leave the Machine alive, as a worker that
	// ignores SIGTERM would.
	KeepRunning bool
}

// FakeMachine is one Machine Fake started.
type FakeMachine struct {
	Machine
	ID        string
	State     State
	Stopped   bool
	Destroyed bool
}

func (f *Fake) Start(_ context.Context, m Machine) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StartErr != nil {
		return "", "", f.StartErr
	}
	f.next++
	id := fmt.Sprintf("m%d", f.next)
	if f.machines == nil {
		f.machines = map[string]*FakeMachine{}
	}
	f.machines[id] = &FakeMachine{Machine: m, ID: id, State: Alive}
	return id, "iad", nil
}

func (f *Fake) State(_ context.Context, id string) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StateErr != nil {
		return Alive, f.StateErr
	}
	m, ok := f.machines[id]
	if !ok {
		return Gone, nil
	}
	return m.State, nil
}

func (f *Fake) Stop(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.machines[id]; ok {
		m.Stopped = true
		if !f.KeepRunning {
			m.State = Gone
		}
	}
	return nil
}

func (f *Fake) Destroy(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.DestroyErr != nil {
		return f.DestroyErr
	}
	if m, ok := f.machines[id]; ok {
		m.State, m.Destroyed = Gone, true
	}
	return nil
}

// Exit marks a Machine gone, as a worker that exited would leave it.
func (f *Fake) Exit(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.machines[id]; ok {
		m.State = Gone
	}
}

// Get returns a copy of the Machine with that id.
func (f *Fake) Get(id string) (FakeMachine, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines[id]
	if !ok {
		return FakeMachine{}, false
	}
	return *m, true
}

// All returns a copy of every Machine started, in start order.
func (f *Fake) All() []FakeMachine {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeMachine, 0, len(f.machines))
	for i := 1; i <= f.next; i++ {
		if m, ok := f.machines[fmt.Sprintf("m%d", i)]; ok {
			out = append(out, *m)
		}
	}
	return out
}

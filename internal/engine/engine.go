// Package engine is the I/O around loop.Transition. It turns stored GitHub
// deliveries, worker verdicts, dashboard actions and the tick into loop
// inputs, gathers the facts each input needs, commits what Transition
// decides, and then runs the effects with chuck's App token.
//
// The order is the rule the spec's "Failure handling" rests on: state and
// the transition row are committed before any effect runs, a failed effect
// leaves the item in its committed state with the error on the row, and
// the tick retries what is safe to retry. internal/loop stays pure; this
// package is where the network and the database meet it.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/showmethebots/chuck/internal/config"
	"github.com/showmethebots/chuck/internal/loop"
	"github.com/showmethebots/chuck/internal/memory"
)

// Launcher starts and stops stage workers. It lands with #27; until then
// the engine has none, and a launch effect fails with the reason on its
// transition row.
type Launcher interface {
	// Launch starts a worker for the item's stage. It records the run, so
	// a retried launch that finds the item's run open does not call it.
	Launch(ctx context.Context, req LaunchRequest) error
	// Destroy stops and destroys a run's Machine.
	Destroy(ctx context.Context, runID int64) error
}

// LaunchRequest is one launch effect.
type LaunchRequest struct {
	Repo   memory.Repo
	Config *config.Loop
	Item   memory.Item
	Stage  config.Stage
}

// Ratchet opens the e2e coverage issue for an item that reached done. It
// lands with #32.
type Ratchet interface {
	Open(ctx context.Context, repo memory.Repo, item memory.Item) error
}

// defaultTick is the tick when none is configured.
const defaultTick = 30 * time.Minute

// EscalateAfter is how many ticks an effect may fail, or an item's facts
// may fail to read, before the item escalates to needs-human.
const EscalateAfter = 3

// Engine runs the loop for every enabled repo.
type Engine struct {
	Store    *memory.Store
	GitHub   Source
	Launcher Launcher // nil until #27
	Ratchet  Ratchet  // nil until #32
	Humans   []int64
	// Tick is config/chuck.yaml's tick; Retention its webhook_retention.
	Tick      time.Duration
	Retention time.Duration
	Log       *slog.Logger
	Now       func() time.Time

	// mu serialises every input. There is one Machine and one writer; a
	// dashboard press, a verdict and the tick never interleave.
	mu    sync.Mutex
	nudge chan struct{}
	once  sync.Once

	factErrors map[int64]int          // item id → consecutive ticks its facts failed
	hookFails  map[int64]hookFailure  // webhook id → failures so far
	logins     map[int64]string       // human id → login, for mentions
	configs    map[int64]*config.Loop // repo id → parsed loop.json, by config sha
	configSHA  map[int64]string
}

type hookFailure struct {
	n    int
	next time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

func (e *Engine) init() {
	e.once.Do(func() {
		e.nudge = make(chan struct{}, 1)
		e.factErrors = map[int64]int{}
		e.hookFails = map[int64]hookFailure{}
		e.logins = map[int64]string{}
		e.configs = map[int64]*config.Loop{}
		e.configSHA = map[int64]string{}
		if e.Log == nil {
			e.Log = slog.New(slog.DiscardHandler)
		}
	})
}

// Notify tells the engine a delivery is waiting. It never blocks; the
// webhook receiver calls it after storing each new delivery.
func (e *Engine) Notify() {
	e.init()
	select {
	case e.nudge <- struct{}{}:
	default:
	}
}

// Run ticks once at start (which also handles deliveries a crash left
// behind), then handles deliveries as they arrive and ticks every Tick,
// until ctx ends.
func (e *Engine) Run(ctx context.Context) {
	e.init()
	e.TickOnce(ctx)
	t := time.NewTicker(e.tickEvery())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.nudge:
			e.mu.Lock()
			e.processWebhooks(ctx)
			e.mu.Unlock()
		case <-t.C:
			e.TickOnce(ctx)
		}
	}
}

// repoCtx is what every input on one repo needs.
type repoCtx struct {
	row  memory.Repo
	cfg  *config.Loop
	api  RepoAPI
	loop loop.Repo
	bot  int64
}

var errRepoOff = errors.New("repo is not in the loop")

// openRepo loads an enabled repo's config and client. A repo that is
// disabled, or whose stored config no longer parses, is errRepoOff.
func (e *Engine) openRepo(ctx context.Context, row memory.Repo) (*repoCtx, error) {
	if !row.Enabled || row.ConfigJSON == "" {
		return nil, errRepoOff
	}
	cfg := e.configs[row.ID]
	if cfg == nil || e.configSHA[row.ID] != row.ConfigSHA {
		var err error
		if cfg, err = config.ParseLoop([]byte(row.ConfigJSON)); err != nil {
			return nil, fmt.Errorf("%w: loop.json invalid: %v", errRepoOff, err)
		}
		e.configs[row.ID], e.configSHA[row.ID] = cfg, row.ConfigSHA
	}
	api, err := e.GitHub.Repo(row)
	if err != nil {
		return nil, err
	}
	bot, err := e.GitHub.BotID(ctx, row.AppOwner)
	if err != nil {
		return nil, fmt.Errorf("the App's bot user: %w", err)
	}
	return &repoCtx{
		row: row, cfg: cfg, api: api, bot: bot,
		loop: loop.Repo{FullName: row.Owner + "/" + row.Name, AppUserID: bot, Humans: e.Humans},
	}, nil
}

// meta is where an input came from, for its transition rows.
type meta struct {
	kind  string // webhook, verdict, dashboard, tick
	ref   string // delivery id, run id, ...
	actor string // GitHub login for a dashboard action, chuck otherwise
}

func toLoop(it *memory.Item) *loop.Item {
	if it == nil {
		return nil
	}
	return &loop.Item{
		ID: it.ID, Kind: it.Kind, Number: it.Number, IssueNumber: it.IssueNumber, RatchetOf: it.RatchetOf,
		State: loop.State(it.State), StateSince: it.StateSince,
		EscalatedFrom: loop.State(it.EscalatedFrom), PendingStage: config.Stage(it.PendingStage),
	}
}

// apply decides and commits one input, then runs its effects.
func (e *Engine) apply(ctx context.Context, rc *repoCtx, item *memory.Item, in loop.Input, m meta) (loop.Result, error) {
	res := loop.Transition(toLoop(item), rc.loop, rc.cfg, in, e.now())
	return res, e.commit(ctx, rc, res, m, nil)
}

// verdictEnd ends a run in the same commit as its verdict's changes.
type verdictEnd struct {
	runID int64
	end   memory.RunEnd
}

// commit writes res's changes and their transition rows in one
// transaction, each row carrying its item's effects as pending, and only
// then runs the effects. An effect for an item no change moves (a status
// set on a human's approval) gets a row of its own that keeps the item's
// state, so every effect has a row to record its result on.
func (e *Engine) commit(ctx context.Context, rc *repoCtx, res loop.Result, m meta, ve *verdictEnd) error {
	now := e.now()
	changes := append([]loop.Change(nil), res.Changes...)
	for _, ef := range res.Effects {
		if hasChange(changes, ef.Item) {
			continue
		}
		it, err := e.Store.ItemByNumber(ctx, rc.row.ID, ef.Item.Kind, ef.Item.Number)
		if err != nil {
			return fmt.Errorf("%s effect for %s#%d: %w", ef.Kind, ef.Item.Kind, ef.Item.Number, err)
		}
		li := toLoop(&it)
		changes = append(changes, loop.Change{Item: *li, To: li.State, EscalatedFrom: li.EscalatedFrom,
			PendingStage: li.PendingStage, Reason: res.Reason})
	}
	if len(changes) == 0 && ve == nil {
		return nil
	}

	// Each item's effects go on its last row of this commit, in order.
	type slot struct{ row, idx int }
	rows := make([][]effectRecord, len(changes))
	slots := make([]slot, len(res.Effects))
	lastRow := map[loop.Key]int{}
	for i, c := range changes {
		lastRow[c.Item.Key()] = i
	}
	for i, ef := range res.Effects {
		r := lastRow[ef.Item]
		slots[i] = slot{r, len(rows[r])}
		rows[r] = append(rows[r], effectRecord{Effect: ef, Result: effectResult{Status: statusPending}})
	}

	mc := make([]memory.Change, 0, len(changes))
	prior := map[int64]int64{} // item id → its latest transition before this commit
	state := map[loop.Key]loop.State{}
	for i, c := range changes {
		from := c.Item.State
		if s, ok := state[c.Item.Key()]; ok {
			from = s // a second change to one item follows the first
		}
		state[c.Item.Key()] = c.To
		ch := memory.Change{
			From: string(from), To: string(c.To),
			EscalatedFrom: string(c.EscalatedFrom), PendingStage: string(c.PendingStage),
			InputKind: m.kind, InputRef: m.ref, Reason: c.Reason, Actor: m.actor,
			ConfigSHA: rc.row.ConfigSHA, Effects: encodeRecords(rows[i]),
		}
		id := c.Item.ID
		if id == 0 {
			ch.New = &memory.Item{RepoID: rc.row.ID, Kind: c.Item.Kind, Number: c.Item.Number,
				IssueNumber: c.Item.IssueNumber, RatchetOf: c.Item.RatchetOf}
		} else {
			ch.ItemID = id
			if _, seen := prior[id]; !seen {
				if t, err := e.Store.LatestTransition(ctx, id); err == nil {
					prior[id] = t.ID
				}
			}
		}
		mc = append(mc, ch)
	}

	var done []memory.Committed
	var err error
	if ve != nil {
		done, err = e.Store.CommitVerdict(ctx, now, ve.runID, ve.end, mc...)
	} else {
		done, err = e.Store.Commit(ctx, now, mc...)
	}
	if err != nil {
		return err
	}
	for _, tid := range prior {
		e.supersede(ctx, tid)
	}

	// State is committed. Effects run in order and stop at the first
	// failure; what is left stays pending for the tick.
	for i, ef := range res.Effects {
		s := slots[i]
		rec := &rows[s.row][s.idx]
		outcome := e.run(ctx, rc, ef, done[s.row].TransitionID, s.idx)
		rec.Result = outcome.result(now, rec.Result)
		if err := e.Store.RecordEffects(ctx, done[s.row].TransitionID, encodeRecords(rows[s.row])); err != nil {
			e.Log.Error("record effect", "transition", done[s.row].TransitionID, "err", err.Error())
		}
		if outcome.escalate != "" {
			// The escalation is a newer row for the item; it supersedes
			// whatever this commit left unrun.
			e.escalateKey(ctx, rc, ef.Item, outcome.escalate)
			break
		}
		if rec.Result.Status == statusFailed {
			e.Log.Warn("effect failed; the tick retries it", "repo", rc.loop.FullName, "effect", string(ef.Kind),
				"item", fmt.Sprintf("%s#%d", ef.Item.Kind, ef.Item.Number), "err", rec.Result.Error)
			break
		}
	}
	return nil
}

func hasChange(cs []loop.Change, k loop.Key) bool {
	for _, c := range cs {
		if c.Item.Key() == k {
			return true
		}
	}
	return false
}

// escalateKey escalates an item by key after an effect asked for it.
func (e *Engine) escalateKey(ctx context.Context, rc *repoCtx, k loop.Key, reason string) {
	it, err := e.Store.ItemByNumber(ctx, rc.row.ID, k.Kind, k.Number)
	if err != nil {
		e.Log.Error("escalate", "repo", rc.loop.FullName, "item", fmt.Sprintf("%s#%d", k.Kind, k.Number), "err", err.Error())
		return
	}
	e.escalate(ctx, rc, it, reason)
}

// escalate moves an item to needs-human for a reason the engine found.
func (e *Engine) escalate(ctx context.Context, rc *repoCtx, it memory.Item, reason string) {
	res := loop.Escalate(*toLoop(&it), rc.cfg, reason)
	if len(res.Changes) == 0 {
		e.Log.Warn("not escalated", "repo", rc.loop.FullName, "item", it.ID, "why", res.Reason, "reason", reason)
		return
	}
	if err := e.commit(ctx, rc, res, meta{kind: "tick", ref: "escalation", actor: "chuck"}, nil); err != nil {
		e.Log.Error("escalate", "repo", rc.loop.FullName, "item", it.ID, "err", err.Error())
	}
}

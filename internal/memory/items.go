package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrStale means a row was not in the state a write expected: something
// else moved it first. Nothing was written.
var ErrStale = errors.New("not in the expected state")

// ErrNotFound means the row does not exist.
var ErrNotFound = errors.New("not found")

// Repo is one repository chuck knows about.
type Repo struct {
	ID             int64
	Owner, Name    string
	InstallationID int64
	AppOwner       string // which configured App acts on it
	DefaultBranch  string
	Enabled        bool
	ConfigJSON     string // loop.json as last read; "" if never read
	ConfigSHA      string
	ConfigReadAt   time.Time
}

// UpsertRepo records a repo by owner and name, case-insensitively, and
// returns its id. The config columns are left alone; SetRepoConfig owns them.
func (s *Store) UpsertRepo(ctx context.Context, r Repo) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO repos
		(owner, name, installation_id, app_owner, default_branch, enabled)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (owner, name) DO UPDATE SET
			installation_id = excluded.installation_id,
			app_owner = excluded.app_owner,
			default_branch = excluded.default_branch,
			enabled = excluded.enabled
		RETURNING id`,
		r.Owner, r.Name, r.InstallationID, r.AppOwner, r.DefaultBranch, r.Enabled).Scan(&id)
	return id, err
}

// SetRepoConfig records the loop.json chuck last read for a repo.
func (s *Store) SetRepoConfig(ctx context.Context, repoID int64, configJSON, sha string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE repos SET config_json = ?, config_sha = ?, config_read_at = ? WHERE id = ?`,
		configJSON, sha, ts(at), repoID)
	return oneRow(res, err)
}

const repoCols = `id, owner, name, installation_id, app_owner, default_branch, enabled,
	config_json, config_sha, config_read_at`

func scanRepo(row interface{ Scan(...any) error }) (Repo, error) {
	var r Repo
	var cfg, sha, at sql.NullString
	if err := row.Scan(&r.ID, &r.Owner, &r.Name, &r.InstallationID, &r.AppOwner,
		&r.DefaultBranch, &r.Enabled, &cfg, &sha, &at); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r, ErrNotFound
		}
		return r, err
	}
	r.ConfigJSON, r.ConfigSHA = cfg.String, sha.String
	var err error
	r.ConfigReadAt, err = scanTS(at)
	return r, err
}

// SetRepoEnabled turns the loop on or off for a repo.
func (s *Store) SetRepoEnabled(ctx context.Context, repoID int64, enabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE repos SET enabled = ? WHERE id = ?`, enabled, repoID)
	return oneRow(res, err)
}

// RepoByName finds a repo by owner and name, case-insensitively.
func (s *Store) RepoByName(ctx context.Context, owner, name string) (Repo, error) {
	return scanRepo(s.db.QueryRowContext(ctx,
		`SELECT `+repoCols+` FROM repos WHERE owner = ? AND name = ?`, owner, name))
}

// Repos lists every repo, by owner and name.
func (s *Store) Repos(ctx context.Context) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+repoCols+` FROM repos ORDER BY owner, name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Item is an issue or PR in the loop.
type Item struct {
	ID            int64
	RepoID        int64
	Kind          string // "issue" or "pr"
	Number        int
	Title         string
	IssueNumber   int // for a PR: the issue it serves
	RatchetOf     int // for a ratchet issue: the issue it covers
	State         string
	StateSince    time.Time
	EscalatedFrom string
	PendingStage  string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Item kinds.
const (
	KindIssue = "issue"
	KindPR    = "pr"
)

// Change is one item's move to a new state and the transition row that
// records it. With ItemID zero it creates the item from New; otherwise
// From must be the item's current state.
type Change struct {
	ItemID int64
	New    *Item // for a create: repo, kind, number, title, issue number, ratchet of
	From   string
	To     string
	// The item's escalated_from and pending_stage after the change; "" is
	// NULL. They are set on every change, so a stale value cannot linger.
	EscalatedFrom string
	PendingStage  string

	InputKind string // webhook, verdict, dashboard, tick
	InputRef  string // delivery id, run id, ...
	Reason    string
	Actor     string // GitHub login for a dashboard action, "chuck" otherwise
	ConfigSHA string
	Effects   string // JSON effect list; "" is []
}

// Committed is what Commit wrote for one Change.
type Committed struct {
	ItemID       int64
	TransitionID int64
}

// Commit applies every change and its transition row in one transaction:
// all of them are written or none. The executor calls it before any GitHub
// call, so chuck is never behind its own database. A change whose item has
// moved returns ErrStale and writes nothing.
func (s *Store) Commit(ctx context.Context, at time.Time, changes ...Change) ([]Committed, error) {
	var out []Committed
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = commitTx(ctx, tx, at, changes)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CommitVerdict ends a run and applies the changes its verdict caused in
// one transaction. A verdict counts only for the run that is open, so if
// the run has already ended (a second post, a late one) it returns
// ErrStale and writes nothing: the duplicate cannot move the item.
func (s *Store) CommitVerdict(ctx context.Context, at time.Time, runID int64, end RunEnd, changes ...Change) ([]Committed, error) {
	var out []Committed
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := endRun(ctx, tx, runID, end, at); err != nil {
			return err
		}
		var err error
		out, err = commitTx(ctx, tx, at, changes)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func commitTx(ctx context.Context, tx *sql.Tx, at time.Time, changes []Change) ([]Committed, error) {
	out := make([]Committed, 0, len(changes))
	for i, c := range changes {
		itemID, from, err := applyChange(ctx, tx, at, c)
		if err != nil {
			return nil, fmt.Errorf("change %d: %w", i, err)
		}
		effects := c.Effects
		if effects == "" {
			effects = "[]"
		}
		var tid int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO transitions
			(item_id, from_state, to_state, input_kind, input_ref, reason, actor, config_sha, effects_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			itemID, from, c.To, c.InputKind, c.InputRef, c.Reason, c.Actor, c.ConfigSHA, effects, ts(at),
		).Scan(&tid); err != nil {
			return nil, fmt.Errorf("change %d: transition: %w", i, err)
		}
		out = append(out, Committed{ItemID: itemID, TransitionID: tid})
	}
	return out, nil
}

func applyChange(ctx context.Context, tx *sql.Tx, at time.Time, c Change) (itemID int64, from any, err error) {
	if c.ItemID == 0 {
		if c.New == nil {
			return 0, nil, errors.New("create without New")
		}
		n := c.New
		err = tx.QueryRowContext(ctx, `INSERT INTO items
			(repo_id, kind, number, title, issue_number, ratchet_of, state, state_since,
			 escalated_from, pending_stage, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			n.RepoID, n.Kind, n.Number, n.Title, nullInt(n.IssueNumber), nullInt(n.RatchetOf),
			c.To, ts(at), nullStr(c.EscalatedFrom), nullStr(c.PendingStage), ts(at), ts(at),
		).Scan(&itemID)
		return itemID, nil, err
	}
	// state_since moves only when the state does, so a self-transition (a
	// relaunch in review) does not reset the stale clock.
	res, err := tx.ExecContext(ctx, `UPDATE items SET
		state = ?,
		state_since = CASE WHEN state = ? THEN state_since ELSE ? END,
		escalated_from = ?, pending_stage = ?, updated_at = ?
		WHERE id = ? AND state = ?`,
		c.To, c.To, ts(at), nullStr(c.EscalatedFrom), nullStr(c.PendingStage), ts(at), c.ItemID, c.From)
	if err != nil {
		return 0, nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil, err
	}
	if n == 0 {
		return 0, nil, ErrStale
	}
	return c.ItemID, c.From, nil
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// RecordEffects replaces a transition's effect list with the same list
// carrying each effect's result.
func (s *Store) RecordEffects(ctx context.Context, transitionID int64, effectsJSON string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE transitions SET effects_json = ? WHERE id = ?`, effectsJSON, transitionID)
	return oneRow(res, err)
}

const itemCols = `id, repo_id, kind, number, title, issue_number, ratchet_of, state, state_since,
	escalated_from, pending_stage, created_at, updated_at`

func scanItem(row interface{ Scan(...any) error }) (Item, error) {
	var it Item
	var issue, ratchet sql.NullInt64
	var esc, pending sql.NullString
	var since, created, updated string
	if err := row.Scan(&it.ID, &it.RepoID, &it.Kind, &it.Number, &it.Title, &issue, &ratchet,
		&it.State, &since, &esc, &pending, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return it, ErrNotFound
		}
		return it, err
	}
	it.IssueNumber, it.RatchetOf = int(issue.Int64), int(ratchet.Int64)
	it.EscalatedFrom, it.PendingStage = esc.String, pending.String
	var err error
	if it.StateSince, err = parseTS(since); err != nil {
		return it, err
	}
	if it.CreatedAt, err = parseTS(created); err != nil {
		return it, err
	}
	it.UpdatedAt, err = parseTS(updated)
	return it, err
}

// ItemByID returns one item.
func (s *Store) ItemByID(ctx context.Context, id int64) (Item, error) {
	return scanItem(s.db.QueryRowContext(ctx, `SELECT `+itemCols+` FROM items WHERE id = ?`, id))
}

// ItemByNumber finds an issue or PR by its number in a repo.
func (s *Store) ItemByNumber(ctx context.Context, repoID int64, kind string, number int) (Item, error) {
	return scanItem(s.db.QueryRowContext(ctx,
		`SELECT `+itemCols+` FROM items WHERE repo_id = ? AND kind = ? AND number = ?`, repoID, kind, number))
}

// Items lists a repo's items, oldest state first within each state, so the
// first ready item is the next to build.
func (s *Store) Items(ctx context.Context, repoID int64) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+itemCols+` FROM items WHERE repo_id = ? ORDER BY state, state_since, id`, repoID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Transition is one row of an item's history.
type Transition struct {
	ID        int64
	ItemID    int64
	From      string // "" for the row that created the item
	To        string
	InputKind string
	InputRef  string
	Reason    string
	Actor     string
	ConfigSHA string
	Effects   string
	CreatedAt time.Time
}

const transitionCols = `id, item_id, from_state, to_state, input_kind, input_ref,
	reason, actor, config_sha, effects_json, created_at`

func scanTransition(row interface{ Scan(...any) error }) (Transition, error) {
	var t Transition
	var from sql.NullString
	var at string
	if err := row.Scan(&t.ID, &t.ItemID, &from, &t.To, &t.InputKind, &t.InputRef,
		&t.Reason, &t.Actor, &t.ConfigSHA, &t.Effects, &at); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return t, ErrNotFound
		}
		return t, err
	}
	t.From = from.String
	var err error
	t.CreatedAt, err = parseTS(at)
	return t, err
}

// Transitions returns an item's history, oldest first.
func (s *Store) Transitions(ctx context.Context, itemID int64) ([]Transition, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+transitionCols+` FROM transitions WHERE item_id = ? ORDER BY id`, itemID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Transition
	for rows.Next() {
		t, err := scanTransition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TransitionByID returns one transition.
func (s *Store) TransitionByID(ctx context.Context, id int64) (Transition, error) {
	return scanTransition(s.db.QueryRowContext(ctx, `SELECT `+transitionCols+` FROM transitions WHERE id = ?`, id))
}

// LatestTransition returns an item's most recent transition. Its effects
// are the only ones the tick retries: an older row's unfinished effects
// were for a state the item has since left.
func (s *Store) LatestTransition(ctx context.Context, itemID int64) (Transition, error) {
	return scanTransition(s.db.QueryRowContext(ctx,
		`SELECT `+transitionCols+` FROM transitions WHERE item_id = ? ORDER BY id DESC LIMIT 1`, itemID))
}

// oneRow turns an Exec that should touch exactly one row into ErrNotFound
// when it touched none.
func oneRow(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

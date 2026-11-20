package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"
)

// Run statuses.
const (
	RunLaunching = "launching"
	RunRunning   = "running"
	RunFinished  = "finished"
	RunFailed    = "failed"
	RunCancelled = "cancelled"
	RunTimedOut  = "timed_out"
)

// Credentials a run can use.
const (
	CredentialAPIKey = "api_key"
	CredentialOAuth  = "oauth"
)

// ReasonCredits is a run's reason when the API key ran out of credit. Such
// a run is relaunched on the OAuth token as the same attempt.
const ReasonCredits = "credits"

// Run is one worker launch.
type Run struct {
	ID            int64
	ItemID        int64
	Stage         string
	Attempt       int
	Status        string
	Reason        string
	MachineID     string
	MachineRegion string
	Image         string
	Model         string
	Credential    string
	Prompt        string
	SchemaName    string
	Ref           string
	TokenHash     string
	CreatedAt     time.Time
	DeadlineAt    time.Time
	StartedAt     time.Time
	CallbackAt    time.Time
	DestroyedAt   time.Time
	ExitCode      *int
	CostUSD       float64
	InputTokens   int64
	OutputTokens  int64
	DurationMS    int64
	VerdictJSON   string
	LogTail       string
}

// HashToken is how a run token or session cookie is stored: sha256, hex.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// InsertRun records a launch and returns it with its id and attempt. The
// attempt is chosen here, not by the caller: a relaunch keeps the previous
// attempt when that run failed without its Machine ever starting or ran out
// of API credit; otherwise it is the next one. The launch counts as one run
// in the month's spend. An item with a run still open is refused.
func (s *Store) InsertRun(ctx context.Context, r Run, at time.Time) (Run, error) {
	r.Status, r.CreatedAt = RunLaunching, at
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		if r.Attempt, err = nextAttempt(ctx, tx, r.ItemID, r.Stage); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `INSERT INTO runs
			(item_id, stage, attempt, status, image, model, credential, prompt, schema_name, ref,
			 token_hash, created_at, deadline_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			r.ItemID, r.Stage, r.Attempt, r.Status, r.Image, r.Model, r.Credential, r.Prompt,
			r.SchemaName, r.Ref, r.TokenHash, ts(at), ts(r.DeadlineAt)).Scan(&r.ID); err != nil {
			return err
		}
		return addSpend(ctx, tx, r.ItemID, r.Credential, at, 0, 1)
	})
	return r, err
}

func nextAttempt(ctx context.Context, tx *sql.Tx, itemID int64, stage string) (int, error) {
	var attempt int
	var status, reason string
	var started sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT attempt, status, reason, started_at FROM runs
		WHERE item_id = ? AND stage = ? ORDER BY id DESC LIMIT 1`, itemID, stage).
		Scan(&attempt, &status, &reason, &started)
	if errors.Is(err, sql.ErrNoRows) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	if status == RunFailed && (!started.Valid || reason == ReasonCredits) {
		return attempt, nil
	}
	var top int
	err = tx.QueryRowContext(ctx, `SELECT MAX(attempt) FROM runs WHERE item_id = ? AND stage = ?`,
		itemID, stage).Scan(&top)
	return top + 1, err
}

// Attempts is how many attempts an item has used at a stage: the
// DISTINCT attempt numbers over its runs, so a same-attempt relaunch is
// not counted twice. Build attempts and review rounds are capped on it.
func (s *Store) Attempts(ctx context.Context, itemID int64, stage string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT attempt) FROM runs WHERE item_id = ? AND stage = ?`, itemID, stage).Scan(&n)
	return n, err
}

// SetRunMachine records the Machine a launch created.
func (s *Store) SetRunMachine(ctx context.Context, runID int64, machineID, region string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET machine_id = ?, machine_region = ?
		WHERE id = ? AND status IN ('launching', 'running')`, machineID, region, runID)
	return staleIfNone(res, err)
}

// MarkRunRunning moves a launching run to running. From here on the run
// uses its attempt up however it ends, unless the API key runs out.
func (s *Store) MarkRunRunning(ctx context.Context, runID int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET status = 'running', started_at = ?
		WHERE id = ? AND status = 'launching'`, ts(at), runID)
	return staleIfNone(res, err)
}

// RunEnd is how a run ended.
type RunEnd struct {
	Status      string // finished, failed, cancelled, timed_out
	Reason      string
	ExitCode    *int
	DurationMS  int64
	VerdictJSON string // "" is NULL
	LogTail     string
	// Callback marks this as the worker's own verdict post: it is accepted
	// only while the run is running, and stamps callback_at.
	Callback bool
}

// EndRun closes an open run. It returns ErrStale if the run is already
// closed, or for a Callback, if it is not running, so a second verdict post
// changes nothing.
func (s *Store) EndRun(ctx context.Context, runID int64, e RunEnd, at time.Time) error {
	return s.inTx(ctx, func(tx *sql.Tx) error { return endRun(ctx, tx, runID, e, at) })
}

func endRun(ctx context.Context, tx *sql.Tx, runID int64, e RunEnd, at time.Time) error {
	switch e.Status {
	case RunFinished, RunFailed, RunCancelled, RunTimedOut:
	default:
		return fmt.Errorf("end run: %q is not a final status", e.Status)
	}
	open := `status IN ('launching', 'running')`
	var callback any
	if e.Callback {
		open, callback = `status = 'running'`, ts(at)
	}
	var exit any
	if e.ExitCode != nil {
		exit = *e.ExitCode
	}
	res, err := tx.ExecContext(ctx, `UPDATE runs SET status = ?, reason = ?, exit_code = ?,
		duration_ms = ?, verdict_json = ?, log_tail = ?, callback_at = COALESCE(?, callback_at)
		WHERE id = ? AND `+open,
		e.Status, e.Reason, exit, e.DurationMS, nullStr(e.VerdictJSON), e.LogTail, callback, runID)
	return staleIfNone(res, err)
}

// MarkRunDestroyed records that the run's Machine is gone.
func (s *Store) MarkRunDestroyed(ctx context.Context, runID int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET destroyed_at = ? WHERE id = ? AND destroyed_at IS NULL`, ts(at), runID)
	return staleIfNone(res, err)
}

// AddRunUsage adds what the Claude proxy saw on one response to the run and
// to the month's spend for the run's repo and credential, in one
// transaction.
func (s *Store) AddRunUsage(ctx context.Context, runID int64, usd float64, inTok, outTok int64, at time.Time) error {
	if !(usd >= 0) || math.IsInf(usd, 0) || inTok < 0 || outTok < 0 {
		return fmt.Errorf("run usage: want non-negative finite values, got %v, %d, %d", usd, inTok, outTok)
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var itemID int64
		var cred string
		err := tx.QueryRowContext(ctx, `UPDATE runs SET cost_usd = cost_usd + ?,
			input_tokens = input_tokens + ?, output_tokens = output_tokens + ?
			WHERE id = ? RETURNING item_id, credential`, usd, inTok, outTok, runID).Scan(&itemID, &cred)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return addSpend(ctx, tx, itemID, cred, at, usd, 0)
	})
}

func addSpend(ctx context.Context, tx *sql.Tx, itemID int64, cred string, at time.Time, usd float64, runs int) error {
	res, err := tx.ExecContext(ctx, `INSERT INTO spend (month, repo_id, credential, usd, runs)
		SELECT ?, repo_id, ?, ?, ? FROM items WHERE id = ?
		ON CONFLICT (month, repo_id, credential) DO UPDATE SET
			usd = usd + excluded.usd, runs = runs + excluded.runs`,
		month(at), cred, usd, runs, itemID)
	return oneRow(res, err)
}

const runCols = `id, item_id, stage, attempt, status, reason, machine_id, machine_region, image, model,
	credential, prompt, schema_name, ref, token_hash, created_at, deadline_at, started_at, callback_at,
	destroyed_at, exit_code, cost_usd, input_tokens, output_tokens, duration_ms, verdict_json, log_tail`

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var created, deadline string
	var started, callback, destroyed, verdict sql.NullString
	var exit sql.NullInt64
	if err := row.Scan(&r.ID, &r.ItemID, &r.Stage, &r.Attempt, &r.Status, &r.Reason, &r.MachineID,
		&r.MachineRegion, &r.Image, &r.Model, &r.Credential, &r.Prompt, &r.SchemaName, &r.Ref,
		&r.TokenHash, &created, &deadline, &started, &callback, &destroyed, &exit, &r.CostUSD,
		&r.InputTokens, &r.OutputTokens, &r.DurationMS, &verdict, &r.LogTail); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r, ErrNotFound
		}
		return r, err
	}
	if exit.Valid {
		n := int(exit.Int64)
		r.ExitCode = &n
	}
	r.VerdictJSON = verdict.String
	var err error
	for _, f := range []struct {
		dst *time.Time
		src sql.NullString
	}{
		{&r.CreatedAt, sql.NullString{String: created, Valid: true}},
		{&r.DeadlineAt, sql.NullString{String: deadline, Valid: true}},
		{&r.StartedAt, started}, {&r.CallbackAt, callback}, {&r.DestroyedAt, destroyed},
	} {
		if *f.dst, err = scanTS(f.src); err != nil {
			return r, err
		}
	}
	return r, nil
}

// RunByID returns one run.
func (s *Store) RunByID(ctx context.Context, id int64) (Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE id = ?`, id))
}

// RunByToken finds the run a worker's token belongs to. The token is
// hashed here; only the hash is stored.
func (s *Store) RunByToken(ctx context.Context, token string) (Run, error) {
	return scanRun(s.db.QueryRowContext(ctx,
		`SELECT `+runCols+` FROM runs WHERE token_hash = ?`, HashToken(token)))
}

// OpenRun returns the item's launching or running run, or ErrNotFound.
// There is at most one (runs_one_open).
func (s *Store) OpenRun(ctx context.Context, itemID int64) (Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs
		WHERE item_id = ? AND status IN ('launching', 'running')`, itemID))
}

// LatestVerdict returns the verdict JSON of the item's most recent
// finished run at a stage, or ErrNotFound. The loop reads the reviewed
// head from the last review this way.
func (s *Store) LatestVerdict(ctx context.Context, itemID int64, stage string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT verdict_json FROM runs
		WHERE item_id = ? AND stage = ? AND status = 'finished' AND verdict_json IS NOT NULL
		ORDER BY id DESC LIMIT 1`, itemID, stage).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// OpenRuns returns every launching or running run, oldest first. The
// watcher resumes these on boot.
func (s *Store) OpenRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runCols+` FROM runs WHERE status IN ('launching', 'running') ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Evidence is one file a validate run returned.
type Evidence struct {
	ID          int64
	RunID       int64
	Path        string
	ContentType string // as the worker claimed it; never trust it for serving
	Bytes       int64
	SHA256      string
	Content     []byte // empty from EvidenceForRun
	CreatedAt   time.Time
}

// InsertEvidence stores a run's evidence files in one transaction. Size
// and hash are computed here from the content.
func (s *Store) InsertEvidence(ctx context.Context, runID int64, files []Evidence, at time.Time) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, f := range files {
			sum := sha256.Sum256(f.Content)
			content := f.Content
			if content == nil {
				content = []byte{}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO evidence
				(run_id, path, content_type, bytes, sha256, content, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				runID, f.Path, f.ContentType, len(f.Content), hex.EncodeToString(sum[:]), content, ts(at)); err != nil {
				return fmt.Errorf("evidence %q: %w", f.Path, err)
			}
		}
		return nil
	})
}

// EvidenceForRun lists a run's evidence without the content.
func (s *Store) EvidenceForRun(ctx context.Context, runID int64) ([]Evidence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, path, content_type, bytes, sha256, created_at
		FROM evidence WHERE run_id = ? ORDER BY path`, runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Evidence
	for rows.Next() {
		var e Evidence
		var at string
		if err := rows.Scan(&e.ID, &e.RunID, &e.Path, &e.ContentType, &e.Bytes, &e.SHA256, &at); err != nil {
			return nil, err
		}
		if e.CreatedAt, err = parseTS(at); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EvidenceContent returns one evidence file's bytes.
func (s *Store) EvidenceContent(ctx context.Context, id int64) ([]byte, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT content FROM evidence WHERE id = ?`, id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// Spend is one month's rollup for a repo and credential.
type Spend struct {
	Month      string // "2026-11"
	RepoID     int64
	Credential string
	USD        float64
	Runs       int
}

// SpendForMonth returns a UTC month's rollup rows, by repo and credential.
func (s *Store) SpendForMonth(ctx context.Context, t time.Time) ([]Spend, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT month, repo_id, credential, usd, runs FROM spend
		WHERE month = ? ORDER BY repo_id, credential`, month(t))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Spend
	for rows.Next() {
		var sp Spend
		if err := rows.Scan(&sp.Month, &sp.RepoID, &sp.Credential, &sp.USD, &sp.Runs); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

func staleIfNone(res sql.Result, err error) error {
	if err := oneRow(res, err); errors.Is(err, ErrNotFound) {
		return ErrStale
	} else if err != nil {
		return err
	}
	return nil
}

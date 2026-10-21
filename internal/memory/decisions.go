package memory

import (
	"context"
	"fmt"
	"time"
)

// Decision stages, outcomes, and Slack post kinds.
const (
	StageRule  = "rule"
	StageHaiku = "haiku"

	OutcomeOK      = "ok"
	OutcomeDropped = "dropped_by_rule"
	OutcomeFailed  = "triage_failed"

	PostGrade    = "grade"
	PostApproval = "approval"
)

// TriageVersion marks decisions made under the current triage vocabulary.
// Version 1 is everything before 0003_dispatch.sql (policy actions only).
const TriageVersion = 2

// Decision is one triage result: what chuck would have done about an event.
type Decision struct {
	ID           int64
	EventID      int64
	Stage        string
	Outcome      string
	Category     string
	WouldDo      string // a policy.yaml action, or "none"
	Permission   string // the action's policy tier when decided
	Target       string
	Confidence   float64
	Rationale    string // model's reason, rule name, or failure reason
	Task         string // a dispatch task, "none", or "" (rule/failed/pre-change)
	Ask          string
	RawOutput    string
	Model        string
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	PostKind     string // "", PostGrade, PostApproval
	SlackChannel string
	SlackTS      string
	CreatedAt    time.Time
}

// DecisionView is a decision with what a Slack post needs alongside it.
type DecisionView struct {
	Decision
	Repo             string
	Kind             string
	Grade            string // latest grade: "", "up", "down"
	ApprovalResponse string // "", "approve", "deny"
	ApprovalBy       string
	URL              string // the event's GitHub page, or "" when the payload has none
}

// RecordDecision stores d and marks its event triaged, in one transaction.
func (s *Store) RecordDecision(ctx context.Context, d Decision) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var postKind any
	if d.PostKind != "" {
		postKind = d.PostKind
	}
	at := ts(d.CreatedAt)
	res, err := tx.ExecContext(ctx, `INSERT INTO decisions
		(event_id, stage, outcome, category, would_do, permission, target, confidence, rationale,
		 task, ask, raw_output, model, input_tokens, output_tokens, cost_usd, post_kind, triage_version, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.EventID, d.Stage, d.Outcome, d.Category, d.WouldDo, d.Permission, d.Target, d.Confidence, d.Rationale,
		d.Task, d.Ask, d.RawOutput, d.Model, d.InputTokens, d.OutputTokens, d.CostUSD, postKind, TriageVersion, at)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE events SET triaged_at = ? WHERE id = ?`, at, d.EventID); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// FailAttempt records a failed triage attempt and when to try again. It
// returns the event's attempt count including this one.
func (s *Store) FailAttempt(ctx context.Context, eventID int64, next time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `UPDATE events SET attempts = attempts + 1, next_attempt_at = ?
		WHERE id = ? RETURNING attempts`, ts(next), eventID).Scan(&n)
	return n, err
}

// SpendSince sums model cost of decisions created at or after since.
func (s *Store) SpendSince(ctx context.Context, since time.Time) (float64, error) {
	var usd float64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd), 0) FROM decisions WHERE created_at >= ?`, ts(since)).Scan(&usd)
	return usd, err
}

// viewQuery's last column is the event's GitHub page. payload is stored as a
// BLOB, which SQLite's JSON functions first try to read as JSONB; the CAST
// makes them read it as the text JSON it is. pull_request comes before issue
// so review events link to the PR.
const viewQuery = `SELECT d.id, d.event_id, d.stage, d.outcome, d.category, d.would_do, d.permission,
	d.target, d.confidence, d.rationale, d.task, d.ask, d.raw_output, d.model, d.input_tokens, d.output_tokens,
	d.cost_usd, COALESCE(d.post_kind, ''), COALESCE(d.slack_channel, ''), COALESCE(d.slack_ts, ''),
	d.created_at, e.repo, e.kind,
	COALESCE((SELECT g.grade FROM grades g WHERE g.decision_id = d.id ORDER BY g.id DESC LIMIT 1), ''),
	COALESCE(a.response, ''), COALESCE(a.responder_slack_user, ''),
	COALESCE(CAST(e.payload AS TEXT) ->> '$.pull_request.html_url', CAST(e.payload AS TEXT) ->> '$.issue.html_url',
		CAST(e.payload AS TEXT) ->> '$.workflow_run.html_url', CAST(e.payload AS TEXT) ->> '$.compare', '')
	FROM decisions d
	JOIN events e ON e.id = d.event_id
	LEFT JOIN approvals a ON a.decision_id = d.id`

func scanView(sc scanner) (DecisionView, error) {
	var v DecisionView
	var created string
	err := sc.Scan(&v.ID, &v.EventID, &v.Stage, &v.Outcome, &v.Category, &v.WouldDo, &v.Permission,
		&v.Target, &v.Confidence, &v.Rationale, &v.Task, &v.Ask, &v.RawOutput, &v.Model, &v.InputTokens, &v.OutputTokens,
		&v.CostUSD, &v.PostKind, &v.SlackChannel, &v.SlackTS,
		&created, &v.Repo, &v.Kind, &v.Grade, &v.ApprovalResponse, &v.ApprovalBy, &v.URL)
	if err != nil {
		return DecisionView{}, err
	}
	v.CreatedAt, err = parseTS(created)
	return v, err
}

// DecisionView loads one decision with its event, latest grade, and approval.
func (s *Store) DecisionView(ctx context.Context, id int64) (DecisionView, error) {
	return scanView(s.db.QueryRowContext(ctx, viewQuery+` WHERE d.id = ?`, id))
}

// DecisionForEvent loads the decision recorded for an event.
func (s *Store) DecisionForEvent(ctx context.Context, eventID int64) (DecisionView, error) {
	return scanView(s.db.QueryRowContext(ctx, viewQuery+` WHERE d.event_id = ? ORDER BY d.id DESC LIMIT 1`, eventID))
}

func (s *Store) views(ctx context.Context, where string, args ...any) ([]DecisionView, error) {
	rows, err := s.db.QueryContext(ctx, viewQuery+" "+where, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DecisionView
	for rows.Next() {
		v, err := scanView(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PendingPosts returns decisions due a Slack post that has not been sent,
// oldest first.
func (s *Store) PendingPosts(ctx context.Context, limit int) ([]DecisionView, error) {
	return s.views(ctx, `WHERE d.post_kind IS NOT NULL AND d.slack_posted_at IS NULL ORDER BY d.id LIMIT ?`, limit)
}

// MarkPosted records where a decision's Slack post landed and appends the
// sent message to the audit trail, in one transaction: a message row must
// never exist without the decision reflecting where it was posted, or vice
// versa. It returns an error, with no message row inserted, if the decision
// does not exist (the UPDATE affects 0 rows).
func (s *Store) MarkPosted(ctx context.Context, id int64, m Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `UPDATE decisions SET slack_channel = ?, slack_ts = ?, slack_posted_at = ?
		WHERE id = ?`, m.Channel, m.TS, ts(m.CreatedAt), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("MarkPosted: decision %d not found", id)
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO messages
		(slack_channel, slack_ts, slack_user, direction, kind, text, allowed, changed_state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.Channel, m.TS, m.User, m.Direction, m.Kind, m.Text, m.Allowed, m.ChangedState, ts(m.CreatedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

// QueueGradePosts asks for grade posts on decisions that have no post yet.
func (s *Store) QueueGradePosts(ctx context.Context, ids []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE decisions SET post_kind = ? WHERE id = ? AND post_kind IS NULL`, PostGrade, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SampleQuiet picks up to n random decisions in [since, until) where chuck
// would do nothing (dropped by a rule, or would_do "none" with no task
// dispatched) and that have no post yet.
func (s *Store) SampleQuiet(ctx context.Context, since, until time.Time, n int) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM decisions
		WHERE created_at >= ? AND created_at < ? AND post_kind IS NULL
		  AND (outcome = ? OR (outcome = ? AND would_do = 'none' AND task IN ('', 'none')))
		ORDER BY random() LIMIT ?`, ts(since), ts(until), OutcomeDropped, OutcomeOK, n)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

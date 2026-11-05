package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *Store {
	t.Helper()
	s := openTemp(t, filepath.Join(t.TempDir(), "state.db"))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedRepo(t *testing.T, s *Store) int64 {
	t.Helper()
	id, err := s.UpsertRepo(context.Background(), Repo{Owner: "showmethebots", Name: "demos", InstallationID: 7, AppOwner: "showmethebots", DefaultBranch: "main", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func seedIssue(t *testing.T, s *Store, repoID int64, number int, state string) int64 {
	t.Helper()
	c, err := s.Commit(context.Background(), t0, Change{
		New: &Item{RepoID: repoID, Kind: KindIssue, Number: number, Title: "t"},
		To:  state, InputKind: "dashboard", Actor: "cwinters8",
	})
	if err != nil {
		t.Fatal(err)
	}
	return c[0].ItemID
}

// Production has 0001 to 0003 applied, with shadow-mode rows. Opening it
// must apply 0004, keep boots, and drop the shadow tables with their rows.
func TestOpenUpgradesPhase1Database(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`}
	for _, m := range migrations[:3] {
		b, err := migrationFS.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		stmts = append(stmts, string(b), fmt.Sprintf(`INSERT INTO schema_migrations VALUES ('%s', '2026-10-01T00:00:00Z')`, m))
	}
	stmts = append(stmts,
		`INSERT INTO boots (version, started_at) VALUES ('v1', '2026-10-01T00:00:00Z')`,
		`INSERT INTO events (source, kind, payload, received_at, next_attempt_at) VALUES ('github', 'push', '{}', 'x', 'x')`,
		`INSERT INTO decisions (event_id, stage, outcome, created_at) VALUES (1, 'rule', 'ok', 'x')`,
		`INSERT INTO grades (decision_id, grade, slack_user, graded_at) VALUES (1, 'up', 'u', 'x')`,
		`INSERT INTO approvals (decision_id, request) VALUES (1, 'r')`,
	)
	for _, st := range stmts {
		if _, err := raw.ExecContext(ctx, st); err != nil {
			t.Fatalf("%.60s: %v", st, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s := openTemp(t, path)
	defer func() { _ = s.Close() }()
	var boots int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM boots`).Scan(&boots); err != nil || boots != 1 {
		t.Fatalf("boots = %d, %v; want 1", boots, err)
	}
	for _, gone := range []string{"events", "decisions", "grades", "approvals", "messages", "control"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, gone).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s still exists", gone)
		}
	}
	for _, table := range []string{"repos", "items", "transitions", "runs", "evidence", "webhooks", "sessions", "spend", "settings"} {
		if _, err := s.db.Exec(`SELECT * FROM ` + table + ` LIMIT 0`); err != nil {
			t.Errorf("%s: %v", table, err)
		}
	}
	var fk int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&fk); err != nil || fk != 0 {
		t.Errorf("foreign key violations = %d, %v", fk, err)
	}
}

func TestCommitWritesStateAndTransitionTogether(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	issue := seedIssue(t, s, repo, 12, "ready")

	later := t0.Add(time.Hour)
	c, err := s.Commit(ctx, later,
		Change{ItemID: issue, From: "ready", To: "in-pr", InputKind: "verdict", InputRef: "run:1", Actor: "chuck", Effects: `[{"kind":"SetLabel"}]`},
		Change{New: &Item{RepoID: repo, Kind: KindPR, Number: 13, IssueNumber: 12}, To: "review", PendingStage: "review", InputKind: "verdict", Actor: "chuck"},
	)
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.ItemByID(ctx, issue)
	if err != nil || it.State != "in-pr" || !it.StateSince.Equal(later) {
		t.Fatalf("issue = %+v, %v", it, err)
	}
	pr, err := s.ItemByNumber(ctx, repo, KindPR, 13)
	if err != nil || pr.State != "review" || pr.IssueNumber != 12 || pr.PendingStage != "review" {
		t.Fatalf("pr = %+v, %v", pr, err)
	}
	hist, err := s.Transitions(ctx, issue)
	if err != nil || len(hist) != 2 || hist[0].From != "" || hist[1].From != "ready" || hist[1].To != "in-pr" {
		t.Fatalf("history = %+v, %v", hist, err)
	}
	if err := s.RecordEffects(ctx, c[0].TransitionID, `[{"kind":"SetLabel","result":"ok"}]`); err != nil {
		t.Fatal(err)
	}
	hist, _ = s.Transitions(ctx, issue)
	if hist[1].Effects != `[{"kind":"SetLabel","result":"ok"}]` {
		t.Errorf("effects = %s", hist[1].Effects)
	}
}

// A failing write anywhere in a Commit leaves neither the state nor any
// transition row behind.
func TestCommitRollsBackTogether(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	issue := seedIssue(t, s, repo, 12, "ready")

	for name, bad := range map[string]Change{
		"bad transition row": {ItemID: issue, From: "ready", To: "building", InputKind: "carrier-pigeon", Actor: "chuck"},
		"bad state":          {ItemID: issue, From: "ready", To: "nap", InputKind: "tick", Actor: "chuck"},
		"stale from":         {ItemID: issue, From: "review", To: "sweep", InputKind: "tick", Actor: "chuck"},
		"bad second change":  {New: &Item{RepoID: repo, Kind: KindIssue, Number: 12}, To: "intake", InputKind: "tick", Actor: "chuck"}, // duplicate number
	} {
		changes := []Change{{ItemID: issue, From: "ready", To: "building", InputKind: "tick", Actor: "chuck"}, bad}
		if name != "bad second change" {
			changes = []Change{bad}
		}
		if _, err := s.Commit(ctx, t0.Add(time.Minute), changes...); err == nil {
			t.Errorf("%s: committed", name)
		}
		it, _ := s.ItemByID(ctx, issue)
		hist, _ := s.Transitions(ctx, issue)
		if it.State != "ready" || len(hist) != 1 {
			t.Errorf("%s: state %s, %d transitions; want ready and 1", name, it.State, len(hist))
		}
	}
	_, err := s.Commit(ctx, t0, Change{ItemID: issue, From: "building", To: "review", InputKind: "tick", Actor: "chuck"})
	if !errors.Is(err, ErrStale) {
		t.Errorf("stale err = %v, want ErrStale", err)
	}
}

// The stale clock measures time in a state, so a self-transition (a
// relaunch in review) must not reset it.
func TestStateSinceMovesOnlyWithTheState(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	issue := seedIssue(t, s, seedRepo(t, s), 1, "review")
	if _, err := s.Commit(ctx, t0.Add(time.Hour), Change{ItemID: issue, From: "review", To: "review", InputKind: "webhook", Actor: "chuck"}); err != nil {
		t.Fatal(err)
	}
	if it, _ := s.ItemByID(ctx, issue); !it.StateSince.Equal(t0) {
		t.Errorf("state_since = %v, want %v", it.StateSince, t0)
	}
}

func newRun(item int64, stage, token string) Run {
	return Run{ItemID: item, Stage: stage, Image: "img", Model: "claude-x", Credential: CredentialAPIKey,
		Prompt: "p", SchemaName: stage, Ref: "main", TokenHash: HashToken(token), DeadlineAt: t0.Add(time.Hour)}
}

func TestAttemptsCountDistinctAndRelaunchKeepsAttempt(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	item := seedIssue(t, s, seedRepo(t, s), 1, "building")
	launch := func(token string) Run {
		t.Helper()
		r, err := s.InsertRun(ctx, newRun(item, "build", token), t0)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	end := func(r Run, status, reason string) {
		t.Helper()
		if err := s.EndRun(ctx, r.ID, RunEnd{Status: status, Reason: reason}, t0); err != nil {
			t.Fatal(err)
		}
	}

	r1 := launch("a") // Machine never starts
	end(r1, RunFailed, "machine_gone")
	r2 := launch("b")
	if r2.Attempt != 1 {
		t.Fatalf("relaunch after a Machine that never started: attempt %d, want 1", r2.Attempt)
	}
	if err := s.MarkRunRunning(ctx, r2.ID, t0); err != nil {
		t.Fatal(err)
	}
	end(r2, RunFailed, ReasonCredits) // ran, then the API key ran out
	r3 := launch("c")
	if r3.Attempt != 1 {
		t.Fatalf("relaunch after credits: attempt %d, want 1", r3.Attempt)
	}
	if err := s.MarkRunRunning(ctx, r3.ID, t0); err != nil {
		t.Fatal(err)
	}
	end(r3, RunFinished, "")
	r4 := launch("d")
	if r4.Attempt != 2 {
		t.Fatalf("after a finished run: attempt %d, want 2", r4.Attempt)
	}
	if n, err := s.Attempts(ctx, item, "build"); err != nil || n != 2 {
		t.Fatalf("Attempts = %d, %v; want 2 over 4 runs", n, err)
	}
	if n, _ := s.Attempts(ctx, item, "review"); n != 0 {
		t.Errorf("review attempts = %d", n)
	}
	// A run that reached running and then failed uses its attempt up.
	if err := s.MarkRunRunning(ctx, r4.ID, t0); err != nil {
		t.Fatal(err)
	}
	end(r4, RunFailed, "exit 1")
	if r5 := launch("e"); r5.Attempt != 3 {
		t.Errorf("after a running failure: attempt %d, want 3", r5.Attempt)
	}
}

func TestOneOpenRunPerItem(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	item := seedIssue(t, s, seedRepo(t, s), 1, "building")
	if _, err := s.InsertRun(ctx, newRun(item, "build", "a"), t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertRun(ctx, newRun(item, "build", "b"), t0); err == nil {
		t.Fatal("second open run accepted")
	}
	open, err := s.OpenRuns(ctx)
	if err != nil || len(open) != 1 {
		t.Fatalf("open runs = %d, %v", len(open), err)
	}
}

// The verdict endpoint accepts one post per run, only while running.
func TestVerdictOnlyOnceAndOnlyWhileRunning(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	item := seedIssue(t, s, seedRepo(t, s), 1, "building")
	r, _ := s.InsertRun(ctx, newRun(item, "build", "tok"), t0)
	v := RunEnd{Status: RunFinished, VerdictJSON: `{"verdict":"pr_opened"}`, Callback: true}
	if err := s.EndRun(ctx, r.ID, v, t0); !errors.Is(err, ErrStale) {
		t.Fatalf("verdict while launching: %v, want ErrStale", err)
	}
	_ = s.MarkRunRunning(ctx, r.ID, t0)
	if err := s.EndRun(ctx, r.ID, v, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.EndRun(ctx, r.ID, v, t0); !errors.Is(err, ErrStale) {
		t.Fatalf("second verdict: %v, want ErrStale", err)
	}
	got, err := s.RunByToken(ctx, "tok")
	if err != nil || got.Status != RunFinished || got.VerdictJSON != v.VerdictJSON || !got.CallbackAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("run = %+v, %v", got, err)
	}
	if _, err := s.RunByToken(ctx, HashToken("tok")); !errors.Is(err, ErrNotFound) {
		t.Error("the stored hash works as a token")
	}
	if err := s.EndRun(ctx, r.ID, RunEnd{Status: RunLaunching}, t0); err == nil {
		t.Error("EndRun accepted a non-final status")
	}
}

func TestSpendRollsUpByMonthRepoAndCredential(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	a := seedIssue(t, s, repo, 1, "building")
	b := seedIssue(t, s, repo, 2, "building")

	ra, _ := s.InsertRun(ctx, newRun(a, "build", "a"), t0)
	rb := newRun(b, "build", "b")
	rb.Credential = CredentialOAuth
	rb, _ = s.InsertRun(ctx, rb, t0)
	for _, u := range []struct {
		run int64
		usd float64
		at  time.Time
	}{{ra.ID, 1.25, t0}, {ra.ID, 0.75, t0}, {rb.ID, 3, t0}, {ra.ID, 5, t0.AddDate(0, 1, 0)}} {
		if err := s.AddRunUsage(ctx, u.run, u.usd, 100, 10, u.at); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.SpendForMonth(ctx, t0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if rows[0].Credential != CredentialAPIKey || rows[0].USD != 2 || rows[0].Runs != 1 ||
		rows[1].Credential != CredentialOAuth || rows[1].USD != 3 || rows[1].Runs != 1 {
		t.Errorf("rows = %+v", rows)
	}
	next, _ := s.SpendForMonth(ctx, t0.AddDate(0, 1, 0))
	if len(next) != 1 || next[0].USD != 5 || next[0].Runs != 0 {
		t.Errorf("next month = %+v", next)
	}
	got, _ := s.RunByID(ctx, ra.ID)
	if got.CostUSD != 7 || got.InputTokens != 300 || got.OutputTokens != 30 {
		t.Errorf("run usage = %v %d %d", got.CostUSD, got.InputTokens, got.OutputTokens)
	}
	if err := s.AddRunUsage(ctx, ra.ID, -1, 0, 0, t0); err == nil {
		t.Error("negative usage accepted")
	}
}

func TestEvidence(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	item := seedIssue(t, s, seedRepo(t, s), 1, "validating")
	r, _ := s.InsertRun(ctx, newRun(item, "validate", "a"), t0)
	if err := s.InsertEvidence(ctx, r.ID, []Evidence{
		{Path: "shot.png", ContentType: "image/png", Content: []byte("png")},
		{Path: "log.txt", ContentType: "text/plain", Content: []byte("hello")},
	}, t0); err != nil {
		t.Fatal(err)
	}
	ev, err := s.EvidenceForRun(ctx, r.ID)
	if err != nil || len(ev) != 2 || ev[0].Path != "log.txt" || ev[0].Bytes != 5 || ev[0].SHA256 != HashToken("hello") {
		t.Fatalf("evidence = %+v, %v", ev, err)
	}
	b, err := s.EvidenceContent(ctx, ev[1].ID)
	if err != nil || string(b) != "png" {
		t.Fatalf("content = %q, %v", b, err)
	}
	// One bad file stores none.
	if err := s.InsertEvidence(ctx, r.ID, []Evidence{{Path: "new.txt"}, {Path: "log.txt"}}, t0); err == nil {
		t.Fatal("duplicate path accepted")
	}
	if ev, _ := s.EvidenceForRun(ctx, r.ID); len(ev) != 2 {
		t.Errorf("partial insert left %d files", len(ev))
	}
}

func TestWebhookDedupeHandledAndRetention(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	w := Webhook{DeliveryID: "d-1", Event: "issues", Action: "labeled", Repo: "o/r", SenderID: 5, Payload: []byte(`{}`), ReceivedAt: t0}
	id, inserted, err := s.InsertWebhook(ctx, w)
	if err != nil || !inserted || id == 0 {
		t.Fatalf("insert: %d %v %v", id, inserted, err)
	}
	if _, inserted, err := s.InsertWebhook(ctx, w); err != nil || inserted {
		t.Fatalf("redelivery: inserted=%v err=%v", inserted, err)
	}
	old := w
	old.DeliveryID, old.ReceivedAt = "d-0", t0.Add(-31*24*time.Hour)
	if _, _, err := s.InsertWebhook(ctx, old); err != nil {
		t.Fatal(err)
	}
	un, err := s.UnhandledWebhooks(ctx, 10)
	if err != nil || len(un) != 2 {
		t.Fatalf("unhandled = %d, %v", len(un), err)
	}
	if err := s.MarkWebhookHandled(ctx, id, "no transition", t0); err != nil {
		t.Fatal(err)
	}
	if un, _ := s.UnhandledWebhooks(ctx, 10); len(un) != 1 || un[0].DeliveryID != "d-0" {
		t.Errorf("unhandled after marking = %+v", un)
	}
	n, err := s.DeleteWebhooksBefore(ctx, t0.Add(-720*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("retention deleted %d, %v; want 1", n, err)
	}
	if _, inserted, _ := s.InsertWebhook(ctx, w); inserted {
		t.Error("retention removed a row inside the window")
	}
}

func TestSessionsSlideRevokeAndExpire(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	week := 7 * 24 * time.Hour
	if _, err := s.CreateSession(ctx, "cookie", Session{UserID: 40615752, Login: "cwinters8", CSRF: "c"}, t0, week); err != nil {
		t.Fatal(err)
	}
	// Six days later it is live and slides to seven days from then.
	got, err := s.TouchSession(ctx, "cookie", t0.Add(6*24*time.Hour), week)
	if err != nil || got.UserID != 40615752 || !got.ExpiresAt.Equal(t0.Add(13*24*time.Hour)) {
		t.Fatalf("touch = %+v, %v", got, err)
	}
	if _, err := s.TouchSession(ctx, "cookie", t0.Add(13*24*time.Hour+time.Second), week); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired session: %v, want ErrNotFound", err)
	}
	if _, err := s.TouchSession(ctx, "forged", t0, week); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session: %v", err)
	}
	if _, err := s.TouchSession(ctx, HashToken("cookie"), t0, week); !errors.Is(err, ErrNotFound) {
		t.Error("the stored hash works as a cookie")
	}

	if _, err := s.CreateSession(ctx, "other", Session{UserID: 1, Login: "x", CSRF: "c"}, t0, week); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSession(ctx, "other", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TouchSession(ctx, "other", t0.Add(2*time.Minute), week); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked session: %v", err)
	}
	n, err := s.DeleteExpiredSessions(ctx, t0.Add(14*24*time.Hour))
	if err != nil || n != 2 {
		t.Errorf("deleted %d, %v; want 2", n, err)
	}
}

func TestSettings(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if until, err := s.APIKeyExhaustedUntil(ctx); err != nil || !until.IsZero() {
		t.Fatalf("unset = %v, %v", until, err)
	}
	dec1 := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	if err := s.SetAPIKeyExhaustedUntil(ctx, dec1, t0); err != nil {
		t.Fatal(err)
	}
	if until, err := s.APIKeyExhaustedUntil(ctx); err != nil || !until.Equal(dec1) {
		t.Fatalf("until = %v, %v", until, err)
	}
	if err := s.DeleteSetting(ctx, SettingAPIKeyExhaustedUntil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Setting(ctx, SettingAPIKeyExhaustedUntil); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
}

func TestReposAreCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	id := seedRepo(t, s)
	again, err := s.UpsertRepo(ctx, Repo{Owner: "ShowMeTheBots", Name: "Demos", InstallationID: 8, AppOwner: "showmethebots"})
	if err != nil || again != id {
		t.Fatalf("upsert = %d, %v; want %d", again, err, id)
	}
	r, err := s.RepoByName(ctx, "SHOWMETHEBOTS", "demos")
	if err != nil || r.InstallationID != 8 {
		t.Fatalf("repo = %+v, %v", r, err)
	}
	if err := s.SetRepoConfig(ctx, id, `{}`, "abc", t0); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Repos(ctx)
	if len(all) != 1 || all[0].ConfigSHA != "abc" || !all[0].ConfigReadAt.Equal(t0) {
		t.Errorf("repos = %+v", all)
	}
}

// The schema enforces every enum-like column, as 0002 did.
func TestLoopEnumsAreChecked(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	item := seedIssue(t, s, repo, 1, "building")
	for name, stmt := range map[string]string{
		"item kind":             fmt.Sprintf(`INSERT INTO items (repo_id, kind, number, state, state_since, created_at, updated_at) VALUES (%d, 'discussion', 9, 'intake', 'x', 'x', 'x')`, repo),
		"issue_number on issue": fmt.Sprintf(`UPDATE items SET issue_number = 3 WHERE id = %d`, item),
		"escalated_from":        fmt.Sprintf(`UPDATE items SET escalated_from = 'needs-human' WHERE id = %d`, item),
		"pending_stage":         fmt.Sprintf(`UPDATE items SET pending_stage = 'deploy' WHERE id = %d`, item),
		"effects json":          fmt.Sprintf(`INSERT INTO transitions (item_id, to_state, input_kind, actor, effects_json, created_at) VALUES (%d, 'x', 'tick', 'chuck', 'not json', 'x')`, item),
		"run status":            fmt.Sprintf(`INSERT INTO runs (item_id, stage, attempt, status, image, model, credential, prompt, schema_name, ref, token_hash, created_at, deadline_at) VALUES (%d, 'build', 1, 'paused', 'i', 'm', 'api_key', 'p', 's', 'r', '%s', 'x', 'x')`, item, HashToken("z")),
		"credential":            fmt.Sprintf(`INSERT INTO spend (month, repo_id, credential) VALUES ('2026-11', %d, 'cash')`, repo),
		"negative spend":        fmt.Sprintf(`INSERT INTO spend (month, repo_id, credential, usd) VALUES ('2026-11', %d, 'oauth', -1)`, repo),
		"raw session id":        `INSERT INTO sessions (id, user_id, login, csrf, created_at, expires_at) VALUES ('cookie', 1, 'x', 'c', 'x', 'x')`,
		"empty delivery":        `INSERT INTO webhooks (delivery_id, event, payload, received_at) VALUES ('', 'push', '{}', 'x')`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

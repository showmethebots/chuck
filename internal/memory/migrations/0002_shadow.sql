-- Phase 1, shadow mode: raw events (the triage queue), the decision log,
-- grades, approvals, Slack messages, and the pause switch. Timestamps are
-- fixed-width UTC text ("2006-01-02T15:04:05.000000Z") so they sort as text.
CREATE TABLE events (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	source          TEXT NOT NULL CHECK (source IN ('github', 'cron')),
	kind            TEXT NOT NULL,
	delivery_id     TEXT UNIQUE,
	repo            TEXT NOT NULL DEFAULT '',
	headers         TEXT NOT NULL DEFAULT '{}',
	payload         BLOB NOT NULL,
	received_at     TEXT NOT NULL,
	attempts        INTEGER NOT NULL DEFAULT 0,
	next_attempt_at TEXT NOT NULL,
	triaged_at      TEXT
);
CREATE INDEX events_queue ON events (triaged_at, id);

CREATE TABLE decisions (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	event_id        INTEGER NOT NULL REFERENCES events (id),
	stage           TEXT NOT NULL CHECK (stage IN ('rule', 'haiku')),
	outcome         TEXT NOT NULL CHECK (outcome IN ('ok', 'dropped_by_rule', 'triage_failed')),
	category        TEXT NOT NULL DEFAULT '',
	would_do        TEXT NOT NULL DEFAULT '',
	permission      TEXT NOT NULL DEFAULT '',
	target          TEXT NOT NULL DEFAULT '',
	confidence      REAL NOT NULL DEFAULT 0,
	rationale       TEXT NOT NULL DEFAULT '',
	raw_output      TEXT NOT NULL DEFAULT '',
	model           TEXT NOT NULL DEFAULT '',
	input_tokens    INTEGER NOT NULL DEFAULT 0,
	output_tokens   INTEGER NOT NULL DEFAULT 0,
	cost_usd        REAL NOT NULL DEFAULT 0,
	post_kind       TEXT CHECK (post_kind IN ('grade', 'approval')),
	slack_channel   TEXT,
	slack_ts        TEXT,
	slack_posted_at TEXT,
	created_at      TEXT NOT NULL
);
CREATE INDEX decisions_created ON decisions (created_at);
CREATE INDEX decisions_event ON decisions (event_id);

CREATE TABLE grades (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	decision_id INTEGER NOT NULL REFERENCES decisions (id),
	grade       TEXT NOT NULL CHECK (grade IN ('up', 'down')),
	slack_user  TEXT NOT NULL,
	graded_at   TEXT NOT NULL
);
CREATE INDEX grades_decision ON grades (decision_id, id);

CREATE TABLE approvals (
	id                   INTEGER PRIMARY KEY AUTOINCREMENT,
	decision_id          INTEGER NOT NULL UNIQUE REFERENCES decisions (id),
	request              TEXT NOT NULL,
	response             TEXT CHECK (response IN ('approve', 'deny')),
	responder_slack_user TEXT,
	responded_at         TEXT
);

CREATE TABLE messages (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	slack_channel TEXT NOT NULL DEFAULT '',
	slack_ts      TEXT NOT NULL DEFAULT '',
	slack_user    TEXT NOT NULL DEFAULT '',
	direction     TEXT NOT NULL CHECK (direction IN ('in', 'out')),
	kind          TEXT NOT NULL CHECK (kind IN ('command', 'button', 'post')),
	text          TEXT NOT NULL,
	allowed       INTEGER NOT NULL,
	changed_state INTEGER NOT NULL DEFAULT 0,
	created_at    TEXT NOT NULL
);

CREATE TABLE control (
	id            INTEGER PRIMARY KEY CHECK (id = 1),
	paused        INTEGER NOT NULL DEFAULT 0,
	reason        TEXT CHECK (reason IN ('pause', 'kill')),
	by_slack_user TEXT,
	changed_at    TEXT
);
INSERT INTO control (id, paused) VALUES (1, 0);

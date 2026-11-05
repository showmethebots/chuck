-- The delivery loop (docs/superpowers/specs/2026-11-03-delivery-loop-design.md,
-- "Data model"). Phase 1's shadow tables go; boots stays. Timestamps are
-- fixed-width UTC text ("2006-01-02T15:04:05.000000Z") so they sort as text.
--
-- runs and evidence are written by worker callbacks. A worker launched
-- before a deploy may post its verdict after it, so from here on migrations
-- only add to those two tables: new columns need a default, and nothing a
-- callback writes is renamed, retyped or dropped.

-- Children first: foreign keys are on.
DROP TABLE grades;
DROP TABLE approvals;
DROP TABLE decisions;
DROP TABLE messages;
DROP TABLE control;
DROP TABLE events;

CREATE TABLE repos (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	-- GitHub owner and repo names are case-insensitive.
	owner           TEXT NOT NULL COLLATE NOCASE CHECK (owner <> ''),
	name            TEXT NOT NULL COLLATE NOCASE CHECK (name <> ''),
	installation_id INTEGER NOT NULL CHECK (installation_id > 0),
	app_owner       TEXT NOT NULL CHECK (app_owner <> ''),
	default_branch  TEXT NOT NULL DEFAULT '',
	enabled         INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
	config_json     TEXT,
	config_sha      TEXT,
	config_read_at  TEXT,
	UNIQUE (owner, name)
);

CREATE TABLE items (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_id        INTEGER NOT NULL REFERENCES repos (id),
	kind           TEXT NOT NULL CHECK (kind IN ('issue', 'pr')),
	number         INTEGER NOT NULL CHECK (number > 0),
	title          TEXT NOT NULL DEFAULT '',
	-- For a PR: the issue number it serves.
	issue_number   INTEGER CHECK (issue_number IS NULL OR (kind = 'pr' AND issue_number > 0)),
	-- For a ratchet issue: the issue number it covers.
	ratchet_of     INTEGER CHECK (ratchet_of IS NULL OR (kind = 'issue' AND ratchet_of > 0)),
	state          TEXT NOT NULL CHECK (state IN (
		'intake', 'awaiting-approval', 'needs-human', 'ready', 'building', 'in-pr',
		'review', 'sweep', 'awaiting-merge', 'merging', 'merged', 'validating',
		'awaiting-prod-approval', 'promoting', 'done', 'closed')),
	state_since    TEXT NOT NULL,
	-- The state a needs-human item re-queues to.
	escalated_from TEXT CHECK (escalated_from IS NULL OR escalated_from IN (
		'intake', 'awaiting-approval', 'ready', 'building', 'in-pr',
		'review', 'sweep', 'awaiting-merge', 'merging', 'merged', 'validating',
		'awaiting-prod-approval', 'promoting')),
	pending_stage  TEXT CHECK (pending_stage IS NULL OR pending_stage IN (
		'intake', 'build', 'review', 'sweep', 'validate')),
	created_at     TEXT NOT NULL,
	updated_at     TEXT NOT NULL,
	UNIQUE (repo_id, kind, number)
);
CREATE INDEX items_state ON items (repo_id, state, state_since);

CREATE TABLE transitions (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	item_id      INTEGER NOT NULL REFERENCES items (id),
	-- NULL for the transition that created the item.
	from_state   TEXT,
	to_state     TEXT NOT NULL,
	input_kind   TEXT NOT NULL CHECK (input_kind IN ('webhook', 'verdict', 'dashboard', 'tick')),
	input_ref    TEXT NOT NULL DEFAULT '',
	reason       TEXT NOT NULL DEFAULT '',
	-- A GitHub login for dashboard actions, 'chuck' otherwise.
	actor        TEXT NOT NULL CHECK (actor <> ''),
	config_sha   TEXT NOT NULL DEFAULT '',
	-- The effect list, then each effect's result once the executor runs it.
	effects_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(effects_json)),
	created_at   TEXT NOT NULL
);
CREATE INDEX transitions_item ON transitions (item_id, id);

CREATE TABLE runs (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	item_id        INTEGER NOT NULL REFERENCES items (id),
	stage          TEXT NOT NULL CHECK (stage IN ('intake', 'build', 'review', 'sweep', 'validate')),
	attempt        INTEGER NOT NULL CHECK (attempt >= 1),
	status         TEXT NOT NULL CHECK (status IN (
		'launching', 'running', 'finished', 'failed', 'cancelled', 'timed_out')),
	-- Why a run ended other than finished: 'credits', 'timeout', 'machine_gone'...
	reason         TEXT NOT NULL DEFAULT '',
	machine_id     TEXT NOT NULL DEFAULT '',
	machine_region TEXT NOT NULL DEFAULT '',
	image          TEXT NOT NULL,
	model          TEXT NOT NULL,
	credential     TEXT NOT NULL CHECK (credential IN ('api_key', 'oauth')),
	prompt         TEXT NOT NULL,
	schema_name    TEXT NOT NULL,
	ref            TEXT NOT NULL,
	-- sha256 of the run token, hex. The token itself is never stored.
	token_hash     TEXT NOT NULL UNIQUE CHECK (length(token_hash) = 64),
	created_at     TEXT NOT NULL,
	deadline_at    TEXT NOT NULL,
	started_at     TEXT,
	callback_at    TEXT,
	destroyed_at   TEXT,
	exit_code      INTEGER,
	cost_usd       REAL NOT NULL DEFAULT 0,
	input_tokens   INTEGER NOT NULL DEFAULT 0,
	output_tokens  INTEGER NOT NULL DEFAULT 0,
	duration_ms    INTEGER NOT NULL DEFAULT 0,
	verdict_json   TEXT CHECK (verdict_json IS NULL OR json_valid(verdict_json)),
	log_tail       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX runs_item_stage ON runs (item_id, stage, attempt);
CREATE INDEX runs_status ON runs (status);
-- At most one open run per item: a stale page or a racing tick cannot
-- double-launch.
CREATE UNIQUE INDEX runs_one_open ON runs (item_id) WHERE status IN ('launching', 'running');

CREATE TABLE evidence (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id       INTEGER NOT NULL REFERENCES runs (id),
	path         TEXT NOT NULL CHECK (path <> ''),
	-- What the worker claimed. Never trusted for serving; the web layer sniffs.
	content_type TEXT NOT NULL DEFAULT '',
	bytes        INTEGER NOT NULL CHECK (bytes >= 0),
	sha256       TEXT NOT NULL CHECK (length(sha256) = 64),
	content      BLOB NOT NULL,
	created_at   TEXT NOT NULL,
	UNIQUE (run_id, path)
);

CREATE TABLE webhooks (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	delivery_id  TEXT NOT NULL UNIQUE CHECK (delivery_id <> ''),
	event        TEXT NOT NULL CHECK (event <> ''),
	action       TEXT NOT NULL DEFAULT '',
	repo         TEXT NOT NULL DEFAULT '',
	sender_id    INTEGER NOT NULL DEFAULT 0,
	sender_login TEXT NOT NULL DEFAULT '',
	payload      BLOB NOT NULL,
	received_at  TEXT NOT NULL,
	handled_at   TEXT,
	result       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX webhooks_received ON webhooks (received_at);
CREATE INDEX webhooks_unhandled ON webhooks (id) WHERE handled_at IS NULL;

CREATE TABLE sessions (
	-- sha256 of the cookie value, hex: a copy of the database holds no
	-- usable session.
	id         TEXT PRIMARY KEY CHECK (length(id) = 64),
	user_id    INTEGER NOT NULL CHECK (user_id > 0),
	login      TEXT NOT NULL,
	csrf       TEXT NOT NULL CHECK (csrf <> ''),
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	revoked_at TEXT
);
CREATE INDEX sessions_expires ON sessions (expires_at);

CREATE TABLE spend (
	month      TEXT NOT NULL CHECK (length(month) = 7), -- "2026-11", UTC
	repo_id    INTEGER NOT NULL REFERENCES repos (id),
	credential TEXT NOT NULL CHECK (credential IN ('api_key', 'oauth')),
	usd        REAL NOT NULL DEFAULT 0 CHECK (usd >= 0),
	runs       INTEGER NOT NULL DEFAULT 0 CHECK (runs >= 0),
	PRIMARY KEY (month, repo_id, credential)
);

CREATE TABLE settings (
	key        TEXT PRIMARY KEY CHECK (key <> ''),
	value      TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

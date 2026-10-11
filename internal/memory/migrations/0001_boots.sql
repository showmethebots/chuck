-- Phase 0: one row per daemon start. The count surviving a destroyed
-- Machine is the proof that Litestream restore works. Phase 1 adds the
-- goals, tasks, decisions, and approvals tables.
CREATE TABLE boots (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	version    TEXT NOT NULL,
	started_at TEXT NOT NULL
);

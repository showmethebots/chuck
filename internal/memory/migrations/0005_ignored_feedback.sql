-- Feedback the trusted-text filter kept out of a run's prompt: comments by
-- anyone not in humans and not chuck's App. The item page lists them with
-- their author, so a human can see what the loop did not act on. Only the
-- author and link are kept, never the body: the text is untrusted and
-- GitHub already holds it.
CREATE TABLE ignored_feedback (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id       INTEGER NOT NULL REFERENCES runs (id),
	author_id    INTEGER NOT NULL,
	author_login TEXT NOT NULL DEFAULT '',
	url          TEXT NOT NULL DEFAULT '',
	-- Why it was left out: 'untrusted_author' or 'chuck_mention'.
	reason       TEXT NOT NULL CHECK (reason <> ''),
	created_at   TEXT NOT NULL
);
CREATE INDEX ignored_feedback_run ON ignored_feedback (run_id);

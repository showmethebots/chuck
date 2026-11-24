-- The review stage's findings comment as chuck saw it when it accepted the
-- review verdict. Every worker's installation token can edit any comment
-- chuck's App wrote, so sweep trusts that comment only while its body still
-- hashes to what was recorded here. Only the hash is kept, never the body.
CREATE TABLE review_findings (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id      INTEGER NOT NULL REFERENCES runs (id),
	item_id     INTEGER NOT NULL REFERENCES items (id),
	comment_url TEXT NOT NULL CHECK (comment_url <> ''),
	body_sha256 TEXT NOT NULL CHECK (length(body_sha256) = 64),
	created_at  TEXT NOT NULL
);
CREATE INDEX review_findings_item ON review_findings (item_id, comment_url);

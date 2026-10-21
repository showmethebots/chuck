-- Triage dispatches tasks (docs/superpowers/specs/2026-10-20-triage-dispatch-design.md).
-- Existing rows keep triage_version 1: they were decided under the old
-- vocabulary, and the Phase 1 exit counts only the current version.
ALTER TABLE decisions ADD COLUMN task TEXT NOT NULL DEFAULT '';
ALTER TABLE decisions ADD COLUMN ask TEXT NOT NULL DEFAULT '';
ALTER TABLE decisions ADD COLUMN triage_version INTEGER NOT NULL DEFAULT 1;

# Specs and plans

Design documents, newest first. The current design is the source of truth
for what chuck should do; the others are kept as history.

| Document | Status |
| --- | --- |
| [specs/2026-11-03-delivery-loop-design.md](specs/2026-11-03-delivery-loop-design.md) | **Current.** chuck as the agent delivery loop. |
| [specs/2026-10-20-triage-dispatch-design.md](specs/2026-10-20-triage-dispatch-design.md) and [its plan](plans/2026-10-20-triage-dispatch.md) | Retired. Triage proposing worker tasks. |
| [specs/2026-10-13-phase-1-shadow-design.md](specs/2026-10-13-phase-1-shadow-design.md) and [its plan](plans/2026-10-13-phase-1-shadow.md) | Retired. The project-manager agent in shadow mode: Haiku triage, Slack, a decision log and grading. |

The retired documents stay at their original paths on purpose: shipped
migrations are immutable and `internal/memory/migrations/0003_dispatch.sql`
cites the triage dispatch spec by path. Read them for why the design
changed, not for how chuck works today.

Some documents link to claude.ai artifacts made during the design
conversations. Those are private to the maintainer; everything they show is
also in the spec text.

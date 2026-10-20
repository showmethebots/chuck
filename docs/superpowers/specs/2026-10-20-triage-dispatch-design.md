# Triage dispatches tasks — design

Status: agreed in conversation 2026-10-20, pending Clark's review of this
document. Amends the Phase 1 shadow design
(`2026-10-13-phase-1-shadow-design.md`) where it says what triage proposes.

## Why

Shadow posts in `#chuck-shadow` say things like "chuck would `comment_issue`
<PR link> (act_freely)" for a review comment that names a bug. Clark can't
grade that: chuck read the event correctly, but commenting is not what he
wants done. The real choice is reply or fix, and making it needs someone
to read the code.

The Notion Design & Plan already says who that is. Routing rules: "The small
model dispatches; it never judges whether something is hard." Haiku triages
and files work. Sonnet implements and does the first-pass review, in worker
sessions. Today, though, Haiku chooses from every policy action, including
`comment_issue` and `open_draft_pr`. In the design those are things a worker
produces after reading the code.

So Haiku should say *what kind of work this is and that a worker should take
it*. A grade then means "chuck understood the event and was right to hand it
off". That's a judgement Clark can make from the post, and it's the labeled
data Phase 2's router is tested against: "replayed events from Phase 1 route
as expected in tests".

## Goals

- Haiku proposes one of: nothing, a **task** to hand to a worker, or an
  `ask_first` action that needs a human.
- Every triage result carries a one-line **ask**: what the event wants, in
  Haiku's words, so Clark can see whether it understood.
- The rationale says why this task or action, not what the event is.
- The Slack post leads with the task and the ask.

## Non-goals

- Evaluating `routes.yaml` to show a tier. That is the Phase 2 router.
  Building a partial matcher now means building it twice.
- Running any analysis of the code, in shadow or otherwise. Multi-step agent
  work runs as Claude Code in a Sprite (Phases 2–3). A single-shot Sonnet
  call over the PR diff would be a throwaway stand-in, and Sonnet budgets
  are out of Phase 1 scope.
- Drafting reply text or fix plans. Haiku can't see the code, so it can't
  write either one honestly.

## Anti-goals

- Haiku deciding whether something warrants a fix, or how hard it is.
- Weakening the `ask_first` → `human` route check, or any path that takes an
  action. Phase 1 still executes nothing.

## Decided

**Task kinds.** Clark agreed these four:

| Task | When |
|---|---|
| `address_review` | Review feedback asking for a change or raising a problem. The worker reads the code and decides whether to reply or fix. |
| `fix_ci` | A CI run failed or timed out. |
| `implement_issue` | An issue asks for something actionable. |
| `review_pr` | A PR is ready for a first-pass review. |

**What Haiku may propose.** `none`, one of the task kinds, or an action in
the `ask_first` tier of `policy.yaml`. `act_freely` and `act_then_notify`
actions (`comment_issue`, `open_draft_pr`, `create_issue`, `mark_pr_ready`,
the Notion actions) come off Haiku's list. Workers and the planner produce
those. `ask_first` proposals still post real approval requests, so checklist
step 6 (`merge`) keeps working.

**The post.** A task post reads:

> chuck would hand this to a worker: `address_review` <PR link>
> repo · event kind · category · confidence · link (context line, as today)
> Ask: reviewer says the retry loop never resets its backoff
> Why: concrete bug report with a suggested fix
> [👍] [👎]

An approval post keeps its shadow banner and buttons, and gains the same
Ask and Why lines. A grade keeps its meaning of one 👍/👎 per post.

**Option 2 moves to Phase 3.** The first step of Phase 3 after the spike
becomes *workers in shadow*. A worker analyzes the task and posts its
reply-or-fix verdict to Slack without pushing, and Clark grades the verdicts
before workers may act. The Notion Design & Plan is the source of truth, so
this wording is added there (Phase 3 entry) once Clark approves it:

> First step after the spike, workers in shadow: a worker takes a dispatched
> task, reads the code, and posts what it would do (reply, and the gist; or
> fix, and what it would change) without pushing. Clark grades the verdicts
> before any worker may act.

## Builder's calls (made here; change if they turn out wrong)

- **Where task kinds live.** `config/routes.yaml` gets a `tasks` section,
  because tasks are what routes match on. It has two lists. `dispatch` maps
  each task Haiku may pick to the one-line description the prompt shows.
  `internal` lists tasks chuck schedules itself (`plan_goal`, `triage`,
  `digest`, `notion_sync`), which Haiku never picks. Validation, collecting
  every error as today:
  - names are non-empty and unique across both lists
  - no **dispatch** task name is also a `policy.yaml` action, so a Haiku
    output names one thing. Internal tasks are exempt: Haiku never outputs
    them, and `triage` is deliberately both an internal task and the
    `act_freely` action that permits it. Today's config must keep passing
    `chuck validate`
  - dispatch descriptions are non-empty
  - every `match.task` in a route names a declared task (today an
    undeclared name passes silently)

  Each rule gets a test.
- **Model output.** The JSON schema gains `task` and `ask`. Exactly one of
  `task` and `would_do` is a real value, or both are `"none"`. `target` is
  required for either. `ask` may be empty only when both are `"none"`.
  `ParseResult` enforces all of this. Collapsing the two into one enum would
  be fine as well; the constraint that matters is that a decision is clearly
  either a dispatch or an action proposal.
- **Storage.** A new migration (`0003_…`) adds `task` and `ask` columns to
  `decisions`. `0002_shadow.sql` is shipped and not edited. `permission`
  stays empty for task decisions. `would_do` keeps its meaning (a policy
  action or `none`).
- **Post kind.** A task decision gets a grade post. An `ask_first` action
  gets an approval post. Nothing else gets a post, apart from the existing
  daily sample of would-do-nothing decisions.
- **Lengths.** `ask` goes through `field()` with the same bound as the
  rationale, for the same Slack-limit and escaping reasons.
- **Prompt.** It lists the dispatch tasks with their descriptions, then the
  `ask_first` actions with their tier, and says: choose a task when the
  event needs work someone must do in the code or the issue; choose `none`
  otherwise; never judge difficulty. The untrusted-`<event>` wording stays
  as it is.
- **Replay.** `sameVerdict` compares category and task/would_do. Old
  decisions mostly won't reproduce. `comment_issue`-style proposals are no
  longer proposable. `none` on an actionable issue, which checklist step 8
  called correct, should now come back as `implement_issue`. And old stored
  raw output has no `task` or `ask`, so `--fake` replay parses it as
  `triage_failed`. The decisions need a durable "before the change" marker,
  set in the migration (for example, old rows get `task = ''` and new
  no-task rows store `'none'`). It has to be right when the migration
  ships, because shipped migrations are immutable.
- **Docs.** Update `docs/shadow-checklist.md` step 8: an actionable issue
  should now come back as `implement_issue`. Amend the Phase 1 design's
  triage-output and grading rows to point here. Run `docs-currency` before
  the PR.

## Exit window restarts (Clark, 2026-10-20)

The Phase 1 exit window restarts when this change deploys. Every exit
criterion counts only decisions made after the migration, using the marker
above:

- the 7 consecutive days
- the 100 graded decisions
- the ≥90% 👍
- zero 👎 on `ask_first`
- the replay agreement

This covers the daily summary's "Exit progress" line and `chuck replay`'s
default selection. Older decisions and grades stay in the database
untouched. Clark believes nothing has been graded yet, so the cost is small.
The Phase 1 design's exit-criterion section gets a line pointing here.

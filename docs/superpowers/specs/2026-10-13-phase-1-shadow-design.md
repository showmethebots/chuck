# Phase 1: shadow mode — design

Status: approved in conversation 2026-10-13, pending written-spec review.
Source of truth for the overall system is the Notion Design & Plan page
(Project HQ → chuck → Design & Plan). This spec covers Phase 1 only and
records the decisions made while designing it, including where it narrows or
extends the Notion text.

## Goal

chuck watches real GitHub activity, triages each event with Haiku, records
what it would have done, and asks for approval where the policy says it must,
but executes nothing. Clark grades its decisions in Slack, so that by the end
of the phase there is measured evidence that triage is correct and a labelled
dataset that Phase 2's router is tested against.

## Non-goals

- Taking any action on GitHub, Notion, or anywhere else. Approvals are
  recorded; nothing executes.
- The router across four tiers, budgets for Sonnet/Opus, promote-on-failure
  (Phase 2).
- Free-form Slack conversation. Slack messages become routed events in
  Phase 2; in Phase 1 Slack carries only slash commands and buttons.
- Sprites, workers, the `PreToolUse` hook (Phases 2–3).
- Notion polling, the projects/goals/tasks tables, hand-off (Phase 4).
- A staging Fly app. Shadow mode takes no actions, so production is the
  end-to-end environment.

## Decisions made in design

| Decision | Choice | Why |
|---|---|---|
| Event pipeline | SQLite-backed queue: webhook stores raw event and returns 202; an in-daemon loop triages | GitHub's 10 s delivery timeout; `pause` needs something to hold back; restarts lose nothing; replay is the same loop |
| Triage output | Structured JSON validated in code | Replay and Phase 2 need comparable fields; grades attach to a specific claim |
| GitHub source | A read-only GitHub App, not repo/org webhooks | Personal accounts have no account-level webhook; one endpoint and one secret across `cwinters8` and `orionsbelt-dev`; Phase 3 needs an App for scoped tokens anyway |
| Sources | App installed on all of `orionsbelt-dev`, plus `cwinters8/agent-ci-kit`, `cwinters8/agent-skills`, `cwinters8/content-workshop` | Measured activity, below |
| Shadow scope | Every repo in the App installation is treated as handed off, for would-have-done purposes only | Nothing is handed off yet; harmless because nothing executes |
| Approvals | `ask_first` would-dos post real approval requests; answers are logged; nothing executes | Exercises the whole approval round-trip at zero risk |
| Grading | Owner-only 👍/👎 on every would-act decision plus a daily random sample of would-do-nothing decisions | Defines "correct"; keeps grading to roughly 10–20 a day |
| Noise rules | In Go, table-tested | Phase 2's router is where rules move to YAML; designing the format now would mean doing it twice |
| Dev/test repo | `orionsbelt-dev/demos` | It is the Phase 3 pilot; near-zero natural traffic, so test traffic there is deliberate |
| Migrations | New `0002_shadow.sql`; `0001_boots.sql` stays as shipped | The runner tracks migrations by filename, so an edited 0001 never re-runs on production and schemas would silently diverge |
| `resume` | Added, owner-only | The Notion design has `pause`/`kill` but no way back; members can stop chuck but not restart it |

### Measured activity (2026-10-12)

Repos pushed to in the last 60 days. Cells are 60-day / 30-day counts. Push
and review counts come from the Events API, which returns at most 300 events
per repo, so 60-day figures for busy repos are truncated; the 30-day figures
are the reliable ones.

| Repo | PRs | Issues | Comments | CI runs | Pushes | Reviews |
|---|---|---|---|---|---|---|
| cwinters8/agent-ci-kit | 8/8 | 3/3 | 215/215 | 419/419 | 31/31 | 93/93 |
| cwinters8/agent-skills | 6/3 | 1/1 | 376/164 | 0/0 | 36/36 | 90/90 |
| cwinters8/content-workshop | 2/2 | 2/2 | 97/97 | 0/0 | 16/16 | 52/52 |
| cwinters8/sprite-locker | 4/0 | 8/0 | 85/0 | 35/0 | 0/0 | 0/0 |
| cwinters8/agent-skills-testbed | 1/0 | 0/0 | 1/0 | 52/31 | 0/0 | 0/0 |
| cwinters8/agent-server | 1/0 | 0/0 | 0/0 | 0/0 | 0/0 | 0/0 |
| orionsbelt-dev/chuck | 2/2 | 0/0 | 1/1 | 5/5 | 2/2 | 0/0 |
| orionsbelt-dev/marketing | 0/0 | 0/0 | 0/0 | 5/5 | 3/3 | 0/0 |
| orionsbelt-dev/demos | 1/1 | 0/0 | 0/0 | 0/0 | 0/0 | 0/0 |

Roughly 300 deliveries a week before CI. About half of all comments are from
`chatgpt-codex-connector[bot]`. Volume is not the constraint; grading load
is. Other repos are added later by ticking them in the App installation.

## Architecture

```
GitHub App webhook ─┐                           ┌─> Haiku (structured JSON)
                    ├─> events table ─> loop ───┤
cron scheduler ─────┘   (raw, verbatim)         ├─> decisions table
                                                └─> Slack outbox: grade post / approval request
Slack (Socket Mode) ─> slash commands + buttons ─> allowlist check ─> control / approvals / grades
```

One goroutine runs the triage loop, sequentially. At the measured volume
there is no need for parallelism, and sequential processing keeps ordering
and budget accounting simple.

## Components

Package layout follows the Notion design's suggested repo layout.

### `internal/events`

**`github.go`** — `POST /webhooks/github`.

1. Read the body through `http.MaxBytesReader` at 25 MB, GitHub's own cap.
2. Verify `X-Hub-Signature-256` against `GITHUB_APP_WEBHOOK_SECRET` with a
   constant-time compare. Missing or wrong signature: 401, nothing stored.
3. Insert into `events` with `delivery_id` from `X-GitHub-Delivery`. A
   duplicate delivery ID is a no-op that still returns 202, so GitHub's
   redeliveries are idempotent.
4. Return 202.

`kind` is `X-GitHub-Event` plus the payload's `action` when present
(`pull_request.opened`, `push`, `workflow_run.completed`). `repo` is the
payload's `repository.full_name`, empty for installation events.

**`cron.go`** — a scheduler driven by an injected clock. Phase 1 has two
jobs, both daily at `CHUCK_DAILY_AT` (UTC `HH:MM`, default `13:00`):

- **Grade sample:** pick `CHUCK_GRADE_SAMPLE` (default 5) random decisions
  from the previous 24 hours whose outcome is `dropped_by_rule` or whose
  `would_do` is `none`, and queue grade posts for them.
- **Shadow summary:** post to #chuck: decisions by category and outcome,
  grades given and pending, running 👍 rate, Haiku spend against budget,
  triage failures, whether chuck is paused.

Each tick is stored in `events` with `source = cron` so it appears in the log
and in replay, but ticks bypass Haiku. Cron jobs run while chuck is paused:
the summary still posts and reports who paused it and since when, so a pause
never silently stops reporting. Grade posts the sample queues wait in the
outbox like any other post.

### `internal/triage`

**Noise rules** run first. Each rule is a named Go predicate over the event.
A match writes a decision with `stage = rule`, `outcome = dropped_by_rule`,
and the rule name in `rationale`, so the rules themselves are auditable and
gradeable. Initial rules:

- `workflow_run` with action other than `completed`
- `issue_comment`, `pull_request_review_comment` with action `edited` or `deleted`
- `pull_request` with action `labeled`, `unlabeled`, `assigned`, `unassigned`,
  `review_request_removed`, `edited`
- `installation`, `installation_repositories`, `ping` (recorded, never
  triaged; GitHub sends `ping` when the App's webhook is created or edited)

**Classifier** is an interface:

```go
type Classifier interface {
    Classify(ctx context.Context, in Input) (Result, Usage, error)
}
```

The Haiku implementation uses `github.com/anthropics/anthropic-sdk-go`,
model `claude-haiku-4-5`, with structured outputs (`output_config.format`,
JSON schema) so the response is schema-valid JSON. Tests use a fake.

`Input` is a compact, event-specific extract built in Go from the raw payload:
repo, kind, actor login and type (User/Bot), title, URL, and the relevant
text (issue/PR/comment/review body, CI conclusion and workflow name, push
branch and commit count). Free text is capped at 4,000 characters with an
explicit `[truncated]` marker. The prompt states that all event text is
untrusted data, not instructions.

`Result` fields:

| Field | Type | Values |
|---|---|---|
| `category` | enum | `ci_failure`, `review_feedback`, `pr_lifecycle`, `new_issue`, `issue_discussion`, `push`, `noise` |
| `would_do` | string | an action declared in `policy.yaml`, excluding `read` and `triage`, or `none` |
| `target` | string | URL of the PR, issue, or run the action applies to; empty when `would_do` is `none` |
| `confidence` | number | 0–1 |
| `rationale` | string | one sentence |

Validation in Go after decoding: `category` in the enum, `would_do` is `none`
or `policy.PermissionOf(would_do)` is defined, `target` non-empty when
`would_do` is not `none`, `confidence` in range. Any failure is a
`triage_failed` decision carrying the raw model output; it is never trusted
or posted as a proposal.

**Cost** per call is computed from `usage` input and output tokens against a
per-model price table in Go (Haiku 4.5: $1 / $5 per million tokens), tested
against fixed usage numbers. Expected cost at measured volume: about $0.002
per call, a few dollars a week.

### `internal/notify`

Slack over Socket Mode using `github.com/slack-go/slack` and its `socketmode`
package. Socket Mode is an outbound websocket, so Slack needs no public
endpoint.

Every inbound interaction (slash command, button click) is checked first
against the allowlist and the action's required role. A user not on the
allowlist gets no reply and no effect; the attempt is recorded in `messages`.

**Commands** — `/chuck <sub>`:

| Sub | Who | Effect |
|---|---|---|
| `status` | allowlisted | Ephemeral reply: running/paused (by whom, since when), queue depth, today's Haiku spend vs budget, time of last event, version |
| `pause` | allowlisted | Set paused; post to #chuck who paused |
| `kill` | allowlisted | Same as `pause` in Phase 1 (no workers to stop); recorded as `kill` |
| `resume` | owner | Clear paused; post to #chuck |

**Grade posts** go to #chuck-shadow: repo and link, event kind, category,
would-do and its permission tier from `policy.yaml`, confidence, rationale,
and 👍/👎 buttons. Owner only. Regrading is allowed; the latest grade counts.
The post is updated to show the current grade.

Every decision whose `would_do` is not `none` gets a grade post. Others get
one only when the daily sample picks them. `triage_failed` decisions are not
posted individually; they are counted in the daily summary.

**Approval posts** go to #chuck for decisions whose `would_do` is in
`ask_first`. Header: *SHADOW — nothing will execute*. Approve/Deny buttons,
any allowlisted user; the first answer wins and the post is updated to show
who answered. The post also carries the owner's grade buttons, so an
approval-worthy decision does not get a second grade post in #chuck-shadow.

**Outbox.** The loop never posts inline. It writes the decision, then a
separate step sends any decision whose post is due and not yet sent
(`slack_posted_at IS NULL`). A Slack outage delays posts; it does not lose
them or block triage.

### `internal/daemon`

Gains the triage loop:

1. If `control.paused` is set, or today's (UTC) Haiku spend is at or over
   `budgets.daily_usd.haiku`, wait. The first budget stop of a day posts one
   notice to #chuck.
2. Take the oldest event with `triaged_at IS NULL`.
3. Rules, then classifier. Write the decision and set `triaged_at` in one
   transaction.
4. Classifier errors (API error after the SDK's own retries) increment
   `events.attempts` and leave the event untriaged with a backoff; after 3
   attempts write a `triage_failed` decision and move on.
5. Flush the outbox.
6. Log the decision as one structured JSON line: event id, kind, repo,
   stage, outcome, category, would_do, cost.

`/healthz` is unchanged.

### `internal/config`

New file `config/slack.yaml`, decoded with `KnownFields(true)`:

```yaml
owner: U0123456789        # Clark
members: []               # Slack user IDs
```

Validation, joined with `errors.Join` like the existing files: exactly one
owner; every ID matches Slack's user-ID shape (`^[UW][A-Z0-9]{8,}$`); no
duplicates; the owner is not also a member. The allowlist changes only by PR.

Channel IDs and tokens are deployment-specific (prod app vs dev app), so they
come from the environment, not config.

### `cmd/chuck`

- `chuck run` wires the new pieces; `getenv`/stdout/stderr injection stays.
- `chuck validate` also validates `slack.yaml`.
- `chuck replay --db <path> [--since <date>] [--fake]` — see Testing.

## Data model

New migration `internal/memory/migrations/0002_shadow.sql`. Timestamps are
RFC 3339 UTC text, matching `0001`.

| Table | Columns | Notes |
|---|---|---|
| `events` | id, source (`github`/`cron`), kind, delivery_id UNIQUE NULL, repo, headers (JSON), payload (verbatim), received_at, attempts, triaged_at NULL | The queue is `triaged_at IS NULL`. Index on `(triaged_at, id)` |
| `decisions` | id, event_id, stage (`rule`/`haiku`), outcome (`ok`/`dropped_by_rule`/`triage_failed`), category, would_do, permission, target, confidence, rationale, raw_output, model, input_tokens, output_tokens, cost_usd, post_kind (`grade`/`approval`/NULL), slack_channel, slack_ts, slack_posted_at, created_at | Today's spend is `SUM(cost_usd)` for today. `permission` is the policy tier at decision time |
| `grades` | id, decision_id, grade (`up`/`down`), slack_user, graded_at | Latest per decision counts |
| `approvals` | id, decision_id, request, response (`approve`/`deny`) NULL, responder_slack_user NULL, responded_at NULL | As in the Notion design; Slack location is on the decision |
| `messages` | id, slack_channel, slack_ts, slack_user, direction (`in`/`out`), kind (`command`/`button`/`post`), text, allowed, created_at | As in the Notion design minus `project_id` (no projects yet); `allowed` records the allowlist result |
| `control` | id (always 1), paused, reason (`pause`/`kill`/NULL), by_slack_user, changed_at | One row, seeded unpaused |

Not in Phase 1: `projects`, `goals`, `tasks` (Phase 4; nothing writes them).

**Storage.** The volume is 1 GB. Raw payloads at measured volume are on the
order of tens of MB a week, so the volume lasts months. Retention or a larger
volume is a later decision, flagged in the daily summary once usage passes
50%.

## Configuration and secrets

Fly secrets (prod) / local env (dev):

| Name | Purpose |
|---|---|
| `GITHUB_APP_WEBHOOK_SECRET` | Webhook signature verification |
| `SLACK_BOT_TOKEN` | `xoxb-`, posting and updating messages |
| `SLACK_APP_TOKEN` | `xapp-`, Socket Mode connection |
| `ANTHROPIC_API_KEY` | Haiku |

Plain env (`[env]` in `fly.toml` for prod):

| Name | Default |
|---|---|
| `CHUCK_SLACK_MAIN_CHANNEL` | required — #chuck |
| `CHUCK_SLACK_SHADOW_CHANNEL` | required — #chuck-shadow |
| `CHUCK_DAILY_AT` | `13:00` |
| `CHUCK_GRADE_SAMPLE` | `5` |

The daemon refuses to start if a required secret or channel is missing,
except that with none of the Slack/GitHub/Anthropic settings present it runs
as Phase 0 did (heartbeat only), so `make run` keeps working with no setup.

The GitHub App needs no private key in Phase 1: chuck makes no GitHub API
calls; everything triage uses is in the webhook payload.

## External setup

Detailed steps go in `deploy/README.md` as part of implementation.

- **GitHub App `chuck`**: webhook URL `https://chuck.fly.dev/webhooks/github`,
  webhook secret set. Read-only permissions: Metadata, Contents, Issues, Pull
  requests, Actions. Events: Pull request, Pull request review, Pull request
  review comment, Issues, Issue comment, Push, Workflow run. Not Check run,
  Check suite or Status, which multiply CI noise. Install on `orionsbelt-dev`
  (all repositories) and on `cwinters8` (selected: agent-ci-kit,
  agent-skills, content-workshop).
- **GitHub App `chuck-dev`**: same settings, webhook URL is a `cloudflared`
  quick-tunnel URL to the laptop, installed on `orionsbelt-dev/demos` only.
- **Slack apps `chuck` and `chuck-dev`**: Socket Mode on, interactivity on,
  slash command `/chuck` (dev: `/chuck-dev`), bot scopes `chat:write` and
  `commands`. Invite each to its channels.
- **Channels**: #chuck, #chuck-shadow (prod), #chuck-dev (dev).
- **Second Slack account** for role testing.
- **Anthropic API key** with a monthly spend limit set in the Console as a
  backstop to chuck's own daily budget.

Two apps per service because Slack spreads events across all open Socket
Mode connections of one app; a laptop connected to the prod app would
silently take a share of production's events.

## Testing

### CI (`go test -race ./...`)

- Webhook: valid, invalid, and missing signature; duplicate delivery ID;
  oversize body; recorded real payloads for each subscribed event under
  `internal/events/testdata/`.
- Noise rules: table test per rule, including near-misses that must not match.
- Classifier validation: unknown category, unknown action, `read`/`triage` as
  would-do, missing target, confidence out of range, malformed JSON — each
  yields `triage_failed`.
- Input extraction: per event kind, including truncation.
- Cost: fixed usage → expected dollars.
- Loop with fake classifier and fake notifier: processes in order; paused
  stops it; budget stops it and notifies once a day; classifier errors back
  off then fail after 3; restart picks up untriaged events; outbox retries
  after a notifier failure.
- Allowlist matrix: owner, member, stranger × every command and button, with
  expected effect and `messages.allowed`.
- `slack.yaml` validation: each rule, and all errors reported together.
- Migrations: fresh DB reaches `0002`; a DB at `0001` with boot rows upgrades
  and keeps them.
- `chuck replay --fake` over a fixture DB.

### Local full stack

`make run` with the dev env set: `chuck-dev` Slack app posting to #chuck-dev,
`chuck-dev` GitHub App on `demos` delivering through a `cloudflared` quick
tunnel (not smee.io, which would pass private-repo payloads through a public
relay). Generate events on `demos` (open an issue, push a failing CI run,
leave a review) and watch them land. The prod App also covers `demos`, so
production triages the same traffic; that is intended.

### Live checklist

`docs/shadow-checklist.md`, run after the first deploy and after any change to
Slack handling, using Clark's account and the second account:

1. `/chuck status` as owner shows running.
2. Second account not on the allowlist: `/chuck status` and a button click do
   nothing; `messages` shows `allowed = 0`.
3. Add second account as member by PR, deploy: `status`, `pause` work;
   `resume` is refused; Approve on a shadow approval works; grade buttons are
   refused. To get an approval post on demand, open a PR on `demos`, approve
   it, and let CI pass; triage should propose `merge`.
4. Owner `resume` works.
5. `pause`, redeploy, `status` still shows paused.
6. A test event on `demos` produces a grade post within a minute.

### Replay

`chuck replay` opens the database read-only (`mode=ro`), runs every stored
GitHub event through rules and the classifier, and prints a report. It writes
nothing. To replay production data, restore the Litestream replica to a local
file first.

For graded decisions it reports, per category and overall:

- **👍 agreement:** share of 👍 decisions where the replay produces the same
  outcome, category, and would-do.
- **👎 divergence:** share of 👎 decisions where the replay produces a
  different outcome or would-do.

`--fake` uses the fake classifier (CI). Without it, real Haiku, and the
report includes the run's cost.

## Exit criterion

Replaces "a week of correct triage and sensible would-have-done entries" with
measurable terms. All must hold:

1. 7 consecutive days running in production unpaused except by deliberate
   test.
2. At least 100 graded decisions.
3. At least 90% 👍 of graded decisions.
4. Zero 👎 on decisions whose would-do is in `ask_first`.
5. Zero allowlist violations: no `messages` row with `allowed = 0` whose
   interaction changed state.
6. A real-Haiku `replay` using the prompt and rules as they stand at the end
   of the week, run against every grade collected during it, has at least 90%
   👍 agreement. Replaying an unchanged prompt would only measure
   nondeterminism; the point is that the prompt will be tuned as grades come
   in, and the final version must still agree with grades given to earlier
   versions.

The daily summary reports progress on 2–4.

## Risks

- **Prompt injection through event text.** Issue and comment bodies are
  attacker-controllable on public repos. In Phase 1 the worst outcome is a
  wrong classification or a spurious approval request, since nothing
  executes. Mitigations: text is marked as untrusted data in the prompt,
  output is schema-constrained and validated against policy, and approvals
  still need a human.
- **Grading fatigue.** If posts exceed what Clark will grade, lower
  `CHUCK_GRADE_SAMPLE` or add noise rules; the exit criterion counts graded
  decisions, not days of posts.
- **Clark's own comments include agent-posted replies.** chuck cannot tell
  them apart from Clark's hand-written comments. Grades will show whether
  that matters.

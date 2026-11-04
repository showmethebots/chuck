# chuck as the agent delivery loop: design

Status: agreed in conversation 2026-11-02 to 2026-11-03, pending Clark's
review of this document. Supersedes the Phase 1 shadow design
(`2026-10-13-phase-1-shadow-design.md`) and the triage dispatch design
(`2026-10-20-triage-dispatch-design.md`). Refines the Notion page "Agent
Delivery Loop: Plan and Architecture" (drafted 2026-11-02), which described
the loop on GitHub Actions; this document moves it into chuck.

Companions, built during the design conversation:

- [Delivery Loop States](https://claude.ai/artifact/9M2cci9cS6x3kQMDs6V5sm):
  clickable state diagram with every entry, exit and escalation.
- [Config, Failures, Tests](https://claude.ai/artifact/7wP26t1bvQGhdXgedvQMw6):
  the two config files, the failure matrix, and the test map.

## Why

The Notion spec's loop is right: a deterministic dispatcher moves issues
through intake, build, review, sweep, merge, validate and promote, with
Claude sessions doing one stage each and reporting a schema-validated
verdict. It put the dispatcher in GitHub Actions because that was the only
always-on compute at hand. chuck is always-on compute that already receives
the same GitHub App events, so chuck becomes the dispatcher. That removes
the consumer trigger workflow, the reusable workflow, the marker-comment
counting that stood in for state, and the `github_job_workflow_sha`
question. It adds a dashboard, which the Notion spec deferred to phase 4
and which is needed from phase 1 for manual stepping.

chuck's project-manager framing (Haiku triage, routing tiers, permission
tiers, grades, Slack, Notion sync) is retired. The Notion "Design & Plan"
page for the project manager agent is no longer the source of truth; this
document and its successors in `docs/superpowers/specs/` are.

## Goals

- One Go binary on one Fly Machine runs the loop for every installed repo.
- Every stage is a fresh Claude Code session on its own Fly Machine that
  reports a verdict and dies.
- State lives in chuck's SQLite; `loop:*` labels on GitHub are a mirror.
- A dashboard in the Show Me The Bots style drives phase 1 by hand and
  becomes the approval surface from phase 2.
- Phases differ by config flags, not by code paths.

## Non-goals

- Anything the old framing did that is not the loop: digests, Notion,
  goal planning, Slack.
- Cross-repo scheduling beyond a per-repo WIP limit.
- Live log streaming or a front-end app. The pages are server-rendered
  and read from a view layer that a JSON API can serialise later.
- Editing acceptance criteria in the dashboard. They are edited on GitHub;
  every page links to the issue.

## Principles (carried from the Notion spec)

- Code manages the flow; agents do the work.
- Agents report; code transitions. Workers never change labels, merge or
  deploy.
- Decide on facts code can check, not on the model's confidence.
- The author is never the reviewer or the validator. The validator is
  read-only.
- The human role moves earlier: write issues, approve criteria, handle
  escalations.

## Architecture

### Packages

| Package | Does | Replaces |
|---|---|---|
| `internal/github` | App auth (JWT, installation tokens scoped per call), the REST calls the loop makes, the webhook receiver | `internal/events`, octokit in the sketch |
| `internal/loop` | `Transition(item, repo, cfg, input, now) Result`, the effect executor, WIP and stale rules, embedded prompts and verdict schemas | `loop/dispatch.cjs`, `prep.sh` |
| `internal/worker` | Launch a Machine with prompt, schema, credentials and run token; watch; destroy; accept the callback | `claude-code-action` |
| `internal/web` | OAuth login, sessions, CSRF, dashboard pages, worker API, `/healthz`, webhook mount | `internal/notify`, `internal/daemon` |
| `internal/memory` | SQLite as today, migration `0004_loop.sql` | same package |
| `internal/config` | `config/chuck.yaml` and per-repo `loop.json`, strict as today | the three YAML files |
| `cmd/chuck` | `run`, `validate`; wires packages from the environment | same, minus `replay` |
| `cmd/chuck-worker` | Entrypoint of the worker image: fetch spec, clone, run `claude -p`, post verdict | new |

Deleted: `internal/triage`, `internal/notify`, `internal/daemon`,
`cmd/chuck/replay.go`, `config/routes.yaml`, `config/policy.yaml`,
`config/slack.yaml`, the Slack and Anthropic SDK dependencies (workers
call Claude; chuck never does).

Kept: `Dockerfile`, `deploy/` (fly.toml, Litestream, entrypoint, smoke
restore), CI, vendored skills, `Makefile` targets. `deploy/worker.Dockerfile`
gains Playwright, Chromium and `chuck-worker`.

### What each consumer repo carries

- `.claude/loop.json` (schema below). chuck reads it from the default
  branch on every transition and records the blob SHA; a merged change
  takes effect on the next event. It is a risk path and never auto-merges.
- `deploy-dev` (on push to main) and `deploy-prod` (`workflow_dispatch`
  with a `sha` input, behind a `production` environment). chuck does not
  deploy; it dispatches and watches `workflow_run`.
- Vendored skills and `.claude/project-profile.md` as now.

The consumer `loop.yml` from the Notion spec does not exist.

### Identities

| Identity | Used for | Notes |
|---|---|---|
| chuck's GitHub App (`orionsbelt-dev`) | Webhooks, labels, comments, approve, merge, workflow dispatch, minting worker tokens, dashboard OAuth | Permissions: contents, issues, pull requests, actions (write); checks (read). Needs a private key as a Fly secret; today's Apps have none. |
| Per-run installation token | One worker run | Scoped to the one repo and the stage's permission set, 1 h TTL. Build and sweep: contents, pull requests, issues write. Review: pull requests write. Validate: read only. |
| `cwinters8` App, dev App | Webhooks only until a repo under them joins the loop | Same secret. chuck shows their repos as "read-only installation". Adding one is a `config/chuck.yaml` entry plus a key. |
| Humans | Dashboard login, labels that start or approve stages | GitHub user ids in `config/chuck.yaml`. One entry to start. |
| Claude | Workers | `ANTHROPIC_API_KEY` from the Console org first (credits included with Max); `CLAUDE_CODE_OAUTH_TOKEN` as fallback when credits are exhausted. Both Fly secrets, passed to a Machine for one run. |

chuck's own comments carry a hidden marker (`<!-- loop-verdict ... -->`)
for the audit trail on GitHub. chuck never reads markers back; counts come
from `runs`.

### Request paths

- Unauthenticated: `GET /healthz`, `POST /webhooks/github`, `GET /login`,
  `GET /oauth/callback`, `GET /static/*`, and `/api/runs/{id}/*` with a
  run token.
- Everything else needs a session.

## Data model

Migration `0004_loop.sql` drops `events`, `decisions`, `grades`,
`approvals`, `messages`, `control` and creates these. `boots` stays.
Timestamps are the store's fixed-width UTC text.

```
repos        id, owner, name, installation_id, app_owner, default_branch,
             enabled, config_json, config_sha, config_read_at
items        id, repo_id, kind ('issue'|'pr'), number, title,
             issue_number,                   -- for a PR: the issue it serves
             state, state_since, escalated_from, pending_stage,
             created_at, updated_at
             UNIQUE (repo_id, kind, number)
transitions  id, item_id, from_state, to_state, input_kind, input_ref,
             reason, actor, config_sha, effects_json, created_at
             -- input_kind: webhook|verdict|dashboard|tick
             -- actor: GitHub login for dashboard actions, 'chuck' otherwise
             -- effects_json: the effect list with each result
runs         id, item_id, stage, attempt, status,
             -- launching|running|finished|failed|cancelled|timed_out
             machine_id, machine_region, image, model,
             credential ('api_key'|'oauth'), prompt, schema_name, ref,
             token_hash, started_at, callback_at, destroyed_at, exit_code,
             cost_usd, input_tokens, output_tokens, duration_ms,
             verdict_json, log_tail
evidence     id, run_id, path, content_type, bytes, sha256, content BLOB,
             created_at
webhooks     id, delivery_id UNIQUE, event, action, repo, sender_id,
             sender_login, payload, received_at, handled_at, result
sessions     id, user_id, login, csrf, created_at, expires_at, revoked_at
spend        month, repo_id, credential, usd, runs
settings     key, value            -- e.g. api_key_exhausted_until
```

Decisions:

- `items.state` is the answer; `transitions` is the history. The executor
  writes both in one transaction before any GitHub call, so chuck is never
  behind its own database. Label drift is repaired on the tick.
- Verdicts are rows. Attempt and round counts are `COUNT(*)` over `runs`.
- Evidence is a BLOB in SQLite, next to its run, replicated by Litestream.
  Moving it to Tigris later is a one-table change.
- Migrations for tables a worker callback touches are additive only, so a
  worker launched before a deploy can still post its verdict after.

## Loop engine

`internal/loop` is a pure transition function plus an executor.

```go
type Input struct {
    Kind    InputKind       // Webhook, Verdict, Dashboard, Tick
    Webhook *WebhookInput   // event, action, label, sender id, numbers, sha, conclusion
    Verdict *VerdictInput   // run id, stage, attempt, decoded verdict or error
    Action  *DashboardInput // StartIntake, RunStage, Requeue, CloseOut, Approve, CancelRun; actor
    Facts   Facts           // PR head sha, changed files, AC ids, counts the caller fetched
}
type Result struct {
    State   State
    Reason  string
    Effects []Effect // SetLabel, Comment, Launch, ApproveAndAutoMerge, Merge,
                     // DispatchWorkflow, Mention, DestroyMachine, OpenRatchetIssue
}
func Transition(item Item, repo Repo, cfg LoopConfig, in Input, now time.Time) Result
```

It reads nothing and calls nothing. Facts it needs from GitHub arrive in
the input. Tests feed fixtures and assert states and effects.

### States

The Notion spec's set plus `merged` (PR merged, waiting for deploy-dev) and
`closed` (abandoned). Terminal: `done`, `closed`. `in-pr` means the issue
follows its PR.

Active states, which count against `wip_limit`: `building`, `review`,
`sweep`, `merging`, `merged`, `validating`, `promoting`.

### Transition table

| Input | Condition | Transition |
|---|---|---|
| `issues.labeled loop:intake` by a human, or dashboard Start intake | no item yet | → `intake`, launch intake |
| verdict intake `ready_for_approval` | issue has `AC-n` lines, or sub-issues were opened | → `awaiting-approval`; sub-issues → `awaiting-approval` |
| verdict intake `needs_clarification` or no criteria | | → `needs-human` |
| `issues.labeled loop:ready` by a human, or dashboard Approve | `awaiting-approval` | → `ready` |
| tick, or any transition that frees WIP | oldest `ready`, active count < `wip_limit` | → `building`, launch build |
| verdict build `pr_opened` | PR open, on the branch chuck named, opened by chuck's App, references the issue | issue → `in-pr`; PR item created → `review`, launch review |
| verdict build `error` | attempts < `build_attempts` | → `ready` |
| verdict build `blocked`, cap hit, or PR check fails | | → `needs-human` |
| verdict review `posted` | `review` | → `sweep`, launch sweep |
| verdict review `failed`, or rounds ≥ `review_rounds` | | → `needs-human` |
| verdict sweep `changes_pushed` | | → `review`, launch review |
| verdict sweep `ready` | head = reviewed sha, no open findings, no unfixed security findings, no risk paths, `auto_merge` | → `merging`, approve and enable auto-merge (direct merge if GitHub refuses on a clean PR) |
| verdict sweep `ready` | any of those fails | → `awaiting-merge` with reasons |
| verdict sweep `ready` | head moved since review | → `review`, launch review |
| verdict sweep `cap_reached` or `blocked` | | → `needs-human` |
| `pull_request.synchronize` by a human | `review`, `sweep`, `awaiting-merge` | → `review`, launch review |
| `pull_request.closed` merged | `merging`, `awaiting-merge` | → `merged` |
| `pull_request.closed` not merged | any PR state | PR → `closed`; issue → `needs-human` |
| `workflow_run deploy-dev` success for the merge sha | `merged` | → `validating`, launch validate |
| `workflow_run deploy-dev` failure | `merged` | → `needs-human` |
| verdict validate | every `AC-n` in the issue reported and `pass`, every regression `pass`, `tested_sha` = deployed sha, no risk paths, `auto_promote` | → `promoting`, dispatch deploy-prod |
| verdict validate | criteria clear but risk path or `auto_promote` off | → `awaiting-prod-approval` |
| verdict validate | anything else, including `undetermined` | → `needs-human` |
| `pull_request.labeled loop:approve-prod` by a human, or dashboard Promote | `awaiting-prod-approval` | → `promoting`, dispatch deploy-prod |
| `workflow_run deploy-prod` success | `promoting` | PR → `done`; issue → `done`; ratchet issue if enabled; start next ready |
| `workflow_run deploy-prod` failure | `promoting` | → `needs-human` |
| tick | active state older than `stale_hours` | → `needs-human` |
| dashboard Re-queue | `needs-human` | → `escalated_from`, launch its stage |
| dashboard Close out | `needs-human` | → `closed` |
| dashboard Cancel run | a run is open | destroy Machine, `error` verdict, then the verdict rules apply |
| anything else | | no transition; reason on the webhook row |

`awaiting-approval` stays a human gate in every phase. It is where you
decide what gets built, and on a public repo it is what stops a stranger's
issue from spending credits.

### Effects and the executor

Effects are data. The executor commits the new state and the transition
row, then runs effects in order with chuck's App token, recording each
result on the row. It stops at the first failure; the next tick retries the
idempotent ones (labels, marker-checked comments, workflow dispatch). Merge
is retried only if the PR is still open and unmerged at the reviewed sha.
After three ticks without success the item escalates.

`auto_advance: false` turns every `Launch` into `items.pending_stage`; the
dashboard shows a Run button, which submits a `RunStage` input. That is
the whole difference between phase 1 and phase 2.

### Gate checks

WIP, caps, duplicate events and "is this still a loop PR" happen inside
`Transition`, with counts supplied in `Facts`. There is no separate gate
job.

## The ratchet

Every criterion the validator passes becomes a permanent end-to-end test.

1. The validator's Playwright scripts under `evidence/` are stored with
   the run, not only the screenshots.
2. On `done`, if `ratchet` is on, chuck opens "Add e2e coverage for #N"
   in the same repo, listing each passed `AC-n` with its script attached,
   labelled `loop:intake`. Its criteria are mechanical: the suite includes
   a test named `#N AC-n` and it passes against `BASE_URL`.
3. The builder turns the scripts into tests in the repo's own e2e suite,
   and the item runs the whole loop. Validate on this item is the suite
   passing in dev.
4. Those tests run in every later validator's regression step
   (`validate.e2e_command`).

Keeping the suite coherent:

- **Provenance.** Every test is named `#N AC-n: ...`. A test without one
  is a review finding.
- **Intake checks for conflicts.** chuck passes the suite's test names
  into the intake prompt. A new criterion that contradicts an existing
  test gets a `Supersedes: #N AC-n` line under it in the issue, which you
  see at `awaiting-approval`. chuck checks every `Supersedes` target
  exists.
- **The builder retires what the issue supersedes** in the same PR. If it
  forgets, the suite fails in validation and the item escalates.
- **Removal goes through the loop.** A removal issue supersedes the
  feature's tests. Nothing in the suite changes outside the loop.
- Flaky tests surface as `undetermined` regressions, which escalate
  rather than fail. Flaky-test count is a metric.

Criteria that escalated never reach `done` through the validator, so
there is nothing to ratchet for them.

## Workers

### Launch

1. Insert the `runs` row; generate a 32-byte run token; store its hash.
2. Mint an installation token for the repo with the stage's permissions,
   1 h TTL.
3. Create a Machine in the `chuck-workers` Fly app (separate from `chuck`
   so workers never share chuck's volume or secrets and survive chuck's
   deploys), `auto_destroy: true`, `restart: no`. Env: `CHUCK_URL`,
   `CHUCK_RUN_ID`, `CHUCK_RUN_TOKEN`, `GITHUB_TOKEN`, `REPO`, `REF`,
   `ISSUE`, `PR`, `ATTEMPT`, `SHA`, the Claude credential, `CLAUDE_MODEL`,
   `CLAUDE_MAX_TURNS`, and for validate `DEV_URL` plus the repo's test
   credentials from the Fly secret `LOOP_ENV_<owner>_<repo>`. Validator
   Machines get Fly's restricted network.
4. Machine size per stage from `loop.json` `machines`.

Before any launch chuck checks spend against `config/chuck.yaml`; at the
cap the item goes to `needs-human` with reason "budget".

### In the Machine

`chuck-worker` is the image entrypoint.

1. `GET /api/runs/{id}/spec` with the run token: prompt, schema, model,
   turns. The prompt never passes through the Machines API.
2. Clone the repo at `REF`; exclude `evidence/` and the token from git.
3. Run `claude -p` with `--json-schema`, `--model`, `--max-turns`,
   `--output-format json`; for validate, `--allowedTools` limited to read,
   write under `evidence/`, `Bash(npx playwright:*)`, `Bash(node:*)`,
   `Bash(curl:*)`. Tee output to a log.
4. `POST /api/runs/{id}/verdict`: structured output, Claude Code's usage
   block, exit code, last 64 KB of log, and for validate a tar of
   `evidence/` (20 MB cap). Retries for 10 minutes with backoff, which
   covers a chuck deploy.
5. On SIGTERM (wall clock) during build or sweep: commit and push the
   working tree to the stage's branch so the next attempt starts from it.
6. Exit; `auto_destroy` removes the Machine.

### Back in chuck

- The verdict endpoint accepts one post per run, only while `running`,
  only with the matching token hash. It validates against the stage
  schema again. Then it feeds a `Verdict` input to the loop.
- A watcher polls the Machines API every 30 s for `running` rows. Machine
  gone without a verdict → `failed`, `error` verdict. Past the stage's
  wall clock → destroy, `timed_out`, `error` verdict. On boot the watcher
  resumes every `running` row.
- Wall clock: `limits.wall_clock` per stage in `loop.json` (defaults:
  build 90m, validate 60m, others 30m), under an absolute ceiling in
  `config/chuck.yaml` (4h). Run duration per stage and timeout count are
  dashboard metrics.
- Credit exhaustion: if the log tail matches Claude Code's credit error,
  chuck sets `api_key_exhausted_until` to the 1st of next month and
  relaunches the same stage on the OAuth token as the same attempt.
- Running workers keep their image through a chuck deploy; `runs.image`
  records which ran.

### Prompts

The Notion spec's five, embedded in chuck, with the Context block
appended as `prep.sh` did, plus: the `Supersedes` convention for intake
and build; "retire every test a `Supersedes` line names" for build; the
suite's test names in intake's context. Promoting intake and validate
into skills waits until they have been tuned on real runs.

### Local testing

`chuck-worker` runs on a laptop with `CHUCK_URL` pointing at a local
chuck. `internal/worker` has a `Launcher` interface: `Fly`, `Local`
(execs the binary) and a fake for tests.

## Dashboard and web layer

### Auth

- `/login` redirects to GitHub with no scopes, a random `state` in a
  10-minute cookie, and PKCE.
- `/oauth/callback` checks `state`, exchanges the code, reads
  `GET /user`, compares the numeric id against `humans`. Unknown: 403,
  nothing stored. Known: a `sessions` row and a `Secure; HttpOnly;
  SameSite=Lax` cookie with the session id, 7-day sliding expiry.
- Every mutating route is `POST`, checks the session's CSRF token from a
  hidden field, records `actor`. `/logout` revokes. Failed logins are
  logged with IP; the callback is rate-limited to 10 a minute.
- The transition table refuses an action that does not apply to the
  current state, so a stale page can never double-launch.

### Pages

`html/template`, one layout, one stylesheet with the Show Me The Bots
tokens (ink, paper, hot, volt, Inter Display for titles, mono for state
names and SHAs, the pop shadow on the one card that needs attention), one
small script for confirm steps and refresh that every page works without.
Light and dark from the same tokens.

| Route | Shows | Actions |
|---|---|---|
| `/` | Repos: enabled or read-only, config status, active and queued counts, open escalations, month spend. Running jobs with elapsed time. | Cancel run |
| `/r/{owner}/{repo}` | Items by state with issue and PR links, state age, attempts and rounds. Open issues not in the loop. `loop.json` as last read, flagged when main has moved. | Start intake, Approve, Promote, Re-queue, Close out, Run next stage |
| `/r/{owner}/{repo}/i/{n}` | State, criteria rendered from the issue with an edit link, `Supersedes` lines, linked PR with head and checks, every run with its verdict rendered, escalation reason, full transitions with actor and effects. Links to issue, PR, Machines, deploy runs. | Same per item |
| `/r/{owner}/{repo}/runs/{id}` | Stage, attempt, model, credential, cost, timings, verdict JSON, log tail, evidence thumbnails. | Cancel |
| `/webhooks` | Last 200 deliveries and how each was handled. | none |
| `/spend` | Month to date by repo and credential, caps, the exhausted flag. | none |

Rule: anything chuck knows about an item is on its page or one click away.

### Webhook receiver

Kept from `internal/events`: HMAC-SHA256 over the raw body against
`GITHUB_APP_WEBHOOK_SECRET`, 25 MB cap, two read slots, 10 s bounds,
verbatim storage keyed by delivery id, 202 then async handling. Added,
closing issue #6: reject before reading unless `User-Agent` starts with
`GitHub-Hookshot/` and `X-Hub-Signature-256` is present.

## Config

### `config/chuck.yaml` (chuck-wide, by PR)

```yaml
humans:
  - id: 1234567            # cwinters8
apps:
  - owner: orionsbelt-dev
    app_id: ${GITHUB_APP_ID}
    private_key: ${GITHUB_APP_PRIVATE_KEY}
    oauth_client_id: ${GITHUB_OAUTH_CLIENT_ID}
    oauth_client_secret: ${GITHUB_OAUTH_CLIENT_SECRET}
workers:
  fly_app: chuck-workers
  image: registry.fly.io/chuck-workers:sha-...
  wall_clock_ceiling: 4h
spend:
  monthly_usd: 100
  per_repo_monthly_usd: 60
tick: 30m
webhook_retention: 720h
```

`${VAR}` is the only substitution and resolves from the environment (Fly
secrets). Strict decoding, every error reported at once, as today.

### `.claude/loop.json` (per repo, a risk path)

```json
{
  "default_branch": "main",
  "wip_limit": 1,
  "merge_method": "SQUASH",
  "auto_advance": false,
  "auto_merge": false,
  "auto_promote": false,
  "ratchet": true,
  "limits": {
    "build_attempts": 2, "review_rounds": 3, "stale_hours": 6,
    "wall_clock": { "default": "30m", "build": "90m", "validate": "60m" }
  },
  "models": { "default": "claude-sonnet-5-5", "build": "claude-opus-5-5", "validate": "claude-opus-5-5" },
  "max_turns": { "default": 60, "build": 200, "validate": 100 },
  "machines": { "default": "shared-cpu-2x:2048", "validate": "shared-cpu-2x:4096" },
  "risk_paths": [".github/**", ".claude/**", "**/migrations/**", "**/auth/**", "package.json", "package-lock.json"],
  "deploy": { "dev_url": "https://dev.example.com", "dev_workflow": "deploy-dev", "prod_workflow": "deploy-prod.yml" },
  "validate": {
    "setup": "npm ci && npx playwright install --with-deps chromium",
    "e2e_command": "BASE_URL=$DEV_URL npx playwright test",
    "smoke": ["Home page loads and lists the demo bots"]
  }
}
```

A JSON Schema ships in chuck and is vendored to consumers. A repo whose
file fails to decode shows as "config invalid" and launches nothing.

## Failure handling

One rule covers most of it: state is committed before effects run, a
failed effect leaves the item in its committed state with the error on
the transition row, idempotent effects are retried on the tick, and
`needs-human` is the only way the loop stops.

| Where | What | Outcome |
|---|---|---|
| GitHub | Bad or missing signature, non-GitHub user agent | Rejected before the body is read; logged with IP |
| GitHub | Duplicate delivery | No-op on the unique delivery id |
| GitHub | Unexpected event (non-human label, wrong state, fork PR) | No transition; reason on the webhook row |
| GitHub | API error during an effect | Retried on the tick; escalates after three ticks |
| GitHub | `loop.json` missing or invalid | Repo shown as config invalid; nothing launches |
| Worker | Machine never starts | Run failed; stage retried on the tick within the attempt cap |
| Worker | Machine gone without a verdict, or past the wall clock | `error` verdict; build retries within the cap from its pushed branch, other stages escalate |
| Worker | Verdict fails schema validation | 422; run becomes failed with the validation error quoted; escalates |
| Worker | API key credits exhausted | Relaunch on the OAuth token, same attempt; flag shown on `/spend` |
| Worker | Both credentials fail | Every launch → `needs-human`, "no working Claude credential" |
| Worker | Evidence over 20 MB | Verdict accepted, evidence dropped, run flagged |
| Policy | Spend cap reached | `needs-human`, "budget"; raise by PR, then Re-queue |
| Policy | Stale item | Escalated by the tick with state and age |
| Deploy | deploy-dev or deploy-prod fails | Escalated with the run link; prod rollback is manual |
| Deploy | deploy-dev never fires | Sits in `merged` until stale, then escalates |
| chuck | Restart mid-run | Watcher resumes `running` rows; workers retry the callback for 10 min |
| chuck | SQLite write fails | 500, nothing partial; Litestream is the recovery path |
| Dashboard | Login not in `humans` | 403, no session, logged |
| Dashboard | Bad CSRF, or action on a state that changed | Rejected; page reloads with current state |

## Testing

Every package tests through its interface with fakes for GitHub, Fly and
the clock. Most tests live in `internal/loop` and `internal/web`.

- `internal/loop`: every row of the transition table, WIP waiting and
  release, every cap, every sweep and validate policy branch, a
  `Supersedes` target that does not exist, non-human labels and wrong-state
  labels, `auto_advance` off, Re-queue to `escalated_from`.
- `internal/web`: OAuth (bad state, replayed code, user not in humans,
  PKCE mismatch), sessions (forged, expired, revoked, sliding), CSRF
  (missing, wrong session, GET on an action), worker API (wrong token,
  second verdict, verdict after close, oversized evidence, invalid
  schema), webhook (bad signature, empty secret, duplicate, bad user
  agent, oversized body), every page with an empty and a full store.
- `internal/worker`: env and permissions per stage, watcher cases,
  credit-exhausted relaunch, spend cap.
- `cmd/chuck-worker`: against an httptest chuck with a fake `claude`;
  evidence tar excludes `.git` and the token; non-zero exit still posts.
- `internal/github`: token minting with scoped permissions, every REST
  call, auto-merge refusal → direct merge, `loop.json` by blob SHA.
- `internal/memory`: migration 0004 on a Phase 1 database, state and
  transition in one tx, spend rollup, retention, session expiry.
- `internal/config`: strict decoding, every error together, schema.

Mechanical checks: `make test`, `make validate`, `make lint` (with
govulncheck, closing issue #8), `make worker-image` with a smoke run that
posts a verdict to a local chuck, `make smoke` (Litestream restore,
unchanged), and new `make e2e`: chuck with the Local launcher and a fake
GitHub drives one issue from intake to done.

## Rollout

### Phase 0: prerequisites

- chuck: migration 0004, the packages above, the dashboard, the worker
  image, the `chuck-workers` Fly app. Private key and OAuth client on the
  `orionsbelt-dev` App; permissions raised. New Fly secrets:
  `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_OAUTH_CLIENT_ID`,
  `GITHUB_OAUTH_CLIENT_SECRET`, `FLY_WORKERS_TOKEN`, `ANTHROPIC_API_KEY`,
  `CLAUDE_CODE_OAUTH_TOKEN`, `CHUCK_SESSION_KEY`.
- Each consumer repo: CI with required checks on main, a Playwright setup
  that targets `BASE_URL`, `deploy-dev` and `deploy-prod` workflows, a
  stable dev URL and `LOOP_ENV_<owner>_<repo>` if it needs credentials,
  `loop.json`, skills vendored and profile current.
- Manual steps in `docs/phase-0-human-steps.md` (replacing the Phase 1
  document) in the order they become possible.

### Phase 1: manual stepping

`auto_advance: false`. Every stage starts from a dashboard button. Run 3
to 5 issues on one Show Me The Bots demo. Tune prompts, caps, risk paths,
wall clocks.

### Phase 2: event driven, human promotion

`auto_advance: true`, `auto_merge: true`. Every prod promotion goes
through `awaiting-prod-approval`.

### Phase 3: auto-promote

`auto_promote: true` for changes outside the risk paths. Spot-check.

### Phase 4: more repos

Add the other demos. Cross-repo WIP and cost budgets if they start to
matter.

### Metrics on the dashboard

Escalation rate per stage with reasons, review rounds per PR, run
duration and timeouts per stage, validator false passes found by spot
checks, time from `ready` to `done`, cost per item, flaky-test count.

## Open questions

- Whether Claude Code's credit-exhausted error is stable enough to match
  on. Verify on the first exhaustion; until then the fallback is manual
  (set `api_key_exhausted_until` from the dashboard's `/spend`).
- `review-sweep`'s trusted-author rules and chuck's App comments: decide
  when the sweep prompt is tuned.
- The `cwinters8` App joining the loop: config and a key, no code, but
  untested until a personal repo needs it.
- Evidence size in practice. 20 MB is a guess; the cap is one constant.

## Changes to AGENTS.md and the profile

`AGENTS.md` invariants to replace: the ask_first, Slack allowlist, "Phase 1
executes nothing", routes and policy declarations, and per-tier budget
rules go. New invariants: state is committed before effects; workers
never hold chuck's App token; the validator token is read-only;
`awaiting-approval` is always a human gate; `loop.json` is a risk path;
additive migrations only for tables a worker callback touches. The
single-writer, Litestream, single-Machine and immutable-migration
invariants stay. "Design decisions live in Notion" becomes "design
decisions live in `docs/superpowers/specs/`". The project profile's
threat model, identity model, secrets policy and trust boundary are
rewritten to match this document in the same change.

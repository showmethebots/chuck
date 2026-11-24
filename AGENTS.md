# Agent rules

Rules for agents working in this repo. Design decisions live in
`docs/superpowers/specs/`; the current design is
`2026-11-03-delivery-loop-design.md`, and it is the source of truth for what
chuck should do. The Notion "Design & Plan" page for the project manager
agent is retired. This file records the conventions the code already
depends on. A loop invariant is added here by the change that makes it
true, not before.

## Invariants

- **Config decoding is strict.** Config files decode with
  `KnownFields(true)` (or `DisallowUnknownFields` for JSON), and validation
  reports every problem at once through `errors.Join`. A new config field
  needs a validation rule and a test. Validation must keep collecting
  errors rather than stopping at the first.
- **`config/chuck.yaml` holds no secrets.** `private_key` and
  `oauth_client_secret` may only be `${VAR}` references, and validation
  errors never quote a resolved value.
- **`loop.json` is a risk path.** `.claude/**` and `.github/**` are risk
  paths in every repo whatever its `risk_paths` lists (`alwaysRisk` in
  `internal/config`), so no repo can configure its way to auto-merging a
  change to its own loop config or workflows.
- **Shipped migrations are immutable.** To change the schema, add a new file
  under `internal/memory/migrations/`. Never edit one that has shipped.
- **`runs` and `evidence` are additive-only.** A worker launched before a
  deploy can post its verdict after it, so a migration may add a column
  with a default to those tables but never rename, retype or drop what a
  worker callback writes.
- **Tokens are stored hashed.** Run tokens and session cookies live in the
  database only as sha256 (`memory.HashToken`), so a copy of the database
  or its Litestream replica holds no usable credential.
- **SQLite runs in WAL mode with a single connection.** Litestream only
  replicates WAL databases, and `SetMaxOpenConns(1)` avoids `SQLITE_BUSY`
  between the daemon's own connections.
- **The app runs on exactly one Fly Machine.** The volume is pinned to it and
  Litestream assumes a single writer. Never scale above one.
- **Litestream is the backup.** `deploy/entrypoint.sh` refuses to start
  without a replica configured. `kill_timeout` in `deploy/fly.toml` gives
  Litestream time to flush on shutdown.
- **Workers are destroyed, not stopped.** Stopped Fly Machines still bill.
- **`chuck/loop` turns `success` only two ways.** The sweep-ready
  transitions, and a human merge approval of the reviewed head of a PR
  held for risk paths. A PR that touches a risk path holds it pending
  until that approval; a PR held for findings cannot be approved past
  them. The hold exists because
  a consumer repo's `CODEOWNERS` covers only `.claude/**` and `.github/**`
  and worker tokens can call the merge API.
- **State is committed before effects.** `internal/engine` writes every
  change and its transition row in one transaction before any GitHub
  call, with the effects on the row as pending, then runs them in order
  and records each result there, stopping at the first failure. A failed
  effect never rolls the state back; the tick retries the item's newest
  row, abandons an effect after three failing ticks and escalates the
  item. So every effect must be safe to run twice: a merge re-reads the
  PR and is never retried once the head has moved from the swept sha.
- **A worker holds its run token and nothing else of chuck's.** Its
  Machine's environment is exactly `CHUCK_URL`, `CHUCK_RUN_ID` and
  `CHUCK_RUN_TOKEN` (`TestMachineEnvIsOnlyTheRunVariables`): the Machine
  config is readable by anything holding the workers app's token. The
  prompt, GitHub tokens and validate credentials come from the run API,
  which refuses the token once the run leaves `running`. chuck's App key,
  its own installation tokens and the Claude credentials never leave chuck.
- **No worker token carries `statuses`, and validate's is read-only.**
  `github.StagePermissions` is the only source of a worker's permission
  set, `WorkerToken` refuses a token GitHub widened, and the run API
  checks again before serving one. So no worker can set `chuck/loop`.
- **Prompts carry feedback only from `humans` and chuck's App.**
  `worker.Trusted` decides by numeric user id, in code, before a prompt is
  built; everything else is recorded in `ignored_feedback` and is never
  placed in a prompt by chuck. App text counts only where the spec names
  it: sweep gets the review run's own findings comment (the App comment at
  the review verdict's `comment_url`, when `reviewed_sha` is the head being
  swept), and build gets none. Every other App comment or review
  (mentions, a worker's comments or reviews) is left out, since every
  worker posts as the App and could carry one worker's text into the
  next. This governs chuck's prompt, not what a worker can read: its token
  and the public API reach every comment.
- **`loop.Transition` does no I/O.** It decides from the item, the repo,
  the config, the input and the `Facts` the caller gathered, and returns
  changes and effects. The package imports no network, database or
  filesystem package (`TestPackageImportsNoIO`), so every rule is a table
  test.

## Conventions

- `cmd/chuck` receives `getenv`, stdout and stderr as arguments so tests
  can drive it without the process environment. Keep that seam.
- Every GitHub Action is pinned to a full commit SHA with the version in a
  trailing comment. To bump one, re-resolve the SHA for the new release.
  Don't edit the comment alone.
- A push to `main` deploys to production through the CI `deploy` job.
- `fly.toml` is at `deploy/fly.toml`, not the repo root. Every app-scoped
  `fly` command needs `-c deploy/fly.toml`.
- Manual steps a person must take for a phase live in
  `docs/phase-N-human-steps.md`, not in a README, in the order they become
  possible.
- Integrations turn on from the environment (see the `cmd/chuck` doc
  comment).
- The skills under `.claude/skills/` are vendored from
  `cwinters8/agent-skills`, pinned to a full commit SHA in the `Makefile`
  (`AGENT_SKILLS`). Pin a commit, not a tag: `v1` still points at package
  1.0.0, which predates `init` and several of the skills vendored here, and
  a tag can be repointed. Never hand-edit a vendored skill; `make
  skills-sync` overwrites it. Fix it upstream and re-sync.

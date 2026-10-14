# Agent rules

Rules for agents working in this repo. Design decisions live in Notion
(Project HQ → Autonomous project manager agent → Design & Plan). That page is
the source of truth for what the agent should do. This file records the
conventions the code already depends on.

## Invariants

- **ask_first actions route to `human`.** `internal/config` rejects any route
  that matches an `ask_first` action and sends it to a model tier. That check
  is what keeps Clark as the gate on every action listed under `ask_first` in
  `config/policy.yaml`. Don't weaken it, bypass it, or add a code path that
  takes one of those actions without going through it.
- **The Slack allowlist is `config/slack.yaml`, changed only by PR.** No
  chat command or runtime path adds a user or changes a role. Every inbound
  Slack interaction is checked against it before anything else happens.
- **Phase 1 executes nothing.** Approvals are recorded; no code path acts
  on GitHub, Notion, or anywhere else because of a decision or an
  approval.
- **Config decoding is strict.** `routes.yaml`, `policy.yaml`, and
  `slack.yaml` decode with `KnownFields(true)`, and validation reports every
  problem at once through `errors.Join`. A new config field needs a
  validation rule and a test.
  Validation must keep collecting errors rather than stopping at the first.
- **Every action a route names is declared in `policy.yaml`, in exactly one
  permission tier.**
- **Every model tier has a daily budget.** There is no "unlimited".
- **Shipped migrations are immutable.** To change the schema, add a new file
  under `internal/memory/migrations/`. Never edit one that has shipped.
- **SQLite runs in WAL mode with a single connection.** Litestream only
  replicates WAL databases, and `SetMaxOpenConns(1)` avoids `SQLITE_BUSY`
  between the daemon's own connections.
- **The app runs on exactly one Fly Machine.** The volume is pinned to it and
  Litestream assumes a single writer. Never scale above one.
- **Litestream is the backup.** `deploy/entrypoint.sh` refuses to start
  without a replica configured. `kill_timeout` in `deploy/fly.toml` gives
  Litestream time to flush on shutdown.
- **Workers are destroyed, not stopped.** Stopped Fly Machines still bill.

## Conventions

- `cmd/chuck` receives `getenv`, stdout and stderr as arguments so tests
  can drive it without the process environment. Keep that seam.
- Every GitHub Action is pinned to a full commit SHA with the version in a
  trailing comment. To bump one, re-resolve the SHA for the new release.
  Don't edit the comment alone.
- Values in `config/policy.yaml` marked `placeholder: not set in the design`
  are deliberate. They change when the design sets them.
- A push to `main` deploys to production through the CI `deploy` job.
- `fly.toml` is at `deploy/fly.toml`, not the repo root. Every app-scoped
  `fly` command needs `-c deploy/fly.toml`.
- Manual steps a person must take for a phase live in
  `docs/phase-N-human-steps.md`, not in a README, in the order they become
  possible.
- Integrations turn on independently from the environment (see the
  `cmd/chuck` doc comment). A partial Slack setup refuses to start.

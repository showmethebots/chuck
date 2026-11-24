# Project profile

## Rules source

`AGENTS.md`. `CLAUDE.md` only imports it (`@AGENTS.md`). The design specs in
`docs/superpowers/specs/` are the source of truth for intended behavior; the
current one is `2026-11-03-delivery-loop-design.md`.

## Repo

`showmethebots/chuck` (formerly `orionsbelt-dev/chuck`), public, MIT
licensed. Collaborators: `cwinters8` (Clark, maintainer) and `etho201` (a
human account). Being public, anyone can react, comment and open issues
and PRs, so review-sweep must require an `APPROVED` review from a
collaborator and must not infer approval from reactions or comments.

## Mechanical checks

test -z "$(gofmt -l .)"
make test
make validate
make lint
govulncheck ./...
make worker-image
make smoke
make skills-check

## Derived docs

| Canonical | Restated in |
| --- | --- |
| `deploy/fly.toml` (app name, region, volume name, single Machine, and its location under `deploy/`) | `deploy/README.md` intro note and bootstrap and exit-check commands (**followed against the Fly account**), `AGENTS.md` conventions (`-c deploy/fly.toml`), `Makefile` `deploy`, `.github/workflows/ci.yml` deploy step, the `fly.toml` header comment |
| `deploy/entrypoint.sh` / `deploy/litestream.yml` (replica URL, Tigris secrets) | `deploy/README.md` step 3 (**followed against the Fly account**) |
| `deploy/smoke-restore.sh` | `deploy/README.md` "Phase 0 exit check" |
| `internal/config` (validation rules) | `config/README.md`, the `config/chuck.yaml` header comment, `config/loop.schema.json` (kept agreeing by `TestSchemaAgreesWithValidator`), `AGENTS.md` invariants on config secrets and risk paths |
| `cmd/chuck/main.go` `retiredConfig` | `config/README.md` |
| `cmd/chuck/main.go` (env vars and defaults) | its package doc comment, `Dockerfile` `ENV`, `deploy/entrypoint.sh`, `deploy/litestream.yml`, `deploy/smoke-restore.sh`, `deploy/fly.toml` `[env]`, `Makefile` `run`, `docs/phase-1-human-steps.md` (secret and env names), `.env.example` (names checked against the doc comment by `cmd/chuck/dotenv_test.go`), `README.md` "Running locally" |
| `Makefile` targets | `README.md` "Development", this profile's `## Mechanical checks` |
| `AGENTS.md` invariants, `internal/github/webhook.go` (the order and scope of the webhook checks), `internal/github/app.go` `StagePermissions`, `.github/workflows/hook-ranges.yml` | `README.md` "Design choices worth a look" |
| Top-level and `internal/` directories, `cmd/chuck` subcommands | `README.md` "Layout" |
| `internal/engine` (executor order, retries, escalation, replay) | `README.md` "Design choices worth a look", `AGENTS.md` invariant on committing state before effects, `config/README.md` (loop on/off and the `loop.json` kill switch) |
| `internal/loop/types.go` states | `README.md` "The loop" diagram |
| Current and retired specs and plans in `docs/superpowers/` | `docs/superpowers/README.md` |
| `.github/workflows/ci.yml` checks | this profile's `## Mechanical checks` |
| `Makefile` `AGENT_SKILLS` (the agent-skills ref) | `AGENTS.md` conventions (pin a commit, not a tag), this profile's `## Dependencies` |
| Current phase | `README.md` status line, `deploy/worker.Dockerfile` header comment |

## Dependencies

- **Go modules:** `go.mod` and `go.sum` are authoritative. The toolchain is
  `go 1.27.3`, and it must match the `golang:1.27-*` images in `Dockerfile`
  and `deploy/worker.Dockerfile`.
- **Container base images:** `golang:1.27-alpine`, `alpine:3.24`,
  `golang:1.27-trixie` and `node:24-trixie-slim`. These are mutable tags,
  recorded under `## Known gaps`.
- **Litestream:** a downloaded release archive pinned by
  `LITESTREAM_VERSION` in `Dockerfile`, with a sha256 per architecture. A
  bump must update both checksums.
- **Claude Code** (worker image): an npm global pinned by `CLAUDE_CODE_VERSION`
  in `deploy/worker.Dockerfile`.
- **agent-skills:** the npm package `@cwinters8/agent-skills`, fetched by
  `npx` from GitHub and pinned to a full commit SHA in `Makefile`
  `AGENT_SKILLS`. It vendors `.claude/skills/` and writes the lock in
  `.claude/skills.json`. It has no dependencies of its own. Bump it with the
  `skills-adopt` skill's "Bumping the pinned ref" steps.
- **Alpine and apt packages:** `ca-certificates`, `git` and `openssh-client`.
  These are unpinned.
- **CI:** GitHub Actions are SHA-pinned in `.github/workflows/ci.yml` and
  `.github/workflows/hook-ranges.yml`.
  golangci-lint is pinned to `v2.14.0` in the same file.
  `superfly/flyctl-actions/setup-flyctl` is held at tag `1.6` on purpose. The
  latest release, 1.5, runs on node20, which hosted runners no longer support.

## Review focus

- The invariants in `AGENTS.md`. Weight single-writer state most heavily:
  WAL, one connection, one Machine and immutable migrations. Breaking it
  loses or forks the only copy of chuck's state.
- A change against the delivery loop spec's security rules (who can set
  `chuck/loop`, what a worker token carries, whose text reaches a prompt)
  is the critical failure once the loop lands, even before `AGENTS.md`
  records the rule.
- A change to the config schema has to land with matching validation and
  tests, and `make validate` has to keep passing on the shipped `config/`.
- A change to the Go version has to touch `go.mod` and both Dockerfiles
  together.

## Not findings

- `setup-flyctl` pinned at a tag with no matching GitHub release. The reason
  is under `## Exemptions`.

## Local skills

none

## Exemptions

- **action-versions:** `superfly/flyctl-actions/setup-flyctl` stays at `1.6`
  (SHA-pinned), not at the latest *release*. 1.6 is a stable tag with no
  GitHub release. Release 1.5 runs on node20, which hosted runners no longer
  support.

## Ship

Push a branch and open a draft PR with `gh`. CI runs on every PR, and a merge
to `main` deploys to production through the `production` environment.

## Threat model

The repo ships a container image to a single Fly Machine (`chuck`). Its
inbound surfaces are the unauthenticated `GET /healthz` behind Fly's HTTPS
proxy (version and boot count only) and `POST /webhooks/github`
(`internal/github`). The webhook first checks `Fly-Client-IP` against the
`hooks` ranges of `https://api.github.com/meta` (fetched at startup and
daily, keeping the last good list, built-in `internal/github/hooks.txt`
until the first fetch succeeds; every change is logged with the ranges
added and removed, and the weekly `hook-ranges` workflow pushes a
`hooks.txt` update to a branch and opens an issue for a person to PR) and answers 403 before reading the body or taking a read slot
when the sender is outside them or the header is missing or repeated.
`CHUCK_WEBHOOK_ALLOW_ANY_IP=1` skips that check for local runs behind a
tunnel and is logged at Warn on startup; it must never be set in
production. It then verifies GitHub's `X-Hub-Signature-256` with
HMAC-SHA256 against `GITHUB_APP_WEBHOOK_SECRET` (an empty secret rejects
every request rather than opening the endpoint), caps the body at 25 MB,
reads at most 2 bodies at once into exactly-sized buffers, and bounds both
the wait for a read slot (then 503) and the read itself (then 408) at 10 s.
The range check rests on Fly's proxy setting `Fly-Client-IP` itself. A
request that skips the proxy, such as one from another Machine in the same
Fly organization over the private network, can set the header, but that
only costs a read slot: it does not get past the signature check. For the
events the loop acts on, the signed payload must also match the unsigned
`X-GitHub-Event` header, so a captured body cannot be replayed as another
event. chuck's outbound calls are GitHub's meta endpoint
and, once the loop is on, the GitHub API with the App's installation
tokens: each scoped to one repository and `ChuckPermissions`, except the
discovery token that lists an installation's repositories, which carries
`metadata: read` and nothing else. What matters is the Fly app, its
volume, and the Tigris bucket holding the replicated state. The control
that guards them is credential custody. `FLY_API_TOKEN` is an app-scoped
deploy token that only the CI `deploy` job can reach, through the
`production` GitHub environment. The Tigris keys are Fly app secrets that
never enter the repo. The `ci` workflow runs on `pull_request` and its token
is `contents: read`, so a fork PR can't reach either credential. The
`hook-ranges` workflow runs only on a schedule or by dispatch, never on a
PR. Its token can write contents and issues, it doesn't keep checkout
credentials, and it can't open or approve PRs: "Allow GitHub Actions to
create and approve pull requests" stays off, so no workflow can satisfy the
human merge approval.

The repo is public, so its contents, issues, PRs and Actions logs are
readable by anyone, and anyone can open an issue, comment or fork PR.
Nothing committed may be a secret or a deployment detail that only matters
when hidden: the Fly app names and `chuck.fly.dev` are public by design,
and the webhook is protected by its checks, not by its URL being unknown.
A fork PR runs `ci`'s `test` and `images` jobs with a read-only token and
no secrets. Two repo settings back this up and are part of the model: the
Actions setting "Require approval for all external contributors", so a
stranger's PR runs nothing until a maintainer approves it, and the
`production` environment's deployment branches limited to `main`, so
`FLY_API_TOKEN` is reachable only from a push to `main` even if a later
workflow change drops the deploy job's `if:`. Text from issues and comments
is untrusted input. The loop acts on a delivery only through the
transition table, which trusts a label, push or approval only from a user
in `humans` (by numeric id, from the signed payload or, for a delivery the
tick replays, from GitHub's own event and review records), and reads
`chuck/loop` only from statuses chuck's App set. Comments chuck posts can
quote a worker's words, so it defuses `@` mentions and escapes the
hidden-marker opener in them. Feedback reaches a build or sweep prompt only
from `humans` and chuck's App, decided by numeric user id in
`worker.Trusted` before the prompt is built; chuck's own mention comments
are left out too, and everything else is recorded in `ignored_feedback`.

Workers are the second inbound path. `/api/runs/{id}/spec`, `/token` and
`/verdict` are public routes authenticated only by the run's own token (32
random bytes, stored as its sha256), checked against the run in the path,
and refused once the run has ended; a worker's Machine is given that token
and `CHUCK_URL`/`CHUCK_RUN_ID`, nothing else. A worker is untrusted: it
runs a model over a public repo's text, so what it posts is validated
against the stage schema again (a failure escalates), its evidence tar is
capped at 20 MB with paths cleaned, and its GitHub token is minted per call
from `github.StagePermissions` (never `statuses`, read-only for validate).
chuck's outbound calls gain Fly's Machines API, with an API token scoped to
the workers app (`FLY_WORKERS_API_TOKEN`), which never reaches a worker.

The loop is on only when every `${VAR}` in `config/chuck.yaml` resolves;
until then deliveries are stored as handled and nothing acts on them. The loop's own model (GitHub App tokens scoped
per worker run, the `chuck/loop` status only chuck can set, the Claude
proxy, the `humans` allowlist for dashboard logins and trusted feedback) is
in the spec's "Identities", "Who can merge", "The Claude proxy" and
"Trusted text" sections, and this model is rewritten as each piece lands.

## Trust boundary

| Path | Groups |
| --- | --- |
| `config/**` | authorization |
| `internal/config/**` | authorization |
| `internal/web/**` | client-data, auth-session, authorization |
| `internal/github/**` | secrets, authorization, client-data |
| `internal/memory/**` | client-data |
| `internal/loop/**` | authorization, client-data |
| `internal/engine/**` | authorization, client-data, secrets |
| `internal/worker/**` | auth-session, authorization, client-data, secrets |
| `cmd/**` | secrets, client-data |
| `deploy/**` | auth-session, secrets, supply-chain |
| `Dockerfile` | supply-chain, secrets |
| `go.mod` | supply-chain |
| `go.sum` | supply-chain |
| `.github/workflows/**` | supply-chain, secrets, release |
| `Makefile` | supply-chain, release |
| `docs/superpowers/specs/**` | authorization, auth-session, secrets, client-data, supply-chain, release |
| `docs/superpowers/plans/**` | authorization, auth-session, secrets, client-data, supply-chain, release |

## Stack

ci-workflows

## Identity model

There are no end users. The HTTP server has no authentication layer of its
own: `GET /healthz` is public through Fly's proxy (`deploy/fly.toml`
`[http_service]`, `force_https`), and any new route is public unless it
checks something itself, as `POST /webhooks/github` does with its
HMAC-SHA256 signature check (see `## Threat model`). GitHub identity is
two private GitHub Apps in production — one owned by `showmethebots` (formerly `orionsbelt-dev`), one
by `cwinters8`, sharing one webhook secret — plus a dev App for local
testing; all three get read-only repository permissions (Actions, Contents,
Issues, Metadata, Pull requests). GitHub makes each App generate a private
key before it can be installed, but the key is deleted rather than given to
chuck, since the loop is off and chuck makes no GitHub API calls until it
is on (`docs/phase-1-human-steps.md` section 1; #34 gives the App its key
and write permissions).
Deploys authenticate to Fly with an app-scoped `FLY_API_TOKEN` deploy
token, which is stored on the `production` GitHub environment. Litestream
authenticates to Tigris with `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`,
which `fly storage create` sets as Fly secrets. Operators reach the Machine
through `fly ssh console` with their own Fly account.

## Secrets policy

Real secrets live in two places only: Fly app secrets and the GitHub
`production` environment (`FLY_API_TOKEN`). The Fly app secrets are
`BUCKET_NAME` and the `AWS_*` Tigris credentials, plus
`GITHUB_APP_WEBHOOK_SECRET` (`docs/phase-1-human-steps.md` section 1.1) and
`ANTHROPIC_API_KEY`, which the retired shadow phase set with `fly secrets
set` and which is kept for the loop's Claude proxy. With the loop off, chuck
reads only the webhook secret; turning it on means setting every
reference in `config/chuck.yaml`. The Slack secrets are unset by
`docs/phase-1-human-steps.md` section 2. The loop's new secrets are listed
in the delivery loop spec's "Rollout / Phase 0", plus the workers'
`FLY_WORKERS_API_TOKEN` and each repo's validate credentials in
`LOOP_ENV_<OWNER>_<REPO>` (served only in a validate run's spec, never in a
Machine's config); their setup steps will
live in `docs/phase-0-human-steps.md`, which issue #34 writes and which does
not exist yet. `config/chuck.yaml` names secrets only as
`${VAR}` references, which `internal/config` enforces. `deploy/fly.toml` `[env]`, `Dockerfile` `ENV` and `config/`
are committed and baked into the image, so they hold non-secret settings
only. Local runs are the one exception to the two places: a developer's
credentials (the dev GitHub App's webhook secret) sit in a `.env` at the
repo root that `cmd/chuck` loads at startup. It is copied from the
committed `.env.example`, which holds empty placeholders only. Never commit
a secret, `.env`, a `.db` file or `data/`. `.gitignore` and `.dockerignore`
exclude the last three, so `.env` never enters a Docker build context,
which `make deploy` sends to Fly's remote builder.

## Known gaps

- **open:** Docker base images (`Dockerfile`, `deploy/worker.Dockerfile`) use
  mutable tags, not digests. This was recorded when the Phase 0 scaffold was
  committed.

## Release targets

Fly.io app `chuck`, deployed by the CI `deploy` job on push to
`main`. The worker image is built in CI but not published.

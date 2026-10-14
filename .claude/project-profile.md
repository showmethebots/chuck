# Project profile

## Rules source

`AGENTS.md`. `CLAUDE.md` only imports it (`@AGENTS.md`). The Notion design page
named there is the source of truth for intended behavior. It lives outside
the repo.

## Repo

`orionsbelt-dev/chuck`, private. Collaborators: `cwinters8` (Clark,
maintainer) and `etho201` (Erik Thomsen, a human account). There are two
possible reactors, so review-sweep must require an `APPROVED` review and must
not infer approval from reaction counts.

## Mechanical checks

test -z "$(gofmt -l .)"
make test
make validate
make lint
govulncheck ./...
make worker-image
make smoke

## Derived docs

| Canonical | Restated in |
| --- | --- |
| `deploy/fly.toml` (app name, region, volume name, single Machine, and its location under `deploy/`) | `deploy/README.md` intro note and bootstrap and exit-check commands (**followed against the Fly account**), `AGENTS.md` conventions (`-c deploy/fly.toml`), `Makefile` `deploy`, `.github/workflows/ci.yml` deploy step, the `fly.toml` header comment |
| `deploy/entrypoint.sh` / `deploy/litestream.yml` (replica URL, Tigris secrets) | `deploy/README.md` step 3 (**followed against the Fly account**) |
| `deploy/smoke-restore.sh` | `deploy/README.md` "Phase 0 exit check" |
| `internal/config/config.go` (validation rules) | `README.md` "Config", header comments in `config/routes.yaml` and `config/policy.yaml`, `AGENTS.md` (Slack allowlist invariant), `docs/phase-1-human-steps.md` section 3.3 (the `slack.yaml` shape) |
| `cmd/chuck/main.go` (env vars and defaults) | its package doc comment, `Dockerfile` `ENV`, `deploy/entrypoint.sh`, `deploy/litestream.yml`, `deploy/smoke-restore.sh`, `deploy/fly.toml` `[env]`, `Makefile` `run`, `docs/phase-1-human-steps.md` (secret and env names) |
| `Makefile` targets | `README.md` "Development", this profile's `## Mechanical checks` |
| `.github/workflows/ci.yml` checks | this profile's `## Mechanical checks` |
| Current phase | `README.md` status line, `deploy/worker.Dockerfile`, `internal/memory/migrations/0001_boots.sql` |

## Dependencies

- **Go modules:** `go.mod` and `go.sum` are authoritative. The toolchain is
  `go 1.27.2`, and it must match the `golang:1.27-*` images in `Dockerfile`
  and `deploy/worker.Dockerfile`.
- **Container base images:** `golang:1.27-alpine`, `alpine:3.24`,
  `golang:1.27-trixie` and `node:24-trixie-slim`. These are mutable tags,
  recorded under `## Known gaps`.
- **Litestream:** a downloaded release archive pinned by
  `LITESTREAM_VERSION` in `Dockerfile`, with a sha256 per architecture. A
  bump must update both checksums.
- **Claude Code** (worker image): an npm global pinned by `CLAUDE_CODE_VERSION`
  in `deploy/worker.Dockerfile`.
- **Alpine and apt packages:** `ca-certificates`, `git` and `openssh-client`.
  These are unpinned.
- **CI:** GitHub Actions are SHA-pinned in `.github/workflows/ci.yml`.
  golangci-lint is pinned to `v2.14.0` in the same file.
  `superfly/flyctl-actions/setup-flyctl` is held at tag `1.6` on purpose. The
  latest release, 1.5, runs on node20, which hosted runners no longer support.

## Review focus

- The invariants in `AGENTS.md`. Weight two of them most heavily:
  - the ask_first → `human` check in `internal/config`, because it is the
    product's authorization model;
  - single-writer state, which covers WAL, one connection, one Machine and
    immutable migrations. Breaking it loses or forks the only copy of the
    agent's memory.
- A change to the config schema has to land with matching validation and
  tests in `internal/config`, and `make validate` has to keep passing on the
  shipped `config/`.
- A change to the Go version has to touch `go.mod` and both Dockerfiles
  together.

## Not findings

- Values in `config/policy.yaml` commented `placeholder: not set in the
  design`. They are deliberate until the Notion design sets them.
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
(`internal/events`), which verifies GitHub's `X-Hub-Signature-256` with
HMAC-SHA256 against `GITHUB_APP_WEBHOOK_SECRET` — an empty secret rejects
every request rather than opening the endpoint — caps the body at 25 MB,
reads at most 2 bodies at once, and (via the server's 30 s `ReadTimeout`)
bounds how long a sender may take, so concurrent or slow requests cannot
exhaust the 256 MB Machine. chuck also makes an outbound Slack Socket Mode connection; inbound
commands and button clicks on it are checked against the `config/slack.yaml`
allowlist before anything else happens (`internal/notify`, enforced with
`internal/config`'s `Slack.RoleOf`). What matters is the Fly app, its
volume, and the Tigris bucket holding the replicated state. The control
that guards them is credential custody. `FLY_API_TOKEN` is an app-scoped
deploy token that only the CI `deploy` job can reach, through the
`production` GitHub environment. The Tigris keys are Fly app secrets that
never enter the repo. The workflow runs on `pull_request` and its token is
`contents: read`, so a fork PR can't reach either credential.

The product's own authorization model is the permission tiers in
`config/policy.yaml`, enforced by `internal/config`. A change that lets an
`ask_first` action reach a model tier is the critical failure. Phase 1 also
sends untrusted GitHub text (issue bodies, comments, review text) to Haiku
for triage (`internal/triage`); that text is data, never instructions —
the model's output is validated against `policy.yaml`'s declared actions,
a proposed `target` must be a `https://github.com/` URL, `</event>` is
escaped in the prompt so untrusted text can't close the tag early, and the
agent marker (`<!-- agent: <skill> -->`) is honoured only when the text's
own author (`user.login`) is `cwinters8`. Phase 1 executes nothing: triage
only records a decision and, for `ask_first` proposals, an approval — no
code path acts on either.

## Trust boundary

| Path | Groups |
| --- | --- |
| `config/**` | authorization |
| `internal/config/**` | authorization |
| `internal/daemon/**` | client-data, auth-session, authorization |
| `internal/events/**` | secrets, client-data |
| `internal/notify/**` | authorization, secrets |
| `internal/triage/**` | client-data, secrets, authorization |
| `internal/memory/**` | client-data |
| `cmd/**` | secrets, client-data |
| `deploy/**` | auth-session, secrets, supply-chain |
| `Dockerfile` | supply-chain, secrets |
| `go.mod` | supply-chain |
| `go.sum` | supply-chain |
| `.github/workflows/**` | supply-chain, secrets, release |
| `Makefile` | supply-chain, release |

## Stack

ci-workflows

## Identity model

There are no end users. The HTTP server has no authentication layer of its
own: `GET /healthz` is public through Fly's proxy (`deploy/fly.toml`
`[http_service]`, `force_https`), and any new route is public unless it
checks something itself, as `POST /webhooks/github` does with its
HMAC-SHA256 signature check (see `## Threat model`). GitHub identity is
two private GitHub Apps in production — one owned by `orionsbelt-dev`, one
by `cwinters8`, sharing one webhook secret — plus a dev App for local
testing; all three get read-only repository permissions (Actions, Contents,
Issues, Metadata, Pull requests) and no private key, since chuck makes no
GitHub API calls in Phase 1 (`docs/phase-1-human-steps.md` section 1).
Slack identity is the owner/member roles in `config/slack.yaml`
(`internal/config`'s `Slack.RoleOf`), changed only by a merged PR — no chat
command or runtime path can add a user or change a role (`AGENTS.md`).
Deploys authenticate to Fly with an app-scoped `FLY_API_TOKEN` deploy
token, which is stored on the `production` GitHub environment. Litestream
authenticates to Tigris with `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`,
which `fly storage create` sets as Fly secrets. Operators reach the Machine
through `fly ssh console` with their own Fly account.

## Secrets policy

Real secrets live in two places only: Fly app secrets and the GitHub
`production` environment (`FLY_API_TOKEN`). The Fly app secrets are
`BUCKET_NAME` and the `AWS_*` Tigris credentials, plus, as each integration
turns on, `GITHUB_APP_WEBHOOK_SECRET`, `ANTHROPIC_API_KEY`,
`SLACK_BOT_TOKEN`, and `SLACK_APP_TOKEN` (`docs/phase-1-human-steps.md`
sections 1-3). The Slack channel IDs (`CHUCK_SLACK_MAIN_CHANNEL`,
`CHUCK_SLACK_SHADOW_CHANNEL`) are set alongside those tokens but are not
secret. `deploy/fly.toml` `[env]`, `Dockerfile` `ENV` and `config/*.yaml`
are committed and baked into the image, so they hold non-secret settings
only. Never commit a secret, a `.db` file or `data/`. `.gitignore` and
`.dockerignore` exclude the last two.

## Known gaps

- **open:** Docker base images (`Dockerfile`, `deploy/worker.Dockerfile`) use
  mutable tags, not digests. This was recorded when the Phase 0 scaffold was
  committed.

## Release targets

Fly.io app `chuck`, deployed by the CI `deploy` job on push to
`main`. The worker image is built in CI but not published.

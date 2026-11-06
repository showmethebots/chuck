# Project profile

## Rules source

`AGENTS.md`. `CLAUDE.md` only imports it (`@AGENTS.md`). The design specs in
`docs/superpowers/specs/` are the source of truth for intended behavior; the
current one is `2026-11-03-delivery-loop-design.md`.

## Repo

`showmethebots/chuck` (formerly `orionsbelt-dev/chuck`), private. Collaborators: `cwinters8` (Clark,
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
- **CI:** GitHub Actions are SHA-pinned in `.github/workflows/ci.yml`.
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
added and removed, and a weekly workflow proposes `hooks.txt` updates by
PR) and answers 403 before reading the body or taking a read slot
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
and, once the executor lands, the GitHub API with the App's tokens. What matters is the Fly app, its
volume, and the Tigris bucket holding the replicated state. The control
that guards them is credential custody. `FLY_API_TOKEN` is an app-scoped
deploy token that only the CI `deploy` job can reach, through the
`production` GitHub environment. The Tigris keys are Fly app secrets that
never enter the repo. The workflow runs on `pull_request` and its token is
`contents: read`, so a fork PR can't reach either credential.

chuck stores webhook deliveries verbatim and acts on none of them while the
delivery loop is rebuilt. The loop's own model (GitHub App tokens scoped
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
chuck, since chuck makes no GitHub API calls yet
(`docs/phase-1-human-steps.md` section 1).
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
set` and which is kept for the loop's Claude proxy. chuck reads only the
webhook secret today. The Slack secrets are unset by
`docs/phase-1-human-steps.md` section 2. The loop's new secrets are listed
in the delivery loop spec's "Rollout / Phase 0"; their setup steps will
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

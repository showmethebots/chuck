<!-- markdownlint-disable-next-line -->
<p align="center"><img src="docs/chuck.png" alt="chuck, a woodchuck project manager holding a checklist" width="200"></p>

# chuck

The agent delivery loop on Fly.io. chuck moves GitHub issues through
intake, build, review, sweep, merge, validate and promote, launching a
fresh Claude Code worker on its own Fly Machine for each stage and deciding
every transition in code. The design is
[docs/superpowers/specs/2026-11-03-delivery-loop-design.md](docs/superpowers/specs/2026-11-03-delivery-loop-design.md);
that document and its successors are the source of truth. Implementation is
tracked on issue #17.

**Status: rebuilding.** The project-manager framing (Haiku triage, Slack,
routing and permission tiers) is retired. chuck serves `/healthz` and
stores signed GitHub App deliveries without acting on them while the loop
packages land.

## Layout

```text
cmd/chuck/          entrypoint: `run` (server), `validate` (config check)
internal/memory/    SQLite state (WAL, embedded migrations)
internal/events/    GitHub webhook receiver
internal/web/       HTTP server: /healthz, the webhook mount, heartbeat
config/             chuck-wide config, changed by PR (config/chuck.yaml lands with the loop)
deploy/             fly.toml, Litestream config, entrypoint, worker image, restore check
docs/               human steps, specs and plans
```

## Development

```sh
make test       # vet + race tests
make lint       # golangci-lint
make run        # chuck locally, state in ./data, no Litestream
make smoke      # build image, run the Litestream restore check
make skills-check  # vendored agent skills match the pinned ref
make skills-sync   # re-vendor them after bumping the ref
```

### Running locally

One-time setup (the dev GitHub App and the ngrok domain) is in
[docs/phase-1-human-steps.md](docs/phase-1-human-steps.md) section 1.5.
Each session after that:

1. `cp .env.example .env` the first time, then fill in what you need. An empty
   file runs chuck without the webhook. Variables set in your shell override `.env`, and so does the
   `CHUCK_HEARTBEAT=5s` that `make run` sets.
2. For real GitHub traffic, start the tunnel:
   `ngrok http --url=<your-domain> 8080`.
3. `make run` from the repo root.

Webhook deliveries made while the tunnel is down fail and are not retried;
redeliver them from the dev App's **Advanced** → **Recent Deliveries**.

Deploying: see [deploy/README.md](deploy/README.md).

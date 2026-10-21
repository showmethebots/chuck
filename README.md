<!-- markdownlint-disable-next-line -->
<p align="center"><img src="docs/chuck.png" alt="chuck, a woodchuck project manager holding a checklist" width="200"></p>

# chuck

An always-on project manager agent on Fly.io. It picks up events, routes
work across Haiku, Sonnet, Opus, and Clark, dispatches Claude Code workers,
and keeps Project HQ current. The design lives in Notion (Project HQ →
Autonomous project manager agent → Design & Plan); that page is the source
of truth.

**Status: Phase 1 (shadow mode).** The daemon receives GitHub App webhooks,
triages each event with Haiku into what it would have done, and posts
decisions to Slack for grading and (for `ask_first` actions) approval. It
announces each new deploy in `#chuck` and takes no other actions. Manual
setup is in [docs/phase-1-human-steps.md](docs/phase-1-human-steps.md); the
live check is [docs/shadow-checklist.md](docs/shadow-checklist.md).

## Layout

```text
cmd/chuck/          entrypoint: `run` (daemon), `validate` (config check), `replay` (triage agreement)
internal/config/    routes.yaml, policy.yaml, slack.yaml loader and schema validation
internal/memory/    SQLite state (WAL, embedded migrations): events, decisions, grades, approvals
internal/events/    GitHub webhook receiver, daily schedule
internal/triage/    noise rules, Haiku classifier, output validation, replay
internal/notify/    Slack: decision posts, outbox, /chuck commands, daily summary
internal/daemon/    heartbeat, HTTP endpoints, triage loop, daily jobs
config/             routes.yaml (routing), policy.yaml (permissions, budgets, caps), slack.yaml (allowlist)
deploy/             fly.toml, Litestream config, entrypoint, worker image, restore check
docs/               Phase 1 human steps, live checklist, specs and plans
```

## Config

All three files are strictly validated: unknown keys, unknown tiers,
malformed `attempts` comparisons, missing budgets or caps all fail, and
every problem is reported at once. Cross-file rules:

- every action a route names must be declared in `policy.yaml` permissions;
- each action belongs to exactly one permission tier;
- any route matching an `ask_first` action must route to `human`;
- every task a route names must be declared under `tasks` in `routes.yaml`;
  a dispatch task's name may not also be a `policy.yaml` action, and its
  description is required;
- `none` is reserved: no dispatch task or `policy.yaml` action may use it.
- `slack.yaml` is optional until Slack is on; it needs exactly one owner,
  every ID must look like a Slack user ID, and the owner is not also a
  member.

Check config without starting anything: `make validate`.

## Development

```sh
make test       # vet + race tests
make lint       # golangci-lint
make run        # daemon locally, state in ./data, no Litestream
make smoke      # build image, run the Litestream restore check
```

### Running locally

One-time setup (the dev GitHub App, the ngrok domain, the dev Slack app) is
in [docs/phase-1-human-steps.md](docs/phase-1-human-steps.md) sections 1.5
and 3.7. Each session after that:

1. `cp .env.example .env` the first time, then fill in what you need. Each
   integration turns on independently, so an empty file runs the bare
   daemon. Variables set in your shell override `.env`, and so does the
   `CHUCK_HEARTBEAT=5s` that `make run` sets.
2. For real GitHub traffic, start the tunnel:
   `ngrok http --url=<your-domain> 8080`.
3. `make run` from the repo root.

Webhook deliveries made while the tunnel is down fail and are not retried;
redeliver them from the dev App's **Advanced** → **Recent Deliveries**.

Deploying: see [deploy/README.md](deploy/README.md).

# chuck

An always-on project manager agent on Fly.io. It picks up events, routes
work across Haiku, Sonnet, Opus, and Clark, dispatches Claude Code workers,
and keeps Project HQ current. The design lives in Notion (Project HQ →
Autonomous project manager agent → Design & Plan); that page is the source
of truth.

**Status: Phase 1 (shadow mode), in progress.** The daemon receives GitHub
App webhooks, triages each event with Haiku, and records what it would have
done. It takes no actions. Manual setup steps are in
[docs/phase-1-human-steps.md](docs/phase-1-human-steps.md).

## Layout

```
cmd/chuck/          entrypoint: `run` (daemon), `validate` (config check)
internal/config/    routes.yaml + policy.yaml loader and schema validation
internal/memory/    SQLite state (WAL, embedded migrations)
internal/events/    GitHub webhook receiver
internal/daemon/    heartbeat loop and HTTP endpoints
internal/triage/    noise rules, Haiku classifier, output validation
config/             routes.yaml (routing rules), policy.yaml (permissions, budgets, caps)
deploy/             fly.toml, Litestream config, entrypoint, worker image, restore check
```

## Config

Both files are strictly validated: unknown keys, unknown tiers, malformed
`attempts` comparisons, missing budgets or caps all fail, and every problem
is reported at once. Cross-file rules:

- every action a route names must be declared in `policy.yaml` permissions;
- each action belongs to exactly one permission tier;
- any route matching an `ask_first` action must route to `human`.

Check config without starting anything: `make validate`.

## Development

```sh
make test       # vet + race tests
make lint       # golangci-lint
make run        # daemon locally, state in ./data, no Litestream
make smoke      # build image, run the Litestream restore check
```

Deploying: see [deploy/README.md](deploy/README.md).

# config

chuck-wide configuration, changed only by pull request.

- `chuck.yaml`: the humans chuck trusts, the GitHub Apps it acts as, where
  workers run, and the monthly spend caps. `${VAR}` is the only
  substitution and resolves from the environment; `private_key` and
  `oauth_client_secret` may only be references.
- `loop.schema.json`: the JSON Schema for a consumer repo's
  `.claude/loop.json`, for editors and consumer CI. chuck's own validator
  in `internal/config` decides, and a test keeps the two agreeing.

Risk-path globs in `loop.json` are matched from the repo root
(`package.json` is the root file only; `**/package.json` is any), and
`.claude/**` and `.github/**` are risk paths in every repo whatever the
file lists.

chuck turns the loop on only when every `${VAR}` in `chuck.yaml` resolves,
the OAuth ones included; until then it serves `/healthz`, stores webhook
deliveries as handled, and acts on none of them. Each tick it reads every
installed repo's `loop.json` from the default branch: a repo is in the
loop exactly when that file exists, is valid, and names the repo's real
default branch. Deleting the file is how to take a repo out; there is no
off switch that a later tick would not undo.

Check everything with `make validate`, or a consumer's file with
`go run ./cmd/chuck validate path/to/loop.json`.

The project-manager framing's `routes.yaml`, `policy.yaml` and `slack.yaml`
are retired. `chuck validate` refuses a directory that still holds one.

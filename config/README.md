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

Check everything with `make validate`, or a consumer's file with
`go run ./cmd/chuck validate path/to/loop.json`.

The project-manager framing's `routes.yaml`, `policy.yaml` and `slack.yaml`
are retired. `chuck validate` refuses a directory that still holds one.

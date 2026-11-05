# config

chuck-wide configuration, changed only by pull request. `config/chuck.yaml`
(humans, Apps, workers, spend caps) lands with the loop; its shape is in
the "Config" section of
`docs/superpowers/specs/2026-11-03-delivery-loop-design.md`.

The project-manager framing's `routes.yaml`, `policy.yaml` and `slack.yaml`
are retired. `chuck validate` refuses a directory that still holds one.

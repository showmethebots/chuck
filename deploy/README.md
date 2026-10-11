# Deploying to Fly.io

One-time bootstrap. Run from the repo root. CI's `deploy` job only works
once these steps are done.

`fly.toml` lives in `deploy/`, not the repo root, so flyctl does not find it
on its own. Pass `-c deploy/fly.toml` to every app-scoped `fly` command;
without it flyctl has no app name and fails or prompts.

1. **Create the app** without deploying. The name in `fly.toml` must be
   globally unique on Fly; change `app` there if `chuck` is taken.
   Also check `primary_region`.

   ```sh
   fly auth login
   fly apps create chuck
   ```

2. **Create the volume** in the same region as `primary_region`. Exactly one:
   the app runs on a single Machine and Litestream assumes one writer.

   ```sh
   fly volumes create chuck_data -c deploy/fly.toml --region iad --size 1
   ```

3. **Create the Tigris bucket.** This sets `BUCKET_NAME`, `AWS_ACCESS_KEY_ID`,
   `AWS_SECRET_ACCESS_KEY`, `AWS_ENDPOINT_URL_S3`, and `AWS_REGION` as app
   secrets. The entrypoint builds the Litestream replica URL from
   `BUCKET_NAME`.

   ```sh
   fly storage create -c deploy/fly.toml --name chuck-state
   ```

4. **First deploy** by hand to confirm it comes up:

   ```sh
   make deploy
   fly logs -c deploy/fly.toml   # expect "heartbeat" lines every minute
   ```

5. **Hand deploys to CI.** Create a deploy token scoped to this app only and
   store it as the `FLY_API_TOKEN` secret on a GitHub environment named
   `production` (Settings → Environments). Add a required reviewer there if
   you want to approve each deploy.

   ```sh
   fly tokens create deploy -c deploy/fly.toml --expiry 2160h
   ```

## Phase 0 exit check

The heartbeat shows up in `fly logs`, and state survives losing the Machine
and its volume:

```sh
fly ssh console -c deploy/fly.toml -C "wget -qO- localhost:8080/healthz"   # note boot_count = N
fly machine destroy <id> -c deploy/fly.toml --force
fly volumes destroy <vol-id> -c deploy/fly.toml
fly volumes create chuck_data -c deploy/fly.toml --region iad --size 1
fly deploy . --config deploy/fly.toml --dockerfile Dockerfile
fly ssh console -c deploy/fly.toml -C "wget -qO- localhost:8080/healthz"   # want boot_count = N+1
```

`deploy/smoke-restore.sh` runs the same sequence locally and in CI, with a
directory standing in for Tigris.

## Gotchas

- Stopped Machines still bill; workers (Phase 3) are destroyed, not stopped.
- Volumes are pinned to one Machine. Never `fly scale count` above 1.
- Volume snapshots cost money; Litestream is the backup.

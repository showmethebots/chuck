# Phase 1: manual steps for Clark

The shadow-mode Phase 1 is retired; the delivery loop replaces it
(`docs/superpowers/specs/2026-11-03-delivery-loop-design.md`). Section 1,
the GitHub Apps, still describes what production runs, and section 2 undoes
the rest. `docs/phase-0-human-steps.md` replaces this file when the loop's
own setup is written.

## 1. GitHub Apps (done)

chuck receives GitHub activity through GitHub Apps. A private App can only be
installed on the account that owns it, so production needs two: one owned by
`orionsbelt-dev` and one owned by `cwinters8`. Both send to the same URL with
the same secret. App names are global on GitHub, so use the names below or
any free ones.

### 1.1 Generate the webhook secret and give it to chuck

```sh
SECRET=$(openssl rand -hex 32)
echo "$SECRET"   # keep this for 1.2 and 1.3
fly secrets set GITHUB_APP_WEBHOOK_SECRET="$SECRET" -c deploy/fly.toml
```

Setting a secret restarts the Machine. Check it picked it up:

```sh
curl -s https://chuck.fly.dev/healthz
# "integrations":["github_webhook"]
```

### 1.2 Create the `orionsbelt-dev` App

1. Open https://github.com/organizations/orionsbelt-dev/settings/apps/new
2. **GitHub App name:** `chuck-orion`
3. **Homepage URL:** `https://github.com/orionsbelt-dev/chuck`
4. **Webhook:** Active. **Webhook URL:** `https://chuck.fly.dev/webhooks/github`.
   **Webhook secret:** the value from 1.1.
5. **Repository permissions**, each **Read-only**. Leave everything else at
   No access.
   - [ ] Actions
   - [ ] Contents
   - [ ] Issues
   - [ ] Metadata
   - [ ] Pull requests
6. **Subscribe to events.** Do not tick Check run, Check suite or Status.
   - [ ] Issue comment
   - [ ] Issues
   - [ ] Pull request
   - [ ] Pull request review
   - [ ] Pull request review comment
   - [ ] Push
   - [ ] Workflow run
7. **Where can this GitHub App be installed?** Only on this account.
8. Create. GitHub won't install an App until it has a private key, so click
   **generate a private key** in the banner (or **General** → **Private
   keys** → **Generate a private key**). It downloads a `.pem` file. Delete
   it: chuck makes no GitHub API calls in Phase 1, and anyone holding the key
   can read every repository the App is installed on. Don't give it to Fly or
   save it anywhere. When a later phase needs the API, generate a new one.
9. **Install App** → `orionsbelt-dev` → **All repositories**.

### 1.3 Create the `cwinters8` App

Same as 1.2, except:

1. Open https://github.com/settings/apps/new
2. **GitHub App name:** `chuck-winters`
3. **Install App** → `cwinters8` → **Only select repositories**:
   `agent-ci-kit`, `agent-skills`.

### 1.4 Check deliveries arrive

1. In each App: **Advanced** → **Recent Deliveries**. The `ping` from
   creation should show response **202**. A 401 means the secret differs from
   1.1.
2. Deliveries are stored and not acted on until the loop lands; that is
   expected.

### 1.5 Dev App (can wait until local testing)

Only needed to run chuck on your laptop against real GitHub traffic.

1. `brew install ngrok`, sign up at https://ngrok.com, and run
   `ngrok config add-authtoken <token>` with the token from the dashboard.
2. In the ngrok dashboard → **Domains**, claim the free static domain
   (`….ngrok-free.app`). It stays the same across sessions.
3. Start the tunnel: `ngrok http --url=<your-domain> 8080`.
4. Same as 1.2 with: name `chuck-orion-dev`; **Webhook URL**
   `https://<your-domain>/webhooks/github`, not the production URL; a
   **new** secret from `openssl rand -hex 32` (put it in your local `.env`,
   copied from `.env.example`, as `GITHUB_APP_WEBHOOK_SECRET`); and
   **Install** on `orionsbelt-dev` → **Only select repositories** → `demos`.
5. Run chuck from the repo root with `make run`. Later sessions follow
   "Running locally" in the [README](../README.md#running-locally); the
   Webhook URL does not change.

## 2. Retire the shadow (after the retirement PR merges and deploys)

chuck no longer reads the Slack secrets, and nothing posts to Slack.

1. `fly secrets unset SLACK_BOT_TOKEN SLACK_APP_TOKEN CHUCK_SLACK_MAIN_CHANNEL CHUCK_SLACK_SHADOW_CHANNEL -c deploy/fly.toml`.
   Keep `ANTHROPIC_API_KEY`: the loop's Claude proxy uses it.
2. Delete the chuck Slack app (and the dev Slack app, if you made one) at
   https://api.slack.com/apps, and archive the channels it posted to if
   you no longer want them.
3. Check: `curl -s https://chuck.fly.dev/healthz` shows
   `"integrations":["github_webhook"]`.

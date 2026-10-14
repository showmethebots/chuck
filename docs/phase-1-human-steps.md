# Phase 1: manual steps for Clark

Everything in Phase 1 that a person has to do by hand, in the order it
becomes possible. chuck's session tells you when each section is ready; don't
start a section before then. Each section ends with how to check it worked.

Spec: `docs/superpowers/specs/2026-10-13-phase-1-shadow-design.md`.

## 1. GitHub Apps (after the slice A PR merges and deploys)

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
2. **GitHub App name:** `orionsbelt-chuck`
3. **Homepage URL:** `https://github.com/orionsbelt-dev/chuck`
4. **Webhook:** Active. **Webhook URL:** `https://chuck.fly.dev/webhooks/github`.
   **Webhook secret:** the value from 1.1.
5. **Repository permissions**, all **Read-only**: Actions, Contents, Issues,
   Metadata, Pull requests. Leave everything else at No access.
6. **Subscribe to events:** Issue comment, Issues, Pull request, Pull request
   review, Pull request review comment, Push, Workflow run. Do not tick Check
   run, Check suite or Status.
7. **Where can this GitHub App be installed?** Only on this account.
8. Create. You do not need to generate a private key; chuck makes no GitHub
   API calls in Phase 1.
9. **Install App** → `orionsbelt-dev` → **All repositories**.

### 1.3 Create the `cwinters8` App

Same as 1.2, except:

1. Open https://github.com/settings/apps/new
2. **GitHub App name:** `cwinters8-chuck`
3. **Install App** → `cwinters8` → **Only select repositories**:
   `agent-ci-kit`, `agent-skills`, `content-workshop`.

### 1.4 Check deliveries arrive

1. In each App: **Advanced** → **Recent Deliveries**. The `ping` from
   creation should show response **202**. A 401 means the secret differs from
   1.1.
2. `curl -s https://chuck.fly.dev/healthz` shows `queue_depth` above 0 once
   real activity arrives. Until triage ships (section 2), events only
   accumulate; that is expected.

### 1.5 Dev App (can wait until local testing)

Only needed to run chuck on your laptop against real GitHub traffic.

1. `brew install cloudflared`
2. Same as 1.2 with: name `orionsbelt-chuck-dev`, a **new** secret from
   `openssl rand -hex 32` (put it in your local `.env` as
   `GITHUB_APP_WEBHOOK_SECRET`), and **Install** on `orionsbelt-dev` →
   **Only select repositories** → `demos`.
3. Each local session: `cloudflared tunnel --url http://localhost:8080`,
   then paste the printed `https://….trycloudflare.com/webhooks/github` URL
   into the dev App's Webhook URL. The URL changes every time the tunnel
   restarts.
4. Run chuck with your `.env` loaded: `set -a; . ./.env; set +a; make run`

## 2. Anthropic API key (after the slice B PR merges and deploys)

Triage calls Claude Haiku. chuck stops itself at `budgets.daily_usd.haiku` in
`config/policy.yaml` (currently $5/day); a Console spend limit is the
backstop if that ever fails.

1. In https://console.anthropic.com → **Settings** → **API keys**, create a key
   named `chuck-prod`.
2. **Settings** → **Limits**: set a monthly spend limit for the workspace the
   key belongs to. $30 is ample for Phase 1 (expected spend is a few dollars a
   week).
3. Give it to chuck:

   ```sh
   fly secrets set ANTHROPIC_API_KEY=sk-ant-... -c deploy/fly.toml
   ```

4. Check: `curl -s https://chuck.fly.dev/healthz` shows
   `"integrations":["github_webhook","triage"]`, and `queue_depth` falls to
   near 0 within a few minutes as the backlog from section 1 is triaged.
   `fly logs -c deploy/fly.toml | grep '"msg":"decision"'` shows decisions.
   If instead `fly logs` repeats `classifier unavailable; triage backs off`
   with an error such as `authentication_error`, the key is wrong or Anthropic
   is down. Triage is pausing itself (1 minute, then every 5 minutes) and no
   events are lost or marked failed: fix the key with `fly secrets set` as in
   step 3 and triage resumes on its own.

## 3. Slack (after the slice C PR merges and deploys)

chuck talks to Slack over Socket Mode, so Slack needs no public URL. Order
matters: the allowlist must be deployed **before** the Slack secrets are
set, because chuck refuses to start with Slack on and no allowlist.

### 3.1 Channels

Create `#chuck` and `#chuck-shadow` (and `#chuck-dev` if you will run chuck
locally). Note each channel's ID: open the channel → click its name → the ID
is at the bottom of the About tab (starts with `C`).

### 3.2 Create the app

1. https://api.slack.com/apps → **Create New App** → **From a manifest** →
   your workspace → paste:

   ```yaml
   display_information:
     name: chuck
   features:
     bot_user:
       display_name: chuck
       always_online: true
     slash_commands:
       - command: /chuck
         description: status, pause, kill, resume
         usage_hint: status | pause | kill | resume
         should_escape: false
   oauth_config:
     scopes:
       bot:
         - chat:write
         - commands
   settings:
     interactivity:
       is_enabled: true
     socket_mode_enabled: true
     org_deploy_enabled: false
     token_rotation_enabled: false
   ```

2. **Install App** → Install to workspace. Copy the **Bot User OAuth Token**
   (`xoxb-…`).
3. **Basic Information** → **App-Level Tokens** → **Generate Token and
   Scopes**, name `socket`, scope `connections:write`. Copy it (`xapp-…`).
4. In `#chuck` and `#chuck-shadow`: `/invite @chuck`.

### 3.3 Your Slack user ID, then the allowlist PR

1. Click your avatar → **Profile** → **⋮** → **Copy member ID** (starts with
   `U`).
2. Send it to the chuck session. It opens a PR adding `config/slack.yaml`
   with you as owner and no members. Merge it and wait for the deploy to
   finish (`gh run watch` or the Actions tab).

### 3.4 Give chuck the Slack secrets

```sh
fly secrets set -c deploy/fly.toml \
  SLACK_BOT_TOKEN=xoxb-... \
  SLACK_APP_TOKEN=xapp-... \
  CHUCK_SLACK_MAIN_CHANNEL=C... \
  CHUCK_SLACK_SHADOW_CHANNEL=C...
```

### 3.5 Check

1. `curl -s https://chuck.fly.dev/healthz` lists `"slack"` in `integrations`.
2. In `#chuck`: `/chuck status` replies (only you see it) with `running`.
3. Grade posts start appearing in `#chuck-shadow` as events arrive.

Expect a burst on the first connection. Every decision queued since triage
went live (section 2) posts now: grade posts in `#chuck-shadow` and approval
requests in `#chuck`, about 20 every 5 seconds until the backlog clears.
`fly logs` may show Slack rate-limit warnings while it drains; posts that hit
one are retried, not lost. These are real decisions: grade them, and they
count toward the 100-graded target. Old approval requests can be answered or
ignored.

### 3.6 Second Slack account

For the live checklist (section 4). Invite a second email address of yours to
the workspace, sign in as it in another browser profile, join both `#chuck`
and `#chuck-shadow` (the checklist clicks buttons in `#chuck-shadow`), and
copy its member ID as in 3.3. Do not add it to `config/slack.yaml` yet.

### 3.7 Dev app (can wait until local testing)

Repeat 3.2 with `name: chuck-dev`, `display_name: chuck-dev` and
`command: /chuck-dev`, invite it to `#chuck-dev`, and put its tokens and
`#chuck-dev`'s ID (as both channel variables) in your local `.env`. Run
locally with `set -a; . ./.env; set +a; make run`.

## 4. Live checklist (after section 3)

Run `docs/shadow-checklist.md` start to finish with your owner account and
the second account. Report anything that differs in the chuck session.

## 5. The shadow week

The week starts once the checklist passes.

- **Daily:** grade the posts in `#chuck-shadow` and answer approvals in
  `#chuck`. Grade what chuck would do, not whether the event mattered: 👍
  when the proposed action (or "nothing") is what you would want, 👎 when
  it is not. The daily summary in `#chuck` tracks progress against the exit
  criterion.
- **If grading is too much:** say so in the chuck session; the sample size
  (`CHUCK_GRADE_SAMPLE`) and the noise rules can change.
- **End of week:** with at least 100 graded decisions, run the replay on
  the live database (read-only):

  ```sh
  fly ssh console -c deploy/fly.toml -C "env CHUCK_CONFIG_DIR=/app/config chuck replay --db /data/chuck.db"
  ```

  If the report says it stopped early, re-run with a higher `--max-usd`
  (the default cap is $1).

  Exit needs, together: 7 days running, at least 100 graded, at least 90%
  👍, zero 👎 on `ask_first` proposals, `0 violations` on the daily
  summary's Allowlist line, and the replay's `👍 agreement` at least 90%.

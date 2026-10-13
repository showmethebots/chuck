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

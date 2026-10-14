# Phase 1 live checklist

Run after the first Slack deploy, and again after any change to Slack
handling. You need your owner account and the second account from
`docs/phase-1-human-steps.md` section 3.6. Each step lists what you should
see; anything else is a failure to report in the chuck session.

1. **Owner status.** As owner in `#chuck`: `/chuck status`.
   See: a reply only you can see, starting `chuck <version>: running`.
2. **Stranger is ignored.** As the second account (not yet on the
   allowlist): `/chuck status`, then `/chuck pause`.
   See: no reply at all, and `/chuck status` as owner still says `running`.
   As the second account, click 👍 on any post in `#chuck-shadow`. See:
   nothing changes on the post.
3. **Member powers.** Send the second account's member ID to the chuck
   session; it opens a PR adding it under `members:` in `config/slack.yaml`.
   Merge and wait for the deploy. Then, as the second account:
   - `/chuck status` replies.
   - `/chuck pause` replies `Paused.`, and `#chuck` shows who paused.
   - `/chuck resume` replies that it needs the owner; status stays paused.
   - Click 👍 on a post in `#chuck-shadow`: you are told only the owner can
     grade, and the post does not change.
4. **Owner resume.** As owner: `/chuck resume`. See `Resumed.` and an
   announcement in `#chuck`.
5. **Pause survives a deploy.** As owner: `/chuck pause`. Then
   `fly apps restart chuck -c deploy/fly.toml`. After it is back:
   `/chuck status` still says paused. `/chuck resume`.
6. **Approval round trip.** On `orionsbelt-dev/demos`: open a small PR,
   approve it as a different GitHub user if you can (or leave a comment
   saying it is ready to merge), and let CI pass. Within a minute or two of
   the CI run completing, `#chuck` shows a post headed *SHADOW — nothing
   will execute* proposing `merge`. As the second account, click Approve.
   See: the buttons disappear and the post says `Approved by @second`. As
   owner, click 👍 on it. See: `Graded 👍`. If triage proposes something
   other than `merge`, grade it 👎 — that is real signal, not a checklist
   failure.
7. **First answer wins.** On another approval post, click Approve as the
   second account, then Deny as owner. See: the owner is told it was
   already answered; the post still says Approved.
8. **Grade post arrives.** Open an issue on `demos`. See: a post in
   `#chuck-shadow` within a minute or two.

Afterwards, remove the second account from `members:` only if you do not
want it to keep member powers; it is harmless to leave.

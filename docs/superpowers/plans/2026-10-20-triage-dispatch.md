# Triage Dispatches Tasks Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Haiku triage proposes a worker task (or an `ask_first` action, or nothing) with a one-line ask, instead of picking from every policy action. The Slack post leads with the task and the ask. The Phase 1 exit counts restart at this deploy.

**Architecture:** `routes.yaml` gains a validated `tasks` section. `triage.Menu`, built from config, replaces `ProposableActions` and drives the prompt, the JSON schema and `ParseResult`. A new migration adds `task`, `ask` and a `triage_version` marker to `decisions`. The summary and replay count only the current version. The daemon picks the post kind from task/permission, and `notify` renders the new post.

**Tech Stack:** Go, SQLite (modernc, embedded migrations), `go.yaml.in/yaml/v3` strict decoding, slack-go Block Kit, Anthropic Go SDK structured outputs.

**Spec:** `docs/superpowers/specs/2026-10-20-triage-dispatch-design.md`

## Global Constraints

- Task kinds, exactly: `address_review`, `fix_ci`, `implement_issue`, `review_pr`.
- Haiku may propose only `none`, a dispatch task, or a `policy.yaml` `ask_first` action.
- Phase 1 executes nothing. No new code path acts on GitHub/Notion.
- The `ask_first` → `human` route check in `internal/config` is unchanged.
- Config decoding stays `KnownFields(true)`. Every new rule collects into `errors.Join`, and every rule gets a test.
- `0001_boots.sql` and `0002_shadow.sql` are not edited. The schema change is a new `0003_dispatch.sql`.
- `ask` and rationale go through `notify.field()` with `maxRationale`.
- No tier evaluation of `routes.yaml` and no code analysis (spec non-goals).
- Mechanical checks before push: `test -z "$(gofmt -l .)" && make test && make validate && make lint && govulncheck ./...`

## Review Focus

1. **Pre-change grade posts re-rendered after a click.** They have `Task == ""` and old `would_do` values like `comment_issue`. They must still render "chuck would `comment_issue` …", not a blank task headline. Test in Task 5.
2. **Model sets both `task` and `would_do`, or proposes with an empty `ask`.** Either must become `triage_failed`, never a post. Tests in Task 3.
3. **`ask` carrying `<!channel>`, `<https://evil|label>` or 5000 chars.** It must be escaped and bounded like the rationale. Test in Task 5.
4. **A dispatch task that is never posted.** It must not be picked up by the daily quiet sample as a "would do nothing" decision. Test in Task 2.
5. **Internal task `triage` sharing its name with the `act_freely` action `triage`.** The shipped config must keep loading (`TestLoadShippedConfig` plus the internal-exemption case in Task 1).

---

### Task 1: Declare tasks in routes.yaml

**Files:**
- Modify: `internal/config/config.go` (`Routes` struct, new `Tasks`/`Task` types, `validate`)
- Modify: `config/routes.yaml`
- Modify: `AGENTS.md` (invariants)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces:
  ```go
  type Routes struct { …; Tasks Tasks `yaml:"tasks"` }
  type Tasks struct {
      Dispatch []Task   `yaml:"dispatch"` // tasks triage may hand to a worker, in prompt order
      Internal []string `yaml:"internal"` // tasks chuck schedules itself; never proposed by triage
  }
  type Task struct {
      Name        string `yaml:"name"`
      Description string `yaml:"description"` // one line, shown to the triage model
  }
  ```

- [ ] **Step 1: Extend the test fixture and write failing tests**

  Append to `validRoutes` in `config_test.go`, keeping the existing two routes first so index-based assertions hold:
  ```yaml
    - match: {task: triage}
      tier: haiku
  tasks:
    dispatch:
      - {name: fix_ci, description: A CI run failed or timed out.}
    internal: [triage]
  ```
  (Put the route under `routes:` and `tasks:` at top level.)

  In `TestLoadParsesValues`, assert `cfg.Routes.Tasks.Dispatch` is `[{fix_ci, "A CI run failed or timed out."}]` and `Internal` is `[triage]`. This also proves an internal task may share a name with an action.

  Add to `TestLoadRejectsInvalid`:

  | name | routes replacement | wantErr |
  |---|---|---|
  | unknown tasks key | `internal:\|intrnal:` | `intrnal` |
  | empty task name | `internal: [triage]\|internal: [triage, ""]` | `tasks.internal[1]: empty name` |
  | task declared twice | `internal: [triage]\|internal: [triage, fix_ci]` | `task "fix_ci" is declared twice` |
  | dispatch task named like an action | `name: fix_ci\|name: merge` | `dispatch task "merge" is also a policy.yaml action` |
  | dispatch task without description | `description: A CI run failed or timed out.\|description: ""` | `tasks.dispatch[0]: description is required` |
  | undeclared route task | `{task: triage}\|{task: digset}` | `task "digset" is not declared in tasks` |

- [ ] **Step 2: Run to verify they fail**

  Run: `go test ./internal/config/`
  Expected: FAIL. `field tasks not found` for every case, since the struct lacks the field.

- [ ] **Step 3: Implement**

  Add the types and the `Tasks` field. In `validate`, before the per-route loop, walk `Dispatch` then `Internal` with `routesErr`:
  - empty name → `tasks.<list>[i]: empty name`
  - a name seen before → `task %q is declared twice`
  - a dispatch name that is a key of `seen` (the policy action map) → `dispatch task %q is also a policy.yaml action`
  - an empty dispatch description → `tasks.dispatch[i]: description is required`

  Collect the declared names in a set. In the route loop, for each non-empty `m.Task` value not in the set, report `%s: task %q is not declared in tasks` with the route's `at`.

- [ ] **Step 4: Update the shipped config**

  In `config/routes.yaml`, add a commented `tasks` section after `routes:`. Use these descriptions verbatim:
  ```yaml
  # Tasks routes can match. dispatch: work triage may hand to a worker; the
  # description is what the triage prompt shows. internal: work chuck
  # schedules itself, never proposed by triage. A dispatch task may not share
  # a name with a policy.yaml action.
  tasks:
    dispatch:
      - name: address_review
        description: Review feedback asking for a change or raising a problem. The worker reads the code and decides whether to reply or fix.
      - name: fix_ci
        description: A CI run failed or timed out.
      - name: implement_issue
        description: An issue asks for something actionable.
      - name: review_pr
        description: A PR is ready for a first-pass review.
    internal: [plan_goal, triage, digest, notion_sync]
  ```
  Also extend the header's `task:` key comment: "each must be declared under tasks".

  Add to the AGENTS.md invariants, after the "Every action a route names" bullet:
  "**Every task a route names is declared under `tasks` in `routes.yaml`.** Dispatch tasks are what triage may hand to a worker and may not share a name with a `policy.yaml` action."

- [ ] **Step 5: Run and verify**

  Run: `go test ./internal/config/ && make validate`
  Expected: PASS, and `chuck validate` reports the config valid.

- [ ] **Step 6: Commit**

  `git commit -m "Config: declare routable tasks in routes.yaml and validate them"`

---

### Task 2: Store task, ask and triage version

**Files:**
- Create: `internal/memory/migrations/0003_dispatch.sql`
- Modify: `internal/memory/decisions.go` (`Decision`, `RecordDecision`, `viewQuery`, `scanView`, `SampleQuiet`)
- Modify: `internal/memory/summary.go`, `internal/memory/replay.go`
- Test: `internal/memory/decisions_test.go`, `summary_test.go`, `replay_test.go`

**Interfaces:**
- Produces:
  ```go
  // TriageVersion marks decisions made under the current triage vocabulary.
  // Version 1 is everything before 0003_dispatch.sql (policy actions only).
  const TriageVersion = 2
  type Decision struct { …; Task string; Ask string } // Task: a dispatch task, "none", or "" (rule/failed/pre-change)
  ```
  `RecordDecision` always writes `triage_version = TriageVersion`. `Summary`'s all-time grade counts (`Graded`, `Up`, `AskFirstDown`, `PendingGrades`) and `GradedEvents` include only `triage_version = TriageVersion`. `PendingGrades` is filtered too: otherwise old posts nobody will grade would show as "waiting" forever.

- [ ] **Step 1: Write failing tests**

  Add a test helper `markPreChange(t, s, id)` that runs `UPDATE decisions SET triage_version = 1 WHERE id = ?`.
  - `TestRecordDecisionStoresTaskAndAsk`: record `{Task: "address_review", Ask: "reviewer says the backoff never resets", WouldDo: "none", PostKind: PostGrade}`. `DecisionView` returns the same `Task` and `Ask`.
  - `TestMigrationMarksExistingDecisionsAsPreChange`: insert a decision row with raw SQL that omits `triage_version`, and assert it reads back as `1`. Assert a `RecordDecision` row reads `TriageVersion`.
  - In `TestSampleQuietPicksOnlyUnpostedQuietDecisionsInWindow`, add `in("f", Decision{WouldDo: "none", Task: "fix_ci"})`. The expected set stays `{none, dropped}`.
  - In `TestSummary`, add a graded-👎 decision marked pre-change. `Graded`, `Up` and `AskFirstDown` are unchanged (2, 1, 1). `GradedInWindow` rises by one, since window activity is not filtered. Also add an ungraded pre-change `PostGrade` decision: `PendingGrades` stays 1.
  - In `TestGradedEventsAndReadOnly`, add a graded pre-change decision. `GradedEvents` does not return it.

- [ ] **Step 2: Run to verify they fail**

  Run: `go test ./internal/memory/`
  Expected: FAIL to compile (`unknown field Task`).

- [ ] **Step 3: Write the migration**

  ```sql
  -- Triage dispatches tasks (docs/superpowers/specs/2026-10-20-triage-dispatch-design.md).
  -- Existing rows keep triage_version 1: they were decided under the old
  -- vocabulary, and the Phase 1 exit counts only the current version.
  ALTER TABLE decisions ADD COLUMN task TEXT NOT NULL DEFAULT '';
  ALTER TABLE decisions ADD COLUMN ask TEXT NOT NULL DEFAULT '';
  ALTER TABLE decisions ADD COLUMN triage_version INTEGER NOT NULL DEFAULT 1;
  ```

- [ ] **Step 4: Implement**

  - Add `Task`, `Ask` to `Decision`.
  - Insert `task`, `ask` and `triage_version` in `RecordDecision`.
  - Select `d.task`, `d.ask` in `viewQuery`, after `d.rationale`, and scan them.
  - In `SampleQuiet`, add `AND task IN ('', 'none')` to the OK branch.
  - In `Summary`'s `latest` query and the `PendingGrades` query, add `d.triage_version = ?`.
  - In `GradedEvents`, add `AND d.triage_version = ?`.
  - Update the `Summary` field comments from "all time" to "current triage version, all time".

- [ ] **Step 5: Run and verify**

  Run: `go test -race ./internal/memory/`
  Expected: PASS, including `TestOpenUpgradesPhase0Database` and `TestMigrateIsIdempotent`.

- [ ] **Step 6: Commit**

  `git commit -m "Memory: store task, ask and triage version on decisions"`

---

### Task 3: Triage proposes tasks

**Files:**
- Create: `internal/triage/menu.go`
- Modify: `internal/triage/result.go`, `prompt.go`, `haiku.go`, `decide.go`, `replay.go`
- Test: `internal/triage/menu_test.go`, `result_test.go`, `prompt_test.go`, `decide_test.go`, `replay_test.go`

**Interfaces:**
- Consumes: `config.Config`, `config.Task` (Task 1).
- Produces:
  ```go
  // Menu is what triage may propose: tasks to hand to a worker, and
  // actions that need a human's approval.
  type Menu struct {
      Tasks   []config.Task // routes.yaml tasks.dispatch, in order
      Actions []string      // policy.yaml ask_first, in order
  }
  func NewMenu(cfg config.Config) Menu
  const None = "none" // replaces WouldDoNone; used for both task and would_do
  type Result struct {
      Category, Task, WouldDo, Target, Ask string
      Confidence float64
      Rationale  string
  } // JSON keys: category, task, would_do, target, ask, confidence, rationale
  func ParseResult(raw string, m Menu) (Result, config.Permission, error)
  type Verdict struct { …; Task string; Ask string }
  func Decide(ctx context.Context, kind, repo string, payload []byte, c Classifier, m Menu) (Verdict, error)
  func NewHaiku(m Menu, opts ...option.RequestOption) *Haiku
  func Replay(ctx context.Context, items []memory.Graded, c Classifier, m Menu, maxUSD float64) ReplayReport
  ```
  `ProposableActions` and `WouldDoNone` are removed.

- [ ] **Step 1: Write failing tests**

  Replace `testPolicy()` in `decide_test.go` with `testMenu()`:
  `Menu{Tasks: []config.Task{{Name: "address_review", Description: "Review feedback."}, {Name: "implement_issue", Description: "An actionable issue."}}, Actions: []string{"merge"}}`.
  Update every caller.

  - `TestNewMenuTakesDispatchTasksAndAskFirstActions` (`menu_test.go`): from a `config.Config` with `act_freely: [read, triage, comment_issue]`, `ask_first: [merge, deploy]`, and two dispatch tasks plus an internal `triage`. `Tasks` equals the two dispatch tasks in order, and `Actions` equals `[merge, deploy]`.
  - `TestParseResultAccepts`:
    - a task: `{"category":"review_feedback","task":"address_review","would_do":"none","target":"https://github.com/o/r/pull/5","ask":"reviewer says X","confidence":0.8,"rationale":"r"}` → perm `""`
    - `merge` with task `none` and a non-empty ask → perm `AskFirst`
    - both `none` with ask `""` → accepted
  - `TestParseResultRejects`: update the existing cases to the new shape, and add these:

    | case | input | rejected because |
    |---|---|---|
    | `comment_issue is not proposable` | `would_do: "comment_issue"` | not on the menu |
    | `internal task` | `task: "triage"` | not a dispatch task |
    | `unknown task` | `task: "refactor"` | not a dispatch task |
    | `task and action both set` | `task: "address_review"`, `would_do: "merge"` | propose one |
    | `task without ask` | `task: "address_review"`, `ask: ""` | ask is required |
    | `action without ask` | `would_do: "merge"`, `ask: ""` | ask is required |
    | `task without target` | `task: "address_review"`, `target: ""` | target is required |
    | `missing task field` | the old-shape JSON without `task`/`ask` | missing field |

    The last case needs no extra mechanism. A missing `task` decodes as `""`, which is neither `none` nor a dispatch task, so the task check rejects old raw output.
  - `TestSystemPromptListsMenu` (`prompt_test.go`): `systemPrompt(testMenu())` contains `- address_review: Review feedback.` and `- merge [ask_first]`. It does not contain `comment_issue`. It contains `never judge how hard`.
  - `TestResultSchemaEnums`: `resultSchema(testMenu())` has the `task` enum `[none address_review implement_issue]` and the `would_do` enum `[none merge]`. `required` lists all seven keys.
  - `TestDecideOK`: the classifier returns an `implement_issue` task. The verdict has `Task "implement_issue"`, `Ask` set, `WouldDo "none"`, `Permission ""`.
  - Replay tests: `commentRaw` becomes an `implement_issue` raw, and `gradedIssue` sets `Task: "implement_issue", WouldDo: "none"`. The divergence stub returns task `none`. Add `TestSameVerdictComparesTask`: same category and would_do but a different task → not the same.

- [ ] **Step 2: Run to verify they fail**

  Run: `go test ./internal/triage/`
  Expected: FAIL to compile.

- [ ] **Step 3: Implement `menu.go`, `result.go`, `decide.go`, `replay.go`, `haiku.go`**

  - `ParseResult` reports every problem through `errors.Join`, as today.
  - The permission is `config.AskFirst` when `would_do != none`, and `""` otherwise.
  - `sameVerdict` compares `Category`, `Task` and `WouldDo` for OK outcomes.
  - `NewHaiku` builds `systemPrompt(m)` and `resultSchema(m)`.

- [ ] **Step 4: Rewrite `systemPrompt(m Menu)` in `prompt.go`**

  Keep the opening paragraph, the actor-type paragraph, the categories list, and the closing untrusted-`<event>` sentences verbatim. Replace the field list and the action list with:
  ```
  For each event, decide:
  - category: what kind of activity this is.
  - task: work to hand to a worker, or "none". A worker reads the code and
    decides what to do; you only decide whether the event needs one.
  - would_do: an action that needs a human's approval, or "none". Set at most
    one of task and would_do.
  - target: the https://github.com/ URL the task or action applies to, or ""
    when both are "none".
  - ask: one line saying what the event wants, in your own words, or "" when
    both are "none".
  - confidence: 0 to 1.
  - rationale: one sentence saying why this task or action, or why nothing.
    Do not restate what the event is.
  ```
  Then `Tasks you may hand to a worker:` with `- name: description` lines, and `Actions that need a human's approval:` with `- action [ask_first]` lines. Replace `Choose "none" unless…` with:
  ```
  Choose a task when the event needs work someone must do in the code or the
  issue. You never judge how hard that work is. Choose "none" when the event
  needs nothing; most events need nothing.
  ```
  `resultSchema(m Menu)` adds `task` (enum `none` + task names) and `ask` (string), and lists all seven keys as required.

- [ ] **Step 5: Run and verify**

  Run: `go test -race ./internal/triage/`
  Expected: PASS.

- [ ] **Step 6: Commit**

  `git commit -m "Triage: propose a worker task or an ask_first action, with the ask"`

---

### Task 4: Wire the menu through the daemon and commands

**Files:**
- Modify: `internal/daemon/triage.go` (`Triage` struct, `Step`, `postKind`)
- Modify: `cmd/chuck/main.go:153-165`, `cmd/chuck/replay.go`
- Test: `internal/daemon/triage_test.go`, any `cmd/chuck` test that builds a `Triage` or runs replay

**Interfaces:**
- Consumes: `triage.Menu`, `triage.NewMenu`, `Verdict.Task/Ask` (Task 3), and `Decision.Task/Ask` (Task 2).
- Produces: `daemon.Triage` gains `Menu triage.Menu`. `Policy` stays for the Haiku budget.

- [ ] **Step 1: Write failing tests**

  - Add `Menu: testMenu()` (same shape as Task 3's) to `newTriageEnv`, and change `noneResult` to the seven-key shape.
  - Rename `TestTriagePostKindFollowsPermission` to `TestTriagePostKindFollowsProposal`. It has three outputs:

    | output | post kind | permission | task |
    |---|---|---|---|
    | `implement_issue` task | `PostGrade` | `""` | `implement_issue` |
    | `merge` | `PostApproval` | `ask_first` | `none` |
    | none | `""` | `""` | `none` |

    Assert `d.Task` and `d.Ask` are stored.
  - `TestTriageInvalidOutputIsFailedNotPosted`: the invalid output becomes the seven-key shape with `would_do: "comment_issue"` (no longer proposable), and the `RawOutput` assertion checks for `comment_issue` instead of `delete_repo`.

- [ ] **Step 2: Run to verify they fail**

  Run: `go test ./internal/daemon/ ./cmd/...`
  Expected: FAIL to compile.

- [ ] **Step 3: Implement**

  - `Step` calls `triage.Decide(…, t.Menu)` and copies `Task`/`Ask` into the `Decision`.
  - The `decision` log line gains `"task", d.Task`.
  - `postKind`: outcome not OK → `""`; `Permission == AskFirst` → approval; `Task` not in `{"", none}` → grade; otherwise `""`. Update its doc comment.
  - `main.go`: `menu := triage.NewMenu(*cfg)`, then `Classifier: triage.NewHaiku(menu, …)` and `Menu: menu`.
  - `replay.go`: build the menu the same way and pass it to `NewHaiku` and `Replay`. In the `--since` flag's help, note that only current-triage-version grades are replayed.

- [ ] **Step 4: Run and verify**

  Run: `make test`
  Expected: PASS.

- [ ] **Step 5: Commit**

  `git commit -m "Daemon: post a grade for dispatched tasks and wire the triage menu"`

---

### Task 5: Slack post leads with the task and the ask

**Files:**
- Modify: `internal/notify/posts.go` (`headline`, `DecisionBlocks`)
- Test: `internal/notify/posts_test.go`

**Interfaces:**
- Consumes: `memory.DecisionView` with `Task`/`Ask` (Task 2). `triage` is not imported. Compare against the literal `"none"`, as `headline` already does.

- [ ] **Step 1: Write failing tests**

  - `TestTaskPostLeadsWithTaskAndAsk`: a view with `Task: "address_review", WouldDo: "none", Target: "https://github.com/orionsbelt-dev/demos/pull/1", Ask: "reviewer says the backoff never resets", Rationale: "concrete bug with a suggested fix", PostKind: PostGrade`. The rendered JSON contains:
    - `chuck would hand this to a worker: `+"`address_review`"
    - `pull/1`
    - `*Ask:* reviewer says the backoff never resets`
    - `*Why:* concrete bug with a suggested fix`
    - both grade buttons

    It contains no `SHADOW`. `DecisionText(v)` contains `hand this to a worker`.
  - `TestApprovalPostOpen`: add `Ask: "approved and CI green"`, and assert `*Ask:*` and `*Why:* approved and green` render alongside the banner.
  - `TestPreChangeGradePostStillRenders` (Review Focus 1): a view with `Task: "", WouldDo: "comment_issue", Permission: "act_freely", PostKind: PostGrade`, `Grade: "up"`. It renders `chuck would `+"`comment_issue`" and `Graded 👍`.
  - Extend `TestPostBoundsLongFields` and `TestPostEscapesUntrustedText` to put the long or hostile string in `Ask` too (Review Focus 3).
  - `TestQuietPosts`: a quiet decision now has `Task: "none"`. It still renders "chuck would do nothing", with no `*Ask:*` line when `Ask` is empty.

- [ ] **Step 2: Run to verify they fail**

  Run: `go test ./internal/notify/`
  Expected: FAIL on the new assertions.

- [ ] **Step 3: Implement**

  `headline` checks, in order:
  1. the existing dropped and failed cases
  2. `Task` not in `{"", "none"}` → `chuck would hand this to a worker: `+"`%s`"+` %s` (task, `field(Target, maxRef)`)
  3. the existing "do nothing" case
  4. the existing action case

  In `DecisionBlocks`, replace the `"> "+rationale` section with one section built from:
  - `*Ask:* <field(Ask, maxRationale)>`, when `Ask != ""`
  - `*Why:* <field(Rationale, maxRationale)>`, when `Rationale != ""`

  Join the two lines with `\n`. Emit the section only for OK outcomes with at least one line.

- [ ] **Step 4: Run and verify**

  Run: `go test -race ./internal/notify/`
  Expected: PASS.

- [ ] **Step 5: Commit**

  `git commit -m "Slack: lead decision posts with the dispatched task and the ask"`

---

### Task 6: Docs, checks, and the PR

**Files:**
- Modify: `docs/shadow-checklist.md` (step 8)
- Modify: `docs/superpowers/specs/2026-10-13-phase-1-shadow-design.md` (triage-output and grading rows, exit criterion)
- Modify: anything `docs-currency` finds (e.g. `.claude/project-profile.md`, README)

- [ ] **Step 1: Update the checklist**

  Change step 8's expectation. An actionable issue should produce a grade post headed "chuck would hand this to a worker: `implement_issue`" with an Ask line. `none` on such an issue is now worth a 👎, not "correct behavior".

- [ ] **Step 2: Amend the Phase 1 design**

  - **Triage output row:** add "Amended 2026-10-20: triage proposes a worker task or an `ask_first` action, see `2026-10-20-triage-dispatch-design.md`".
  - **Exit criterion section:** add a line saying the window restarted when that change deployed, and that the counts cover `triage_version` 2 only.

- [ ] **Step 3: Run the docs-currency skill**

  Reconcile every doc it flags in this branch.

- [ ] **Step 4: Run the mechanical checks**

  Run: `test -z "$(gofmt -l .)" && make test && make validate && make lint && govulncheck ./...`
  Expected: all pass, with no output from gofmt.

- [ ] **Step 5: Commit, then run pr-preflight**

  `git commit -m "Docs: triage dispatch in the checklist and Phase 1 design"`, then invoke `pr-preflight`. It owns the push and the draft PR.

package loop

import (
	"time"

	"github.com/showmethebots/chuck/internal/config"
)

// State is where an item is in the loop. The values are the CHECK list on
// items.state in migration 0004.
type State string

const (
	StateIntake               State = "intake"
	StateAwaitingApproval     State = "awaiting-approval"
	StateNeedsHuman           State = "needs-human"
	StateReady                State = "ready"
	StateBuilding             State = "building"
	StateInPR                 State = "in-pr"
	StateReview               State = "review"
	StateSweep                State = "sweep"
	StateAwaitingMerge        State = "awaiting-merge"
	StateMerging              State = "merging"
	StateMerged               State = "merged"
	StateValidating           State = "validating"
	StateAwaitingProdApproval State = "awaiting-prod-approval"
	StatePromoting            State = "promoting"
	StateDone                 State = "done"
	StateClosed               State = "closed"
)

// States lists every state.
var States = []State{
	StateIntake, StateAwaitingApproval, StateNeedsHuman, StateReady, StateBuilding, StateInPR,
	StateReview, StateSweep, StateAwaitingMerge, StateMerging, StateMerged, StateValidating,
	StateAwaitingProdApproval, StatePromoting, StateDone, StateClosed,
}

// Active reports whether a state counts against wip_limit.
func (s State) Active() bool {
	switch s {
	case StateBuilding, StateReview, StateSweep, StateMerging, StateMerged, StateValidating, StatePromoting:
		return true
	}
	return false
}

// Terminal reports whether nothing moves an item out of s.
func (s State) Terminal() bool { return s == StateDone || s == StateClosed }

// stageFor is the stage a state runs a worker for, if any.
func stageFor(s State) (config.Stage, bool) {
	switch s {
	case StateIntake:
		return config.StageIntake, true
	case StateBuilding:
		return config.StageBuild, true
	case StateReview:
		return config.StageReview, true
	case StateSweep:
		return config.StageSweep, true
	case StateValidating:
		return config.StageValidate, true
	}
	return "", false
}

// Item kinds.
const (
	KindIssue = "issue"
	KindPR    = "pr"
)

// Item is the loop's view of an issue or PR.
type Item struct {
	ID            int64 // 0 for an item a Change creates
	Kind          string
	Number        int
	IssueNumber   int // for a PR: the issue it serves
	RatchetOf     int // for a ratchet issue: the issue it covers
	State         State
	StateSince    time.Time
	EscalatedFrom State
	PendingStage  config.Stage
}

// Key names an item whether or not it has been stored yet.
type Key struct {
	Kind   string
	Number int
}

func (it Item) Key() Key { return Key{it.Kind, it.Number} }

// Repo is what Transition needs to know about the repo and who to trust.
type Repo struct {
	FullName  string
	AppUserID int64   // chuck's App's bot user; it opens loop PRs
	Humans    []int64 // config/chuck.yaml humans
}

func (r Repo) isHuman(id int64) bool {
	for _, h := range r.Humans {
		if h == id && id > 0 {
			return true
		}
	}
	return false
}

// InputKind says what happened.
type InputKind string

const (
	InputWebhook   InputKind = "webhook"
	InputVerdict   InputKind = "verdict"
	InputDashboard InputKind = "dashboard"
	InputTick      InputKind = "tick"
)

// Input is one event for one item.
type Input struct {
	Kind    InputKind
	Webhook *WebhookInput
	Verdict *VerdictInput
	Action  *DashboardInput
	Facts   Facts
}

// WebhookInput is the part of a GitHub delivery the table reads. The
// caller maps the payload; Transition trusts only SenderID for identity.
type WebhookInput struct {
	Event    string // issues, pull_request, pull_request_review, workflow_run
	Action   string // labeled, synchronize, closed, submitted, completed
	Label    string
	SenderID int64
	Number   int    // issue or PR number
	SHA      string // PR head (pull_request), commit_id (review), head_sha (workflow_run)
	Merged   bool   // pull_request.closed
	// pull_request_review.
	ReviewState string // approved, changes_requested, commented
	// workflow_run.
	Workflow   string // the workflow file name or name, matched against loop.json deploy
	Branch     string
	Conclusion string // success, failure, cancelled, ...
	// RunEvent is what triggered the run (push, workflow_dispatch, or
	// pull_request for a fork's run), and SameRepo that the run's head
	// repository is this repository. A fork can name its branch main and
	// reuse a workflow name, so the branch alone proves nothing.
	RunEvent string
	SameRepo bool
	// RunCreated is when GitHub created the run. A dispatched run created
	// before the item reached its state was not dispatched for it.
	RunCreated time.Time
}

// VerdictInput is a worker's report, or chuck's own error for a run that
// produced none.
type VerdictInput struct {
	RunID   int64
	Stage   config.Stage
	Attempt int
	Verdict *Verdict // nil when Err is set
	Err     string   // timeout, machine gone, cancelled, invalid verdict
	// SameAttempt means the run failed without using its attempt (the
	// Machine never started, or the API key ran out of credit): relaunch
	// the stage as the same attempt, whatever the caps say.
	SameAttempt bool
}

// DashboardAction is a button on the dashboard.
type DashboardAction string

const (
	ActionStartIntake  DashboardAction = "start_intake"
	ActionApprove      DashboardAction = "approve"
	ActionRunStage     DashboardAction = "run_stage"
	ActionApproveMerge DashboardAction = "approve_merge"
	ActionPromote      DashboardAction = "promote"
	ActionRequeue      DashboardAction = "requeue"
	ActionCloseOut     DashboardAction = "close_out"
	ActionCancelRun    DashboardAction = "cancel_run"
)

// DashboardInput is a button press by a logged-in human, or by chuck itself
// for OpenRatchetIssue.
type DashboardInput struct {
	Action  DashboardAction
	Actor   string // GitHub login, or "chuck"
	ActorID int64  // 0 for chuck
	Number  int    // StartIntake: the issue
	// RatchetOf marks a StartIntake that the OpenRatchetIssue effect made.
	RatchetOf int
	Stage     config.Stage // RunStage
	SHA       string       // ApproveMerge: the head the human saw
}

// Facts are what the caller read from GitHub and the store for this input.
type Facts struct {
	// WIP. ActiveCount includes the item itself if it is active. Ready is
	// every ready item in the repo, oldest first.
	ActiveCount int
	Ready       []Item
	// Attempts used per stage for this item, counting DISTINCT attempt.
	Attempts map[config.Stage]int
	// The linked item: the issue a PR serves, or the PR an in-pr issue
	// follows.
	Linked *Item
	// Intake.
	IssueCriteria  []string // AC-n ids in the issue body
	Supersedes     []string // "#N AC-n" targets of Supersedes lines in the issue body
	SuiteTests     []string // "#N AC-n" provenance of every test in the suite
	SubIssuesKnown []int    // sub-issues already in the loop
	SubIssues      []int    // the issue's sub-issues as GitHub lists them
	// Build: the branch chuck named.
	Branch string
	PR     *PRFacts
	// The head the last review verdict reported.
	ReviewedSHA string
	// The run still open on this item, if any. For a verdict input it is
	// the run the verdict is for: the caller ends that run in the same
	// commit as the result, so a verdict whose run has already ended (a
	// duplicate post, a late one) finds no open run and does nothing.
	OpenRun *OpenRun
}

// PRFacts is the PR as GitHub has it now.
type PRFacts struct {
	Number        int
	Open          bool
	Merged        bool
	HeadSHA       string
	HeadRef       string
	MergeSHA      string
	AuthorID      int64
	ClosesIssue   bool // the body or a closing keyword references the issue
	ChecksFailed  bool
	ChangedFiles  []string
	PreviousNames []string // renamed files' old paths: a rename out of a risk path is a risk-path change
	// TooManyFiles means GitHub would not list every changed file (it
	// stops at 3000). The list is then partial, so the PR is treated as
	// touching a risk path: a human approves it.
	TooManyFiles bool
	// LoopStatus is the chuck/loop status on the head: "" when there is
	// none. Only chuck sets it, so at awaiting-merge "pending" means the
	// sweep held the PR for risk paths, and "failure" that it left
	// findings.
	LoopStatus string
}

// OpenRun is a run that has not ended.
type OpenRun struct {
	ID      int64
	Stage   config.Stage
	Attempt int
}

// Change moves one item. Item.ID zero creates it.
type Change struct {
	Item          Item // the item before the change; for a create, Kind, Number, IssueNumber, RatchetOf
	To            State
	EscalatedFrom State
	PendingStage  config.Stage
	Reason        string
}

// EffectKind is what the executor does.
type EffectKind string

const (
	EffectStateLabel       EffectKind = "state_label" // mirror the state as a loop:* label
	EffectMention          EffectKind = "mention"     // comment that mentions the humans
	EffectLaunch           EffectKind = "launch"
	EffectSetStatus        EffectKind = "set_status" // chuck/loop on SHA
	EffectEnableAutoMerge  EffectKind = "enable_auto_merge"
	EffectDispatchWorkflow EffectKind = "dispatch_workflow"
	EffectDestroyMachine   EffectKind = "destroy_machine"
	EffectOpenRatchetIssue EffectKind = "open_ratchet_issue"
)

// Status values for chuck/loop.
const (
	StatusPending = "pending"
	StatusSuccess = "success"
	StatusFailure = "failure"
)

// Effect is one thing the executor does after committing the changes.
type Effect struct {
	Kind     EffectKind   `json:"kind"`
	Item     Key          `json:"item"`
	State    State        `json:"state,omitempty"`
	Stage    config.Stage `json:"stage,omitempty"`
	SHA      string       `json:"sha,omitempty"`
	Status   string       `json:"status,omitempty"`
	Text     string       `json:"text,omitempty"`
	Workflow string       `json:"workflow,omitempty"`
	RunID    int64        `json:"run_id,omitempty"`
}

// Result is what Transition decided. With no Changes, nothing moves and
// Reason says why, for the webhook row.
type Result struct {
	Changes []Change
	Effects []Effect
	Reason  string
}

package triage

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/orionsbelt-dev/chuck/internal/config"
)

// Categories is the fixed set triage sorts events into.
var Categories = []string{
	"ci_failure", "review_feedback", "pr_lifecycle", "new_issue", "issue_discussion", "push", "noise",
}

// None means no task and no action. It is used for both task and would_do.
const None = "none"

// Result is the classifier's structured output. At most one of Task and
// WouldDo is a real value; the other, or both, are None.
type Result struct {
	Category   string  `json:"category"`
	Task       string  `json:"task"`
	WouldDo    string  `json:"would_do"`
	Target     string  `json:"target"`
	Ask        string  `json:"ask"`
	Confidence float64 `json:"confidence"`
	Rationale  string  `json:"rationale"`
}

// ParseResult decodes the classifier's output and checks it against the
// category list and the menu. Nothing downstream trusts output that fails
// here; it becomes a triage_failed decision. The permission is AskFirst
// when the result proposes an action, and "" otherwise.
func ParseResult(raw string, m Menu) (Result, config.Permission, error) {
	var r Result
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Result{}, "", fmt.Errorf("decode: %w", err)
	}
	var errs []error
	if !slices.Contains(Categories, r.Category) {
		errs = append(errs, fmt.Errorf("unknown category %q", r.Category))
	}
	// A missing task field decodes as "", which is neither None nor a
	// dispatch task, so output from before tasks existed fails here.
	if r.Task != None && !m.hasTask(r.Task) {
		errs = append(errs, fmt.Errorf("task %q is not a dispatch task", r.Task))
	}
	if r.WouldDo != None && !slices.Contains(m.Actions, r.WouldDo) {
		errs = append(errs, fmt.Errorf("would_do %q is not a proposable action", r.WouldDo))
	}
	if r.Task != None && r.WouldDo != None {
		errs = append(errs, fmt.Errorf("task %q and would_do %q are both set; propose one", r.Task, r.WouldDo))
	}
	if r.Task != None || r.WouldDo != None {
		if !strings.HasPrefix(r.Target, "https://github.com/") {
			errs = append(errs, fmt.Errorf("target %q is not a https://github.com/ URL", r.Target))
		}
		if strings.TrimSpace(r.Ask) == "" {
			errs = append(errs, errors.New("ask is required with a task or action"))
		}
	}
	if r.Confidence < 0 || r.Confidence > 1 {
		errs = append(errs, fmt.Errorf("confidence %v is outside 0..1", r.Confidence))
	}
	if err := errors.Join(errs...); err != nil {
		return Result{}, "", err
	}
	var perm config.Permission
	if r.WouldDo != None {
		perm = config.AskFirst
	}
	return r, perm, nil
}

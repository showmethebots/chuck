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

// WouldDoNone means chuck would take no action.
const WouldDoNone = "none"

// Result is the classifier's structured output.
type Result struct {
	Category   string  `json:"category"`
	WouldDo    string  `json:"would_do"`
	Target     string  `json:"target"`
	Confidence float64 `json:"confidence"`
	Rationale  string  `json:"rationale"`
}

// ProposableActions are the policy's declared actions in tier order, minus
// read and triage, which chuck always does and never proposes.
func ProposableActions(p config.Policy) []string {
	var out []string
	for _, tier := range [][]string{p.Permissions.ActFreely, p.Permissions.ActThenNotify, p.Permissions.AskFirst} {
		for _, a := range tier {
			if a != "read" && a != "triage" {
				out = append(out, a)
			}
		}
	}
	return out
}

// ParseResult decodes the classifier's output and checks it against the
// category list and the policy. Nothing downstream trusts output that fails
// here; it becomes a triage_failed decision.
func ParseResult(raw string, p config.Policy) (Result, config.Permission, error) {
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
	var perm config.Permission
	if r.WouldDo != WouldDoNone {
		if !slices.Contains(ProposableActions(p), r.WouldDo) {
			errs = append(errs, fmt.Errorf("would_do %q is not a proposable action", r.WouldDo))
		} else {
			perm = p.PermissionOf(r.WouldDo)
		}
		if !strings.HasPrefix(r.Target, "https://github.com/") {
			errs = append(errs, fmt.Errorf("target %q is not a https://github.com/ URL", r.Target))
		}
	}
	if r.Confidence < 0 || r.Confidence > 1 {
		errs = append(errs, fmt.Errorf("confidence %v is outside 0..1", r.Confidence))
	}
	if err := errors.Join(errs...); err != nil {
		return Result{}, "", err
	}
	return r, perm, nil
}

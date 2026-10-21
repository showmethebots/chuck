package triage

import (
	"slices"
	"testing"

	"github.com/orionsbelt-dev/chuck/internal/config"
)

func TestNewMenuTakesDispatchTasksAndAskFirstActions(t *testing.T) {
	tasks := []config.Task{
		{Name: "address_review", Description: "Review feedback."},
		{Name: "implement_issue", Description: "An actionable issue."},
	}
	cfg := config.Config{
		Routes: config.Routes{Tasks: config.Tasks{Dispatch: tasks, Internal: []string{"triage"}}},
		Policy: config.Policy{Permissions: config.Permissions{
			ActFreely: []string{"read", "triage", "comment_issue"},
			AskFirst:  []string{"merge", "deploy"},
		}},
	}
	m := NewMenu(cfg)
	if !slices.Equal(m.Tasks, tasks) {
		t.Errorf("Tasks = %v, want %v", m.Tasks, tasks)
	}
	if want := []string{"merge", "deploy"}; !slices.Equal(m.Actions, want) {
		t.Errorf("Actions = %v, want %v", m.Actions, want)
	}
}

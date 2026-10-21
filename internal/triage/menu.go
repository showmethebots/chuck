package triage

import "github.com/orionsbelt-dev/chuck/internal/config"

// Menu is what triage may propose: tasks to hand to a worker, and
// actions that need a human's approval.
type Menu struct {
	Tasks   []config.Task // routes.yaml tasks.dispatch, in order
	Actions []string      // policy.yaml ask_first, in order
}

// NewMenu builds the menu from cfg. act_freely and act_then_notify actions
// are not on it: workers produce those after reading the code.
func NewMenu(cfg config.Config) Menu {
	return Menu{
		Tasks:   cfg.Routes.Tasks.Dispatch,
		Actions: cfg.Policy.Permissions.AskFirst,
	}
}

func (m Menu) hasTask(name string) bool {
	for _, t := range m.Tasks {
		if t.Name == name {
			return true
		}
	}
	return false
}

func (m Menu) taskNames() []string {
	names := make([]string, len(m.Tasks))
	for i, t := range m.Tasks {
		names[i] = t.Name
	}
	return names
}

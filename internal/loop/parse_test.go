package loop

import (
	"slices"
	"testing"
)

func TestIssueCriteria(t *testing.T) {
	body := "Intro mentions AC-9 in passing.\n\n## Acceptance criteria\n- [ ] AC-1: home loads\n  - [x] AC-2: empty state\n* [ ] AC-1: again\n- AC-3: not a checkbox\n"
	if got := IssueCriteria(body); !slices.Equal(got, []string{"AC-1", "AC-2"}) {
		t.Errorf("IssueCriteria = %v", got)
	}
}

func TestSupersedesTargets(t *testing.T) {
	body := "- [ ] AC-1: x\n  Supersedes: #4 AC-2\n  Supersedes:  #4   AC-2\nSupersedes: #7 AC-1 and more\n"
	if got := SupersedesTargets(body); !slices.Equal(got, []string{"#4 AC-2"}) {
		t.Errorf("SupersedesTargets = %v", got)
	}
}

func TestClosesIssue(t *testing.T) {
	for body, want := range map[string]bool{
		"Closes #12":        true,
		"fixes: #12.":       true,
		"Resolved #12\n":    true,
		"Closes #123":       false,
		"See #12":           false,
		"enclosES #12":      false,
		"closes owner/r#12": false,
	} {
		if got := ClosesIssue(body, 12); got != want {
			t.Errorf("ClosesIssue(%q) = %v; want %v", body, got, want)
		}
	}
}

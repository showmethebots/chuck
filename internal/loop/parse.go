package loop

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	criterionRe  = regexp.MustCompile(`(?m)^[ \t]*[-*][ \t]*\[[ xX]\][ \t]*(AC-[0-9]+)[ \t]*:`)
	supersedesRe = regexp.MustCompile(`(?m)^[ \t]*Supersedes:[ \t]*#([0-9]+)[ \t]+(AC-[0-9]+)[ \t]*$`)
)

// IssueCriteria are the AC-n ids of the checklist lines in an issue body
// ("- [ ] AC-1: ..."), in order, each once. The body is anyone's text, but
// an id it adds only makes validation stricter: every id must pass.
func IssueCriteria(body string) []string {
	var ids []string
	for _, m := range criterionRe.FindAllStringSubmatch(body, -1) {
		ids = appendOnce(ids, m[1])
	}
	return ids
}

// SupersedesTargets are the "#N AC-n" tests an issue body's Supersedes
// lines name, each once.
func SupersedesTargets(body string) []string {
	var out []string
	for _, m := range supersedesRe.FindAllStringSubmatch(body, -1) {
		out = appendOnce(out, "#"+m[1]+" "+m[2])
	}
	return out
}

// ClosesIssue reports whether a PR body uses a closing keyword on the
// issue ("Closes #12"), which the build prompt requires.
func ClosesIssue(body string, issue int) bool {
	re := regexp.MustCompile(fmt.Sprintf(`(?i)\b(close[sd]?|fix(e[sd])?|resolve[sd]?)[ \t]*:?[ \t]+#%d\b`, issue))
	return re.MatchString(body)
}

func appendOnce(xs []string, x string) []string {
	for _, y := range xs {
		if strings.EqualFold(y, x) {
			return xs
		}
	}
	return append(xs, x)
}

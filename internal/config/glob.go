package config

import (
	"fmt"
	"regexp"
	"strings"
)

// compileGlob turns a risk-path glob into an anchored regexp. Patterns are
// matched against the whole repo-relative path from the root, so
// "package.json" is the root file only and "**/package.json" is any.
//
//	**/   zero or more directories
//	**    anything, slashes included (only as a whole segment)
//	*     anything within one segment
//	?     one character within a segment
//
// Anything else is literal. Character classes and braces are rejected
// rather than half-supported, since a risk path that silently matches less
// than its author meant is the failure that matters here.
func compileGlob(g string) (*regexp.Regexp, error) {
	if g == "" {
		return nil, fmt.Errorf("empty pattern")
	}
	if strings.HasPrefix(g, "/") {
		return nil, fmt.Errorf("%q: patterns are repo-relative; drop the leading /", g)
	}
	if strings.ContainsAny(g, "[]{}\\") {
		return nil, fmt.Errorf("%q: [], {} and \\ are not supported", g)
	}
	var b strings.Builder
	b.WriteString("^")
	segs := strings.Split(g, "/")
	for i, seg := range segs {
		last := i == len(segs)-1
		switch {
		case seg == "**" && last:
			b.WriteString(".*")
		case seg == "**":
			b.WriteString("(?:[^/]+/)*")
		case strings.Contains(seg, "**"):
			return nil, fmt.Errorf("%q: ** must be a whole path segment", g)
		case seg == "":
			return nil, fmt.Errorf("%q: empty path segment", g)
		case seg == "." || seg == "..":
			// GitHub reports changed files as clean paths, so a pattern
			// with . or .. could never match one.
			return nil, fmt.Errorf("%q: . and .. segments never match a changed file", g)
		default:
			for _, r := range seg {
				switch r {
				case '*':
					b.WriteString("[^/]*")
				case '?':
					b.WriteString("[^/]")
				default:
					b.WriteString(regexp.QuoteMeta(string(r)))
				}
			}
			if !last {
				b.WriteString("/")
			}
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

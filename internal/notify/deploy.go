package notify

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/orionsbelt-dev/chuck/internal/memory"
)

const repoURL = "https://github.com/orionsbelt-dev/chuck"

// deployAttempts bounds how often AnnounceDeploy tries Slack before giving up.
const deployAttempts = 5

// commitSHA matches a version CI built from a commit (the full github.sha).
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func shortVersion(v string) string {
	if commitSHA.MatchString(v) {
		return v[:7]
	}
	return v
}

// DeployText announces that version is now running in place of prev. When
// both are commits it links to the changes between them.
func DeployText(version, prev string) string {
	text := fmt.Sprintf("chuck `%s` deployed (was `%s`).", shortVersion(version), shortVersion(prev))
	if commitSHA.MatchString(version) && commitSHA.MatchString(prev) {
		text += fmt.Sprintf(" Changes: <%s/compare/%s...%s|%s...%s>",
			repoURL, prev, version, shortVersion(prev), shortVersion(version))
	}
	return text
}

// AnnounceDeploy posts DeployText to the main channel when this boot runs a
// different build from the boot before it (prev). Restarts of the same
// build, the first boot ever, and local "dev" builds stay quiet. A failed
// post is retried with backoff starting at retry, up to deployAttempts
// times; after that the notice is dropped and logged.
func (h *Interactions) AnnounceDeploy(ctx context.Context, prev string, retry time.Duration) {
	if prev == "" || prev == h.Version || h.Version == "dev" {
		return
	}
	text := DeployText(h.Version, prev)
	for i := 1; ; i++ {
		ts, err := h.Poster.Post(ctx, h.MainChannel, text, nil)
		if err == nil {
			h.record(ctx, memory.Message{Channel: h.MainChannel, TS: ts, Direction: "out", Kind: "post",
				Text: text, Allowed: true, CreatedAt: h.now()})
			return
		}
		if i == deployAttempts {
			h.Log.Error("slack deploy notice; giving up", "attempts", i, "err", err.Error())
			return
		}
		h.Log.Warn("slack deploy notice", "attempt", i, "err", err.Error())
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
		retry *= 2
	}
}

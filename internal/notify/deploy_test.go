package notify

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

const (
	shaOld = "0578a6d1b2c3d4e5f60718293a4b5c6d7e8f9012"
	shaNew = "e960f2b1b2c3d4e5f60718293a4b5c6d7e8f9012"
)

func TestDeployText(t *testing.T) {
	got := DeployText(shaNew, shaOld)
	for _, want := range []string{
		"`e960f2b`", "`0578a6d`",
		"<https://github.com/orionsbelt-dev/chuck/compare/" + shaOld + "..." + shaNew + "|",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("DeployText = %q, missing %q", got, want)
		}
	}

	// Not a pair of commits: no compare link, versions shown as they are.
	got = DeployText("v2", "v1")
	if strings.Contains(got, "compare") || !strings.Contains(got, "`v2`") || !strings.Contains(got, "`v1`") {
		t.Errorf("DeployText(v2, v1) = %q", got)
	}
}

func TestAnnounceDeploy(t *testing.T) {
	cases := []struct {
		name, version, prev string
		wantPost            bool
	}{
		{"new build", shaNew, shaOld, true},
		{"rollback", shaOld, shaNew, true},
		{"restart of the same build", shaNew, shaNew, false},
		{"first boot ever", shaNew, "", false},
		{"local build", "dev", shaOld, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, s, p := newInteractions(t)
			h.Version = c.version
			h.AnnounceDeploy(context.Background(), c.prev, 0)
			if got := len(p.posts) == 1; got != c.wantPost {
				t.Fatalf("posted = %v, want %v (posts: %+v)", got, c.wantPost, p.posts)
			}
			if !c.wantPost {
				return
			}
			if p.posts[0].channel != "CMAIN" {
				t.Errorf("posted to %q, want CMAIN", p.posts[0].channel)
			}
			msgs, err := s.Messages(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 || msgs[0].Text != DeployText(c.version, c.prev) {
				t.Errorf("recorded messages = %+v", msgs)
			}
		})
	}
}

// flakyPoster fails the first fails posts, then succeeds.
type flakyPoster struct {
	fakePoster
	fails, calls int
}

func (f *flakyPoster) Post(ctx context.Context, channel, text string, blocks []slack.Block) (string, error) {
	f.calls++
	if f.calls <= f.fails {
		return "", errors.New("slack down")
	}
	return f.fakePoster.Post(ctx, channel, text, blocks)
}

// A Slack blip at boot should not lose the notice, but a long outage must
// not keep the job retrying forever.
func TestAnnounceDeployRetries(t *testing.T) {
	h, _, _ := newInteractions(t)
	h.Version = shaNew
	p := &flakyPoster{fails: 2}
	h.Poster = p
	h.AnnounceDeploy(context.Background(), shaOld, 0)
	if len(p.posts) != 1 {
		t.Fatalf("after 2 failures: %d posts, want 1", len(p.posts))
	}

	p = &flakyPoster{fails: 1000}
	h.Poster = p
	h.AnnounceDeploy(context.Background(), shaOld, 0)
	if p.calls != deployAttempts {
		t.Fatalf("Slack always down: %d attempts, want %d", p.calls, deployAttempts)
	}
}

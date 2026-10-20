package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/orionsbelt-dev/chuck/internal/memory"
)

type recordingPoster struct{ posts []string }

func (r *recordingPoster) Post(_ context.Context, channel, text string, _ []slack.Block) (string, error) {
	r.posts = append(r.posts, channel+"|"+text)
	return "1.1", nil
}
func (r *recordingPoster) Update(context.Context, string, string, string, []slack.Block) error {
	return nil
}
func (r *recordingPoster) Ephemeral(context.Context, string, string, string) error { return nil }

func TestDailyJobsQueueSampleAndPostSummary(t *testing.T) {
	ctx := context.Background()
	s, err := memory.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	at := time.Date(2026, 10, 13, 13, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		ev := addEvent(t, s, "issues.opened", `{}`, at.Add(-time.Hour))
		if _, err := s.RecordDecision(ctx, memory.Decision{EventID: ev, Stage: memory.StageHaiku, Outcome: memory.OutcomeOK,
			Category: "noise", WouldDo: "none", CreatedAt: at.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	p := &recordingPoster{}
	j := &DailyJobs{Store: s, Poster: p, MainChannel: "CMAIN", Sample: 2, HaikuBudget: 5,
		Disk: func() (uint64, uint64, error) { return 1 << 20, 1 << 30, nil }, Log: discardLog()}
	if err := j.Run(ctx, at); err != nil {
		t.Fatal(err)
	}
	if pending, _ := s.PendingPosts(ctx, 10); len(pending) != 2 {
		t.Fatalf("queued %d grade posts, want 2", len(pending))
	}
	if len(p.posts) != 1 || !strings.HasPrefix(p.posts[0], "CMAIN|") || !strings.Contains(p.posts[0], "chuck daily summary") {
		t.Fatalf("posts = %v", p.posts)
	}
	if msgs, _ := s.Messages(ctx); len(msgs) != 1 || msgs[0].Direction != "out" || msgs[0].TS != "1.1" ||
		!strings.Contains(msgs[0].Text, "chuck daily summary") {
		t.Fatalf("summary not recorded: %+v", msgs)
	}
	if n, _ := s.QueueDepth(ctx); n != 0 {
		t.Fatalf("cron tick entered the triage queue")
	}
}

func TestDailyJobsWithoutSlack(t *testing.T) {
	ctx := context.Background()
	s, err := memory.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	j := &DailyJobs{Store: s, Sample: 5, HaikuBudget: 5, Log: discardLog()}
	if err := j.Run(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestDiskUsage(t *testing.T) {
	used, total, err := DiskUsage(t.TempDir())
	if err != nil || total == 0 || used > total {
		t.Fatalf("DiskUsage = %d, %d, %v", used, total, err)
	}
}

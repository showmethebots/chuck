package daemon

import (
	"context"
	"log/slog"
	"syscall"
	"time"

	"github.com/orionsbelt-dev/chuck/internal/memory"
	"github.com/orionsbelt-dev/chuck/internal/notify"
)

// JobStore is the part of memory.Store the daily jobs use.
type JobStore interface {
	InsertEvent(ctx context.Context, e memory.Event) (int64, bool, error)
	SampleQuiet(ctx context.Context, since, until time.Time, n int) ([]int64, error)
	QueueGradePosts(ctx context.Context, ids []int64) error
	Summary(ctx context.Context, since, until time.Time) (memory.Summary, error)
	Control(ctx context.Context) (memory.Control, error)
	RecordMessage(ctx context.Context, m memory.Message) error
}

// DailyJobs runs once a day, paused or not, so a pause never silently stops
// reporting: it records a cron tick, queues a random sample of quiet
// decisions for grading, and posts the summary.
type DailyJobs struct {
	Store       JobStore
	Poster      notify.Poster // nil when Slack is off; the summary is only logged
	MainChannel string
	Sample      int
	HaikuBudget float64
	Disk        func() (used, total uint64, err error)
	Log         *slog.Logger
}

func (j *DailyJobs) Run(ctx context.Context, at time.Time) error {
	if _, _, err := j.Store.InsertEvent(ctx, memory.Event{
		Source: memory.SourceCron, Kind: "cron.daily", Payload: []byte(`{}`), ReceivedAt: at,
	}); err != nil {
		return err
	}
	since := at.Add(-24 * time.Hour)
	ids, err := j.Store.SampleQuiet(ctx, since, at, j.Sample)
	if err != nil {
		return err
	}
	// The sample is queued before the summary posts, so if the post fails
	// the sample stands and that day's summary is lost; only the error is
	// logged. That is accepted for Phase 1.
	if err := j.Store.QueueGradePosts(ctx, ids); err != nil {
		return err
	}
	sum, err := j.Store.Summary(ctx, since, at)
	if err != nil {
		return err
	}
	ctl, err := j.Store.Control(ctx)
	if err != nil {
		return err
	}
	var used, total uint64
	if j.Disk != nil {
		if used, total, err = j.Disk(); err != nil {
			j.Log.Warn("disk usage", "err", err.Error())
		}
	}
	text := notify.SummaryText(sum, ctl, j.HaikuBudget, used, total)
	j.Log.Info("daily summary", "sampled", len(ids), "text", text)
	if j.Poster == nil {
		return nil
	}
	ts, err := j.Poster.Post(ctx, j.MainChannel, text, nil)
	if err != nil {
		return err
	}
	return j.Store.RecordMessage(ctx, memory.Message{Channel: j.MainChannel, TS: ts, Direction: "out",
		Kind: "post", Text: text, Allowed: true, CreatedAt: time.Now()})
}

// DiskUsage reports used and total bytes on the filesystem holding path.
func DiskUsage(path string) (used, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return (st.Blocks - st.Bfree) * bs, st.Blocks * bs, nil
}

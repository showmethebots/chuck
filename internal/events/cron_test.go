package events

import (
	"context"
	"testing"
	"time"
)

func TestParseDaily(t *testing.T) {
	for _, ok := range []string{"13:00", "00:00", "23:59", "9:05"} {
		if _, err := ParseDaily(ok); err != nil {
			t.Errorf("ParseDaily(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "1300", "25:00", "13:60", "1pm"} {
		if _, err := ParseDaily(bad); err == nil {
			t.Errorf("ParseDaily(%q) accepted", bad)
		}
	}
}

func TestDailyNext(t *testing.T) {
	d, _ := ParseDaily("13:00")
	day := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	cases := []struct{ now, want time.Time }{
		{day.Add(9 * time.Hour), day.Add(13 * time.Hour)},
		{day.Add(13 * time.Hour), day.Add(37 * time.Hour)}, // exactly at: next day
		{day.Add(14 * time.Hour), day.Add(37 * time.Hour)},
		{day.Add(9 * time.Hour).In(time.FixedZone("CDT", -5*3600)), day.Add(13 * time.Hour)},
	}
	for _, c := range cases {
		if got := d.Next(c.now); !got.Equal(c.want) {
			t.Errorf("Next(%v) = %v, want %v", c.now, got, c.want)
		}
	}
}

func TestRunDailyFiresAtTheScheduledTime(t *testing.T) {
	d, _ := ParseDaily("13:00")
	now := func() time.Time { return time.Date(2026, 10, 13, 12, 59, 59, 990_000_000, time.UTC) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fired := make(chan time.Time, 1)
	go RunDaily(ctx, d, now, func(_ context.Context, at time.Time) {
		select {
		case fired <- at:
		default:
		}
		cancel()
	})
	select {
	case at := <-fired:
		if want := time.Date(2026, 10, 13, 13, 0, 0, 0, time.UTC); !at.Equal(want) {
			t.Fatalf("fired at %v, want %v", at, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not fire")
	}
}

// The timer can fire while the wall clock still reads before the occurrence;
// a now that never advances is the extreme case. The occurrence must still
// run only once.
func TestRunDailyFiresEachOccurrenceOnce(t *testing.T) {
	d, _ := ParseDaily("13:00")
	now := func() time.Time { return time.Date(2026, 10, 13, 12, 59, 59, 990_000_000, time.UTC) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fired := make(chan time.Time, 10)
	go RunDaily(ctx, d, now, func(_ context.Context, at time.Time) {
		select {
		case fired <- at:
		default:
		}
	})
	time.Sleep(200 * time.Millisecond)
	cancel()
	if n := len(fired); n != 1 {
		t.Fatalf("fired %d times, want 1", n)
	}
}

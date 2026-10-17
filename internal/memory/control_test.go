package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestControlPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	s := openTemp(t, path)
	c, err := s.Control(ctx)
	if err != nil || c.Paused {
		t.Fatalf("fresh control = %+v, %v; want running", c, err)
	}
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	if err := s.SetControl(ctx, Control{Paused: true, Reason: "kill", By: "U0123456789", ChangedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openTemp(t, path)
	defer func() { _ = s.Close() }()
	c, err = s.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Paused || c.Reason != "kill" || c.By != "U0123456789" || !c.ChangedAt.Equal(at) {
		t.Fatalf("control after reopen = %+v", c)
	}

	if err := s.SetControl(ctx, Control{Paused: false, By: "U0123456789", ChangedAt: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Control(ctx); c.Paused || c.Reason != "" {
		t.Fatalf("after resume = %+v", c)
	}
}

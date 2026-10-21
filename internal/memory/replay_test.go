package memory

import (
	"context"
	"path/filepath"
	"testing"
)

func TestGradedEventsAndReadOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s := openTemp(t, path)
	graded := recordDecision(t, s, Decision{EventID: insertEvent(t, s, Event{DeliveryID: "a", Kind: "issues.opened", Payload: []byte(`{"x":1}`)}), RawOutput: "raw"})
	recordDecision(t, s, Decision{EventID: insertEvent(t, s, Event{DeliveryID: "b"})}) // ungraded
	preChangeGraded := recordDecision(t, s, Decision{EventID: insertEvent(t, s, Event{DeliveryID: "c"})})
	markPreChange(t, s, preChangeGraded)
	if err := s.RecordGrade(ctx, graded, "down", "UOWNER0001", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordGrade(ctx, preChangeGraded, "down", "UOWNER0001", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	items, err := ro.GradedEvents(ctx, t0.Add(-1))
	if err != nil || len(items) != 1 {
		t.Fatalf("GradedEvents = %+v, %v", items, err)
	}
	it := items[0]
	if it.Grade != "down" || it.Decision.ID != graded || string(it.Event.Payload) != `{"x":1}` || it.Decision.RawOutput != "raw" {
		t.Fatalf("item = %+v", it)
	}
	if err := ro.RecordGrade(ctx, graded, "up", "UOWNER0001", t0); err == nil {
		t.Fatal("read-only store accepted a write")
	}
}

func TestOpenReadOnlyNeedsExistingFile(t *testing.T) {
	if _, err := OpenReadOnly(context.Background(), filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatal("opened a missing file")
	}
}

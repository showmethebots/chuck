package notify

import (
	"context"
	"errors"
	"testing"

	"github.com/orionsbelt-dev/chuck/internal/memory"
)

func newOutbox(s *memory.Store, p *fakePoster) *Outbox {
	return &Outbox{Store: s, Poster: p, MainChannel: "CMAIN", ShadowChannel: "CSHADOW", Log: discardLog()}
}

func TestOutboxPostsByKindOnce(t *testing.T) {
	ctx := context.Background()
	s, p := newStore(t), &fakePoster{}
	grade := seedDecision(t, s, memory.Decision{WouldDo: "comment_issue", Target: "https://github.com/o/r/issues/1", PostKind: memory.PostGrade})
	approval := seedDecision(t, s, memory.Decision{WouldDo: "merge", Target: "https://github.com/o/r/pull/1", PostKind: memory.PostApproval})
	seedDecision(t, s, memory.Decision{WouldDo: "none"}) // no post

	ob := newOutbox(s, p)
	if err := ob.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ob.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(p.posts) != 2 {
		t.Fatalf("posted %d, want 2", len(p.posts))
	}
	if p.posts[0].channel != "CSHADOW" || p.posts[1].channel != "CMAIN" {
		t.Errorf("channels = %q, %q", p.posts[0].channel, p.posts[1].channel)
	}
	for id, want := range map[int64]string{grade: p.posts[0].ts, approval: p.posts[1].ts} {
		v, err := s.DecisionView(ctx, id)
		if err != nil || v.SlackTS != want {
			t.Errorf("decision %d slack_ts = %q, %v; want %q", id, v.SlackTS, err, want)
		}
	}
	msgs, _ := s.Messages(ctx)
	if len(msgs) != 2 || msgs[0].Direction != "out" || msgs[0].Kind != "post" || !msgs[0].Allowed {
		t.Errorf("messages = %+v", msgs)
	}
}

func TestOutboxKeepsPostsWhileSlackIsDown(t *testing.T) {
	ctx := context.Background()
	s, p := newStore(t), &fakePoster{failPost: errors.New("slack down")}
	seedDecision(t, s, memory.Decision{WouldDo: "comment_issue", Target: "https://github.com/o/r/issues/1", PostKind: memory.PostGrade})
	ob := newOutbox(s, p)
	if err := ob.Flush(ctx); err == nil {
		t.Fatal("Flush hid the Slack error")
	}
	if pending, _ := s.PendingPosts(ctx, 10); len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	p.failPost = nil
	if err := ob.Flush(ctx); err != nil || len(p.posts) != 1 {
		t.Fatalf("after recovery: err %v, posts %d", err, len(p.posts))
	}
}

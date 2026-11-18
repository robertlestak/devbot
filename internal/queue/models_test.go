package queue

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// A model's limit only moves later, and lapses on its own.
func TestLimitModel(t *testing.T) {
	m := miniredis.RunT(t)
	s := New(m.Addr(), 0)
	ctx := context.Background()
	key := ModelKey("claude", "", "claude-opus-5-5")
	if at, _, err := s.ModelLimit(ctx, key); err != nil || !at.IsZero() {
		t.Fatalf("%v %v", at, err)
	}
	far := time.Unix(time.Now().Add(2*time.Hour).Unix(), 0)
	s.LimitModel(ctx, key, far, "rate_limited")
	s.LimitModel(ctx, key, time.Now().Add(time.Minute), "overloaded")
	at, why, err := s.ModelLimit(ctx, key)
	if err != nil || !at.Equal(far) || why != "rate_limited" {
		t.Fatalf("%v %q %v", at, why, err)
	}
	if at, _, _ := s.ModelLimit(ctx, ModelKey("claude", "us-east-1", "claude-opus-5-5")); !at.IsZero() {
		t.Fatal("a limit on one provider is another's too")
	}
	m.FastForward(3 * time.Hour)
	if at, _, _ := s.ModelLimit(ctx, key); !at.IsZero() {
		t.Fatalf("still limited until %v", at)
	}
}

func TestModelRan(t *testing.T) {
	s := New(miniredis.RunT(t).Addr(), 0)
	ctx := context.Background()
	if _, ok, err := s.ModelRan(ctx, "gitea:acme/app#1", "implement"); ok || err != nil {
		t.Fatal(ok, err)
	}
	r := ModelRun{Model: "b", Fallback: true, From: "a", Reason: "rate_limited", At: time.Unix(1, 0).UTC()}
	if err := s.SetModelRan(ctx, "gitea:acme/app#1", "implement", r); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.ModelRan(ctx, "gitea:acme/app#1", "implement")
	if !ok || err != nil || got != r {
		t.Fatalf("%+v %v %v", got, ok, err)
	}
	if _, ok, _ := s.ModelRan(ctx, "gitea:acme/app#1", "review"); ok {
		t.Fatal("one role's run is another's")
	}
}

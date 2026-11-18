package harness

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// cancelAfterRun cancels ref, as a person would, once the model's run is
// over and before the heartbeat could see it.
func (h *hEnv) cancelAfterRun(ref forge.ItemRef) {
	h.r.Agent = &spy{Runner: h.r.Agent, after: func(string) {
		if _, err := h.e.Cancel(context.Background(), ref.String(), core.Caller{Principal: "test"}); err != nil {
			h.t.Error(err)
		}
	}}
}

// cancelled checks that the job left a cancelled task as the cancel left it.
func (h *hEnv) cancelled(ref forge.ItemRef, kind string) {
	h.t.Helper()
	ctx := context.Background()
	if s := h.state(ref); s != "" {
		h.t.Fatalf("a cancel was undone: state %q\n%s", s, h.comments(ref))
	}
	cs := h.comments(ref)
	if n := h.failedAttempts(ref); n != 0 || strings.Contains(cs, "failed:") {
		h.t.Fatalf("a cancelled job recorded a failure:\n%s", cs)
	}
	if kind == "implement" && strings.Contains(cs, "Opened PR") {
		h.t.Fatalf("a cancelled job announced its PR:\n%s", cs)
	}
	if ds, _ := h.e.Q.Delayed(ctx); len(ds) != 0 {
		h.t.Fatalf("a cancelled job was parked: %+v", ds)
	}
	if d, _ := h.e.Q.Depths(ctx); d[queue.KindImplement]+d[queue.KindReview] != 0 {
		h.t.Fatalf("a cancelled job was re-queued: %v", d)
	}
	if stats, _ := h.e.Q.Stats(ctx); stats["jobs|"+kind+"|"+repo.String()+"|cancelled"] != 1 {
		h.t.Fatalf("the job should count as cancelled: %v", stats)
	}
}

// A cancel the heartbeat hasn't seen yet still holds: an implement job that
// then succeeds, fails or hits a usage limit leaves the task cancelled.
func TestCancelDuringImplementHolds(t *testing.T) {
	for _, mode := range []string{"done", "nothing", "ratelimit"} {
		t.Run(mode, func(t *testing.T) {
			h := newHEnv(t, yamlChecks)
			h.mode(mode)
			ref := h.file("Say hello")
			h.cancelAfterRun(ref)
			if err := h.implement(context.Background()); err != nil {
				t.Fatal(err)
			}
			h.cancelled(ref, "implement")
		})
	}
}

// The same for a review that approves, fails or hits a usage limit.
func TestCancelDuringReviewHolds(t *testing.T) {
	for _, mode := range []string{"approve", "nothing", "ratelimit"} {
		t.Run(mode, func(t *testing.T) {
			h := newHEnv(t, yamlChecks)
			ref, pr, _ := h.inReview("done")
			h.mode(mode)
			h.cancelAfterRun(ref)
			if err := h.review(context.Background()); err != nil {
				t.Fatal(err)
			}
			if rv := h.reviews(pr); len(rv) != 0 {
				t.Fatalf("a verdict on a cancelled task: %+v", rv)
			}
			if mode == "approve" {
				// Dropped as the task changed under the review.
				if s := h.state(ref); s != "" {
					t.Fatalf("a cancel was undone: state %q", s)
				}
				return
			}
			h.cancelled(ref, "review")
		})
	}
}

// A job whose lease another worker took (Redis wiped, and the task's job
// queued again) leaves the task to that worker: no comment, no transition,
// no attempt spent.
func TestLostLeaseExitsQuietly(t *testing.T) {
	for _, mode := range []string{"done", "nothing"} {
		t.Run(mode, func(t *testing.T) {
			h := newHEnv(t, yamlChecks)
			ctx := context.Background()
			h.mode(mode)
			ref := h.file("Say hello")
			var before string
			h.r.Agent = &spy{Runner: h.r.Agent, after: func(string) {
				h.e.Q.Release(ctx, queue.KindImplement, ref.String(), h.r.Owner)
				if ok, err := h.e.Q.Acquire(ctx, queue.KindImplement, ref.String(), "pod-2", time.Minute); err != nil || !ok {
					t.Errorf("pod-2 should take the lease: %v %v", ok, err)
				}
				before = h.comments(ref)
			}}
			if err := h.implement(ctx); err != nil {
				t.Fatal(err)
			}
			if s := h.state(ref); s != core.LWorking {
				t.Fatalf("the task is pod-2's: want working, got %q\n%s", s, h.comments(ref))
			}
			if after := h.comments(ref); after != before {
				t.Fatalf("the job commented after losing its lease:\n%s", strings.TrimPrefix(after, before))
			}
			if n := h.failedAttempts(ref); n != 0 {
				t.Fatalf("losing the lease spent an attempt: %d", n)
			}
			if prs, _ := h.f.ListOpenPRs(ctx, repo); mode == "nothing" && len(prs) != 0 {
				t.Fatalf("a PR from a failed run: %+v", prs)
			}
			if l, _ := h.e.Q.Leases(ctx); l[queue.KindImplement] != 1 {
				t.Fatalf("pod-2's lease should stand: %v", l)
			}
			if stats, _ := h.e.Q.Stats(ctx); stats["jobs|implement|"+repo.String()+"|lease_lost"] != 1 {
				t.Fatalf("the lost lease should be counted: %v", stats)
			}
		})
	}
}

// heartbeatFor runs heartbeat on a lease of its own and returns the cause it
// cancelled with, or nil if it hadn't by wait.
func heartbeatFor(t *testing.T, mr *miniredis.Miniredis, ttl time.Duration, during func(q *queue.Store), wait time.Duration) error {
	t.Helper()
	q := queue.New(mr.Addr(), 0)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	if ok, err := q.Acquire(ctx, queue.KindImplement, "r#1", "pod-1", ttl); err != nil || !ok {
		t.Fatal(ok, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		heartbeat(ctx, cancel, lease{q: q, kind: queue.KindImplement, ref: "r#1", owner: "pod-1", ttl: ttl}, 20*time.Millisecond, nil, forge.ItemRef{}, nil)
	}()
	during(q)
	select {
	case <-done:
	case <-time.After(wait):
		cancel(nil)
		<-done
		return nil
	}
	return context.Cause(ctx)
}

func TestHeartbeatLease(t *testing.T) {
	t.Run("taken", func(t *testing.T) {
		mr := miniredis.RunT(t)
		err := heartbeatFor(t, mr, time.Second, func(q *queue.Store) {
			q.Release(context.Background(), queue.KindImplement, "r#1", "pod-1")
			q.Acquire(context.Background(), queue.KindImplement, "r#1", "pod-2", time.Second)
		}, time.Second)
		if !errors.Is(err, errLeaseLost) {
			t.Fatalf("want errLeaseLost, got %v", err)
		}
	})
	t.Run("brief outage", func(t *testing.T) {
		mr := miniredis.RunT(t)
		err := heartbeatFor(t, mr, time.Second, func(*queue.Store) {
			mr.SetError("LOADING")
			time.Sleep(100 * time.Millisecond)
			mr.SetError("")
		}, 500*time.Millisecond)
		if err != nil {
			t.Fatalf("a renewal that errors briefly is retried, but the job stopped: %v", err)
		}
	})
	t.Run("outage past the ttl", func(t *testing.T) {
		mr := miniredis.RunT(t)
		start := time.Now()
		err := heartbeatFor(t, mr, 300*time.Millisecond, func(*queue.Store) { mr.SetError("LOADING") }, 2*time.Second)
		if !errors.Is(err, errLeaseLost) {
			t.Fatalf("a lease that can't be confirmed before its ttl should stop the job: %v", err)
		}
		if d := time.Since(start); d > 300*time.Millisecond+50*time.Millisecond {
			t.Fatalf("stopped %v in, after the lease could have lapsed", d)
		}
	})
}

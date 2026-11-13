package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/fake"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// limitedForge answers every read with GitHub's primary rate limit until
// reset, on the test's clock.
type limitedForge struct {
	*fake.Forge
	now   *time.Time
	reset time.Time
	calls int
}

func (f *limitedForge) limited() error {
	f.calls++
	if f.now.Before(f.reset) {
		return &forge.RateLimitError{Provider: "github", Kind: forge.LimitPrimary, Until: f.reset}
	}
	return nil
}

func (f *limitedForge) GetFile(ctx context.Context, r forge.RepoRef, path, ref string) ([]byte, error) {
	if err := f.limited(); err != nil {
		return nil, err
	}
	return f.Forge.GetFile(ctx, r, path, ref)
}

func (f *limitedForge) GetItem(ctx context.Context, ref forge.ItemRef) (forge.Item, error) {
	if err := f.limited(); err != nil {
		return forge.Item{}, err
	}
	return f.Forge.GetItem(ctx, ref)
}

// A webhook event whose handling hits the forge's primary rate limit is
// kept until the reset, with its delivery still marked, and the task is
// evaluated then. Another event about the same task meanwhile coalesces
// into it, and a redelivery is a duplicate.
func TestRateLimitedEventIsHandledAfterTheReset(t *testing.T) {
	s, f := newServer(t, "")
	ctx := context.Background()
	now := time.Now()
	s.now = func() time.Time { return now }
	lf := &limitedForge{Forge: f, now: &now, reset: now.Add(time.Hour)}
	s.E.Forges["fake"] = lf
	s.E.Forges["hook"] = hookForge{f}
	ref := f.NewIssue(appRepo, "alice", "t", "- x")
	f.HumanLabel(ref, "alice", "devbot/ready")
	h := s.Handler()

	postHook(t, h, "d1")
	s.handle(ctx, <-s.events)
	if q, _ := s.E.Q.IsQueued(ctx, queue.KindImplement, ref.String()); q {
		t.Fatal("evaluated through the rate limit")
	}
	if n, _ := s.E.Q.LaterCount(ctx); n != 1 {
		t.Fatalf("%d events kept, want the one", n)
	}
	postHook(t, h, "d1")
	if len(s.events) != 0 {
		t.Fatal("a kept event's redelivery must be a duplicate")
	}
	postHook(t, h, "d2")
	s.handle(ctx, <-s.events)
	if n, _ := s.E.Q.LaterCount(ctx); n != 1 {
		t.Fatalf("%d events kept, want one for the task", n)
	}

	now = now.Add(30 * time.Minute)
	calls := lf.calls
	s.handleLater(ctx)
	if lf.calls != calls {
		t.Fatal("a kept event was handled before the reset")
	}

	now = lf.reset.Add(time.Second)
	s.handleLater(ctx)
	if q, _ := s.E.Q.IsQueued(ctx, queue.KindImplement, ref.String()); !q {
		t.Fatal("the task wasn't evaluated after the reset")
	}
	if n, _ := s.E.Q.LaterCount(ctx); n != 0 {
		t.Fatalf("%d events still kept", n)
	}
	stats, _ := s.E.Q.Stats(ctx)
	if stats["webhook_requeue|requeued"] != 1 || stats["webhook_requeue|coalesced"] != 1 {
		t.Fatalf("counted %v", stats)
	}
	if m := scrape(t, s); !strings.Contains(m, `devbot_webhook_requeue_total{outcome="requeued"} 1`) || !strings.Contains(m, "devbot_webhook_events_kept 0") {
		t.Fatalf("metrics:\n%s", m)
	}
}

// An event the rate limit puts off past maxEventTries is dropped, and its
// delivery unmarked, so a redelivery is handled.
func TestEventPutOffTooOftenIsDropped(t *testing.T) {
	s, f := newServer(t, "")
	ctx := context.Background()
	now := time.Now()
	s.now = func() time.Time { return now }
	lf := &limitedForge{Forge: f, now: &now, reset: now.Add(time.Minute)}
	s.E.Forges["fake"] = lf
	s.E.Forges["hook"] = hookForge{f}
	f.NewIssue(appRepo, "alice", "t", "- x", "devbot/ready")

	postHook(t, s.Handler(), "d1")
	s.handle(ctx, <-s.events)
	for range maxEventTries {
		// The limit never lifts.
		now = lf.reset.Add(time.Second)
		lf.reset = now.Add(time.Minute)
		s.handleLater(ctx)
	}
	if n, _ := s.E.Q.LaterCount(ctx); n != 0 {
		t.Fatalf("%d events still kept", n)
	}
	if first, _ := s.E.Q.FirstDelivery(ctx, "d1"); !first {
		t.Fatal("a dropped event's delivery must be unmarked")
	}
	if stats, _ := s.E.Q.Stats(ctx); stats["webhook_requeue|dropped"] != 1 {
		t.Fatalf("counted %v", stats)
	}
}

// The forge's quotas, as any process noted them, are gauges by bucket and
// resource, beside this process's requests and rate-limit errors. People's
// own buckets are left out.
func TestForgeQuotaMetrics(t *testing.T) {
	s, _ := newServer(t, "")
	ctx := context.Background()
	reset := time.Unix(1700000000, 0)
	req := forge.Request{Provider: "fake", Bucket: "installation 7", Method: "GET", Endpoint: "/repos/{owner}/{repo}", Status: "304"}
	// The counters are process-wide and earlier runs have added to them, so
	// count on from what they held when this one started.
	reqs := forge.Requests.Snapshot()[req]
	lows := forge.RateLimitErrors.Snapshot()[[2]string{"fake", forge.LimitLowQuota}]
	s.E.Q.NoteQuota(ctx, "fake", "installation 7", "core", forge.Quota{Remaining: 42, Limit: 5000, Reset: reset})
	s.E.Q.NoteQuota(ctx, "fake", "user 0a1b2c", "core", forge.Quota{Remaining: 1, Limit: 5000, Reset: reset})
	forge.Requests.Add(req)
	forge.RateLimitErrors.Add("fake", forge.LimitLowQuota)
	m := scrape(t, s)
	for _, want := range []string{
		`devbot_forge_ratelimit_remaining{provider="fake",bucket="installation 7",resource="core"} 42` + "\n",
		`devbot_forge_ratelimit_limit{provider="fake",bucket="installation 7",resource="core"} 5000` + "\n",
		`devbot_forge_ratelimit_reset_seconds{provider="fake",bucket="installation 7",resource="core"} 1700000000` + "\n",
		fmt.Sprintf(`devbot_forge_requests_total{provider="fake",bucket="installation 7",method="GET",endpoint="/repos/{owner}/{repo}",status="304"} %d`+"\n", reqs+1),
		fmt.Sprintf(`devbot_forge_ratelimit_errors_total{provider="fake",kind="low_quota"} %d`+"\n", lows+1),
	} {
		if !strings.Contains(m, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(m, "user 0a1b2c") {
		t.Error("a person's bucket is a series")
	}
}

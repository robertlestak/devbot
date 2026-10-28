package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"git.shdw.tech/rob/devbot/internal/changeset"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/fake"
	"git.shdw.tech/rob/devbot/internal/queue"
)

var haRepo = forge.RepoRef{Provider: "fake", Owner: "acme", Name: "app"}

// searching counts SearchItems, which only a reconcile pass calls here.
type searching struct {
	*fake.Forge
	searches atomic.Int32
}

func (s *searching) SearchItems(ctx context.Context, label, state string) ([]forge.Item, error) {
	s.searches.Add(1)
	return s.Forge.SearchItems(ctx, label, state)
}

// replica is one serve: its own engine, Redis connection and caches, on the
// shared Redis and forge.
type replica struct {
	s      *Server
	f      *searching
	stop   context.CancelFunc
	done   *sync.WaitGroup
	closed atomic.Bool
}

// newReplica starts a replica; tune adjusts its server before its loops
// start.
func newReplica(t *testing.T, mr *miniredis.Miniredis, f *fake.Forge, cfg *config.Config, tune ...func(*Server)) *replica {
	t.Helper()
	sf := &searching{Forge: f}
	reg := forge.Registry{"fake": sf}
	q := queue.New(mr.Addr(), 0)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := &core.Engine{Forges: reg, Q: q, Cfg: cfg, Repos: &config.RepoCache{Forges: reg, Generation: q.RepoConfigGeneration}, Log: log}
	// Every task's PR changes one ordinary file.
	e.Changes = func(context.Context, forge.Provider, forge.PR) ([]changeset.Change, error) {
		return []changeset.Change{{Status: "M", Path: "main.go", OldMode: "100644", NewMode: "100644"}}, nil
	}
	s, err := New(e, cfg, log, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, fn := range tune {
		fn(s)
	}
	ctx, stop := context.WithCancel(context.Background())
	r := &replica{s: s, f: sf, stop: stop, done: &sync.WaitGroup{}}
	r.done.Go(func() { s.background(ctx).Wait() })
	r.done.Go(func() { s.workers(ctx, nil).Wait() })
	t.Cleanup(r.crash)
	return r
}

// crash stops the replica the way a killed pod would: it never gets to
// resign, so its lease has to expire.
func (r *replica) crash() {
	if r.closed.Swap(true) {
		return
	}
	r.s.E.Q.R.Close()
	r.stop()
	r.done.Wait()
}

// approvedTask is an issue in review whose PR is green and approved, so the
// next evaluation approves it (automerge is off): one comment, one event. Without
// labels it isn't a task yet.
func approvedTask(t *testing.T, f *fake.Forge, n int, labels ...string) forge.ItemRef {
	ctx := context.Background()
	ref := f.NewIssue(haRepo, "alice", fmt.Sprintf("task %d", n), "", labels...)
	pr, _ := f.OpenPR(ctx, haRepo, fmt.Sprintf("devbot/%d-x", ref.Number), "main", "t", core.RenderPRMeta(core.PRMeta{Issue: ref.Number}))
	f.AddComment(ref, "devbot", "opened "+core.RenderStatus(core.Status{Kind: "pr", PR: pr.Number}))
	f.SetStatus(pr.HeadSHA, "ci", "success")
	f.AddReview(haRepo, pr.Number, "devbot-review", pr.HeadSHA, forge.VerdictApprove, "lgtm")
	return ref
}

func approvals(t *testing.T, f *fake.Forge, ref forge.ItemRef) int {
	cs, _ := f.ListComments(context.Background(), ref)
	n := 0
	for _, s := range core.Statuses(cs, "devbot", "devbot-review") {
		if s.Kind == "approved" {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !cond() {
		if time.Since(start) > within {
			t.Fatalf("not within %s: %s", within, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return time.Since(start)
}

// Two serve replicas on one Redis: one leads the singleton loops at a time,
// webhooks reach both and no task is processed twice, and when the leader
// dies a follower takes over within the lease and carries on its work,
// subscriber delivery included.
func TestTwoReplicasOneLeader(t *testing.T) {
	mr := miniredis.RunT(t)
	// miniredis only expires keys when told time has passed.
	clock := make(chan struct{})
	ticked := make(chan struct{})
	go func() {
		defer close(ticked)
		last := time.Now()
		for {
			select {
			case <-clock:
				return
			case <-time.After(5 * time.Millisecond):
			}
			now := time.Now()
			mr.FastForward(now.Sub(last))
			last = now
		}
	}()
	defer func() {
		close(clock)
		<-ticked
	}()

	var mu sync.Mutex
	var delivered []string
	sub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e queue.Event
		json.NewDecoder(r.Body).Decode(&e)
		mu.Lock()
		delivered = append(delivered, r.Header.Get("X-Devbot-Event-Id"))
		mu.Unlock()
	}))
	defer sub.Close()

	f := fake.New()
	f.AddRepo(haRepo, "version: 1\nenabled: true\n")
	var tasks []forge.ItemRef
	for i := range 4 {
		tasks = append(tasks, approvedTask(t, f, i, core.LInReview))
	}
	// Labelled only once the leader has died; set up now, before the
	// replicas read the fake.
	late := approvedTask(t, f, 99)
	var ready []forge.ItemRef
	for i := range 3 {
		ready = append(ready, f.NewIssue(haRepo, "alice", fmt.Sprintf("ready %d", i), "", core.LReady))
	}

	cfg, _ := config.Load("")
	cfg.LeaderLease = time.Second
	cfg.ReconcileInterval = 50 * time.Millisecond
	cfg.Subscribers = []config.Subscriber{{Name: "relay", URL: sub.URL}}
	ctx := context.Background()
	// The subscriber has been running: it delivers from here on.
	q := queue.New(mr.Addr(), 0)
	tok, _ := q.TakeLeader(ctx, "setup", time.Minute)
	q.SetCursor(ctx, "relay", "0-1", tok)
	q.ResignLeader(ctx, tok)

	fast := func(s *Server) { s.deliverEvery = 20 * time.Millisecond }
	a, b := newReplica(t, mr, f, cfg, fast), newReplica(t, mr, f, cfg, fast)
	both := []*replica{a, b}
	// Stopped before the test returns, not in its cleanup, so no loop
	// outlives what it reads.
	defer func() {
		for _, r := range both {
			r.crash()
		}
	}()

	// Never two leaders at once, sampled throughout.
	var overlap atomic.Bool
	watch := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-watch:
				return
			case <-time.After(time.Millisecond):
			}
			if a.s.leading.Load() && b.s.leading.Load() {
				overlap.Store(true)
			}
		}
	}()

	waitFor(t, 2*time.Second, "a leader", func() bool { return a.s.leading.Load() || b.s.leading.Load() })
	leader, follower := a, b
	if b.s.leading.Load() {
		leader, follower = b, a
	}
	// The same webhooks, redelivered to both replicas, while the leader
	// reconciles.
	for _, r := range both {
		for _, ref := range append(slices.Clone(tasks), ready...) {
			r.s.events <- forge.Event{Kind: forge.EventItem, Repo: ref.Repo, Number: ref.Number}
		}
	}
	waitFor(t, 5*time.Second, "every task approved", func() bool {
		for _, ref := range tasks {
			if it, _ := f.GetItem(ctx, ref); core.State(it) != core.LApproved {
				return false
			}
		}
		return true
	})
	// Some more reconcile passes, which must find nothing to do.
	passes := leader.f.searches.Load()
	waitFor(t, 5*time.Second, "more reconcile passes", func() bool { return leader.f.searches.Load() > passes+3*int32(len(core.StateLabels)) })
	for _, ref := range tasks {
		if n := approvals(t, f, ref); n != 1 {
			t.Errorf("%s approved %d times", ref, n)
		}
	}
	if n, _ := q.R.LLen(ctx, queue.QueueKey(queue.KindImplement, false)).Result(); n != int64(len(ready)) {
		t.Errorf("want one implement job per ready task, got %d", n)
	}
	if n := follower.f.searches.Load(); n != 0 {
		t.Errorf("the follower ran %d reconcile searches", n)
	}

	// The leader dies. The follower leads within the lease (plus its poll),
	// and picks up work that arrives after.
	leader.crash()
	took := waitFor(t, 3*time.Second, "failover", follower.s.leading.Load)
	if limit := cfg.LeaderLease + cfg.LeaderLease/10 + 200*time.Millisecond; took > limit {
		t.Errorf("failover took %s, want under %s", took, limit)
	}
	f.SetLabels(ctx, late, []string{core.LInReview}, nil)
	waitFor(t, 5*time.Second, "the new leader reconciles", func() bool {
		it, _ := f.GetItem(ctx, late)
		return core.State(it) == core.LApproved
	})
	if n := approvals(t, f, late); n != 1 {
		t.Errorf("the late task was approved %d times", n)
	}

	// Every event reached the subscriber, in order, across the handover.
	evs, _, _ := q.Events(ctx, "", 1000)
	var want []string
	for _, e := range evs {
		want = append(want, e.ID)
	}
	waitFor(t, 5*time.Second, "every event delivered", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(delivered) > 0 && delivered[len(delivered)-1] == want[len(want)-1]
	})
	mu.Lock()
	got := slices.Compact(slices.Clone(delivered))
	mu.Unlock()
	if !slices.Equal(got, want) {
		t.Errorf("delivered %v, want %v in order", got, want)
	}

	close(watch)
	<-watched
	if overlap.Load() {
		t.Error("both replicas led at once")
	}
}

// A replica that shuts down cleanly resigns, so the other takes over at
// its next try instead of waiting for the lease to run out.
func TestLeaderResignsOnShutdown(t *testing.T) {
	mr := miniredis.RunT(t)
	f := fake.New()
	f.AddRepo(haRepo, "version: 1\nenabled: true\n")
	cfg, _ := config.Load("")
	// miniredis's clock stands still here, so the lease never expires: only
	// a resignation frees it.
	cfg.LeaderLease = 10 * time.Second
	a := newReplica(t, mr, f, cfg)
	waitFor(t, 2*time.Second, "a leads", a.s.leading.Load)
	b := newReplica(t, mr, f, cfg)
	defer b.crash()
	a.stop()
	a.done.Wait()
	if a.s.leading.Load() {
		t.Fatal("a still leads after shutting down")
	}
	waitFor(t, 3*time.Second, "b leads", b.s.leading.Load)
}

package queue

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"git.shdw.tech/rob/devbot/internal/config"
)

func TestEvalLock(t *testing.T) {
	mr := miniredis.RunT(t)
	s := New(mr.Addr(), 0)
	ctx := context.Background()
	const ref, ttl = "fake:acme/app#1", time.Minute

	if ok, err := s.LockEval(ctx, ref, "a", ttl); err != nil || !ok {
		t.Fatalf("first take: %v %v", ok, err)
	}
	if ok, _ := s.LockEval(ctx, ref, "b", ttl); ok {
		t.Fatal("a held lock must not be taken")
	}
	if dirty, _ := s.UnlockEval(ctx, ref, "b"); dirty {
		t.Fatal("a non-owner's release must neither drop the lock nor consume the request")
	}
	if dirty, _ := s.UnlockEval(ctx, ref, "a"); !dirty {
		t.Fatal("the holder should learn that b asked meanwhile")
	}
	if dirty, _ := s.UnlockEval(ctx, ref, "a"); dirty {
		t.Fatal("the request is consumed once")
	}

	// An evaluation that outlives its TTL doesn't release the next holder's
	// lock, and leaves any request to it.
	s.LockEval(ctx, ref, "a", ttl)
	mr.FastForward(2 * ttl)
	if ok, _ := s.LockEval(ctx, ref, "c", ttl); !ok {
		t.Fatal("an expired lock should be free")
	}
	s.LockEval(ctx, ref, "d", ttl)
	if dirty, _ := s.UnlockEval(ctx, ref, "a"); dirty {
		t.Fatal("a stale owner must not take c's request")
	}
	if h, _ := s.R.Get(ctx, evalKey(ref)).Result(); h != "c" {
		t.Fatalf("c's lock should survive a's release, holder %q", h)
	}
	if dirty, _ := s.UnlockEval(ctx, ref, "c"); !dirty {
		t.Fatal("c should see d's request")
	}
}

func TestRunningQueuedDelayed(t *testing.T) {
	mr := miniredis.RunT(t)
	s := New(mr.Addr(), 0)
	ctx := context.Background()
	const ref = "fake:acme/app#1"

	if ok, _ := s.Acquire(ctx, KindImplement, ref, "pod-a", time.Minute); !ok {
		t.Fatal("acquire")
	}
	// Only the holder describes the job.
	s.Describe(ctx, KindImplement, ref, "pod-b", LeaseDetail{Attempt: 9})
	s.Describe(ctx, KindImplement, ref, "pod-a", LeaseDetail{Mode: "fix", PR: 4, Attempt: 2, MaxAttempts: 3})
	run, err := s.Running(ctx)
	if err != nil || len(run) != 1 {
		t.Fatalf("running: %v %v", run, err)
	}
	l := run[0]
	if l.Kind != KindImplement || l.Ref != ref || l.Holder != "pod-a" || l.Attempt != 2 || l.MaxAttempts != 3 || l.PR != 4 || l.Started.IsZero() || l.Heartbeat.IsZero() {
		t.Fatalf("lease: %+v", l)
	}
	if n, _ := s.Leases(ctx); n[KindImplement] != 1 {
		t.Fatalf("the info key mustn't count as a lease: %v", n)
	}
	// The info goes with the lease, whether released or expired.
	s.Release(ctx, KindImplement, ref, "pod-a")
	if run, _ := s.Running(ctx); len(run) != 0 || mr.Exists(leaseInfoKey(KindImplement, ref)) {
		t.Fatalf("released: %v", run)
	}
	s.Acquire(ctx, KindReview, ref, "pod-c", time.Minute)
	mr.FastForward(2 * time.Minute)
	if run, _ := s.Running(ctx); len(run) != 0 || mr.Exists(leaseInfoKey(KindReview, ref)) {
		t.Fatalf("expired: %v", run)
	}
	// A lease re-taken by Renew after a Redis wipe still shows when.
	s.Renew(ctx, KindReview, ref, "pod-c", time.Minute)
	if run, _ := s.Running(ctx); len(run) != 1 || run[0].Started.IsZero() {
		t.Fatalf("renewed: %v", run)
	}

	s.Enqueue(ctx, Job{Kind: KindImplement, Ref: "fake:acme/app#2"})
	s.Enqueue(ctx, Job{Kind: KindImplement, Ref: "fake:acme/app#3"})
	q, err := s.Queued(ctx)
	if err != nil || len(q) != 2 || q[0].Ref != "fake:acme/app#2" {
		t.Fatalf("queued, next first: %v %v", q, err)
	}
	at := time.Now().Add(time.Hour).Truncate(time.Second)
	s.Delay(ctx, Job{Kind: KindReview, Ref: "fake:acme/app#5", RateLimitN: 1}, at, "")
	d, err := s.Delayed(ctx)
	if err != nil || len(d) != 1 || !d[0].Release.Equal(at) || d[0].RateLimitN != 1 {
		t.Fatalf("delayed: %v %v", d, err)
	}
}

// A lease that lapses while Redis is out of reach comes back with its
// detail when the holder renews it, so serve still sees its services.
func TestRenewRestoresDetail(t *testing.T) {
	mr := miniredis.RunT(t)
	s := New(mr.Addr(), 0)
	ctx := context.Background()
	const ref, ttl = "fake:acme/app#1", time.Minute

	s.Acquire(ctx, KindImplement, ref, "pod-a", ttl)
	s.Describe(ctx, KindImplement, ref, "pod-a", LeaseDetail{Pod: "job-pod", Services: []config.Service{{Name: "db", Image: "postgres"}}, Attempt: 1})
	mr.FastForward(2 * ttl)
	if run, _ := s.Running(ctx); len(run) != 0 {
		t.Fatalf("the lease should have lapsed: %v", run)
	}
	if ok, err := s.Renew(ctx, KindImplement, ref, "pod-a", ttl); err != nil || !ok {
		t.Fatalf("renew: %v %v", ok, err)
	}
	run, _ := s.Running(ctx)
	if len(run) != 1 || run[0].Pod != "job-pod" || len(run[0].Services) != 1 || run[0].Services[0].Name != "db" || run[0].Attempt != 1 {
		t.Fatalf("re-taken lease lost its detail: %+v", run)
	}
	// After a wipe too, and with what was described last.
	s.Describe(ctx, KindImplement, ref, "pod-a", LeaseDetail{Pod: "job-pod", Services: []config.Service{{Name: "db", Image: "postgres"}}, Attempt: 2})
	mr.FlushAll()
	s.Renew(ctx, KindImplement, ref, "pod-a", ttl)
	if run, _ := s.Running(ctx); len(run) != 1 || run[0].Pod != "job-pod" || run[0].Attempt != 2 {
		t.Fatalf("after a wipe: %+v", run)
	}
	// Another holder's renewal doesn't get it, and a released lease forgets it.
	s.Release(ctx, KindImplement, ref, "pod-a")
	s.Renew(ctx, KindImplement, ref, "pod-a", ttl)
	if run, _ := s.Running(ctx); len(run) != 1 || run[0].Pod != "" {
		t.Fatalf("released: %+v", run)
	}
	s.Release(ctx, KindImplement, ref, "pod-a")
	s.Renew(ctx, KindImplement, ref, "pod-b", ttl)
	if run, _ := s.Running(ctx); len(run) != 1 || run[0].Pod != "" {
		t.Fatalf("another holder: %+v", run)
	}
}

func TestEventsBefore(t *testing.T) {
	mr := miniredis.RunT(t)
	s := New(mr.Addr(), 0)
	ctx := context.Background()
	for _, typ := range []string{"a", "b", "c"} {
		s.Emit(ctx, Event{Type: typ}, time.Hour)
	}
	evs, next, err := s.EventsBefore(ctx, "", 2)
	if err != nil || len(evs) != 2 || evs[0].Type != "c" || evs[1].Type != "b" {
		t.Fatalf("newest first: %v %v", evs, err)
	}
	evs, _, _ = s.EventsBefore(ctx, next, 2)
	if len(evs) != 1 || evs[0].Type != "a" {
		t.Fatalf("the page after: %v", evs)
	}
}

func TestPopDropsDedupe(t *testing.T) {
	mr := miniredis.RunT(t)
	s := New(mr.Addr(), 0)
	ctx := context.Background()
	const ref = "fake:acme/app#1"

	s.Enqueue(ctx, Job{Kind: KindImplement, Ref: ref})
	if j, err := s.Pop(ctx, KindImplement, false); err != nil || j == nil || j.Ref != ref {
		t.Fatalf("pop: %+v %v", j, err)
	}
	if q, _ := s.IsQueued(ctx, KindImplement, ref); q {
		t.Fatal("a popped job must not stay queued")
	}
	if j, err := s.Pop(ctx, KindImplement, false); err != nil || j != nil {
		t.Fatalf("empty queue: %+v %v", j, err)
	}

	// Pop's error reaches the caller.
	mr.SetError("down")
	if _, err := s.Pop(ctx, KindImplement, false); err == nil {
		t.Fatal("a Redis error must be returned")
	}
}

func TestPruneQueued(t *testing.T) {
	mr := miniredis.RunT(t)
	s := New(mr.Addr(), 0)
	ctx := context.Background()
	const stale, queued, delayed, running = "fake:acme/app#1", "fake:acme/app#2", "fake:acme/app#3", "fake:acme/app#4"

	// A Pop that died between the list and the set, as one could before the
	// two were a single script.
	s.R.SAdd(ctx, keyQueued, KindImplement+"|"+stale)
	s.Enqueue(ctx, Job{Kind: KindImplement, Ref: queued})
	s.Delay(ctx, Job{Kind: KindReview, Ref: delayed}, time.Now().Add(time.Hour), "")
	s.R.SAdd(ctx, keyQueued, KindImplement+"|"+running)
	s.Acquire(ctx, KindImplement, running, "pod-a", time.Minute)

	if n, err := s.PruneQueued(ctx); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	for _, c := range []struct {
		kind, ref string
		want      bool
	}{{KindImplement, stale, false}, {KindImplement, queued, true}, {KindReview, delayed, true}, {KindImplement, running, true}} {
		if q, _ := s.IsQueued(ctx, c.kind, c.ref); q != c.want {
			t.Errorf("%s %s queued=%v, want %v", c.kind, c.ref, q, c.want)
		}
	}
	if added, err := s.Enqueue(ctx, Job{Kind: KindImplement, Ref: stale}); err != nil || !added {
		t.Fatalf("the unblocked task should enqueue: %v %v", added, err)
	}
	if n, err := s.PruneQueued(ctx); err != nil || n != 0 {
		t.Fatalf("second prune should do nothing: %d %v", n, err)
	}
}

// The once key goes with the append: an append that fails leaves the event
// to a later try, and only one try appends it.
func TestEmitOnceSurvivesAFailedAppend(t *testing.T) {
	mr := miniredis.RunT(t)
	s := New(mr.Addr(), 0)
	ctx := context.Background()
	mr.Set(keyEvents, "not a stream")
	if _, first, err := s.EmitOnce(ctx, Event{Type: "task.merged"}, "k", time.Hour); err == nil || first {
		t.Fatalf("an append to a broken stream must fail: %v %v", first, err)
	}
	mr.Del(keyEvents)
	if _, first, err := s.EmitOnce(ctx, Event{Type: "task.merged"}, "k", time.Hour); err != nil || !first {
		t.Fatalf("the retry should append: %v %v", first, err)
	}
	if _, first, err := s.EmitOnce(ctx, Event{Type: "task.merged"}, "k", time.Hour); err != nil || first {
		t.Fatalf("a second emit must not append: %v %v", first, err)
	}
	evs, _, _ := s.Events(ctx, "", 10)
	stats, _ := s.Stats(ctx)
	if len(evs) != 1 || stats["events|task.merged"] != 1 {
		t.Fatalf("want one event, counted once: %v %v", evs, stats)
	}
}

// One leader at a time; a renewal or a cursor write with a token that no
// longer holds the lease is refused, even after the lease passes on.
func TestLeaderLeaseFences(t *testing.T) {
	mr := miniredis.RunT(t)
	s := New(mr.Addr(), 0)
	ctx := context.Background()
	const ttl = 10 * time.Second

	a, err := s.TakeLeader(ctx, "pod-a", ttl)
	if err != nil || a == "" {
		t.Fatalf("first take: %q %v", a, err)
	}
	if b, _ := s.TakeLeader(ctx, "pod-b", ttl); b != "" {
		t.Fatalf("a held lease was taken: %q", b)
	}
	if ok, _ := s.SetCursor(ctx, "relay", "1-0", a); !ok {
		t.Fatal("the leader should move the cursor")
	}
	if ok, _ := s.RenewLeader(ctx, a, ttl); !ok {
		t.Fatal("the leader should renew")
	}

	// a stalls past its lease; b takes over with a later term.
	mr.FastForward(ttl + time.Second)
	b, _ := s.TakeLeader(ctx, "pod-b", ttl)
	if b == "" || b == a {
		t.Fatalf("an expired lease should pass on with a new token: a=%q b=%q", a, b)
	}
	if ok, _ := s.RenewLeader(ctx, a, ttl); ok {
		t.Fatal("a deposed leader must not renew")
	}
	if ok, _ := s.SetCursor(ctx, "relay", "9-0", a); ok {
		t.Fatal("a deposed leader must not move the cursor")
	}
	if c, _ := s.Cursor(ctx, "relay"); c != "1-0" {
		t.Fatalf("cursor moved to %q", c)
	}
	s.ResignLeader(ctx, a)
	if l, _ := s.Leader(ctx); l != b {
		t.Fatalf("a deposed leader's resignation dropped the new lease: %q", l)
	}
	s.ResignLeader(ctx, b)
	if l, _ := s.Leader(ctx); l != "" {
		t.Fatalf("resigned, the lease should be free: %q", l)
	}

	// After a wipe the term starts over, but a token is still never reused,
	// because owners differ.
	mr.FlushAll()
	if c, _ := s.TakeLeader(ctx, "pod-c", ttl); c == a || c == b || c == "" {
		t.Fatalf("token after a wipe: %q", c)
	}
}

func TestRebaseQueue(t *testing.T) {
	s := New(miniredis.RunT(t).Addr(), 0)
	ctx := context.Background()
	const base = "fake:acme/app|main"
	t0 := time.Now()
	s.JoinRebaseQueue(ctx, base, "b", t0.Add(time.Second))
	s.JoinRebaseQueue(ctx, base, "a", t0)
	s.JoinRebaseQueue(ctx, base, "a", t0.Add(time.Hour)) // keeps its place
	if q, _ := s.RebaseQueue(ctx, base); len(q) != 2 || q[0] != "a" {
		t.Fatalf("want oldest first, got %v", q)
	}
	if h, _ := s.RebaseTurn(ctx, base, "b", true); h != "" {
		t.Fatalf("b isn't first: holder %q", h)
	}
	if h, _ := s.RebaseTurn(ctx, base, "a", true); h != "a" {
		t.Fatalf("a is first: holder %q", h)
	}
	if h, _ := s.RebaseTurn(ctx, base, "b", false); h != "a" {
		t.Fatalf("a held turn isn't taken: holder %q", h)
	}
	s.LeaveRebaseQueue(ctx, base, "b") // not the holder: the turn stays
	if h, _ := s.RebaseTurnHolder(ctx, base); h != "a" {
		t.Fatalf("holder %q", h)
	}
	s.LeaveRebaseQueue(ctx, base, "a")
	if h, _ := s.RebaseTurnHolder(ctx, base); h != "" {
		t.Fatalf("leaving gives up the turn: holder %q", h)
	}
	if h, _ := s.RebaseTurn(ctx, base, "c", false); h != "c" {
		t.Fatalf("a rebase in flight claims a free turn: holder %q", h)
	}
}

func TestBehind(t *testing.T) {
	s := New(miniredis.RunT(t).Addr(), 0)
	ctx := context.Background()
	s.MarkBehind(ctx, "fake:acme/app", "fake:acme/app#1", true)
	s.MarkBehind(ctx, "fake:acme/app", "fake:acme/app#2", true)
	s.MarkBehind(ctx, "fake:acme/other", "fake:acme/other#1", true)
	s.MarkBehind(ctx, "fake:acme/app", "fake:acme/app#2", false)
	got, err := s.Behind(ctx, time.Now().Add(-time.Minute))
	if err != nil || got["fake:acme/app"] != 1 || got["fake:acme/other"] != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, _ := s.Behind(ctx, time.Now().Add(time.Minute)); len(got) != 0 {
		t.Fatalf("entries nobody refreshed should age out: %v", got)
	}
}

// A repo that asks for a Docker daemon has its jobs on their kind's Docker
// list, which only pods with a daemon pop, and they come back there from a
// delay. Dedupe is by kind and task either way.
func TestDockerJobsHaveTheirOwnList(t *testing.T) {
	mr := miniredis.RunT(t)
	s := New(mr.Addr(), 0)
	ctx := context.Background()
	const ref = "fake:acme/app#1"

	s.Enqueue(ctx, Job{Kind: KindReview, Ref: ref, PR: 2, Docker: true})
	if added, _ := s.Enqueue(ctx, Job{Kind: KindReview, Ref: ref, PR: 2}); added {
		t.Fatal("a task queued on the Docker list must not queue again on the other")
	}
	if j, err := s.Pop(ctx, KindReview, false); err != nil || j != nil {
		t.Fatalf("a pod without a daemon took a Docker job: %+v %v", j, err)
	}
	if d, _ := s.Depths(ctx); d[KindReview] != 1 {
		t.Errorf("depths %v, want the Docker list counted in review", d)
	}
	if q, _ := s.Queued(ctx); len(q) != 1 || !q[0].Docker {
		t.Errorf("queued %+v", q)
	}
	j, err := s.Pop(ctx, KindReview, true)
	if err != nil || j == nil || !j.Docker || j.Ref != ref {
		t.Fatalf("pop: %+v %v", j, err)
	}

	s.Delay(ctx, *j, time.Now().Add(-time.Second), "")
	if n, err := s.ReleaseDue(ctx); err != nil || n != 1 {
		t.Fatalf("release: %d %v", n, err)
	}
	if !mr.Exists(QueueKey(KindReview, true)) || mr.Exists(QueueKey(KindReview, false)) {
		t.Fatal("a released Docker job must go back to the Docker list")
	}
	if n, _ := s.PruneQueued(ctx); n != 0 {
		t.Fatalf("prune dropped the Docker job's entry: %d", n)
	}
}

// Undock puts the Docker list's jobs ahead of the plain list's, oldest
// first, and leaves a plain delayed job as it was.
func TestUndockKeepsTheOrder(t *testing.T) {
	mr := miniredis.RunT(t)
	s := New(mr.Addr(), 0)
	ctx := context.Background()
	s.Enqueue(ctx, Job{Kind: KindImplement, Ref: "fake:acme/app#1", Docker: true})
	s.Enqueue(ctx, Job{Kind: KindImplement, Ref: "fake:acme/app#2", Docker: true, DockerWhy: "`compose.yaml` is a Docker Compose file"})
	s.Enqueue(ctx, Job{Kind: KindImplement, Ref: "fake:acme/app#3"})
	s.Delay(ctx, Job{Kind: KindReview, Ref: "fake:acme/app#4", PR: 5}, time.Now().Add(time.Hour), "")
	before, _ := mr.ZMembers(keyDelayed)

	if n, err := s.Undock(ctx); err != nil || n != 2 {
		t.Fatalf("undock: %d %v", n, err)
	}
	if after, _ := mr.ZMembers(keyDelayed); after[0] != before[0] {
		t.Fatalf("a plain delayed job changed: %s", after[0])
	}
	for _, want := range []string{"fake:acme/app#1", "fake:acme/app#2", "fake:acme/app#3"} {
		if j, err := s.Pop(ctx, KindImplement, false); err != nil || j == nil || j.Ref != want || j.Docker || j.DockerWhy != "" {
			t.Fatalf("want %s, got %+v %v", want, j, err)
		}
	}
	if n, _ := s.Undock(ctx); n != 0 {
		t.Fatalf("nothing left to move, but moved %d", n)
	}
}

// Claim takes the job and its lease in one step, so an Enqueue at any
// moment finds the one or the other. A job someone else holds the lease of
// is a duplicate, and is dropped.
func TestClaim(t *testing.T) {
	mr := miniredis.RunT(t)
	s := New(mr.Addr(), 0)
	ctx := context.Background()
	const ref = "fake:acme/app#1"

	s.Enqueue(ctx, Job{Kind: KindImplement, Ref: ref, Mode: "new"})
	j, held, err := s.Claim(ctx, KindImplement, false, "pod-a", time.Minute)
	if err != nil || j == nil || !held || j.Ref != ref || j.Mode != "new" {
		t.Fatalf("claim: %+v %v %v", j, held, err)
	}
	if h, _ := s.Holder(ctx, KindImplement, ref); h != "pod-a" {
		t.Fatalf("the lease should be pod-a's, got %q", h)
	}
	if q, _ := s.IsQueued(ctx, KindImplement, ref); q {
		t.Fatal("a claimed job must not stay queued")
	}
	if ls, _ := s.Running(ctx); len(ls) != 1 || ls[0].Started.IsZero() {
		t.Fatalf("the lease should be shown running from now: %+v", ls)
	}

	// A duplicate queued meanwhile is dropped by whoever claims it.
	s.Enqueue(ctx, Job{Kind: KindImplement, Ref: ref})
	if j, held, err := s.Claim(ctx, KindImplement, false, "pod-b", time.Minute); err != nil || j == nil || held {
		t.Fatalf("a duplicate should be dropped: %+v %v %v", j, held, err)
	}
	if d, _ := s.Depths(ctx); d[KindImplement] != 0 {
		t.Fatalf("the duplicate should be gone: %v", d)
	}
	if h, _ := s.Holder(ctx, KindImplement, ref); h != "pod-a" {
		t.Fatalf("pod-a keeps its lease, got %q", h)
	}
	if j, held, err := s.Claim(ctx, KindImplement, false, "pod-b", time.Minute); err != nil || j != nil || held {
		t.Fatalf("empty queue: %+v %v %v", j, held, err)
	}
}

// Delay puts a job back to wait without making a duplicate: not when one
// is queued or delayed for the task already, nor when another pod holds
// its lease. The caller's own lease doesn't count.
func TestDelayRespectsDedupe(t *testing.T) {
	mr := miniredis.RunT(t)
	s := New(mr.Addr(), 0)
	ctx := context.Background()
	const ref = "fake:acme/app#1"
	later := time.Now().Add(time.Hour)
	delayed := func() int {
		ds, _ := s.Delayed(ctx)
		return len(ds)
	}

	s.Acquire(ctx, KindReview, ref, "pod-a", time.Minute)
	if ok, err := s.Delay(ctx, Job{Kind: KindReview, Ref: ref, RateLimitN: 1}, later, "pod-a"); err != nil || !ok || delayed() != 1 {
		t.Fatalf("the lease holder parks its own job: %v %v, %d delayed", ok, err, delayed())
	}
	if ok, _ := s.Delay(ctx, Job{Kind: KindReview, Ref: ref, RateLimitN: 2}, later, "pod-a"); ok || delayed() != 1 {
		t.Fatalf("a job already delayed must not be delayed twice: %d delayed", delayed())
	}
	s.Release(ctx, KindReview, ref, "pod-a")
	s.R.Del(ctx, keyDelayed, keyQueued)

	s.Enqueue(ctx, Job{Kind: KindReview, Ref: ref})
	if ok, _ := s.Delay(ctx, Job{Kind: KindReview, Ref: ref}, later, ""); ok || delayed() != 0 {
		t.Fatalf("a job already queued must not be delayed besides: %d delayed", delayed())
	}
	s.Pop(ctx, KindReview, false)

	s.Acquire(ctx, KindReview, ref, "pod-b", time.Minute)
	if ok, _ := s.Delay(ctx, Job{Kind: KindReview, Ref: ref}, later, "pod-a"); ok || delayed() != 0 {
		t.Fatalf("a job another pod runs must not be delayed: %d delayed", delayed())
	}
	if q, _ := s.IsQueued(ctx, KindReview, ref); q {
		t.Fatal("a dropped job leaves no dedupe entry")
	}
}

// A head's pending checks are timed from when they were first seen, each
// head on its own.
func TestPendingSince(t *testing.T) {
	mr := miniredis.RunT(t)
	s := New(mr.Addr(), 0)
	ctx := context.Background()
	const ref, ttl = "fake:acme/app#1", time.Hour
	t0 := time.Unix(1_000_000, 0).UTC()

	if at, err := s.PendingSince(ctx, ref, 2, "aaa", t0, ttl); err != nil || !at.Equal(t0) {
		t.Fatalf("first sighting: %v %v", at, err)
	}
	if at, _ := s.PendingSince(ctx, ref, 2, "aaa", t0.Add(time.Minute), ttl); !at.Equal(t0) {
		t.Fatalf("a later sighting keeps the first: %v", at)
	}
	if at, _ := s.PendingSince(ctx, ref, 2, "bbb", t0.Add(time.Minute), ttl); !at.Equal(t0.Add(time.Minute)) {
		t.Fatalf("a new head starts over: %v", at)
	}
	mr.FastForward(ttl + 25*time.Hour)
	if at, _ := s.PendingSince(ctx, ref, 2, "aaa", t0.Add(2*time.Minute), ttl); !at.Equal(t0.Add(2 * time.Minute)) {
		t.Fatalf("an expired record starts over: %v", at)
	}
}

// A head's required reviews are timed on their own, apart from its checks.
func TestAwaitedSince(t *testing.T) {
	mr := miniredis.RunT(t)
	s := New(mr.Addr(), 0)
	ctx := context.Background()
	const ref, ttl = "fake:acme/app#1", time.Hour
	t0 := time.Unix(1_000_000, 0).UTC()

	s.PendingSince(ctx, ref, 2, "aaa", t0, ttl)
	if at, err := s.AwaitedSince(ctx, ref, 2, "aaa", t0.Add(time.Minute), ttl); err != nil || !at.Equal(t0.Add(time.Minute)) {
		t.Fatalf("first sighting: %v %v", at, err)
	}
	if at, _ := s.AwaitedSince(ctx, ref, 2, "aaa", t0.Add(2*time.Minute), ttl); !at.Equal(t0.Add(time.Minute)) {
		t.Fatalf("a later sighting keeps the first: %v", at)
	}
	if at, _ := s.AwaitedSince(ctx, ref, 2, "bbb", t0.Add(2*time.Minute), ttl); !at.Equal(t0.Add(2 * time.Minute)) {
		t.Fatalf("a new head starts over: %v", at)
	}
}

// Each bump moves a provider's inventory generation, and only its own; a
// wipe moves it too.
func TestInventoryGeneration(t *testing.T) {
	mr := miniredis.RunT(t)
	s := New(mr.Addr(), 0)
	ctx := context.Background()
	before, err := s.InventoryGeneration(ctx, "github")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BumpInventory(ctx, "github"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.InventoryGeneration(ctx, "github")
	other, _ := s.InventoryGeneration(ctx, "ghe")
	if after == before || other != before {
		t.Fatalf("before %q, after %q, another provider's %q", before, after, other)
	}
	mr.FlushAll()
	if wiped, _ := s.InventoryGeneration(ctx, "github"); wiped == after {
		t.Fatalf("a wipe left the generation at %q", wiped)
	}
}

func TestWaiters(t *testing.T) {
	mr := miniredis.RunT(t)
	s := New(mr.Addr(), 0)
	ctx := context.Background()
	const on = "fake:acme/lib#2"
	s.AddWaiter(ctx, on, "fake:acme/app#1")
	s.AddWaiter(ctx, on, "github:acme/web#7")
	s.AddWaiter(ctx, on, "fake:acme/app#1")
	got, err := s.TakeWaiters(ctx, on)
	if err != nil || len(got) != 2 || got[0] != "fake:acme/app#1" || got[1] != "github:acme/web#7" {
		t.Fatalf("want both waiters once: %v %v", got, err)
	}
	if got, _ := s.TakeWaiters(ctx, on); len(got) != 0 {
		t.Fatalf("taken waiters are forgotten: %v", got)
	}
	s.AddWaiter(ctx, on, "fake:acme/app#1")
	mr.FastForward(waitersTTL + time.Second)
	if got, _ := s.TakeWaiters(ctx, on); len(got) != 0 {
		t.Fatalf("a waiter nobody refreshes ages out: %v", got)
	}
}

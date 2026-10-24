// Package queue is devbot's Redis state: work queues KEDA scales on, leases,
// serve's leader lease, the rate-limit hold, the event feed and counters.
//
// None of it is authoritative. The forge is the system of record; wiping
// Redis and running one reconcile pass puts every queue and lease back.
package queue

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"git.shdw.tech/rob/devbot/internal/config"
)

const (
	KindImplement = "implement"
	KindReview    = "review"
	// KindPlan is a parent task's plan job: a model proposes its children
	// from read-only clones of its candidate repos. Implement job pods take
	// it (PodKind), and it never needs a Docker daemon.
	KindPlan = "plan"
	// KindIntegrate is a parent task's integration check, run over its
	// children's PR heads together. It runs no model, and implement job
	// pods take it (PodKind).
	KindIntegrate = "integrate"

	prefix     = "devbot:"
	keyQueued  = prefix + "queued"
	keyDelayed = prefix + "q:delayed"
	keyHold    = prefix + "ratelimit:until"
	keyEvents  = prefix + "events"
	keyStats   = prefix + "stats"
)

// QueueKey is the list KEDA watches for a kind. Jobs of repos that ask for
// a Docker daemon (Job.Docker) have lists of their own, which only the
// install's job pods with a daemon take, so no other repo pays for one.
func QueueKey(kind string, docker bool) string {
	if docker {
		return prefix + "q:" + kind + "-docker"
	}
	return prefix + "q:" + kind
}

// PodKind is the kind of job pod that runs a job of kind: the integration
// check and the plan job run in an implement pod, as the implementer, so
// they need no pod, credential or KEDA scaler of their own.
func PodKind(kind string) string {
	if kind == KindIntegrate || kind == KindPlan {
		return KindImplement
	}
	return kind
}

// queueKeys are every work queue: each kind's, and the Docker lists of the
// kinds that run in a job pod.
func queueKeys() []string {
	return []string{QueueKey(KindImplement, false), QueueKey(KindImplement, true),
		QueueKey(KindReview, false), QueueKey(KindReview, true), QueueKey(KindIntegrate, false), QueueKey(KindIntegrate, true), QueueKey(KindPlan, false)}
}

// Job is one unit of work. Ref is the issue the work is for.
type Job struct {
	Kind       string `json:"kind"`
	Ref        string `json:"ref"`
	PR         int    `json:"pr,omitempty"`
	Mode       string `json:"mode,omitempty"`        // implement: "new" | "fix"
	Reason     string `json:"reason,omitempty"`      // why a fix was asked for
	RateLimitN int    `json:"ratelimit_n,omitempty"` // usage limits hit in a row
	// FailN counts reviews that failed for another reason, apart from
	// RateLimitN: a usage limit says nothing about whether the next try works.
	FailN int `json:"fail_n,omitempty"`
	// Pin is what the issue said when devbot accepted devbot/ready and
	// queued this job (core.PinOf), for the job to check and record.
	Pin string `json:"pin,omitempty"`
	// Base is the base branch devbot accepted the task onto, for the job
	// to record when the task has none of its own yet (core.StartBase).
	Base string `json:"base,omitempty"`
	// Docker: the repo asks for a Docker daemon (.devbot.yaml docker), or
	// its base branch showed it needs one, so the job waits on its kind's
	// Docker list. DockerWhy is what showed it (config.DockerSignal), for
	// the run to say.
	Docker    bool   `json:"docker,omitempty"`
	DockerWhy string `json:"docker_why,omitempty"`
	// Heads, on an integrate job, are the heads it checks, each a child
	// task's PR head as core.Head writes it ("<task>@<sha>"), sorted.
	Heads      []string  `json:"heads,omitempty"`
	EnqueuedAt time.Time `json:"enqueued_at"`
}

func (j Job) dedupe() string { return j.Kind + "|" + j.Ref }

type Store struct {
	R *redis.Client

	// described is what Describe last recorded for each lease this process
	// holds, for Renew to put back if the lease lapses and takes it along.
	mu        sync.Mutex
	described map[string][]byte
}

func New(addr string, db int) *Store {
	return Open(Options{Addr: addr, DB: db})
}

// Options says how to reach Redis: one endpoint, or a master found through
// Sentinel, with ACL credentials and TLS either way.
type Options struct {
	Addr     string
	DB       int
	Username string
	Password string
	// TLS, when set, is used for every connection, Sentinel's included.
	TLS *tls.Config

	SentinelMaster   string
	SentinelAddrs    []string
	SentinelUsername string
	SentinelPassword string
}

// Open connects to Redis as o says. Redis Cluster isn't supported: devbot's
// scripts touch several keys at once, which Cluster only allows within one
// hash slot.
func Open(o Options) *Store {
	if o.SentinelMaster != "" {
		return &Store{R: redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName: o.SentinelMaster, SentinelAddrs: o.SentinelAddrs,
			SentinelUsername: o.SentinelUsername, SentinelPassword: o.SentinelPassword,
			Username: o.Username, Password: o.Password, DB: o.DB, TLSConfig: o.TLS,
		})}
	}
	return &Store{R: redis.NewClient(&redis.Options{Addr: o.Addr, DB: o.DB, Username: o.Username, Password: o.Password, TLSConfig: o.TLS})}
}

// Enqueue adds a job unless one of the same kind is already queued or
// delayed for that ref. It reports whether it added one.
func (s *Store) Enqueue(ctx context.Context, j Job) (bool, error) {
	if j.EnqueuedAt.IsZero() {
		j.EnqueuedAt = time.Now().UTC()
	}
	b, err := json.Marshal(j)
	if err != nil {
		return false, err
	}
	added, err := enqueueScript.Run(ctx, s.R, []string{keyQueued, QueueKey(j.Kind, j.Docker)}, j.dedupe(), b).Int()
	return added == 1, err
}

var enqueueScript = redis.NewScript(`
if redis.call('SADD', KEYS[1], ARGV[1]) == 1 then
  redis.call('LPUSH', KEYS[2], ARGV[2])
  return 1
end
return 0`)

// Pop takes the oldest job of a kind off its Docker list or the other, or
// nil when that is empty.
func (s *Store) Pop(ctx context.Context, kind string, docker bool) (*Job, error) {
	b, err := popScript.Run(ctx, s.R, []string{QueueKey(kind, docker), keyQueued}).Text()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var j Job
	if err := json.Unmarshal([]byte(b), &j); err != nil {
		return nil, fmt.Errorf("bad job %q: %w", b, err)
	}
	return &j, nil
}

// The pop and the dedupe removal are one step: an entry left behind with no
// job would block every later Enqueue for the task. A job too broken to name
// its entry leaves it to PruneQueued.
var popScript = redis.NewScript(`
local b = redis.call('RPOP', KEYS[1])
if not b then return false end
local ok, j = pcall(cjson.decode, b)
if ok and type(j) == 'table' and type(j.kind) == 'string' and type(j.ref) == 'string' then
  redis.call('SREM', KEYS[2], j.kind .. '|' .. j.ref)
end
return b`)

// Claim pops the oldest job of a kind, as Pop does, and takes its lease for
// owner in the same step. Apart, an Enqueue between the two would find
// neither the dedupe entry nor a lease, and queue the work again. A job
// whose lease someone else holds is a duplicate of what they run, and is
// dropped: held reports false with it.
func (s *Store) Claim(ctx context.Context, kind string, docker bool, owner string, ttl time.Duration) (j *Job, held bool, err error) {
	v, err := claimScript.Run(ctx, s.R, []string{QueueKey(kind, docker), keyQueued},
		owner, ttl.Milliseconds(), time.Now().Unix(), prefix+"lease:", prefix+"lease-info:").Slice()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	b, _ := v[0].(string)
	j = &Job{}
	if err := json.Unmarshal([]byte(b), j); err != nil {
		return nil, false, fmt.Errorf("bad job %q: %w", b, err)
	}
	n, _ := v[1].(int64)
	return j, n == 1, nil
}

// The lease keys are leaseKey and leaseInfoKey, built here from the job.
var claimScript = redis.NewScript(`
local b = redis.call('RPOP', KEYS[1])
if not b then return false end
local ok, j = pcall(cjson.decode, b)
if not (ok and type(j) == 'table' and type(j.kind) == 'string' and type(j.ref) == 'string') then
  return {b, 0}
end
redis.call('SREM', KEYS[2], j.kind .. '|' .. j.ref)
if not redis.call('SET', ARGV[4] .. j.kind .. ':' .. j.ref, ARGV[1], 'NX', 'PX', ARGV[2]) then
  return {b, 0}
end
local info = ARGV[5] .. j.kind .. ':' .. j.ref
redis.call('DEL', info)
redis.call('HSET', info, 'started', ARGV[3], 'heartbeat', ARGV[3])
redis.call('PEXPIRE', info, ARGV[2])
return {b, 1}`)

// PruneQueued drops dedupe entries that no queued or delayed job and no live
// lease stand behind, so a task they block can be enqueued again. It reports
// how many it dropped.
func (s *Store) PruneQueued(ctx context.Context) (int, error) {
	keys := append([]string{keyQueued, keyDelayed}, queueKeys()...)
	return pruneScript.Run(ctx, s.R, keys, prefix+"lease:").Int()
}

// One script, so an Enqueue or Pop can't land between reading the jobs and
// dropping an entry.
var pruneScript = redis.NewScript(`
local live = {}
local function mark(b)
  local ok, j = pcall(cjson.decode, b)
  if ok and type(j) == 'table' and type(j.kind) == 'string' and type(j.ref) == 'string' then
    live[j.kind .. '|' .. j.ref] = true
  end
end
for _, b in ipairs(redis.call('ZRANGE', KEYS[2], 0, -1)) do mark(b) end
for i = 3, #KEYS do
  for _, b in ipairs(redis.call('LRANGE', KEYS[i], 0, -1)) do mark(b) end
end
local n = 0
for _, m in ipairs(redis.call('SMEMBERS', KEYS[1])) do
  local kind, ref = string.match(m, '^([^|]*)|(.*)$')
  if not live[m] and not (kind and redis.call('EXISTS', ARGV[1] .. kind .. ':' .. ref) == 1) then
    redis.call('SREM', KEYS[1], m)
    n = n + 1
  end
end
return n`)

// IsQueued reports whether a job of kind for ref is queued or delayed.
func (s *Store) IsQueued(ctx context.Context, kind, ref string) (bool, error) {
	return s.R.SIsMember(ctx, keyQueued, kind+"|"+ref).Result()
}

// Delay parks a job until at. It stays deduplicated while it waits. Like
// Enqueue, it adds nothing when a job of the kind is already queued or
// delayed for the ref, nor when anyone but owner holds its lease: that is a
// duplicate, which would run the work twice. owner is the caller's own lease
// on the job it puts back, or "" for none. It reports whether it parked it.
func (s *Store) Delay(ctx context.Context, j Job, at time.Time, owner string) (bool, error) {
	b, err := json.Marshal(j)
	if err != nil {
		return false, err
	}
	added, err := delayScript.Run(ctx, s.R, []string{keyQueued, keyDelayed, leaseKey(j.Kind, j.Ref)}, j.dedupe(), b, at.Unix(), owner).Int()
	return added == 1, err
}

var delayScript = redis.NewScript(`
if redis.call('SISMEMBER', KEYS[1], ARGV[1]) == 1 then return 0 end
local l = redis.call('GET', KEYS[3])
if l and l ~= ARGV[4] then return 0 end
redis.call('SADD', KEYS[1], ARGV[1])
redis.call('ZADD', KEYS[2], ARGV[3], ARGV[2])
return 1`)

// ReleaseDue moves every delayed job whose time has come back onto its queue.
func (s *Store) ReleaseDue(ctx context.Context) (int, error) {
	due, err := s.R.ZRangeArgs(ctx, redis.ZRangeArgs{Key: keyDelayed, ByScore: true, Start: "-inf", Stop: strconv.FormatInt(time.Now().Unix(), 10)}).Result()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range due {
		var j Job
		if json.Unmarshal([]byte(m), &j) != nil {
			if err := s.R.ZRem(ctx, keyDelayed, m).Err(); err != nil {
				return n, err
			}
			continue
		}
		moved, err := releaseScript.Run(ctx, s.R, []string{keyDelayed, QueueKey(j.Kind, j.Docker)}, m).Int()
		if err != nil {
			return n, err
		}
		n += moved
	}
	return n, nil
}

var releaseScript = redis.NewScript(`
if redis.call('ZREM', KEYS[1], ARGV[1]) == 1 then
  redis.call('LPUSH', KEYS[2], ARGV[1])
  return 1
end
return 0`)

// Undock moves every job on a Docker list to its kind's plain list, and
// clears Docker on those and on every delayed job, for an install that runs
// no Docker daemon. It reports how many jobs it changed.
func (s *Store) Undock(ctx context.Context) (int, error) {
	keys := []string{keyDelayed}
	for _, k := range []string{KindImplement, KindReview, KindIntegrate} {
		keys = append(keys, QueueKey(k, true), QueueKey(k, false))
	}
	return undockScript.Run(ctx, s.R, keys).Int()
}

// One script, so a Pop or ReleaseDue never sees a job half moved. The moved
// jobs have waited longest, so they go on the end Pop takes from, the
// oldest last in.
var undockScript = redis.NewScript(`
local function undock(b)
  local ok, j = pcall(cjson.decode, b)
  if ok and type(j) == 'table' and j.docker == true then
    j.docker = nil
    j.docker_why = nil
    return cjson.encode(j)
  end
  return b
end
local n = 0
local delayed = redis.call('ZRANGE', KEYS[1], 0, -1, 'WITHSCORES')
for i = 1, #delayed, 2 do
  local b = undock(delayed[i])
  if b ~= delayed[i] then
    redis.call('ZREM', KEYS[1], delayed[i])
    redis.call('ZADD', KEYS[1], delayed[i + 1], b)
    n = n + 1
  end
end
for i = 2, #KEYS, 2 do
  for _, b in ipairs(redis.call('LRANGE', KEYS[i], 0, -1)) do
    redis.call('RPUSH', KEYS[i + 1], undock(b))
    n = n + 1
  end
  redis.call('DEL', KEYS[i])
end
return n`)

// ---- leases ----

func leaseKey(kind, ref string) string { return prefix + "lease:" + kind + ":" + ref }

// leaseInfoKey is a hash beside each lease, for people watching: when it was
// taken, its last heartbeat, and what the job is (Describe). It lives and
// dies with the lease. Its prefix doesn't match lease:*, so Leases doesn't
// count it.
func leaseInfoKey(kind, ref string) string { return prefix + "lease-info:" + kind + ":" + ref }

// Acquire takes the lease for kind+ref if nobody holds it.
func (s *Store) Acquire(ctx context.Context, kind, ref, owner string, ttl time.Duration) (bool, error) {
	n, err := acquireScript.Run(ctx, s.R, []string{leaseKey(kind, ref), leaseInfoKey(kind, ref)}, owner, ttl.Milliseconds(), time.Now().Unix()).Int()
	return n == 1, err
}

var acquireScript = redis.NewScript(`
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[2]) then
  redis.call('DEL', KEYS[2])
  redis.call('HSET', KEYS[2], 'started', ARGV[3], 'heartbeat', ARGV[3])
  redis.call('PEXPIRE', KEYS[2], ARGV[2])
  return 1
end
return 0`)

// Renew extends a lease the owner holds, or re-takes one that vanished (Redis
// restarted under a running job, or was out of reach for longer than ttl).
// It fails only if someone else holds it. The lease's detail is put back as
// Describe last had it here, so a re-taken lease still names the job's pod
// and services: serve deletes service pods no lease asks for.
func (s *Store) Renew(ctx context.Context, kind, ref, owner string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	d := s.described[describedKey(kind, ref, owner)]
	s.mu.Unlock()
	n, err := renewScript.Run(ctx, s.R, []string{leaseKey(kind, ref), leaseInfoKey(kind, ref)}, owner, ttl.Milliseconds(), time.Now().Unix(), d).Int()
	return n == 1, err
}

var renewScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v == false or v == ARGV[1] then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
  redis.call('HSET', KEYS[2], 'heartbeat', ARGV[3])
  redis.call('HSETNX', KEYS[2], 'started', ARGV[3])
  if ARGV[4] ~= '' then redis.call('HSET', KEYS[2], 'detail', ARGV[4]) end
  redis.call('PEXPIRE', KEYS[2], ARGV[2])
  return 1
end
return 0`)

func describedKey(kind, ref, owner string) string { return kind + "|" + ref + "|" + owner }

// Release drops a lease the owner holds.
func (s *Store) Release(ctx context.Context, kind, ref, owner string) error {
	s.mu.Lock()
	delete(s.described, describedKey(kind, ref, owner))
	s.mu.Unlock()
	return releaseLease.Run(ctx, s.R, []string{leaseKey(kind, ref), leaseInfoKey(kind, ref)}, owner).Err()
}

var releaseLease = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  if KEYS[2] then redis.call('DEL', KEYS[2]) end
  return redis.call('DEL', KEYS[1])
end
return 0`)

// LeaseDetail is what a running job is doing, as far as its holder knows
// once it has read the task: attempt N of M for an implementer, the review
// round for a reviewer.
type LeaseDetail struct {
	PR          int    `json:"pr,omitempty"`
	Mode        string `json:"mode,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Attempt     int    `json:"attempt,omitempty"`
	MaxAttempts int    `json:"max_attempts,omitempty"`
	Round       int    `json:"round,omitempty"`
	MaxRounds   int    `json:"max_rounds,omitempty"`
	// Pod is the job's pod, and PodLabels and PodAnnotations what serve
	// puts on it (pod_labels): KEDA starts the pod before it knows the job.
	Pod            string            `json:"pod,omitempty"`
	PodLabels      map[string]string `json:"pod_labels,omitempty"`
	PodAnnotations map[string]string `json:"pod_annotations,omitempty"`
	// Services are the service containers the job asked for, which serve
	// starts in a pod of their own beside Pod.
	Services []config.Service `json:"services,omitempty"`
}

// Describe records d for a lease the owner holds; a lease that changed hands
// meanwhile is left alone. It is kept here too, for Renew to put back.
func (s *Store) Describe(ctx context.Context, kind, ref, owner string, d LeaseDetail) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.described == nil {
		s.described = map[string][]byte{}
	}
	s.described[describedKey(kind, ref, owner)] = b
	s.mu.Unlock()
	return describeScript.Run(ctx, s.R, []string{leaseKey(kind, ref), leaseInfoKey(kind, ref)}, owner, b).Err()
}

var describeScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('HSET', KEYS[2], 'detail', ARGV[2])
  redis.call('PEXPIRE', KEYS[2], redis.call('PTTL', KEYS[1]))
  return 1
end
return 0`)

// Lease is one running job: who holds it, since when, its last heartbeat.
type Lease struct {
	Kind      string    `json:"kind"`
	Ref       string    `json:"ref"`
	Holder    string    `json:"holder"`
	Started   time.Time `json:"started,omitzero"`
	Heartbeat time.Time `json:"heartbeat,omitzero"`
	Expires   time.Time `json:"expires,omitzero"`
	LeaseDetail
}

// Running lists every live lease, oldest first.
func (s *Store) Running(ctx context.Context) ([]Lease, error) {
	var out []Lease
	iter := s.R.Scan(ctx, 0, prefix+"lease:*", 200).Iterator()
	for iter.Next(ctx) {
		kind, ref, ok := strings.Cut(strings.TrimPrefix(iter.Val(), prefix+"lease:"), ":")
		if !ok {
			continue
		}
		l := Lease{Kind: kind, Ref: ref}
		var err error
		if l.Holder, err = s.Holder(ctx, kind, ref); err != nil {
			return nil, err
		} else if l.Holder == "" {
			continue // released since the scan saw it
		}
		info, err := s.R.HGetAll(ctx, leaseInfoKey(kind, ref)).Result()
		if err != nil {
			return nil, err
		}
		l.Started, l.Heartbeat = unixTime(info["started"]), unixTime(info["heartbeat"])
		if d := info["detail"]; d != "" {
			json.Unmarshal([]byte(d), &l.LeaseDetail)
		}
		ttl, err := s.R.PTTL(ctx, leaseKey(kind, ref)).Result()
		if err != nil {
			return nil, err
		}
		if ttl > 0 {
			l.Expires = time.Now().Add(ttl).Truncate(time.Second)
		}
		out = append(out, l)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out, iter.Err()
}

func unixTime(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

// Holder returns who holds a lease, or "".
func (s *Store) Holder(ctx context.Context, kind, ref string) (string, error) {
	v, err := s.R.Get(ctx, leaseKey(kind, ref)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

// ---- merge mutex ----

// LockMerge serialises merges per repo, across every serve replica and job
// pod, so two approved PRs can't both merge against the same base. owner
// must be unique to the caller: only it can unlock.
func (s *Store) LockMerge(ctx context.Context, repo, owner string) (bool, error) {
	return s.R.SetNX(ctx, prefix+"merge:"+repo, owner, 2*time.Minute).Result()
}

func (s *Store) UnlockMerge(ctx context.Context, repo, owner string) error {
	return releaseLease.Run(ctx, s.R, []string{prefix + "merge:" + repo}, owner).Err()
}

// ---- merge retries ----

// MergeRetry is how many merges of a task's head in a row failed for a
// reason that may pass (a timeout, a 5xx, a rate limit), and when to try the
// next. Lost with Redis, it only means the count starts over.
type MergeRetry struct {
	Head string    `json:"head"`
	N    int       `json:"n"`
	Next time.Time `json:"next"`
}

func mergeRetryKey(ref string) string { return prefix + "merge-retry:" + ref }

// MergeRetry returns the task's retry state, zero when there is none.
func (s *Store) MergeRetry(ctx context.Context, ref string) (MergeRetry, error) {
	var m MergeRetry
	b, err := s.R.Get(ctx, mergeRetryKey(ref)).Bytes()
	if errors.Is(err, redis.Nil) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

// SetMergeRetry records the task's retry state, kept for a day past the
// next try.
func (s *Store) SetMergeRetry(ctx context.Context, ref string, m MergeRetry) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.R.Set(ctx, mergeRetryKey(ref), b, time.Until(m.Next)+24*time.Hour).Err()
}

// ClearMergeRetry forgets the task's retry state.
func (s *Store) ClearMergeRetry(ctx context.Context, ref string) error {
	return s.R.Del(ctx, mergeRetryKey(ref)).Err()
}

// ---- checks pending ----

func pendingKey(ref string, pr int, sha string) string {
	return prefix + "checks-pending:" + ref + "|" + strconv.Itoa(pr) + "|" + sha
}

func awaitedKey(ref string, pr int, sha string) string {
	return prefix + "reviews-awaited:" + ref + "|" + strconv.Itoa(pr) + "|" + sha
}

// PendingSince returns when a PR head's checks were first seen pending,
// recording now when nobody has yet. It is per head, so a push by anyone
// starts the wait over, and it outlives ttl, the longest wait, only by a
// margin: lost with Redis, the wait only starts over.
func (s *Store) PendingSince(ctx context.Context, ref string, pr int, sha string, now time.Time, ttl time.Duration) (time.Time, error) {
	return s.firstSeen(ctx, pendingKey(ref, pr, sha), now, ttl)
}

// AwaitedSince is PendingSince for a PR head's required reviews: when
// devbot first found that head waiting for one.
func (s *Store) AwaitedSince(ctx context.Context, ref string, pr int, sha string, now time.Time, ttl time.Duration) (time.Time, error) {
	return s.firstSeen(ctx, awaitedKey(ref, pr, sha), now, ttl)
}

func (s *Store) firstSeen(ctx context.Context, k string, now time.Time, ttl time.Duration) (time.Time, error) {
	if err := s.R.SetNX(ctx, k, now.Unix(), ttl+24*time.Hour).Err(); err != nil {
		return time.Time{}, err
	}
	v, err := s.R.Get(ctx, k).Result()
	if errors.Is(err, redis.Nil) {
		return now, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return unixTime(v), nil
}

// ---- rebase queue ----

// Approved PRs that are behind their base and wait for automerge queue per
// base ("repo|branch"), oldest approval first, and take turns: one turn is
// a rebase, a re-review and the merge. The turn is held until its holder
// leaves the queue, so it needs no TTL; whoever finds a holder gone from
// the forge makes it leave. Everything here can be rebuilt from the forge.

func rebaseQKey(base string) string    { return prefix + "rebaseq:" + base }
func rebaseTurnKey(base string) string { return prefix + "rebaseq-turn:" + base }

// JoinRebaseQueue adds ref to base's queue at its approval time, keeping
// its place if it is already queued.
func (s *Store) JoinRebaseQueue(ctx context.Context, base, ref string, approved time.Time) error {
	return s.R.ZAddNX(ctx, rebaseQKey(base), redis.Z{Score: float64(approved.UnixMicro()), Member: ref}).Err()
}

// LeaveRebaseQueue takes ref out of base's queue, and gives up its turn.
func (s *Store) LeaveRebaseQueue(ctx context.Context, base, ref string) error {
	if err := s.R.ZRem(ctx, rebaseQKey(base), ref).Err(); err != nil {
		return err
	}
	return releaseLease.Run(ctx, s.R, []string{rebaseTurnKey(base)}, ref).Err()
}

// RebaseQueue lists base's queue in turn order.
func (s *Store) RebaseQueue(ctx context.Context, base string) ([]string, error) {
	return s.R.ZRange(ctx, rebaseQKey(base), 0, -1).Result()
}

// RebaseTurn gives base's turn to ref when nobody holds it and, if ifFirst,
// ref is first in the queue; a rebase found already in flight claims it
// without. It returns who holds the turn afterwards, "" when nobody does.
func (s *Store) RebaseTurn(ctx context.Context, base, ref string, ifFirst bool) (string, error) {
	f := "0"
	if ifFirst {
		f = "1"
	}
	return rebaseTurn.Run(ctx, s.R, []string{rebaseTurnKey(base), rebaseQKey(base)}, ref, f).Text()
}

// RebaseTurnHolder is who holds base's turn, or "".
func (s *Store) RebaseTurnHolder(ctx context.Context, base string) (string, error) {
	v, err := s.R.Get(ctx, rebaseTurnKey(base)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

var rebaseTurn = redis.NewScript(`
local h = redis.call('GET', KEYS[1])
if h then return h end
if ARGV[2] == '1' then
  local q = redis.call('ZRANGE', KEYS[2], 0, 0)
  if q[1] ~= ARGV[1] then return '' end
end
redis.call('SET', KEYS[1], ARGV[1])
return ARGV[1]`)

// ---- PRs behind their base ----

func behindKey(repo string) string { return prefix + "behind:" + repo }

// MarkBehind records whether an approved PR's task is waiting behind its
// base, for the gauge. An entry nobody refreshes ages out, so a task that
// ends without saying so stops counting.
func (s *Store) MarkBehind(ctx context.Context, repo, ref string, behind bool) error {
	if !behind {
		return s.R.ZRem(ctx, behindKey(repo), ref).Err()
	}
	return s.R.ZAdd(ctx, behindKey(repo), redis.Z{Score: float64(time.Now().Unix()), Member: ref}).Err()
}

// Behind counts, by repo, the tasks marked behind since since, dropping
// older entries.
func (s *Store) Behind(ctx context.Context, since time.Time) (map[string]int64, error) {
	out := map[string]int64{}
	iter := s.R.Scan(ctx, 0, prefix+"behind:*", 200).Iterator()
	for iter.Next(ctx) {
		k := iter.Val()
		cut := strconv.FormatInt(since.Unix(), 10)
		if err := s.R.ZRemRangeByScore(ctx, k, "-inf", "("+cut).Err(); err != nil {
			return nil, err
		}
		n, err := s.R.ZCard(ctx, k).Result()
		if err != nil {
			return nil, err
		}
		if n > 0 {
			out[strings.TrimPrefix(k, prefix+"behind:")] = n
		}
	}
	return out, iter.Err()
}

// ---- waits on a task of another repo ----

// waitersTTL is how long a waiter stays indexed after the last evaluation
// that found it waiting. Every reconcile pass finds every wait again, so
// only one nobody holds any more ages out.
const waitersTTL = 48 * time.Hour

func waitersKey(on string) string { return prefix + "waiters:" + on }

// AddWaiter records that task waiter waits on task on, of another repo,
// whose merge or close then finds it (TakeWaiters) without searching every
// repo; or, with on "tags:" and a repo, for a tag of that repo, which its
// push finds. The wait itself is on the forge; this only makes its release
// prompt.
func (s *Store) AddWaiter(ctx context.Context, on, waiter string) error {
	_, err := s.R.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.SAdd(ctx, waitersKey(on), waiter)
		p.Expire(ctx, waitersKey(on), waitersTTL)
		return nil
	})
	return err
}

// TakeWaiters returns the tasks AddWaiter recorded as waiting on on, and
// forgets them: on has merged or closed, so none waits on it any more once
// evaluated.
func (s *Store) TakeWaiters(ctx context.Context, on string) ([]string, error) {
	var members *redis.StringSliceCmd
	_, err := s.R.TxPipelined(ctx, func(p redis.Pipeliner) error {
		members = p.SMembers(ctx, waitersKey(on))
		p.Del(ctx, waitersKey(on))
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := members.Val()
	sort.Strings(out)
	return out, nil
}

// ---- evaluation lock ----

func evalKey(ref string) string  { return prefix + "eval:" + ref }
func dirtyKey(ref string) string { return prefix + "eval-dirty:" + ref }

// LockEval takes the per-task evaluation lock. When someone else holds it,
// it marks the task dirty in the same step, so the holder's UnlockEval sees
// the request and nothing falls between a failed take and a release.
func (s *Store) LockEval(ctx context.Context, ref, owner string, ttl time.Duration) (bool, error) {
	n, err := lockEvalScript.Run(ctx, s.R, []string{evalKey(ref), dirtyKey(ref)}, owner, ttl.Milliseconds()).Int()
	return n == 1, err
}

// Taking the lock clears the dirty mark: the evaluation that follows reads
// the forge after whatever asked for it.
var lockEvalScript = redis.NewScript(`
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[2]) then
  redis.call('DEL', KEYS[2])
  return 1
end
redis.call('SET', KEYS[2], 1, 'PX', ARGV[2])
return 0`)

// UnlockEval releases the lock if owner still holds it, and reports whether
// anyone asked for an evaluation meanwhile. An owner whose lock expired and
// was taken by another leaves both alone: the new holder covers it.
func (s *Store) UnlockEval(ctx context.Context, ref, owner string) (dirty bool, err error) {
	n, err := unlockEvalScript.Run(ctx, s.R, []string{evalKey(ref), dirtyKey(ref)}, owner).Int()
	return n == 1, err
}

var unlockEvalScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v == ARGV[1] then
  redis.call('DEL', KEYS[1])
elseif v then
  return 0
end
return redis.call('DEL', KEYS[2])`)

// ---- evaluation requests ----

const keyEvalRequests = prefix + "eval-requests"

// RequestEval asks serve to evaluate a task: a job pod holds one bot's
// credential, and an evaluation may act as either. A request lost with
// Redis is made up for by the next reconcile pass.
func (s *Store) RequestEval(ctx context.Context, ref string) error {
	return s.R.SAdd(ctx, keyEvalRequests, ref).Err()
}

// keyEvalRetries holds requested evaluations that failed, by when to try
// them again.
const keyEvalRetries = prefix + "eval-retries"

// RetryEval asks again, at at, for an evaluation that was requested and
// failed: taken off the set, it would otherwise wait for reconcile. An
// earlier retry already set for ref stands.
func (s *Store) RetryEval(ctx context.Context, ref string, at time.Time) error {
	return s.R.ZAddNX(ctx, keyEvalRetries, redis.Z{Score: float64(at.Unix()), Member: ref}).Err()
}

// TakeEvalRequests removes and returns up to n requested evaluations, each
// task once however often it was asked for, with the retries that are due.
func (s *Store) TakeEvalRequests(ctx context.Context, n int) ([]string, error) {
	if err := dueEvalScript.Run(ctx, s.R, []string{keyEvalRetries, keyEvalRequests}, time.Now().Unix()).Err(); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	refs, err := s.R.SPopN(ctx, keyEvalRequests, int64(n)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return refs, err
}

// One script, so a retry taken off by one replica is in the set for all.
var dueEvalScript = redis.NewScript(`
local due = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
for _, r in ipairs(due) do
  redis.call('SADD', KEYS[2], r)
  redis.call('ZREM', KEYS[1], r)
end
return #due`)

// ---- bot logins ----

func botsKey(provider string) string { return prefix + "bots:" + provider }

// SetBots records a provider's bot logins, as serve read them from the
// forge, for job pods that can't: a GitHub App's login takes its key to
// read, and a pod holds only its own role's.
func (s *Store) SetBots(ctx context.Context, provider, implementer, reviewer string) error {
	return s.R.Set(ctx, botsKey(provider), implementer+"\n"+reviewer, 0).Err()
}

// Bots is what SetBots last recorded, or "" for both when nothing is.
func (s *Store) Bots(ctx context.Context, provider string) (implementer, reviewer string, err error) {
	v, err := s.R.Get(ctx, botsKey(provider)).Result()
	if errors.Is(err, redis.Nil) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	implementer, reviewer, _ = strings.Cut(v, "\n")
	return implementer, reviewer, nil
}

// ---- rate-limit hold ----

// Hold returns until when the fleet is holding off Claude, or zero.
func (s *Store) Hold(ctx context.Context) (time.Time, error) {
	v, err := s.R.Get(ctx, keyHold).Int64()
	if errors.Is(err, redis.Nil) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(v, 0), nil
}

// SetHold extends the hold to until (never shortens it).
func (s *Store) SetHold(ctx context.Context, until time.Time) error {
	ttl := time.Until(until)
	if ttl <= 0 {
		return nil
	}
	return setHoldScript.Run(ctx, s.R, []string{keyHold}, until.Unix(), ttl.Milliseconds()).Err()
}

var setHoldScript = redis.NewScript(`
local cur = tonumber(redis.call('GET', KEYS[1]) or '0')
if tonumber(ARGV[1]) > cur then redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2]) end
return 1`)

// ---- webhook dedupe ----

// FirstDelivery reports whether a webhook delivery ID is new.
func (s *Store) FirstDelivery(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return true, nil
	}
	return s.R.SetNX(ctx, prefix+"delivery:"+id, 1, 24*time.Hour).Result()
}

// ForgetDelivery unmarks a delivery ID, so the delivery counts as new again.
func (s *Store) ForgetDelivery(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	return s.R.Del(ctx, prefix+"delivery:"+id).Err()
}

// ---- tracked PRs ----

// trackTTL is how long a PR stays tracked after the last evaluation that
// found it. Every reconcile pass finds every task's PR again, so only a PR
// no task has any more ages out.
const trackTTL = 24 * time.Hour

func trackedPRKey(repo string, number int) string {
	return prefix + "tracked:pr:" + repo + "#" + strconv.Itoa(number)
}

func trackedHeadKey(repo, sha string) string { return prefix + "tracked:head:" + repo + "@" + sha }

// TrackPR records PR number of repo, at head sha, as a task's, for webhooks
// to tell what concerns devbot without asking the forge.
func (s *Store) TrackPR(ctx context.Context, repo string, number int, sha string) error {
	_, err := s.R.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, trackedPRKey(repo, number), sha, trackTTL)
		if sha != "" {
			p.Set(ctx, trackedHeadKey(repo, sha), number, trackTTL)
		}
		return nil
	})
	return err
}

// TrackedPR reports whether TrackPR recorded PR number of repo.
func (s *Store) TrackedPR(ctx context.Context, repo string, number int) (bool, error) {
	n, err := s.R.Exists(ctx, trackedPRKey(repo, number)).Result()
	return n > 0, err
}

// TrackedHead is the tracked PR of repo whose head TrackPR last recorded as
// sha, or 0.
func (s *Store) TrackedHead(ctx context.Context, repo, sha string) (int, error) {
	n, err := s.R.Get(ctx, trackedHeadKey(repo, sha)).Int()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return n, err
}

// ---- branch sweep ----

// SweepDue reports whether provider's branches are due a sweep, and if so
// marks them swept for every.
func (s *Store) SweepDue(ctx context.Context, provider string, every time.Duration) (bool, error) {
	return s.R.SetNX(ctx, prefix+"branch-sweep:"+provider, 1, every).Result()
}

// ---- event feed ----

// Event is one state transition, as clients see it.
type Event struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Time      time.Time `json:"time"`
	Task      string    `json:"task"`
	PR        string    `json:"pr,omitempty"`
	State     string    `json:"state"`
	Origin    string    `json:"origin,omitempty"`
	Requester string    `json:"requester,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	URL       string    `json:"url,omitempty"`
	// Base is the branch the task's work goes into, when it isn't the
	// repo's default branch.
	Base string `json:"base,omitempty"`
	// Parent is the parent task of a child task, by its full ref.
	Parent string `json:"parent,omitempty"`
}

// Emit appends to the feed and trims anything older than retention.
func (s *Store) Emit(ctx context.Context, e Event, retention time.Duration) (string, error) {
	id, _, err := s.EmitOnce(ctx, e, "", retention)
	return id, err
}

// EmitOnce is Emit at most once per once key in retention ("" = every
// time); it reports whether it appended. The key is set in the same script
// as the append, so an append that fails leaves it free for the next try.
func (s *Store) EmitOnce(ctx context.Context, e Event, once string, retention time.Duration) (string, bool, error) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return "", false, err
	}
	keys := []string{keyEvents, keyStats}
	if once != "" {
		keys = append(keys, prefix+"once:"+once)
	}
	minID := strconv.FormatInt(time.Now().Add(-retention).UnixMilli(), 10)
	id, err := emitScript.Run(ctx, s.R, keys, minID, b, retention.Milliseconds(), "events|"+e.Type).Text()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	return id, err == nil, err
}

var emitScript = redis.NewScript(`
if #KEYS == 3 and redis.call('EXISTS', KEYS[3]) == 1 then return false end
local id = redis.call('XADD', KEYS[1], 'MINID', '~', ARGV[1], '*', 'e', ARGV[2])
if #KEYS == 3 then redis.call('SET', KEYS[3], 1, 'PX', ARGV[3]) end
redis.call('HINCRBY', KEYS[2], ARGV[4], 1)
return id`)

// Events returns up to limit events after cursor ("" = from the start).
// The returned cursor is the last ID seen; pass it back to continue.
func (s *Store) Events(ctx context.Context, cursor string, limit int64) ([]Event, string, error) {
	start := "-"
	if cursor != "" {
		start = "(" + cursor
	}
	msgs, err := s.R.XRangeN(ctx, keyEvents, start, "+", limit).Result()
	if err != nil {
		return nil, cursor, err
	}
	var out []Event
	for _, m := range msgs {
		raw, _ := m.Values["e"].(string)
		var e Event
		if json.Unmarshal([]byte(raw), &e) != nil {
			continue
		}
		e.ID = m.ID
		out = append(out, e)
		cursor = m.ID
	}
	return out, cursor, nil
}

// EventsBefore returns up to limit events older than before ("" = the
// newest), newest first, and the ID to pass back for the page after.
func (s *Store) EventsBefore(ctx context.Context, before string, limit int64) ([]Event, string, error) {
	end := "+"
	if before != "" {
		end = "(" + before
	}
	msgs, err := s.R.XRevRangeN(ctx, keyEvents, end, "-", limit).Result()
	if err != nil {
		return nil, before, err
	}
	var out []Event
	for _, m := range msgs {
		raw, _ := m.Values["e"].(string)
		before = m.ID
		var e Event
		if json.Unmarshal([]byte(raw), &e) != nil {
			continue
		}
		e.ID = m.ID
		out = append(out, e)
	}
	return out, before, nil
}

// EventsExpired reports whether cursor points before the oldest retained
// event, i.e. the client may have missed some.
func (s *Store) EventsExpired(ctx context.Context, cursor string) (bool, error) {
	if cursor == "" {
		return false, nil
	}
	first, err := s.R.XRangeN(ctx, keyEvents, "-", "+", 1).Result()
	if err != nil || len(first) == 0 {
		return false, err
	}
	return streamIDLess(cursor, first[0].ID), nil
}

func streamIDLess(a, b string) bool {
	am, as, _ := strings.Cut(a, "-")
	bm, bs, _ := strings.Cut(b, "-")
	ai, _ := strconv.ParseInt(am, 10, 64)
	bi, _ := strconv.ParseInt(bm, 10, 64)
	if ai != bi {
		return ai < bi
	}
	asq, _ := strconv.ParseInt(as, 10, 64)
	bsq, _ := strconv.ParseInt(bs, 10, 64)
	return asq < bsq
}

// Cursor/SetCursor persist an outbound subscriber's position.
func (s *Store) Cursor(ctx context.Context, name string) (string, error) {
	v, err := s.R.Get(ctx, prefix+"sub:"+name).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

// SetCursor moves the cursor only while token is the leader's (see
// TakeLeader), and reports whether it did: a deposed leader that hasn't
// noticed yet can't move it, so it never skips or rewinds the new
// leader's delivery.
func (s *Store) SetCursor(ctx context.Context, name, id, token string) (bool, error) {
	n, err := fencedSetScript.Run(ctx, s.R, []string{keyLeader, prefix + "sub:" + name}, token, id).Int()
	return n == 1, err
}

var fencedSetScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('SET', KEYS[2], ARGV[2])
  return 1
end
return 0`)

// ---- counters ----

// Count adds to a counter that serve exports to Prometheus. Jobs run in
// short-lived pods, so their counters live here rather than in-process.
func (s *Store) Count(ctx context.Context, name string, labels []string, by float64) error {
	return s.R.HIncrByFloat(ctx, keyStats, name+"|"+strings.Join(labels, "|"), by).Err()
}

// Observe records v in a histogram with these upper bounds, as the
// counters a Prometheus histogram is made of: name_bucket|labels|le (one
// for each bound v fits under, 0 for the rest so every bucket exists, and
// +Inf), name_sum|labels and name_count|labels.
func (s *Store) Observe(ctx context.Context, name string, labels []string, bounds []float64, v float64) error {
	l := strings.Join(labels, "|")
	p := s.R.Pipeline()
	for _, b := range bounds {
		n := 0.0
		if v <= b {
			n = 1
		}
		p.HIncrByFloat(ctx, keyStats, name+"_bucket|"+l+"|"+strconv.FormatFloat(b, 'g', -1, 64), n)
	}
	p.HIncrByFloat(ctx, keyStats, name+"_bucket|"+l+"|+Inf", 1)
	p.HIncrByFloat(ctx, keyStats, name+"_sum|"+l, v)
	p.HIncrByFloat(ctx, keyStats, name+"_count|"+l, 1)
	_, err := p.Exec(ctx)
	return err
}

// Stats returns every counter as name|label|label... -> value.
func (s *Store) Stats(ctx context.Context) (map[string]float64, error) {
	m, err := s.R.HGetAll(ctx, keyStats).Result()
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		f, _ := strconv.ParseFloat(v, 64)
		out[k] = f
	}
	return out, nil
}

// Depths returns each kind's queue length, its Docker list's included, and
// the delayed set's size.
func (s *Store) Depths(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	for _, k := range []string{KindImplement, KindReview, KindIntegrate, KindPlan} {
		for _, docker := range []bool{false, true} {
			n, err := s.R.LLen(ctx, QueueKey(k, docker)).Result()
			if err != nil {
				return nil, err
			}
			out[k] += n
		}
	}
	n, err := s.R.ZCard(ctx, keyDelayed).Result()
	if err != nil {
		return nil, err
	}
	out["delayed"] = n
	return out, nil
}

// Leases counts live leases by kind.
func (s *Store) Leases(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	iter := s.R.Scan(ctx, 0, prefix+"lease:*", 200).Iterator()
	for iter.Next(ctx) {
		kind, _, _ := strings.Cut(strings.TrimPrefix(iter.Val(), prefix+"lease:"), ":")
		out[kind]++
	}
	return out, iter.Err()
}

// ---- waiting jobs ----

// Queued lists the jobs waiting on every queue, next to run first.
func (s *Store) Queued(ctx context.Context) ([]Job, error) {
	var out []Job
	for _, k := range queueKeys() {
		raw, err := s.R.LRange(ctx, k, 0, -1).Result()
		if err != nil {
			return nil, err
		}
		// Enqueue pushes on the left and Pop takes from the right.
		for i := len(raw) - 1; i >= 0; i-- {
			var j Job
			if json.Unmarshal([]byte(raw[i]), &j) == nil {
				out = append(out, j)
			}
		}
	}
	return out, nil
}

// DelayedJob is a job parked until Release, by a usage-limit backoff or the
// fleet's rate-limit hold.
type DelayedJob struct {
	Job
	Release time.Time `json:"release_at"`
}

// Delayed lists the parked jobs, soonest released first.
func (s *Store) Delayed(ctx context.Context) ([]DelayedJob, error) {
	zs, err := s.R.ZRangeWithScores(ctx, keyDelayed, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	var out []DelayedJob
	for _, z := range zs {
		m, _ := z.Member.(string)
		var j Job
		if json.Unmarshal([]byte(m), &j) == nil {
			out = append(out, DelayedJob{Job: j, Release: time.Unix(int64(z.Score), 0).UTC()})
		}
	}
	return out, nil
}

// ---- service pods ----

// The states of a job's services.
const (
	ServicesStarting = "starting"
	ServicesReady    = "ready"
	ServicesFailed   = "failed"
)

// ServicesStatus is how a job pod's services are doing, as serve last saw
// them. Failed is final: serve doesn't start them again for the pod.
type ServicesStatus struct {
	State string `json:"state"`
	// IP is the service pod's address, once they are ready.
	IP string `json:"ip,omitempty"`
	// Reason says why they failed, or what they are waiting for.
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

func servicesKey(pod string) string { return prefix + "services:" + pod }

// SetServices records a job pod's services' status, for ttl: serve
// rewrites it while the job runs.
func (s *Store) SetServices(ctx context.Context, pod string, st ServicesStatus, ttl time.Duration) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.R.Set(ctx, servicesKey(pod), b, ttl).Err()
}

// Services reads a job pod's services' status; ok is false when serve has
// recorded none yet.
func (s *Store) Services(ctx context.Context, pod string) (st ServicesStatus, ok bool, err error) {
	b, err := s.R.Get(ctx, servicesKey(pod)).Bytes()
	if errors.Is(err, redis.Nil) {
		return st, false, nil
	}
	if err != nil {
		return st, false, err
	}
	return st, true, json.Unmarshal(b, &st)
}

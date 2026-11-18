package queue

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	keyLeader     = prefix + "leader"
	keyLeaderTerm = prefix + "leader-term"
	keyLastRecon  = prefix + "last-reconcile"
)

// ---- leader lease ----
//
// serve's singleton loops (reconcile, releasing delayed jobs, outbound
// delivery) run on one replica at a time: the holder of devbot:leader. The
// lease's value is its fencing token, "<term>/<owner>", where term counts
// up with every new leader, so no two leaders ever hold the same token,
// even after a Redis wipe resets the count (owners are unique per process).
// Writes only a leader may make check the token in the same script.

// TakeLeader takes the leader lease for ttl if nobody holds it, and returns
// its token, or "" when another replica leads.
func (s *Store) TakeLeader(ctx context.Context, owner string, ttl time.Duration) (string, error) {
	tok, err := takeLeaderScript.Run(ctx, s.R, []string{keyLeader, keyLeaderTerm}, owner, ttl.Milliseconds()).Text()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return tok, err
}

var takeLeaderScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then return false end
local tok = redis.call('INCR', KEYS[2]) .. '/' .. ARGV[1]
redis.call('SET', KEYS[1], tok, 'PX', ARGV[2])
return tok`)

// RenewLeader extends the lease while token holds it. It reports false once
// the lease has expired or passed to another replica: the holder has been
// deposed and must stop its singleton loops.
func (s *Store) RenewLeader(ctx context.Context, token string, ttl time.Duration) (bool, error) {
	n, err := renewLeaderScript.Run(ctx, s.R, []string{keyLeader}, token, ttl.Milliseconds()).Int()
	return n == 1, err
}

// Unlike a job's lease (Renew), a vanished leader lease is not re-taken: a
// follower may already have taken it, and a lease that comes back from
// nothing has no term to fence with.
var renewLeaderScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0`)

// ResignLeader gives the lease up, if token still holds it, so a follower
// takes over without waiting for it to expire.
func (s *Store) ResignLeader(ctx context.Context, token string) error {
	return releaseLease.Run(ctx, s.R, []string{keyLeader}, token).Err()
}

// Leader returns the current leader's token, or "".
func (s *Store) Leader(ctx context.Context) (string, error) {
	v, err := s.R.Get(ctx, keyLeader).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

// SetLastReconcile records when a reconcile pass finished, for every
// replica's /api/status and metrics, whichever one ran it.
func (s *Store) SetLastReconcile(ctx context.Context, at time.Time) error {
	return s.R.Set(ctx, keyLastRecon, at.Unix(), 0).Err()
}

// LastReconcile is when the last reconcile pass finished, or zero.
func (s *Store) LastReconcile(ctx context.Context) (time.Time, error) {
	v, err := s.R.Get(ctx, keyLastRecon).Result()
	if errors.Is(err, redis.Nil) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, nil
	}
	return time.Unix(n, 0), nil
}

// ---- repo config generation ----

func repoConfigKey(repo string) string { return prefix + "repo-config:" + repo }

// BumpRepoConfig marks every process's cached .devbot.yaml for repo stale,
// wherever the push that changed it was delivered.
func (s *Store) BumpRepoConfig(ctx context.Context, repo string) error {
	return s.R.Incr(ctx, repoConfigKey(repo)).Err()
}

// RepoConfigGeneration is repo's generation: a cached config read under
// any other is stale. After a wipe it is "", which differs from whatever
// was cached before.
func (s *Store) RepoConfigGeneration(ctx context.Context, repo string) (string, error) {
	v, err := s.R.Get(ctx, repoConfigKey(repo)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

// ---- forge repo inventory generation ----

func inventoryKey(provider string) string { return prefix + "inventory:" + provider }

// BumpInventory marks every process's list of what provider's App is
// installed on stale, wherever the installation webhook was delivered.
func (s *Store) BumpInventory(ctx context.Context, provider string) error {
	return s.R.Incr(ctx, inventoryKey(provider)).Err()
}

// InventoryGeneration is provider's inventory generation: a list made under
// any other is stale. After a wipe it is "", as for RepoConfigGeneration.
func (s *Store) InventoryGeneration(ctx context.Context, provider string) (string, error) {
	v, err := s.R.Get(ctx, inventoryKey(provider)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

package queue

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// ---- forge rate limits ----

func quotaKey(provider string) string { return prefix + "quota:" + provider }

// quotaTTL is how long a provider's quotas outlive the last answer noted:
// past an hour every primary window has reset.
const quotaTTL = 2 * time.Hour

// NoteQuota records what a forge's answer said of a credential bucket's
// rate limit for resource, for every process holding that bucket to see.
// An answer that crossed a newer one on the way changes nothing: a later
// window replaces an earlier one, and within one window only less left
// does, so no process's note can make a spent limit look unspent.
func (s *Store) NoteQuota(ctx context.Context, provider, bucket, resource string, q forge.Quota) error {
	return noteQuotaScript.Run(ctx, s.R, []string{quotaKey(provider)}, bucket+"|"+resource,
		q.Remaining, q.Limit, q.Reset.Unix(), quotaTTL.Milliseconds()).Err()
}

var noteQuotaScript = redis.NewScript(`
local cur = redis.call('HGET', KEYS[1], ARGV[1])
if cur then
  local r, l, t = string.match(cur, '^(%d+) (%d+) (%d+)$')
  if t then
    t = tonumber(t)
    local nt = tonumber(ARGV[4])
    if nt < t or (nt == t and tonumber(ARGV[2]) >= tonumber(r)) then return 0 end
  end
end
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2] .. ' ' .. ARGV[3] .. ' ' .. ARGV[4])
redis.call('PEXPIRE', KEYS[1], ARGV[5])
return 1`)

// Quotas is what NoteQuota last recorded for bucket's resources, in order:
// a zero Quota for one nothing is known of.
func (s *Store) Quotas(ctx context.Context, provider, bucket string, resources ...string) ([]forge.Quota, error) {
	fields := make([]string, len(resources))
	for i, r := range resources {
		fields[i] = bucket + "|" + r
	}
	vs, err := s.R.HMGet(ctx, quotaKey(provider), fields...).Result()
	if err != nil {
		return nil, err
	}
	out := make([]forge.Quota, len(vs))
	for i, v := range vs {
		if v, ok := v.(string); ok {
			out[i] = parseQuota(v)
		}
	}
	return out, nil
}

// AllQuotas is every quota NoteQuota recorded for provider, by bucket and
// resource, for the metrics.
func (s *Store) AllQuotas(ctx context.Context, provider string) (map[[2]string]forge.Quota, error) {
	m, err := s.R.HGetAll(ctx, quotaKey(provider)).Result()
	if err != nil {
		return nil, err
	}
	out := make(map[[2]string]forge.Quota, len(m))
	for k, v := range m {
		bucket, res, ok := strings.Cut(k, "|")
		if ok {
			out[[2]string{bucket, res}] = parseQuota(v)
		}
	}
	return out, nil
}

func parseQuota(v string) forge.Quota {
	f := strings.Fields(v)
	if len(f) != 3 {
		return forge.Quota{}
	}
	r, _ := strconv.Atoi(f[0])
	l, _ := strconv.Atoi(f[1])
	t, _ := strconv.ParseInt(f[2], 10, 64)
	return forge.Quota{Remaining: r, Limit: l, Reset: time.Unix(t, 0)}
}

// ---- webhook events kept for later ----

const (
	keyLaterDue  = prefix + "later:due"
	keyLaterData = prefix + "later:data"
)

// The outcomes of Later, as devbot_webhook_requeue_total counts them.
const (
	LaterKept      = "requeued"
	LaterCoalesced = "coalesced"
	LaterDropped   = "dropped"
)

// Later keeps a webhook event, payload, whose handling a forge's rate limit
// put off, to be handled again at at (TakeLater). key names what it is
// about: one kept already under key stands for both, with the newer
// payload, at the later time and the higher of their tries. tries counts
// the times it was put off, this one included; past max it is dropped
// instead, and reconcile makes up for it.
func (s *Store) Later(ctx context.Context, key string, payload []byte, at time.Time, tries, max int) (string, error) {
	return laterScript.Run(ctx, s.R, []string{keyLaterDue, keyLaterData}, key, payload, at.Unix(), tries, max).Text()
}

var laterScript = redis.NewScript(`
local n = tonumber(ARGV[4])
local cur = tonumber(redis.call('HGET', KEYS[2], 'n:' .. ARGV[1]) or '0')
if cur > n then n = cur end
local existed = redis.call('ZSCORE', KEYS[1], ARGV[1])
if n > tonumber(ARGV[5]) then
  redis.call('ZREM', KEYS[1], ARGV[1])
  redis.call('HDEL', KEYS[2], 'p:' .. ARGV[1], 'n:' .. ARGV[1])
  return 'dropped'
end
local at = tonumber(ARGV[3])
if existed and tonumber(existed) > at then at = tonumber(existed) end
redis.call('ZADD', KEYS[1], at, ARGV[1])
redis.call('HSET', KEYS[2], 'p:' .. ARGV[1], ARGV[2], 'n:' .. ARGV[1], n)
if existed then return 'coalesced' end
return 'requeued'`)

// Kept is an event Later kept, taken back by TakeLater.
type Kept struct {
	Payload []byte
	Tries   int
}

// TakeLater removes and returns up to n kept events due by now. Taking is
// atomic, so each is handled by one replica.
func (s *Store) TakeLater(ctx context.Context, now time.Time, n int) ([]Kept, error) {
	vs, err := takeLaterScript.Run(ctx, s.R, []string{keyLaterDue, keyLaterData}, now.Unix(), n).StringSlice()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Kept
	for i := 0; i+1 < len(vs); i += 2 {
		tries, _ := strconv.Atoi(vs[i])
		out = append(out, Kept{Payload: []byte(vs[i+1]), Tries: tries})
	}
	return out, nil
}

var takeLaterScript = redis.NewScript(`
local due = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, ARGV[2])
local out = {}
for _, k in ipairs(due) do
  redis.call('ZREM', KEYS[1], k)
  local p = redis.call('HGET', KEYS[2], 'p:' .. k)
  local n = redis.call('HGET', KEYS[2], 'n:' .. k)
  redis.call('HDEL', KEYS[2], 'p:' .. k, 'n:' .. k)
  if p then
    table.insert(out, n or '0')
    table.insert(out, p)
  end
end
return out`)

// LaterCount is how many events Later is keeping.
func (s *Store) LaterCount(ctx context.Context) (int64, error) {
	return s.R.ZCard(ctx, keyLaterDue).Result()
}

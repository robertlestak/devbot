package queue

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// ---- models that can't serve runs ----

// ModelKey names a model as its limits apply: on one harness, and on
// Bedrock in one region, since a limit on Anthropic's API isn't one on
// Bedrock's.
func ModelKey(harness, region, model string) string { return harness + "/" + region + "/" + model }

func modelLimitKey(key string) string { return prefix + "model:limit:" + key }

// LimitModel records that the model key names (ModelKey) can't serve runs
// until until, and why, for every pod to pass it over. It never shortens a
// limit already recorded: a run that gave no reset can't undo one that did.
func (s *Store) LimitModel(ctx context.Context, key string, until time.Time, why string) error {
	ttl := time.Until(until)
	if ttl <= 0 {
		return nil
	}
	return limitModelScript.Run(ctx, s.R, []string{modelLimitKey(key)}, until.Unix(), why, ttl.Milliseconds()).Err()
}

var limitModelScript = redis.NewScript(`
local cur = tonumber(string.match(redis.call('GET', KEYS[1]) or '0', '^%d+') or '0')
if tonumber(ARGV[1]) > cur then redis.call('SET', KEYS[1], ARGV[1] .. '|' .. ARGV[2], 'PX', ARGV[3]) end
return 1`)

// ModelLimit is until when the model key names is limited, and why, or the
// zero time when it isn't.
func (s *Store) ModelLimit(ctx context.Context, key string) (time.Time, string, error) {
	v, err := s.R.Get(ctx, modelLimitKey(key)).Result()
	if errors.Is(err, redis.Nil) {
		return time.Time{}, "", nil
	}
	if err != nil {
		return time.Time{}, "", err
	}
	at, why, _ := strings.Cut(v, "|")
	n, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return time.Time{}, "", nil
	}
	return time.Unix(n, 0), why, nil
}

// ---- the model a task's role last ran on ----

// ModelRun is the model a role of a task last ran on, for task_status.
type ModelRun struct {
	Model string `json:"model"`
	// Fallback says Model is one of the role's fallbacks, which the run
	// moved to from From, for Reason.
	Fallback bool      `json:"fallback,omitempty"`
	From     string    `json:"from,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	At       time.Time `json:"at"`
}

func modelRanKey(ref, role string) string { return prefix + "model:ran:" + ref + ":" + role }

// modelRanTTL outlives any task that is still being worked on; one that
// is idle that long shows its configured models alone.
const modelRanTTL = 30 * 24 * time.Hour

// SetModelRan records the model role of task ref last ran on.
func (s *Store) SetModelRan(ctx context.Context, ref, role string, r ModelRun) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.R.Set(ctx, modelRanKey(ref, role), b, modelRanTTL).Err()
}

// ModelRan is the model role of task ref last ran on, if one is recorded.
func (s *Store) ModelRan(ctx context.Context, ref, role string) (ModelRun, bool, error) {
	var r ModelRun
	b, err := s.R.Get(ctx, modelRanKey(ref, role)).Bytes()
	if errors.Is(err, redis.Nil) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	if json.Unmarshal(b, &r) != nil {
		return r, false, nil
	}
	return r, true, nil
}

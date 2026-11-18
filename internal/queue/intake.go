package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"git.shdw.tech/rob/devbot/internal/intake"
)

// KindIntake is a Slack intake job: a model drafts a task, or asks
// questions, from a Slack request and its thread (package intake). It runs
// in a job pod of its own, with no forge credential and no repo, and
// reaches Redis as a user that can only reach its list and the keys here
// (the chart's devbot-intake): serve writes the request, the pod writes
// back what its model said, and serve, which holds no model credential,
// checks that and acts on it.
//
// Its list holds request IDs, not Jobs: nothing about it is a task's, so it
// takes no part in the dedupe set, leases or the rate-limit hold.
const KindIntake = "intake"

func intakeReqKey(id string) string { return prefix + "intake:req:" + id }
func intakeResKey(id string) string { return prefix + "intake:res:" + id }

// EnqueueIntake queues req for an intake pod. The request expires after
// ttl, taken or not.
func (s *Store) EnqueueIntake(ctx context.Context, req intake.Request, ttl time.Duration) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = s.R.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, intakeReqKey(req.ID), b, ttl)
		p.LPush(ctx, QueueKey(KindIntake, false), req.ID)
		return nil
	})
	return err
}

// TakeIntake takes the oldest intake request, or nil when none waits. One
// that expired before a pod got to it is skipped: its requester was told.
func (s *Store) TakeIntake(ctx context.Context) (*intake.Request, error) {
	for {
		id, err := s.R.RPop(ctx, QueueKey(KindIntake, false)).Result()
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		b, err := s.R.Get(ctx, intakeReqKey(id)).Bytes()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var req intake.Request
		if err := json.Unmarshal(b, &req); err != nil {
			return nil, fmt.Errorf("bad intake request %s: %w", id, err)
		}
		return &req, nil
	}
}

// PutIntakeResult records what the intake job for id came to, for ttl.
func (s *Store) PutIntakeResult(ctx context.Context, id string, r intake.Result, ttl time.Duration) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.R.Set(ctx, intakeResKey(id), b, ttl).Err()
}

// IntakeResult is what the intake job for id came to; ok is false while
// there is nothing yet.
func (s *Store) IntakeResult(ctx context.Context, id string) (r intake.Result, ok bool, err error) {
	b, err := s.R.Get(ctx, intakeResKey(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return intake.Result{Error: "its result couldn't be read"}, true, nil
	}
	return r, true, nil
}

// DropIntake forgets the request and result of id.
func (s *Store) DropIntake(ctx context.Context, id string) error {
	return s.R.Del(ctx, intakeReqKey(id), intakeResKey(id)).Err()
}

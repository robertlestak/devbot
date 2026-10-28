package server

import (
	"context"
	"sync"
	"time"
)

// Every replica serves requests and handles webhooks; the loops that must
// run once per install (reconcile, releasing delayed jobs, labelling job
// pods, outbound delivery) run only on the replica holding the leader lease
// in Redis.
// Running them twice wouldn't corrupt anything (evaluations take their
// task's lock, releases are atomic, and a subscriber's cursor only moves
// under the leader's fencing token), but it would double the forge calls
// and the deliveries.

// campaign tries for the leader lease until ctx ends, and leads whenever
// it holds it.
func (s *Server) campaign(ctx context.Context) {
	ttl := s.Cfg.LeaderLease
	fails := 0
	for {
		token, err := s.E.Q.TakeLeader(ctx, s.owner, ttl)
		// Polled often against the lease, so a follower takes over soon after
		// a leader that died lets it expire. Without Redis no replica can
		// lead, so it backs off, though never past the lease.
		wait := ttl / 10
		if err != nil && ctx.Err() == nil {
			fails++
			wait = backoff(ttl/10, ttl, fails)
			s.Log.Warn("leader election", "err", err, "failures", fails)
		} else {
			fails = 0
		}
		if token != "" {
			s.lead(ctx, token)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// lead runs the singleton loops while token holds the lease. It returns
// once they have stopped: when ctx ends (and it resigns, so a follower
// takes over at once), or when it can no longer be sure it leads.
func (s *Server) lead(ctx context.Context, token string) {
	ttl := s.Cfg.LeaderLease
	every := ttl / 3
	s.Log.Info("leading", "token", token)
	s.leading.Store(true)
	lctx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { s.singletons(lctx, token) })

	t := time.NewTicker(every)
	renewed := time.Now()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-t.C:
		}
		ok, err := s.E.Q.RenewLeader(ctx, token, ttl)
		switch {
		case err == nil && !ok:
			s.Log.Warn("lost the leader lease", "token", token)
			break loop
		case err == nil:
			renewed = time.Now()
		case time.Since(renewed)+every >= ttl:
			// Stop before the lease can expire under us: past that, a
			// follower may lead.
			s.Log.Warn("can't renew the leader lease; stepping down", "token", token, "err", err)
			break loop
		}
	}
	t.Stop()
	stop()
	wg.Wait()
	s.leading.Store(false)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	s.E.Q.ResignLeader(rctx, token)
	s.Log.Info("stopped leading", "token", token)
}

// singletons runs every loop only the leader runs, until ctx ends.
func (s *Server) singletons(ctx context.Context, token string) {
	var wg sync.WaitGroup
	wg.Go(func() { s.reconcileLoop(ctx) })
	wg.Go(func() { s.releaseLoop(ctx) })
	if s.Pods != nil {
		wg.Go(func() { s.podLabelLoop(ctx) })
	}
	if s.ServicePods != nil {
		wg.Go(func() { s.servicesLoop(ctx) })
	}
	if s.JobPods != nil {
		wg.Go(func() { s.agentExitLoop(ctx) })
	}
	for _, sub := range s.Cfg.Subscribers {
		wg.Go(func() { s.deliver(ctx, sub, token) })
	}
	wg.Wait()
}

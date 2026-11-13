package harness

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// overloadRetries is how many more times a run whose model was overloaded
// is tried on that model, OverloadWait apart, before it moves on: an
// overload often passes within seconds.
const overloadRetries = 2

// How long a model that couldn't serve a run is passed over by every run
// whose role has another to go to, when the error gave no reset time of
// its own. Each is held to backoff.max_hold.
const (
	limitedFor     = 15 * time.Minute // a usage or rate limit
	overloadedFor  = 5 * time.Minute  // an overload past its retries
	unavailableFor = time.Hour        // a model the provider doesn't serve this credential
)

// modelOut is a model of a role's chain that couldn't serve the run.
type modelOut struct {
	model string
	class agent.ErrorClass
	until time.Time
	why   string // a line, for the issue and the logs
}

// backsOff says the model is out for a while, not for good: a run waits
// for it, as for a usage limit, once every model is out.
func (o modelOut) backsOff() bool { return o.class != agent.ErrUnavailable }

// run has the agent run the model for attr's role, on a.Model, or on the
// next of a.FallbackModels when a model can't serve it: a usage limit, an
// overload that outlasted overloadRetries, a model the provider doesn't
// serve. A model that couldn't is remembered for every pod (Store.LimitModel)
// until it can again, so runs pass it over meanwhile and go back to it after.
// Before each fallback, carry, when set, keeps the work so far, and the
// fallback starts from it with the same request.
//
// Only once every model is out does the result say so: RateLimited, for the
// job to back off and hold the fleet, when one of them is out for a while,
// with ResetAt the soonest any is back; otherwise the last model's failure.
// Without fallbacks the role runs on its one model, and an overload backs
// off as a usage limit does.
func (r *Runner) run(ctx context.Context, attr core.Attribution, a config.Agent, req agent.RunRequest, carry func(context.Context)) (agent.RunResult, error) {
	region := ""
	if a.AWS != nil {
		region = a.AWS.Region
	}
	key := func(m string) string { return queue.ModelKey(a.Harness, region, m) }
	primary := a.Model
	if len(a.FallbackModels) == 0 {
		res, err := r.runOn(ctx, attr, a, req, nil)
		if err == nil && !res.TimedOut && res.ErrorClass.ModelOut() {
			r.limitModel(ctx, key(primary), r.outOf(primary, res))
			if res.ErrorClass == agent.ErrOverloaded {
				res.RateLimited = true
			}
		}
		r.ranOn(ctx, attr, primary, nil)
		return res, err
	}
	var outs []modelOut
	var res agent.RunResult
	ran, said := false, false
	for _, m := range append([]string{primary}, a.FallbackModels...) {
		if until, why, err := r.E.Q.ModelLimit(ctx, key(m)); err != nil {
			r.Log.Warn("reading whether a model is limited; trying it", "model", m, "err", err)
		} else if time.Now().Before(until) {
			outs = append(outs, modelOut{model: m, class: agent.ErrorClass(why), until: until,
				why: fmt.Sprintf("out until %s", until.UTC().Format(time.RFC3339))})
			continue
		}
		var first *modelOut
		if len(outs) > 0 {
			f := outs[0]
			first = &f
			r.E.Count(context.WithoutCancel(ctx), "agent_fallbacks", []string{a.Harness, attr.Role, outs[len(outs)-1].model, m, string(outs[len(outs)-1].class)}, 1)
			r.Log.Warn("model can't serve the run, falling back", "role", attr.Role, "task", attr.Task, "model", outs[len(outs)-1].model,
				"class", outs[len(outs)-1].class, "reason", outs[len(outs)-1].why, "fallback", m)
			if !said {
				said = true
				ref := attr.Ref()
				if err := r.E.ModelFailover(ctx, ref, attr.Role, first.model, string(first.class), first.why, m); err != nil {
					r.Log.Warn("saying the fallback on the issue", "task", ref, "err", err)
				}
			}
			if ran && carry != nil {
				carry(ctx)
			}
		}
		a.Model = m
		var err error
		res, err = r.runOverloaded(ctx, attr, a, req, first)
		if err != nil || res.TimedOut || !res.ErrorClass.ModelOut() {
			r.ranOn(ctx, attr, m, first)
			return res, err
		}
		ran = true
		o := r.outOf(m, res)
		r.limitModel(ctx, key(m), o)
		outs = append(outs, o)
	}
	// Every model is out.
	var soonest time.Time
	backoff := false
	for _, o := range outs {
		if o.backsOff() {
			if !backoff || o.until.Before(soonest) {
				soonest = o.until
			}
			backoff = true
		}
	}
	var lines []string
	for _, o := range outs {
		lines = append(lines, fmt.Sprintf("%s: %s, %s", o.model, o.class, o.why))
	}
	r.Log.Warn("no model of the role can serve the run", "role", attr.Role, "task", attr.Task, "models", strings.Join(lines, "; "))
	if !ran {
		res = agent.RunResult{IsError: true, ErrorClass: agent.ErrUnavailable,
			Result: "No model of the role can serve the run:\n" + strings.Join(lines, "\n")}
	}
	if backoff {
		res.RateLimited, res.ResetAt = true, soonest
		if !ran {
			res.ErrorClass = agent.ErrRateLimited
		}
	}
	return res, nil
}

// runOverloaded runs a on its model, trying it again overloadRetries times
// while the model is overloaded.
func (r *Runner) runOverloaded(ctx context.Context, attr core.Attribution, a config.Agent, req agent.RunRequest, from *modelOut) (agent.RunResult, error) {
	for try := 0; ; try++ {
		res, err := r.runOn(ctx, attr, a, req, from)
		if err != nil || res.TimedOut || res.ErrorClass != agent.ErrOverloaded || try >= overloadRetries {
			return res, err
		}
		r.Log.Info("model overloaded, trying it again", "role", attr.Role, "model", a.Model, "retry", try+1)
		select {
		case <-ctx.Done():
			return res, nil
		case <-time.After(r.OverloadWait):
		}
	}
}

// outOf is how a run left model out, and until when.
func (r *Runner) outOf(model string, res agent.RunResult) modelOut {
	now := time.Now()
	until := res.ResetAt
	if !until.After(now) {
		switch res.ErrorClass {
		case agent.ErrRateLimited:
			until = now.Add(limitedFor)
		case agent.ErrOverloaded:
			until = now.Add(overloadedFor)
		default:
			until = now.Add(unavailableFor)
		}
	}
	if limit := now.Add(r.E.Cfg.Backoff.MaxHold); r.E.Cfg.Backoff.MaxHold > 0 && until.After(limit) {
		until = limit
	}
	return modelOut{model: model, class: res.ErrorClass, until: until, why: firstLine(cmp.Or(strings.TrimSpace(res.Result), res.StderrTail))}
}

func (r *Runner) limitModel(ctx context.Context, key string, o modelOut) {
	if err := r.E.Q.LimitModel(context.WithoutCancel(ctx), key, o.until, string(o.class)); err != nil {
		r.Log.Warn("recording a limited model", "model", o.model, "err", err)
	}
}

// ranOn records the model attr's role ran on, for task_status: a fallback
// when from, the first model that couldn't serve the run, is set.
func (r *Runner) ranOn(ctx context.Context, attr core.Attribution, model string, from *modelOut) {
	run := queue.ModelRun{Model: model, At: time.Now().UTC()}
	if from != nil {
		run.Fallback, run.From, run.Reason = true, from.model, string(from.class)
	}
	if err := r.E.Q.SetModelRan(context.WithoutCancel(ctx), attr.Ref().String(), attr.Role, run); err != nil {
		r.Log.Warn("recording the model a run ran on", "err", err)
	}
}

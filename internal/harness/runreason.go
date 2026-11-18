package harness

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// runError is a model run the agent didn't see through, said in the one
// line an attempt-failed comment gives it. The agent's and serve's logs have
// the rest.
type runError struct {
	reason string
	err    error
}

func (e *runError) Error() string { return e.reason }
func (e *runError) Unwrap() error { return e.err }

// agentReason says why a run got no result from the agent: the agent
// answered that it failed the run itself, or it stopped answering, and then
// how its container ended (lost), when serve could read that from the pod.
func agentReason(err error, took time.Duration, lost *queue.AgentExit) string {
	if ae := (*agent.AgentError)(nil); errors.As(err, &ae) {
		return fmt.Sprintf("the agent failed the run after %s: %s", roughDuration(took), ae.Msg)
	}
	what := err.Error()
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		what = "EOF"
	} else if ue := (*url.Error)(nil); errors.As(err, &ue) {
		what = ue.Err.Error()
	}
	s := fmt.Sprintf("the agent stopped answering (%s after %s)", what, roughDuration(took))
	if lost != nil {
		s += fmt.Sprintf("; its container ended %s, exit %d", lost.Reason, lost.ExitCode)
		if m := lastLine(lost.Message); m != "" {
			s += ": " + m
		}
	}
	return s
}

// modelReason says why a run whose CLI exited non-zero, or was killed,
// failed: its exit, how long it ran, and the last thing it said.
func modelReason(res agent.RunResult) string {
	took := roughDuration(res.Took())
	if res.OOMSuspected {
		at := ""
		if res.MemoryLimit > 0 {
			at = " at the " + agent.Bytes(res.MemoryLimit) + " limit"
		}
		return fmt.Sprintf("the model run exited %d (OOM-killed%s after %s)", res.ExitCode, at, took)
	}
	s := fmt.Sprintf("the model run exited %d", res.ExitCode)
	if res.Signal != "" {
		s += " (" + res.Signal + ")"
	}
	s += " after " + took
	// Stderr first; the CLI's result or noise when stderr says nothing.
	if l := cmp.Or(lastLine(res.StderrTail), lastLine(res.Result)); l != "" {
		s += ": " + l
	}
	return s
}

// modelFailed is the error for a run that came back failed: the one-line
// reason when its CLI exited non-zero or was killed, else what the CLI
// reported (its result, then stderr). A timeout's kill is the timeout's.
func modelFailed(what string, res agent.RunResult) error {
	if res.Failed() && !res.TimedOut {
		return errors.New(modelReason(res))
	}
	return fmt.Errorf("the %s run failed (%s): %s", what, res.ErrorClass, lastLines(res.Result+"\n"+res.StderrTail, 30))
}

// agentExit asks serve how this pod's agent container ended, and waits up
// to AgentExitWait for it: the kubelet reports a container's end some
// seconds after it. nil when there is no answer, outside a cluster or when
// serve can't read pods.
func (r *Runner) agentExit(ctx context.Context, since time.Time) *queue.AgentExit {
	r.mu.Lock()
	l := r.lease
	r.mu.Unlock()
	if r.Pod == "" || l == nil {
		return nil
	}
	d := l.detail
	d.AgentLost = true
	if err := r.E.Q.Describe(ctx, l.kind, l.ref, r.Owner, d); err != nil {
		r.Log.Warn("asking serve how the agent container ended", "err", err)
		return nil
	}
	deadline := time.Now().Add(r.AgentExitWait)
	for {
		e, ok, err := r.E.Q.AgentExit(ctx, r.Pod)
		if err != nil {
			r.Log.Warn("reading how the agent container ended", "err", err)
			return nil
		}
		// An end from before the run is an earlier one's.
		if ok && !e.FinishedAt.Before(since.Truncate(time.Second)) {
			return &e
		}
		if time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(min(time.Second, r.AgentExitWait)):
		}
	}
}

// roughDuration is d as a person would say it: 45s, 28m, 1h12m.
func roughDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
	}
	d = d.Round(time.Minute)
	if m := int(d.Minutes()) % 60; m != 0 {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), m)
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// lastLine is s's last non-blank line, trimmed and capped.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	l := strings.TrimSpace(lines[len(lines)-1])
	if len(l) > 300 {
		l = strings.ToValidUTF8(l[:300], "") + "..."
	}
	return l
}

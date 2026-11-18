package harness

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/audit/audittest"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// Each way a run can end without a result is one line in the
// attempt-failed comment.
func TestRunReasons(t *testing.T) {
	exit := func(code int, signal string, took time.Duration, oom bool, limit int64, stderr string) agent.RunResult {
		return agent.RunResult{IsError: true, Exit: agent.Exit{ExitCode: code, Signal: signal, DurationS: took.Seconds(), OOMSuspected: oom, MemoryLimit: limit, StderrTail: stderr}}
	}
	eof := &url.Error{Op: "Post", URL: "http://agent/run", Err: io.EOF}
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"exit 1", modelFailed("model", exit(1, "", 47*time.Minute, false, 0, "starting\nError: ENOSPC: no space left on device\n\n")).Error(),
			"the model run exited 1 after 47m: Error: ENOSPC: no space left on device"},
		{"OOM-killed", modelFailed("model", exit(137, "SIGKILL", 28*time.Minute+10*time.Second, true, 8<<30, "loading\n")).Error(),
			"the model run exited 137 (OOM-killed at the 8Gi limit after 28m)"},
		{"OOM-killed, no limit", modelReason(exit(137, "SIGKILL", 90*time.Second, true, 0, "")),
			"the model run exited 137 (OOM-killed after 2m)"},
		{"killed", modelReason(exit(137, "SIGKILL", 75*time.Minute, false, 0, "last words")),
			"the model run exited 137 (SIGKILL) after 1h15m: last words"},
		{"exit 1, no stderr", modelReason(agent.RunResult{Result: "Invalid API key · Please run /login", Exit: agent.Exit{ExitCode: 1, DurationS: 3}}),
			"the model run exited 1 after 3s: Invalid API key · Please run /login"},
		{"reported failure", modelFailed("review", agent.RunResult{IsError: true, ErrorClass: agent.ErrModel, Result: "the model gave up"}).Error(),
			"the review run failed (model_error): the model gave up"},
		{"EOF", agentReason(eof, 28*time.Minute, nil),
			"the agent stopped answering (EOF after 28m)"},
		{"EOF, container ended", agentReason(eof, 28*time.Minute, &queue.AgentExit{Reason: "OOMKilled", ExitCode: 137}),
			"the agent stopped answering (EOF after 28m); its container ended OOMKilled, exit 137"},
		{"refused", agentReason(&url.Error{Op: "Post", URL: "http://agent/run", Err: errors.New("dial unix /run/devbot/agent.sock: connect: connection refused")}, time.Second, nil),
			"the agent stopped answering (dial unix /run/devbot/agent.sock: connect: connection refused after 1s)"},
		{"agent failed", agentReason(&agent.AgentError{Msg: "the agent was stopped during the run (interrupt)"}, 12*time.Minute, nil),
			"the agent failed the run after 12m: the agent was stopped during the run (interrupt)"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, tc.got, tc.want)
		}
	}
	// A timeout's kill is the timeout's to report.
	if err := modelFailed("review", agent.RunResult{IsError: true, TimedOut: true, ErrorClass: agent.ErrTimeout, Exit: agent.Exit{ExitCode: 137, Signal: "SIGKILL"}}); !strings.Contains(err.Error(), "(timeout)") {
		t.Errorf("timeout: %v", err)
	}
}

// failingAgent answers every run with res and err.
type failingAgent struct {
	agent.Runner
	res agent.RunResult
	err error
}

func (f *failingAgent) Run(context.Context, agent.RunRequest) (agent.RunResult, error) {
	return f.res, f.err
}

// A CLI that exits non-zero: the attempt-failed comment says how, in a
// line, and the run's audit record has the whole exit.
func TestModelExitInFailureComment(t *testing.T) {
	h := newHEnv(t, yamlChecks)
	au := h.audited()
	h.r.Agent = &failingAgent{res: agent.RunResult{IsError: true, ErrorClass: agent.ErrHarness,
		Exit: agent.Exit{ExitCode: 1, DurationS: 47 * 60, StderrTail: "a\nb\nfatal: boom\n", StdoutTail: "{}"}}}
	ref := h.file("Say hello")
	if err := h.r.Implement(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := h.comments(ref); !strings.Contains(c, "Attempt 1 of 3 failed:\n\n```text\nthe model run exited 1 after 47m: fatal: boom\n```") {
		t.Fatalf("comments:\n%s", c)
	}
	d := audittest.Detail(au.Of("agent.run")[0])
	if d["exit_code"] != float64(1) || d["signal"] != "" || d["oom_suspected"] != false || d["stderr_tail"] != "a\nb\nfatal: boom\n" || d["duration_seconds"] != float64(47*60) {
		t.Fatalf("the run's record: %v", d)
	}
	if _, ok := d["stdout_tail"]; ok {
		t.Fatalf("the stream's tail is the transcript's to record: %v", d)
	}
}

// An agent that stops answering: the harness asks serve how the agent
// container ended, and the comment says that with the EOF.
func TestAgentStoppedAnsweringInFailureComment(t *testing.T) {
	for _, served := range []bool{true, false} {
		h := newHEnv(t, yamlChecks)
		au := h.audited()
		h.r.Pod, h.r.AgentExitWait = "devbot-implement-x", 3*time.Second
		h.r.Agent = &failingAgent{err: &url.Error{Op: "Post", URL: "http://agent/run", Err: io.EOF}}
		ref := h.file("Say hello")
		ctx, cancel := context.WithCancel(context.Background())
		// serve, reading the pod for the lease that asks.
		go func() {
			for ctx.Err() == nil {
				ls, _ := h.e.Q.Running(ctx)
				for _, l := range ls {
					if served && l.AgentLost && l.Pod == "devbot-implement-x" {
						h.e.Q.SetAgentExit(ctx, l.Pod, queue.AgentExit{Reason: "OOMKilled", ExitCode: 137, FinishedAt: time.Now()}, time.Minute)
					}
				}
				time.Sleep(50 * time.Millisecond)
			}
		}()
		err := h.r.Implement(context.Background())
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		want := "running the model: the agent stopped answering (EOF after 0s)\n```"
		if served {
			want = "running the model: the agent stopped answering (EOF after 0s); its container ended OOMKilled, exit 137\n```"
		}
		if c := h.comments(ref); !strings.Contains(c, want) {
			t.Fatalf("served %v: comments:\n%s", served, c)
		}
		d := audittest.Detail(au.Of("agent.run")[0])
		if c, _ := d["agent_container"].(map[string]any); served != (c != nil && c["reason"] == "OOMKilled" && c["exit_code"] == float64(137)) {
			t.Fatalf("served %v: the run's record: %v", served, d)
		}
	}
}

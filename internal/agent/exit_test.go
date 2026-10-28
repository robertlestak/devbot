package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scriptHarness runs a bash script as its CLI: a fake harness, to see how
// the agent answers for each way a CLI can end.
type scriptHarness struct{ script string }

func (scriptHarness) Name() string { return "script" }
func (h scriptHarness) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if h.script == "panic" {
		panic("boom")
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", h.script)
	cmd.Dir = req.Dir
	var stdout, stderr bytes.Buffer
	exit, err := RunCLI(cmd, &stdout, &stderr)
	res := RunResult{Exit: exit, IsError: exit.Failed(), TimedOut: ctx.Err() != nil}
	if ee := (*exec.ExitError)(nil); err != nil && !errors.As(err, &ee) {
		return res, err
	}
	return res, nil
}

// fakeCgroup points the memory files at a temporary memory.events, and
// returns its path.
func fakeCgroup(t *testing.T, ooms string) string {
	t.Helper()
	dir := t.TempDir()
	events, max := filepath.Join(dir, "memory.events"), filepath.Join(dir, "memory.max")
	os.WriteFile(events, []byte("low 0\nhigh 0\nmax 12\noom 1\noom_kill "+ooms+"\noom_group_kill 0\n"), 0o644)
	os.WriteFile(max, []byte("8589934592\n"), 0o644)
	oldE, oldM := MemoryEventsFile, MemoryMaxFile
	MemoryEventsFile, MemoryMaxFile = events, max
	t.Cleanup(func() { MemoryEventsFile, MemoryMaxFile = oldE, oldM })
	return events
}

// serveScripts serves the agent with one script harness per name, and
// returns a client and where the agent logs.
func serveScripts(t *testing.T, ctx context.Context, scripts map[string]string) (*Client, *bytes.Buffer) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "a.sock")
	hs := map[string]Harness{}
	for name, s := range scripts {
		hs[name] = scriptHarness{script: s}
	}
	var logs bytes.Buffer
	go Serve(ctx, sock, &Local{Harnesses: hs}, slog.New(slog.NewJSONHandler(&logs, nil)))
	c := NewClient(sock)
	if err := c.Wait(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	return c, &logs
}

// Every way the CLI ends gets an answer with how it ended: the status says
// whether the run or the agent failed, the body has the exit, and the agent
// logs the same.
func TestRunAnswersHowTheCLIEnded(t *testing.T) {
	events := fakeCgroup(t, "0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, logs := serveScripts(t, ctx, map[string]string{
		"ok":      `echo '{"type":"result"}'`,
		"exit1":   `echo some output; echo starting >&2; echo 'fatal: out of disk' >&2; exit 1`,
		"exit137": `echo 'killed by a script' >&2; exit 137`,
		"sigkill": `echo 'loading 9 GB of candles' >&2; kill -9 $$`,
		"oom":     `echo 'loading 9 GB of candles' >&2; n=$(sed -n 's/^oom_kill //p' ` + events + `); printf 'oom_kill %d\n' $((n+1)) > ` + events + `; kill -9 $$`,
	})
	for _, tc := range []struct {
		harness string
		status  int
		code    int
		signal  string
		oom     bool
		stderr  string
	}{
		{"ok", StatusRunOK, 0, "", false, ""},
		{"exit1", StatusRunFailed, 1, "", false, "fatal: out of disk"},
		{"exit137", StatusRunFailed, 137, "", false, "killed by a script"},
		{"sigkill", StatusRunFailed, 137, "SIGKILL", false, "loading 9 GB"},
		{"oom", StatusRunFailed, 137, "SIGKILL", true, "loading 9 GB"},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			// The body as the harness gets it, field names and all.
			b, _ := json.Marshal(RunRequest{Harness: tc.harness, Dir: t.TempDir()})
			resp, err := c.hc.Post("http://agent/run", "application/json", bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, tc.status, body)
			}
			var raw map[string]any
			if err := json.Unmarshal(body, &raw); err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{"exit_code", "duration_s", "oom_suspected"} {
				if _, ok := raw[k]; !ok {
					t.Errorf("no %s in %s", k, body)
				}
			}
			var res RunResult
			json.Unmarshal(body, &res)
			if res.ExitCode != tc.code || res.Signal != tc.signal || res.OOMSuspected != tc.oom || !strings.Contains(res.StderrTail, tc.stderr) || res.DurationS <= 0 {
				t.Fatalf("got %+v", res.Exit)
			}
			if tc.oom != (res.MemoryLimit == 8<<30) {
				t.Errorf("memory limit %d", res.MemoryLimit)
			}
			if tc.harness == "exit1" && res.StdoutTail != "some output\n" {
				t.Errorf("stdout tail %q", res.StdoutTail)
			}
			// The client hands back the result, failed or not, without an error.
			if got, err := c.Run(ctx, RunRequest{Harness: tc.harness, Dir: t.TempDir()}); err != nil || got.ExitCode != tc.code {
				t.Fatalf("client: %+v %v", got.Exit, err)
			}
		})
	}
	// The agent container's log says the same, once the pod is gone.
	var ended int
	for l := range strings.Lines(logs.String()) {
		var rec map[string]any
		json.Unmarshal([]byte(l), &rec)
		if rec["msg"] != "run ended" {
			continue
		}
		ended++
		if rec["harness"] == "oom" && (rec["level"] != "ERROR" || rec["oom_suspected"] != true || rec["signal"] != "SIGKILL" ||
			rec["exit_code"] != 137.0 || !strings.Contains(rec["stderr_tail"].(string), "candles")) {
			t.Errorf("the OOM run's log: %s", l)
		}
		if rec["harness"] == "ok" && rec["level"] != "INFO" {
			t.Errorf("the good run's log: %s", l)
		}
	}
	if ended != 10 {
		t.Errorf("%d runs logged their end, want 10:\n%s", ended, logs)
	}
}

// When the agent itself fails a run - a harness it doesn't have, a panic,
// or being stopped part way through - it still answers, with
// StatusAgentFailed, and the client says the agent failed.
func TestRunAnswersWhenTheAgentFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _ := serveScripts(t, ctx, map[string]string{
		"panic": "panic",
		"slow":  `echo working >&2; sleep 60`,
	})
	for _, h := range []string{"nope", "panic"} {
		_, err := c.Run(ctx, RunRequest{Harness: h, Dir: t.TempDir()})
		if ae := (*AgentError)(nil); !errors.As(err, &ae) {
			t.Fatalf("%s: want an AgentError, got %v", h, err)
		}
	}

	type answer struct {
		res RunResult
		err error
	}
	done := make(chan answer, 1)
	go func() {
		res, err := c.Run(context.Background(), RunRequest{Harness: "slow", Dir: t.TempDir()})
		done <- answer{res, err}
	}()
	time.Sleep(500 * time.Millisecond)
	start := time.Now()
	cancel() // SIGTERM, say
	a := <-done
	if ae := (*AgentError)(nil); !errors.As(a.err, &ae) || !strings.Contains(ae.Msg, "stopped during the run") {
		t.Fatalf("a stopped agent must say so, not drop the connection: %v", a.err)
	}
	if a.res.Signal != "SIGKILL" || !strings.Contains(a.res.StderrTail, "working") || time.Since(start) > 10*time.Second {
		t.Fatalf("the stopped run's exit: %+v after %v", a.res.Exit, time.Since(start))
	}
}

func TestOOMKills(t *testing.T) {
	events := fakeCgroup(t, "3")
	if n := oomKills(events); n != 3 {
		t.Fatalf("oom_kill %d", n)
	}
	if n := oomKills(filepath.Join(t.TempDir(), "none")); n != 0 {
		t.Fatalf("no file: %d", n)
	}
	// A count from before the run isn't this run's.
	exit, _ := RunCLI(exec.CommandContext(t.Context(), "bash", "-c", "kill -9 $$"), &bytes.Buffer{}, &bytes.Buffer{})
	if exit.Signal != "SIGKILL" || exit.ExitCode != 137 || exit.OOMSuspected {
		t.Fatalf("%+v", exit)
	}
	// Nor is an OOM kill that ended a run some other way.
	exit, _ = RunCLI(exec.CommandContext(t.Context(), "bash", "-c", "printf 'oom_kill 4\n' > "+events+"; exit 1"), &bytes.Buffer{}, &bytes.Buffer{})
	if exit.ExitCode != 1 || exit.OOMSuspected {
		t.Fatalf("%+v", exit)
	}
}

func TestBytes(t *testing.T) {
	for n, want := range map[int64]string{8 << 30: "8Gi", 16 << 30: "16Gi", 512 << 20: "512Mi", 1536 << 20: "1536Mi", 1000: "1000"} {
		if got := Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %s, want %s", n, got, want)
		}
	}
}

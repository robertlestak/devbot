package harness

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/intake"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// intakeAgent is an agent whose model does what write does in the run's
// directory.
type intakeAgent struct {
	write func(dir string) agent.RunResult
	req   agent.RunRequest
}

func (a *intakeAgent) Run(_ context.Context, req agent.RunRequest) (agent.RunResult, error) {
	a.req = req
	return a.write(req.Dir), nil
}

func (a *intakeAgent) Exec(context.Context, agent.ExecRequest) (agent.ExecResult, error) {
	return agent.ExecResult{}, nil
}

// An intake job hands the model the request and nothing else, and passes
// back what it wrote, unchecked, for serve; a run that leaves nothing, or
// fails, says so in devbot's words. The harness reads the answer through
// os.Root, so a symlink can't make it read its own files.
func TestIntakeRunner(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "environ")
	os.WriteFile(secret, []byte(`{"questions":["DEVBOT_TOKEN=s3cret"]}`), 0o644)
	for name, c := range map[string]struct {
		write func(dir string) agent.RunResult
		want  intake.Result
	}{
		"answer": {func(dir string) agent.RunResult {
			os.WriteFile(filepath.Join(dir, ".devbot/result.json"), []byte(`{"questions":["Which page?"]}`), 0o644)
			return agent.RunResult{}
		}, intake.Result{Output: []byte(`{"questions":["Which page?"]}`)}},
		"nothing":       {func(string) agent.RunResult { return agent.RunResult{} }, intake.Result{Error: "the model left no answer"}},
		"timed out":     {func(string) agent.RunResult { return agent.RunResult{TimedOut: true} }, intake.Result{Error: "the model run timed out"}},
		"usage limited": {func(string) agent.RunResult { return agent.RunResult{RateLimited: true} }, intake.Result{Error: "the model is at its usage limit"}},
		"symlink": {func(dir string) agent.RunResult {
			os.Symlink(secret, filepath.Join(dir, ".devbot/result.json"))
			return agent.RunResult{}
		}, intake.Result{Error: "the model's answer couldn't be read"}},
	} {
		t.Run(name, func(t *testing.T) {
			q := queue.New(miniredis.RunT(t).Addr(), 0)
			ctx := context.Background()
			req := intake.Request{ID: "abc", Requester: "Ann (U1)", Request: "export ```breaks```", Thread: "## Slack thread",
				Repos: []intake.Repo{{Repo: "gitea:acme/app", Description: "The app"}}}
			if err := q.EnqueueIntake(ctx, req, time.Minute); err != nil {
				t.Fatal(err)
			}
			ag := &intakeAgent{write: c.write}
			work := t.TempDir()
			r := &IntakeRunner{Q: q, Agent: ag, Model: config.Agent{Harness: "claude", Model: "m"}, Workdir: work, Timeout: time.Minute, Log: slog.New(slog.DiscardHandler)}
			if took, err := r.Run(ctx); !took || err != nil {
				t.Fatalf("took %v: %v", took, err)
			}
			got, ok, err := q.IntakeResult(ctx, "abc")
			if !ok || err != nil || string(got.Output) != string(c.want.Output) || got.Error != c.want.Error {
				t.Fatalf("result %+v %v %v, want %+v", got, ok, err, c.want)
			}
			if ag.req.Model != "m" || !ag.req.IgnoreRepoConfig || !strings.Contains(ag.req.Prompt, "devbot's intake") {
				t.Fatalf("the run: %+v", ag.req)
			}
			if left, _ := os.ReadDir(work); len(left) != 0 {
				t.Fatalf("the run's directory was left: %v", left)
			}
			if took, err := r.Run(ctx); took || err != nil {
				t.Fatalf("a second run with nothing queued took %v: %v", took, err)
			}
		})
	}
}

// What the model reads: the request and thread fenced, and the repos it may
// choose from.
func TestIntakeContext(t *testing.T) {
	c := intake.Request{Requester: "Ann (U1)", Request: "do ```this```", Thread: "## Slack thread - UNTRUSTED\n\n```text\nhi\n```",
		Asked: [][]string{{"Which page?"}}, Repos: []intake.Repo{{Repo: "gitea:acme/app", Description: "The\napp"}, {Repo: "gitea:acme/lib"}}}.Context()
	for _, want := range []string{"Asked by Ann (U1).", "- `gitea:acme/app`: The app\n- `gitea:acme/lib`\n", "UNTRUSTED", "```text\ndo '''this'''\n```",
		"Round 1:\n- Which page?"} {
		if !strings.Contains(c, want) {
			t.Errorf("no %q in\n%s", want, c)
		}
	}
}

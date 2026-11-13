package harness

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/intake"
	"git.shdw.tech/rob/devbot/internal/prompts"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// IntakeRunner runs Slack intake jobs, in an intake job pod: the model
// reads a request, its thread and the requester's repos, and drafts a task
// or asks questions. The pod has no forge credential and clones nothing;
// its Redis user reaches only the intake keys. What the model writes goes
// back to serve as it is, for serve to check (intake.Parse): nothing here
// trusts it, or acts on it.
type IntakeRunner struct {
	Q     *queue.Store
	Agent agent.Runner
	// Model is the intake's agent settings (config.IntakeAgent).
	Model config.Agent
	// Workdir is the directory shared with the agent container.
	Workdir string
	// Timeout bounds the model's run.
	Timeout time.Duration
	Log     *slog.Logger
}

// intakeResultTTL is how long a result waits for serve, which polls for it
// every second or so.
const intakeResultTTL = time.Hour

// Run runs one intake job, when one waits. It reports whether it took one;
// whatever came of it is the job's result, for serve.
func (r *IntakeRunner) Run(ctx context.Context) (bool, error) {
	req, err := r.Q.TakeIntake(ctx)
	if err != nil || req == nil {
		return false, err
	}
	log := r.Log.With("kind", queue.KindIntake, "intake", req.ID)
	res := r.run(ctx, log, req)
	if res.Error != "" {
		log.Warn("intake run failed", "why", res.Error)
	}
	return true, r.Q.PutIntakeResult(context.WithoutCancel(ctx), req.ID, res, intakeResultTTL)
}

func (r *IntakeRunner) run(ctx context.Context, log *slog.Logger, req *intake.Request) intake.Result {
	dir, err := os.MkdirTemp(r.Workdir, "intake-")
	if err != nil {
		log.Error("intake workspace", "err", err)
		return intake.Result{Error: "devbot couldn't prepare the run"}
	}
	defer os.RemoveAll(dir)
	// Writable from the agent side, whatever uid it runs as.
	if err := os.Chmod(dir, 0o777); err != nil {
		return intake.Result{Error: "devbot couldn't prepare the run"}
	}
	w := &workspace{repo: dir}
	if err := w.writeShared(".devbot/context.md", []byte(req.Context())); err != nil {
		log.Error("intake context", "err", err)
		return intake.Result{Error: "devbot couldn't prepare the run"}
	}
	os.Chmod(filepath.Join(dir, ".devbot"), 0o777)
	a := r.Model
	run := agent.RunRequest{Harness: a.Harness, Model: a.Model, Dir: dir, Timeout: r.Timeout, IgnoreRepoConfig: true,
		Prompt: prompts.Intake(prompts.IntakeVars{MaxTitle: intake.MaxTitle, MaxText: intake.MaxText, MaxQuestions: intake.MaxQuestions,
			MaxQuestion: intake.MaxQuestion, MaxRepos: intake.MaxRepos})}
	if a.AWS != nil {
		run.BedrockRegion = a.AWS.Region
	}
	res, err := r.Agent.Run(ctx, run)
	outcome := string(res.ErrorClass)
	if err != nil {
		outcome = string(agent.ErrHarness)
	} else if outcome == "" {
		outcome = "ok"
	}
	r.Q.Count(context.WithoutCancel(ctx), "agent_runs", []string{a.Harness, a.Model, queue.KindIntake, outcome}, 1)
	log.Info("intake run", "outcome", outcome, "took", res.Took())
	switch {
	case err != nil:
		log.Error("intake run", "err", err)
		return intake.Result{Error: "the model run failed"}
	case res.RateLimited:
		return intake.Result{Error: "the model is at its usage limit"}
	case res.TimedOut:
		return intake.Result{Error: "the model run timed out"}
	}
	out, err := readIntakeResult(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return intake.Result{Error: "the model left no answer"}
	case err != nil:
		log.Warn("reading the intake result", "err", err)
		return intake.Result{Error: "the model's answer couldn't be read"}
	case !json.Valid(out):
		return intake.Result{Error: "the model's answer isn't JSON"}
	}
	return intake.Result{Output: out}
}

// readIntakeResult reads the model's .devbot/result.json in dir through
// os.Root, so a symlink there can't make the harness read its own files.
func readIntakeResult(dir string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(".devbot/result.json")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, intake.MaxOutput+1))
	if err == nil && len(b) > intake.MaxOutput {
		return nil, errors.New("result.json is too large")
	}
	return b, err
}

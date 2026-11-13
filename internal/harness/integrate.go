package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/audit"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// integrationOutput is how much of the check's output the result quotes:
// its last lines, as CI's log would end.
const (
	integrationLines = 80
	integrationBytes = 8 << 10
)

// infraError is an integrate run that couldn't run the check (a clone that
// failed, services that didn't start), which is tried again: it says
// nothing about the heads.
type infraError struct{ err error }

func (e *infraError) Error() string { return e.err.Error() }
func (e *infraError) Unwrap() error { return e.err }

// Integrate runs one integrate job: a parent task's integration check over
// the heads the job names. It runs no model. The heads are cloned into the
// harness's private storage with the implementer's credential, and copies
// handed to the agent container, laid out by repo, where the repo's command
// runs from the workspace root. It returns nil when there was nothing to do;
// the result is recorded on the forge.
func (r *Runner) Integrate(ctx context.Context) error {
	r.defaults()
	j, err := r.take(ctx, queue.KindIntegrate)
	if err != nil || j == nil {
		return err
	}
	claimed := time.Now()
	released := false
	release := func() {
		if !released {
			r.E.Q.Release(context.WithoutCancel(ctx), queue.KindIntegrate, j.Ref, r.Owner)
			released = true
		}
	}
	defer release()
	ref, err := forge.ParseItem(j.Ref)
	if err != nil {
		return err
	}
	log := r.Log.With("task", j.Ref, "kind", queue.KindIntegrate)
	ctx = audit.With(ctx, audit.Fields{Job: r.Owner})
	// serve evaluates the parent, and with it what the result lets merge.
	var heads []core.IntegrationHead
	defer func() {
		release()
		for _, ref := range append([]string{ref.String()}, headTasks(heads)...) {
			if err := r.E.Q.RequestEval(context.WithoutCancel(ctx), ref); err != nil {
				log.Warn("request evaluation after job", "of", ref, "err", err)
			}
		}
	}()
	p, err := r.E.Forges.For(ref.Repo)
	if err != nil {
		return err
	}
	rc, err := r.E.Repos.Get(ctx, ref.Repo)
	if err != nil {
		if errors.Is(err, config.ErrNotOptedIn) {
			return nil
		}
		return err
	}
	if moved, err := r.misrouted(ctx, j, rc, release); moved {
		return err
	}
	unlockTask, err := r.lockTask(ctx, ref)
	if err != nil {
		return err
	}
	defer unlockTask()
	it, err := p.GetItem(ctx, ref)
	if err != nil {
		return err
	}
	ctx = r.E.TaskContext(ctx, it)
	comments, err := p.ListComments(ctx, ref)
	if err != nil {
		return err
	}
	cfg, in, err := r.E.ParentIntegration(ctx, it, comments)
	if err != nil {
		return err
	}
	// Queued for heads that moved since, or that a run has settled: the
	// evaluation this job's end asks for queues what is due now.
	if cfg == nil || !in.Due() || !slices.Equal(in.Keys(), j.Heads) || in.GaveUp(*cfg.Retries) {
		log.Info("stale job, the parent's integration check isn't due for these heads")
		return nil
	}
	heads = in.Heads
	m := r.E.Meta(ctx, it, comments)
	attr := core.NewAttribution(it, m, queue.KindIntegrate, 0, rc.Labels)
	log = log.With(attr.LogAttrs()...)
	defer r.observe(ctx, attr, claimed)
	attempt, tries := in.Errors+1, *cfg.Retries+1
	r.describe(ctx, queue.KindIntegrate, attr, queue.LeaseDetail{Mode: "integration", Attempt: attempt, MaxAttempts: tries, Services: rc.Services})
	if err := r.E.RecordIntegration(ctx, it, j.Heads, core.IntegrationStarted,
		fmt.Sprintf("Running the integration check (attempt %d of %d) on:\n\n%s", attempt, tries, headList(it.Ref.Repo, heads))); err != nil {
		return err
	}
	r.setStatuses(ctx, log, it, heads, "pending", "running on every child task's head")
	unlockTask()

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	// The clones and services come out of the run's time too, but no
	// further than the pod's own deadline would let it go.
	runCtx, cancelT := context.WithTimeout(runCtx, cfg.Timeout+15*time.Minute)
	defer cancelT()
	defer r.startHeartbeat(runCtx, cancel, p, queue.KindIntegrate, ref, func(forge.Item) bool { return true })()

	passed, output, err := r.integrate(runCtx, cancel, log, rc, cfg, heads, in.Bare, j.Heads)
	if errors.Is(context.Cause(runCtx), errLeaseLost) || errors.Is(context.Cause(runCtx), errCancelled) {
		r.stopped(ctx, log, queue.KindIntegrate, ref, context.Cause(runCtx), nil)
		return nil
	}
	if se := (*servicesError)(nil); errors.As(context.Cause(runCtx), &se) {
		err = &infraError{se}
	}
	ctx = context.WithoutCancel(ctx)
	mode, text, outcome := core.IntegrationPassed, "", "passed"
	switch {
	case err != nil && errors.As(err, new(*infraError)):
		mode, outcome = core.IntegrationError, "error"
		text = fmt.Sprintf("The integration check couldn't run (attempt %d of %d):\n\n%s", attempt, tries, fence(firstBytes(err.Error(), integrationBytes)))
		if attempt >= tries {
			text += "\n\nIt has used up its tries (`integration.retries`), so this parent task needs a person: retry it to run the check again."
		} else {
			text += "\n\ndevbot runs it again."
		}
	case err != nil:
		return err
	case passed:
		text = fmt.Sprintf("The integration check passed on:\n\n%s\n\nEvery child task with these heads may merge as its own gate allows.", headList(it.Ref.Repo, heads))
	default:
		mode, outcome = core.IntegrationFailed, "failed"
		text = fmt.Sprintf("The integration check failed on:\n\n%s\n\nIts output ends:\n\n%s\n\n"+
			"None of the child tasks merges until it passes on their heads, so this parent task needs a person: send the child task that broke it back (with `edit_task`, or a review on its PR), or retry this parent task to run the check again.",
			headList(it.Ref.Repo, heads), fence(output))
	}
	err = r.settleParent(ctx, p, queue.KindIntegrate, ref, func(it forge.Item) error { return r.E.RecordIntegration(ctx, it, j.Heads, mode, text) })
	if r.stopped(ctx, log, queue.KindIntegrate, ref, nil, err) {
		return nil
	}
	if err != nil {
		return err
	}
	if mode == core.IntegrationError && attempt >= tries {
		r.E.Emit(ctx, it, "parent.needs_human", core.ParentNeedsHuman, "the integration check couldn't run", "")
	}
	state := map[string]string{core.IntegrationPassed: "success", core.IntegrationFailed: "failure", core.IntegrationError: "error"}[mode]
	r.setStatuses(ctx, log, it, heads, state, map[string]string{"success": "passed on every child task's head", "failure": "failed on these heads: see the parent task", "error": "couldn't run: see the parent task"}[state])
	log.Info("integration check", "outcome", outcome)
	r.count(ctx, queue.KindIntegrate, ref.Repo.String(), outcome)
	return nil
}

// settleParent runs fn, a job's result, under the parent task's lock, on
// the issue as it is now, and only while the job of kind holds its lease
// and the parent is open: anything else is a cancel or a lost lease.
func (r *Runner) settleParent(ctx context.Context, p forge.Provider, kind string, ref forge.ItemRef, fn func(forge.Item) error) error {
	unlock, err := r.lockTask(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()
	if ok, err := r.E.Q.Renew(ctx, kind, ref.String(), r.Owner, r.LeaseTTL); err != nil {
		return err
	} else if !ok {
		return errLeaseLost
	}
	it, err := p.GetItem(ctx, ref)
	if err != nil {
		return err
	}
	if it.State != "open" {
		return errCancelled
	}
	return fn(it)
}

// integrate prepares the workspace and runs the check, reporting whether it
// passed and the end of its output. An error is an *infraError when the
// check couldn't run.
func (r *Runner) integrate(ctx context.Context, stop context.CancelCauseFunc, log *slog.Logger, rc *config.Repo, cfg *config.Integration, heads []core.IntegrationHead, bare []forge.RepoRef, keys []string) (bool, string, error) {
	if err := r.E.Cfg.CheckServices(rc); err != nil {
		return false, "", &infraError{fmt.Errorf("services: %w", err)}
	}
	start := time.Now()
	w, err := r.prepareIntegration(ctx, heads, bare, cfg.Repos)
	if w != nil {
		defer w.cleanup()
	}
	var ie *integrationLayoutError
	if errors.As(err, &ie) {
		return false, ie.Error(), nil
	}
	if err != nil {
		return false, "", &infraError{fmt.Errorf("preparing the workspace: %w", err)}
	}
	r.phase(ctx, queue.KindIntegrate, "prepare", start)
	svc, err := r.startServices(ctx, stop, rc)
	if err != nil {
		return false, "", &infraError{err}
	}
	defer svc.Close()
	env := map[string]string{"DEVBOT_INTEGRATION_HEADS": strings.Join(keys, " ")}
	for k, v := range rc.Env {
		env[k] = v
	}
	start = time.Now()
	res, err := r.Agent.Exec(ctx, agent.ExecRequest{Dir: w.shared, Script: "set -o pipefail\n( " + cfg.Command + " )\n", Timeout: cfg.Timeout, Env: env})
	r.phase(ctx, queue.KindIntegrate, "check", start)
	if err != nil {
		return false, "", &infraError{fmt.Errorf("running the check: %w", err)}
	}
	out := firstBytes(lastLines(res.Output, integrationLines), integrationBytes)
	if res.TimedOut {
		out += fmt.Sprintf("\n\n(the check ran out of time: integration.timeout is %s)", cfg.Timeout)
	}
	return res.ExitCode == 0 && !res.TimedOut, out, nil
}

// integrationWorkspace is an integrate run's checkouts: each repo cloned
// privately with the implementer's credential, and copied into shared,
// under <provider>/<owner>/<name>, for the agent. As for an implement run,
// the harness runs no git in shared once the agent may touch it, and reads
// nothing back from it.
type integrationWorkspace struct {
	priv, shared string
}

func (w *integrationWorkspace) cleanup() {
	os.RemoveAll(w.priv)
	os.RemoveAll(w.shared)
}

// integrationLayoutError is heads the workspace can't lay out: two children
// of one repo, which would share its one directory. Running again wouldn't
// change that, so it is the result.
type integrationLayoutError struct{ why string }

func (e *integrationLayoutError) Error() string { return e.why }

// prepareIntegration checks out each head, and at its default branch each
// repo of bare and of extra (full repo refs) that no head is of.
func (r *Runner) prepareIntegration(ctx context.Context, heads []core.IntegrationHead, bare []forge.RepoRef, extra []string) (*integrationWorkspace, error) {
	id := randomID()
	privRoot := r.PrivateDir
	if privRoot == "" {
		privRoot = os.TempDir()
	}
	w := &integrationWorkspace{priv: filepath.Join(privRoot, "devbot-"+id), shared: filepath.Join(r.Workdir, id)}
	bare = slices.Clone(bare)
	type checkout struct {
		repo forge.RepoRef
		pr   *forge.PR // nil: the default branch
	}
	var cos []checkout
	seen := map[forge.RepoRef]forge.ItemRef{}
	for _, h := range heads {
		if other, ok := seen[h.Task.Repo]; ok {
			return nil, &integrationLayoutError{fmt.Sprintf("Child tasks %s and %s are both of %s, and the integration workspace has one checkout of each repo, so it can't hold both heads. Give each repo one child task.",
				other, h.Task, h.Task.Repo)}
		}
		seen[h.Task.Repo] = h.Task
		pr := h.PR
		cos = append(cos, checkout{repo: h.Task.Repo, pr: &pr})
	}
	for _, s := range extra {
		repo, err := forge.ParseRepo(s)
		if err != nil {
			return nil, err
		}
		bare = append(bare, repo)
	}
	for _, repo := range bare {
		if _, ok := seen[repo]; !ok {
			seen[repo] = forge.ItemRef{}
			cos = append(cos, checkout{repo: repo})
		}
	}
	if err := os.MkdirAll(w.priv, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(w.shared, 0o777); err != nil {
		return w, err
	}
	os.Chmod(w.shared, 0o777)
	for _, co := range cos {
		if err := r.checkoutFor(ctx, w, co.repo, co.pr); err != nil {
			return w, fmt.Errorf("%s: %w", co.repo, err)
		}
	}
	// Everything in the shared volume must be writable from the agent side,
	// whatever uid each container runs as.
	exec.CommandContext(ctx, "chmod", "-R", "a+rwX", w.shared).Run()
	return w, nil
}

// checkoutFor clones repo privately and copies it to its place in shared,
// at pr's head, which must still be the one the job names, or at the
// default branch.
func (r *Runner) checkoutFor(ctx context.Context, w *integrationWorkspace, repo forge.RepoRef, pr *forge.PR) error {
	if !config.ValidPathPart(repo.Provider) || !config.ValidPathPart(repo.Owner) || !config.ValidPathPart(repo.Name) {
		return fmt.Errorf("%q isn't a repo the workspace can lay out", repo)
	}
	p, err := r.E.Forges.For(repo)
	if err != nil {
		return err
	}
	url, header, err := p.GitAuth(ctx, repo, forge.RoleImplementer)
	if err != nil {
		return err
	}
	// The credential must never reach an audit record, whatever git says.
	r.E.Audit.Secret(header, header[strings.LastIndex(header, " ")+1:])
	rel := filepath.Join(repo.Provider, repo.Owner, repo.Name)
	priv := filepath.Join(w.priv, rel)
	if err := os.MkdirAll(filepath.Dir(priv), 0o700); err != nil {
		return err
	}
	if _, err := runGit(ctx, filepath.Dir(priv), header, true, "clone", "--quiet", "--no-tags", url, priv); err != nil {
		return err
	}
	sha := "HEAD"
	if pr != nil {
		// The PR's own ref: its head branch may be a fork's, or gone since
		// it merged.
		if _, err := runGit(ctx, priv, header, true, "fetch", "--quiet", "--no-tags", "origin", fmt.Sprintf("+refs/pull/%d/head:refs/devbot/head", pr.Number)); err != nil {
			return err
		}
		if _, err := runGit(ctx, priv, "", false, "rev-parse", "--verify", "--quiet", pr.HeadSHA+"^{commit}"); err != nil {
			return fmt.Errorf("PR #%d's head %s isn't on the forge any more", pr.Number, pr.HeadSHA)
		}
		sha = pr.HeadSHA
	}
	if _, err := runGit(ctx, priv, "", false, "checkout", "--quiet", "--detach", sha); err != nil {
		return err
	}
	dest := filepath.Join(w.shared, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
		return err
	}
	// The agent's copy, configured before it is handed over.
	if _, err := runGit(ctx, filepath.Dir(dest), "", false, "clone", "--quiet", "--no-hardlinks", priv, dest); err != nil {
		return err
	}
	for _, s := range [][]string{{"checkout", "--quiet", "--detach", sha}, {"remote", "set-url", "origin", url}} {
		if _, err := runGit(ctx, dest, "", false, s...); err != nil {
			return err
		}
	}
	return nil
}

// setStatuses posts the devbot/integration commit status on each open head,
// linking the parent. A forge that turns it away (a GitHub App without
// Commit statuses write) leaves the result on the parent alone.
func (r *Runner) setStatuses(ctx context.Context, log *slog.Logger, parent forge.Item, heads []core.IntegrationHead, state, desc string) {
	for _, h := range heads {
		if h.PR.Merged {
			continue
		}
		p, err := r.E.Forges.For(h.PR.Repo)
		if err == nil {
			err = p.SetCommitStatus(ctx, h.PR.Repo, h.PR.HeadSHA, forge.CommitStatus{Context: core.IntegrationContext, State: state, Description: desc, URL: parent.URL})
		}
		if err != nil {
			log.Warn("posting the integration check's commit status", "pr", h.PR.Number, "repo", h.PR.Repo, "err", err)
		}
	}
}

// headList is one line per head, as an issue of own names its task.
func headList(own forge.RepoRef, heads []core.IntegrationHead) string {
	var b strings.Builder
	for _, h := range heads {
		state := ""
		if h.PR.Merged {
			state = ", merged"
		}
		fmt.Fprintf(&b, "- %s: PR %s at `%s`%s\n", core.RefText(own, h.Task), h.PR.URL, h.PR.HeadSHA[:min(12, len(h.PR.HeadSHA))], state)
	}
	return strings.TrimRight(b.String(), "\n")
}

func headTasks(heads []core.IntegrationHead) []string {
	out := make([]string, len(heads))
	for i, h := range heads {
		out[i] = h.Task.String()
	}
	return out
}

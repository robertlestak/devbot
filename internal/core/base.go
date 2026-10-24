package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// A task's work goes into one base branch. Highest first: its own (filed
// with one, a carried PR's, or retargeted since), then the repo's
// base_branch, then the repo's default branch. The record is on the forge,
// so it survives a Redis wipe: the last retarget's status comment, else the
// base the last implement run started on, else the base in the routing
// block the bot posted when it filed the task. A task without one is
// accepted onto the repo's base as it is then (acceptBase), and its first
// run records that, so a later edit to base_branch doesn't move it. The
// protected .devbot.yaml is still read from the default branch only.

// recordedBase is the base the task's own record names, or "" for none.
func recordedBase(ss []statusComment, m Meta) string {
	for i := len(ss) - 1; i >= 0; i-- {
		if (ss[i].Kind == "reset" || ss[i].Kind == "started") && ss[i].Base != "" {
			return ss[i].Base
		}
	}
	return m.Base
}

// defaultBranch is the repo's default branch, as the repo cache last read it.
func (e *Engine) defaultBranch(ctx context.Context, p forge.Provider, rc *config.Repo, repo forge.RepoRef) (string, error) {
	if rc != nil && rc.DefaultBranch != "" {
		return rc.DefaultBranch, nil
	}
	return p.DefaultBranch(ctx, repo)
}

// repoBase is the base of a task that names none of its own: the repo's
// base_branch, else its default branch.
func (e *Engine) repoBase(ctx context.Context, p forge.Provider, rc *config.Repo, repo forge.RepoRef) (string, error) {
	if rc != nil && rc.BaseBranch != "" {
		return rc.BaseBranch, nil
	}
	return e.defaultBranch(ctx, p, rc, repo)
}

// taskBase resolves the branch a task's work goes into, and remembers it for
// the events emitted about the task.
func (e *Engine) taskBase(ctx context.Context, p forge.Provider, rc *config.Repo, it forge.Item, comments []forge.Comment) (string, error) {
	b := recordedBase(statuses(comments, e.bots(p)...), e.meta(ctx, it, comments))
	if b == "" {
		var err error
		if b, err = e.repoBase(ctx, p, rc, it.Ref.Repo); err != nil {
			return "", err
		}
	}
	e.bases.put(it.Ref.String(), b)
	return b, nil
}

// TaskBase is the branch a task's work goes into, for the harness.
func (e *Engine) TaskBase(ctx context.Context, it forge.Item, comments []forge.Comment) (string, error) {
	p, err := e.provider(it.Ref.Repo)
	if err != nil {
		return "", err
	}
	rc, err := e.Repos.Get(ctx, it.Ref.Repo)
	if err != nil {
		return "", err
	}
	return e.taskBase(ctx, p, rc, it, comments)
}

// StartBase is the base an implement run records in its "started" status:
// the task's own, else accepted, the base its job was queued with when
// devbot accepted the task (acceptBase), else the task's base as it
// resolves now, for a job queued without one.
func (e *Engine) StartBase(ctx context.Context, it forge.Item, comments []forge.Comment, accepted string) (string, error) {
	p, err := e.provider(it.Ref.Repo)
	if err != nil {
		return "", err
	}
	if b := recordedBase(statuses(comments, e.bots(p)...), e.meta(ctx, it, comments)); b != "" || accepted == "" {
		return e.TaskBase(ctx, it, comments)
	}
	e.bases.put(it.Ref.String(), accepted)
	return accepted, nil
}

// acceptBase is the base devbot accepts a task onto, for its job to carry to
// the run that records it. A base_branch the repo doesn't have stops a task
// that would take it.
func (t *task) acceptBase(ctx context.Context) (base string, stopped bool, err error) {
	if base, err = t.base(ctx); err != nil {
		return "", false, err
	}
	if t.rc.BaseBranch == "" || recordedBase(t.ss, t.e.meta(ctx, t.it, t.comments)) != "" {
		return base, false, nil
	}
	ok, err := t.p.BranchExists(ctx, t.it.Ref.Repo, base)
	if err != nil {
		return "", false, fmt.Errorf("checking base branch %q on %s: %w", base, t.it.Ref.Repo, err)
	}
	if !ok {
		return "", true, t.needsHuman(ctx, fmt.Sprintf("This repo's `base_branch` in %s is `%s`, but the repo has no branch `%s`, so this task has nothing to start from. Create the branch or change `base_branch`, then re-apply `%s`; or retarget the task onto another branch.",
			config.RepoFile, base, base, LReady))
	}
	return base, false, nil
}

// shownBase is the base to show for a task: "" when it is the default
// branch, or isn't known without reading the task's comments.
func (e *Engine) shownBase(ctx context.Context, it forge.Item, comments []forge.Comment) string {
	p, err := e.provider(it.Ref.Repo)
	if err != nil {
		return ""
	}
	rc, err := e.Repos.Get(ctx, it.Ref.Repo)
	if err != nil {
		return ""
	}
	var b string
	if comments != nil {
		b = recordedBase(statuses(comments, e.bots(p)...), e.meta(ctx, it, comments))
	} else if b = e.bases.get(it.Ref.String()); b == "" {
		b = e.meta(ctx, it, nil).Base
	}
	if b == "" {
		b = rc.BaseBranch
	}
	if d, err := e.defaultBranch(ctx, p, rc, it.Ref.Repo); err != nil || b == d {
		return ""
	}
	return b
}

// ErrBadBase is a base branch file_task or retarget can't use.
var ErrBadBase = errors.New("bad base branch")

// checkBase refuses a base that isn't a plain branch name, or that the repo
// doesn't have.
func checkBase(ctx context.Context, p forge.Provider, repo forge.RepoRef, base string) error {
	if !config.ValidBranch(base) {
		return invalid(fmt.Errorf("%w %q: want %s", ErrBadBase, base, config.BranchRule))
	}
	ok, err := p.BranchExists(ctx, repo, base)
	if err != nil {
		return fmt.Errorf("checking base branch %q on %s: %w", base, repo, err)
	}
	if !ok {
		return invalid(fmt.Errorf("%w: %s has no branch %q; create it first", ErrBadBase, repo, base))
	}
	return nil
}

// base is the task's base, read once per evaluation.
func (t *task) base(ctx context.Context) (string, error) {
	if t.baseBranch != "" {
		return t.baseBranch, nil
	}
	if err := t.load(ctx); err != nil {
		return "", err
	}
	b, err := t.e.taskBase(ctx, t.p, t.rc, t.it, t.comments)
	t.baseBranch = b
	return b, err
}

// followBase keeps an open PR on the task's base. A PR whose base someone
// changed on the forge has that base adopted, as a retarget would; a PR
// whose head was built on a base the task has since left is rebased onto
// the new one. Either is the gate's first step, before reviews and checks.
func (t *task) followBase(ctx context.Context, pr *forge.PR) (bool, error) {
	base, err := t.base(ctx)
	if err != nil {
		return false, err
	}
	acted := false
	if pr.BaseBranch != base {
		body := fmt.Sprintf("PR #%d's base was changed on the forge from `%s` to `%s`, so this task's work now goes into `%s`. %s",
			pr.Number, base, pr.BaseBranch, pr.BaseBranch, retargetNote(pr))
		if err := t.e.leaveBase(ctx, pr.Repo, base, t.ref()); err != nil {
			return false, err
		}
		if err := t.p.Comment(ctx, t.it.Ref, body+"\n\n"+RenderStatus(Status{Kind: "reset", Mode: "retarget", Base: pr.BaseBranch})); err != nil {
			return false, err
		}
		t.e.Emit(ctx, t.it, "task.retargeted", t.state, fmt.Sprintf("base changed on the forge from %s to %s", base, pr.BaseBranch), pr.URL)
		t.comments, t.baseBranch = nil, ""
		if base, err = t.base(ctx); err != nil {
			return true, err
		}
		acted = true
	}
	from, owed := t.rebaseOwed(pr, base)
	if !owed {
		return acted, nil
	}
	reason := fmt.Sprintf("This task's base branch is now `%s`. Rebase the branch's own commits onto `origin/%s`", base, base)
	if from != "" && from != base {
		reason += fmt.Sprintf(" (it was built on `origin/%s`: `git rebase --onto origin/%s origin/%s`)", from, base, from)
	}
	reason += ", leaving out anything that came from the old base, resolve any conflicts, make sure the checks still pass, and push."
	did, err := t.fix(ctx, pr, "retarget", reason)
	return acted || did, err
}

// rebaseOwed reports whether the PR's head is on a base the task has left
// and no round since has moved it, and which base the head was built on,
// when devbot recorded it. The head may predate the task's last retarget, or
// have been built on another base without one: an implement run that read
// the base before FileTask's routing comment landed opens its PR against the
// task's base all the same.
func (t *task) rebaseOwed(pr *forge.PR, base string) (from string, owed bool) {
	retarget, built, fixed := -1, -1, -1
	for i, s := range t.ss {
		switch {
		case s.Kind == "reset" && s.Mode == "retarget":
			retarget = i
		case s.Kind == "fix-requested":
			fixed = i
		case s.Kind == "pr" && s.PR == pr.Number:
			built, from = i, s.Base
		}
	}
	if built < 0 || (fixed > built && fixed > retarget) {
		return "", false // a round since is on it
	}
	return from, built < retarget || (from != "" && from != base)
}

func retargetNote(pr *forge.PR) string {
	return fmt.Sprintf("Attempts, review rounds and rebase rounds start over, and PR #%d is rebased onto `%s`.", pr.Number, pr.BaseBranch)
}

// Retarget points a task's work at another base branch. An open PR is moved
// onto it on the forge and then rebased onto it; attempts, review rounds and
// rebase rounds start over, as with a retry.
func (e *Engine) Retarget(ctx context.Context, refStr, base string, c Caller) (TaskStatus, error) {
	it, p, rc, err := e.load(ctx, c, refStr)
	if err != nil {
		return TaskStatus{}, err
	}
	ctx = e.TaskContext(ctx, it)
	if it.State != "open" {
		return TaskStatus{}, invalid(fmt.Errorf("%s is closed", it.Ref))
	}
	if State(it) == "" {
		return TaskStatus{}, invalid(fmt.Errorf("%s isn't a devbot task", it.Ref))
	}
	if c.User != nil {
		if err := e.mayDirect(ctx, c, p, rc, it.Ref.Repo, "retarget"); err != nil {
			return TaskStatus{}, err
		}
	}
	if err := checkBase(ctx, p, it.Ref.Repo, base); err != nil {
		return TaskStatus{}, err
	}
	// Held so that an evaluation (the webhook for the PR's edit) can't see
	// the PR moved before the record says so, and take it for a change made
	// on the forge. It runs once the lock is let go.
	unlock, err := e.LockTask(ctx, it.Ref, connectorLockWait)
	if err != nil {
		return TaskStatus{}, err
	}
	who := c.Principal
	if c.User != nil {
		who = "@" + c.Login
	}
	err = e.retarget(ctx, p, rc, it, base, who, c.Principal)
	if _, uerr := unlock(); uerr != nil && err == nil {
		err = uerr
	}
	if err != nil {
		return TaskStatus{}, err
	}
	if _, err := e.EvaluateItem(ctx, it.Ref, SrcAPI); err != nil {
		return TaskStatus{}, err
	}
	return e.Status(ctx, c, refStr)
}

// retarget moves the task onto base for who, as the comment names them,
// recording principal: a connector call's caller, or a `/base` comment's
// author.
func (e *Engine) retarget(ctx context.Context, p forge.Provider, rc *config.Repo, it forge.Item, base, who, principal string) error {
	cs, err := p.ListComments(ctx, it.Ref)
	if err != nil {
		return err
	}
	cur, err := e.taskBase(ctx, p, rc, it, cs)
	if err != nil {
		return err
	}
	pr, err := e.FindPR(ctx, it)
	if err != nil {
		return err
	}
	if pr != nil && pr.State != "open" {
		pr = nil
	}
	if cur == base && (pr == nil || pr.BaseBranch == base) {
		return nil // already there
	}
	// The PR moves first: a record without the move would read as the PR
	// having been moved back on the forge.
	what := fmt.Sprintf("Attempts, review rounds and rebase rounds start over; the next PR is opened against `%s`.", base)
	oldPRBase := ""
	if pr != nil {
		oldPRBase = pr.BaseBranch
		if pr.BaseBranch != base {
			if err := p.SetPRBase(ctx, it.Ref.Repo, pr.Number, base); err != nil {
				return fmt.Errorf("moving PR #%d onto %s: %w", pr.Number, base, err)
			}
		}
		pr.BaseBranch = base
		what = retargetNote(pr)
	}
	// Out of the old base's queue before the record says it left: a task
	// taken out too early only joins again on its next evaluation.
	for _, b := range []string{cur, oldPRBase} {
		if b != "" && b != base {
			if err := e.leaveBase(ctx, it.Ref.Repo, b, it.Ref.String()); err != nil {
				return err
			}
		}
	}
	body := fmt.Sprintf("Retargeted by %s: this task's work now goes into `%s` instead of `%s`. %s\n\n%s",
		who, base, cur, what, RenderStatus(Status{Kind: "reset", Mode: "retarget", Base: base, Principal: principal}))
	if err := p.Comment(ctx, it.Ref, body); err != nil {
		return err
	}
	e.bases.put(it.Ref.String(), base)
	prURL := ""
	if pr != nil {
		prURL = pr.URL
	}
	e.Emit(ctx, it, "task.retargeted", State(it), fmt.Sprintf("retargeted from %s to %s by %s", cur, base, principal), prURL)
	// A stopped task starts again, as with a retry: the gate for one with a
	// PR, the implementer otherwise.
	if State(it) == LNeedsHuman {
		to := LReady
		if pr != nil {
			to = LInReview
		}
		return e.transition(ctx, p, it, to, "", "")
	}
	return nil
}

// baseCommand is the comment that sets a task's base: `/base develop`.
const baseCommand = "/base"

func parseBaseCommand(body string) (string, bool) {
	line := strings.TrimSpace(firstLine(body))
	if line == baseCommand {
		return "", true // refused by checkBase, with what it wants
	}
	rest, ok := strings.CutPrefix(line, baseCommand+" ")
	return strings.TrimSpace(rest), ok
}

// baseCommand acts on the latest `/base` comment from someone who can direct
// devbot since the task's base was last set or a `/base` refused. Posted
// before devbot accepted the task, it is the base the task starts from; after,
// it is a retarget. Comments from anyone else are ignored, as with `/rebase`.
// A stopped task reads no comments: its `/base` counts once it is retried.
func (t *task) baseCommand(ctx context.Context) (bool, error) {
	if t.state == LNeedsHuman {
		return false, nil
	}
	if err := t.load(ctx); err != nil {
		return false, err
	}
	var since int64
	for _, s := range t.ss {
		if (s.Kind == "reset" && s.Mode == "retarget") || s.Kind == "base-refused" {
			since = s.ID
		}
	}
	var base, by string
	bots := t.e.bots(t.p)
	for _, c := range t.comments {
		if c.ID <= since || forge.HasLogin(bots, c.Author) {
			continue
		}
		if b, ok := parseBaseCommand(c.Body); ok && t.e.authorized(ctx, t.p, t.rc, t.it.Ref.Repo, c.Author) {
			base, by = b, c.Author
		}
	}
	if by == "" {
		return false, nil
	}
	// Left unanswered: a no-op has nothing to say, and a later retarget
	// moves past it.
	if cur, err := t.base(ctx); err != nil || cur == base {
		return false, err
	}
	if err := checkBase(ctx, t.p, t.it.Ref.Repo, base); err != nil {
		if !errors.Is(err, ErrInvalid) {
			return false, err
		}
		body := fmt.Sprintf("Not changing this task's base branch, as @%s asked: %s.\n\n%s",
			by, Defang(err.Error()), RenderStatus(Status{Kind: "base-refused", Principal: by}))
		if err := t.p.Comment(ctx, t.it.Ref, body); err != nil {
			return false, err
		}
		t.comments = nil
		return true, t.load(ctx)
	}
	if err := t.e.retarget(ctx, t.p, t.rc, t.it, base, "@"+by, by); err != nil {
		return false, err
	}
	t.comments, t.baseBranch = nil, ""
	return true, t.load(ctx)
}

// baseCache remembers each task's base for the events emitted about it,
// which are written without reading the task's comments. It is only ever a
// label: what devbot does is decided from the forge.
type baseCache struct {
	mu sync.Mutex
	m  map[string]string
}

func (c *baseCache) get(ref string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[ref]
}

func (c *baseCache) put(ref, base string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]string{}
	}
	makeRoom(c.m, detailMax)
	c.m[ref] = base
}

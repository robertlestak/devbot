package core

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// branchPrefix is where every branch devbot pushes lives.
const branchPrefix = "devbot/"

// attachmentsBranch is where the GitHub adapter keeps PR attachments. It is
// under devbot/ but belongs to no task, so cleanup never touches it, on any
// forge.
const attachmentsBranch = "devbot/attachments"

// branchSweepEvery is how often reconcile sweeps a forge's branches. The
// sweep reads every repo's config and every devbot branch's issue, and the
// grace periods it waits out are hours, so every reconcile pass would be
// waste.
const branchSweepEvery = 15 * time.Minute

// closedPR does what a closed or merged PR leaves to do: release its
// attachments and delete its branch. Neither is returned as an error, since
// each only costs storage; both are logged.
func (t *task) closedPR(ctx context.Context, pr *forge.PR) {
	t.releaseAttachments(ctx, pr)
	if !*t.rc.BranchCleanup {
		return
	}
	if err := t.e.cleanupBranch(ctx, t.p, pr); err != nil {
		t.e.Log.Warn("deleting a closed PR's branch failed", "task", t.it.Ref, "pr", pr.Number, "branch", pr.HeadBranch, "err", err)
	}
}

// cleanupBranch deletes the head branch of pr, a closed or merged PR, unless
// something else may still need it.
func (e *Engine) cleanupBranch(ctx context.Context, p forge.Provider, pr *forge.PR) error {
	if pr.State == "open" && !pr.Merged {
		return nil
	}
	if keepBranchName(pr.HeadBranch) != "" {
		return nil
	}
	bs, err := p.ListBranches(ctx, pr.Repo, pr.HeadBranch)
	if err != nil {
		return err
	}
	for _, b := range bs {
		if b.Name != pr.HeadBranch {
			continue
		}
		open, err := p.ListOpenPRs(ctx, pr.Repo)
		if err != nil {
			return err
		}
		if why := keepPRBranch(p, pr, b, open); why != "" {
			e.Log.Info("keeping a closed PR's branch", "repo", pr.Repo, "branch", b.Name, "pr", pr.Number, "why", why)
			return nil
		}
		why := fmt.Sprintf("PR #%d was closed", pr.Number)
		if pr.Merged {
			why = fmt.Sprintf("PR #%d was merged", pr.Number)
		}
		return e.deleteBranch(ctx, p, pr.Repo, b.Name, why)
	}
	return nil
}

// keepBranchName says why a branch is never devbot's to delete, by its
// name alone, or "".
func keepBranchName(name string) string {
	switch {
	case !strings.HasPrefix(name, branchPrefix):
		return "not a " + branchPrefix + " branch"
	case name == attachmentsBranch:
		return "it holds PR attachments"
	}
	return ""
}

// keepPRBranch says why b, the head branch of pr, a closed or merged PR,
// must stay, or "" when it can go.
func keepPRBranch(p forge.Provider, pr *forge.PR, b forge.Branch, open []forge.PR) string {
	impl, _ := p.BotUsers()
	if why := keepBranchName(b.Name); why != "" {
		return why
	}
	switch {
	case pr.Fork:
		return "the PR's head is in a fork"
	case !forge.SameLogin(pr.Author, impl):
		return "the PR was opened by @" + pr.Author + ", not the implementer"
	case b.SHA != pr.HeadSHA:
		return "the branch has moved on since the PR closed"
	}
	if why := usedByOpenPR(b.Name, open); why != "" {
		return why
	}
	return ""
}

// usedByOpenPR says which open PR uses branch as its head or base, or "".
func usedByOpenPR(branch string, open []forge.PR) string {
	for _, o := range open {
		if (o.HeadBranch == branch && !o.Fork) || o.BaseBranch == branch {
			return fmt.Sprintf("open PR #%d uses it", o.Number)
		}
	}
	return ""
}

// deleteBranch deletes a branch, logs it and counts it. One the forge
// already removed, by its own delete-after-merge between our list and this
// delete, is the normal case: neither an error nor ours to count.
func (e *Engine) deleteBranch(ctx context.Context, p forge.Provider, repo forge.RepoRef, name, why string) error {
	err := p.DeleteBranch(ctx, repo, name)
	if errors.Is(err, forge.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	e.Log.Info("deleted branch", "repo", repo, "branch", name, "why", why)
	e.Q.Count(ctx, "branches_deleted", []string{repo.String()}, 1)
	return nil
}

// sweepBranches deletes the devbot branches every task has left behind on
// each repo the forges list: those whose PR closed more than
// branch_cleanup_grace ago, by the conditions cleanupBranch applies, and
// those that never got a PR, older than orphan_branch_grace. It catches what
// the task path missed: a PR merged or closed after its task ended, a task
// that was cancelled, an attempt that failed before opening a PR.
func (e *Engine) sweepBranches(ctx context.Context) error {
	var errs []error
	for _, n := range e.Forges.Names() {
		if first, err := e.Q.SweepDue(ctx, n, branchSweepEvery); err != nil || !first {
			errs = append(errs, err)
			continue
		}
		p := e.Forges[n]
		repos, err := p.ListRepos(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("list repos on %s: %w", n, err))
			continue
		}
		for _, r := range repos {
			if err := e.sweepRepo(ctx, p, r); err != nil {
				errs = append(errs, fmt.Errorf("sweep branches of %s: %w", r, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) sweepRepo(ctx context.Context, p forge.Provider, repo forge.RepoRef) error {
	rc, err := e.Repos.Get(ctx, repo)
	if err != nil {
		return ignoreNotOptedIn(err)
	}
	if !*rc.BranchCleanup {
		return nil
	}
	bs, err := p.ListBranches(ctx, repo, branchPrefix)
	if err != nil || len(bs) == 0 {
		return err
	}
	open, err := p.ListOpenPRs(ctx, repo)
	if err != nil {
		return err
	}
	var errs []error
	for _, b := range bs {
		if keepBranchName(b.Name) != "" || usedByOpenPR(b.Name, open) != "" {
			continue
		}
		busy, pr, err := e.branchTask(ctx, p, repo, b.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", b.Name, err))
			continue
		}
		switch {
		case busy:
		case pr != nil:
			if pr.ClosedAt.IsZero() || since(pr.ClosedAt) < rc.BranchCleanupGrace {
				continue
			}
			if why := keepPRBranch(p, pr, b, open); why != "" {
				continue
			}
			err = e.deleteBranch(ctx, p, repo, b.Name, fmt.Sprintf("PR #%d closed %s ago", pr.Number, since(pr.ClosedAt).Round(time.Minute)))
		case !b.CommittedAt.IsZero() && since(b.CommittedAt) >= rc.OrphanBranchGrace:
			err = e.deleteBranch(ctx, p, repo, b.Name, fmt.Sprintf("no PR, and no commit for %s", since(b.CommittedAt).Round(time.Minute)))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", b.Name, err))
		}
	}
	return errors.Join(errs...)
}

// branchTask reads the task a devbot/<issue>-<slug> or devbot/pr-<N> branch
// was pushed for.
// busy means the task is in flight: queued or running, or its issue still
// carries a state label, so its own evaluation decides about the branch.
// pr is the latest of the task's PRs with the branch as head, or nil.
func (e *Engine) branchTask(ctx context.Context, p forge.Provider, repo forge.RepoRef, branch string) (busy bool, pr *forge.PR, err error) {
	num, _, _ := strings.Cut(strings.TrimPrefix(branch, branchPrefix), "-")
	// devbot/pr-<N>, a fork's PR carried on a branch of devbot's own, is the
	// task PR #N's carry link names.
	if rest, ok := strings.CutPrefix(branch, branchPrefix+"pr-"); ok {
		if orig, err := strconv.Atoi(rest); err == nil && orig > 0 {
			cs, err := p.ListComments(ctx, forge.ItemRef{Repo: repo, Number: orig})
			if err != nil && !errors.Is(err, forge.ErrNotFound) {
				return false, nil, err
			}
			num = strconv.Itoa(carryLink(cs, e.bots(p)...))
		}
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return false, nil, nil // no task's branch
	}
	ref := forge.ItemRef{Repo: repo, Number: n}
	for _, kind := range []string{queue.KindImplement, queue.KindReview} {
		if q, err := e.Q.IsQueued(ctx, kind, ref.String()); err != nil || q {
			return true, nil, err
		}
		if h, err := e.Q.Holder(ctx, kind, ref.String()); err != nil || h != "" {
			return true, nil, err
		}
	}
	it, err := p.GetItem(ctx, ref)
	if errors.Is(err, forge.ErrNotFound) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if State(it) != "" {
		return true, nil, nil
	}
	cs, err := p.ListComments(ctx, ref)
	if err != nil {
		return false, nil, err
	}
	seen := map[int]bool{}
	for _, s := range statuses(cs, e.bots(p)...) {
		if s.Kind != "pr" || s.PR <= 0 || seen[s.PR] {
			continue
		}
		seen[s.PR] = true
		got, err := p.GetPR(ctx, repo, s.PR)
		if errors.Is(err, forge.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, nil, err
		}
		if got.HeadBranch == branch && (pr == nil || got.Number > pr.Number) {
			pr = &got
		}
	}
	return false, pr, nil
}

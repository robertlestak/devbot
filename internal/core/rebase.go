package core

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/audit"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// Rebasing an approved PR costs a rebase and a review, so devbot only does it
// when it pays: when the PR is about to merge, or a person asks. PRs that
// merge themselves take turns per base branch, oldest approval first, so one
// merge doesn't send every approved PR on the base back at once.

// rebaseBase names the queue a PR waits in: its repo and base branch.
func rebaseBase(pr *forge.PR) string { return rebaseQueue(pr.Repo, pr.BaseBranch) }

func rebaseQueue(repo forge.RepoRef, base string) string { return repo.String() + "|" + base }

// leaveBase takes a task out of the rebase queue of a base its work no
// longer goes into, giving up its turn there, and drops its mark as behind:
// left in, a task that held the turn would hold it for good, being still on
// its way to a merge, only into another base.
func (e *Engine) leaveBase(ctx context.Context, repo forge.RepoRef, base, ref string) error {
	if err := e.Q.LeaveRebaseQueue(ctx, rebaseQueue(repo, base), ref); err != nil {
		return err
	}
	return e.Q.MarkBehind(ctx, repo.String(), ref, false)
}

// rebaseRequest is a person asking for a rebase: the devbot/rebase label, a
// `/rebase` comment, or the rebase connector call.
type rebaseRequest struct {
	by    string // "@login", or a connector caller's name
	label bool
}

// rebaseCommand is the comment that asks for a rebase.
const rebaseCommand = "/rebase"

func isRebaseCommand(body string) bool {
	return strings.TrimSpace(firstLine(body)) == rebaseCommand
}

// rebaseAsked finds a request for a rebase that devbot hasn't acted on yet:
// one made after its last rebase, answer to a request, stop or retry. A
// label from someone who can't direct devbot is taken off again with a
// comment that says why; a comment from them is ignored.
func (t *task) rebaseAsked(ctx context.Context) (*rebaseRequest, error) {
	if err := t.load(ctx); err != nil {
		return nil, err
	}
	if t.it.HasLabel(LRebase) {
		evs, err := t.p.LabelEvents(ctx, t.it.Ref)
		if err != nil {
			return nil, err
		}
		actor := ""
		for i := len(evs) - 1; i >= 0; i-- {
			if evs[i].Label == LRebase && evs[i].Added {
				actor = evs[i].Actor
				break
			}
		}
		if actor == "" {
			actor = t.it.Author // created with the label already on
		}
		ok, why := t.e.authorize(ctx, t.p, t.rc, t.it.Ref.Repo, actor)
		if ok {
			return &rebaseRequest{by: "@" + actor, label: true}, nil
		}
		t.e.auditRefusal(ctx, actor, "rebase", why, audit.Fields{})
		if err := t.p.Comment(ctx, t.it.Ref, fmt.Sprintf("Ignoring `%s` applied by @%s: %s.", LRebase, actor, why)); err != nil {
			return nil, err
		}
		if err := t.p.SetLabels(ctx, t.it.Ref, nil, []string{LRebase}); err != nil {
			return nil, err
		}
	}
	var since time.Time
	for _, s := range t.ss {
		if (s.Kind == "fix-requested" && s.Mode == "rebase") || contains([]string{"rebase-skipped", "needs-human", "reset"}, s.Kind) {
			since = s.CreatedAt
		}
	}
	bots := t.e.bots(t.p)
	for _, c := range t.comments {
		if !c.CreatedAt.After(since) {
			continue
		}
		if forge.HasLogin(bots, c.Author) {
			if s, ok := ParseStatus(c.Body); ok && s.Kind == "rebase-requested" {
				return &rebaseRequest{by: s.Principal}, nil // a name, not a login
			}
			continue
		}
		if isRebaseCommand(c.Body) && t.e.authorized(ctx, t.p, t.rc, t.it.Ref.Repo, c.Author) {
			return &rebaseRequest{by: "@" + c.Author}, nil
		}
	}
	return nil, nil
}

// rebase sends an approved PR back to be rebased onto its base. It costs no
// review round as long as the rebase turns out clean (see fix).
func (t *task) rebase(ctx context.Context, pr *forge.PR, asked *rebaseRequest, trigger string) (bool, error) {
	if err := t.load(ctx); err != nil {
		return false, err
	}
	reason := fmt.Sprintf("The base branch `%s` has moved on since this PR was approved (or it now conflicts). Rebase onto `origin/%s`, resolve any conflicts, make sure the checks still pass, and push.", pr.BaseBranch, pr.BaseBranch)
	if pr.Unrelated {
		reason = RewrittenBaseText(pr.BaseBranch, BuiltOn(t.ss, pr.Number))
	}
	if asked != nil {
		trigger = "asked"
		reason = fmt.Sprintf("%s asked for a rebase. %s", asked.by, reason)
	}
	t.trigger = trigger
	acted, err := t.fix(ctx, pr, "rebase", reason)
	if err != nil {
		return acted, err
	}
	if asked != nil && asked.label {
		return true, t.p.SetLabels(ctx, t.it.Ref, nil, []string{LRebase})
	}
	return acted, nil
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// BuiltOn is the base commit PR pr was last pushed on, as its last "pr"
// status records it, or "" when that status doesn't say: an earlier one's
// may be from before a rebase, and would cut too early.
func BuiltOn(ss []StatusComment, pr int) string {
	for i := len(ss) - 1; i >= 0; i-- {
		if s := ss[i]; s.Kind == "pr" && s.PR == pr {
			if shaRE.MatchString(s.BaseSHA) {
				return s.BaseSHA
			}
			return ""
		}
	}
	return ""
}

// RewrittenBaseText is the rebase asked of a PR that shares no commit with
// base any more, its history having been rewritten: a plain rebase, or a
// merge, would carry the old history back in, so only the PR's own commits
// are replayed, those after cut, the base commit it was last built on.
func RewrittenBaseText(base, cut string) string {
	s := fmt.Sprintf("The history of the base branch `%s` was rewritten (force-pushed) since this PR was built: its branch shares no commit with `origin/%s` any more. Don't `git rebase origin/%s` or merge it, which would carry the old history back in. Replay only this PR's own commits onto the new base: ", base, base, base)
	if cut != "" {
		s += fmt.Sprintf("the ones after `%s`, the commit of `%s` this task last built on (`git rebase --onto origin/%s %s`).", cut, base, base, cut)
	} else {
		s += fmt.Sprintf("devbot has no record of the commit of `%s` this task last built on, so find where the PR's own commits start, and `git rebase --onto origin/%s <the commit before the first of them>`.", base, base)
	}
	return s + fmt.Sprintf(" Resolve any conflicts, make sure the checks still pass, and push. A push that doesn't have `origin/%s` in its history is refused.", base)
}

// rebaseNotNeeded answers a request for a rebase of a PR that is up to date.
// A PR that shares no commit with its base never is: it is stale, and
// rebased.
func (t *task) rebaseNotNeeded(ctx context.Context, pr *forge.PR, asked *rebaseRequest) error {
	body := fmt.Sprintf("PR #%d is up to date with `%s` and doesn't conflict with it, so there's nothing to rebase.\n\n%s",
		pr.Number, pr.BaseBranch, RenderStatus(Status{Kind: "rebase-skipped", PR: pr.Number, SHA: pr.HeadSHA}))
	if err := t.p.Comment(ctx, t.it.Ref, body); err != nil {
		return err
	}
	if asked.label {
		return t.p.SetLabels(ctx, t.it.Ref, nil, []string{LRebase})
	}
	return nil
}

// queueRebase handles an approved, green PR that will merge itself once it
// is up to date with its base. It waits its turn in its base's queue; on its
// turn it is rebased, re-reviewed and merged, and the turn passes on.
func (t *task) queueRebase(ctx context.Context, pr *forge.PR, approvedAt time.Time) (bool, error) {
	base, me := rebaseBase(pr), t.ref()
	if err := t.e.Q.JoinRebaseQueue(ctx, base, me, approvedAt); err != nil {
		return false, err
	}
	ahead, err := t.rebaseTurn(ctx, pr, base)
	if err != nil {
		return false, err
	}
	if ahead == me {
		t.markBehind(ctx, pr, false)
		return t.rebase(ctx, pr, nil, "queue")
	}
	t.markBehind(ctx, pr, true)
	st := Status{Kind: "queued", PR: pr.Number, SHA: pr.HeadSHA}
	for i := len(t.ss) - 1; i >= 0 && t.ss[i].Kind != "reset"; i-- {
		if t.ss[i].key() == st.key() && t.state == LApproved {
			return false, nil // already said so
		}
	}
	behind := fmt.Sprintf("has fallen behind `%s`", pr.BaseBranch)
	switch {
	case pr.Unrelated:
		behind = fmt.Sprintf("shares no history with `%s` any more, whose history was rewritten", pr.BaseBranch)
	case !pr.Mergeable:
		behind = fmt.Sprintf("conflicts with `%s`", pr.BaseBranch)
	}
	if _, err := t.postStatus(ctx, fmt.Sprintf("PR #%d is approved and its checks pass, but it %s. It's queued to be rebased and merged: devbot does that one PR at a time per base branch, oldest approval first, and %s goes first.",
		pr.Number, behind, itemNumber(ahead)), st); err != nil {
		return false, err
	}
	return true, t.e.transitionOnce(ctx, t.p, t.it, LApproved, "task.approved", fmt.Sprintf("PR #%d approved, queued behind %s to be rebased and merged", pr.Number, itemNumber(ahead)), "", t.onceKey(st))
}

// itemNumber shortens a task ref to "#n", for a comment on the same repo.
func itemNumber(ref string) string {
	if r, err := forge.ParseItem(ref); err == nil {
		return fmt.Sprintf("#%d", r.Number)
	}
	return ref
}

// rebaseTurn returns the task whose turn comes before this one's in base's
// queue: the one holding the turn, or the first in line when nobody does.
// When that is this task, the turn is now its own. A task ahead that has
// left devbot's hands (stopped, closed, cancelled) is dropped on the way, so
// it can't hold up the queue.
func (t *task) rebaseTurn(ctx context.Context, pr *forge.PR, base string) (string, error) {
	q, me := t.e.Q, t.ref()
	ahead := ""
	for range 5 {
		var err error
		if ahead, err = q.RebaseTurnHolder(ctx, base); err != nil {
			return "", err
		}
		if ahead == "" {
			order, err := q.RebaseQueue(ctx, base)
			if err != nil || len(order) == 0 {
				return "", err
			}
			if ahead = order[0]; ahead == me {
				if err := t.recoverRebaseQueue(ctx, pr, base); err != nil {
					return "", err
				}
				if ahead, err = q.RebaseTurn(ctx, base, me, true); err != nil {
					return "", err
				}
				if ahead == "" {
					continue // someone turned up ahead of it
				}
			}
		}
		if ahead == me {
			return me, nil
		}
		ok, err := t.e.stillQueued(ctx, ahead)
		if err != nil || ok {
			return ahead, err
		}
		if err := q.LeaveRebaseQueue(ctx, base, ahead); err != nil {
			return "", err
		}
	}
	return ahead, nil
}

// stillQueued reports whether a task ahead in a rebase queue is still on its
// way to a merge.
func (e *Engine) stillQueued(ctx context.Context, ref string) (bool, error) {
	r, err := forge.ParseItem(ref)
	if err != nil {
		return false, nil
	}
	p, err := e.provider(r.Repo)
	if err != nil {
		return false, err
	}
	it, err := p.GetItem(ctx, r)
	if errors.Is(err, forge.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch State(it) {
	case LReady, LWorking, LChanges, LInReview, LApproved:
		return it.State == "open", nil
	}
	return false, nil
}

// recoverRebaseQueue makes sure, before a task takes the turn, that the
// queue in Redis holds every task that should go first. After Redis was
// wiped, tasks re-join only as they are evaluated, so the forge is asked: a
// rebase already in flight on the same base takes the turn back, and a task
// whose last word was that it is queued re-joins at its approval time.
func (t *task) recoverRebaseQueue(ctx context.Context, pr *forge.PR, base string) error {
	prs, err := t.p.ListOpenPRs(ctx, pr.Repo)
	if err != nil {
		return err
	}
	_, reviewer := t.p.BotUsers()
	for _, o := range prs {
		m, ok := ParsePRMeta(o.Body)
		if !ok || o.Number == pr.Number || o.BaseBranch != pr.BaseBranch || m.Issue == t.it.Ref.Number {
			continue
		}
		ref := forge.ItemRef{Repo: pr.Repo, Number: m.Issue}
		it, err := t.p.GetItem(ctx, ref)
		if errors.Is(err, forge.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		state := State(it)
		if it.State != "open" || !contains([]string{LReady, LWorking, LChanges, LInReview, LApproved}, state) {
			continue
		}
		cs, err := t.p.ListComments(ctx, ref)
		if err != nil {
			return err
		}
		s, ok := lastStatus(statuses(cs, t.e.bots(t.p)...), "fix-requested", "queued", "approved", "needs-human", "reset")
		switch {
		case !ok || s.PR != o.Number:
		case s.Kind == "fix-requested" && s.Mode == "rebase" && state != LApproved:
			_, err := t.e.Q.RebaseTurn(ctx, base, ref.String(), false)
			return err
		case s.Kind == "queued" && s.SHA == o.HeadSHA && state == LApproved:
			rs, err := t.p.Reviews(ctx, o.Repo, o.Number)
			if err != nil {
				return err
			}
			for _, r := range rs {
				if !r.Dismissed && r.CommitSHA == o.HeadSHA && forge.SameLogin(r.Author, reviewer) && r.Verdict == forge.VerdictApprove {
					if err := t.e.Q.JoinRebaseQueue(ctx, base, ref.String(), r.SubmittedAt); err != nil {
						return err
					}
					break
				}
			}
		}
	}
	return nil
}

// landed passes the turn on once a PR has merged, by evaluating the task
// next in line, and the tasks waiting on this one, now rather than at the
// next reconcile.
func (t *task) landed(ctx context.Context, pr *forge.PR) {
	defer t.wakeWaiters(ctx)
	base := rebaseBase(pr)
	t.markBehind(ctx, pr, false)
	if err := t.e.Q.LeaveRebaseQueue(ctx, base, t.ref()); err != nil {
		t.e.Log.Warn("leaving the rebase queue", "task", t.it.Ref, "err", err)
		return
	}
	order, err := t.e.Q.RebaseQueue(ctx, base)
	if err != nil || len(order) == 0 {
		return
	}
	next, err := forge.ParseItem(order[0])
	if err != nil {
		return
	}
	if _, err := t.e.EvaluateItem(ctx, next, t.src); err != nil {
		t.e.Log.Warn("evaluating the next in the rebase queue", "task", next, "err", err)
	}
}

// markBehind keeps the count of approved PRs waiting behind their base. It
// is only a metric, so a failure is logged.
func (t *task) markBehind(ctx context.Context, pr *forge.PR, behind bool) {
	if err := t.e.Q.MarkBehind(ctx, pr.Repo.String(), t.ref(), behind); err != nil {
		t.e.Log.Warn("marking a PR behind its base", "task", t.it.Ref, "err", err)
	}
}

// rebaseAdvice is what the approval comment says about the PR being up to
// date with its base, and how to ask for a rebase when it matters.
func rebaseAdvice(pr *forge.PR, prot forge.Protection) string {
	ask := fmt.Sprintf("To have devbot rebase it, add `%s` to this issue, comment `%s`, or use the `rebase` connector call.", LRebase, rebaseCommand)
	switch {
	case pr.Unrelated:
		return fmt.Sprintf("\n\nIt shares no history with `%s` any more: `%s` was force-pushed to a history of its own. devbot never merges it like this, so it needs a rebase that replays only its own commits before it can merge. %s", pr.BaseBranch, pr.BaseBranch, ask)
	case !pr.Mergeable:
		return fmt.Sprintf("\n\nIt conflicts with `%s`, so it needs a rebase before it can merge. %s", pr.BaseBranch, ask)
	case pr.Behind && prot.UpToDate:
		return fmt.Sprintf("\n\nIt's behind `%s`, and the repo's rules on `%s` only merge a branch that is up to date, so it needs a rebase before it can merge. %s", pr.BaseBranch, pr.BaseBranch, ask)
	case pr.Behind:
		return fmt.Sprintf("\n\nIt's behind `%s`. The forge can merge it as it is; devbot leaves it alone unless someone asks. %s", pr.BaseBranch, ask)
	case prot.UpToDate:
		return fmt.Sprintf("\n\nThe repo's rules on `%s` only merge a branch that is up to date, so if `%s` moves on first, it will need a rebase before it can merge. %s", pr.BaseBranch, pr.BaseBranch, ask)
	}
	return ""
}

// rebaseNote is what the task's status says about an approved PR that is
// waiting behind its base.
func (e *Engine) rebaseNote(ctx context.Context, it forge.Item, pr *forge.PR) string {
	if State(it) != LApproved || pr == nil || pr.State != "open" || (pr.Mergeable && !pr.Behind && !pr.Unrelated) {
		return ""
	}
	note := fmt.Sprintf("behind `%s`", pr.BaseBranch)
	switch {
	case pr.Unrelated:
		note = fmt.Sprintf("shares no history with `%s`, which was rewritten", pr.BaseBranch)
	case !pr.Mergeable:
		note = fmt.Sprintf("conflicts with `%s`", pr.BaseBranch)
	}
	order, err := e.Q.RebaseQueue(ctx, rebaseBase(pr))
	if err != nil || !contains(order, it.Ref.String()) {
		return note
	}
	ahead, _ := e.Q.RebaseTurnHolder(ctx, rebaseBase(pr))
	if ahead == "" {
		ahead = order[0]
	}
	if ahead == it.Ref.String() {
		return note + ", next to be rebased"
	}
	return note + ", queued behind " + itemNumber(ahead)
}

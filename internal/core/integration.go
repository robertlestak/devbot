package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// Each child of a parent task passes its own repo's CI, against the other
// repos as they are, not as its siblings' PRs leave them. A parent whose
// repo defines `integration` checks them together:
//
//   - once every child with an open PR is approved, its checks green, on
//     its current head, the parent's evaluation queues an integrate job for
//     exactly those heads (and those of children already merged), and does
//     so again whenever one of them moves;
//   - the job (harness.Integrate) runs the command over a workspace with
//     each child at its head, and records the result on the parent, as an
//     "integration" status naming the heads, and on each head as the
//     devbot/integration commit status;
//   - a child's merge gate holds its merge until a result for the current
//     heads passed. A failure stops the parent at needs-human, for a person
//     to send the child that broke it back.
//
// Everything is read from the forge each time: the heads from the children's
// PRs, the result from the parent's comments.

// IntegrationContext is the commit status the integration check posts on
// each child's head. The children's own gates leave it out of their CI: it
// is the parent's to act on.
const IntegrationContext = "devbot/integration"

// What an "integration" status on a parent says of the heads it names.
const (
	IntegrationStarted = "started"
	IntegrationPassed  = "passed"
	IntegrationFailed  = "failed"
	// IntegrationError is a run that couldn't finish: a clone that failed,
	// say. It is tried again, up to integration.retries times.
	IntegrationError = "error"
	// IntegrationRetry is a retry of the parent: what was recorded for the
	// heads before it counts for nothing.
	IntegrationRetry = "retry"
)

// Head names a child task's PR head as integration statuses and jobs
// record it.
func Head(task forge.ItemRef, sha string) string { return task.String() + "@" + sha }

// IntegrationHead is one child's PR head that a check covers.
type IntegrationHead struct {
	Task forge.ItemRef
	PR   forge.PR
}

func (h IntegrationHead) String() string { return Head(h.Task, h.PR.HeadSHA) }

// Integration is where a parent's integration check stands for its
// children's current heads.
type Integration struct {
	// Heads are the PR heads of every child with an open or merged PR, by
	// task.
	Heads []IntegrationHead
	// Bare are the repos of children still without a PR, which the check
	// has at their default branches.
	Bare []forge.RepoRef
	// Waiting are the children with an open PR that isn't approved with
	// green checks on its head yet.
	Waiting []forge.ItemRef
	// Result is the last passed or failed result for exactly these heads;
	// "" for none.
	Result string
	// Started is a run for these heads that hasn't said how it ended.
	Started *statusComment
	// Errors are the runs for these heads that couldn't finish.
	Errors int
}

// Keys are the heads as statuses and jobs name them, sorted.
func (in Integration) Keys() []string {
	out := make([]string, len(in.Heads))
	for i, h := range in.Heads {
		out[i] = h.String()
	}
	slices.Sort(out)
	return out
}

// Due reports whether the check should run for these heads: every child
// with an open PR is ready, at least one has one, and nothing has been
// recorded for them.
func (in Integration) Due() bool {
	open := slices.ContainsFunc(in.Heads, func(h IntegrationHead) bool { return !h.PR.Merged })
	return open && len(in.Waiting) == 0 && in.Result == ""
}

// GaveUp reports whether runs that couldn't finish have used up retries.
func (in Integration) GaveUp(retries int) bool { return in.Errors > retries }

// integrationOf is where the check stands for kids, from the parent's
// statuses ss.
func integrationOf(kids []child, ss []statusComment) Integration {
	var in Integration
	for _, k := range kids {
		if k.pr == nil && k.state != childCancelled && k.state != childGone && !slices.Contains(in.Bare, k.ref.Repo) {
			in.Bare = append(in.Bare, k.ref.Repo)
		}
		if k.pr == nil || (k.pr.State != "open" && !k.pr.Merged) {
			continue
		}
		in.Heads = append(in.Heads, IntegrationHead{Task: k.ref, PR: *k.pr})
		if k.pr.Merged {
			continue
		}
		if a, ok := lastStatus(k.ss, "approved"); k.state != strings.TrimPrefix(LApproved, "devbot/") || !ok || a.PR != k.pr.Number || a.SHA != k.pr.HeadSHA {
			in.Waiting = append(in.Waiting, k.ref)
		}
	}
	slices.SortFunc(in.Heads, func(a, b IntegrationHead) int { return strings.Compare(a.Task.String(), b.Task.String()) })
	keys := in.Keys()
	for _, s := range ss {
		if s.Kind != "integration" || !slices.Equal(s.Heads, keys) {
			continue
		}
		switch s.Mode {
		case IntegrationRetry:
			in.Result, in.Started, in.Errors = "", nil, 0
		case IntegrationStarted:
			in.Started = &s
		case IntegrationError:
			in.Started = nil
			in.Errors++
		case IntegrationPassed, IntegrationFailed:
			in.Result, in.Started = s.Mode, nil
		}
	}
	return in
}

// integrationFor is the integration check of parent, an open parent task:
// nil when its repo's policy defines none.
func (e *Engine) integrationFor(ctx context.Context, parent forge.Item) (*config.Integration, error) {
	if parent.State != "open" {
		return nil, nil
	}
	rc, err := e.Repos.Get(ctx, parent.Ref.Repo)
	if errors.Is(err, config.ErrNotOptedIn) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !rc.Integration.On() {
		return nil, nil
	}
	return &rc.Integration, nil
}

// integrate queues the parent's integration check when it is due for its
// children's current heads, unless runs that couldn't finish have used up
// their retries: the parent waits for a person then. A run that died
// without a word (its pod went away) counts as one that couldn't finish
// once it is stale.
func (t *task) integrate(ctx context.Context, kids []child) (bool, error) {
	if t.rc == nil || !t.rc.Integration.On() {
		return false, nil
	}
	in := integrationOf(kids, t.ss)
	if !in.Due() {
		return false, nil
	}
	busy, err := t.busy(ctx, queue.KindIntegrate)
	if err != nil || busy {
		return false, err
	}
	if in.Started != nil {
		if since(in.Started.CreatedAt) < staleWorking {
			return false, nil // just started; the lease may not be visible yet
		}
		if err := t.e.RecordIntegration(ctx, t.it, in.Keys(), IntegrationError,
			"The integration check's run stopped without finishing (its pod went away)."); err != nil {
			return false, err
		}
		t.comments = nil
		if err := t.load(ctx); err != nil {
			return true, err
		}
		if in = integrationOf(kids, t.ss); in.GaveUp(*t.rc.Integration.Retries) {
			t.e.emit(ctx, t.it, "parent.needs_human", ParentNeedsHuman, "the integration check couldn't run", "", "integration|"+strings.Join(in.Keys(), ","))
			return true, nil
		}
	}
	if in.GaveUp(*t.rc.Integration.Retries) {
		return false, nil
	}
	return t.enqueue(ctx, queue.Job{Kind: queue.KindIntegrate, Ref: t.ref(), Heads: in.Keys()})
}

// ParentIntegration is where the integration check of parent, a parent task,
// stands for its children's current heads, with the repo's check; nil when
// its repo defines none, or it isn't an open parent.
func (e *Engine) ParentIntegration(ctx context.Context, parent forge.Item, comments []forge.Comment) (*config.Integration, Integration, error) {
	p, err := e.provider(parent.Ref.Repo)
	if err != nil {
		return nil, Integration{}, err
	}
	if !isParent(parent, e.meta(ctx, parent, comments)) {
		return nil, Integration{}, nil
	}
	ss := statuses(comments, e.bots(p)...)
	cfg, err := e.integrationFor(ctx, parent)
	if err != nil || cfg == nil {
		return nil, Integration{}, err
	}
	kids, err := e.children(ctx, parent, ss)
	if err != nil {
		return nil, Integration{}, err
	}
	return cfg, integrationOf(kids, ss), nil
}

// RecordIntegration posts an integration status for heads on parent, with
// text, which may quote the check's output: its markers are removed first.
// A failure tells whoever follows the parent that it needs a person.
func (e *Engine) RecordIntegration(ctx context.Context, parent forge.Item, heads []string, mode, text string) error {
	p, err := e.provider(parent.Ref.Repo)
	if err != nil {
		return err
	}
	if err := p.Comment(ctx, parent.Ref, Defang(text)+"\n\n"+RenderStatus(Status{Kind: "integration", Mode: mode, Heads: heads})); err != nil {
		return err
	}
	if mode == IntegrationFailed {
		e.emit(ctx, parent, "parent.needs_human", ParentNeedsHuman, "the integration check failed", "", "integration|"+strings.Join(heads, ","))
	}
	return nil
}

// integrationNote is what a person following a parent of repo own should
// know of its integration check, naming none of the children in hidden,
// and whether it holds the parent at needs-human.
func integrationNote(cfg *config.Integration, in Integration, own forge.RepoRef, hidden map[forge.ItemRef]bool) (string, bool) {
	switch {
	case cfg == nil:
		return "", false
	case in.Result == IntegrationFailed:
		return "the integration check failed on the children's current heads: send the child that broke it back (edit_task, or a review on its PR), or retry the parent task to run it again", true
	case in.GaveUp(*cfg.Retries):
		return fmt.Sprintf("the integration check couldn't run on the children's current heads, %d times: retry the parent task to run it again", in.Errors), true
	case in.Result == IntegrationPassed:
		return "the integration check passed on the children's current heads", false
	case len(in.Waiting) > 0:
		return "the integration check waits for " + names(own, in.Waiting, hidden) + " to be approved with green checks", false
	case in.Due():
		return "the integration check is queued or running on the children's current heads", false
	}
	return "", false
}

// integrationGate is why the merge of pr, this child task's PR, waits on
// its parent's integration check, or "" when it doesn't: its parent's repo
// defines none, or a result for every child's current head passed.
func (t *task) integrationGate(ctx context.Context, pr *forge.PR) (string, error) {
	ref, err := forge.ParseItem(t.e.meta(ctx, t.it, t.comments).Parent)
	if err != nil {
		return "", nil
	}
	pp, err := t.e.provider(ref.Repo)
	if err != nil {
		return "", nil
	}
	parent, err := pp.GetItem(ctx, ref)
	if errors.Is(err, forge.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	cs, err := pp.ListComments(ctx, ref)
	if err != nil {
		return "", err
	}
	cfg, in, err := t.e.ParentIntegration(ctx, parent, cs)
	if err != nil || cfg == nil {
		return "", err
	}
	i := slices.IndexFunc(in.Heads, func(h IntegrationHead) bool { return h.Task == t.it.Ref })
	if i < 0 {
		return "", nil // not a child the parent lists, whatever its block says
	}
	name := RefText(t.it.Ref.Repo, ref)
	switch {
	case in.Heads[i].PR.Number != pr.Number || in.Heads[i].PR.HeadSHA != pr.HeadSHA:
		// Read a moment apart: the next evaluation sees them agree.
		return fmt.Sprintf("The integration check of parent task %s has to pass on every child's current head first.", name), nil
	case in.Result == IntegrationPassed:
		return "", nil
	case in.Result == IntegrationFailed:
		return fmt.Sprintf("The integration check of parent task %s failed on its children's current heads, so this PR doesn't merge until one of them moves and the check passes. What it said is on %s.", name, parent.URL), nil
	case in.GaveUp(*cfg.Retries):
		return fmt.Sprintf("The integration check of parent task %s couldn't run on its children's current heads, so this PR doesn't merge until it does: retry the parent task.", name), nil
	case len(in.Waiting) > 0:
		return fmt.Sprintf("The integration check of parent task %s has to pass on every child's current head first. It waits for %s to be approved with green checks.", name, names(t.it.Ref.Repo, in.Waiting, nil)), nil
	}
	return fmt.Sprintf("The integration check of parent task %s has to pass on every child's current head first. It is running, or about to.", name), nil
}

// withoutIntegration takes the integration check's commit status out of a
// child's CI: it is the parent's to act on, and a failure there is no fix
// round for this child.
func withoutIntegration(checks forge.CheckState, details []forge.CheckDetail) (forge.CheckState, []forge.CheckDetail) {
	if !slices.ContainsFunc(details, func(d forge.CheckDetail) bool { return d.Context == IntegrationContext }) {
		return checks, details
	}
	rest := slices.DeleteFunc(slices.Clone(details), func(d forge.CheckDetail) bool { return d.Context == IntegrationContext })
	switch {
	case len(rest) == 0:
		return forge.ChecksNone, nil
	case slices.ContainsFunc(rest, func(d forge.CheckDetail) bool { return d.Pending }):
		return forge.ChecksPending, rest
	case slices.ContainsFunc(rest, func(d forge.CheckDetail) bool { return d.Failed }):
		return forge.ChecksFailure, rest
	}
	return forge.ChecksSuccess, rest
}

// names are refs as an issue of own names them, but those in hidden.
func names(own forge.RepoRef, refs []forge.ItemRef, hidden map[forge.ItemRef]bool) string {
	var out []string
	for _, r := range refs {
		if !hidden[r] {
			out = append(out, RefText(own, r))
		}
	}
	if n := len(refs) - len(out); n > 0 {
		out = append(out, fmt.Sprintf("%d in repos you can't read", n))
	}
	return strings.Join(out, ", ")
}

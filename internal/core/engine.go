// Package core is devbot's state machine. It is level-triggered: every entry
// point (a webhook, a reconcile pass, a job finishing, a connector call)
// re-reads the issue and its PR from the forge and moves it one step towards
// where it should be. Nothing depends on which event arrived or in what order,
// which is what lets webhooks be lossy and Redis be disposable.
package core

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"git.shdw.tech/rob/devbot/internal/audit"
	"git.shdw.tech/rob/devbot/internal/changeset"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// staleWorking is how long an issue may sit in devbot/working with no lease
// before it is treated as abandoned. Longer than a lease TTL, so a pod that is
// merely between renewals is never reset.
const staleWorking = 4 * time.Minute

// checksGrace is how long a freshly pushed head may have no checks reported
// (or none of those the repo's own rules require) before the gate treats the
// absence as "these checks don't run on PRs".
const checksGrace = 10 * time.Minute

type Engine struct {
	Forges forge.Registry
	Q      *queue.Store
	Cfg    *config.Config
	Repos  *config.RepoCache
	Log    *slog.Logger
	// Changes lists what a PR changes against its base, for the protected-paths
	// gate. Nil fetches it with git from the forge, into the temp dir.
	Changes func(ctx context.Context, p forge.Provider, pr forge.PR) ([]changeset.Change, error)
	// ForkReachable reports whether the implementer's git credential reaches
	// a fork's PR's head branch, which adoption asks before carrying the PR
	// there. Nil asks git (forkReachable).
	ForkReachable func(ctx context.Context, p forge.Provider, pr forge.PR) (bool, error)
	// Audit records transitions and refusals. Forge writes are audited by
	// the providers in Forges (audit.Forge). Nil records nothing.
	Audit *audit.Logger

	ensured  sync.Map    // repo -> true once labels exist
	metas    metaCache   // task ref -> Meta, once found: devbot posts it again only to move a carried PR (SupersedeCarry)
	readable readCache   // for CanRead
	details  detailCache // for ListTasksDetailed
	changes  changeCache // for changeSet
	bases    baseCache   // for events
}

// Source says who asked for an evaluation, for logs and metrics.
type Source string

const (
	SrcWebhook   Source = "webhook"
	SrcReconcile Source = "reconcile"
	SrcJob       Source = "job"
	SrcAPI       Source = "api"
)

func (e *Engine) provider(r forge.RepoRef) (forge.Provider, error) { return e.Forges.For(r) }

// Count bumps a Prometheus counter. A failed increment is logged rather than
// returned: losing one metric sample must not fail the work it measures.
func (e *Engine) Count(ctx context.Context, name string, labels []string, by float64) {
	if err := e.Q.Count(ctx, name, labels, by); err != nil {
		e.Log.Warn("counter update failed", "counter", name, "err", err)
	}
}

func (e *Engine) bots(p forge.Provider) []string {
	impl, rev := p.BotUsers()
	return []string{impl, rev}
}

func (e *Engine) authorized(ctx context.Context, p forge.Provider, rc *config.Repo, repo forge.RepoRef, actor string) bool {
	ok, _ := e.authorize(ctx, p, rc, repo, actor)
	return ok
}

// reviewCounts reports whether a change request from actor sends a PR back
// and holds its merge: an authorized person's does, and so does one from a
// login on the repo's trusted_reviewers, which needs no forge permission.
// Being trusted counts for that alone, never for directing the work.
func (e *Engine) reviewCounts(ctx context.Context, p forge.Provider, rc *config.Repo, repo forge.RepoRef, actor string) bool {
	return forge.HasLogin(rc.TrustedReviewers, actor) || e.authorized(ctx, p, rc, repo, actor)
}

// authorize: the bots themselves (agent identities are authorized), or anyone
// the forge gives the install's minimum permission who is also on the repo's
// authorized_users when it has one. The list only narrows. When the answer is
// no, why says which rule failed, for the comment that explains it.
func (e *Engine) authorize(ctx context.Context, p forge.Provider, rc *config.Repo, repo forge.RepoRef, actor string) (ok bool, why string) {
	if actor == "" {
		return false, "nobody applied it"
	}
	if forge.HasLogin(e.bots(p), actor) {
		return true, ""
	}
	if len(rc.AuthorizedUsers) > 0 && !forge.HasLogin(rc.AuthorizedUsers, actor) {
		return false, fmt.Sprintf("@%s isn't on this repo's `authorized_users` in `%s`", actor, config.RepoFile)
	}
	min := e.Cfg.Authorization.Min
	ok, err := p.Authorized(ctx, repo, actor, min)
	if err != nil {
		// Failing closed is right, but it's a setup problem to alert on,
		// not a decision about actor.
		e.Log.Warn("authorization lookup failed", "repo", repo, "actor", actor, "err", err)
		e.Count(ctx, "authz_lookup_errors", []string{repo.String(), p.Name()}, 1)
		var fe *forge.LookupForbiddenError
		if errors.As(err, &fe) {
			return false, fmt.Sprintf("devbot couldn't check @%s's permission on this repo: %s", actor, fe.Need)
		}
		return false, fmt.Sprintf("devbot couldn't check @%s's permission on this repo (the forge returned an error; see devbot's logs)", actor)
	}
	if !ok {
		return false, fmt.Sprintf("@%s doesn't have %s access to this repo", actor, min)
	}
	return true, ""
}

// EnsureLabels creates the devbot/* labels on a repo once per process.
func (e *Engine) EnsureLabels(ctx context.Context, repo forge.RepoRef) error {
	if _, ok := e.ensured.Load(repo.String()); ok {
		return nil
	}
	p, err := e.provider(repo)
	if err != nil {
		return err
	}
	if err := p.EnsureLabels(ctx, repo, LabelDefs); err != nil {
		return err
	}
	e.ensured.Store(repo.String(), true)
	return nil
}

// withLabels runs fn, which puts devbot labels on an issue in repo, once
// they exist. A label someone deleted since EnsureLabels made it is gone
// although this process thinks otherwise, so on a not-found the repo's
// labels are ensured afresh and fn tried once more.
func (e *Engine) withLabels(ctx context.Context, repo forge.RepoRef, fn func() error) error {
	if err := e.EnsureLabels(ctx, repo); err != nil {
		return err
	}
	err := fn()
	if !errors.Is(err, forge.ErrNotFound) {
		return err
	}
	e.ensured.Delete(repo.String())
	if err := e.EnsureLabels(ctx, repo); err != nil {
		return err
	}
	return fn()
}

// HandleEvent maps a normalised webhook event onto evaluations. One that
// can't concern devbot is dropped before anything is read from the forge:
// on an org-wide install most deliveries are about repos and PRs devbot has
// nothing to do with, and reading each would spend the rate limit the
// tasks need. Reconcile finds whatever that drops wrongly. One that may
// concern devbot but no task stands behind (a comment that reads like a
// command, on an item with no devbot label) is low priority
// (forge.LowPriority): it waits while the forge's quota is low.
func (e *Engine) HandleEvent(ctx context.Context, ev forge.Event) error {
	switch ev.Kind {
	case forge.EventItem, forge.EventPR:
		ok, tied, err := e.concerns(ctx, ev)
		if err != nil || !ok {
			return err
		}
		if !tied {
			ctx = forge.LowPriority(ctx)
		}
		if ev.Kind == forge.EventPR {
			return e.evaluatePRNumber(ctx, ev.Repo, ev.Number)
		}
		_, err = e.EvaluateItem(ctx, forge.ItemRef{Repo: ev.Repo, Number: ev.Number}, SrcWebhook)
		return err
	case forge.EventStatus:
		// Checks matter only on a task's PR, so the head is looked up among
		// those, not in the repo's open PRs.
		n, err := e.Q.TrackedHead(ctx, ev.Repo.String(), ev.SHA)
		if err != nil {
			return err
		}
		if n == 0 {
			e.Count(ctx, "webhook_events_skipped", []string{string(ev.Kind)}, 1)
			return nil
		}
		return e.evaluatePRNumber(ctx, ev.Repo, n)
	case forge.EventDefaultBranch:
		if len(ev.Paths) == 0 || contains(ev.Paths, config.RepoFile) {
			// Bumped first: the other replicas and job pods drop theirs on
			// their next read, not when their TTL runs out.
			if err := e.Q.BumpRepoConfig(ctx, ev.Repo.String()); err != nil {
				e.Log.Warn("marking the repo config stale", "repo", ev.Repo, "err", err)
			}
			e.Repos.Invalidate(ev.Repo)
		}
	case forge.EventTag:
		// Only tasks held for a tag of the repo care, and are indexed by it.
		return e.wakeTagWaiters(ctx, ev.Repo)
	}
	return nil
}

// concerns reports whether an issue or PR event may start, hand over or
// move a task, from what the delivery says and the PRs evaluations have
// tracked, and counts the ones it says no to. Only a state label makes an
// issue a task, and only devbot/ready or a task's link makes a PR devbot's,
// so an evaluation of an item with no devbot label, on no devbot branch and
// not a tracked PR, finds nothing to do. A comment that reads like a
// command is let through anyway, at the cost of one read, so a command
// never rests on the index alone. tied reports whether a task stands
// behind the event: a devbot label or branch, or a tracked PR.
func (e *Engine) concerns(ctx context.Context, ev forge.Event) (ok, tied bool, err error) {
	if !ev.Facts {
		return true, false, nil
	}
	if slices.ContainsFunc(ev.Labels, func(l string) bool { return strings.HasPrefix(l, "devbot/") }) ||
		strings.HasPrefix(ev.Head, "devbot/") {
		return true, true, nil
	}
	if ev.Kind == forge.EventPR {
		tracked, err := e.Q.TrackedPR(ctx, ev.Repo.String(), ev.Number)
		if err != nil || tracked {
			return tracked, tracked, err
		}
	}
	if p, err := e.provider(ev.Repo); err == nil && couldBeCommand(ev.Comment, e.bots(p)) {
		return true, false, nil
	}
	e.Count(ctx, "webhook_events_skipped", []string{string(ev.Kind)}, 1)
	return false, false, nil
}

// couldBeCommand reports whether a comment may be one devbot acts on: a
// line that starts with one of its commands, or a mention of a bot.
func couldBeCommand(body string, bots []string) bool {
	if body == "" {
		return false
	}
	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		for _, c := range []string{baseCommand, rebaseCommand, waitCommand} {
			if strings.HasPrefix(line, c) {
				return true
			}
		}
	}
	lower := strings.ToLower(body)
	for _, b := range bots {
		if b = strings.TrimSuffix(strings.ToLower(b), "[bot]"); b != "" && strings.Contains(lower, "@"+b) {
			return true
		}
	}
	return false
}

func ignoreNotOptedIn(err error) error {
	if errors.Is(err, config.ErrNotOptedIn) {
		return nil
	}
	return err
}

// evaluatePRNumber finds the issue a PR belongs to and evaluates that, or
// adopts a PR someone handed devbot (see evaluatePR).
func (e *Engine) evaluatePRNumber(ctx context.Context, repo forge.RepoRef, number int) error {
	p, err := e.provider(repo)
	if err != nil {
		return err
	}
	if _, err := e.Repos.Get(ctx, repo); err != nil {
		return ignoreNotOptedIn(err)
	}
	pr, err := p.GetPR(ctx, repo, number)
	if errors.Is(err, forge.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return e.evaluatePR(ctx, p, pr)
}

// EvaluateItem moves one issue a step towards where it should be. It reports
// whether it changed anything.
//
// Evaluations of one task are serialised across every process by a Redis
// lock: two that both read the labels before either wrote would both comment
// and transition. A caller that finds the lock held leaves the task marked
// dirty and returns; the holder evaluates once more before letting go, so
// however many triggers arrive meanwhile cost one extra evaluation and none
// is lost.
//
// Each pass runs under a deadline short of the lock's TTL, which is never
// renewed: forge calls that wait out a rate limit could otherwise keep a
// pass going after the lock lapsed and another evaluation took it.
func (e *Engine) EvaluateItem(ctx context.Context, ref forge.ItemRef, src Source) (bool, error) {
	return e.serialised(ctx, ref.String(), func(ctx context.Context) (bool, error) { return e.evaluateItem(ctx, ref, src) })
}

// serialised runs pass under key's evaluation lock, as EvaluateItem
// describes, once more for every request that came in while it ran.
func (e *Engine) serialised(ctx context.Context, key string, pass func(context.Context) (bool, error)) (bool, error) {
	owner := rand.Text()
	ok, err := e.Q.LockEval(ctx, key, owner, e.Cfg.EvalLockTTL)
	if err != nil || !ok {
		return false, err
	}
	acted := false
	var errs []error
	for {
		pctx, cancel := context.WithTimeout(ctx, e.evalBudget())
		did, err := pass(pctx)
		cancel()
		acted = acted || did
		again, uerr := e.Q.UnlockEval(context.WithoutCancel(ctx), key, owner)
		errs = append(errs, err, uerr)
		// The release consumed any request that arrived meanwhile, so it is
		// ours to serve even when this pass failed: a transient forge error
		// must not leave a webhook waiting for reconcile.
		if uerr != nil || !again {
			return acted, errors.Join(errs...)
		}
		// Someone asked while we held it. Take it back; if another caller got
		// in first, its evaluation covers the request.
		if ok, err := e.Q.LockEval(ctx, key, owner, e.Cfg.EvalLockTTL); err != nil || !ok {
			return acted, errors.Join(append(errs, err)...)
		}
	}
}

// evalBudget is how long one pass of EvaluateItem may run: a fifth of the
// lock's TTL is left for the release, and for Redis's clock and this
// process's to disagree.
func (e *Engine) evalBudget() time.Duration { return e.Cfg.EvalLockTTL - e.Cfg.EvalLockTTL/5 }

// connectorLockWait is how long a connector call waits for a task's lock
// before asking its caller to try again.
const connectorLockWait = 5 * time.Second

// LockTask takes ref's evaluation lock, waiting up to wait for whoever has
// it, for a change to the task that an evaluation or a job starting must
// not see half made. unlock lets it go and reports whether an evaluation was
// asked for meanwhile, which is then the caller's to serve or pass on.
func (e *Engine) LockTask(ctx context.Context, ref forge.ItemRef, wait time.Duration) (unlock func() (bool, error), err error) {
	key, owner := ref.String(), rand.Text()
	deadline := time.Now().Add(wait)
	for {
		ok, err := e.Q.LockEval(ctx, key, owner, e.Cfg.EvalLockTTL)
		if err != nil {
			return nil, err
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s is being evaluated: try again", ref)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return func() (bool, error) { return e.Q.UnlockEval(context.WithoutCancel(ctx), key, owner) }, nil
}

func (e *Engine) evaluateItem(ctx context.Context, ref forge.ItemRef, src Source) (bool, error) {
	p, err := e.provider(ref.Repo)
	if err != nil {
		return false, err
	}
	rc, err := e.Repos.Get(ctx, ref.Repo)
	if err != nil {
		return false, ignoreNotOptedIn(err)
	}
	it, err := p.GetItem(ctx, ref)
	if errors.Is(err, forge.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if it.HasLabel(LParent) {
		t := &task{e: e, p: p, rc: rc, it: it, src: src}
		if err := t.load(ctx); err != nil {
			return false, err
		}
		// The label alone is anyone's to add: without devbot's block saying
		// so, the issue is no parent, and is whatever its labels make it.
		if isParent(it, e.meta(ctx, it, t.comments)) {
			return t.evaluateParent(e.TaskContext(ctx, it))
		}
	}
	state := State(it)
	if state == "" {
		return false, nil
	}
	ctx = e.TaskContext(ctx, it)
	t := &task{e: e, p: p, rc: rc, it: it, state: state, src: src}
	acted, err := t.evaluate(ctx)
	if acted {
		e.parentMoved(ctx, it, src)
		e.Count(ctx, "evaluations_acted", []string{string(src)}, 1)
		e.Log.Info("evaluated", "task", ref, "state", state, "src", src)
	}
	return acted, err
}

// task carries what one evaluation has read so far.
type task struct {
	e     *Engine
	p     forge.Provider
	rc    *config.Repo
	it    forge.Item
	state string
	src   Source

	comments []forge.Comment
	ss       []statusComment
	// trigger says what started a rebase round, for its metric.
	trigger string

	baseBranch string // see base
	// held is the wait that holds this task, while the task it is on is yet
	// to merge, or for a wait until tagged, no tag has its merge yet (see
	// wait): nothing is queued for it meanwhile, and its PR doesn't merge.
	held Wait
	// building is a held wait that builds against its task, which has a PR
	// to build against: implement jobs are queued after all, and run with
	// that PR's head checked out (BuildAgainst). The PR is held all the same.
	building bool
}

func (t *task) load(ctx context.Context) error {
	if t.comments != nil {
		return nil
	}
	cs, err := t.p.ListComments(ctx, t.it.Ref)
	if err != nil {
		return err
	}
	t.comments = cs
	t.ss = statuses(cs, t.e.bots(t.p)...)
	t.e.meta(ctx, t.it, cs) // caches the routing for audit records, at no extra read
	return nil
}

func (t *task) ref() string { return t.it.Ref.String() }

func (t *task) evaluate(ctx context.Context) (bool, error) {
	if t.it.State == "closed" {
		return t.closed(ctx)
	}
	// The base first: a /wait is checked against it.
	based, err := t.baseCommand(ctx)
	if err != nil {
		return based, err
	}
	waited, moved, err := t.wait(ctx)
	waited = waited || based
	if err != nil || moved {
		return waited, err
	}
	// A carried PR that merged or closed under the task is the gate's to
	// record, from any state: there is no starting over on a new branch.
	if t.state != LNeedsHuman && t.state != LInReview && t.state != LApproved {
		if gone, err := t.carriedGone(ctx); err != nil {
			return waited, err
		} else if gone {
			acted, err := t.inReview(ctx)
			return waited || acted, err
		}
	}
	var acted bool
	switch t.state {
	case LReady:
		acted, err = t.ready(ctx)
	case LWorking:
		acted, err = t.working(ctx)
	case LNeedsInfo:
		acted, err = t.needsInfo(ctx)
	case LChanges:
		acted, err = t.changesRequested(ctx)
	case LInReview, LApproved:
		acted, err = t.inReview(ctx)
	case LNeedsApproval:
		acted, err = t.needsApproval(ctx)
	} // needs-human: waits for a person
	return waited || acted, err
}

func (t *task) busy(ctx context.Context, kind string) (bool, error) {
	q, err := t.e.Q.IsQueued(ctx, kind, t.ref())
	if err != nil || q {
		return q, err
	}
	h, err := t.e.Q.Holder(ctx, kind, t.ref())
	return h != "", err
}

func (t *task) ready(ctx context.Context) (bool, error) {
	if b, err := t.busy(ctx, queue.KindImplement); err != nil || b {
		return false, err
	}
	// Only someone allowed to direct work can start it, whichever path
	// noticed the label.
	evs, err := t.p.LabelEvents(ctx, t.it.Ref)
	if err != nil {
		return false, err
	}
	actor, labelledAt := "", time.Time{}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Label == LReady && evs[i].Added {
			actor, labelledAt = evs[i].Actor, evs[i].At
			break
		}
	}
	if actor == "" {
		actor = t.it.Author // created with the label already on
	}
	if ok, why := t.e.authorize(ctx, t.p, t.rc, t.it.Ref.Repo, actor); !ok {
		t.e.auditRefusal(ctx, actor, "start", why, audit.Fields{})
		body := fmt.Sprintf("Ignoring `%s` applied by @%s: %s.\n\n%s",
			LReady, actor, why, RenderStatus(Status{Kind: "ignored"}))
		if err := t.p.Comment(ctx, t.it.Ref, body); err != nil {
			return false, err
		}
		return true, t.e.transition(ctx, t.p, t.it, "", "task.ignored", "label applied by an unauthorized user")
	}
	// A task that already has an open PR picks up where it left off. A PR
	// that can't be looked up isn't absent: a new run would open a second one.
	pr, err := t.findPR(ctx)
	if err != nil {
		return false, err
	}
	// A person putting devbot/ready back on a task that failed or stopped is
	// a retry from the forge UI: attempts and fix rounds start over, as with
	// the retry connector. Forges keep both times to the second, so a
	// relabel in the same second as the stop counts.
	if !forge.HasLogin(t.e.bots(t.p), actor) {
		if s, ok := lastStatus(t.ss, retryStops...); ok && s.Kind != "reset" && !labelledAt.Before(s.CreatedAt) {
			if err := t.retried(ctx, "@"+actor, actor); err != nil {
				return false, err
			}
		}
	}
	// The job carries what the issue said when the label was accepted, for
	// the run to hold the issue to (CheckPin). That is the issue as read now,
	// not as labelled: a forge that doesn't record body edits (Gitea) can't
	// show one made in between, which docs/devbot-yaml.md spells out.
	// So is the base it goes into, so a later edit to base_branch doesn't
	// move it.
	base, stopped, err := t.acceptBase(ctx)
	if err != nil || stopped {
		return stopped, err
	}
	j := queue.Job{Kind: queue.KindImplement, Ref: t.ref(), Mode: "new", Pin: PinOf(t.it), Base: base}
	if pr != nil && pr.State == "open" {
		j.Mode, j.PR, j.Reason = "fix", pr.Number, "Retried with the PR still open: read the issue and the PR's review history, and carry on from the branch."
		// A carried PR's first run is a fix round on it like any other.
		if n, err := t.carried(ctx); err != nil {
			return false, err
		} else if n == pr.Number {
			if ForkGone(*pr) {
				return true, t.needsHuman(ctx, ForkGoneText(pr.Number))
			}
			if stopped, err := t.carriedProtected(ctx, pr); err != nil || stopped {
				return stopped, err
			}
			if _, ok := lastStatus(t.ss, "pr"); !ok {
				j.Reason = carryReason(pr)
			}
		}
	} else if stopped, err := t.supersedeStart(ctx); err != nil || stopped {
		// The run that opens a PR to supersede a fork's starts from it.
		return stopped, err
	}
	if t.holdsImplement() {
		return false, nil
	}
	return t.enqueue(ctx, j)
}

// retryStops are the statuses a retry starts over from, and the reset that
// does it.
var retryStops = []string{"failed", "needs-human", "fix-requested", "reset"}

// retried posts the "reset" status for a retry by who, from which attempts,
// review rounds and rebase rounds count from zero. It is posted once per
// retry: not when the last stop already has a reset after it, nor when
// nothing has stopped yet. Comments are re-read first, as in postStatus,
// since the retry connector or another evaluation may just have posted it.
func (t *task) retried(ctx context.Context, who, principal string) error {
	t.comments = nil
	if err := t.load(ctx); err != nil {
		return err
	}
	if s, ok := lastStatus(t.ss, retryStops...); !ok || s.Kind == "reset" {
		return nil
	}
	body := fmt.Sprintf("Retried by %s. Attempts and review rounds start over.\n\n%s", who, RenderStatus(Status{Kind: "reset", Principal: principal}))
	if err := t.p.Comment(ctx, t.it.Ref, body); err != nil {
		return err
	}
	t.comments = nil // re-read with the reset
	return t.load(ctx)
}

// holdsImplement reports whether the task's wait keeps implementers off it:
// any that stands, but one building against a task with a PR, or a request
// to change its parent's contract.
func (t *task) holdsImplement() bool {
	if t.held.On.Number > 0 && !t.building {
		return true
	}
	// Read from the comments every caller has loaded: a child that asked its
	// parent to change the contract waits for a replan (plan.go).
	_, asked := contractAsked(t.ss)
	return asked
}

// enqueueImplement queues the task for an implementer, unless it is held by
// a wait: its release queues it then.
func (t *task) enqueueImplement(ctx context.Context, mode string, pr int, reason string) (bool, error) {
	if t.holdsImplement() {
		return false, nil
	}
	added, err := t.enqueue(ctx, queue.Job{Kind: queue.KindImplement, Ref: t.ref(), Mode: mode, PR: pr, Reason: reason})
	return added, err
}

func (t *task) working(ctx context.Context) (bool, error) {
	if b, err := t.busy(ctx, queue.KindImplement); err != nil || b {
		return false, err
	}
	if err := t.load(ctx); err != nil {
		return false, err
	}
	if s, ok := lastStatus(t.ss, "started"); ok && since(s.CreatedAt) < staleWorking {
		return false, nil // just started; the lease may not be visible yet
	}
	pr, err := t.findPR(ctx)
	if err != nil {
		return false, err
	}
	// The pod died (or Redis was wiped and nothing is running). That counts
	// as a failed attempt, so a run that keeps dying (out of memory, say)
	// ends in needs-human instead of looping. Put it back where it came from.
	// A failure since the run started is this one's, recorded by the run
	// itself or an evaluation whose label write then failed: it counts once.
	t.comments = nil
	if err := t.load(ctx); err != nil {
		return false, err
	}
	// Work a run held for approval stays held: the run posted the hold but
	// not its label.
	if s, ok := lastStatus(t.ss, "started", "held", "reset"); ok && s.Kind == "held" {
		return true, t.e.transition(ctx, t.p, t.it, LNeedsApproval, "", "")
	}
	failed := FailedAttempts(t.ss)
	if s, ok := lastStatus(t.ss, "started", "failed", "reset"); !ok || s.Kind != "failed" {
		if err := t.p.Comment(ctx, t.it.Ref, "The previous run stopped without finishing (its pod went away). Retrying.\n\n"+
			RenderStatus(Status{Kind: "failed", Role: "implement"})); err != nil {
			return false, err
		}
		failed++
	}
	if failed >= t.rc.MaxAttempts {
		return true, t.needsHuman(ctx, fmt.Sprintf("Giving up after %d failed attempts (`max_attempts`).", t.rc.MaxAttempts))
	}
	// The run pushing an approved change goes back to it, to be tried again.
	if s, ok := lastStatus(t.ss, "started"); ok && s.Mode == "held" {
		if err := t.e.transition(ctx, t.p, t.it, LNeedsApproval, "", ""); err != nil {
			return false, err
		}
		t.state = LNeedsApproval
		t.it.Labels = []string{LNeedsApproval}
		_, err := t.needsApproval(ctx)
		return true, err
	}
	if pr != nil && pr.State == "open" {
		if err := t.e.transition(ctx, t.p, t.it, LChanges, "", ""); err != nil {
			return false, err
		}
		return t.enqueueImplement(ctx, "fix", pr.Number, "previous run stopped without finishing")
	}
	if err := t.e.transition(ctx, t.p, t.it, LReady, "", ""); err != nil {
		return false, err
	}
	return t.enqueueImplement(ctx, "new", 0, "")
}

func (t *task) needsInfo(ctx context.Context) (bool, error) {
	if err := t.load(ctx); err != nil {
		return false, err
	}
	q, ok := lastStatus(t.ss, "question")
	if !ok {
		return false, nil
	}
	bots := t.e.bots(t.p)
	for _, c := range t.comments {
		// Comment IDs, not times, which are only to the second: an answer
		// can come in the same second as the question.
		if c.ID <= q.ID {
			continue
		}
		answered := false
		if forge.HasLogin(bots, c.Author) {
			s, ok := ParseStatus(c.Body)
			answered = ok && s.Kind == "answer" // relayed through a connector
		} else {
			answered = t.e.authorized(ctx, t.p, t.rc, t.it.Ref.Repo, c.Author)
		}
		if answered {
			p, err := t.findPR(ctx)
			if err != nil {
				return false, err
			}
			mode, pr := "new", 0
			if p != nil && p.State == "open" {
				mode, pr = "fix", p.Number
			}
			if err := t.e.transition(ctx, t.p, t.it, LReady, "task.answered", firstLine(stripMarkers(c.Body))); err != nil {
				return false, err
			}
			_, err = t.enqueueImplement(ctx, mode, pr, "")
			return true, err
		}
	}
	return false, nil
}

func (t *task) changesRequested(ctx context.Context) (bool, error) {
	if b, err := t.busy(ctx, queue.KindImplement); err != nil || b {
		return false, err
	}
	pr, err := t.findPR(ctx)
	if err != nil {
		return false, err
	}
	if pr == nil || pr.State != "open" {
		return t.inReview(ctx) // merged or closed underneath us
	}
	if acted, err := t.followBase(ctx, pr); err != nil || acted {
		return acted, err
	}
	reason := ""
	if err := t.load(ctx); err == nil {
		if s, ok := lastStatus(t.ss, "fix-requested"); ok {
			reason = stripMarkers(s.Body)
		}
		// The round ended waiting on another task, which has merged since:
		// it goes on from that merge, and its push answers for the rebase. Or
		// it was built against one that is released since, and depends on the
		// release.
		if rb := waitRound(t.ss, pr); rb != "" && !strings.Contains(reason, rb) {
			reason = strings.TrimSpace(reason + "\n\n" + rb)
		}
	}
	return t.enqueueImplement(ctx, "fix", pr.Number, reason)
}

// inReview runs the gate: checks, review, protected paths, mergeability,
// then merge or hand to a human.
func (t *task) inReview(ctx context.Context) (bool, error) {
	pr, err := t.findPR(ctx)
	if err != nil {
		return false, err
	}
	if pr == nil {
		if n, err := t.carried(ctx); err != nil || n > 0 {
			if err != nil {
				return false, err
			}
			return true, t.needsHuman(ctx, fmt.Sprintf("PR #%d, which this task carries, is gone.", n))
		}
		if b, err := t.busy(ctx, queue.KindImplement); err != nil || b {
			return false, err
		}
		if err := t.e.transition(ctx, t.p, t.it, LReady, "", ""); err != nil {
			return false, err
		}
		return t.enqueueImplement(ctx, "new", 0, "")
	}
	if pr.Merged {
		err := t.merged(ctx, pr)
		t.landed(ctx, pr)
		return true, err
	}
	if pr.State != "open" {
		t.closedPR(ctx, pr)
		return true, t.needsHuman(ctx, fmt.Sprintf("PR #%d was closed without merging.", pr.Number))
	}
	if t.state == LChanges {
		return false, nil
	}
	if acted, err := t.followBase(ctx, pr); err != nil || acted {
		return acted, err
	}
	// followBase has the PR on the task's base, so the forge's Behind and
	// Mergeable are against it too.
	base, err := t.base(ctx)
	if err != nil {
		return false, err
	}

	head := pr.HeadSHA
	reviews, err := t.p.Reviews(ctx, pr.Repo, pr.Number)
	if err != nil {
		return false, err
	}
	if err := t.load(ctx); err != nil {
		return false, err
	}
	// The reviewer judged the PR against the issue as it then stood, so its
	// verdicts from before an edit_task are on a task that no longer says
	// that, whatever the head. Forges keep times to the second: one in the
	// edit's second counts as before.
	redirected := redirectedAt(t.ss).Truncate(time.Second)
	_, reviewer := t.p.BotUsers()
	var approved bool
	var approvedAt time.Time
	var changes []forge.Review
	for _, r := range reviews {
		if r.Dismissed || r.CommitSHA != head {
			continue
		}
		if forge.SameLogin(r.Author, reviewer) && !redirected.IsZero() && !r.SubmittedAt.Truncate(time.Second).After(redirected) {
			continue
		}
		// A required reviewer is settled by its own last word on the head,
		// below, whatever it may direct otherwise.
		if !forge.SameLogin(r.Author, reviewer) && forge.HasLogin(t.rc.RequiredReviewers, r.Author) {
			continue
		}
		switch {
		case r.Verdict == forge.VerdictRequestChanges && (forge.SameLogin(r.Author, reviewer) || t.e.reviewCounts(ctx, t.p, t.rc, pr.Repo, r.Author)):
			changes = append(changes, r)
		case r.Verdict == forge.VerdictApprove && forge.SameLogin(r.Author, reviewer):
			if !approved || r.SubmittedAt.Before(approvedAt) {
				approvedAt = r.SubmittedAt
			}
			approved = true
		}
	}
	reqApproved, awaited, reqChanges := requiredReviews(t.rc.RequiredReviewers, reviews, head)
	changes = append(changes, reqChanges...)
	if len(changes) > 0 {
		lines, err := t.p.ReviewComments(ctx, pr.Repo, pr.Number)
		if err != nil {
			return false, err
		}
		var b strings.Builder
		for _, r := range changes {
			fmt.Fprintf(&b, "Review from @%s:\n\n%s\n\n", r.Author, Defang(r.Body))
			b.WriteString(lineComments(lines, r.ID))
		}
		return t.fix(ctx, pr, "review", b.String())
	}
	// A PR built against another task's unmerged work isn't reviewed, merged
	// or sent back for its CI, which builds against the published
	// dependency, without that work, until the work is released.
	if t.held.Build {
		return t.heldForRelease(ctx, pr)
	}
	// Someone pushed to an approved PR, or updated its branch. The approval
	// was for another commit, so the label goes back to in-review before
	// anything else: the review harness only takes in-review tasks.
	if t.state == LApproved && !approved {
		st := Status{Kind: "head-moved", PR: pr.Number, SHA: head}
		if _, err := t.postStatus(ctx, fmt.Sprintf("PR #%d's head moved to `%s` since it was approved, so it needs a fresh review.", pr.Number, head[:min(12, len(head))]), st); err != nil {
			return false, err
		}
		if err := t.e.transition(ctx, t.p, t.it, LInReview, "", ""); err != nil {
			return false, err
		}
		t.state = LInReview
		t.it.Labels = append(slices.DeleteFunc(slices.Clone(t.it.Labels), func(l string) bool { return l == LApproved }), LInReview)
		_, err := t.inReview(ctx)
		return true, err
	}

	// The wait for required reviewers starts when devbot first finds this
	// head without them, which is about when it was pushed: they review
	// alongside CI and devbot's reviewer, not after.
	var awaitedFrom time.Time
	if len(awaited) > 0 {
		if awaitedFrom, err = t.e.Q.AwaitedSince(ctx, t.ref(), pr.Number, head, time.Now(), t.rc.ChecksPendingTimeout); err != nil {
			return false, err
		}
	}

	checks, details, err := t.p.CombinedChecks(ctx, pr.Repo, head)
	if err != nil {
		return false, err
	}
	// Checks only a person can make pass are kept out of what devbot reacts
	// to: no fix round, no re-run, no pending timeout. They still hold the
	// merge below. A parent's integration check is left out too: it holds
	// the merge through integrationGate.
	checks, ours, human := t.splitHumanChecks(withoutIntegration(checks, details))
	switch checks {
	case forge.ChecksPending:
		return t.checksPending(ctx, pr, ours)
	case forge.ChecksFailure:
		if cancelled := cancelledOnly(ours); cancelled != nil {
			return t.rerun(ctx, pr, cancelled)
		}
		var b strings.Builder
		b.WriteString("CI failed on this PR:\n\n")
		for _, d := range ours {
			switch {
			case d.Cancelled:
				fmt.Fprintf(&b, "- `%s`: cancelled %s\n", Defang(d.Context), Defang(d.Description))
			case d.Failed:
				fmt.Fprintf(&b, "- `%s`: %s %s\n", Defang(d.Context), d.State, Defang(d.Description))
			}
		}
		// What CI said about each check goes to the run alone (the harness
		// reads it): this reason is posted on the issue too.
		b.WriteString("\nReproduce them locally where the repo's own checks cover them, fix them, and push. A check that runs only on the forge (a scanner, a licence or policy check) can't be reproduced here: what it reported is all there is to go on.")
		return t.fix(ctx, pr, "ci", b.String())
	}

	if !approved {
		if b, err := t.busy(ctx, queue.KindReview); err != nil || b {
			return false, err
		}
		return t.enqueue(ctx, queue.Job{Kind: queue.KindReview, Ref: t.ref(), PR: pr.Number})
	}

	// Approved on the current head. The deterministic gate from here. The
	// change set comes from git, in full, or there is no merge.
	cs, err := t.e.changeSet(ctx, t.p, *pr)
	if err != nil {
		return false, fmt.Errorf("listing what PR #%d changes: %w", pr.Number, err)
	}
	approvable, hit := t.rc.HeldChanges(cs)
	if len(hit) > 0 {
		return true, t.needsHuman(ctx, fmt.Sprintf("PR #%d touches protected paths, so a human has to merge it:\n\n- %s",
			pr.Number, strings.Join(hit, "\n- ")))
	}
	// Protected files a person may approve merge only as an approval on
	// this task left them: whoever changed them since, a person on the
	// branch included, did so unapproved.
	if files := HeldFiles(approvable); len(files) > 0 && !Approved(t.ss, files) {
		var b strings.Builder
		for _, f := range files {
			fmt.Fprintf(&b, "\n- %s", config.ShowPath(f.Path))
		}
		return true, t.needsHuman(ctx, fmt.Sprintf("PR #%d changes protected files, and what they hold at head `%s` isn't what an approval on this task approved, so a human has to merge it:\n%s",
			pr.Number, shortSHA(head), b.String()))
	}
	// The repo's own rules count as CI too: a check they require that never
	// reports would hold the merge for ever, so it waits like no checks at
	// all, then stops.
	// Rules that can't be read don't hold the gate: the merge is the
	// authority, and a refusal there stops the task with the forge's reason.
	prot, err := t.p.Protection(ctx, pr.Repo, base)
	if err != nil {
		t.e.Log.Warn("protection lookup failed", "repo", pr.Repo, "branch", base, "err", err)
		t.e.Count(ctx, "protection_lookup_errors", []string{pr.Repo.String(), t.p.Name()}, 1)
		prot = forge.Protection{}
	}
	missing := missingChecks(prot.RequiredChecks, details)
	if (checks == forge.ChecksNone && *t.rc.RequireChecks) || len(missing) > 0 {
		if since(t.headSince(pr)) < checksGrace {
			return false, nil
		}
		if len(missing) > 0 {
			return true, t.needsHuman(ctx, fmt.Sprintf("The repo's own rules on `%s` require checks that never reported on PR #%d's head:\n\n- `%s`\n\nThe forge won't allow the merge without them. Make CI report them on pull requests, or change the rule, then add `%s` to retry; or merge it by hand.",
				base, pr.Number, strings.Join(missing, "`\n- `"), LReady))
		}
		return true, t.needsHuman(ctx, fmt.Sprintf("PR #%d has no checks reported on its head, and this repo sets `require_checks: true`. Add CI that runs on pull requests, or merge it by hand.", pr.Number))
	}
	// Not known yet either way: the forge's answer comes with a later
	// evaluation, and merging now would only find out the hard way.
	if pr.MergeableUnknown {
		return false, nil
	}
	// A task waiting on another neither merges nor takes a turn to be rebased
	// before that one has merged; its release rebases it onto the result, or,
	// for a task of another repo, lets it go on as it is.
	if t.held.On.Number > 0 {
		return t.heldApproved(ctx, pr, head)
	}
	// A person's change request stands until they approve or dismiss it,
	// even once pushes have moved the head on: only they can say the push
	// did what they asked.
	held := t.heldBy(ctx, pr, reviews)
	perTask, why := false, ""
	if !t.rc.Automerge || len(held) > 0 {
		if perTask, why, err = t.taskAutomerge(ctx); err != nil {
			return false, err
		}
	}
	auto := (t.rc.Automerge || perTask) && len(held) == 0
	// A required reviewer that never comes would hold the PR for good, so
	// it is waited for as long as a pending check, then the task stops.
	if len(awaited) > 0 {
		for _, s := range t.ss {
			if s.Kind == "reset" && s.CreatedAt.After(awaitedFrom) {
				awaitedFrom = s.CreatedAt
			}
		}
		if since(awaitedFrom) >= t.rc.ChecksPendingTimeout {
			if err := t.e.leaveBase(ctx, pr.Repo, pr.BaseBranch, t.ref()); err != nil {
				return false, err
			}
			return true, t.needsHuman(ctx, fmt.Sprintf("PR #%d's head `%s` has waited more than %s (`checks_pending_timeout`) for an approval from its required reviewers (`required_reviewers`):\n\n- @%s\n\nGet them to review it, then add `%s` to retry; or merge it by hand.",
				pr.Number, head[:min(12, len(head))], t.rc.ChecksPendingTimeout, strings.Join(awaited, "\n- @"), LReady))
		}
	}
	// What only someone else can do holds the merge, and the rebase queue's
	// turn with it, but the task waits at approved rather than stopping.
	merges := auto
	waiting := len(awaited) > 0 || len(human) > 0
	// A child of a parent with an integration check merges only once it
	// passed on every child's current head.
	integ, err := t.integrationGate(ctx, pr)
	if err != nil {
		return false, err
	}
	auto = auto && !waiting && integ == ""
	// An approved PR is rebased only when it is about to merge, or someone
	// asks: rebasing every one each time the base moves would cost a
	// rebase and a review apiece for PRs that land one at a time anyway.
	asked, err := t.rebaseAsked(ctx)
	if err != nil {
		return false, err
	}
	stale := !pr.Mergeable || pr.Behind
	switch {
	case stale && (asked != nil || t.rc.Rebase == config.RebaseEager):
		t.markBehind(ctx, pr, false)
		return t.rebase(ctx, pr, asked, "eager")
	case stale && auto:
		return t.queueRebase(ctx, pr, approvedAt)
	case asked != nil:
		if err := t.rebaseNotNeeded(ctx, pr, asked); err != nil {
			return false, err
		}
	}
	if !auto {
		if err := t.e.Q.LeaveRebaseQueue(ctx, rebaseBase(pr), t.ref()); err != nil {
			return false, err
		}
	}
	t.markBehind(ctx, pr, stale)
	// A PR the forge reports blocked by the repo's rules is still tried: the
	// reviewer, which merges, may be exempt from them. A refusal stops.
	if auto {
		return t.merge(ctx, pr, prot, perTask, approvedAt)
	}
	mode := ""
	switch {
	case waiting:
		mode = "waiting"
	case integ != "":
		mode = "integration"
	}
	// An approved task says so again only when what it waits for changed on
	// this head: its required reviewers all approved, say.
	if last, ok := lastStatus(t.ss, "approved"); t.state == LApproved && (!ok || last.PR != pr.Number || last.SHA != head || last.Mode == mode) {
		return false, nil
	}
	wait := "`automerge` is off for this repo, so it's waiting for a human to merge."
	switch {
	case merges:
		wait = "It merges itself once that's done."
	case len(held) > 0:
		wait = fmt.Sprintf("@%s asked for changes on an earlier commit and hasn't approved or dismissed that review since, so it isn't merged automatically. It's waiting for that, or for a human to merge.",
			strings.Join(held, ", @"))
	}
	if why != "" {
		why = " " + why
	}
	if pr.Blocked {
		why += fmt.Sprintf("\n\nThe forge reports that the repo's own rules on `%s` block the merge as things stand, so whoever merges it may have to meet them first (an approving review from a person, say).%s",
			base, ruleList(prot))
	}
	why += rebaseAdvice(pr, prot)
	text := fmt.Sprintf("PR #%d is approved and its checks pass.", pr.Number)
	switch {
	case waiting:
		text = fmt.Sprintf("PR #%d is approved by devbot's reviewer, and is waiting for what only others can do.", pr.Number)
	case integ != "":
		text = fmt.Sprintf("PR #%d is approved and its checks pass, and is waiting for its parent task's integration check.", pr.Number)
	}
	if integ != "" {
		text += "\n\n" + integ
	}
	if len(human) > 0 {
		text += fmt.Sprintf("\n\nChecks only a person can make pass (`human_checks`) aren't green on head `%s`:\n\n%s\n\nA person needs to act on them. devbot doesn't send them to the implementer, and they use no review round or pending timeout.",
			head[:min(12, len(head))], checkList(human))
	}
	if len(t.rc.RequiredReviewers) > 0 {
		text += "\n\n" + requiredLine(reqApproved, awaited, head)
	}
	if waiting || integ != "" || len(t.rc.RequiredReviewers) > 0 {
		text += "\n\n"
	} else {
		text += " "
	}
	text += wait + why
	st := Status{Kind: "approved", PR: pr.Number, SHA: head, Mode: mode}
	posted, err := t.postStatus(ctx, text, st)
	if err != nil {
		return false, err
	}
	// Back to something this head already said (a human check that went
	// red again): nothing new to say or do.
	if !posted && t.state == LApproved {
		return false, nil
	}
	// Even when the comment was already there, finish the transition: the
	// evaluation that posted it may have died before getting this far.
	return true, t.e.transitionOnce(ctx, t.p, t.it, LApproved, "task.approved", fmt.Sprintf("PR #%d approved, waiting for a human to merge", pr.Number), "", t.onceKey(st))
}

// requiredReviews reads each required reviewer's last verdict on head:
// approved or awaited, by login as configured, and the change requests to
// send back. An earlier head's verdicts count for nothing, as with devbot's
// reviewer, so a push needs fresh approvals.
func requiredReviews(required []string, reviews []forge.Review, head string) (approved, awaited []string, changes []forge.Review) {
	reviews = slices.Clone(reviews)
	slices.SortStableFunc(reviews, func(a, b forge.Review) int { return a.SubmittedAt.Compare(b.SubmittedAt) })
	for _, login := range required {
		var last *forge.Review
		for i, r := range reviews {
			if forge.SameLogin(r.Author, login) && !r.Dismissed && r.CommitSHA == head && r.Verdict != forge.VerdictComment {
				last = &reviews[i]
			}
		}
		switch {
		case last != nil && last.Verdict == forge.VerdictApprove:
			approved = append(approved, login)
		case last != nil && last.Verdict == forge.VerdictRequestChanges:
			awaited, changes = append(awaited, login), append(changes, *last)
		default:
			awaited = append(awaited, login)
		}
	}
	return approved, awaited, changes
}

// requiredLine says which required reviewers approved head and which it
// still waits for.
func requiredLine(approved, awaited []string, head string) string {
	list := func(ls []string) string {
		if len(ls) == 0 {
			return "nobody"
		}
		return "@" + strings.Join(ls, ", @")
	}
	return fmt.Sprintf("Required reviewers (`required_reviewers`) on head `%s`: approved by %s; still awaited from %s.",
		head[:min(12, len(head))], list(approved), list(awaited))
}

// splitHumanChecks takes the checks that match human_checks out of what
// the head's combined state says, so that state is only what devbot can
// act on: ours is every other check, and human the matching ones that
// aren't green.
func (t *task) splitHumanChecks(checks forge.CheckState, details []forge.CheckDetail) (forge.CheckState, []forge.CheckDetail, []forge.CheckDetail) {
	if len(t.rc.HumanChecks) == 0 {
		return checks, details, nil
	}
	var ours, human []forge.CheckDetail
	for _, d := range details {
		switch {
		case !t.rc.HumanCheck(d.Context):
			ours = append(ours, d)
		case d.Failed || d.Pending:
			human = append(human, d)
		}
	}
	if len(human) == 0 {
		return checks, ours, nil
	}
	switch {
	case slices.ContainsFunc(ours, func(d forge.CheckDetail) bool { return d.Failed }):
		return forge.ChecksFailure, ours, human
	case slices.ContainsFunc(ours, func(d forge.CheckDetail) bool { return d.Pending }):
		return forge.ChecksPending, ours, human
	}
	return forge.ChecksSuccess, ours, human
}

// checkList is one line per check, with what the forge says of it.
func checkList(ds []forge.CheckDetail) string {
	var b strings.Builder
	for _, d := range ds {
		fmt.Fprintf(&b, "- `%s`: %s\n", Defang(d.Context), strings.TrimSpace(d.State+" "+Defang(d.Description)))
	}
	return strings.TrimRight(b.String(), "\n")
}

// heldBy is who, of the people who can direct devbot and the repo's
// trusted_reviewers, last reviewed the PR asking for changes, on any head,
// in a review not since dismissed. The bots' own reviews are settled by
// their next one.
func (t *task) heldBy(ctx context.Context, pr *forge.PR, reviews []forge.Review) []string {
	reviews = slices.Clone(reviews)
	slices.SortStableFunc(reviews, func(a, b forge.Review) int { return a.SubmittedAt.Compare(b.SubmittedAt) })
	last := map[string]forge.Review{}
	var people []string
	for _, r := range reviews {
		// A dismissal (GitHub reports the review as a comment then) settles
		// it; a plain comment says nothing either way.
		if (r.Verdict == forge.VerdictComment && !r.Dismissed) || forge.HasLogin(t.e.bots(t.p), r.Author) {
			continue
		}
		k := strings.ToLower(r.Author)
		if _, ok := last[k]; !ok {
			people = append(people, k)
		}
		last[k] = r
	}
	var out []string
	for _, k := range people {
		if r := last[k]; r.Verdict == forge.VerdictRequestChanges && !r.Dismissed && t.e.reviewCounts(ctx, t.p, t.rc, pr.Repo, r.Author) {
			out = append(out, r.Author)
		}
	}
	return out
}

// postStatus posts a status comment unless the issue already has one from a
// bot with the same kind, PR and head: the backstop for two evaluations that
// both decided to say the same thing. Comments are re-read first, since what
// this evaluation loaded may predate the other's post. A retry starts over,
// so only statuses since the last reset count.
func (t *task) postStatus(ctx context.Context, text string, s Status) (bool, error) {
	t.comments = nil
	if err := t.load(ctx); err != nil {
		return false, err
	}
	for i := len(t.ss) - 1; i >= 0 && t.ss[i].Kind != "reset"; i-- {
		if t.ss[i].key() == s.key() {
			return false, nil
		}
	}
	return true, t.p.Comment(ctx, t.it.Ref, text+"\n\n"+RenderStatus(s))
}

// onceKey keys the event that goes with s, so it is emitted once per status
// and retry, like the comment.
func (t *task) onceKey(s Status) string {
	reset := int64(0)
	if r, ok := lastStatus(t.ss, "reset"); ok {
		reset = r.ID
	}
	return fmt.Sprintf("%s|%d", s.key(), reset)
}

// taskAutomerge reports whether the issue's devbot/automerge label stands in
// for the human merge, and if it is there but doesn't, why not. It is only
// asked once every other gate condition has passed.
func (t *task) taskAutomerge(ctx context.Context) (bool, string, error) {
	if !t.it.HasLabel(LAutomerge) {
		return false, "", nil
	}
	if !*t.rc.TaskAutomerge {
		return false, fmt.Sprintf("The issue has `%s`, but per-task automerge is disabled for this repo (`task_automerge: false`).", LAutomerge), nil
	}
	// The label merges into the task's base, so it counts only from someone
	// who could have merged by hand, as with devbot/ready.
	evs, err := t.p.LabelEvents(ctx, t.it.Ref)
	if err != nil {
		return false, "", err
	}
	actor := ""
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Label == LAutomerge && evs[i].Added {
			actor = evs[i].Actor
			break
		}
	}
	if actor == "" {
		actor = t.it.Author // created with the label already on
	}
	if ok, why := t.e.authorize(ctx, t.p, t.rc, t.it.Ref.Repo, actor); !ok {
		return false, fmt.Sprintf("The issue has `%s`, but it was applied by @%s: %s.", LAutomerge, actor, why), nil
	}
	return true, "", nil
}

// maxReruns is how many times devbot re-runs a head's cancelled checks
// before it stops: a check that keeps being cancelled is CI's to sort out.
const maxReruns = 3

// rerunGrace is how long a re-run may take to show on the head's checks.
// Until then the forge still reports the cancellation it replaces.
const rerunGrace = 5 * time.Minute

// cancelledOnly is the failed checks, when every one of them was cancelled
// rather than failing, and nil otherwise.
func cancelledOnly(details []forge.CheckDetail) []forge.CheckDetail {
	var out []forge.CheckDetail
	for _, d := range details {
		if d.Failed && !d.Cancelled {
			return nil
		}
		if d.Failed {
			out = append(out, d)
		}
	}
	return out
}

// rerun re-runs checks that were cancelled on the PR's head. A cancelled run
// says nothing about the code, so the implementer can't fix it, and it uses
// no review round; but the re-runs are counted from their status comments,
// since the last retry, so a check that keeps being cancelled stops the
// task instead of holding it in review. The comment goes first, so a re-run
// that is never recorded can't happen, and only the evaluation that posted
// it re-runs.
func (t *task) rerun(ctx context.Context, pr *forge.PR, cancelled []forge.CheckDetail) (bool, error) {
	if err := t.load(ctx); err != nil {
		return false, err
	}
	n, last := 0, time.Time{}
	for _, s := range t.ss {
		switch {
		case s.Kind == "reset":
			n = 0
		case s.Kind == "rerun" && s.PR == pr.Number && s.SHA == pr.HeadSHA:
			n, last = n+1, s.CreatedAt
		}
	}
	names := make([]string, len(cancelled))
	for i, d := range cancelled {
		names[i] = Defang(d.Context)
	}
	list := "- `" + strings.Join(names, "`\n- `") + "`"
	if n > 0 && since(last) < rerunGrace {
		return false, nil
	}
	if n >= maxReruns {
		return true, t.needsHuman(ctx, fmt.Sprintf("CI on PR #%d keeps being cancelled: devbot re-ran it %d times on head `%s`, and it was cancelled again:\n\n%s\n\nA cancelled check says nothing about the code, so it isn't sent back to the implementer. Find out what cancels it (a concurrency group another branch's runs share, say), then re-run it on the forge or add `%s` to retry.",
			pr.Number, n, pr.HeadSHA[:min(12, len(pr.HeadSHA))], list, LReady))
	}
	st := Status{Kind: "rerun", PR: pr.Number, SHA: pr.HeadSHA, Attempt: n + 1}
	posted, err := t.postStatus(ctx, fmt.Sprintf("CI on PR #%d was cancelled, not failed, which says nothing about the code:\n\n%s\n\nRe-running it on the same head (%d of %d). This doesn't use a review round.",
		pr.Number, list, n+1, maxReruns), st)
	if err != nil || !posted {
		return false, err
	}
	if err := t.p.RerunChecks(ctx, pr.Repo, pr.HeadSHA, cancelled); err != nil {
		if !errors.Is(err, forge.ErrNotRerunnable) && !errors.Is(err, forge.ErrForbidden) {
			return true, err
		}
		return true, t.needsHuman(ctx, fmt.Sprintf("CI on PR #%d was cancelled, not failed, and devbot couldn't re-run it:\n\n%s\n\n> %s\n\nRe-run it on the forge, then add `%s` to retry.",
			pr.Number, list, Defang(err.Error()), LReady))
	}
	t.e.Count(ctx, "check_reruns", []string{pr.Repo.String(), t.p.Name()}, 1)
	return true, nil
}

// fix sends a PR back to the implementer, at most once per head commit. Every
// round counts towards max_review_rounds except a clean rebase: one whose push
// left the PR's diff unchanged and whose new head was then approved and green,
// which is what being asked to rebase again means. Those count towards the
// more generous max_rebase_rounds instead, so a PR waiting for a human merge
// on a busy base isn't stopped for work it never needed.
func (t *task) fix(ctx context.Context, pr *forge.PR, kind, reason string) (bool, error) {
	if err := t.load(ctx); err != nil {
		return false, err
	}
	// A retarget's reset can ask again for a head asked about before it. An
	// edit_task's round asks for what the task now says, not what this head
	// lacks, and its push may leave the head where it was, so it neither asks
	// nor answers for the head. A round is answered by a push that made this
	// head: one that moved it may not show in GetPR yet, and isn't over until
	// it does.
	asked, answered, editing := false, false, false
	for _, s := range t.ss {
		switch {
		case s.Kind == "reset":
			asked, answered, editing = false, false, false
		case s.Kind == "fix-requested" && s.Mode == "edit":
			editing = true
		case s.Kind == "fix-requested" && s.SHA == pr.HeadSHA:
			asked, answered, editing = true, false, false
		case s.Kind == "fix-requested":
			editing = false
		case s.Kind == "pr" && s.PR == pr.Number && asked && !editing && s.SHA == pr.HeadSHA:
			answered = true
		}
	}
	// The round asked for on this head is over and left the head as it was
	// (the implementer found nothing to change, say). Asking again would get
	// the same answer, and not asking would leave the task in review with
	// nothing moving it.
	if asked && answered && (t.state == LInReview || t.state == LApproved) {
		return true, t.needsHuman(ctx, fmt.Sprintf("PR #%d was sent back to the implementer for its head `%s`, and the round ended without changing it, so the same reason (%s) still holds and asking again would get the same answer. Latest:\n\n%s",
			pr.Number, pr.HeadSHA[:min(12, len(pr.HeadSHA))], kind, reason))
	}
	if asked {
		return false, nil // already asked for this head since the last reset
	}
	rounds, rebases := roundCounts(t.ss, kind)
	if kind == "rebase" && rebases >= t.rc.MaxRebaseRounds {
		return true, t.needsHuman(ctx, fmt.Sprintf("PR #%d has been rebased cleanly %d times and `%s` has moved again. That's the rebase cap (`max_rebase_rounds`), not `max_review_rounds`: merge it, or retry to allow more rebases.", pr.Number, rebases, pr.BaseBranch))
	}
	// A rebase may yet turn out clean, so it only needs the review cap not
	// to be exceeded already; if it isn't clean, the next request stops.
	if rounds > t.rc.MaxReviewRounds || (kind != "rebase" && kind != "retarget" && kind != "release" && rounds == t.rc.MaxReviewRounds) {
		return true, t.needsHuman(ctx, fmt.Sprintf("PR #%d still needs changes after %d rounds (`max_review_rounds`). Latest:\n\n%s", pr.Number, rounds, reason))
	}
	st := Status{Kind: "fix-requested", PR: pr.Number, SHA: pr.HeadSHA, Mode: kind}
	body := fmt.Sprintf("Sending PR #%d back to the implementer (%s, round %d of %d).\n\n%s",
		pr.Number, kind, rounds+1, t.rc.MaxReviewRounds, reason)
	switch kind {
	case "rebase":
		body = fmt.Sprintf("Sending PR #%d back to the implementer (rebase, %d of %d review rounds used; a clean rebase doesn't use one, up to %d in all).\n\n%s",
			pr.Number, rounds, t.rc.MaxReviewRounds, t.rc.MaxRebaseRounds, reason)
	case "retarget":
		body = fmt.Sprintf("Sending PR #%d back to the implementer to rebase onto its new base `%s` (this doesn't use a review round).\n\n%s",
			pr.Number, pr.BaseBranch, reason)
	case "release":
		body = fmt.Sprintf("Sending PR #%d back to the implementer to depend on the release it was waiting for (this doesn't use a review round).\n\n%s", pr.Number, reason)
	}
	// Unlike approval, a round someone else just posted is theirs to finish:
	// enqueueing again could start a second fix after the first was popped.
	if posted, err := t.postStatus(ctx, body, st); err != nil || !posted {
		return false, err
	}
	if err := t.e.transitionOnce(ctx, t.p, t.it, LChanges, "task.changes_requested", fmt.Sprintf("%s: changes requested on PR #%d", kind, pr.Number), "", t.onceKey(st)); err != nil {
		return false, err
	}
	if kind == "rebase" {
		t.e.Q.Count(ctx, "rebases_started", []string{pr.Repo.String(), cmp.Or(t.trigger, "eager")}, 1)
	}
	_, err := t.enqueueImplement(ctx, "fix", pr.Number, reason)
	return true, err
}

// roundCounts counts the fix rounds that used up max_review_rounds since the
// last retry, and the clean rebases that didn't. A round is only settled by
// what came after it: the push it led to, and next, the reason for the round
// about to start ("" for none, when a rebase just pushed may yet turn out
// clean and so isn't counted).
func roundCounts(ss []statusComment, next string) (rounds, rebases int) {
	prev, unchanged := "", false
	settle := func(next string) {
		switch {
		case prev == "" || prev == "retarget": // moving onto a new base is the retarget's doing
		case prev == "release": // depending on a release is the wait's doing
		case prev == "edit": // a person changed the task, not the implementer's work
		case prev == "rebase" && unchanged && next == "rebase":
			rebases++
		case prev == "rebase" && unchanged && next == "":
		default:
			rounds++
		}
		prev = ""
	}
	for _, s := range ss {
		switch s.Kind {
		case "reset":
			rounds, rebases, prev = 0, 0, ""
		case "pr":
			unchanged = s.Unchanged
		case "fix-requested":
			settle(s.Mode)
			prev, unchanged = s.Mode, false
		}
	}
	settle(next)
	return rounds, rebases
}

// merge merges a PR that passed the gate. perTask says it was the issue's
// devbot/automerge label rather than the repo's automerge, which the comment
// and the event say so the requester can tell. Once it has merged, the turn
// in the rebase queue passes on, after the merge lock is let go so the next
// task can take it.
func (t *task) merge(ctx context.Context, pr *forge.PR, prot forge.Protection, perTask bool, approvedAt time.Time) (bool, error) {
	retry, err := t.e.Q.MergeRetry(ctx, t.ref())
	if err != nil {
		return false, err
	}
	if retry.Head != pr.HeadSHA {
		retry = queue.MergeRetry{} // the failures were on an older head, which this one replaces
	}
	if time.Now().Before(retry.Next) {
		return false, nil // backing off a merge that failed; reconcile comes back
	}
	owner := fmt.Sprintf("%s#%d/%s", t.ref(), pr.Number, rand.Text())
	ok, err := t.e.Q.LockMerge(ctx, pr.Repo.String(), owner)
	if err != nil || !ok {
		return false, err // another merge on this repo is in flight; re-evaluated later
	}
	acted, err := t.mergeLocked(ctx, pr, prot, perTask, approvedAt, retry)
	t.e.Q.UnlockMerge(context.WithoutCancel(ctx), pr.Repo.String(), owner)
	if pr.Merged {
		t.landed(ctx, pr)
	}
	return acted, err
}

func (t *task) mergeLocked(ctx context.Context, pr *forge.PR, prot forge.Protection, perTask bool, approvedAt time.Time, retry queue.MergeRetry) (bool, error) {
	// The gate read the PR before the lock: a merge that held it meanwhile,
	// from any replica or job, may have moved the base under it. Then it
	// waits its turn like any PR that fell behind.
	fresh, err := t.p.GetPR(ctx, pr.Repo, pr.Number)
	if err != nil {
		return false, err
	}
	switch {
	case fresh.Merged || fresh.State != "open" || fresh.HeadSHA != pr.HeadSHA:
		return false, nil // changed since the gate read it; that change re-evaluates
	case fresh.MergeableUnknown:
		return false, nil // as in the gate: a later evaluation has the answer
	case (!fresh.Mergeable || fresh.Behind) && t.rc.Rebase == config.RebaseEager:
		return t.rebase(ctx, &fresh, nil, "eager")
	case !fresh.Mergeable || fresh.Behind:
		return t.queueRebase(ctx, &fresh, approvedAt)
	}
	if err := t.p.Merge(ctx, pr.Repo, pr.Number, pr.HeadSHA, t.rc.MergeMethod); err != nil {
		return true, t.mergeFailed(ctx, pr, prot, perTask, retry, err)
	}
	pr.Merged = true
	return true, t.mergedHere(ctx, pr, perTask)
}

// mergedHere records a merge devbot made.
func (t *task) mergedHere(ctx context.Context, pr *forge.PR, perTask bool) error {
	if perTask {
		if err := t.recordMerge(ctx, pr, fmt.Sprintf("PR #%d merged (per-task automerge)", pr.Number)); err != nil {
			return err
		}
		return t.p.Comment(ctx, t.it.Ref, fmt.Sprintf("Merged PR #%d by per-task automerge: the issue has `%s` and the merge gate passed.", pr.Number, LAutomerge))
	}
	return t.merged(ctx, pr)
}

// mergeFailed deals with an error from the merge. The forge may have merged
// all the same (a reply lost to the client's timeout, or a slow server-side
// merge), so the PR is read again first. A head that really moved is left
// to the next evaluation, which reads the new head. ErrHeadMoved with the
// head unchanged is not one: Gitea's 409 also covers a conflict or a
// rejected push found while merging, which would fail the same way on every
// evaluation, so it is counted like the errors that may pass. Only a definite
// refusal goes to a person. Anything else is tried again on a later
// evaluation, backing off as for usage limits, and stops after
// backoff.max_retries.
func (t *task) mergeFailed(ctx context.Context, pr *forge.PR, prot forge.Protection, perTask bool, retry queue.MergeRetry, err error) error {
	now, gerr := t.p.GetPR(ctx, pr.Repo, pr.Number)
	if gerr == nil && now.Merged {
		pr.Merged = true
		return t.mergedHere(ctx, &now, perTask)
	}
	t.e.Log.Warn("merge failed", "task", t.it.Ref, "pr", pr.Number, "err", err)
	switch {
	case errors.Is(err, forge.ErrHeadMoved) && gerr == nil && now.HeadSHA != pr.HeadSHA:
		return t.e.Q.ClearMergeRetry(ctx, t.ref())
	case errors.Is(err, forge.ErrNotMergeable):
		return t.mergeStopped(ctx, t.mergeRefused(ctx, pr, prot, err))
	case errors.Is(err, forge.ErrForbidden), errors.Is(err, forge.ErrUnauthorized):
		return t.mergeStopped(ctx, t.needsHuman(ctx, fmt.Sprintf("Merging PR #%d failed: %s", pr.Number, Defang(err.Error()))))
	}
	retry.Head = pr.HeadSHA
	retry.N++
	if retry.N > t.e.Cfg.Backoff.MaxRetries {
		return t.mergeStopped(ctx, t.needsHuman(ctx, fmt.Sprintf("Merging PR #%d failed %d times in a row (`backoff.max_retries`). Last error:\n\n> %s\n\nCheck the PR on the forge, then merge it yourself or add `%s` to retry.",
			pr.Number, retry.N, Defang(err.Error()), LReady)))
	}
	retry.Next = time.Now().Add(t.e.mergeBackoff(retry.N))
	return t.e.Q.SetMergeRetry(ctx, t.ref(), retry)
}

// mergeStopped forgets the retries once a person has the task, so a retry
// from them starts the count over.
func (t *task) mergeStopped(ctx context.Context, err error) error {
	if err != nil {
		return err
	}
	return t.e.Q.ClearMergeRetry(ctx, t.ref())
}

// mergeBackoff is the wait after the nth merge failure in a row: backoff.base,
// doubling each time up to backoff.cap.
func (e *Engine) mergeBackoff(n int) time.Duration {
	d := e.Cfg.Backoff.Base
	for i := 1; i < n && d < e.Cfg.Backoff.Cap; i++ {
		d *= 2
	}
	return min(d, e.Cfg.Backoff.Cap)
}

// mergeRefused stops a task whose merge the forge refused although the gate
// passed and the forge reported no conflict: that is the repo's own rules,
// which trying again won't change, so a person decides.
func (t *task) mergeRefused(ctx context.Context, pr *forge.PR, prot forge.Protection, err error) error {
	_, reviewer := t.p.BotUsers()
	var b strings.Builder
	fmt.Fprintf(&b, "devbot's merge gate passed for PR #%d, but the forge refused the merge into `%s`, most likely because of the repo's own rules. It said:\n\n> %s%s\n\n",
		pr.Number, pr.BaseBranch, Defang(err.Error()), ruleList(prot))
	b.WriteString("Either:\n\n")
	exempt := prot.Exempt
	if exempt == "" {
		exempt = fmt.Sprintf("Exempt devbot's reviewer, @%s, which merges, from the rules that block it.", reviewer)
	}
	fmt.Fprintf(&b, "1. **Let devbot merge past them.** %s Then add `%s` to retry.\n", exempt, LReady)
	fmt.Fprintf(&b, "2. **Leave it to a person.** Meet the rules yourself (approve PR #%d, if they ask for a review) and merge it.", pr.Number)
	return t.needsHuman(ctx, b.String())
}

// ruleList lists the rules the forge reports, after a blank line, or nothing.
func ruleList(prot forge.Protection) string {
	if len(prot.Rules) == 0 {
		return ""
	}
	return "\n\nThe rules the forge reports on the branch:\n\n- " + strings.Join(prot.Rules, "\n- ")
}

// missingChecks are the required checks no check on the head reports as.
func missingChecks(required []string, details []forge.CheckDetail) []string {
	var out []string
	for _, r := range required {
		if !slices.ContainsFunc(details, func(d forge.CheckDetail) bool { return d.Context == r }) {
			out = append(out, r)
		}
	}
	return out
}

// checksPending waits for checks still running on the PR's head, up to
// checks_pending_timeout from when devbot first saw that head's checks
// pending, or its last re-run of them, or the last retry, whichever is
// latest. It is per head, not from devbot's own last push, so a push by a
// person or the forge's "update branch" starts the wait over. A check that
// never finishes (a runner that died, a status left "waiting") would
// otherwise hold the task in review for good, and with it its base's rebase
// queue when it has the turn, so past that the task stops, naming them, and
// gives the turn up.
func (t *task) checksPending(ctx context.Context, pr *forge.PR, details []forge.CheckDetail) (bool, error) {
	if err := t.load(ctx); err != nil {
		return false, err
	}
	from, err := t.e.Q.PendingSince(ctx, t.ref(), pr.Number, pr.HeadSHA, time.Now(), t.rc.ChecksPendingTimeout)
	if err != nil {
		return false, err
	}
	for _, s := range t.ss {
		if (s.Kind == "reset" || (s.Kind == "rerun" && s.PR == pr.Number && s.SHA == pr.HeadSHA)) && s.CreatedAt.After(from) {
			from = s.CreatedAt
		}
	}
	if since(from) < t.rc.ChecksPendingTimeout {
		return false, nil
	}
	var stuck []string
	for _, d := range details {
		if d.Pending {
			stuck = append(stuck, Defang(d.Context))
		}
	}
	if err := t.e.leaveBase(ctx, pr.Repo, pr.BaseBranch, t.ref()); err != nil {
		return false, err
	}
	which := ":\n\n- `" + strings.Join(stuck, "`\n- `") + "`"
	if len(stuck) == 0 {
		// The forge said pending without saying which.
		which = "."
	}
	return true, t.needsHuman(ctx, fmt.Sprintf("Checks on PR #%d's head `%s` have been pending for more than %s (`checks_pending_timeout`)%s\n\nCI may have lost them (a runner that died, say). Get them to finish, by re-running them on the forge, then add `%s` to retry; or merge it by hand.",
		pr.Number, pr.HeadSHA[:min(12, len(pr.HeadSHA))], t.rc.ChecksPendingTimeout, which, LReady))
}

// headSince is when the PR's head got its last push from devbot, or the PR
// was opened: when its checks could first have reported.
func (t *task) headSince(pr *forge.PR) time.Time {
	at := pr.CreatedAt
	for _, s := range t.ss {
		if s.Kind == "pr" && s.PR == pr.Number && s.CreatedAt.After(at) {
			at = s.CreatedAt
		}
	}
	return at
}

// merged records a merge, whoever made it.
func (t *task) merged(ctx context.Context, pr *forge.PR) error {
	return t.recordMerge(ctx, pr, fmt.Sprintf("PR #%d merged", pr.Number))
}

// recordMerge closes the issue before clearing its state label: a close that
// failed leaves the label on, where reconcile finds the task and records the
// merge again, while an open issue without one is never looked at again (and
// on a base other than the default branch, "Closes #N" doesn't close it).
// Recording it twice is harmless: the event is once-only.
func (t *task) recordMerge(ctx context.Context, pr *forge.PR, summary string) error {
	t.closedPR(ctx, pr)
	if err := t.e.Q.ClearMergeRetry(ctx, t.ref()); err != nil {
		return err
	}
	if err := t.closeSuperseded(ctx, pr); err != nil {
		return err
	}
	if t.it.State == "open" {
		if err := t.p.CloseItem(ctx, t.it.Ref); err != nil {
			return err
		}
	}
	return t.e.transitionWithPR(ctx, t.p, t.it, "", "task.merged", summary, pr.URL)
}

// closed ends the task of a closed issue. A merged PR is recorded as merged,
// whatever state the task was in: a person merging from needs-human, or a
// merge devbot made but couldn't confirm, closing the issue with "Closes #N".
// A PR closed unmerged has its attachments released and its branch deleted
// before the task is cancelled; an open PR keeps both, or its body's images
// would break.
func (t *task) closed(ctx context.Context) (bool, error) {
	pr, err := t.findPR(ctx)
	if err != nil {
		return false, err
	}
	if pr != nil {
		if pr.Merged {
			err := t.merged(ctx, pr)
			t.landed(ctx, pr)
			return true, err
		}
		if pr.State != "open" {
			t.closedPR(ctx, pr)
		}
	}
	err = t.e.transition(ctx, t.p, t.it, "", "task.cancelled", "issue closed")
	t.wakeWaiters(ctx)
	return true, err
}

func (t *task) needsHuman(ctx context.Context, why string) error {
	if t.state == LNeedsHuman {
		return nil
	}
	said, err := t.saidLast(ctx, "needs-human", why)
	if err != nil {
		return err
	}
	if !said {
		if err := t.p.Comment(ctx, t.it.Ref, why+"\n\n"+RenderStatus(Status{Kind: "needs-human"})); err != nil {
			return err
		}
	}
	return t.e.transition(ctx, t.p, t.it, LNeedsHuman, "task.needs_human", firstLine(why))
}

// saidLast reports whether the task's latest status is already kind, saying
// text: the comment an earlier evaluation posted before a label write that
// failed, which this one only has to finish. Comments are re-read first, as
// in postStatus. Unlike postStatus, any status since counts as moving on, so
// the same stop for a new reason, or again after a person moved the task
// on, is still said.
func (t *task) saidLast(ctx context.Context, kind, text string) (bool, error) {
	t.comments = nil
	if err := t.load(ctx); err != nil {
		return false, err
	}
	if len(t.ss) == 0 {
		return false, nil
	}
	s := t.ss[len(t.ss)-1]
	return s.Kind == kind && stripMarkers(s.Body) == strings.TrimSpace(text), nil
}

// findPR locates the task's PR: the PR it carries, else the latest "pr"
// status marker, else an open PR the implementer bot opened from
// devbot/<number>-* in the repo itself.
// Anyone can open a PR from a branch of that name on a fork, and devbot
// would push to and retitle whatever it adopts.
func (t *task) findPR(ctx context.Context) (*forge.PR, error) {
	pr, err := t.lookupPR(ctx)
	// An evaluation keeps the index webhooks are filtered by (concerns)
	// up to date, and a reconcile pass, which evaluates every task, puts
	// it back after a Redis wipe. FindPR's callers aren't evaluations, and
	// a job pod may not write it.
	if pr != nil && t.src != "" {
		if terr := t.e.Q.TrackPR(ctx, pr.Repo.String(), pr.Number, pr.HeadSHA); terr != nil {
			t.e.Log.Warn("tracking the PR", "task", t.ref(), "pr", pr.Number, "err", terr)
		}
	}
	return pr, err
}

func (t *task) lookupPR(ctx context.Context) (*forge.PR, error) {
	if err := t.load(ctx); err != nil {
		return nil, err
	}
	// A carried PR is the task's whatever else is open: its tracking issue
	// has no branch of its own.
	m := t.e.meta(ctx, t.it, t.comments)
	if n := m.Carry; n > 0 {
		pr, err := t.p.GetPR(ctx, t.it.Ref.Repo, n)
		if errors.Is(err, forge.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &pr, nil
	}
	// A "pr" status for the fork's PR itself is from before devbot moved it
	// onto a branch of its own (SupersedeCarry): devbot doesn't push there.
	if s, ok := lastStatus(t.ss, "pr"); ok && s.PR > 0 && s.PR != m.Supersedes {
		pr, err := t.p.GetPR(ctx, t.it.Ref.Repo, s.PR)
		if err == nil {
			return &pr, nil
		}
		if !errors.Is(err, forge.ErrNotFound) {
			return nil, err
		}
	}
	prs, err := t.p.ListOpenPRs(ctx, t.it.Ref.Repo)
	if err != nil {
		return nil, err
	}
	prefix := fmt.Sprintf("devbot/%d-", t.it.Ref.Number)
	// A fork's PR carried on a branch of devbot's own has its branch by
	// that PR's number.
	supersede := ""
	if m.Supersedes > 0 {
		supersede = SupersedeBranch(m.Supersedes)
	}
	impl, _ := t.p.BotUsers()
	for i := range prs {
		if (strings.HasPrefix(prs[i].HeadBranch, prefix) || (supersede != "" && prs[i].HeadBranch == supersede)) && forge.SameLogin(prs[i].Author, impl) && !prs[i].Fork {
			// A listed PR may lack what only a single read has (GitHub's
			// mergeability).
			pr, err := t.p.GetPR(ctx, t.it.Ref.Repo, prs[i].Number)
			if err != nil {
				return nil, err
			}
			return &pr, nil
		}
	}
	return nil, nil
}

// FindPR is findPR for callers outside an evaluation (the harness, status).
func (e *Engine) FindPR(ctx context.Context, it forge.Item) (*forge.PR, error) {
	p, err := e.provider(it.Ref.Repo)
	if err != nil {
		return nil, err
	}
	t := &task{e: e, p: p, it: it}
	return t.findPR(ctx)
}

// Transition swaps the issue's state label to `to` ("" = none) and, when
// eventType is set, emits an event.
func (e *Engine) transition(ctx context.Context, p forge.Provider, it forge.Item, to, eventType, summary string) error {
	return e.transitionWithPR(ctx, p, it, to, eventType, summary, "")
}

func (e *Engine) Transition(ctx context.Context, it forge.Item, to, eventType, summary string) error {
	p, err := e.provider(it.Ref.Repo)
	if err != nil {
		return err
	}
	return e.transition(ctx, p, it, to, eventType, summary)
}

func (e *Engine) transitionWithPR(ctx context.Context, p forge.Provider, it forge.Item, to, eventType, summary, prURL string) error {
	return e.transitionOnce(ctx, p, it, to, eventType, summary, prURL, "")
}

// transitionOnce is transitionWithPR whose event, when once is set, is
// emitted at most once per task, type and once key, however many evaluations
// race to record it.
func (e *Engine) transitionOnce(ctx context.Context, p forge.Provider, it forge.Item, to, eventType, summary, prURL, once string) error {
	// The evaluation may have read the routing comment since ctx was made.
	ctx = e.TaskContext(ctx, it)
	var remove []string
	for _, l := range StateLabels {
		if l != to && it.HasLabel(l) {
			remove = append(remove, l)
		}
	}
	var add []string
	if to != "" && !it.HasLabel(to) {
		add = []string{to}
	}
	set := func() error { return p.SetLabels(ctx, it.Ref, add, remove) }
	var err error
	switch {
	case len(add) > 0:
		err = e.withLabels(ctx, it.Ref.Repo, set)
	case len(remove) > 0:
		err = set()
	}
	if len(add)+len(remove) > 0 {
		impl, _ := p.BotUsers()
		e.auditTransition(ctx, it, impl, string(forge.RoleImplementer), to, eventType, summary, err)
	}
	if err != nil {
		return err
	}
	if eventType != "" {
		e.emit(ctx, it, eventType, to, summary, prURL, once)
	}
	return nil
}

// EventTypes are every type of event the feed carries, in a task's usual
// order, and a parent task's (see parent.go) after them. The MCP events
// tool, the UI's filter and docs/connectors.md list them from here;
// internal/config/docs_test.go checks every type the code emits is one of
// them.
var EventTypes = []string{"task.filed", "task.started", "task.question", "task.answered", "task.pr_opened",
	"task.changes_requested", "task.needs_approval", "task.approved", "task.merged", "task.needs_human", "task.cancelled", "task.retried",
	"task.ignored", "task.retargeted", "task.edited", "task.waiting", "task.released", "task.note",
	"parent.filed", "parent.plan_proposed", "parent.plan_approved", "parent.needs_human", "parent.completed", "parent.cancelled"}

// Emit appends an event for the task to the feed.
func (e *Engine) Emit(ctx context.Context, it forge.Item, typ, state, summary, prURL string) {
	e.emit(ctx, it, typ, state, summary, prURL, "")
}

// meta is the task's routing block, read from comments when the caller has
// them and from the forge otherwise. Only a found block is remembered: one
// read just after filing may come before the bot's comment lands.
func (e *Engine) meta(ctx context.Context, it forge.Item, comments []forge.Comment) Meta {
	key := it.Ref.String()
	// Comments already read are the word: another process may have moved a
	// carried PR onto a branch of devbot's own since this one cached it.
	if comments != nil {
		if p, err := e.provider(it.Ref.Repo); err == nil {
			if m, ok := metaOf(comments, e.bots(p)...); ok {
				e.metas.put(key, m)
				return m
			}
		}
	}
	if m, ok := e.metas.get(key); ok {
		return m
	}
	p, err := e.provider(it.Ref.Repo)
	if err != nil {
		return Meta{}
	}
	if comments == nil {
		if comments, err = p.ListComments(ctx, it.Ref); err != nil {
			e.Log.Warn("reading a task's routing", "task", it.Ref, "err", err)
			return Meta{}
		}
	}
	m, ok := metaOf(comments, e.bots(p)...)
	if ok {
		e.metas.put(key, m)
	}
	return m
}

// Meta is meta for callers outside the engine (the harness).
func (e *Engine) Meta(ctx context.Context, it forge.Item, comments []forge.Comment) Meta {
	return e.meta(ctx, it, comments)
}

func (e *Engine) emit(ctx context.Context, it forge.Item, typ, state, summary, prURL, once string) {
	m := e.meta(ctx, it, nil)
	// A carried PR is known from the start, so every event shows it.
	if prURL == "" && m.Carry > 0 {
		if p, err := e.provider(it.Ref.Repo); err == nil {
			prURL = p.PRURL(it.Ref.Repo, m.Carry)
		}
	}
	ev := queue.Event{
		Type: typ, Task: it.Ref.String(), PR: prURL, State: strings.TrimPrefix(state, "devbot/"),
		Origin: m.Origin, Requester: m.Requester, Summary: summary, URL: it.URL, Parent: m.Parent,
	}
	if m.Kind != KindParent {
		ev.Base = e.shownBase(ctx, it, nil)
	}
	if ev.State == "" {
		ev.State = "none"
	}
	// A terminal event happens once per task and PR, however many
	// evaluations race to record it.
	if typ == "task.merged" && once == "" {
		once = prURL
	}
	if once != "" {
		once = typ + "|" + ev.Task + "|" + once
	}
	if _, _, err := e.Q.EmitOnce(ctx, ev, once, e.Cfg.EventRetention); err != nil {
		e.Log.Warn("emit failed", "task", it.Ref, "type", typ, "err", err)
	}
	// Whichever process moved a child, a job pod's included, serve then
	// rolls its parent up again (EvaluateRequested); an evaluation that moved
	// it has done so already (parentMoved).
	if _, err := forge.ParseItem(m.Parent); err == nil {
		if err := e.Q.RequestEval(ctx, m.Parent); err != nil {
			e.Log.Warn("asking for a parent task's evaluation", "task", it.Ref, "parent", m.Parent, "err", err)
		}
	}
}

// Reconcile re-evaluates every item that carries a devbot label, open or
// closed, releases due delayed jobs, drops dedupe entries no job stands
// behind and sweeps up the branches finished tasks left behind. It returns
// how many items it changed.
//
// Finding the items and sweeping are low priority (forge.LowPriority):
// while little is left of the forge's quota they are deferred, and a pass
// after the reset makes up for them, so what is left goes to the tasks'
// own evaluations, merges and jobs.
func (e *Engine) Reconcile(ctx context.Context) (int, error) {
	if _, err := e.Q.ReleaseDue(ctx); err != nil {
		e.Log.Warn("release delayed jobs", "err", err)
	}
	// With no daemon in the install, nothing takes the Docker lists' jobs,
	// and their dedupe entries would keep those tasks from being queued
	// again, so they go where the install's pods look.
	if e.Cfg.Docker.Mode == "" {
		if n, err := e.Q.Undock(ctx); err != nil {
			e.Log.Warn("move Docker jobs to the plain lists", "err", err)
		} else if n > 0 {
			e.Log.Warn("moved Docker jobs to the plain lists: this install runs no Docker daemon", "n", n)
		}
	}
	// Before the evaluations, so a task a stale entry blocked is queued in
	// this same pass.
	if n, err := e.Q.PruneQueued(ctx); err != nil {
		e.Log.Warn("prune dedupe entries", "err", err)
	} else if n > 0 {
		e.Log.Warn("dropped stale dedupe entries", "n", n)
	}
	seen := map[string]bool{}
	acted := 0
	var errs []error
	for _, n := range e.Forges.Names() {
		p := e.Forges[n]
		// Every pass, so they are back soon after a Redis wipe.
		impl, rev := p.BotUsers()
		if err := e.Q.SetBots(ctx, n, impl, rev); err != nil {
			errs = append(errs, fmt.Errorf("record %s's bot logins: %w", n, err))
		}
		// PRs handed over first: the tasks they get are evaluated with the
		// rest below.
		did, cerrs := e.searchCarry(ctx, n, p)
		acted += did
		errs = append(errs, cerrs...)
		// Open parent tasks too, which roll up their children.
		for _, l := range append(slices.Clone(StateLabels), LParent) {
			for _, state := range []string{"open", "closed"} {
				if state == "closed" && (l == LNeedsHuman || l == LParent) {
					continue
				}
				// What a failing repo or installation hides is skipped
				// this pass; the rest is still evaluated.
				items, err := p.SearchItems(forge.LowPriority(ctx), l, state)
				if err := e.discoveryFailed(ctx, n, err); err != nil {
					errs = append(errs, fmt.Errorf("search %s %s: %w", l, state, err))
				}
				for _, it := range items {
					if seen[it.Ref.String()] {
						continue
					}
					seen[it.Ref.String()] = true
					did, err := e.EvaluateItem(ctx, it.Ref, SrcReconcile)
					if err != nil {
						errs = append(errs, fmt.Errorf("%s: %w", it.Ref, err))
					}
					if did {
						acted++
						e.Count(ctx, "reconcile_actions", []string{strings.TrimPrefix(l, "devbot/")}, 1)
					}
				}
			}
		}
	}
	if err := e.sweepBranches(forge.LowPriority(ctx)); err != nil && !deferred(err) {
		errs = append(errs, err)
	}
	return acted, errors.Join(errs...)
}

// discoveryFailed counts what a reconcile search of provider n failed
// with, and returns it, unless all it held was calls the forge's quota put
// off (deferred), which are counted apart and aren't an error.
func (e *Engine) discoveryFailed(ctx context.Context, n string, err error) error {
	switch {
	case err == nil:
		return nil
	case deferred(err):
		e.Count(ctx, "reconcile_deferred", []string{n}, 1)
		return nil
	}
	e.Count(ctx, "search_errors", []string{n}, 1)
	return err
}

// deferred reports whether err is low-priority forge calls put off for a
// low quota (forge.LimitLowQuota) and nothing else.
func deferred(err error) bool {
	switch u := err.(type) {
	case *forge.RateLimitError:
		return u.Kind == forge.LimitLowQuota
	case interface{ Unwrap() []error }:
		for _, err := range u.Unwrap() {
			if !deferred(err) {
				return false
			}
		}
		return len(u.Unwrap()) > 0
	case interface{ Unwrap() error }:
		return deferred(u.Unwrap())
	}
	return false
}

// evalRetryAfter is how long a requested evaluation that failed waits to be
// tried again: long enough for a forge that is down not to be asked every
// second.
const evalRetryAfter = 30 * time.Second

// EvaluateRequested evaluates the tasks job pods asked serve to (see
// queue.RequestEval), and returns how many it changed. A request that isn't
// a task ref is dropped: anything in a job pod can reach Redis. One whose
// evaluation fails is asked for again, after evalRetryAfter.
func (e *Engine) EvaluateRequested(ctx context.Context) (int, error) {
	refs, err := e.Q.TakeEvalRequests(ctx, 50)
	if err != nil {
		return 0, err
	}
	acted := 0
	var errs []error
	for _, s := range refs {
		ref, err := forge.ParseItem(s)
		if err != nil {
			continue
		}
		did, err := e.EvaluateItem(ctx, ref, SrcJob)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ref, err))
			// Not before a rate limit that stopped it resets.
			at := time.Now().Add(evalRetryAfter)
			if rl := (*forge.RateLimitError)(nil); errors.As(err, &rl) && rl.Until.After(at) {
				at = rl.Until
			}
			if rerr := e.Q.RetryEval(context.WithoutCancel(ctx), s, at); rerr != nil {
				errs = append(errs, fmt.Errorf("%s: asking again: %w", ref, rerr))
			}
		}
		if did {
			acted++
		}
	}
	return acted, errors.Join(errs...)
}

// maxLineComments bounds the line comments one review adds to a fix round's
// reason, which is posted on the issue: a bot that comments on every line
// can't push the rest out of it.
const maxLineComments = 16 << 10

// lineComments lists the comments review left on lines, for a fix round's
// reason: the newest that fit in maxLineComments, and how many didn't.
func lineComments(all []forge.ReviewComment, review int64) string {
	if review == 0 {
		return ""
	}
	var lines []string
	for _, c := range all {
		if c.ReviewID != review {
			continue
		}
		loc := c.Path
		if c.Line > 0 {
			loc = fmt.Sprintf("%s:%d", c.Path, c.Line)
		}
		lines = append(lines, fmt.Sprintf("- `%s`: %s\n", Defang(strings.ReplaceAll(loc, "`", "'")), Defang(c.Body)))
	}
	n, kept := 0, len(lines)
	for kept > 0 && n+len(lines[kept-1]) <= maxLineComments {
		kept--
		n += len(lines[kept])
	}
	if len(lines) == 0 {
		return ""
	}
	out := "On lines:\n\n" + strings.Join(lines[kept:], "")
	if kept > 0 {
		out += fmt.Sprintf("- ...and %d older line comments, left out for size.\n", kept)
	}
	return out + "\n"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return s
}

func stripMarkers(s string) string { return strings.TrimSpace(markerRE.ReplaceAllString(s, "")) }

// StatusComment is a devbot comment with its parsed marker.
type StatusComment = statusComment

// Statuses parses the status markers on comments posted by the bots.
func Statuses(cs []forge.Comment, bots ...string) []StatusComment { return statuses(cs, bots...) }

// FailedAttempts counts failed implement attempts since the last retry.
func FailedAttempts(ss []StatusComment) int {
	n := 0
	for _, s := range ss {
		switch s.Kind {
		case "reset":
			n = 0
		case "failed":
			n++
		}
	}
	return n
}

// ReviewRound is the review round a PR is in: one more than the rounds it
// has used up since the last retry. A clean rebase uses none, nor does
// following a retarget or an edit_task.
func ReviewRound(ss []StatusComment) int {
	rounds, _ := roundCounts(ss, "")
	return rounds + 1
}

// LastStatus returns the most recent status of one of kinds.
func LastStatus(ss []StatusComment, kinds ...string) (StatusComment, bool) {
	return lastStatus(ss, kinds...)
}

// NeedsHuman stops a task and says why.
func (e *Engine) NeedsHuman(ctx context.Context, it forge.Item, why string) error {
	p, err := e.provider(it.Ref.Repo)
	if err != nil {
		return err
	}
	rc, err := e.Repos.Get(ctx, it.Ref.Repo)
	if err != nil {
		return err
	}
	t := &task{e: e, p: p, rc: rc, it: it, state: State(it)}
	return t.needsHuman(ctx, why)
}

// Authorized reports whether actor may direct work on repo.
func (e *Engine) Authorized(ctx context.Context, repo forge.RepoRef, actor string) bool {
	p, err := e.provider(repo)
	if err != nil {
		return false
	}
	rc, err := e.Repos.Get(ctx, repo)
	if err != nil {
		return false
	}
	return e.authorized(ctx, p, rc, repo, actor)
}

// TrustedReviewer reports whether actor is on repo's trusted_reviewers or
// required_reviewers: their reviews and comments on a PR are feedback to act
// on, though they can't direct work.
func (e *Engine) TrustedReviewer(ctx context.Context, repo forge.RepoRef, actor string) bool {
	rc, err := e.Repos.Get(ctx, repo)
	if err != nil {
		return false
	}
	return forge.HasLogin(rc.TrustedReviewers, actor) || forge.HasLogin(rc.RequiredReviewers, actor)
}

// Bots returns the provider's bot logins.
func (e *Engine) Bots(repo forge.RepoRef) []string {
	p, err := e.provider(repo)
	if err != nil {
		return nil
	}
	return e.bots(p)
}

// TransitionWithPR is Transition with the PR's URL on the event.
func (e *Engine) TransitionWithPR(ctx context.Context, it forge.Item, to, eventType, summary, prURL string) error {
	p, err := e.provider(it.Ref.Repo)
	if err != nil {
		return err
	}
	return e.transitionWithPR(ctx, p, it, to, eventType, summary, prURL)
}

// releaseAttachments frees the storage a closed or merged PR's attachments
// take outside the PR. It runs before the transition that ends the task, so
// a crash in between repeats it rather than skipping it. A failure costs
// only storage, so it is logged, not returned.
func (t *task) releaseAttachments(ctx context.Context, pr *forge.PR) {
	if err := t.p.ReleaseAttachments(ctx, pr.Repo, pr.Number); err != nil {
		t.e.Log.Warn("releasing a closed PR's attachments failed", "task", t.it.Ref, "pr", pr.Number, "err", err)
	}
}

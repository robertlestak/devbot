package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/audit"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// These are what devbot's connectors (MCP, HTTP, CLI) do. Each is a thin
// write to the forge followed by an evaluation, so a connector call and a
// person clicking in the forge UI end up on the same path.

type FileRequest struct {
	Repo               string `json:"repo"`
	Title              string `json:"title"`
	Body               string `json:"body"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
	Origin             string `json:"origin,omitempty"`
	RequesterHandle    string `json:"requester_handle,omitempty"`
	Automerge          bool   `json:"automerge,omitempty"`
	// Base is the branch to work from and open the PR against; when
	// empty, the repo's base_branch, else its default branch.
	Base string `json:"base,omitempty"`
	// Parent is the parent task (see FileProject) this one is a child of,
	// in any repo: "#12" in this one, a full ref in another.
	Parent string `json:"parent,omitempty"`
	// planned is set for a child a parent task's approved plan files: never
	// by a caller.
	planned *plannedChild
}

type TaskStatus struct {
	Task       string   `json:"task"`
	Repo       string   `json:"repo"`
	Title      string   `json:"title"`
	State      string   `json:"state"`
	IssueState string   `json:"issue_state"`
	URL        string   `json:"url"`
	PR         string   `json:"pr,omitempty"`
	Origin     string   `json:"origin,omitempty"`
	Requester  string   `json:"requester,omitempty"`
	LastUpdate string   `json:"last_update,omitempty"`
	Automerge  bool     `json:"automerge"`
	Labels     []string `json:"labels,omitempty"`
	Note       string   `json:"note,omitempty"`
	// Base is the branch the task's work goes into, when it isn't the
	// repo's default branch.
	Base string `json:"base,omitempty"`
	// WaitingOn is the task this one waits on to merge first (see
	// WaitTask), until devbot releases it. WaitingFor is what it waits for
	// now: "merge", or, for a wait with a release gate (WaitUntil, "tagged"
	// and any tag pattern) on a task that has merged, "tag".
	WaitingOn  string `json:"waiting_on,omitempty"`
	WaitingFor string `json:"waiting_for,omitempty"`
	WaitUntil  string `json:"wait_until,omitempty"`
	// BuildAgainst, for a wait that builds against the task it waits on, is
	// where its runs get that task's unmerged PR head checked out (see
	// Engine.BuildAgainst). While the wait stands, the task's own PR isn't
	// reviewed, merged or sent back for its CI, and Note says so.
	BuildAgainst string `json:"build_against,omitempty"`
	// Held is the change a task in needs-approval holds, as devbot showed it
	// for a person to approve (see Approve).
	Held *HeldChange `json:"held,omitempty"`
	// Kind is KindParent for a parent task, whose State is rolled up from
	// its Children (ParentInProgress and the rest), not a label.
	Kind string `json:"kind,omitempty"`
	// Parent is the parent task this one is a child of.
	Parent   string       `json:"parent,omitempty"`
	Children []TaskStatus `json:"children,omitempty"`

	// Filled in for the web UI (ListTasksDetailed, TaskDetail) only: what
	// they cost in forge calls per task is too much for every list.
	PRNumber   int        `json:"pr_number,omitempty"`
	StateSince *time.Time `json:"state_since,omitempty"`
}

// Caller is who a connector call is for.
type Caller struct {
	// Principal is recorded as the task's requester.
	Principal string
	// User is set when the caller signed in through the forge (auth.mode
	// oauth). What they start is then done on the forge with their own
	// token, as them, exactly as if they had done it in the forge UI, and
	// they only see tasks on repos they can read. Without it (auth.mode
	// none), the implementer bot does it and records Principal.
	User forge.User
	// Provider and Login are the forge account behind User.
	Provider, Login string
}

// ErrRefused is a connector call devbot or the forge won't do for the
// caller. Nothing was written.
var ErrRefused = errors.New("refused")

// ErrInvalid is a connector call that is wrong in itself: a missing field,
// a malformed ref, or a task in the wrong state for it. Nothing was written.
var ErrInvalid = errors.New("invalid request")

// invalid marks err as ErrInvalid, keeping its message: the caller sees it.
func invalid(err error) error { return invalidErr{err} }

type invalidErr struct{ error }

func (e invalidErr) Unwrap() []error { return []error{e.error, ErrInvalid} }

// mayDirect checks, before anything is written, that a signed-in caller may
// direct devbot on repo, by the rule the engine applies to whoever adds
// devbot/ready. A call that would be ignored is refused instead of leaving
// an ignored issue or comment behind.
func (e *Engine) mayDirect(ctx context.Context, c Caller, p forge.Provider, rc *config.Repo, repo forge.RepoRef, action string) error {
	if ok, err := e.CanRead(ctx, c, repo); err != nil || !ok {
		if err == nil {
			e.auditRefusal(ctx, callerWho(c), action, "repo not visible to the caller", audit.Fields{Repo: repo.String()})
		}
		return notFound(repo, err)
	}
	if ok, why := e.authorize(ctx, p, rc, repo, c.Login); !ok {
		e.auditRefusal(ctx, callerWho(c), action, why, audit.Fields{Repo: repo.String()})
		return fmt.Errorf("%w: %s", ErrRefused, why)
	}
	return nil
}

// refused words a write the forge turned down for the caller.
func refused(err error) error {
	if errors.Is(err, forge.ErrForbidden) {
		return fmt.Errorf("%w: the forge doesn't allow you to do that: %w", ErrRefused, err)
	}
	return err
}

// FileTask creates the issue, labelled ready, and queues it. A rate limit
// before the issue exists says that nothing was filed, so the caller knows
// that trying again later won't file it twice; one after names the issue.
func (e *Engine) FileTask(ctx context.Context, req FileRequest, c Caller) (_ TaskStatus, err error) {
	var it forge.Item
	defer func() {
		var rl *forge.RateLimitError
		if it.URL == "" && errors.As(err, &rl) {
			err = fmt.Errorf("nothing was filed: %w", err)
		}
	}()
	repo, err := forge.ParseRepo(req.Repo)
	if err != nil {
		return TaskStatus{}, invalid(err)
	}
	if strings.TrimSpace(req.Title) == "" {
		return TaskStatus{}, invalid(errors.New("title is required"))
	}
	if strings.TrimSpace(req.AcceptanceCriteria) == "" {
		return TaskStatus{}, invalid(errors.New("acceptance_criteria is required: say what done looks like, so the implementer and the reviewer can both check it"))
	}
	p, err := e.provider(repo)
	if err != nil {
		return TaskStatus{}, invalid(err)
	}
	if h := req.RequesterHandle; h != "" && !config.ValidName(h) {
		return TaskStatus{}, invalid(fmt.Errorf("requester_handle is %q; want %s", h, config.NameRule))
	}
	ctx = audit.With(ctx, audit.Fields{Repo: repo.String(), Origin: req.Origin})
	if ok, err := e.CanRead(ctx, c, repo); err != nil || !ok {
		if err == nil {
			e.auditRefusal(ctx, callerWho(c), "file_task", "repo not visible to the caller", audit.Fields{})
		}
		return TaskStatus{}, notFound(repo, err)
	}
	rc, err := e.Repos.Get(ctx, repo)
	if err != nil {
		return TaskStatus{}, fmt.Errorf("%s: %w", repo, err)
	}
	if err := e.mayLabel(ctx, c, p, rc, repo, "file_task", LReady); err != nil {
		return TaskStatus{}, err
	}
	base := strings.TrimSpace(req.Base)
	if base != "" {
		if err := checkBase(ctx, p, repo, base); err != nil {
			return TaskStatus{}, err
		}
	}
	var parent forge.Item
	var pp forge.Provider
	if strings.TrimSpace(req.Parent) != "" {
		if parent, pp, err = e.parentFor(ctx, c, req.Parent, repo); err != nil {
			return TaskStatus{}, err
		}
	}
	// Creating devbot's labels is the bot's job, whoever files.
	if err := e.EnsureLabels(ctx, repo); err != nil {
		return TaskStatus{}, err
	}
	body := taskBody(Defang(req.Body), Defang(req.AcceptanceCriteria))
	// A signed-in person's issue is visibly theirs, so the comment only
	// says what it is, and the handle to mention is their verified login, not
	// whatever the client claims. A principal is only a name, so the bot
	// says who it filed for.
	handle := req.RequesterHandle
	var note string
	switch {
	case c.User != nil:
		handle = c.Login
		note = "devbot is tracking this task."
	case handle != "":
		note = fmt.Sprintf("Requested by @%s via %s.", handle, Defang(c.Principal))
	default:
		note = fmt.Sprintf("Filed via %s.", Defang(c.Principal))
	}
	if parent.URL != "" {
		note += fmt.Sprintf(" It is a child task of %s, part of a change that parent task follows across repos.", parent.Ref)
	}

	labels := []string{LReady}
	if req.Automerge {
		labels = append(labels, LAutomerge)
	}
	if it, err = e.createLabelled(ctx, c, p, repo, strings.TrimSpace(req.Title), body, labels); err != nil {
		return TaskStatus{}, err
	}
	// closeAgain closes the issue just filed, which a caller who saw this
	// call fail would otherwise file again: as its author first, as with a
	// dropped label, and the bot can try too.
	closeAgain := func(what string, err error) error {
		e.Log.Warn("recording a filed task's "+what, "task", it.Ref, "err", err)
		if c.User == nil || c.User.CloseItem(ctx, it.Ref) != nil {
			if cerr := p.CloseItem(ctx, it.Ref); cerr != nil {
				e.Log.Warn("closing an issue filed without its "+what, "task", it.Ref, "err", cerr)
				return fmt.Errorf("devbot filed %s but couldn't record its %s (%w), and couldn't close it: close it yourself before filing again", it.URL, what, err)
			}
		}
		return fmt.Errorf("devbot filed %s but couldn't record its %s (%w), so it closed it again: file it again", it.URL, what, err)
	}
	// The routing goes in the bot's own comment: the issue's author can edit
	// its body, and nobody but the bot (and admins) can edit this.
	m := Meta{Origin: req.Origin, Requester: c.Principal, RequesterHandle: handle, Base: base}
	if parent.URL != "" {
		m.Parent = parent.Ref.String()
	}
	if err := p.Comment(ctx, it.Ref, note+"\n\n"+RenderMeta(m)); err != nil {
		switch {
		case base != "":
			// Without its base the issue would be worked on against the
			// default branch.
			return TaskStatus{}, closeAgain(fmt.Sprintf("base %q", base), err)
		case m.Parent != "":
			return TaskStatus{}, closeAgain("parent "+m.Parent, err)
		}
		return TaskStatus{}, fmt.Errorf("filed %s, but recording who it is for failed: %w", it.URL, err)
	}
	e.metas.put(it.Ref.String(), m)
	if base != "" {
		e.bases.put(it.Ref.String(), base)
	}
	if parent.URL != "" {
		if err := e.recordChild(ctx, parent, pp, it, req.planned); err != nil {
			return TaskStatus{}, closeAgain("place on parent task "+m.Parent, err)
		}
	}
	// A plan's child waits from the start: nothing is queued for it first.
	if pc := req.planned; pc != nil && pc.wait != nil && pc.wait.On.Number > 0 {
		t := &task{e: e, p: p, rc: rc, it: it, state: LReady}
		if err := t.load(ctx); err != nil {
			return TaskStatus{}, err
		}
		if err := t.setWait(ctx, *pc.wait, "the plan of parent task "+RefText(repo, parent.Ref), c.Principal, "", "", director{all: true}); err != nil {
			return TaskStatus{}, err
		}
	}
	e.Emit(ctx, it, "task.filed", LReady, it.Title, "")
	if _, err := e.EvaluateItem(ctx, it.Ref, SrcAPI); err != nil {
		e.Log.Warn("evaluate after file", "task", it.Ref, "err", err)
	}
	if parent.URL != "" {
		if _, err := e.EvaluateItem(ctx, parent.Ref, SrcAPI); err != nil {
			e.Log.Warn("evaluate a parent task after filing its child", "task", it.Ref, "parent", parent.Ref, "err", err)
		}
	}
	st := e.statusOf(ctx, it, nil)
	if req.Automerge && !rc.Automerge && !*rc.TaskAutomerge {
		st.Note = "automerge won't apply: this repo sets task_automerge: false, so the PR will wait at devbot/approved for a person to merge"
	}
	return st, nil
}

// Answer relays an answer to a task waiting on a question. A signed-in
// caller's answer is their own comment, which the engine takes as an answer
// like any authorized reply on the issue.
func (e *Engine) Answer(ctx context.Context, refStr, text string, c Caller) (TaskStatus, error) {
	it, p, rc, err := e.load(ctx, c, refStr)
	if err != nil {
		return TaskStatus{}, err
	}
	ctx = e.TaskContext(ctx, it)
	if State(it) != LNeedsInfo {
		return TaskStatus{}, invalid(fmt.Errorf("%s is %q, not waiting for an answer", it.Ref, State(it)))
	}
	if strings.TrimSpace(text) == "" {
		return TaskStatus{}, invalid(errors.New("text is required"))
	}
	if c.User != nil {
		if err := e.mayDirect(ctx, c, p, rc, it.Ref.Repo, "answer"); err != nil {
			return TaskStatus{}, err
		}
		if err := c.User.Comment(ctx, it.Ref, strings.TrimSpace(text)); err != nil {
			return TaskStatus{}, refused(err)
		}
	} else {
		quoted := quote(text)
		body := fmt.Sprintf("**Answer relayed from %s:**\n\n%s\n\n%s", Defang(c.Principal), quoted, RenderStatus(Status{Kind: "answer", Principal: c.Principal}))
		if err := p.Comment(ctx, it.Ref, body); err != nil {
			return TaskStatus{}, err
		}
	}
	if _, err := e.EvaluateItem(ctx, it.Ref, SrcAPI); err != nil {
		return TaskStatus{}, err
	}
	return e.Status(ctx, c, refStr)
}

// Cancel takes devbot off a task. A running job notices on its next
// heartbeat. A signed-in caller removes the state label themselves.
func (e *Engine) Cancel(ctx context.Context, refStr string, c Caller) (TaskStatus, error) {
	it, p, rc, err := e.load(ctx, c, refStr)
	if err != nil {
		return TaskStatus{}, err
	}
	ctx = e.TaskContext(ctx, it)
	if isParent(it, e.meta(ctx, it, nil)) {
		return e.cancelParent(ctx, c, it, p, rc)
	}
	if State(it) == "" {
		return e.statusOf(ctx, it, nil), nil
	}
	if c.User != nil {
		if err := e.mayDirect(ctx, c, p, rc, it.Ref.Repo, "cancel"); err != nil {
			return TaskStatus{}, err
		}
		err := c.User.SetLabels(ctx, it.Ref, nil, stateLabelsOn(it, ""))
		e.auditTransition(ctx, it, c.Login, "user", "", "task.cancelled", "cancelled by "+c.Principal, err)
		if err != nil {
			return TaskStatus{}, refused(err)
		}
		e.Emit(ctx, it, "task.cancelled", "", "cancelled by "+c.Principal, "")
		return e.Status(ctx, c, refStr)
	}
	// A cancel whose label write failed is asked again: its comment stands.
	text := fmt.Sprintf("Cancelled by %s.", Defang(c.Principal))
	t := &task{e: e, p: p, rc: rc, it: it, state: State(it)}
	said, err := t.saidLast(ctx, "cancelled", text)
	if err != nil {
		return TaskStatus{}, err
	}
	if !said {
		if err := p.Comment(ctx, it.Ref, text+"\n\n"+RenderStatus(Status{Kind: "cancelled", Principal: c.Principal})); err != nil {
			return TaskStatus{}, err
		}
	}
	if err := e.transition(ctx, p, it, "", "task.cancelled", "cancelled by "+c.Principal); err != nil {
		return TaskStatus{}, err
	}
	return e.Status(ctx, c, refStr)
}

// Retry restarts a task: from devbot's "reset" status, attempts, review
// rounds and rebase rounds count from zero. A signed-in caller puts
// devbot/ready back themselves, but the bot still posts the reset, and
// before the relabel: an evaluation the relabel sets off must not start an
// attempt, or give up, on the old counts. If the relabel then fails, the
// next retry finds the reset already there.
func (e *Engine) Retry(ctx context.Context, refStr string, c Caller) (TaskStatus, error) {
	it, p, rc, err := e.load(ctx, c, refStr)
	if err != nil {
		return TaskStatus{}, err
	}
	ctx = e.TaskContext(ctx, it)
	if it.State != "open" {
		return TaskStatus{}, invalid(fmt.Errorf("%s is closed", it.Ref))
	}
	if isParent(it, e.meta(ctx, it, nil)) {
		return e.retryParent(ctx, c, it, p, rc)
	}
	if c.User != nil {
		if err := e.mayDirect(ctx, c, p, rc, it.Ref.Repo, "retry"); err != nil {
			return TaskStatus{}, err
		}
		t := &task{e: e, p: p, rc: rc, it: it, state: State(it)}
		if err := t.retried(ctx, "@"+c.Login, c.Principal); err != nil {
			return TaskStatus{}, err
		}
		err := e.withLabels(ctx, it.Ref.Repo, func() error {
			return c.User.SetLabels(ctx, it.Ref, []string{LReady}, stateLabelsOn(it, LReady))
		})
		e.auditTransition(ctx, it, c.Login, "user", LReady, "task.retried", "retried by "+c.Principal, err)
		if err != nil {
			return TaskStatus{}, refused(err)
		}
		e.Emit(ctx, it, "task.retried", LReady, "retried by "+c.Principal, "")
	} else {
		if err := p.Comment(ctx, it.Ref, fmt.Sprintf("Retried by %s. Attempts and review rounds start over.\n\n%s", Defang(c.Principal), RenderStatus(Status{Kind: "reset", Principal: c.Principal}))); err != nil {
			return TaskStatus{}, err
		}
		if err := e.transition(ctx, p, it, LReady, "task.retried", "retried by "+c.Principal); err != nil {
			return TaskStatus{}, err
		}
	}
	if _, err := e.EvaluateItem(ctx, it.Ref, SrcAPI); err != nil {
		return TaskStatus{}, err
	}
	return e.Status(ctx, c, refStr)
}

// Rebase asks devbot to rebase a task's PR onto its base, as a `/rebase`
// comment on the issue does: it happens once the PR is approved and green.
// A signed-in caller's request is their own comment.
func (e *Engine) Rebase(ctx context.Context, refStr string, c Caller) (TaskStatus, error) {
	it, p, rc, err := e.load(ctx, c, refStr)
	if err != nil {
		return TaskStatus{}, err
	}
	ctx = e.TaskContext(ctx, it)
	if s := State(it); it.State != "open" || (s != LInReview && s != LApproved) {
		return TaskStatus{}, invalid(fmt.Errorf("%s is %q: only a task whose PR is in review or approved can be rebased", it.Ref, s))
	}
	if c.User != nil {
		if err := e.mayDirect(ctx, c, p, rc, it.Ref.Repo, "rebase"); err != nil {
			return TaskStatus{}, err
		}
		if err := c.User.Comment(ctx, it.Ref, rebaseCommand); err != nil {
			return TaskStatus{}, refused(err)
		}
	} else {
		body := fmt.Sprintf("Rebase requested by %s.\n\n%s", Defang(c.Principal), RenderStatus(Status{Kind: "rebase-requested", Principal: c.Principal}))
		if err := p.Comment(ctx, it.Ref, body); err != nil {
			return TaskStatus{}, err
		}
	}
	if _, err := e.EvaluateItem(ctx, it.Ref, SrcAPI); err != nil {
		return TaskStatus{}, err
	}
	return e.Status(ctx, c, refStr)
}

// stateLabelsOn lists the state labels on it other than keep.
func stateLabelsOn(it forge.Item, keep string) []string {
	var out []string
	for _, l := range StateLabels {
		if l != keep && it.HasLabel(l) {
			out = append(out, l)
		}
	}
	return out
}

// Status re-reads a task from the forge.
func (e *Engine) Status(ctx context.Context, c Caller, refStr string) (TaskStatus, error) {
	it, p, _, err := e.load(ctx, c, refStr)
	if err != nil {
		return TaskStatus{}, err
	}
	cs, err := p.ListComments(ctx, it.Ref)
	if err != nil {
		return TaskStatus{}, err
	}
	if isParent(it, e.meta(ctx, it, cs)) {
		return e.parentStatus(ctx, c, it, cs)
	}
	return e.statusOf(ctx, it, cs), nil
}

func (e *Engine) statusOf(ctx context.Context, it forge.Item, comments []forge.Comment) TaskStatus {
	m := e.meta(ctx, it, comments)
	st := TaskStatus{
		Task: it.Ref.String(), Repo: it.Ref.Repo.String(), Title: it.Title, State: strings.TrimPrefix(State(it), "devbot/"), IssueState: it.State,
		URL: it.URL, Origin: m.Origin, Requester: m.Requester, Automerge: it.HasLabel(LAutomerge), Labels: it.Labels,
	}
	if isParent(it, m) {
		st.Kind = KindParent
	}
	if ref, err := forge.ParseItem(m.Parent); err == nil {
		st.Parent = ref.String()
	}
	st.Base = e.shownBase(ctx, it, comments)
	if st.State == "" {
		st.State = "none"
	}
	if comments != nil {
		if p, err := e.provider(it.Ref.Repo); err == nil {
			bots := e.bots(p)
			for i := len(comments) - 1; i >= 0; i-- {
				// A note is another task's word, not an update on this one.
				if s, ok := ParseStatus(comments[i].Body); forge.HasLogin(bots, comments[i].Author) && !(ok && s.Kind == "note") {
					st.LastUpdate = firstLine(stripMarkers(comments[i].Body))
					break
				}
			}
			if w, _ := waitOf(statuses(comments, bots...), it.Ref.Repo); w.On.Number > 0 {
				st.WaitingOn, st.WaitUntil = w.On.String(), w.UntilText()
				st.WaitingFor = e.waitingFor(ctx, w)
				if w.Build {
					st.BuildAgainst, _ = DepsDir(w.On.Repo)
				}
			}
			if hold, ok := LastHold(statuses(comments, bots...)); ok && State(it) == LNeedsApproval {
				st.Held = heldChange(hold)
			}
			if _, ok := contractAsked(statuses(comments, bots...)); ok && st.Parent != "" && it.State == "open" {
				st.Note = "held: its implementer asked to change the contract of parent task " + st.Parent + ", and it goes on once a person approves the new plan"
			}
		}
		if pr, err := e.FindPR(ctx, it); err == nil && pr != nil {
			st.PR = pr.URL
			st.Note = e.rebaseNote(ctx, it, pr)
			if st.BuildAgainst != "" && pr.State == "open" {
				st.Note = fmt.Sprintf("the PR is built against %s's unmerged work, so it isn't reviewed, merged or sent back for its CI until that work is released (wait_until %s); it then goes back for one round that depends on the release", st.WaitingOn, st.WaitUntil)
			}
			if p, err := e.provider(it.Ref.Repo); err == nil && State(it) == LApproved && st.Parent != "" {
				if a, ok := lastStatus(statuses(comments, e.bots(p)...), "approved"); ok && a.Mode == "integration" && a.SHA == pr.HeadSHA {
					st.Note = strings.TrimPrefix(st.Note+"; waiting for the integration check of parent task "+st.Parent+" to pass on every child's current head", "; ")
				}
			}
		}
	}
	return st
}

// ListTasks lists tasks devbot is tracking that c can see, filtered by repo,
// state and the origin prefix a client filed them with; state parent lists
// the parent tasks instead (ListParents). Those it could read come back with
// any error.
func (e *Engine) ListTasks(ctx context.Context, c Caller, repo, state, originPrefix string) ([]TaskStatus, error) {
	if strings.TrimPrefix(state, "devbot/") == KindParent {
		return e.ListParents(ctx, c, repo, originPrefix)
	}
	items, err := e.listItems(ctx, c, repo, state, originPrefix)
	var out []TaskStatus
	for _, it := range items {
		out = append(out, e.statusOf(ctx, it, nil))
	}
	return out, err
}

// listItems reads the open tasks for ListTasks. Given a repo, it asks only
// that repo, whatever else the forge has; without one, it uses each
// provider's search. A label or provider that fails doesn't hide the rest:
// what was read comes back with the errors.
func (e *Engine) listItems(ctx context.Context, c Caller, repo, state, originPrefix string) ([]forge.Item, error) {
	labels := StateLabels
	if state != "" {
		labels = []string{"devbot/" + strings.TrimPrefix(state, "devbot/")}
	}
	type source struct {
		name string
		list func(label string) ([]forge.Item, error)
	}
	var sources []source
	if repo != "" {
		r, err := forge.ParseRepo(repo)
		if err != nil {
			return nil, invalid(err)
		}
		p, err := e.provider(r)
		if err != nil {
			return nil, invalid(err)
		}
		if ok, err := e.CanRead(ctx, c, r); err != nil || !ok {
			return nil, err
		}
		sources = []source{{r.String(), func(l string) ([]forge.Item, error) { return p.ListItems(ctx, r, l, "open") }}}
	} else {
		for _, n := range e.Forges.Names() {
			p := e.Forges[n]
			sources = append(sources, source{n, func(l string) ([]forge.Item, error) { return p.SearchItems(ctx, l, "open") }})
		}
	}
	var out []forge.Item
	var errs []error
	seen := map[string]bool{}
	for _, src := range sources {
		for _, l := range labels {
			items, err := src.list(l)
			if err != nil {
				errs = append(errs, fmt.Errorf("listing %s on %s: %w", l, src.name, err))
			}
			for _, it := range items {
				if seen[it.Ref.String()] {
					continue
				}
				if originPrefix != "" && !strings.HasPrefix(e.meta(ctx, it, nil).Origin, originPrefix) {
					continue
				}
				if ok, err := e.CanRead(ctx, c, it.Ref.Repo); err != nil {
					return nil, err
				} else if !ok {
					continue
				}
				seen[it.Ref.String()] = true
				out = append(out, it)
			}
		}
	}
	return out, errors.Join(errs...)
}

// CanRead reports whether c may see tasks on repo: always in auth mode none;
// otherwise when the forge gives the signed-in person read access to it,
// asked with their own token. A repo they can't read is invisible, not an
// error. An error is the forge failing to say (or no longer accepting the
// caller's sign-in), and is never remembered.
func (e *Engine) CanRead(ctx context.Context, c Caller, repo forge.RepoRef) (bool, error) {
	if c.User == nil {
		return true, nil
	}
	if repo.Provider != c.Provider {
		return false, nil
	}
	key := c.Provider + ":" + strings.ToLower(c.Login) + "|" + repo.String()
	if ok, found := e.readable.get(key); found {
		return ok, nil
	}
	p, err := c.User.Permission(ctx, repo)
	if err != nil {
		return false, fmt.Errorf("checking your access to %s: %w", repo, err)
	}
	ok := p >= forge.PermRead
	e.readable.put(key, ok)
	return ok, nil
}

// notFound is what a caller who can't read repo hears about it, or why
// devbot couldn't tell. A sign-in may reach less than the person can (on
// GitHub, only the repos the sign-in App is installed on), so it says so.
func notFound(repo forge.RepoRef, err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("%s: %w%s", repo, forge.ErrNotFound, notVisible)
}

const notVisible = ", or not visible through your sign-in"

// load reads a task for c. A task on a repo c can't read is not found.
func (e *Engine) load(ctx context.Context, c Caller, refStr string) (forge.Item, forge.Provider, *config.Repo, error) {
	ref, err := forge.ParseItem(refStr)
	if err != nil {
		return forge.Item{}, nil, nil, invalid(err)
	}
	if ok, err := e.CanRead(ctx, c, ref.Repo); err != nil {
		return forge.Item{}, nil, nil, err
	} else if !ok {
		e.auditRefusal(ctx, callerWho(c), "read", "repo not visible to the caller", audit.Fields{Repo: ref.Repo.String(), Task: ref.String()})
		return forge.Item{}, nil, nil, fmt.Errorf("%s: %w%s", ref, forge.ErrNotFound, notVisible)
	}
	p, err := e.provider(ref.Repo)
	if err != nil {
		return forge.Item{}, nil, nil, invalid(err)
	}
	rc, err := e.Repos.Get(ctx, ref.Repo)
	if err != nil {
		return forge.Item{}, nil, nil, fmt.Errorf("%s: %w", ref.Repo, err)
	}
	it, err := p.GetItem(ctx, ref)
	return it, p, rc, err
}

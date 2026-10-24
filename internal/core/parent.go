package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"git.shdw.tech/rob/devbot/internal/audit"
	"git.shdw.tech/rob/devbot/internal/changeset"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// A change that spans repos is several tasks, one per repo, each with its
// own pod, PR, policy and retries. A parent task groups them, so that a
// client follows, cancels and is told about the change as one:
//
//   - a parent is an issue labelled devbot/parent (FileProject), whose
//     routing block says it is one. It has no work of its own: no implement
//     or review job is queued for it, and it has no branch or PR;
//   - a child is an ordinary task filed with a parent (FileTask), in any
//     repo on any provider of the install. Its routing block names the
//     parent, and the parent's issue gets a "child" status naming it, so
//     each side is on the forge, and a child is the parent's only when both
//     say so. A parent can't be a child;
//   - an evaluation of the parent rolls it up from its children, read
//     afresh from their issues every time (evaluateParent): a checklist
//     comment it keeps up to date, a child that stopped, and its close once
//     every child has merged, and the integration check of its children's
//     heads where its repo defines one (integration.go). A child that
//     moves evaluates its parent at once (parentMoved).
//
// Children order themselves with waits and talk with notes, across repos,
// as any tasks may; each is told of its siblings (InFlight).

// KindParent is a parent task's Meta.Kind.
const KindParent = "parent"

// isParent reports whether it, whose routing block is m, is a parent task:
// the label alone is anyone's to add.
func isParent(it forge.Item, m Meta) bool { return it.HasLabel(LParent) && m.Kind == KindParent }

// A parent task's state, as TaskStatus shows it: rolled up from its
// children, never a label of its own.
const (
	ParentInProgress = "in-progress"
	ParentNeedsHuman = "needs-human"
	ParentMerged     = "merged"
	ParentCancelled  = "cancelled"
)

// Where a child stands once it is no longer a task in flight.
const (
	childMerged    = "merged"
	childCancelled = "cancelled"
	childGone      = "gone"
)

// child is one of a parent's children, as an evaluation read it.
type child struct {
	ref      forge.ItemRef
	it       forge.Item
	comments []forge.Comment
	// state is its state label without devbot/, or childMerged,
	// childCancelled or childGone.
	state string
	pr    *forge.PR
	// stop is the status that put it in needs-human, while it is there.
	stop statusComment
	// ss are the statuses on its issue.
	ss []statusComment
}

// childRefs are the children the "child" statuses on a parent of repo own
// name, in the order they were filed.
func childRefs(ss []statusComment, own forge.RepoRef) []forge.ItemRef {
	var out []forge.ItemRef
	seen := map[forge.ItemRef]bool{}
	for _, s := range ss {
		if ref := s.taskRef(own); s.Kind == "child" && ref.Number > 0 && !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	return out
}

// children reads each child parent's statuses ss name from its own issue.
// One whose routing block doesn't name parent isn't its child, whatever a
// status says.
func (e *Engine) children(ctx context.Context, parent forge.Item, ss []statusComment) ([]child, error) {
	var out []child
	for _, ref := range childRefs(ss, parent.Ref.Repo) {
		k := child{ref: ref, state: childGone}
		p, err := e.provider(ref.Repo)
		if err != nil {
			out = append(out, k) // a provider the install no longer has
			continue
		}
		it, err := p.GetItem(ctx, ref)
		if errors.Is(err, forge.ErrNotFound) {
			out = append(out, k)
			continue
		}
		if err != nil {
			return nil, err
		}
		cs, err := p.ListComments(ctx, ref)
		if err != nil {
			return nil, err
		}
		if e.meta(ctx, it, cs).Parent != parent.Ref.String() {
			continue
		}
		t := &task{e: e, p: p, it: it, comments: cs, ss: statuses(cs, e.bots(p)...)}
		if k.pr, err = t.findPR(ctx); err != nil {
			return nil, err
		}
		k.it, k.comments, k.ss = it, cs, t.ss
		switch {
		case k.pr != nil && k.pr.Merged:
			k.state = childMerged
		case it.State != "open" || State(it) == "":
			k.state = childCancelled
		default:
			k.state = strings.TrimPrefix(State(it), "devbot/")
		}
		if State(it) == LNeedsHuman {
			k.stop, _ = lastStatus(t.ss, "needs-human")
		}
		out = append(out, k)
	}
	return out, nil
}

func allMerged(kids []child) bool {
	for _, k := range kids {
		if k.state != childMerged {
			return false
		}
	}
	return len(kids) > 0
}

// rollup is where a parent of repo own stands, from its children, and what
// a person following it should know of them. completed is its "completed"
// status, posted when every child had merged. Children in hidden still count
// toward the state, but the notes neither name them nor say why they
// stopped: whoever asks may not be able to read their repos.
func rollup(open, completed bool, kids []child, own forge.RepoRef, hidden map[forge.ItemRef]bool) (string, []string) {
	var notes []string
	stopped, unseen := false, 0
	for _, k := range kids {
		if k.state == "needs-human" {
			stopped = true
		}
		if hidden[k.ref] {
			switch k.state {
			case "needs-human", "needs-info", "needs-approval", childCancelled, childGone:
				unseen++
			}
			continue
		}
		name := RefText(own, k.ref)
		switch k.state {
		case "needs-human":
			why := firstLine(stripMarkers(k.stop.Body))
			if why == "" {
				why = "no reason recorded"
			}
			notes = append(notes, fmt.Sprintf("%s stopped at needs-human: %s", name, why))
		case "needs-info":
			notes = append(notes, fmt.Sprintf("%s is waiting for an answer to its question (needs-info)", name))
		case "needs-approval":
			notes = append(notes, fmt.Sprintf("%s holds a change to protected files for a person's approval (needs-approval)", name))
		case childCancelled:
			notes = append(notes, fmt.Sprintf("%s was cancelled without merging: retry it, file a replacement, or cancel the parent task", name))
		case childGone:
			notes = append(notes, fmt.Sprintf("%s no longer exists", name))
		}
	}
	if unseen > 0 {
		notes = append(notes, fmt.Sprintf("%d child task(s) in repos you can't read need a person", unseen))
	}
	switch {
	case !open && completed:
		return ParentMerged, nil
	case !open:
		return ParentCancelled, nil
	case stopped:
		return ParentNeedsHuman, notes
	case allMerged(kids):
		return ParentMerged, notes
	case len(kids) == 0:
		return ParentInProgress, []string{"no child tasks yet: file them with file_task and parent"}
	}
	return ParentInProgress, notes
}

// checklist is the comment a parent of ref keeps listing its children;
// planned is whether devbot plans them.
func checklist(ref forge.ItemRef, kids []child, planned bool) string {
	var b strings.Builder
	b.WriteString("**Child tasks**\n\n")
	if len(kids) == 0 && planned {
		b.WriteString("None yet. devbot files them once a person approves its plan.\n")
	} else if len(kids) == 0 {
		fmt.Fprintf(&b, "None yet. File them with `file_task` and `parent: %s`.\n", ref)
	}
	for _, k := range kids {
		box := " "
		if k.state == childMerged {
			box = "x"
		}
		if k.it.URL != "" {
			fmt.Fprintf(&b, "- [%s] [%s](%s)", box, RefText(ref.Repo, k.ref), k.it.URL)
		} else {
			fmt.Fprintf(&b, "- [%s] %s", box, RefText(ref.Repo, k.ref))
		}
		if t := strings.TrimSpace(firstLine(k.it.Title)); t != "" {
			b.WriteString(" " + Defang(t))
		}
		switch k.state {
		case childMerged:
			b.WriteString(": merged")
		case childCancelled:
			b.WriteString(": cancelled without merging")
		case childGone:
			b.WriteString(": no longer exists")
		default:
			fmt.Fprintf(&b, ": `devbot/%s`", k.state)
		}
		if k.pr != nil {
			b.WriteString(", PR " + k.pr.URL)
		}
		b.WriteString("\n")
	}
	return b.String() + "\n" + render("checklist", struct{}{})
}

// evaluateParent rolls a parent task up from its children: its checklist, a
// child that stopped, and, once every child has merged, a summary and its
// close. A closed parent has nothing left to do.
func (t *task) evaluateParent(ctx context.Context) (bool, error) {
	if t.it.State != "open" {
		return false, nil
	}
	acted := false
	// A state label on a parent would list it, and queue it, as a task.
	if stray := stateLabelsOn(t.it, ""); len(stray) > 0 {
		if err := t.p.SetLabels(ctx, t.it.Ref, nil, stray); err != nil {
			return false, err
		}
		acted = true
	}
	kids, err := t.e.children(ctx, t.it, t.ss)
	if err != nil {
		return acted, err
	}
	did, err := t.plan(ctx, kids)
	if did {
		acted = true
		if err == nil {
			t.comments = nil
			if err = t.load(ctx); err == nil {
				kids, err = t.e.children(ctx, t.it, t.ss)
			}
		}
	}
	if err != nil {
		return acted, err
	}
	did, err = t.updateChecklist(ctx, kids)
	acted = acted || did
	if err != nil {
		return acted, err
	}
	for _, k := range kids {
		if k.state != "needs-human" {
			continue
		}
		did, err := t.childStopped(ctx, k)
		acted = acted || did
		if err != nil {
			return acted, err
		}
	}
	did, err = t.integrate(ctx, kids)
	acted = acted || did
	if err != nil {
		return acted, err
	}
	if allMerged(kids) {
		return true, t.completed(ctx, kids)
	}
	return acted, nil
}

// updateChecklist posts the parent's checklist, or edits it to match kids.
func (t *task) updateChecklist(ctx context.Context, kids []child) (bool, error) {
	body := checklist(t.it.Ref, kids, t.e.meta(ctx, t.it, t.comments).Plan)
	bots := t.e.bots(t.p)
	for i := len(t.comments) - 1; i >= 0; i-- {
		c := t.comments[i]
		if !forge.HasLogin(bots, c.Author) || !find(c.Body, "checklist", &struct{}{}) {
			continue
		}
		if strings.TrimSpace(c.Body) == body {
			return false, nil
		}
		return true, t.p.UpdateComment(ctx, t.it.Ref, c.ID, body)
	}
	if err := t.p.Comment(ctx, t.it.Ref, body); err != nil {
		return false, err
	}
	t.comments = nil
	return true, t.load(ctx)
}

// childStopped says, once per stop, that child k is in needs-human, and so
// the parent is.
func (t *task) childStopped(ctx context.Context, k child) (bool, error) {
	own := t.it.Ref.Repo
	st := Status{Kind: "needs-human", Since: k.stop.ID}.naming(own, k.ref)
	said := func() bool {
		for _, s := range t.ss {
			if s.Kind == "needs-human" && s.taskRef(own) == k.ref && s.Since == st.Since {
				return true
			}
		}
		return false
	}
	if said() {
		return false, nil
	}
	// Another evaluation may have said it since this one read the comments.
	t.comments = nil
	if err := t.load(ctx); err != nil || said() {
		return false, err
	}
	// A link, not the reason: that may hold what only the child's repo should
	// show (the implementer's account of its code, protected paths, mentions
	// of its reviewers), and the parent's issue may have a wider audience.
	name := RefText(own, k.ref)
	text := fmt.Sprintf("Child task %s stopped at `devbot/needs-human`, so this parent task needs a person too. Its reason is on %s.\n\n"+
		"Retrying this parent task retries every child task that stopped; cancelling it cancels them all.", name, k.it.URL)
	if err := t.p.Comment(ctx, t.it.Ref, text+"\n\n"+RenderStatus(st)); err != nil {
		return false, err
	}
	t.e.emit(ctx, t.it, "parent.needs_human", ParentNeedsHuman, name+" stopped at needs-human", "", fmt.Sprintf("%s|%d", k.ref, k.stop.ID))
	t.comments = nil
	return true, t.load(ctx)
}

// completed posts the parent's summary, once, and closes it. A close that
// failed leaves it open with its summary said, for the next evaluation.
func (t *task) completed(ctx context.Context, kids []child) error {
	if _, ok := lastStatus(t.ss, "completed"); !ok {
		var b strings.Builder
		b.WriteString("Every child task has merged, so this parent task is done:\n\n")
		for _, k := range kids {
			fmt.Fprintf(&b, "- %s %s: PR %s\n", RefText(t.it.Ref.Repo, k.ref), Defang(firstLine(k.it.Title)), k.pr.URL)
		}
		if err := t.p.Comment(ctx, t.it.Ref, b.String()+"\n"+RenderStatus(Status{Kind: "completed"})); err != nil {
			return err
		}
	}
	if err := t.p.CloseItem(ctx, t.it.Ref); err != nil {
		return err
	}
	t.e.emit(ctx, t.it, "parent.completed", ParentMerged, fmt.Sprintf("all %d child tasks merged", len(kids)), "", "completed")
	return nil
}

// parentMoved evaluates the parent of it, a child task that has just moved,
// so that the parent shows it now rather than at the next reconcile pass.
// The evaluation that moved it read its routing block, which is cached:
// asking the forge for one a task doesn't have would cost every evaluation
// of a forge-labelled task a read.
func (e *Engine) parentMoved(ctx context.Context, it forge.Item, src Source) {
	m, _ := e.metas.get(it.Ref.String())
	ref, err := forge.ParseItem(m.Parent)
	if err != nil {
		return
	}
	if _, err := e.EvaluateItem(ctx, ref, src); err != nil {
		e.Log.Warn("evaluating a parent task after its child moved", "task", it.Ref, "parent", ref, "err", err)
	}
}

// parentStatus is a parent task's status for c: its rolled-up state, and
// each child c can see.
func (e *Engine) parentStatus(ctx context.Context, c Caller, it forge.Item, comments []forge.Comment) (TaskStatus, error) {
	p, err := e.provider(it.Ref.Repo)
	if err != nil {
		return TaskStatus{}, err
	}
	m := e.meta(ctx, it, comments)
	ss := statuses(comments, e.bots(p)...)
	kids, err := e.children(ctx, it, ss)
	if err != nil {
		return TaskStatus{}, err
	}
	hidden := map[forge.ItemRef]bool{}
	for _, k := range kids {
		if ok, err := e.CanRead(ctx, c, k.ref.Repo); err != nil {
			return TaskStatus{}, err
		} else if !ok {
			hidden[k.ref] = true
		}
	}
	_, completed := lastStatus(ss, "completed")
	state, notes := rollup(it.State == "open", completed, kids, it.Ref.Repo, hidden)
	cfg, err := e.integrationFor(ctx, it)
	if err != nil {
		return TaskStatus{}, err
	}
	if note, stop := integrationNote(cfg, integrationOf(kids, ss), it.Ref.Repo, hidden); note != "" && state != ParentMerged && state != ParentCancelled {
		notes = append(notes, note)
		if stop {
			state = ParentNeedsHuman
		}
	}
	var held *HeldChange
	if m.Plan && it.State == "open" {
		rc, err := e.Repos.Get(ctx, it.Ref.Repo)
		if err != nil {
			return TaskStatus{}, err
		}
		pstate, note, h := planStatus(planOf(ss), rc.MaxAttempts, kids, it.Ref.Repo)
		if len(kids) == 0 {
			notes = nil // not "file them": devbot does
		}
		if pstate == ParentPlanning && state == ParentNeedsHuman {
			notes = append(notes, note) // a stopped child still needs a person
		} else if pstate != "" {
			state, held, notes = pstate, h, append([]string{note}, notes...)
		}
	}
	st := TaskStatus{
		Task: it.Ref.String(), Repo: it.Ref.Repo.String(), Title: it.Title, Kind: KindParent, State: state, IssueState: it.State,
		URL: it.URL, Origin: m.Origin, Requester: m.Requester, Labels: it.Labels, Note: strings.Join(notes, "; "), Children: []TaskStatus{}, Held: held,
	}
	if len(ss) > 0 {
		st.LastUpdate = firstLine(stripMarkers(ss[len(ss)-1].Body))
	}
	for _, k := range kids {
		if hidden[k.ref] {
			continue
		}
		if k.state == childGone {
			st.Children = append(st.Children, TaskStatus{Task: k.ref.String(), Repo: k.ref.Repo.String(), State: childGone, Parent: it.Ref.String()})
			continue
		}
		cs := e.statusOf(ctx, k.it, k.comments)
		if k.state == childMerged || k.state == childCancelled {
			cs.State = k.state
		}
		st.Children = append(st.Children, cs)
	}
	return st, nil
}

// ProjectRequest is what file_project files.
type ProjectRequest struct {
	Repo               string `json:"repo"`
	Title              string `json:"title"`
	Body               string `json:"body"`
	AcceptanceCriteria string `json:"acceptance_criteria,omitempty"`
	Origin             string `json:"origin,omitempty"`
	// Plan has devbot plan the children (see plan.go), each in one of
	// Repos, rather than the caller file them.
	Plan  bool     `json:"plan,omitempty"`
	Repos []string `json:"repos,omitempty"`
}

// planRepos are the candidate repos of a parent task filed with plan, as
// req names them, checked before anything is written: each must be one
// devbot works on, that c may direct devbot on.
func (e *Engine) planRepos(ctx context.Context, req ProjectRequest, c Caller) ([]string, error) {
	switch {
	case !req.Plan && len(req.Repos) > 0:
		return nil, invalid(errors.New("repos are the candidate repos of a planned parent task: set plan too, or leave them out"))
	case !req.Plan:
		return nil, nil
	case strings.TrimSpace(req.AcceptanceCriteria) == "":
		return nil, invalid(errors.New("acceptance_criteria is required with plan: say what done looks like for the whole change, so the planner and each child's reviewer can check it"))
	case len(req.Repos) == 0:
		return nil, invalid(errors.New("repos is required with plan: the repos the change may touch, each as provider:owner/name"))
	case len(req.Repos) > MaxPlanRepos:
		return nil, invalid(fmt.Errorf("repos has %d repos, over %d", len(req.Repos), MaxPlanRepos))
	}
	var out []string
	for _, s := range req.Repos {
		repo, err := forge.ParseRepo(strings.TrimSpace(s))
		if err != nil {
			return nil, invalid(fmt.Errorf("repos: %w", err))
		}
		if slices.Contains(out, repo.String()) {
			continue
		}
		if ok, err := e.CanRead(ctx, c, repo); err != nil || !ok {
			if err == nil {
				e.auditRefusal(ctx, callerWho(c), "file_project", "candidate repo not visible to the caller", audit.Fields{Repo: repo.String()})
			}
			return nil, notFound(repo, err)
		}
		p, rc, err := e.otherRepo(ctx, repo)
		if err != nil {
			return nil, err
		}
		if c.User != nil {
			if err := e.mayDirect(ctx, c, p, rc, repo, "file_project"); err != nil {
				return nil, err
			}
		}
		out = append(out, repo.String())
	}
	return out, nil
}

// FileProject files a parent task: an issue labelled devbot/parent, whose
// child tasks FileTask then files with its ref as their parent.
func (e *Engine) FileProject(ctx context.Context, req ProjectRequest, c Caller) (_ TaskStatus, err error) {
	repo, err := forge.ParseRepo(req.Repo)
	if err != nil {
		return TaskStatus{}, invalid(err)
	}
	if strings.TrimSpace(req.Title) == "" {
		return TaskStatus{}, invalid(errors.New("title is required"))
	}
	p, err := e.provider(repo)
	if err != nil {
		return TaskStatus{}, invalid(err)
	}
	ctx = audit.With(ctx, audit.Fields{Repo: repo.String(), Origin: req.Origin})
	if ok, err := e.CanRead(ctx, c, repo); err != nil || !ok {
		if err == nil {
			e.auditRefusal(ctx, callerWho(c), "file_project", "repo not visible to the caller", audit.Fields{})
		}
		return TaskStatus{}, notFound(repo, err)
	}
	rc, err := e.Repos.Get(ctx, repo)
	if err != nil {
		return TaskStatus{}, fmt.Errorf("%s: %w", repo, err)
	}
	if err := e.mayLabel(ctx, c, p, rc, repo, "file_project", LParent); err != nil {
		return TaskStatus{}, err
	}
	repos, err := e.planRepos(ctx, req, c)
	if err != nil {
		return TaskStatus{}, err
	}
	if err := e.EnsureLabels(ctx, repo); err != nil {
		return TaskStatus{}, err
	}
	body := Defang(req.Body)
	if ac := strings.TrimSpace(req.AcceptanceCriteria); ac != "" {
		body = taskBody(body, Defang(ac))
	}
	it, err := e.createLabelled(ctx, c, p, repo, strings.TrimSpace(req.Title), body, []string{LParent})
	if err != nil {
		return TaskStatus{}, err
	}
	handle, note := "", fmt.Sprintf("Filed via %s.", Defang(c.Principal))
	if c.User != nil {
		handle, note = c.Login, "devbot is tracking this parent task."
	}
	if req.Plan {
		note += fmt.Sprintf(" It has no work of its own: devbot plans its child tasks, in %s, and posts the plan here for a person to approve before it files any.", strings.Join(repos, ", "))
	} else {
		note += fmt.Sprintf(" It has no work of its own: file its child tasks, in any repo, with `file_task` and `parent: %s`, and devbot follows them here.", it.Ref)
	}
	m := Meta{Kind: KindParent, Origin: req.Origin, Requester: c.Principal, RequesterHandle: handle, Plan: req.Plan, Repos: repos}
	if err := p.Comment(ctx, it.Ref, note+"\n\n"+RenderMeta(m)); err != nil {
		// Without its block the issue is no parent, and would be filed again
		// by a caller who saw this fail.
		if c.User == nil || c.User.CloseItem(ctx, it.Ref) != nil {
			if cerr := p.CloseItem(ctx, it.Ref); cerr != nil {
				return TaskStatus{}, fmt.Errorf("devbot filed %s but couldn't record it as a parent task (%w), and couldn't close it: close it yourself before filing again", it.URL, err)
			}
		}
		return TaskStatus{}, fmt.Errorf("devbot filed %s but couldn't record it as a parent task (%w), so it closed it again: file it again", it.URL, err)
	}
	e.metas.put(it.Ref.String(), m)
	e.Emit(ctx, it, "parent.filed", ParentInProgress, it.Title, "")
	if _, err := e.EvaluateItem(ctx, it.Ref, SrcAPI); err != nil {
		e.Log.Warn("evaluate after file_project", "task", it.Ref, "err", err)
	}
	return e.Status(ctx, c, it.Ref.String())
}

// mayLabel checks, for a signed-in caller filing an issue with label, that
// they may direct devbot on repo and that the forge lets them label its
// issues. Push isn't always the right to label issues, and a forge may drop
// the label at create instead of refusing.
func (e *Engine) mayLabel(ctx context.Context, c Caller, p forge.Provider, rc *config.Repo, repo forge.RepoRef, action, label string) error {
	if c.User == nil {
		return nil
	}
	if err := e.mayDirect(ctx, c, p, rc, repo, action); err != nil {
		return err
	}
	if ok, err := c.User.CanEditIssues(ctx, repo); err != nil {
		return fmt.Errorf("checking your access to %s's issues: %w", repo, err)
	} else if !ok {
		e.auditRefusal(ctx, callerWho(c), action, "the forge doesn't let the caller label issues", audit.Fields{})
		return fmt.Errorf("%w: the forge doesn't let you label issues on %s, so you can't mark one %s", ErrRefused, repo, label)
	}
	return nil
}

// createLabelled creates an issue with labels, labels[0] its devbot label,
// as a signed-in caller or else the implementer bot. One the forge created
// without that label anyway is closed again, as its author, so a refusal
// leaves nothing open.
func (e *Engine) createLabelled(ctx context.Context, c Caller, p forge.Provider, repo forge.RepoRef, title, body string, labels []string) (it forge.Item, err error) {
	create := p.CreateItem
	if c.User != nil {
		create = c.User.CreateItem
	}
	err = e.withLabels(ctx, repo, func() (err error) {
		it, err = create(ctx, repo, title, body, labels)
		return err
	})
	if c.User != nil && err == nil && !it.HasLabel(labels[0]) {
		if cerr := c.User.CloseItem(ctx, it.Ref); cerr != nil {
			e.Log.Warn("closing an issue filed without its label", "task", it.Ref, "err", cerr)
			return it, fmt.Errorf("%w: the forge created %s without the %s label, so devbot won't work on it, and devbot couldn't close it: close it yourself", ErrRefused, it.URL, labels[0])
		}
		return it, fmt.Errorf("%w: the forge created %s without the %s label, so devbot closed it again", ErrRefused, it.URL, labels[0])
	}
	return it, refused(err)
}

// parentFor reads the parent task s names, for a child of repo own that c
// files: an open parent task, of a repo devbot works on and c may direct
// devbot on, that isn't itself a child. One that isn't is ErrInvalid or
// ErrRefused, before anything is written.
func (e *Engine) parentFor(ctx context.Context, c Caller, s string, own forge.RepoRef) (forge.Item, forge.Provider, error) {
	ref, err := ParseTaskRef(s, own)
	if err != nil || ref.Number == 0 {
		return forge.Item{}, nil, invalid(fmt.Errorf("parent %q: want a parent task, like %s#12", s, own))
	}
	if ok, err := e.CanRead(ctx, c, ref.Repo); err != nil || !ok {
		if err == nil {
			e.auditRefusal(ctx, callerWho(c), "file_task", "parent's repo not visible to the caller", audit.Fields{Repo: ref.Repo.String()})
		}
		return forge.Item{}, nil, notFound(ref.Repo, err)
	}
	p, rc, err := e.otherRepo(ctx, ref.Repo)
	if err != nil {
		return forge.Item{}, nil, err
	}
	if c.User != nil {
		if err := e.mayDirect(ctx, c, p, rc, ref.Repo, "file_task"); err != nil {
			return forge.Item{}, nil, err
		}
	}
	it, err := p.GetItem(ctx, ref)
	if errors.Is(err, forge.ErrNotFound) {
		return forge.Item{}, nil, invalid(fmt.Errorf("parent %s doesn't exist", ref))
	}
	if err != nil {
		return forge.Item{}, nil, err
	}
	cs, err := p.ListComments(ctx, ref)
	if err != nil {
		return forge.Item{}, nil, err
	}
	switch m := e.meta(ctx, it, cs); {
	case m.Parent != "":
		return forge.Item{}, nil, invalid(fmt.Errorf("%s is a child task of %s, and a child task can't have children of its own: file this one under %s", ref, m.Parent, m.Parent))
	case !isParent(it, m):
		return forge.Item{}, nil, invalid(fmt.Errorf("%s isn't a parent task: file one with file_project", ref))
	case it.State != "open":
		return forge.Item{}, nil, invalid(fmt.Errorf("parent task %s is closed", ref))
	}
	return it, p, nil
}

// recordChild records child, just filed, on its parent's issue, where the
// parent's evaluations find it, with its place in the plan that filed it,
// when one did.
func (e *Engine) recordChild(ctx context.Context, parent forge.Item, pp forge.Provider, child forge.Item, pc *plannedChild) error {
	text := fmt.Sprintf("Child task %s filed: %s", RefText(parent.Ref.Repo, child.Ref), Defang(firstLine(child.Title)))
	st := Status{Kind: "child"}.naming(parent.Ref.Repo, child.Ref)
	if pc != nil {
		text = fmt.Sprintf("Child task %s filed, as child %d of plan `%s`: %s", RefText(parent.Ref.Repo, child.Ref), pc.item-1, shortSHA(pc.sha), Defang(firstLine(child.Title)))
		st.SHA, st.Item = pc.sha, pc.item
	}
	return pp.Comment(ctx, parent.Ref, text+"\n\n"+RenderStatus(st))
}

// cancelParent cancels each open child of parent it for c, then closes it.
// A child that can't be cancelled leaves the parent open, for the caller to
// try again.
func (e *Engine) cancelParent(ctx context.Context, c Caller, it forge.Item, p forge.Provider, rc *config.Repo) (TaskStatus, error) {
	if it.State != "open" {
		return e.Status(ctx, c, it.Ref.String())
	}
	if c.User != nil {
		if err := e.mayDirect(ctx, c, p, rc, it.Ref.Repo, "cancel"); err != nil {
			return TaskStatus{}, err
		}
	}
	cs, err := p.ListComments(ctx, it.Ref)
	if err != nil {
		return TaskStatus{}, err
	}
	kids, err := e.children(ctx, it, statuses(cs, e.bots(p)...))
	if err != nil {
		return TaskStatus{}, err
	}
	var cancelled []string
	for _, k := range kids {
		if k.it.State != "open" || State(k.it) == "" {
			continue
		}
		if _, err := e.Cancel(ctx, k.ref.String(), c); err != nil {
			return TaskStatus{}, fmt.Errorf("cancelling child task %s: %w", k.ref, err)
		}
		cancelled = append(cancelled, RefText(it.Ref.Repo, k.ref))
	}
	t := &task{e: e, p: p, rc: rc, it: it}
	if err := t.load(ctx); err != nil {
		return TaskStatus{}, err
	}
	if _, ok := lastStatus(t.ss, "cancelled"); !ok {
		text := fmt.Sprintf("Cancelled by %s.", Defang(c.Principal))
		if len(cancelled) > 0 {
			text += " devbot cancelled its open child tasks: " + strings.Join(cancelled, ", ") + "."
		}
		if err := p.Comment(ctx, it.Ref, text+"\n\n"+RenderStatus(Status{Kind: "cancelled", Principal: c.Principal})); err != nil {
			return TaskStatus{}, err
		}
	}
	closeItem := p.CloseItem
	if c.User != nil {
		closeItem = c.User.CloseItem
	}
	if err := closeItem(ctx, it.Ref); err != nil {
		return TaskStatus{}, refused(err)
	}
	e.emit(ctx, it, "parent.cancelled", ParentCancelled, "cancelled by "+c.Principal, "", "cancelled")
	return e.Status(ctx, c, it.Ref.String())
}

// retryParent retries each child of parent it in needs-human, for c.
func (e *Engine) retryParent(ctx context.Context, c Caller, it forge.Item, p forge.Provider, rc *config.Repo) (TaskStatus, error) {
	if c.User != nil {
		if err := e.mayDirect(ctx, c, p, rc, it.Ref.Repo, "retry"); err != nil {
			return TaskStatus{}, err
		}
	}
	cs, err := p.ListComments(ctx, it.Ref)
	if err != nil {
		return TaskStatus{}, err
	}
	kids, err := e.children(ctx, it, statuses(cs, e.bots(p)...))
	if err != nil {
		return TaskStatus{}, err
	}
	n := 0
	for _, k := range kids {
		if k.state != "needs-human" {
			continue
		}
		if _, err := e.Retry(ctx, k.ref.String(), c); err != nil {
			return TaskStatus{}, fmt.Errorf("retrying child task %s: %w", k.ref, err)
		}
		n++
	}
	// An integration check that failed, or couldn't run, on the current
	// heads runs again.
	if rc.Integration.On() {
		in := integrationOf(kids, statuses(cs, e.bots(p)...))
		if in.Result == IntegrationFailed || in.GaveUp(*rc.Integration.Retries) {
			if err := p.Comment(ctx, it.Ref, fmt.Sprintf("Retried by %s: the integration check runs again on the children's current heads.\n\n%s",
				Defang(c.Principal), RenderStatus(Status{Kind: "integration", Mode: IntegrationRetry, Heads: in.Keys(), Principal: c.Principal}))); err != nil {
				return TaskStatus{}, err
			}
			n++
		}
	}
	// Planning that stopped for a person plans again: from scratch after the
	// planner gave up, with the replans counted afresh past max_replans.
	if e.meta(ctx, it, cs).Plan {
		ps := planOf(statuses(cs, e.bots(p)...))
		var mode, text string
		switch st, _, _ := planStatus(ps, rc.MaxAttempts, kids, it.Ref.Repo); {
		case st == ParentNeedsHuman && ps.Open && ps.gaveUp(rc.MaxAttempts):
			mode, text = PlanReset, "devbot plans this change again, attempts counted afresh."
		case st == ParentNeedsHuman:
			mode, text = PlanRetry, "its replans count afresh, so devbot plans this change again for the child tasks that asked to change the contract."
		}
		if mode != "" {
			if err := p.Comment(ctx, it.Ref, fmt.Sprintf("Retried by %s: %s\n\n%s", Defang(c.Principal), text, RenderStatus(Status{Kind: "plan", Mode: mode, Principal: c.Principal}))); err != nil {
				return TaskStatus{}, err
			}
			n++
		}
	}
	if n == 0 {
		return TaskStatus{}, invalid(fmt.Errorf("no child task of %s is in needs-human, and neither its plan nor its integration check stopped, so there's nothing to retry", it.Ref))
	}
	if _, err := e.EvaluateItem(ctx, it.Ref, SrcAPI); err != nil {
		return TaskStatus{}, err
	}
	return e.Status(ctx, c, it.Ref.String())
}

// ListChildren lists the children of parent task parent that c can see,
// open or closed, filtered as ListTasks filters: a closed child's state is
// merged or cancelled.
func (e *Engine) ListChildren(ctx context.Context, c Caller, parent, repo, state, originPrefix string) ([]TaskStatus, error) {
	it, p, _, err := e.load(ctx, c, parent)
	if err != nil {
		return nil, err
	}
	cs, err := p.ListComments(ctx, it.Ref)
	if err != nil {
		return nil, err
	}
	if !isParent(it, e.meta(ctx, it, cs)) {
		return nil, invalid(fmt.Errorf("%s isn't a parent task", it.Ref))
	}
	st, err := e.parentStatus(ctx, c, it, cs)
	if err != nil {
		return nil, err
	}
	out := []TaskStatus{}
	state = strings.TrimPrefix(state, "devbot/")
	for _, k := range st.Children {
		if (repo == "" || k.Repo == repo) && (state == "" || k.State == state) && strings.HasPrefix(k.Origin, originPrefix) {
			out = append(out, k)
		}
	}
	return out, nil
}

// ListParents lists the open parent tasks c can see, rolled up, filtered
// by repo and the origin prefix they were filed with.
func (e *Engine) ListParents(ctx context.Context, c Caller, repo, originPrefix string) ([]TaskStatus, error) {
	items, lerr := e.listItems(ctx, c, repo, KindParent, originPrefix)
	out := []TaskStatus{}
	for _, it := range items {
		p, err := e.provider(it.Ref.Repo)
		if err != nil {
			return out, err
		}
		cs, err := p.ListComments(ctx, it.Ref)
		if err != nil {
			return out, err
		}
		if !isParent(it, e.meta(ctx, it, cs)) {
			continue
		}
		st, err := e.parentStatus(ctx, c, it, cs)
		if err != nil {
			return out, err
		}
		out = append(out, st)
	}
	return out, lerr
}

// siblings are the other children of the parent task of it, a child task,
// as the parent's evaluations read them; none for a task with no parent.
func (e *Engine) siblings(ctx context.Context, it forge.Item, comments []forge.Comment) ([]child, error) {
	ref, err := forge.ParseItem(e.meta(ctx, it, comments).Parent)
	if err != nil {
		return nil, nil
	}
	p, err := e.provider(ref.Repo)
	if err != nil {
		return nil, nil
	}
	parent, err := p.GetItem(ctx, ref)
	if errors.Is(err, forge.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cs, err := p.ListComments(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !isParent(parent, e.meta(ctx, parent, cs)) {
		return nil, nil
	}
	kids, err := e.children(ctx, parent, statuses(cs, e.bots(p)...))
	var out []child
	for _, k := range kids {
		if k.ref != it.Ref {
			out = append(out, k)
		}
	}
	return out, err
}

// PRFiles lists the files pr changes against its base, from git, fetched
// with role's credential: what a sibling's PR in another repo changes, which
// a run's clone of its own repo can't tell.
func (e *Engine) PRFiles(ctx context.Context, pr forge.PR, role forge.Role) ([]string, error) {
	p, err := e.provider(pr.Repo)
	if err != nil {
		return nil, err
	}
	var cs []changeset.Change
	if e.Changes != nil {
		cs, err = e.Changes(ctx, p, pr)
	} else {
		cs, err = e.fetchChangesAs(ctx, p, pr, role)
	}
	if err != nil {
		return nil, err
	}
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Path
	}
	return out, nil
}

// otherSiblings are those of sibs in other repos than self's, as InFlight
// lists them. A sibling's description is shown only when self's requester
// may direct devbot on its repo, as for a note to it (reach): whoever files
// two children under one parent needn't be able to read both repos.
func (e *Engine) otherSiblings(ctx context.Context, self forge.Item, sibs []child) ([]InFlightTask, error) {
	var out []InFlightTask
	var d *director
	for _, k := range sibs {
		if k.ref.Repo == self.Ref.Repo || k.state == childGone {
			continue
		}
		p, rc, err := e.otherRepo(ctx, k.ref.Repo)
		if errors.Is(err, ErrInvalid) {
			continue
		}
		if err != nil {
			return nil, err
		}
		base, err := e.taskBase(ctx, p, rc, k.it, k.comments)
		if err != nil {
			return nil, err
		}
		w, _ := waitOf(statuses(k.comments, e.bots(p)...), k.ref.Repo)
		f := InFlightTask{Number: k.ref.Number, Ref: k.ref, Title: k.it.Title, State: k.state, Base: base, URL: k.it.URL, WaitsOn: w.On, WaitUntil: w.UntilText(), PR: k.pr, Sibling: true}
		if d == nil {
			r := e.requester(ctx, self, nil)
			d = &r
		}
		if e.directs(ctx, *d, self.Ref.Repo, p, rc, k.ref.Repo) == nil {
			f.Body = stripMarkers(k.it.Body)
		} else {
			f.Limited = true
		}
		out = append(out, f)
	}
	return out, nil
}

package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// EditRequest redirects a task: each of Title, Body and AcceptanceCriteria
// that is set replaces that part of the issue.
type EditRequest struct {
	Ref                string `json:"ref"`
	Title              string `json:"title,omitempty"`
	Body               string `json:"body,omitempty"`
	AcceptanceCriteria string `json:"acceptance_criteria,omitempty"`
	// Note is a one-line reason, quoted in the comment.
	Note string `json:"note,omitempty"`
}

// acHeading opens the section file_task writes the acceptance criteria in.
const acHeading = "## Acceptance criteria\n\n"

// taskBody is an issue body as file_task writes it.
func taskBody(desc, ac string) string {
	var b strings.Builder
	if desc != "" {
		b.WriteString(desc)
		b.WriteString("\n\n")
	}
	b.WriteString(acHeading)
	b.WriteString(ac)
	b.WriteString("\n")
	return b.String()
}

// sections splits a body taskBody wrote into its two parts. The criteria
// are last, so the last heading is theirs.
func sections(body string) (desc, ac string, ok bool) {
	body = strings.ReplaceAll(body, "\r\n", "\n") // a forge's web editor
	i := strings.LastIndex(body, acHeading)
	if i < 0 || (i > 0 && !strings.HasSuffix(body[:i], "\n\n")) {
		return "", "", false
	}
	return strings.TrimSuffix(body[:i], "\n\n"), strings.TrimRight(body[i+len(acHeading):], "\n"), true
}

// redirectedAt is when the task was last edited through EditTask, or zero.
func redirectedAt(ss []statusComment) time.Time {
	for i := len(ss) - 1; i >= 0; i-- {
		if ss[i].Kind == "pinned" && ss[i].Mode == "edit" {
			return ss[i].CreatedAt
		}
	}
	return time.Time{}
}

// EditTask replaces parts of a task's issue for c and pins the new text as
// approved by them, so the next job carries on from it on any forge,
// whether or not the forge records who edits an issue. devbot makes the
// edit as the implementer, whoever asks: it is devbot's record of the task
// that changes, as with a retarget, and only a bot's status can pin it.
//
// The edit, its pin and, for a task with an open PR, the fix round that
// redirects the PR are made under the task's lock, which a job starting
// takes to read the issue and check its pin, so no job sees the new text
// without the pin that approves it.
func (e *Engine) EditTask(ctx context.Context, req EditRequest, c Caller) (TaskStatus, error) {
	title, body, ac := strings.TrimSpace(req.Title), Defang(req.Body), Defang(req.AcceptanceCriteria)
	note := firstLine(strings.Join(strings.Fields(Defang(req.Note)), " "))
	// A planned parent task may be planned again for a note alone.
	if title == "" && body == "" && ac == "" && note == "" {
		return TaskStatus{}, invalid(errors.New("give at least one of title, body and acceptance_criteria"))
	}
	it, p, rc, err := e.load(ctx, c, req.Ref)
	if err != nil {
		return TaskStatus{}, err
	}
	ctx = e.TaskContext(ctx, it)
	if isParent(it, e.meta(ctx, it, nil)) {
		return e.editParent(ctx, c, it, p, rc, title, body, ac, note)
	}
	if title == "" && body == "" && ac == "" {
		return TaskStatus{}, invalid(errors.New("give at least one of title, body and acceptance_criteria"))
	}
	if err := editable(it); err != nil {
		return TaskStatus{}, err
	}
	if c.User != nil {
		if err := e.mayDirect(ctx, c, p, rc, it.Ref.Repo, "edit_task"); err != nil {
			return TaskStatus{}, err
		}
	}
	unlock, err := e.LockTask(ctx, it.Ref, connectorLockWait)
	if err != nil {
		return TaskStatus{}, err
	}
	t := &task{e: e, p: p, rc: rc, it: it}
	next, err := t.edit(ctx, c, title, body, ac, note)
	if _, uerr := unlock(); uerr != nil && err == nil {
		err = uerr
	}
	if err != nil {
		return TaskStatus{}, err
	}
	if _, err := e.EvaluateItem(ctx, it.Ref, SrcAPI); err != nil {
		return TaskStatus{}, err
	}
	st, err := e.Status(ctx, c, req.Ref)
	st.Note = strings.TrimSpace(next + " " + st.Note)
	return st, err
}

func editable(it forge.Item) error {
	if it.State != "open" {
		return invalid(fmt.Errorf("%s is closed: there's no task left to redirect", it.Ref))
	}
	if State(it) == "" {
		return invalid(fmt.Errorf("%s isn't a devbot task", it.Ref))
	}
	return nil
}

// edit is EditTask under the lock. It reports what happens next.
func (t *task) edit(ctx context.Context, c Caller, title, body, ac, note string) (string, error) {
	// Re-read under the lock: what load read may be stale by now.
	it, err := t.p.GetItem(ctx, t.it.Ref)
	if err != nil {
		return "", err
	}
	if err := editable(it); err != nil {
		return "", err
	}
	t.it, t.state = it, State(it)
	if err := t.load(ctx); err != nil {
		return "", err
	}
	newTitle, newBody := it.Title, it.Body
	if title != "" {
		newTitle = title
	}
	_, filed := metaOf(t.comments, t.e.bots(t.p)...)
	desc, oldAC, split := sections(it.Body)
	switch {
	case filed && split:
		if body != "" {
			desc = body
		}
		if ac != "" {
			oldAC = ac
		}
		if body != "" || ac != "" {
			newBody = taskBody(desc, oldAC)
		}
	case ac != "":
		return "", invalid(fmt.Errorf("%s wasn't filed through devbot, or no longer has the %q section file_task writes, so there are no acceptance criteria to replace: put them in body, which replaces the whole of the issue's body", it.Ref, "Acceptance criteria"))
	case body != "":
		newBody = body
	}
	var fields []string
	if newTitle != it.Title {
		fields = append(fields, "title")
	}
	if newBody != it.Body {
		if filed && split {
			ndesc, nac, _ := sections(newBody)
			odesc, oac, _ := sections(it.Body)
			if ndesc != odesc {
				fields = append(fields, "body")
			}
			if nac != oac {
				fields = append(fields, "acceptance criteria")
			}
		} else {
			fields = append(fields, "body")
		}
	}
	if len(fields) == 0 {
		newTitle, newBody = it.Title, it.Body // as it is, line endings and all
	}
	pin := PinOf(forge.Item{Title: newTitle, Body: newBody})
	pr, err := t.findPR(ctx)
	if err != nil {
		return "", err
	}
	redirect := pr != nil && pr.State == "open" && (t.state == LInReview || t.state == LApproved || t.state == LChanges)
	// The same edit again, as from a caller retrying after an error, does
	// only what the first left undone: the pin, then the fix round.
	pinned, redirected := false, false
	if len(fields) == 0 {
		for i := len(t.ss) - 1; i >= 0 && t.ss[i].Kind != "reset"; i-- {
			s := t.ss[i]
			if s.Kind == "fix-requested" && s.Mode == "edit" && redirect && s.PR == pr.Number {
				redirected = true
			}
			if s.Pin != "" {
				pinned = s.Pin == pin && s.Kind == "pinned" && s.Mode == "edit"
				break
			}
		}
	}
	if pinned && (!redirect || redirected) {
		return "The issue already says that; nothing changed.", nil
	}

	who := Defang(c.Principal)
	if c.User != nil {
		who = "@" + c.Login
	}
	var next string
	switch {
	case redirect:
		next = fmt.Sprintf("PR #%d goes back to the implementer with what changed, then the reviewer reviews it against the new text. An approval from before the edit doesn't count towards the merge.", pr.Number)
	case t.state == LNeedsHuman || t.state == LNeedsInfo || t.state == LNeedsApproval:
		next = fmt.Sprintf("The task stays at `%s`: the edit doesn't restart it. `retry`, an answer or `%s` does, and devbot then works from the new text.", t.state, LReady)
	default:
		next = "A run already under way carries on from the text it started with; the next job (a fix round or a review) works from the new text."
	}
	changed := strings.Join(fields, ", ")
	if changed == "" {
		changed = "nothing (the issue already said this)"
	}
	summary := fmt.Sprintf("edited by %s: %s", c.Principal, changed)
	if note != "" {
		summary += ": " + note
	}
	old := it
	if !pinned {
		if len(fields) > 0 {
			if err := t.p.EditItem(ctx, it.Ref, newTitle, newBody); err != nil {
				return "", refused(err)
			}
		}
		text := fmt.Sprintf("%s edited the issue through devbot. Changed: %s.", who, changed)
		if note != "" {
			text += " Note: " + note
		}
		text += fmt.Sprintf("\n\ndevbot works from the issue as it now stands, approved by %s. %s", who, next)
		if err := t.p.Comment(ctx, it.Ref, text+"\n\n"+RenderStatus(Status{Kind: "pinned", Mode: "edit", Pin: pin, Principal: c.Principal})); err != nil {
			return "", fmt.Errorf("devbot edited %s but couldn't pin the new text (%w): make the same edit again", it.URL, err)
		}
		it.Title, it.Body = newTitle, newBody
		t.it = it
	}
	if !redirect {
		t.e.Emit(ctx, it, "task.edited", t.state, summary, "")
		return next, nil
	}
	// On a retry, what the PR is asked to change is known only as far as
	// the last edit's diff: the earlier text is gone.
	reason := editReason(who, note, old, it)
	st := Status{Kind: "fix-requested", PR: pr.Number, SHA: pr.HeadSHA, Mode: "edit"}
	msg := fmt.Sprintf("Sending PR #%d back to the implementer: %s changed the task (this doesn't use a review round).\n\n%s", pr.Number, who, reason)
	if err := t.p.Comment(ctx, it.Ref, msg+"\n\n"+RenderStatus(st)); err != nil {
		return "", err
	}
	if err := t.e.transition(ctx, t.p, it, LChanges, "", ""); err != nil {
		return "", err
	}
	t.e.Emit(ctx, it, "task.edited", LChanges, summary, pr.URL)
	// A fix job already queued keeps its own reason; the harness adds this
	// round's from the status above.
	_, err = t.enqueueImplement(ctx, "fix", pr.Number, stripMarkers(msg))
	return next, err
}

// editReason tells the implementer what an edit changed, as a diff of the
// issue before and after.
func editReason(who, note string, old, cur forge.Item) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s changed the issue after this PR was opened, so the PR has to do what it says now. ", who)
	b.WriteString("Read the issue again in full, change the branch to match it (adding, changing or removing work), and say in your summary what you changed because of the edit.")
	if note != "" {
		fmt.Fprintf(&b, "\n\nTheir note: %s", note)
	}
	if old.Title != cur.Title {
		fmt.Fprintf(&b, "\n\nThe title was %q and is now %q.", Defang(old.Title), Defang(cur.Title))
	}
	if old.Body != cur.Body {
		d := Defang(lineDiff(old.Body, cur.Body))
		f := fenceFor(d)
		fmt.Fprintf(&b, "\n\nThe body changed (`-` was, `+` is now):\n\n%sdiff\n%s\n%s", f, d, f)
	}
	return b.String()
}

// fenceFor is a code fence longer than any run of backticks in s.
func fenceFor(s string) string {
	n, run := 3, 0
	for _, r := range s {
		if r == '`' {
			run++
			n = max(n, run+1)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", n)
}

// lineDiff is a unified-style diff of two texts, line by line, without
// hunk headers. Texts too long to compare line by line are shown whole.
func lineDiff(a, b string) string {
	x := strings.Split(strings.ReplaceAll(a, "\r\n", "\n"), "\n")
	y := strings.Split(strings.ReplaceAll(b, "\r\n", "\n"), "\n")
	var out []string
	if len(x)*len(y) > 1<<20 {
		for _, l := range x {
			out = append(out, "-"+l)
		}
		for _, l := range y {
			out = append(out, "+"+l)
		}
		return strings.Join(out, "\n")
	}
	// lcs[i][j] is the longest common subsequence of x[i:] and y[j:].
	lcs := make([][]int, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			out = append(out, " "+x[i])
			i, j = i+1, j+1
		case j < len(y) && (i == len(x) || lcs[i][j+1] >= lcs[i+1][j]):
			out = append(out, "+"+y[j])
			j++
		default:
			out = append(out, "-"+x[i])
			i++
		}
	}
	return strings.Join(out, "\n")
}

// editParent edits a parent task filed with plan for c, and has devbot plan
// it again from the new text: a plan waiting for approval no longer counts,
// and one already applied is planned again over the children as they stand.
// note, when set, is said to the planner, so a person can turn a plan down
// with a reason and nothing else changed.
func (e *Engine) editParent(ctx context.Context, c Caller, it forge.Item, p forge.Provider, rc *config.Repo, title, body, ac, note string) (TaskStatus, error) {
	if !e.meta(ctx, it, nil).Plan {
		return TaskStatus{}, invalid(fmt.Errorf("%s is a parent task without a plan, which does no work of its own: edit its child tasks instead", it.Ref))
	}
	if it.State != "open" {
		return TaskStatus{}, invalid(fmt.Errorf("parent task %s is closed", it.Ref))
	}
	if c.User != nil {
		if err := e.mayDirect(ctx, c, p, rc, it.Ref.Repo, "edit_task"); err != nil {
			return TaskStatus{}, err
		}
	}
	unlock, err := e.LockTask(ctx, it.Ref, connectorLockWait)
	if err != nil {
		return TaskStatus{}, err
	}
	next, err := func() (string, error) {
		defer unlock()
		it, err := p.GetItem(ctx, it.Ref)
		if err != nil {
			return "", err
		}
		newTitle, newBody := it.Title, it.Body
		if title != "" {
			newTitle = title
		}
		desc, oldAC, split := sections(it.Body)
		switch {
		case split && (body != "" || ac != ""):
			newBody = taskBody(cmp.Or(body, desc), cmp.Or(ac, oldAC))
		case ac != "":
			newBody = taskBody(cmp.Or(body, it.Body), ac)
		case body != "":
			newBody = body
		}
		var fields []string
		if newTitle != it.Title {
			fields = append(fields, "title")
		}
		if newBody != it.Body {
			fields = append(fields, "body")
		}
		if len(fields) == 0 && note == "" {
			return "The issue already says that; nothing changed.", nil
		}
		if len(fields) > 0 {
			if err := p.EditItem(ctx, it.Ref, newTitle, newBody); err != nil {
				return "", refused(err)
			}
		}
		who := Defang(c.Principal)
		if c.User != nil {
			who = "@" + c.Login
		}
		changed := strings.Join(fields, ", ")
		if changed == "" {
			changed = "nothing"
		}
		text := fmt.Sprintf("%s edited this parent task through devbot. Changed: %s.", who, changed)
		if note != "" {
			text += " Note: " + note
		}
		next := "devbot plans the change again from the issue as it now stands; a plan waiting for approval no longer counts."
		if err := p.Comment(ctx, it.Ref, text+"\n\n"+next+"\n\n"+RenderStatus(Status{Kind: "plan", Mode: PlanReset, Principal: c.Principal})); err != nil {
			return "", err
		}
		e.Emit(ctx, it, "task.edited", ParentPlanning, fmt.Sprintf("edited by %s: %s", c.Principal, changed), "")
		return next, nil
	}()
	if err != nil {
		return TaskStatus{}, err
	}
	if _, err := e.EvaluateItem(ctx, it.Ref, SrcAPI); err != nil {
		return TaskStatus{}, err
	}
	st, err := e.Status(ctx, c, it.Ref.String())
	st.Note = strings.TrimSpace(next + " " + st.Note)
	return st, err
}

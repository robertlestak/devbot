package core

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"git.shdw.tech/rob/devbot/internal/audit"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// A task runs each role at a reasoning effort, chosen as its model is (see
// model.go). Highest first: its own (filed with one, inherited from its
// parent task when filed, or set since by edit_task or an `/effort`
// comment), the repo's agents.<role>.effort, then the deployment's. None of
// them set runs the role at its CLI's default. The task's own is recorded
// on the forge, the last "effort" status for the role, else the routing
// block, so every run reads the same one after a Redis wipe. The effort
// belongs to the role, not to its model: a run that falls back to another
// model keeps it.

// EffortChoice is the effort a role of a task runs at, and where it came
// from (ModelFromTask, ModelFromRepo or ModelFromDeployment). Both are ""
// when nothing sets one.
type EffortChoice struct {
	Effort string `json:"effort,omitempty"`
	From   string `json:"from,omitempty"`
}

// TaskEfforts are a task's effective efforts, by role.
type TaskEfforts struct {
	Implement EffortChoice `json:"implement"`
	Review    EffortChoice `json:"review"`
}

// ErrBadEffort is an effort file_task, edit_task or `/effort` can't use.
var ErrBadEffort = errors.New("bad effort")

// parseEffort reads a task's effort as a caller or a comment gives it: ""
// is none given, and "none" clears it.
func parseEffort(s string) (effort string, set bool, err error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", false, nil
	case s == modelNone:
		return "", true, nil
	case !config.ValidEffort(s):
		return "", false, invalid(fmt.Errorf("%w %q: want %s, or %s to clear it", ErrBadEffort, s, config.EffortRule, modelNone))
	}
	return s, true, nil
}

// recordedEffort is the task's own effort for role, or "" for none.
func recordedEffort(ss []statusComment, m Meta, role string) string {
	for i := len(ss) - 1; i >= 0; i-- {
		if ss[i].Kind == "effort" && ss[i].Role == role {
			return ss[i].Effort
		}
	}
	if role == "review" {
		return m.ReviewEffort
	}
	return m.Effort
}

// effortChoice resolves role's effort for a task whose own is own ("" for
// none), on repo config rc.
func (e *Engine) effortChoice(rc *config.Repo, role, own string) EffortChoice {
	d, r := e.Cfg.Agents.Implement.Effort, rc.Agents.Implement.Effort
	if role == "review" {
		d, r = e.Cfg.Agents.Review.Effort, rc.Agents.Review.Effort
	}
	switch {
	case own != "":
		return EffortChoice{own, ModelFromTask}
	case r != "":
		return EffortChoice{r, ModelFromRepo}
	case d != "":
		return EffortChoice{d, ModelFromDeployment}
	}
	return EffortChoice{}
}

// taskEfforts are the task's effective efforts, from its comments.
func (e *Engine) taskEfforts(ctx context.Context, rc *config.Repo, it forge.Item, ss []statusComment, comments []forge.Comment) TaskEfforts {
	m := e.meta(ctx, it, comments)
	return TaskEfforts{
		Implement: e.effortChoice(rc, "implement", recordedEffort(ss, m, "implement")),
		Review:    e.effortChoice(rc, "review", recordedEffort(ss, m, "review")),
	}
}

// TaskEffort is the effort a job of the task runs role at, for the harness,
// from the task's comments as the job read them.
func (e *Engine) TaskEffort(ctx context.Context, rc *config.Repo, it forge.Item, comments []forge.Comment, role string) (EffortChoice, error) {
	p, err := e.provider(it.Ref.Repo)
	if err != nil {
		return EffortChoice{}, err
	}
	ss := statuses(comments, e.bots(p)...)
	return e.effortChoice(rc, role, recordedEffort(ss, e.meta(ctx, it, comments), role)), nil
}

// setEffort records the task's own effort for role, "" for none, set by
// who, as the comment names them, for principal, as the audit log does. It
// reports whether that changed it.
func (e *Engine) setEffort(ctx context.Context, p forge.Provider, it forge.Item, ss []statusComment, m Meta, role, effort, who, principal, note string) (bool, error) {
	cur := recordedEffort(ss, m, role)
	if cur == effort {
		return false, nil
	}
	var text string
	if effort == "" {
		text = fmt.Sprintf("%s cleared this task's %s effort, so it runs at this repo's or the deployment's, else the harness's default.", who, roleWord(role))
	} else {
		text = fmt.Sprintf("%s set this task's %s effort to %s.", who, roleWord(role), codeText(effort))
	}
	if note != "" {
		text += " Note: " + note
	}
	text += " It applies from the next run on; a run under way carries on at the effort it started at."
	err := p.Comment(ctx, it.Ref, text+"\n\n"+RenderStatus(Status{Kind: "effort", Role: role, Effort: effort, Principal: principal}))
	e.Audit.Log(e.TaskContext(ctx, it), audit.Record{Event: "effort.set", Actor: principal, ActorKind: "user", Err: err,
		Detail: map[string]any{"role": role, "effort": effort, "from": cur}})
	return err == nil, err
}

// effortCommand is the comment that sets a task's effort: `/effort <level>`
// for the implementer, `/effort review <level>` for the reviewer, with
// `none` for the level clearing it.
const effortCommand = "/effort"

// parseEffortCommand reads an `/effort` comment's role and what it asks
// for, unchecked.
func parseEffortCommand(body string) (role, arg string, ok bool) {
	f := strings.Fields(firstLine(body))
	if len(f) == 0 || f[0] != effortCommand {
		return "", "", false
	}
	role, f = "implement", f[1:]
	if len(f) > 0 && f[0] == "review" {
		role, f = "review", f[1:]
	}
	return role, strings.Join(f, " "), true
}

// effortCommand acts on the latest `/effort` comment for each role from
// someone who can direct devbot since that role's effort was last set or
// an `/effort` for it refused, as modelCommand does for `/model`.
func (t *task) effortCommand(ctx context.Context) (bool, error) {
	if t.state == LNeedsHuman {
		return false, nil
	}
	if err := t.load(ctx); err != nil {
		return false, err
	}
	since := map[string]int64{}
	answered := map[int64]bool{}
	for _, s := range t.ss {
		switch s.Kind {
		case "effort", "effort-refused":
			since[s.Role] = s.ID
		case "effort-ignored":
			answered[s.Since] = true
		}
	}
	type ask struct{ arg, by string }
	asks := map[string]ask{}
	acted := false
	bots := t.e.bots(t.p)
	for _, c := range t.comments {
		if forge.HasLogin(bots, c.Author) {
			continue
		}
		role, arg, ok := parseEffortCommand(c.Body)
		if !ok || c.ID <= since[role] {
			continue
		}
		if ok, why := t.e.authorize(ctx, t.p, t.rc, t.it.Ref.Repo, c.Author); !ok {
			if answered[c.ID] {
				continue
			}
			t.e.auditRefusal(ctx, c.Author, "effort", why, audit.Fields{})
			body := fmt.Sprintf("Ignoring `%s` from @%s: %s.\n\n%s", effortCommand, c.Author, why, RenderStatus(Status{Kind: "effort-ignored", Since: c.ID, Principal: c.Author}))
			if err := t.p.Comment(ctx, t.it.Ref, body); err != nil {
				return acted, err
			}
			acted = true
			continue
		}
		asks[role] = ask{arg, c.Author}
	}
	m := t.e.meta(ctx, t.it, t.comments)
	for _, role := range []string{"implement", "review"} {
		a, ok := asks[role]
		if !ok {
			continue
		}
		effort, set, err := parseEffort(a.arg)
		if err == nil && !set {
			err = invalid(fmt.Errorf("%w: name one, as `%s <level>`, `%s review <level>`, or `%s` to clear it", ErrBadEffort, effortCommand, effortCommand, modelNone))
		}
		if err != nil {
			body := fmt.Sprintf("Not changing this task's %s effort, as @%s asked: %s.\n\n%s",
				roleWord(role), a.by, Defang(err.Error()), RenderStatus(Status{Kind: "effort-refused", Role: role, Principal: a.by}))
			if err := t.p.Comment(ctx, t.it.Ref, body); err != nil {
				return acted, err
			}
			acted = true
			continue
		}
		// Left unanswered when it changes nothing, as with `/model`.
		changed, err := t.e.setEffort(ctx, t.p, t.it, t.ss, m, role, effort, "@"+a.by, a.by, "")
		if err != nil {
			return acted, err
		}
		acted = acted || changed
	}
	if !acted {
		return false, nil
	}
	t.comments = nil
	return true, t.load(ctx)
}

// setEfforts sets the task's own efforts for a connector call: each of
// effort and review that is set ("none" clears it) replaces that role's. It
// reports what it changed, for the caller.
func (e *Engine) setEfforts(ctx context.Context, p forge.Provider, it forge.Item, effort, review, who, principal, note string) (string, error) {
	cs, err := p.ListComments(ctx, it.Ref)
	if err != nil {
		return "", err
	}
	ss := statuses(cs, e.bots(p)...)
	m := e.meta(ctx, it, cs)
	var said []string
	for _, r := range []struct{ role, arg string }{{"implement", effort}, {"review", review}} {
		effort, set, _ := parseEffort(r.arg) // checked by the caller
		if !set {
			continue
		}
		changed, err := e.setEffort(ctx, p, it, ss, m, r.role, effort, who, principal, note)
		if err != nil {
			return "", err
		}
		if changed {
			said = append(said, fmt.Sprintf("The %s effort changes from the next run on.", roleWord(r.role)))
		}
	}
	return strings.Join(said, " "), nil
}

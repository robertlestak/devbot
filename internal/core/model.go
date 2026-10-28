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

// A task runs each role on a model. Highest first: its own (filed with one,
// inherited from its parent task when filed, or set since by edit_task or a
// `/model` comment), the repo's agents.<role>.model, then the deployment's.
// The harness is always the deployment's. The task's own is recorded on the
// forge, as its base is, so every run of it reads the same one after a Redis
// wipe: the last "model" status for the role, else the routing block the bot
// posted when it filed the task. A task's model that repos.limits.models
// doesn't allow is passed over for the repo's, then the deployment's, and
// devbot says so on the issue once ("model-fallback").

// Where a role's model came from, as task_status shows it.
const (
	ModelFromTask       = "task"
	ModelFromRepo       = "repo"
	ModelFromDeployment = "deployment"
)

// modelNone, as a task's model, clears it.
const modelNone = "none"

// ModelChoice is the model a role of a task runs on, and where it came from.
type ModelChoice struct {
	Model string `json:"model"`
	From  string `json:"from"`
	// Refused is the task's own model, when repos.limits.models doesn't
	// allow it and Model is the next one down instead.
	Refused string `json:"refused,omitempty"`
}

// TaskModels are a task's effective models, by role.
type TaskModels struct {
	Implement ModelChoice `json:"implement"`
	Review    ModelChoice `json:"review"`
}

// ErrBadModel is a model name file_task, edit_task or `/model` can't use.
var ErrBadModel = errors.New("bad model")

// parseModel reads a task's model as a caller or a comment gives it: "" is
// none given, and "none" clears it.
func parseModel(s string) (model string, set bool, err error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", false, nil
	case s == modelNone:
		return "", true, nil
	case !config.ValidModel(s):
		return "", false, invalid(fmt.Errorf("%w %q: want %s, or %s to clear it", ErrBadModel, s, config.ModelRule, modelNone))
	}
	return s, true, nil
}

// roleWord names a role in what devbot says.
func roleWord(role string) string {
	if role == "review" {
		return "reviewer"
	}
	return "implementer"
}

// recordedModel is the task's own model for role, or "" for none.
func recordedModel(ss []statusComment, m Meta, role string) string {
	for i := len(ss) - 1; i >= 0; i-- {
		if ss[i].Kind == "model" && ss[i].Role == role {
			return ss[i].Model
		}
	}
	if role == "review" {
		return m.ReviewModel
	}
	return m.Model
}

// modelChoice resolves role's model for a task whose own is own ("" for
// none), on repo config rc, whose model Cap has already held to the limits.
func (e *Engine) modelChoice(rc *config.Repo, role, own string) ModelChoice {
	d, r := e.Cfg.Agents.Implement.Model, rc.Agents.Implement.Model
	if role == "review" {
		d, r = e.Cfg.Agents.Review.Model, rc.Agents.Review.Model
	}
	var c ModelChoice
	if own != "" {
		if e.Cfg.Repos.Limits.AllowsModel(own) {
			return ModelChoice{Model: own, From: ModelFromTask}
		}
		c.Refused = own
	}
	if r != "" {
		c.Model, c.From = r, ModelFromRepo
	} else {
		c.Model, c.From = d, ModelFromDeployment
	}
	return c
}

// taskModels are the task's effective models, from its comments.
func (e *Engine) taskModels(ctx context.Context, rc *config.Repo, it forge.Item, ss []statusComment, comments []forge.Comment) TaskModels {
	m := e.meta(ctx, it, comments)
	return TaskModels{
		Implement: e.modelChoice(rc, "implement", recordedModel(ss, m, "implement")),
		Review:    e.modelChoice(rc, "review", recordedModel(ss, m, "review")),
	}
}

// TaskModel is the model a job of the task runs role on, for the harness,
// from the task's comments as the job read them. A task's model the install
// doesn't allow is said on the issue, once for each time it is set.
func (e *Engine) TaskModel(ctx context.Context, rc *config.Repo, it forge.Item, comments []forge.Comment, role string) (ModelChoice, error) {
	p, err := e.provider(it.Ref.Repo)
	if err != nil {
		return ModelChoice{}, err
	}
	ss := statuses(comments, e.bots(p)...)
	c := e.modelChoice(rc, role, recordedModel(ss, e.meta(ctx, it, comments), role))
	if c.Refused == "" {
		return c, nil
	}
	for i := len(ss) - 1; i >= 0; i-- {
		s := ss[i]
		if s.Role != role || (s.Kind != "model" && s.Kind != "model-fallback") {
			continue
		}
		if s.Kind == "model-fallback" && s.Model == c.Refused {
			return c, nil // said
		}
		break
	}
	from := "the deployment's"
	if c.From == ModelFromRepo {
		from = fmt.Sprintf("this repo's, from `%s`", config.RepoFile)
	}
	body := fmt.Sprintf("This task asks for %s as its %s model, which this install's `repos.limits.models` doesn't allow, so the %s runs on %s (%s) instead.",
		codeText(c.Refused), roleWord(role), roleWord(role), codeText(c.Model), from)
	return c, p.Comment(ctx, it.Ref, body+"\n\n"+RenderStatus(Status{Kind: "model-fallback", Role: role, Model: c.Refused}))
}

// setModel records the task's own model for role, "" for none, set by who,
// as the comment names them, for principal, as the audit log does. It
// reports whether that changed it.
func (e *Engine) setModel(ctx context.Context, p forge.Provider, it forge.Item, ss []statusComment, m Meta, role, model, who, principal, note string) (bool, error) {
	cur := recordedModel(ss, m, role)
	if cur == model {
		return false, nil
	}
	var text string
	if model == "" {
		text = fmt.Sprintf("%s cleared this task's %s model, so it runs on this repo's or the deployment's.", who, roleWord(role))
	} else {
		text = fmt.Sprintf("%s set this task's %s model to %s.", who, roleWord(role), codeText(model))
	}
	if note != "" {
		text += " Note: " + note
	}
	text += " It applies from the next run on; a run under way carries on with the model it started on."
	err := p.Comment(ctx, it.Ref, text+"\n\n"+RenderStatus(Status{Kind: "model", Role: role, Model: model, Principal: principal}))
	e.Audit.Log(e.TaskContext(ctx, it), audit.Record{Event: "model.set", Actor: principal, ActorKind: "user", Err: err,
		Detail: map[string]any{"role": role, "model": model, "from": cur}})
	return err == nil, err
}

// modelCommand is the comment that sets a task's model: `/model <name>` for
// the implementer, `/model review <name>` for the reviewer, with `none` for
// the name clearing it.
const modelCommand = "/model"

// parseModelCommand reads a `/model` comment's role and what it asks for,
// unchecked.
func parseModelCommand(body string) (role, arg string, ok bool) {
	f := strings.Fields(firstLine(body))
	if len(f) == 0 || f[0] != modelCommand {
		return "", "", false
	}
	role, f = "implement", f[1:]
	if len(f) > 0 && f[0] == "review" {
		role, f = "review", f[1:]
	}
	return role, strings.Join(f, " "), true
}

// modelCommand acts on the latest `/model` comment for each role from
// someone who can direct devbot since that role's model was last set or a
// `/model` for it refused. One from anyone else is answered once, saying
// why it is ignored. A stopped task reads no comments: its `/model` counts
// once it is retried.
func (t *task) modelCommand(ctx context.Context) (bool, error) {
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
		case "model", "model-refused":
			since[s.Role] = s.ID
		case "model-ignored":
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
		role, arg, ok := parseModelCommand(c.Body)
		if !ok || c.ID <= since[role] {
			continue
		}
		if ok, why := t.e.authorize(ctx, t.p, t.rc, t.it.Ref.Repo, c.Author); !ok {
			if answered[c.ID] {
				continue
			}
			t.e.auditRefusal(ctx, c.Author, "model", why, audit.Fields{})
			body := fmt.Sprintf("Ignoring `%s` from @%s: %s.\n\n%s", modelCommand, c.Author, why, RenderStatus(Status{Kind: "model-ignored", Since: c.ID, Principal: c.Author}))
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
		model, set, err := parseModel(a.arg)
		if err == nil && !set {
			err = invalid(fmt.Errorf("%w: name one, as `%s <name>`, `%s review <name>`, or `%s` to clear it", ErrBadModel, modelCommand, modelCommand, modelNone))
		}
		if err != nil {
			body := fmt.Sprintf("Not changing this task's %s model, as @%s asked: %s.\n\n%s",
				roleWord(role), a.by, Defang(err.Error()), RenderStatus(Status{Kind: "model-refused", Role: role, Principal: a.by}))
			if err := t.p.Comment(ctx, t.it.Ref, body); err != nil {
				return acted, err
			}
			acted = true
			continue
		}
		// Left unanswered when it changes nothing, as with `/base`.
		changed, err := t.e.setModel(ctx, t.p, t.it, t.ss, m, role, model, "@"+a.by, a.by, "")
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

// setModels sets the task's own models for a connector call: each of model
// and review that is set ("none" clears it) replaces that role's. It reports
// what it changed, for the caller.
func (e *Engine) setModels(ctx context.Context, p forge.Provider, it forge.Item, model, review, who, principal, note string) (string, error) {
	cs, err := p.ListComments(ctx, it.Ref)
	if err != nil {
		return "", err
	}
	ss := statuses(cs, e.bots(p)...)
	m := e.meta(ctx, it, cs)
	var said []string
	for _, r := range []struct{ role, arg string }{{"implement", model}, {"review", review}} {
		model, set, _ := parseModel(r.arg) // checked by the caller
		if !set {
			continue
		}
		changed, err := e.setModel(ctx, p, it, ss, m, r.role, model, who, principal, note)
		if err != nil {
			return "", err
		}
		if changed {
			said = append(said, fmt.Sprintf("The %s model changes from the next run on.", roleWord(r.role)))
		}
	}
	return strings.Join(said, " "), nil
}

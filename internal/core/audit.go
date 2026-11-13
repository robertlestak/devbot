package core

import (
	"context"

	"git.shdw.tech/rob/devbot/internal/audit"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// TaskContext is ctx with it's audit fields: whatever is written while
// working on it is recorded against the task and the origin it was filed
// with. The origin comes from the routing cache only: reading comments for
// it would cost every evaluation of a forge-labelled task a forge call,
// since such a task has no routing comment to cache.
func (e *Engine) TaskContext(ctx context.Context, it forge.Item) context.Context {
	m, _ := e.metas.get(it.Ref.String())
	return audit.With(ctx, audit.Fields{Repo: it.Ref.Repo.String(), Task: it.Ref.String(), Origin: m.Origin})
}

// auditTransition records a change of it's state label, made by actor.
func (e *Engine) auditTransition(ctx context.Context, it forge.Item, actor, kind, to, eventType, summary string, err error) {
	e.Audit.Log(e.TaskContext(ctx, it), audit.Record{Event: "task.transition", Actor: actor, ActorKind: kind, Err: err,
		Detail: map[string]any{"from": State(it), "to": to, "type": eventType, "summary": summary}})
}

// auditRefusal records devbot refusing who (a forge login, or a principal)
// something: action is what they asked for, why the rule that said no.
func (e *Engine) auditRefusal(ctx context.Context, who, action, why string, f audit.Fields) {
	e.Audit.Log(ctx, audit.Record{Event: "authz.refused", Outcome: audit.Refused, Actor: who, ActorKind: "user", Fields: f,
		Detail: map[string]any{"action": action, "reason": why}})
}

// callerWho is who a refusal of c is recorded against.
func callerWho(c Caller) string {
	if c.Login != "" {
		return c.Login
	}
	return c.Principal
}

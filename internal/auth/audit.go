package auth

import (
	"context"

	"git.shdw.tech/rob/devbot/internal/audit"
)

// auditEvent audits a sign-in event for login (when known) from client. No
// token, code or secret is ever passed in detail: only what they were for.
func (s *Server) auditEvent(ctx context.Context, event, outcome, login, client string, detail map[string]any) {
	if s.o.Audit == nil {
		return
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["provider"] = s.o.Forge.Name()
	if client != "" {
		detail["client_id"] = client
	}
	s.o.Audit.Log(ctx, audit.Record{Event: event, Outcome: outcome, Actor: login, ActorKind: "user",
		Fields: audit.Fields{Login: login, Principal: login}, Detail: detail})
}

// tokenEvent is the audit event of a token request with grant type gt.
func tokenEvent(gt string) string {
	if gt == grantRefresh {
		return "auth.token.refresh"
	}
	return "auth.token.issue"
}

// failure is the outcome of a request answered with HTTP status code.
func failure(code int) string {
	if code >= 500 {
		return audit.Error
	}
	return audit.Refused
}

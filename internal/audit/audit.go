// Package audit writes devbot's audit records: one JSON line per auditable
// action (a forge write, a task transition, a refusal, a sign-in event, a
// model run), on stdout of the process that took it. devbot keeps no audit
// store; the platform collects stdout and owns routing and retention.
// Operational logs go to stderr, and every audit line carries
// "log_type":"audit", so the two never need telling apart by content.
package audit

import (
	"context"
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// LogType is the value of every record's log_type.
const LogType = "audit"

// Outcomes.
const (
	OK      = "ok"
	Error   = "error"
	Refused = "refused"
	Denied  = "denied" // a person said no (a consent page)
)

// Fields are the common fields an action inherits from its context: who
// asked, about which task, in which request or job.
type Fields struct {
	Principal string `json:"principal,omitempty"` // the connector caller, as recorded on tasks
	Login     string `json:"login,omitempty"`     // the caller's forge login, when signed in
	Origin    string `json:"origin,omitempty"`    // the task's origin, as filed
	Repo      string `json:"repo,omitempty"`
	Task      string `json:"task,omitempty"`
	PR        int    `json:"pr,omitempty"`
	Job       string `json:"job_id,omitempty"`     // the job pod
	Request   string `json:"request_id,omitempty"` // the HTTP request or webhook delivery
	Attempt   int    `json:"attempt,omitempty"`    // implement attempt
	Round     int    `json:"round,omitempty"`      // review round
}

type fieldsKey struct{}

// With returns ctx carrying f's non-zero fields over those already there.
func With(ctx context.Context, f Fields) context.Context {
	cur := From(ctx)
	cur.merge(f)
	return context.WithValue(ctx, fieldsKey{}, cur)
}

// From is the fields ctx carries.
func From(ctx context.Context) Fields {
	f, _ := ctx.Value(fieldsKey{}).(Fields)
	return f
}

func (f *Fields) merge(o Fields) {
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&f.Principal, o.Principal)
	set(&f.Login, o.Login)
	set(&f.Origin, o.Origin)
	set(&f.Repo, o.Repo)
	set(&f.Task, o.Task)
	set(&f.Job, o.Job)
	set(&f.Request, o.Request)
	for _, p := range []struct {
		dst *int
		v   int
	}{{&f.PR, o.PR}, {&f.Attempt, o.Attempt}, {&f.Round, o.Round}} {
		if p.v != 0 {
			*p.dst = p.v
		}
	}
}

// Record is one auditable action. Fields set here win over the context's.
type Record struct {
	Event   string
	Outcome string // default OK
	// Actor is who the forge sees doing it: a bot's login, or the person's
	// whose token was used. ActorKind is implementer, reviewer, user or
	// devbot (an action of devbot's own, such as issuing a token).
	Actor, ActorKind string
	Err              error
	Detail           map[string]any
	Fields
}

// line is a record as written. The order of the fields is the schema's.
type line struct {
	Time      time.Time `json:"time"`
	LogType   string    `json:"log_type"`
	Component string    `json:"component"`
	Event     string    `json:"event"`
	Outcome   string    `json:"outcome"`
	Actor     string    `json:"actor"`
	ActorKind string    `json:"actor_kind"`
	Fields
	Error  string         `json:"error,omitempty"`
	Detail map[string]any `json:"detail,omitempty"`
}

// Logger writes records to one writer, a line each. A nil *Logger writes
// nothing, so code that audits needn't check.
type Logger struct {
	// Component names the process: serve, harness, reconcile.
	Component string
	Now       func() time.Time

	mu      sync.Mutex
	w       io.Writer
	secrets []string
}

func New(w io.Writer, component string) *Logger {
	return &Logger{w: w, Component: component, Now: time.Now}
}

// Secret registers values that must never appear in a record, such as the
// credentials this process loaded. Each is replaced wherever it occurs, on
// top of the patterns every credential devbot knows of matches.
func (l *Logger) Secret(vals ...string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, v := range vals {
		v = strings.TrimSpace(v)
		// Short values would redact ordinary words; no real credential is.
		if len(v) < 8 {
			continue
		}
		// A credential refreshed for each git command comes back unchanged
		// until it nears expiry; once is enough.
		if slices.Contains(l.secrets, v) {
			continue
		}
		l.secrets = append(l.secrets, v)
		if b, _ := json.Marshal(v); string(b[1:len(b)-1]) != v {
			l.secrets = append(l.secrets, string(b[1:len(b)-1]))
		}
	}
}

// Log writes r, with ctx's fields under its own.
func (l *Logger) Log(ctx context.Context, r Record) {
	if l == nil {
		return
	}
	f := From(ctx)
	f.merge(r.Fields)
	ln := line{Time: l.Now().UTC(), LogType: LogType, Component: l.Component, Event: r.Event, Outcome: r.Outcome,
		Actor: r.Actor, ActorKind: r.ActorKind, Fields: f, Detail: r.Detail}
	if ln.Outcome == "" {
		ln.Outcome = OK
		if r.Err != nil {
			ln.Outcome = Error
		}
	}
	if r.Err != nil {
		ln.Error = r.Err.Error()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.scrub(&ln)
	b, err := json.Marshal(ln)
	if err != nil {
		b, _ = json.Marshal(line{Time: ln.Time, LogType: LogType, Component: l.Component, Event: ln.Event,
			Outcome: ln.Outcome, Actor: ln.Actor, ActorKind: ln.ActorKind, Fields: ln.Fields, Error: "unencodable detail: " + err.Error()})
	}
	l.w.Write(append(b, '\n'))
}

// scrub redacts every string in ln. It holds l.mu.
func (l *Logger) scrub(ln *line) {
	for _, s := range []*string{&ln.Actor, &ln.Principal, &ln.Login, &ln.Origin, &ln.Repo, &ln.Task, &ln.Job, &ln.Request, &ln.Error} {
		*s = l.redact(*s)
	}
	if ln.Detail != nil {
		d := make(map[string]any, len(ln.Detail))
		for k, v := range ln.Detail {
			d[k] = l.value(v)
		}
		ln.Detail = d
	}
}

func (l *Logger) value(v any) any {
	switch x := v.(type) {
	case string:
		return l.redact(x)
	case []string:
		out := make([]string, len(x))
		for i, s := range x {
			out[i] = l.redact(s)
		}
		return out
	case json.RawMessage:
		r := l.redact(string(x))
		if json.Valid([]byte(r)) {
			return json.RawMessage(r)
		}
		return r
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = l.value(v)
		}
		return out
	}
	return v // numbers, bools, times
}

const redacted = "[REDACTED]"

// secretRE matches the credentials devbot handles or an agent might print:
// devbot's own tokens, forge tokens, model API keys, AWS keys, private keys,
// JWTs and HTTP auth headers. kvRE is a credential-named key's value, and
// userinfoRE a password in a URL.
var (
	secretRE = regexp.MustCompile(strings.Join([]string{
		`devbot_(?:at|rt|cs|code|dc|session)_[A-Za-z0-9_-]{16,}`,
		`\bgh[pousr]_[A-Za-z0-9]{20,}`,
		`\bgithub_pat_[A-Za-z0-9_]{20,}`,
		`\bglpat-[A-Za-z0-9_-]{20,}`,
		`\bsk-[A-Za-z0-9_-]{20,}`,
		`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`,
		`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`,
		`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`,
		`(?i)\b(?:bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{16,}`,
	}, "|"))
	kvRE       = regexp.MustCompile(`(?i)((?:aws_secret_access_key|aws_session_token|client_secret|api[_-]?key|password|passwd|secret|token)["']?\s*[:=]\s*["']?)[^\s"',;&]{8,}`)
	userinfoRE = regexp.MustCompile(`://[^/\s:@"]+:[^/\s@"]+@`)
)

func (l *Logger) redact(s string) string {
	if s == "" {
		return s
	}
	for _, v := range l.secrets {
		s = strings.ReplaceAll(s, v, redacted)
	}
	s = secretRE.ReplaceAllString(s, redacted)
	s = kvRE.ReplaceAllString(s, "${1}"+redacted)
	return userinfoRE.ReplaceAllString(s, "://"+redacted+"@")
}

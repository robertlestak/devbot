package server

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/audit"
	"git.shdw.tech/rob/devbot/internal/auth"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// The web UI (ui.enabled): server-rendered pages over the same functions the
// /api endpoints serve, with the same visibility rule. Every page sits
// directly under /ui/ and links relatively, so it works under whatever path
// a proxy puts in front. It only reads.

//go:embed ui
var uiFS embed.FS

var uiPages = map[string]*template.Template{}

func init() {
	base := template.Must(template.New("").Funcs(uiFuncs).ParseFS(uiFS, "ui/layout.html"))
	for _, p := range []string{"jobs", "tasks", "task", "events", "signin", "error"} {
		uiPages[p] = template.Must(template.Must(base.Clone()).ParseFS(uiFS, "ui/"+p+".html"))
	}
}

// uiCSP allows the page's own stylesheet and script and nothing else: no
// inline script or style, no frames, no other origin.
const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; " +
	"form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

func uiHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", uiCSP)
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) uiRoutes(mux *http.ServeMux) {
	handle := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, uiHeaders(s.knownHost(h))) }
	toUI := func(w http.ResponseWriter, r *http.Request) {
		// Relative, so a proxy's path prefix is kept.
		w.Header().Set("Location", "ui/")
		w.WriteHeader(http.StatusFound)
	}
	handle("GET /ui", toUI)
	// The host (or prefix) root opens the UI too, for installs served at the
	// root of their own host. {$} matches only "/", so /mcp, /api/... and the
	// OAuth routes are untouched.
	handle("GET /{$}", toUI)
	handle("GET /ui/{$}", s.ui(s.uiJobs))
	handle("GET /ui/tasks", s.ui(s.uiTasks))
	handle("GET /ui/task", s.ui(s.uiTask))
	handle("GET /ui/events", s.ui(s.uiEvents))
	static, _ := fs.Sub(uiFS, "ui/static")
	mux.Handle("GET /ui/static/", uiHeaders(s.knownHost(http.StripPrefix("/ui/static/", http.FileServerFS(static)).ServeHTTP)))
	if s.Auth != nil {
		handle("GET /ui/login", s.Auth.SignIn)
		handle("POST /ui/logout", s.Auth.SignOut)
	}
}

// uiData is everything a page renders. Each page uses its own part.
type uiData struct {
	Page    string
	Title   string
	Refresh int // seconds between live refreshes; 0 for none
	OAuth   bool
	Login   string
	CSRF    string
	Grafana string
	Forge   string
	// OnlyForge is the install's provider when it has just one, so repos
	// can show as owner/name; empty with several.
	OnlyForge string
	Now       time.Time
	Error     string

	Jobs   Jobs
	Groups []taskGroup
	// Parents are the open parent tasks, each with its children.
	Parents []core.TaskStatus
	Task    core.TaskDetail
	Events  EventsPage
	Older   string

	Repos, States, Types      []string
	Repo, State, Type, Origin string
}

type taskGroup struct {
	State string
	Tasks []core.TaskStatus
}

// uiStates is the tasks page's order: what needs a person first.
var uiStates = []string{"needs-human", "needs-approval", "needs-info", "working", "in-review", "changes-requested", "approved", "ready"}

type uiHandler func(w http.ResponseWriter, r *http.Request, c core.Caller, d *uiData) (string, int, error)

// ui resolves who the page is for: in auth mode none, the connectors'
// anonymous principal, so with none the UI is closed like they are; else
// the person signed in to the UI. Either sees only what they could through
// the API.
func (s *Server) ui(h uiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d := &uiData{OAuth: s.Auth != nil, Grafana: s.Cfg.UI.GrafanaURL, Now: time.Now(), Forge: s.Cfg.Auth.Provider}
		if len(s.E.Forges) == 1 {
			for name := range s.E.Forges {
				d.OnlyForge = name
			}
		}
		var c core.Caller
		if s.Auth == nil {
			if s.Cfg.AnonymousPrincipal == "" {
				s.E.Audit.Log(r.Context(), audit.Record{Event: "authz.refused", Outcome: audit.Refused,
					Detail: map[string]any{"action": "ui " + r.URL.Path, "reason": "no anonymous_principal"}})
				d.Title, d.Error = "Not available", "This install's web UI has no one to read as: it needs anonymous_principal, or auth.mode oauth."
				s.render(w, "error", 401, d)
				return
			}
			c = core.Caller{Principal: s.Cfg.AnonymousPrincipal}
		} else {
			id, err := s.Auth.Session(r)
			var u forge.User
			if err == nil {
				u, err = s.Auth.User(r.Context(), id)
			}
			if err != nil {
				s.uiSignIn(w, r, err, d)
				return
			}
			c = core.Caller{Principal: id.Login, User: u, Provider: id.Provider, Login: id.Login}
			d.Login, d.CSRF = id.Login, s.Auth.CSRF(r)
		}
		page, code, err := h(w, r.WithContext(callerContext(r.Context(), c)), c, d)
		switch {
		case errors.Is(err, auth.ErrInvalidToken):
			s.uiSignIn(w, r, err, d)
			return
		case errors.Is(err, forge.ErrNotFound):
			page, code, d.Title, d.Error = "error", 404, "Not found", "There's no such task, or you can't see it."
		case err != nil:
			s.Log.Warn("ui", "path", r.URL.Path, "err", err)
			page, code, d.Title, d.Error = "error", 502, "Something went wrong", "devbot couldn't read this from the forge or Redis. It'll retry."
		}
		s.render(w, page, code, d)
	}
}

func (s *Server) uiSignIn(w http.ResponseWriter, r *http.Request, err error, d *uiData) {
	if !errors.Is(err, auth.ErrInvalidToken) {
		s.Log.Warn("ui: checking a session", "err", err)
		d.Title, d.Error = "Something went wrong", "devbot couldn't check your sign-in. Try again."
		s.render(w, "error", 503, d)
		return
	}
	s.E.Audit.Log(r.Context(), audit.Record{Event: "authz.refused", Outcome: audit.Refused, Detail: map[string]any{"action": "ui " + r.URL.Path, "reason": "not signed in"}})
	d.Title, d.Page = "Sign in", "signin"
	s.render(w, "signin", 401, d)
}

func (s *Server) render(w http.ResponseWriter, page string, code int, d *uiData) {
	var b bytes.Buffer
	if err := uiPages[page].ExecuteTemplate(&b, "layout", d); err != nil {
		s.Log.Error("ui: rendering", "page", page, "err", err)
		http.Error(w, "couldn't render the page", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	w.Write(b.Bytes())
}

func (s *Server) uiJobs(w http.ResponseWriter, r *http.Request, c core.Caller, d *uiData) (string, int, error) {
	d.Page, d.Title, d.Refresh = "jobs", "Running now", 5
	var err error
	d.Jobs, err = s.jobs(r.Context(), c, "")
	return "jobs", 200, err
}

func (s *Server) uiTasks(w http.ResponseWriter, r *http.Request, c core.Caller, d *uiData) (string, int, error) {
	d.Page, d.Title, d.Refresh = "tasks", "Tasks", 30
	q := r.URL.Query()
	d.Repo, d.State, d.States = q.Get("repo"), q.Get("state"), uiStates
	if d.State != "" && !slices.Contains(uiStates, d.State) {
		d.State = ""
	}
	// Every repo with a task, for the filter, so only the state narrows
	// the forge search.
	ts, err := s.E.ListTasksDetailed(r.Context(), c, "", d.State, "")
	if err != nil {
		return "tasks", 200, err
	}
	repos := map[string]bool{}
	byState := map[string][]core.TaskStatus{}
	for _, t := range ts {
		repos[t.Repo] = true
		if d.Repo == "" || t.Repo == d.Repo {
			byState[t.State] = append(byState[t.State], t)
		}
	}
	// Parent tasks head the page, unfiltered by state: each is a whole
	// change, its children in the groups below.
	if d.State == "" {
		ps, err := s.E.ListParents(r.Context(), c, "", "")
		if err != nil {
			return "tasks", 200, err
		}
		for _, p := range ps {
			repos[p.Repo] = true
			if d.Repo == "" || p.Repo == d.Repo {
				d.Parents = append(d.Parents, p)
			}
		}
	}
	d.Repos = sortedKeys(repos)
	for _, st := range uiStates {
		g := byState[st]
		if len(g) == 0 {
			continue
		}
		// Longest in the state first.
		sort.SliceStable(g, func(i, j int) bool { return since(g[i]).Before(since(g[j])) })
		d.Groups = append(d.Groups, taskGroup{State: st, Tasks: g})
	}
	return "tasks", 200, nil
}

func since(t core.TaskStatus) time.Time {
	if t.StateSince == nil {
		return time.Time{}
	}
	return *t.StateSince
}

func (s *Server) uiTask(w http.ResponseWriter, r *http.Request, c core.Caller, d *uiData) (string, int, error) {
	d.Page, d.Refresh = "tasks", 30
	ref := r.URL.Query().Get("ref")
	if _, err := forge.ParseItem(ref); err != nil {
		return "", 0, fmt.Errorf("%w: %v", forge.ErrNotFound, err)
	}
	var err error
	d.Task, err = s.E.TaskDetail(r.Context(), c, ref)
	d.Title = d.Task.Title
	return "task", 200, err
}

func (s *Server) uiEvents(w http.ResponseWriter, r *http.Request, c core.Caller, d *uiData) (string, int, error) {
	d.Page, d.Title, d.Refresh = "events", "Events", 15
	q := r.URL.Query()
	d.Repo, d.Type, d.Origin, d.Types = q.Get("repo"), q.Get("type"), q.Get("origin"), core.EventTypes
	before := q.Get("before")
	if before != "" {
		d.Refresh = 0 // an older page doesn't change
	}
	var err error
	d.Events, err = s.recentEvents(r.Context(), c, before, newEventFilter(d.Repo, d.Origin, d.Type), 100)
	if err == nil && d.Events.Cursor != "" && len(d.Events.Events) == 100 {
		v := q
		v.Set("before", d.Events.Cursor)
		d.Older = "events?" + v.Encode()
	}
	return "events", 200, err
}

var uiFuncs = template.FuncMap{
	// age is how long ago t was, roughly: "45s", "3m", "2h 5m", "4d 1h".
	"age": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return roughly(time.Since(t))
	},
	// until is how long until t, or "now" once it has passed.
	"until": func(t time.Time) string {
		if d := time.Until(t); d > 0 {
			return "in " + roughly(d)
		}
		return "now"
	},
	"iso": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"clock": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format("2006-01-02 15:04 UTC")
	},
	"deref": func(t *time.Time) time.Time {
		if t == nil {
			return time.Time{}
		}
		return *t
	},
	// num is an issue ref's "#12".
	"num": func(ref string) string {
		if _, n, ok := strings.Cut(ref, "#"); ok {
			return "#" + n
		}
		return ref
	},
	"repoOf": func(ref string) string {
		r, _, _ := strings.Cut(ref, "#")
		return r
	},
	// repo drops the provider from a repo or issue ref when it's the only
	// one: "acme/app", where several need "gitea:acme/app".
	"repo": func(only, ref string) string {
		if only != "" {
			return strings.TrimPrefix(ref, only+":")
		}
		return ref
	},
	// pod is a holder's name from its kind on: the Job and pod suffixes of
	// "devbot-implement-t6cw4-b6dht", which are what tell runs apart.
	"pod": func(kind, holder string) string {
		if _, rest, ok := strings.Cut(holder, "-"+kind+"-"); ok && rest != "" {
			return rest
		}
		return holder
	},
	"attention": func(state string) bool {
		return state == "needs-human" || state == "needs-approval" || state == "needs-info"
	},
	"short": func(s string) string { return strings.TrimPrefix(s, "task.") },
}

func roughly(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}

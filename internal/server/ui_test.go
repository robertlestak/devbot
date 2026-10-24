package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/fake"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// uiSeed is a little of everything the UI shows, on two repos: in oauth
// tests alice can read app but not secret.
type uiSeed struct {
	working, stuck, ready, hidden forge.ItemRef
	pr                            int
}

func seedUI(t *testing.T, f *fake.Forge, e *core.Engine) uiSeed {
	t.Helper()
	ctx := context.Background()
	var s uiSeed
	s.working = f.NewIssue(appRepo, "alice", "Fix the <script>alert(1)</script> bug", "", core.LWorking)
	f.AddComment(s.working, f.Impl, core.RenderMeta(core.Meta{Origin: "chat:7", Requester: "alice"}))
	it, _ := f.GetItem(ctx, s.working)
	e.Emit(ctx, it, "task.filed", core.LReady, "filed it", "")
	e.Emit(ctx, it, "task.started", core.LWorking, "Working on it", "")
	pr, _ := f.OpenPR(ctx, appRepo, fmt.Sprintf("devbot/%d-fix", s.working.Number), "main", "Fix", "")
	s.pr = pr.Number
	f.AddComment(s.working, f.Impl, "Opened PR.\n\n"+core.RenderStatus(core.Status{Kind: "pr", PR: pr.Number}))
	s.stuck = f.NewIssue(appRepo, "alice", "Stuck task", "", core.LNeedsHuman)
	f.AddComment(s.stuck, f.Impl, "Giving up after 3 failed attempts.\n\nThe last one ran out of time.\n\n"+core.RenderStatus(core.Status{Kind: "needs-human"}))
	s.ready = f.NewIssue(appRepo, "alice", "Waiting task", "", core.LReady)
	s.hidden = f.NewIssue(secretRepo, "carol", "Hidden secret task", "", core.LInReview)

	hid, _ := f.GetItem(ctx, s.hidden)
	e.Emit(ctx, hid, "task.filed", core.LReady, "hidden filed", "")

	e.Q.Acquire(ctx, queue.KindImplement, s.working.String(), "pod-a", time.Minute)
	e.Q.Describe(ctx, queue.KindImplement, s.working.String(), "pod-a", queue.LeaseDetail{Mode: "fix", PR: pr.Number, Attempt: 2, MaxAttempts: 3})
	e.Q.Acquire(ctx, queue.KindReview, s.hidden.String(), "pod-secret", time.Minute)
	e.Q.Enqueue(ctx, queue.Job{Kind: queue.KindImplement, Ref: s.ready.String(), Mode: "new"})
	e.Q.Enqueue(ctx, queue.Job{Kind: queue.KindImplement, Ref: s.hidden.String() + "0", Mode: "new"})
	e.Q.Delay(ctx, queue.Job{Kind: queue.KindReview, Ref: s.working.String(), PR: pr.Number, RateLimitN: 2}, time.Now().Add(30*time.Minute), "")
	e.Q.Delay(ctx, queue.Job{Kind: queue.KindReview, Ref: secretRepo.String() + "#99", RateLimitN: 1}, time.Now().Add(time.Hour), "")
	return s
}

func newUIServer(t *testing.T, anon string) (*Server, *fake.Forge, http.Handler) {
	s, f := newServer(t, anon)
	f.AddRepo(secretRepo, "version: 1\nenabled: true\n")
	s.Cfg.UI.Enabled = true
	s.Cfg.UI.GrafanaURL = "https://grafana.example.com/d/devbot"
	return s, f, s.Handler()
}

func get(t *testing.T, h http.Handler, path string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w, w.Body.String()
}

func TestUIDisabled(t *testing.T) {
	s, _ := newServer(t, "anonymous")
	h := s.Handler()
	for _, p := range []string{"/", "/ui", "/ui/", "/ui/tasks", "/ui/static/app.js"} {
		if w, _ := get(t, h, p); w.Code != 404 {
			t.Errorf("%s with the UI off: %d", p, w.Code)
		}
	}
}

func TestUIRoot(t *testing.T) {
	s, f, h := newUIServer(t, "anonymous")
	seedUI(t, f, s.E)
	w, _ := get(t, h, "/")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "ui/" {
		t.Fatalf("GET / sends a relative redirect to the UI: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w.Header().Get("Content-Security-Policy") != uiCSP {
		t.Fatalf("the root redirect carries the UI headers: %v", w.Header())
	}
	if w, body := get(t, h, "/ui/"); w.Code != 200 || !strings.Contains(body, "Running") {
		t.Fatalf("where it leads: %d %s", w.Code, body)
	}
	// Everything else served beside the UI is untouched by the root route.
	if w, body := get(t, h, "/healthz"); w.Code != 200 || body != "ok" {
		t.Fatalf("/healthz: %d %s", w.Code, body)
	}
	if w, body := get(t, h, "/api/jobs"); w.Code != 200 || !strings.HasPrefix(body, "{") {
		t.Fatalf("/api/jobs: %d %s", w.Code, body)
	}
	if w, _ := get(t, h, "/nope"); w.Code != 404 {
		t.Fatalf("only the exact root redirects: %d", w.Code)
	}
	if ping := rpc(t, h, "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); ping["error"] != nil {
		t.Fatalf("POST /mcp: %v", ping)
	}
}

func TestUIJobsAPI(t *testing.T) {
	s, f, h := newUIServer(t, "anonymous")
	seed := seedUI(t, f, s.E)
	w, body := get(t, h, "/api/jobs")
	if w.Code != 200 {
		t.Fatalf("/api/jobs: %d %s", w.Code, body)
	}
	var js Jobs
	json.Unmarshal([]byte(body), &js)
	if len(js.Running) != 2 || len(js.Queued) != 2 || len(js.Delayed) != 2 {
		t.Fatalf("auth mode none shows every job: %s", body)
	}
	var run JobView
	for _, j := range js.Running {
		if j.Task == seed.working.String() {
			run = j
		}
	}
	want := JobView{Kind: "implement", Task: seed.working.String(), Repo: appRepo.String(), Title: "Fix the <script>alert(1)</script> bug",
		URL: f.IssueURL(seed.working), PR: seed.pr, PRURL: f.PRURL(appRepo, seed.pr), Mode: "fix", Attempt: 2, MaxAttempts: 3, Holder: "pod-a"}
	if run.Started.IsZero() || run.Heartbeat.IsZero() {
		t.Fatalf("a running job has its start and heartbeat: %+v", run)
	}
	run.Started, run.Heartbeat = time.Time{}, time.Time{}
	if run != want {
		t.Fatalf("running job:\n got %+v\nwant %+v", run, want)
	}
	if js.Queued[0].Task != seed.ready.String() && js.Queued[1].Task != seed.ready.String() {
		t.Fatalf("queued: %+v", js.Queued)
	}
	if d := js.Delayed[0]; d.Task != seed.working.String() || d.ReleaseAt.IsZero() || d.Retries != 2 || d.PRURL == "" {
		t.Fatalf("delayed, soonest first, with its release time: %+v", d)
	}
	if _, body := get(t, h, "/api/jobs?repo="+url.QueryEscape(secretRepo.String())); strings.Contains(body, seed.working.String()) {
		t.Fatalf("repo filter: %s", body)
	}
}

func TestUINoneMode(t *testing.T) {
	// In mode none the UI reads as the anonymous principal, like the
	// connectors: every repo.
	s, f, h := newUIServer(t, "anonymous")
	seed := seedUI(t, f, s.E)

	w, _ := get(t, h, "/ui")
	if w.Code != 302 || w.Header().Get("Location") != "ui/" {
		t.Fatalf("/ui should redirect relatively: %d %s", w.Code, w.Header().Get("Location"))
	}
	w, body := get(t, h, "/ui/")
	if w.Code != 200 {
		t.Fatalf("/ui/: %d %s", w.Code, body)
	}
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") {
		t.Fatalf("CSP: %q", csp)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("headers: %v", w.Header())
	}
	for _, want := range []string{
		"&lt;script&gt;alert(1)&lt;/script&gt; bug", `href="` + f.IssueURL(seed.working), `href="` + f.PRURL(appRepo, seed.pr) + `"`,
		"attempt 2 of 3", "pod-a", "pod-secret", "Hidden secret task", `href="` + f.IssueURL(seed.ready), "Delayed", "in 29m",
		`href="https://grafana.example.com/d/devbot"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("running page lacks %q", want)
		}
	}
	if strings.Contains(body, "<script>alert") {
		t.Fatal("a forge-supplied title must be escaped")
	}
	assertRelative(t, body)

	// Tasks: attention states first, with the PR, origin and how long.
	_, body = get(t, h, "/ui/tasks")
	order := []string{">needs-human ", ">working ", ">in-review ", ">ready "}
	last := -1
	for _, o := range order {
		i := strings.Index(body, o)
		if i < 0 || i < last {
			t.Fatalf("tasks should be grouped needs-human, working, in-review, ready; %q at %d: %s", o, i, body)
		}
		last = i
	}
	for _, want := range []string{"chat:7", "Giving up after 3 failed attempts.", `href="` + f.PRURL(appRepo, seed.pr) + `"`, "#" + fmt.Sprint(seed.pr)} {
		if !strings.Contains(body, want) {
			t.Errorf("tasks page lacks %q", want)
		}
	}
	if _, body = get(t, h, "/ui/tasks?state=working"); strings.Contains(body, "Stuck task") || !strings.Contains(body, "Fix the") {
		t.Fatalf("state filter: %s", body)
	}
	if _, body = get(t, h, "/ui/tasks?repo="+url.QueryEscape(secretRepo.String())); strings.Contains(body, "Stuck task") || !strings.Contains(body, "Hidden secret task") {
		t.Fatalf("repo filter: %s", body)
	}

	// Task detail: the timeline in order, with the reasons.
	_, body = get(t, h, "/ui/task?ref="+url.QueryEscape(seed.stuck.String()))
	if !strings.Contains(body, "needs-human") || !strings.Contains(body, "The last one ran out of time.") {
		t.Fatalf("needs-human with its reason: %s", body)
	}
	_, body = get(t, h, "/ui/task?ref="+url.QueryEscape(seed.working.String()))
	filed, started, opened := strings.Index(body, "<b>filed</b>"), strings.Index(body, "<b>started</b>"), strings.Index(body, "<b>pr</b>")
	if filed < 0 || started < filed || opened < started {
		t.Fatalf("timeline order filed %d, started %d, pr %d: %s", filed, started, opened, body)
	}
	if !strings.Contains(body, `href="`+f.BranchURL(appRepo, fmt.Sprintf("devbot/%d-fix", seed.working.Number))+`"`) {
		t.Fatalf("branch link: %s", body)
	}
	if w, _ := get(t, h, "/ui/task?ref=nonsense"); w.Code != 404 {
		t.Fatalf("a bad ref: %d", w.Code)
	}

	// Events, newest first, filtered.
	_, body = get(t, h, "/ui/events")
	if strings.Index(body, "hidden filed") > strings.Index(body, "filed it") {
		t.Fatalf("events newest first: %s", body)
	}
	_, body = get(t, h, "/ui/events?type=task.filed&repo="+url.QueryEscape(appRepo.String()))
	if !strings.Contains(body, "filed it") || strings.Contains(body, "hidden filed") || strings.Contains(body, "Working on it") {
		t.Fatalf("events filtered by type and repo: %s", body)
	}
	if _, body = get(t, h, "/ui/events?origin=chat:"); !strings.Contains(body, "filed it") || strings.Contains(body, "hidden filed") {
		t.Fatalf("events filtered by origin: %s", body)
	}

	// The assets are embedded, under the same headers.
	w, body = get(t, h, "/ui/static/app.js")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") || !strings.Contains(body, "data-refresh") ||
		w.Header().Get("Content-Security-Policy") != uiCSP {
		t.Fatalf("app.js: %d %v", w.Code, w.Header())
	}
	if w, _ = get(t, h, "/ui/static/app.css"); w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("app.css: %d %v", w.Code, w.Header())
	}
	// No sign-in in mode none.
	if w, _ = get(t, h, "/ui/login"); w.Code != 404 {
		t.Fatalf("/ui/login in mode none: %d", w.Code)
	}
}

// Without an anonymous principal, mode none's UI is closed like its
// connectors: no page, nor the API the UI's jobs page reads, shows a task.
func TestUIClosedWithoutPrincipal(t *testing.T) {
	s, f, h := newUIServer(t, "")
	seed := seedUI(t, f, s.E)
	for _, p := range []string{"/ui/", "/ui/tasks", "/ui/task?ref=" + url.QueryEscape(seed.hidden.String()), "/ui/events", "/api/jobs", "/api/tasks", "/api/events"} {
		w, body := get(t, h, p)
		if w.Code != 401 && w.Code != 404 {
			t.Errorf("%s with no anonymous_principal: %d", p, w.Code)
		}
		for _, leak := range []string{"Hidden secret task", "Stuck task", "pod-a", "hidden filed", seed.working.String()} {
			if strings.Contains(body, leak) {
				t.Errorf("%s shows %q: %s", p, leak, body)
			}
		}
	}
}

var absURL = regexp.MustCompile(`(href|src|action)="/`)

// assertRelative checks a page has no root-relative URL, which would miss
// a proxy's path prefix.
func assertRelative(t *testing.T, body string) {
	t.Helper()
	if m := absURL.FindString(body); m != "" {
		t.Fatalf("a root-relative URL breaks behind a path prefix: %s", m)
	}
	if strings.Contains(body, "<script>") || strings.Contains(body, "style=") {
		t.Fatal("no inline script or style")
	}
}

func TestUIOAuth(t *testing.T) {
	v := newOAuthEnvWith(t, func(c *config.Config) { c.UI.Enabled = true })
	seed := seedUI(t, v.f, v.s.E)

	// The root leads to the UI here too, beside the OAuth discovery routes.
	resp, _ := v.get(v.url + "/")
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "ui/" {
		t.Fatalf("GET /: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, body := v.get(v.url + "/.well-known/oauth-authorization-server"); resp.StatusCode != 200 || !strings.Contains(body, "authorization_endpoint") {
		t.Fatalf("authorization server metadata: %d %s", resp.StatusCode, body)
	}

	// Signed out, every page is the sign-in page, and shows nothing.
	resp, body := v.get(v.url + "/ui/")
	if resp.StatusCode != 401 || !strings.Contains(body, `href="login"`) || strings.Contains(body, "Fix the") {
		t.Fatalf("signed out: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Content-Security-Policy") != uiCSP {
		t.Fatalf("the sign-in page carries the CSP too: %v", resp.Header)
	}

	// Sign in through the broker and the forge.
	resp, _ = v.get(v.url + "/ui/login")
	if resp.StatusCode != 302 || !strings.HasPrefix(resp.Header.Get("Location"), "https://fake/login/oauth/authorize?") {
		t.Fatalf("login should go to the forge: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	cb, err := v.f.SignIn(resp.Header.Get("Location"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	resp, _ = v.get(cb)
	if resp.StatusCode != 302 || resp.Header.Get("Location") != v.url+"/ui/" {
		t.Fatalf("callback should land on the UI: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "devbot_session" {
			session = c
		}
	}
	if session == nil || !session.HttpOnly || session.SameSite != http.SameSiteLaxMode || session.Path != "/ui/" {
		t.Fatalf("session cookie: %+v", session)
	}

	// Every view shows app, never secret: not its jobs, tasks or events.
	for _, path := range []string{"/ui/", "/ui/tasks", "/ui/events", "/ui/tasks?repo=" + url.QueryEscape(secretRepo.String())} {
		resp, body = v.get(v.url + path)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, body)
		}
		for _, leak := range []string{"secret", "Hidden", "pod-secret", "#99"} {
			if strings.Contains(body, leak) {
				t.Errorf("%s shows %q from a repo alice can't read", path, leak)
			}
		}
		if !strings.Contains(path, "secret") && !strings.Contains(body, "@alice") {
			t.Errorf("%s doesn't say who's signed in", path)
		}
	}
	if _, body = v.get(v.url + "/ui/"); !strings.Contains(body, "pod-a") || !strings.Contains(body, "attempt 2 of 3") {
		t.Fatalf("alice sees app's running job: %s", body)
	}
	if resp, body = v.get(v.url + "/ui/task?ref=" + url.QueryEscape(seed.hidden.String())); resp.StatusCode != 404 || strings.Contains(body, "Hidden") {
		t.Fatalf("secret's task detail: %d %s", resp.StatusCode, body)
	}
	if resp, _ = v.get(v.url + "/ui/task?ref=" + url.QueryEscape(seed.working.String())); resp.StatusCode != 200 {
		t.Fatalf("app's task detail: %d", resp.StatusCode)
	}

	// The session opens the UI only: it isn't a bearer token for the API,
	// and the API doesn't take the cookie.
	req, _ := http.NewRequest("GET", v.url+"/api/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+session.Value)
	if resp, _ := v.do(req); resp.StatusCode != 401 {
		t.Fatalf("a session as a bearer token: %d", resp.StatusCode)
	}
	if resp, _ := v.get(v.url + "/api/jobs"); resp.StatusCode != 401 {
		t.Fatalf("the API with only the cookie: %d", resp.StatusCode)
	}
	v.assertNoLeak(session.Value)

	// The API, signed in as alice, filters jobs the same way.
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none"}`)
	tok := v.login(cid, "http://127.0.0.1/callback", "alice")
	req, _ = http.NewRequest("GET", v.url+"/api/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+tok["access_token"].(string))
	if resp, body := v.do(req); resp.StatusCode != 200 || strings.Contains(body, "secret") || !strings.Contains(body, seed.working.String()) {
		t.Fatalf("/api/jobs as alice: %d %s", resp.StatusCode, body)
	}

	// Signing out needs the page's CSRF token and a same-origin post.
	_, body = v.get(v.url + "/ui/")
	m := regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no sign-out form: %s", body)
	}
	logout := func(csrf, origin string) *http.Response {
		req, _ := http.NewRequest("POST", v.url+"/ui/logout", strings.NewReader(url.Values{"csrf": {csrf}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", origin)
		resp, _ := v.do(req)
		return resp
	}
	if r := logout("nope", v.url); r.StatusCode != 403 {
		t.Fatalf("logout with a bad CSRF token: %d", r.StatusCode)
	}
	if r := logout(m[1], "https://evil.example"); r.StatusCode != 403 {
		t.Fatalf("logout from another origin: %d", r.StatusCode)
	}
	if resp, _ = v.get(v.url + "/ui/"); resp.StatusCode != 200 {
		t.Fatal("a refused sign-out leaves the session alone")
	}
	if r := logout(m[1], v.url); r.StatusCode != 303 {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	// The grant is gone, so the old cookie is dead even if replayed.
	req, _ = http.NewRequest("GET", v.url+"/ui/", nil)
	req.AddCookie(&http.Cookie{Name: "devbot_session", Value: session.Value})
	if resp, _ := v.do(req); resp.StatusCode != 401 {
		t.Fatalf("after sign-out: %d", resp.StatusCode)
	}
	// A session ends by itself, too.
	v.hc.Jar, _ = cookiejar.New(nil)
	resp, _ = v.get(v.url + "/ui/login")
	cb, _ = v.f.SignIn(resp.Header.Get("Location"), "alice")
	v.get(cb)
	if resp, _ = v.get(v.url + "/ui/"); resp.StatusCode != 200 {
		t.Fatalf("signed in again: %d", resp.StatusCode)
	}
	v.advance(13 * time.Hour)
	if resp, _ = v.get(v.url + "/ui/"); resp.StatusCode != 401 {
		t.Fatalf("after 13 hours: %d", resp.StatusCode)
	}
}

func TestUITableLayout(t *testing.T) {
	s, f, h := newUIServer(t, "anonymous")
	seed := seedUI(t, f, s.E)
	ctx := context.Background()
	long := strings.Repeat("rebase onto main and resolve the conflicts ", 8)
	s.E.Q.Acquire(ctx, queue.KindImplement, seed.stuck.String(), "devbot-implement-t6cw4-b6dht", time.Minute)
	s.E.Q.Describe(ctx, queue.KindImplement, seed.stuck.String(), "devbot-implement-t6cw4-b6dht", queue.LeaseDetail{Mode: "fix", Reason: long})

	// One provider: owner/name, with the full ref on hover.
	_, body := get(t, h, "/ui/")
	for _, want := range []string{
		`title="` + appRepo.String() + `">acme/app</td>`,
		`title="held by devbot-implement-t6cw4-b6dht">on <code>t6cw4-b6dht</code>`,
		`<div class="sub clamp" title="` + long[:100],
		`title="started `, " ago</time>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("running page lacks %q", want)
		}
	}
	if strings.Contains(body, ">"+appRepo.String()+"<") {
		t.Error("a single-provider install shows the provider")
	}
	// One time column for a running job, the heartbeat under the start.
	if head := body[strings.Index(body, "<thead>"):strings.Index(body, "</thead>")]; strings.Contains(head, "Elapsed") || strings.Count(head, "<th>") != 6 {
		t.Errorf("running table head: %s", head)
	}
	_, body = get(t, h, "/ui/tasks")
	if !strings.Contains(body, `title="`+appRepo.String()+`">acme/app</td>`) || !strings.Contains(body, "for alice, via chat:7") ||
		!strings.Contains(body, `<div class="clamp" title="Giving up after 3 failed attempts.`) {
		t.Errorf("tasks page: %s", body)
	}
	_, body = get(t, h, "/ui/events")
	if !strings.Contains(body, `title="`+seed.working.String()+`">`+fmt.Sprintf("acme/app#%d", seed.working.Number)+`</a>`) {
		t.Errorf("events page: %s", body)
	}

	// With several providers each repo needs its provider to be told apart.
	s.E.Forges["other"] = fake.New()
	for _, p := range []string{"/ui/", "/ui/tasks"} {
		if _, body = get(t, h, p); !strings.Contains(body, ">"+appRepo.String()+"</td>") {
			t.Errorf("%s with two providers shows the full ref: %s", p, body)
		}
	}
}

// A task whose base isn't the default branch says so on every page, and
// retarget is a connector call like retry.
func TestUIShowsATasksBase(t *testing.T) {
	s, f, h := newUIServer(t, "anonymous")
	ctx := context.Background()
	f.AddBranch(appRepo, "release/x")
	f.AddBranch(appRepo, "release/y")
	st, err := s.E.FileTask(ctx, core.FileRequest{Repo: appRepo.String(), Title: "Batched", AcceptanceCriteria: "- y", Base: "release/x"}, core.Caller{Principal: "client/a"})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := s.E.FileTask(ctx, core.FileRequest{Repo: appRepo.String(), Title: "Plain", AcceptanceCriteria: "- y"}, core.Caller{Principal: "client/a"})
	for path, want := range map[string]string{
		"/ui/tasks":               "into release/x",
		"/ui/task?ref=" + st.Task: "<code>release/x</code>",
		"/ui/events":              "into release/x",
	} {
		if w, body := get(t, h, path); w.Code != 200 || !strings.Contains(body, want) {
			t.Errorf("%s should show %q: %d %s", path, want, w.Code, body)
		}
	}
	if _, body := get(t, h, "/ui/task?ref="+other.Task); strings.Contains(body, "<dt>Base</dt>") {
		t.Error("a task on the default branch shows no base")
	}

	r := httptest.NewRequest("POST", "/api/task/retarget", strings.NewReader(`{"ref":"`+st.Task+`","base":"release/y"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"base":"release/y"`) {
		t.Fatalf("retarget: %d %s", w.Code, w.Body.String())
	}
	r = httptest.NewRequest("POST", "/api/task/retarget", strings.NewReader(`{"ref":"`+st.Task+`","base":"release/z"}`))
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `no branch "release/z"`) {
		t.Fatalf("a missing base is a bad request: %d %s", w.Code, w.Body.String())
	}
	if _, body := get(t, h, "/ui/task?ref="+st.Task); !strings.Contains(body, "<code>release/y</code>") {
		t.Errorf("the task page should show the new base: %s", body)
	}
}

// A parent task is filed, and its children filed and listed under it,
// through MCP and HTTP alike; task_status on it rolls its children up, and
// the web UI shows a parent with its children and a child with its parent.
func TestParentTasksThroughTheConnectorsAndTheUI(t *testing.T) {
	s, f, h := newUIServer(t, "anonymous")
	lib := forge.RepoRef{Provider: "fake", Owner: "acme", Name: "lib"}
	f.AddRepo(lib, "version: 1\nenabled: true\n")
	texts, isErr, _ := callTool(t, h, "file_project", map[string]string{"repo": appRepo.String(), "title": "Rename parse", "body": "across lib and app", "origin": "chat:1"})
	if isErr {
		t.Fatalf("file_project: %v", texts)
	}
	var parent core.TaskStatus
	json.Unmarshal([]byte(texts[0]), &parent)
	if parent.Kind != core.KindParent || parent.State != core.ParentInProgress {
		t.Fatalf("file_project: %+v", parent)
	}
	var other core.TaskStatus
	if w := call(h, "POST", "/api/projects", `{"repo":"`+appRepo.String()+`","title":"Another"}`); json.Unmarshal(w.Body.Bytes(), &other) != nil || w.Code != 200 || other.Kind != core.KindParent {
		t.Fatalf("POST /api/projects: %d %s", w.Code, w.Body.String())
	}
	texts, isErr, _ = callTool(t, h, "file_task", map[string]string{"repo": lib.String(), "title": "New parse API", "acceptance_criteria": "- parse.go", "parent": parent.Task})
	if isErr {
		t.Fatalf("file_task with a parent: %v", texts)
	}
	var child core.TaskStatus
	json.Unmarshal([]byte(texts[0]), &child)
	if child.Parent != parent.Task {
		t.Fatalf("the child shows its parent: %+v", child)
	}
	if _, err := s.E.FileTask(context.Background(), core.FileRequest{Repo: appRepo.String(), Title: "Unrelated", AcceptanceCriteria: "- y"}, core.Caller{Principal: "client/a"}); err != nil {
		t.Fatal(err)
	}
	w := call(h, "GET", "/api/tasks?parent="+url.QueryEscape(parent.Task), "")
	var kids []core.TaskStatus
	if json.Unmarshal(w.Body.Bytes(), &kids); w.Code != 200 || len(kids) != 1 || kids[0].Task != child.Task {
		t.Fatalf("GET /api/tasks?parent: %d %s", w.Code, w.Body.String())
	}
	texts, _, _ = callTool(t, h, "list_tasks", map[string]string{"parent": parent.Task})
	if json.Unmarshal([]byte(texts[0]), &kids); len(kids) != 1 || kids[0].Task != child.Task {
		t.Fatalf("list_tasks with a parent: %v", texts)
	}
	texts, _, _ = callTool(t, h, "task_status", map[string]string{"ref": parent.Task})
	json.Unmarshal([]byte(texts[0]), &parent)
	if len(parent.Children) != 1 || parent.Children[0].Task != child.Task || parent.Children[0].State != "ready" {
		t.Fatalf("task_status on a parent: %+v", parent)
	}
	for path, want := range map[string][]string{
		"/ui/tasks":                   {"parent tasks", "Rename parse", child.Task},
		"/ui/task?ref=" + parent.Task: {"<h2>Child tasks</h2>", "New parse API", "in-progress"},
		"/ui/task?ref=" + child.Task:  {"<dt>Parent task</dt>", "<code>" + parent.Task + "</code>"},
	} {
		w, body := get(t, h, path)
		for _, s := range want {
			if w.Code != 200 || !strings.Contains(body, s) {
				t.Errorf("%s should show %q: %d %s", path, s, w.Code, body)
			}
		}
	}
}

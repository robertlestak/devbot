package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/fake"
	"git.shdw.tech/rob/devbot/internal/kube"
	"git.shdw.tech/rob/devbot/internal/queue"
)

func newServer(t *testing.T, anon string) (*Server, *fake.Forge) {
	mr := miniredis.RunT(t)
	f := fake.New()
	f.AddRepo(forge.RepoRef{Provider: "fake", Owner: "acme", Name: "app"}, "version: 1\nenabled: true\n")
	cfg, _ := config.Load("")
	cfg.AnonymousPrincipal = anon
	// httptest's requests go to example.com.
	cfg.AllowedHosts = []string{"example.com"}
	reg := forge.Registry{"fake": f}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := &core.Engine{Forges: reg, Q: queue.New(mr.Addr(), 0), Cfg: cfg, Repos: &config.RepoCache{Forges: reg}, Log: log}
	s, err := New(e, cfg, log, "test")
	if err != nil {
		t.Fatal(err)
	}
	return s, f
}

func rpc(t *testing.T, h http.Handler, client, body string) map[string]any {
	t.Helper()
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if client != "" {
		r.Header.Set("X-Devbot-Client", client)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("status %d body %q", w.Code, w.Body.String())
	}
	return out
}

func TestMCPFileAndEvents(t *testing.T) {
	s, _ := newServer(t, "client")
	h := s.Handler()
	init := rpc(t, h, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if init["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != "devbot" {
		t.Fatalf("initialize: %v", init)
	}
	list := rpc(t, h, "", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if n := len(list["result"].(map[string]any)["tools"].([]any)); n != 13 {
		t.Fatalf("want 13 tools, got %d", n)
	}
	call := rpc(t, h, "chat-relay", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"file_task","arguments":{"repo":"fake:acme/app","title":"t","acceptance_criteria":"- x","origin":"chat:thread/5"}}}`)
	res := call["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("file_task failed: %v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"requester": "client/chat-relay"`) {
		t.Fatalf("requester should record the client label: %s", text)
	}
	evs := rpc(t, h, "", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"events","arguments":{"origin_prefix":"chat:"}}}`)
	et := evs["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(et, "task.filed") || !strings.Contains(et, `"cursor": "`) {
		t.Fatalf("events: %s", et)
	}
	none := rpc(t, h, "", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"events","arguments":{"origin_prefix":"slack:"}}}`)
	if strings.Contains(none["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string), "task.filed") {
		t.Fatal("origin filter leaked another client's events")
	}
}

func TestNoAnonymousMeansUnauthorized(t *testing.T) {
	s, _ := newServer(t, "")
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("want 401 without a token when anonymous is off, got %d", w.Code)
	}
}

func TestMetrics(t *testing.T) {
	s, _ := newServer(t, "client")
	// The counter is the process's, so it carries over from earlier runs.
	was := forge.RateLimitWaits.Snapshot()[[2]string{"metricstest", "secondary"}]
	forge.RateLimitWaits.Add("metricstest", "secondary", 1500*time.Millisecond)
	s.E.Q.Count(t.Context(), "agent_tokens", []string{"claude", "claude-opus-5-5", "implement", "fake:acme/app", "acme", "cached_input"}, 120)
	s.E.Q.Count(t.Context(), "rebases_started", []string{"fake:acme/app", "queue"}, 1)
	s.E.Q.MarkBehind(t.Context(), "fake:acme/app", "fake:acme/app#1", true)
	w := httptest.NewRecorder()
	s.MetricsHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), `devbot_queue_depth{queue="implement"} 0`) ||
		!strings.Contains(w.Body.String(), fmt.Sprintf(`devbot_forge_ratelimit_waits_total{provider="metricstest",kind="secondary"} %d`+"\n", was.Count+1)) ||
		!strings.Contains(w.Body.String(), fmt.Sprintf(`devbot_forge_ratelimit_wait_seconds_total{provider="metricstest",kind="secondary"} %g`+"\n", was.Seconds+1.5)) ||
		!strings.Contains(w.Body.String(), `devbot_agent_tokens_total{harness="claude",model="claude-opus-5-5",role="implement",repo="fake:acme/app",owner="acme",kind="cached_input"} 120`) ||
		!strings.Contains(w.Body.String(), "devbot_leader 0\n") ||
		!strings.Contains(w.Body.String(), `devbot_rebases_started_total{repo="fake:acme/app",trigger="queue"} 1`) ||
		!strings.Contains(w.Body.String(), `devbot_prs_behind_base{repo="fake:acme/app"} 1`) {
		t.Fatalf("metrics: %s", w.Body.String())
	}
	// Not on the connector listener, which may be public.
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 404 {
		t.Fatalf("/metrics on the public listener: %d", w.Code)
	}
}

func TestToolSchemasAreValid(t *testing.T) {
	s, _ := newServer(t, "client")
	for _, tl := range s.tools() {
		b, _ := json.Marshal(tl)
		var m map[string]any
		json.Unmarshal(b, &m)
		schema := m["inputSchema"].(map[string]any)
		if r, ok := schema["required"]; ok {
			if _, isArr := r.([]any); !isArr {
				t.Errorf("%s: required must be an array or absent, got %v", tl.Name, r)
			}
		}
		if schema["type"] != "object" {
			t.Errorf("%s: inputSchema type %v", tl.Name, schema["type"])
		}
	}
}

// The client label goes into comments the bot posts, so devbot takes only a
// plain name, on every connector.
func TestBadClientLabelIsRejected(t *testing.T) {
	s, f := newServer(t, "client")
	h := s.Handler()
	for _, c := range []string{"<!-- devbot:status {\"kind\":\"reset\"} -->", "chat relay", "a/b", strings.Repeat("x", 65)} {
		for _, path := range []string{"/mcp", "/api/tasks"} {
			r := httptest.NewRequest("POST", path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
			r.Header.Set("X-Devbot-Client", c)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 400 {
				t.Fatalf("%s with X-Devbot-Client %q: want 400, got %d", path, c, w.Code)
			}
		}
	}
	if len(f.Items) != 0 {
		t.Fatal("a rejected call wrote something")
	}
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	r.Header.Set("X-Devbot-Client", "chat-relay_2.0")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("a plain label should pass, got %d", w.Code)
	}
}

func scrape(t *testing.T, s *Server) string {
	t.Helper()
	w := httptest.NewRecorder()
	s.MetricsHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	return w.Body.String()
}

// The cost, token and duration metrics carry the attribution labels
// metrics.cost_labels and metrics.static_labels pick, in that order.
func TestMetricsAttributionLabels(t *testing.T) {
	s, _ := newServer(t, "client")
	s.Cfg.Metrics = config.Metrics{CostLabels: []string{"repo", "requester"}, StaticLabels: []string{"team"}}
	ctx := t.Context()
	q := s.E.Q
	q.Count(ctx, "agent_cost_usd", []string{"claude", "m", "implement", "fake:acme/app", "alice", "payments"}, 1.25)
	q.Count(ctx, "agent_run_seconds", []string{"claude", "m", "review", "fake:acme/app", "alice", "payments"}, 30)
	q.Count(ctx, "agent_tokens", []string{"claude", "m", "implement", "fake:acme/app", "alice", "payments", "output"}, 7)
	// Written under the defaults, before the config changed: left out.
	q.Count(ctx, "agent_cost_usd", []string{"claude", "m", "implement", "fake:acme/app", "acme"}, 9)
	q.Observe(ctx, "job_duration_seconds", []string{"implement", "fake:acme/app", "alice", "payments"}, []float64{60, 600}, 90)
	q.Observe(ctx, "job_duration_seconds", []string{"implement", "fake:acme/app", "alice", "payments"}, []float64{60, 600}, 30)
	body := scrape(t, s)
	for _, want := range []string{
		`devbot_agent_cost_usd_total{harness="claude",model="m",role="implement",repo="fake:acme/app",requester="alice",label_team="payments"} 1.25`,
		`devbot_agent_run_seconds_total{harness="claude",model="m",role="review",repo="fake:acme/app",requester="alice",label_team="payments"} 30`,
		`devbot_agent_tokens_total{harness="claude",model="m",role="implement",repo="fake:acme/app",requester="alice",label_team="payments",kind="output"} 7`,
		"# TYPE devbot_job_duration_seconds histogram\n" +
			`devbot_job_duration_seconds_bucket{role="implement",repo="fake:acme/app",requester="alice",label_team="payments",le="60"} 1` + "\n" +
			`devbot_job_duration_seconds_bucket{role="implement",repo="fake:acme/app",requester="alice",label_team="payments",le="600"} 2` + "\n" +
			`devbot_job_duration_seconds_bucket{role="implement",repo="fake:acme/app",requester="alice",label_team="payments",le="+Inf"} 2` + "\n" +
			`devbot_job_duration_seconds_sum{role="implement",repo="fake:acme/app",requester="alice",label_team="payments"} 120` + "\n" +
			`devbot_job_duration_seconds_count{role="implement",repo="fake:acme/app",requester="alice",label_team="payments"} 2` + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack\n%s\n\n%s", want, body)
		}
	}
	if strings.Contains(body, `owner="acme"`) || regexp.MustCompile(`(?m)^devbot_agent_cost_usd_total\{.*\} 9$`).MatchString(body) {
		t.Errorf("a series with other labels was exported:\n%s", body)
	}

	// No cost labels at all: just the metrics' own.
	s.Cfg.Metrics = config.Metrics{CostLabels: []string{}}
	q.Count(ctx, "agent_cost_usd", []string{"claude", "m", "implement"}, 2)
	if body := scrape(t, s); !strings.Contains(body, `devbot_agent_cost_usd_total{harness="claude",model="m",role="implement"} 2`) || strings.Contains(body, "requester=") {
		t.Errorf("cost_labels: [] still labels by attribution:\n%s", body)
	}
}

type fakePods struct {
	pods    map[string]kube.PodMeta
	patches []string
	sent    map[string]kube.PodMeta // the last patch sent to each pod
}

func (f *fakePods) PodMeta(_ context.Context, name string) (kube.PodMeta, error) {
	m, ok := f.pods[name]
	if !ok {
		return m, kube.ErrNotFound
	}
	return m, nil
}

func (f *fakePods) PatchPodMeta(_ context.Context, name string, m kube.PodMeta) error {
	f.patches = append(f.patches, name)
	if f.sent == nil {
		f.sent = map[string]kube.PodMeta{}
	}
	f.sent[name] = m
	p := f.pods[name]
	maps.Copy(p.Labels, m.Labels)
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	maps.Copy(p.Annotations, m.Annotations)
	f.pods[name] = p
	return nil
}

// serve puts each running job's attribution on its pod, once, and only on
// this install's job pod of the lease's kind.
func TestLabelPods(t *testing.T) {
	s, _ := newServer(t, "client")
	s.Cfg.PodLabels.Enabled, s.Cfg.PodLabels.Instance = true, "devbot"
	jobPod := func(kind string) kube.PodMeta {
		return kube.PodMeta{Labels: map[string]string{"app.kubernetes.io/instance": "devbot", "app.kubernetes.io/component": kind}}
	}
	pods := &fakePods{pods: map[string]kube.PodMeta{
		"devbot-implement-a": jobPod("implement"),
		"devbot-review-b":    jobPod("review"),
		"devbot-serve":       {Labels: map[string]string{"app.kubernetes.io/instance": "devbot", "app.kubernetes.io/component": "serve"}},
	}}
	s.Pods = pods
	ctx := t.Context()
	q := s.E.Q
	lease := func(kind, ref, pod string, labels map[string]string) {
		q.Acquire(ctx, kind, ref, pod, time.Minute)
		q.Describe(ctx, kind, ref, pod, queue.LeaseDetail{Pod: pod, PodLabels: labels, PodAnnotations: map[string]string{"devbot.io/requester": "anonymous/cli"}})
	}
	lease(queue.KindImplement, "fake:acme/app#1", "devbot-implement-a", map[string]string{"devbot.io/task": "1", "devbot.io/requester": "anonymous-cli"})
	// A lease naming a pod that isn't a job pod of its kind.
	lease(queue.KindReview, "fake:acme/app#2", "devbot-serve", map[string]string{"devbot.io/task": "2"})
	// A pod that's gone.
	lease(queue.KindReview, "fake:acme/app#3", "devbot-review-gone", map[string]string{"devbot.io/task": "3"})
	// No pod recorded (outside a cluster).
	q.Acquire(ctx, queue.KindImplement, "fake:acme/app#4", "laptop", time.Minute)

	s.labelPods(ctx)
	if !slices.Equal(pods.patches, []string{"devbot-implement-a"}) {
		t.Fatalf("patched %v", pods.patches)
	}
	got := pods.pods["devbot-implement-a"]
	if got.Labels["devbot.io/task"] != "1" || got.Labels["devbot.io/requester"] != "anonymous-cli" || got.Labels["app.kubernetes.io/component"] != "implement" ||
		got.Annotations["devbot.io/requester"] != "anonymous/cli" {
		t.Fatalf("labelled pod: %+v", got)
	}
	if _, ok := pods.pods["devbot-serve"].Labels["devbot.io/task"]; ok {
		t.Fatal("serve's own pod was labelled")
	}

	// Again, and again after a restart forgets what was done: nothing to do.
	s.labelPods(ctx)
	s.labelled = map[string]string{}
	s.labelPods(ctx)
	if len(pods.patches) != 1 {
		t.Fatalf("a labelled pod was patched again: %v", pods.patches)
	}

	// A new job on a pod that has come up since.
	lease(queue.KindReview, "fake:acme/app#5", "devbot-review-b", map[string]string{"devbot.io/task": "5"})
	s.labelPods(ctx)
	if !slices.Equal(pods.patches, []string{"devbot-implement-a", "devbot-review-b"}) || pods.pods["devbot-review-b"].Labels["devbot.io/task"] != "5" {
		t.Fatalf("patched %v", pods.patches)
	}
	// A finished job's pod is forgotten.
	q.Release(ctx, queue.KindImplement, "fake:acme/app#1", "devbot-implement-a")
	s.labelPods(ctx)
	if _, ok := s.labelled["devbot-implement-a"]; ok {
		t.Fatal("a finished job's pod is still remembered")
	}
}

// A lease is written by anything that reaches Redis, the agent in a job pod
// included: serve puts on the pod only the keys devbot sets, with valid values.
func TestLabelPodsForgedLease(t *testing.T) {
	s, _ := newServer(t, "client")
	s.Cfg.PodLabels.Enabled, s.Cfg.PodLabels.Instance = true, "devbot"
	selector := map[string]string{"app.kubernetes.io/instance": "devbot", "app.kubernetes.io/component": "implement",
		"batch.kubernetes.io/job-name": "devbot-implement-x", "batch.kubernetes.io/controller-uid": "uid-1"}
	pods := &fakePods{pods: map[string]kube.PodMeta{"devbot-implement-x": {Labels: maps.Clone(selector)}}}
	s.Pods = pods
	ctx := t.Context()
	s.E.Q.Acquire(ctx, queue.KindImplement, "fake:acme/app#1", "devbot-implement-x", time.Minute)
	s.E.Q.Describe(ctx, queue.KindImplement, "fake:acme/app#1", "devbot-implement-x", queue.LeaseDetail{Pod: "devbot-implement-x",
		PodLabels: map[string]string{
			"devbot.io/task":                     "1",
			"labels.devbot.io/team":              "payments",
			"app.kubernetes.io/component":        "serve",
			"batch.kubernetes.io/job-name":       "other",
			"batch.kubernetes.io/controller-uid": "uid-2",
			"devbot.io/requester":                "not a label value",
			"devbot.io/Bad/key":                  "x",
			"labels.devbot.io/":                  "x",
		},
		PodAnnotations: map[string]string{
			"devbot.io/origin": "https://chat.example/1",
			"devbot.io/notes":  strings.Repeat("x", 2000),
			"kubectl.kubernetes.io/last-applied-configuration": "{}",
		}})

	s.labelPods(ctx)
	sent := pods.sent["devbot-implement-x"]
	if !maps.Equal(sent.Labels, map[string]string{"devbot.io/task": "1", "labels.devbot.io/team": "payments"}) ||
		!maps.Equal(sent.Annotations, map[string]string{"devbot.io/origin": "https://chat.example/1"}) {
		t.Fatalf("sent %+v", sent)
	}
	got := pods.pods["devbot-implement-x"].Labels
	for k, v := range selector {
		if got[k] != v {
			t.Fatalf("%s is %q, want %q", k, got[k], v)
		}
	}

	// Again: the pod already has what may go on it.
	s.labelled = map[string]string{}
	s.labelPods(ctx)
	if len(pods.patches) != 1 {
		t.Fatalf("patched again: %v", pods.patches)
	}
}

// Readiness follows Redis, so the Service stops sending a replica requests
// it can't serve; liveness doesn't, since restarting serve won't bring
// Redis back.
func TestReadinessFollowsRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	cfg, _ := config.Load("")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := &core.Engine{Forges: forge.Registry{}, Q: queue.New(mr.Addr(), 0), Cfg: cfg, Repos: &config.RepoCache{}, Log: log}
	s, err := New(e, cfg, log, "test")
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	probe := func(path string) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w.Code
	}
	if probe("/readyz") != 200 || probe("/healthz") != 200 {
		t.Fatal("with Redis up, ready and live")
	}
	mr.Close()
	if probe("/readyz") != 503 {
		t.Fatal("without Redis, not ready")
	}
	if probe("/healthz") != 200 {
		t.Fatal("without Redis, still live")
	}
}

// edit_task is on MCP and HTTP: the edit is made and pinned, and a call
// that asks for nothing, or names a closed task, is a bad request.
func TestEditTaskSurfaces(t *testing.T) {
	s, f := newServer(t, "client")
	h := s.Handler()
	ctx := context.Background()
	st, err := s.E.FileTask(ctx, core.FileRequest{Repo: "fake:acme/app", Title: "t", AcceptanceCriteria: "- x"}, core.Caller{Principal: "client"})
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := forge.ParseItem(st.Task)
	call := rpc(t, h, "chat-relay", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"edit_task","arguments":{"ref":"`+st.Task+`","acceptance_criteria":"- x and y","note":"y too"}}}`)
	if res := call["result"].(map[string]any); res["isError"] == true {
		t.Fatalf("edit_task failed: %v", res)
	}
	if it, _ := f.GetItem(ctx, ref); !strings.Contains(it.Body, "- x and y") {
		t.Fatalf("the MCP edit didn't land: %q", it.Body)
	}
	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/task/edit", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post(`{"ref":"` + st.Task + `","title":"t2"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"title":"t2"`) {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	if w := post(`{"ref":"` + st.Task + `","note":"nothing"}`); w.Code != 400 {
		t.Fatalf("an edit of nothing: %d %s", w.Code, w.Body.String())
	}
	f.CloseItem(ctx, ref)
	if w := post(`{"ref":"` + st.Task + `","title":"t3"}`); w.Code != 400 || !strings.Contains(w.Body.String(), "closed") {
		t.Fatalf("a closed task: %d %s", w.Code, w.Body.String())
	}
	if evs, _, _ := s.E.Q.Events(ctx, "", 100); !slices.ContainsFunc(evs, func(e queue.Event) bool { return e.Type == "task.edited" }) {
		t.Fatalf("want task.edited events: %+v", evs)
	}
}

// A page is 100 events unless asked otherwise, and never more than 500.
func TestEventLimit(t *testing.T) {
	for in, want := range map[int64]int64{0: 100, -3: 100, 7: 7, 500: 500, 501: 500, 10000: 500} {
		if got := eventLimit(in); got != want {
			t.Errorf("eventLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

// approve_task and POST /api/task/approve take a person signed in through
// the forge: without sign-in (auth.mode none) both are refused, whatever
// the call says, and nothing is approved or queued.
func TestApproveTaskNeedsSignIn(t *testing.T) {
	s, f := newServer(t, "client")
	h := s.Handler()
	r := forge.RepoRef{Provider: "fake", Owner: "acme", Name: "app"}
	ref := f.NewIssue(r, "alice", "x", "", core.LNeedsApproval)
	held := core.HeldFile{Path: ".github/workflows/ci.yml", Mode: "100644", Blob: "1111111111111111111111111111111111111111"}
	f.AddComment(ref, "devbot", "Holding.\n\n"+core.RenderStatus(core.Status{Kind: "held", SHA: "abcdef1234567", Branch: "devbot/1-x", Files: []core.HeldFile{held}}))
	call := rpc(t, h, "chat-relay", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"approve_task","arguments":{"ref":"`+ref.String()+`","sha":"abcdef1"}}}`)
	if res := call["result"].(map[string]any); res["isError"] != true || !strings.Contains(fmt.Sprint(res["content"]), "signed in") {
		t.Fatalf("approve_task without sign-in: %v", call)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/task/approve", strings.NewReader(`{"ref":"`+ref.String()+`","sha":"abcdef1"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(w, req)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "signed in") {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if q, _ := s.E.Q.IsQueued(t.Context(), "implement", ref.String()); q {
		t.Fatal("nothing is queued")
	}
	if it, _ := f.GetItem(t.Context(), ref); it.HasLabel(core.LApprove) {
		t.Fatal("no label is added")
	}
}

// wait_task's until and build_against fields read as the same words in on.
func TestWaitFields(t *testing.T) {
	for v, want := range map[refText]string{
		{On: "gitea:acme/lib#2"}:                                                 "gitea:acme/lib#2",
		{On: "gitea:acme/lib#2", Until: "tagged v*"}:                             "gitea:acme/lib#2 until tagged v*",
		{On: "gitea:acme/lib#2", Until: "until tagged", BuildAgainst: true}:      "gitea:acme/lib#2 until tagged, build against",
		{On: "gitea:acme/lib#2 until tagged, build against", BuildAgainst: true}: "gitea:acme/lib#2 until tagged, build against",
		{On: "gitea:acme/lib#2", BuildAgainst: true}:                             "gitea:acme/lib#2, build against",
	} {
		if got := v.wait(); got != want {
			t.Errorf("%+v: got %q, want %q", v, got, want)
		}
	}
}

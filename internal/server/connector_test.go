package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/fake"
)

// spentForge is a forge whose rate limit has run out for the calls in
// limited, by method, and for listing the labels in labels.
type spentForge struct {
	*fake.Forge
	limit   *forge.RateLimitError
	methods map[string]bool
	labels  map[string]bool
}

func (p *spentForge) CreateItem(ctx context.Context, r forge.RepoRef, title, body string, labels []string) (forge.Item, error) {
	if p.methods["CreateItem"] {
		return forge.Item{}, p.limit
	}
	return p.Forge.CreateItem(ctx, r, title, body, labels)
}

func (p *spentForge) GetItem(ctx context.Context, ref forge.ItemRef) (forge.Item, error) {
	if p.methods["GetItem"] {
		return forge.Item{}, p.limit
	}
	return p.Forge.GetItem(ctx, ref)
}

func (p *spentForge) SearchItems(ctx context.Context, label, state string) ([]forge.Item, error) {
	if p.labels[label] {
		return nil, p.limit
	}
	return p.Forge.SearchItems(ctx, label, state)
}

func (p *spentForge) ListItems(ctx context.Context, r forge.RepoRef, label, state string) ([]forge.Item, error) {
	if p.labels[label] {
		return nil, p.limit
	}
	return p.Forge.ListItems(ctx, r, label, state)
}

// call makes one HTTP API request.
func call(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// callTool makes one MCP tool call, and returns its texts, whether it is
// an error, and the HTTP response.
func callTool(t *testing.T, h http.Handler, name string, args any) ([]string, bool, *httptest.ResponseRecorder) {
	t.Helper()
	a, _ := json.Marshal(args)
	w := call(h, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+string(a)+`}}`)
	var out struct {
		Result struct {
			Content []struct{ Text string }
			IsError bool
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
	}
	var texts []string
	for _, c := range out.Result.Content {
		texts = append(texts, c.Text)
	}
	return texts, out.Result.IsError, w
}

// retryAfterOK checks a Retry-After of about until.
func retryAfterOK(h http.Header, until time.Time) bool {
	n, err := strconv.Atoi(h.Get("Retry-After"))
	want := time.Until(until).Seconds()
	return err == nil && float64(n) >= want-1 && float64(n) <= want+1
}

// A forge's rate limit is a 429 with Retry-After and its reset, on the
// HTTP API and the MCP tools alike, never an internal error. file_task
// says whether the issue was filed.
func TestRateLimitsAreReportedAsSuch(t *testing.T) {
	s, f := newServer(t, "client")
	h := s.Handler()
	repo := forge.RepoRef{Provider: "fake", Owner: "acme", Name: "app"}
	ref := f.NewIssue(repo, "alice", "t", "", core.LWorking)
	until := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	reset := until.UTC().Format(time.RFC3339)
	p := &spentForge{Forge: f, limit: &forge.RateLimitError{Provider: "github", Kind: forge.LimitPrimary, Until: until},
		methods: map[string]bool{"CreateItem": true, "GetItem": true}, labels: map[string]bool{}}
	for _, l := range core.StateLabels {
		p.labels[l] = true
	}
	s.E.Forges["fake"] = p
	file := map[string]any{"repo": repo.String(), "title": "t", "acceptance_criteria": "- x"}
	fileJSON, _ := json.Marshal(file)

	for _, c := range []struct {
		name, method, path, body, tool, says string
		args                                 any
	}{
		{"file_task", "POST", "/api/tasks", string(fileJSON), "file_task", "nothing was filed", file},
		{"list_tasks", "GET", "/api/tasks?repo=" + repo.String(), "", "list_tasks", "", map[string]string{"repo": repo.String()}},
		{"list_tasks unscoped", "GET", "/api/tasks", "", "list_tasks", "", map[string]string{}},
		{"task_status", "GET", "/api/task?ref=" + ref.String(), "", "task_status", "", map[string]string{"ref": ref.String()}},
	} {
		w := call(h, c.method, c.path, c.body)
		if w.Code != http.StatusTooManyRequests || !retryAfterOK(w.Header(), until) || !strings.Contains(w.Body.String(), reset) ||
			strings.Contains(w.Body.String(), "internal error") || !strings.Contains(w.Body.String(), c.says) {
			t.Errorf("HTTP %s: %d, Retry-After %q, %q", c.name, w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
		texts, isErr, w := callTool(t, h, c.tool, c.args)
		if !isErr || len(texts) != 1 || !strings.Contains(texts[0], "rate limited") || !strings.Contains(texts[0], reset) ||
			!strings.Contains(texts[0], c.says) || !retryAfterOK(w.Header(), until) {
			t.Errorf("MCP %s: %v %q, Retry-After %q", c.name, isErr, texts, w.Header().Get("Retry-After"))
		}
	}

	// Past the issue's creation, the error names it.
	p.methods = map[string]bool{}
	f.CommentErr = p.limit
	w := call(h, "POST", "/api/tasks", string(fileJSON))
	items, _ := f.SearchItems(context.Background(), core.LReady, "all")
	if w.Code != http.StatusTooManyRequests || len(items) != 1 || !strings.Contains(w.Body.String(), "filed "+items[0].URL) || strings.Contains(w.Body.String(), "nothing was filed") {
		t.Fatalf("after filing: %d %q (filed %v)", w.Code, w.Body.String(), items)
	}
}

// A list read in part is what was read, with what is missing: a header on
// the HTTP API, a second text from the MCP tool.
func TestPartialListSaysWhatIsMissing(t *testing.T) {
	s, f := newServer(t, "client")
	h := s.Handler()
	repo := forge.RepoRef{Provider: "fake", Owner: "acme", Name: "app"}
	ref := f.NewIssue(repo, "alice", "t", "", core.LWorking)
	until := time.Now().Add(time.Hour)
	s.E.Forges["fake"] = &spentForge{Forge: f, limit: &forge.RateLimitError{Provider: "github", Kind: forge.LimitPrimary, Until: until},
		labels: map[string]bool{core.LReady: true}}

	for _, path := range []string{"/api/tasks", "/api/tasks?repo=" + repo.String()} {
		w := call(h, "GET", path, "")
		var ts []core.TaskStatus
		json.Unmarshal(w.Body.Bytes(), &ts)
		if w.Code != 200 || len(ts) != 1 || ts[0].Task != ref.String() || !strings.Contains(w.Header().Get("X-Devbot-Incomplete"), "rate limited") ||
			!retryAfterOK(w.Header(), until) {
			t.Errorf("%s: %d %v, %q", path, w.Code, w.Body.String(), w.Header())
		}
	}
	texts, isErr, w := callTool(t, h, "list_tasks", map[string]string{"repo": repo.String()})
	if isErr || len(texts) != 2 || !strings.Contains(texts[0], ref.String()) || !strings.HasPrefix(texts[1], "incomplete: rate limited") || !retryAfterOK(w.Header(), until) {
		t.Fatalf("MCP: %v %q", isErr, texts)
	}
}

// A repo on a provider the install doesn't have is a 400 naming the ones
// it has.
func TestUnknownProviderIs400(t *testing.T) {
	s, _ := newServer(t, "client")
	h := s.Handler()
	w := call(h, "POST", "/api/tasks", `{"repo":"gitea:umg/app","title":"t","acceptance_criteria":"- x"}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "this install has: fake") {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	texts, isErr, _ := callTool(t, h, "file_task", map[string]string{"repo": "gitea:umg/app", "title": "t", "acceptance_criteria": "- x"})
	if !isErr || !strings.Contains(texts[0], "this install has: fake") {
		t.Fatalf("MCP: %q", texts)
	}
}

// The tools' examples and the instructions name the install's own
// providers, never one it hasn't.
func TestToolsNameTheConfiguredProviders(t *testing.T) {
	s, f := newServer(t, "client")
	h := s.Handler()
	for _, c := range []struct {
		providers []string
		want      []string
	}{
		{[]string{"github"}, []string{"github:acme/webapp", "github:acme/webapp#12", "providers are: github."}},
		{[]string{"github", "gitea"}, []string{"gitea:acme/webapp or github:acme/webapp", "gitea:acme/webapp#12 or github:acme/webapp#12", "providers are: gitea, github."}},
	} {
		s.E.Forges = forge.Registry{}
		for _, n := range c.providers {
			s.E.Forges[n] = f
		}
		tools := call(h, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).Body.String()
		init := call(h, "POST", "/mcp", `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`).Body.String()
		all := tools + init
		for _, w := range c.want {
			if !strings.Contains(all, w) {
				t.Errorf("%v: no %q in\n%s", c.providers, w, all)
			}
		}
		for _, n := range []string{"gitea", "github", "fake"} {
			if !strings.Contains(strings.Join(c.providers, " "), n) && strings.Contains(all, n+":") {
				t.Errorf("%v: names %s, which the install hasn't", c.providers, n)
			}
		}
	}
}

// budgetForge records what wait budget its GetItem was called with.
type budgetForge struct {
	*fake.Forge
	fits, over bool
}

func (p *budgetForge) GetItem(ctx context.Context, ref forge.ItemRef) (forge.Item, error) {
	p.over = forge.SpendWait(ctx, connectorWaitBudget+time.Second)
	p.fits = forge.SpendWait(ctx, connectorWaitBudget)
	return p.Forge.GetItem(ctx, ref)
}

// A connector call's forge calls wait out rate limits for no more than
// connectorWaitBudget in all, over HTTP and MCP.
func TestConnectorCallsHaveAWaitBudget(t *testing.T) {
	s, f := newServer(t, "client")
	h := s.Handler()
	ref := f.NewIssue(forge.RepoRef{Provider: "fake", Owner: "acme", Name: "app"}, "alice", "t", "", core.LWorking)
	p := &budgetForge{Forge: f}
	s.E.Forges["fake"] = p
	if w := call(h, "GET", "/api/task?ref="+ref.String(), ""); w.Code != 200 || p.over || !p.fits {
		t.Fatalf("HTTP: %d, a wait past the budget allowed: %v, one within it: %v", w.Code, p.over, p.fits)
	}
	p.fits = false
	if _, isErr, _ := callTool(t, h, "task_status", map[string]string{"ref": ref.String()}); isErr || p.over || !p.fits {
		t.Fatalf("MCP: a wait past the budget allowed: %v, one within it: %v", p.over, p.fits)
	}
}

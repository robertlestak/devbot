package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/audit/audittest"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
)

// recorder keeps every run's request, and the MCP config the CLI was given.
type recorder struct {
	agent.Runner
	reqs []agent.RunRequest
	mcp  []string
}

func (r *recorder) Run(ctx context.Context, req agent.RunRequest) (agent.RunResult, error) {
	res, err := r.Runner.Run(ctx, req)
	r.reqs = append(r.reqs, req)
	b, _ := os.ReadFile(filepath.Join(req.Dir, ".devbot", "seen-mcp.json"))
	r.mcp = append(r.mcp, string(b))
	return res, err
}

func (r *recorder) servers(i int) []string {
	var out []string
	for _, s := range r.reqs[i].MCPServers {
		out = append(out, s.Name)
	}
	return out
}

func newMCPEnv(t *testing.T, devbotYAML string) (*hEnv, *recorder) {
	h := newHEnv(t, devbotYAML)
	h.e.Cfg.MCPServers = map[string]config.MCPServer{
		"context7":   {Type: "http", URL: "https://mcp.example.com/context7/mcp", DisabledTools: []string{"resolve-library-id"}},
		"searxng":    {Type: "http", URL: "https://mcp.example.com/searxng/mcp"},
		"playwright": {Type: "http", URL: "https://mcp.example.com/playwright/mcp"},
	}
	h.e.Cfg.Agents.Implement.MCPServers = []string{"context7", "searxng"}
	h.e.Cfg.Agents.Review.MCPServers = []string{"context7"}
	rec := &recorder{Runner: h.r.Agent}
	h.r.Agent = rec
	return h, rec
}

// Each role's run gets the install's servers for that role, listed in its
// prompt with their output marked untrusted, and its MCP tool calls are
// counted by server and role.
func TestMCPServersReachTheRun(t *testing.T) {
	h, rec := newMCPEnv(t, yamlChecks)
	h.mode("mcp")
	au := h.audited()
	ref := h.file("Say hello")
	ctx := context.Background()
	if err := h.r.Implement(ctx); err != nil {
		t.Fatal(err)
	}
	if s := h.state(ref); s != core.LInReview {
		t.Fatalf("a server that didn't connect mustn't fail the run: %q\n%s", s, h.comments(ref))
	}
	if got := rec.servers(0); !slices.Equal(got, []string{"context7", "searxng"}) {
		t.Fatalf("implement servers %q", got)
	}
	if s := rec.reqs[0].MCPServers[0]; s.URL != "https://mcp.example.com/context7/mcp" || !slices.Equal(s.DisabledTools, []string{"resolve-library-id"}) {
		t.Fatalf("context7 %+v", s)
	}
	p := rec.reqs[0].Prompt
	for _, want := range []string{"- `context7` (https://mcp.example.com/context7/mcp)\n- `searxng` (https://mcp.example.com/searxng/mcp)\n", "**untrusted** data"} {
		if !strings.Contains(p, want) {
			t.Fatalf("no %q in the prompt:\n%s", want, p)
		}
	}
	var cfg struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(rec.mcp[0]), &cfg); err != nil || len(cfg.MCPServers) != 2 || cfg.MCPServers["context7"] == nil || cfg.MCPServers["searxng"] == nil {
		t.Fatalf("the CLI's MCP config: %s", rec.mcp[0])
	}
	stats, _ := h.e.Q.Stats(ctx)
	if got := stats["agent_mcp_calls|claude|implement|context7"]; got != 2 {
		t.Fatalf("agent_mcp_calls = %v", got)
	}
	runs := au.Of("agent.run")
	if len(runs) != 1 {
		t.Fatalf("want one model run, got %v", au.Events())
	}
	d := audittest.Detail(runs[0])
	if calls, _ := d["mcp_calls"].(map[string]any); !slices.Equal(anyStrings(d["mcp_servers"]), []string{"context7", "searxng"}) ||
		len(calls) != 1 || calls["context7"] != float64(2) || !slices.Equal(anyStrings(d["mcp_unavailable"]), []string{"searxng: failed"}) {
		t.Fatalf("the run's audit record: %v", d)
	}
	for k := range stats {
		if strings.Contains(k, "forged") {
			t.Fatalf("a server the run didn't have became a label: %s", k)
		}
	}

	prs, _ := h.f.ListOpenPRs(ctx, repo)
	sha := git(t, h.bare, "rev-parse", prs[0].HeadBranch)
	h.f.SetHead(repo, prs[0].Number, sha)
	h.f.SetStatus(sha, "ci", "success")
	h.e.EvaluateItem(ctx, ref, core.SrcWebhook)
	h.mode("approve")
	if err := h.r.Review(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rec.reqs) != 2 || !slices.Equal(rec.servers(1), []string{"context7"}) || !strings.Contains(rec.reqs[1].Prompt, "`context7`") || strings.Contains(rec.reqs[1].Prompt, "searxng") {
		t.Fatalf("the reviewer gets its own role's servers: %+v", rec.reqs[1:])
	}
	if strings.Contains(rec.reqs[1].Prompt, "**Dependencies.**") {
		t.Fatalf("a task that built against nothing has its dependencies judged:\n%s", rec.reqs[1].Prompt)
	}
}

// A repo narrows the install's servers for a role, or turns them off.
func TestMCPServersRepoSubset(t *testing.T) {
	for _, c := range []struct {
		name, yaml string
		want       []string
	}{
		{"subset", yamlChecks + "agents:\n  implement: {mcp_servers: [searxng]}\n", []string{"searxng"}},
		{"none", yamlChecks + "agents:\n  implement: {mcp_servers: []}\n", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, rec := newMCPEnv(t, c.yaml)
			h.mode("done")
			ref := h.file("Say hello")
			if err := h.r.Implement(context.Background()); err != nil {
				t.Fatal(err)
			}
			if s := h.state(ref); s != core.LInReview {
				t.Fatalf("state %q\n%s", s, h.comments(ref))
			}
			if got := rec.servers(0); !slices.Equal(got, c.want) {
				t.Fatalf("servers %q, want %q", got, c.want)
			}
			if c.want == nil && (strings.Contains(rec.reqs[0].Prompt, "MCP") || rec.mcp[0] != `{"mcpServers":{}}`) {
				t.Fatalf("no servers, but the run was told of some: %s\n%s", rec.mcp[0], rec.reqs[0].Prompt)
			}
		})
	}
}

// A repo naming a server the install doesn't give the role is a config
// error on the task, before any model runs; the next run has nothing to do.
func TestMCPServersRepoUnknownName(t *testing.T) {
	h, rec := newMCPEnv(t, yamlChecks+"agents:\n  implement: {mcp_servers: [context7, playwright]}\n")
	h.mode("done")
	ref := h.file("Say hello")
	ctx := context.Background()
	if err := h.r.Implement(ctx); err != nil {
		t.Fatal(err)
	}
	if s := h.state(ref); s != core.LNeedsHuman {
		t.Fatalf("state %q", s)
	}
	c := h.comments(ref)
	if !strings.Contains(c, `agents.implement.mcp_servers: "playwright" isn't one of the MCP servers this install gives the implement role (context7, searxng)`) {
		t.Fatalf("the error isn't on the task:\n%s", c)
	}
	if len(rec.reqs) != 0 {
		t.Fatal("the model ran with a config error")
	}
	if err := h.r.Implement(ctx); err != nil {
		t.Fatal(err)
	}
	if h.comments(ref) != c || len(rec.reqs) != 0 {
		t.Fatal("a second run acted again")
	}
}

// anyStrings is a JSON array of strings, decoded into []any.
func anyStrings(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, s := range l {
		out = append(out, fmt.Sprint(s))
	}
	return out
}

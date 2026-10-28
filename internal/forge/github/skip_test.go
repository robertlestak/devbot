package github

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// toAcme makes a recorded delivery about acme/app, with edit, when set,
// applied after.
func toAcme(edit func(map[string]any)) func(map[string]any) {
	return func(p map[string]any) {
		p["repository"].(map[string]any)["full_name"] = "acme/app"
		if edit != nil {
			edit(p)
		}
	}
}

func withLabels(key string, names ...string) func(map[string]any) {
	return func(p map[string]any) {
		var ls []any
		for _, n := range names {
			ls = append(ls, map[string]any{"name": n})
		}
		p[key].(map[string]any)["labels"] = ls
	}
}

// A burst of deliveries about issues and PRs devbot has nothing to do with
// costs no call to GitHub, not even the repo's config: an org-wide install
// gets them from every repo, and reading each spent the rate limit the
// tasks needed. Each is handled twice, as a redelivery would be. A label
// of devbot's on the same delivery is read.
func TestUnconcernedDeliveriesCostNoCalls(t *testing.T) {
	f := newFakeGitHub(t, "")
	cfg := f.config()
	cfg.WebhookSecret = "s3cret"
	c, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	dcfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	reg := forge.Registry{"github": c}
	mr := miniredis.RunT(t)
	e := &core.Engine{Forges: reg, Q: queue.New(mr.Addr(), 0), Cfg: dcfg, Repos: &config.RepoCache{Forges: reg},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	calls := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.calls
	}
	handle := func(event, file string, edit func(map[string]any)) {
		t.Helper()
		r, body := delivery(t, event, file, toAcme(edit))
		r.Header.Set("X-GitHub-Hook-Installation-Target-ID", strconv.Itoa(implAppID))
		evs, err := c.ParseWebhook(r, body)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			if ev.Kind == forge.EventIgnored {
				t.Fatalf("%s %s: ignored", event, file)
			}
			if err := e.HandleEvent(context.Background(), ev); err != nil {
				t.Fatalf("%s %s: %v", event, file, err)
			}
		}
	}
	prComment := func(p map[string]any) {
		p["issue"].(map[string]any)["pull_request"] = map[string]any{"url": "https://api.github.com/repos/acme/app/pulls/1"}
	}
	setComment := func(body string) func(map[string]any) {
		return func(p map[string]any) { p["comment"].(map[string]any)["body"] = body }
	}

	before := calls()
	for range 2 {
		handle("status", "status", nil)
		handle("check_run", "check_run.completed", nil)
		handle("check_run", "check_run.completed", func(p map[string]any) { p["check_run"].(map[string]any)["pull_requests"] = []any{} })
		handle("check_suite", "check_suite.completed", nil)
		handle("pull_request", "pull_request.opened", nil)
		handle("pull_request", "pull_request.synchronize", nil)
		handle("pull_request", "pull_request.labeled", nil)
		handle("pull_request_review", "pull_request_review.submitted", nil)
		handle("issues", "issues.opened", nil)
		handle("issues", "issues.labeled", nil)
		handle("issues", "issues.unlabeled", nil)
		handle("issue_comment", "issue_comment.created", setComment("Thanks, looks good"))
		handle("issue_comment", "issue_comment.created", func(p map[string]any) { prComment(p); setComment("LGTM")(p) })
	}
	if n := calls() - before; n != 0 {
		t.Fatalf("%d calls to GitHub, want none", n)
	}
	stats, err := e.Q.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := stats["webhook_events_skipped|status"] + stats["webhook_events_skipped|pr"] + stats["webhook_events_skipped|item"]; got != 26 {
		t.Errorf("counted %v skipped, want 26: %v", got, stats)
	}

	// The same deliveries with devbot in them are read.
	for _, tc := range []struct {
		name, event, file string
		edit              func(map[string]any)
	}{
		{"devbot/ready on an issue", "issues", "issues.labeled", withLabels("issue", "bug", "devbot/ready")},
		{"devbot/ready on a PR", "pull_request", "pull_request.labeled", withLabels("pull_request", "devbot/ready")},
		{"a PR on a devbot branch", "pull_request", "pull_request.synchronize", func(p map[string]any) {
			p["pull_request"].(map[string]any)["head"].(map[string]any)["ref"] = "devbot/7-fix"
		}},
		{"a command", "issue_comment", "issue_comment.created", setComment("/wait #4")},
	} {
		before := calls()
		handle(tc.event, tc.file, tc.edit)
		if calls() == before {
			t.Errorf("%s: not read", tc.name)
		}
	}
}

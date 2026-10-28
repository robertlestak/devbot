package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// processes are two clients of one fake GitHub, as two replicas (or a
// replica and a job pod) would be: their own tokens and memory, one Redis,
// one clock. The first defers nothing; the second paces below 10%.
func processes(t *testing.T, f *fakeGitHub) (a, b *Client, clk *clock, s *sleeper) {
	t.Helper()
	q := queue.New(miniredis.RunT(t).Addr(), 0)
	share := func(pct int) *Client {
		cfg := f.config()
		cfg.LoadQuota = func(ctx context.Context, bucket string, res ...string) ([]forge.Quota, error) {
			return q.Quotas(ctx, "github", bucket, res...)
		}
		cfg.NoteQuota = func(ctx context.Context, bucket, res string, qt forge.Quota) error {
			return q.NoteQuota(ctx, "github", bucket, res, qt)
		}
		cfg.LowQuotaPercent = pct
		c, err := New(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b = share(0), share(10)
	clk, s = withClock(f, a)
	b.now, b.sleep = clk.now, s.sleep
	// Each mints its token first, so the calls below are the repo's alone
	// until the token expires.
	for _, c := range []*Client{a, b} {
		if _, err := c.DefaultBranch(ctx, repo); err != nil {
			t.Fatal(err)
		}
	}
	return a, b, clk, s
}

func (f *fakeGitHub) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Once one process has seen an installation's primary limit spent, another
// with the same Redis sends nothing on that installation until the reset,
// and goes on after it.
func TestPrimaryRateLimitIsShared(t *testing.T) {
	f := newFakeGitHub(t, "")
	a, b, clk, s := processes(t, f)
	reset := clk.now().Add(time.Hour).Unix()
	rateLimited(f, "/repos/acme/app", func(w http.ResponseWriter) {
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.Header().Set("X-Ratelimit-Limit", "5000")
		w.Header().Set("X-Ratelimit-Reset", fmt.Sprint(reset))
		writeJSON(w, 403, map[string]string{"message": "API rate limit exceeded for installation ID 11."})
	})
	var rl *forge.RateLimitError
	if _, err := a.DefaultBranch(ctx, repo); !errors.As(err, &rl) || rl.Kind != forge.LimitPrimary {
		t.Fatalf("got %v", err)
	}
	calls := f.callCount()
	if _, err := b.DefaultBranch(ctx, repo); !errors.As(err, &rl) || rl.Kind != forge.LimitPrimary || !rl.Until.After(time.Unix(reset, 0)) {
		t.Fatalf("got %v", err)
	}
	if n := f.callCount() - calls; n != 0 || len(s.waits) != 0 {
		t.Fatalf("the other process made %d calls into a spent limit, waits %v", n, s.waits)
	}

	clk.add(time.Hour + 2*time.Second)
	if br, err := b.DefaultBranch(ctx, repo); err != nil || br != "main" || f.callCount() == calls {
		t.Fatalf("after the reset: %q, %v", br, err)
	}
}

// Below the threshold, a low-priority call is deferred to the reset without
// a request, on a process that never saw the answer that said so, while a
// call at the normal priority goes on. After the reset both go out.
func TestLowQuotaDefersLowPriorityCalls(t *testing.T) {
	f := newFakeGitHub(t, "")
	a, b, clk, _ := processes(t, f)
	reset := clk.now().Add(30 * time.Minute)
	rateLimited(f, "/repos/acme/app", func(w http.ResponseWriter) {
		w.Header().Set("X-Ratelimit-Remaining", "400")
		w.Header().Set("X-Ratelimit-Limit", "5000")
		w.Header().Set("X-Ratelimit-Reset", fmt.Sprint(reset.Unix()))
		w.Header().Set("X-Ratelimit-Resource", "core")
		writeJSON(w, 200, map[string]string{"default_branch": "main"})
	})
	if _, err := a.DefaultBranch(ctx, repo); err != nil {
		t.Fatal(err)
	}
	low := forge.LowPriority(ctx)
	// The counter is process-wide and earlier runs have added to it.
	deferrals := forge.RateLimitErrors.Snapshot()[[2]string{"github", forge.LimitLowQuota}]
	calls := f.callCount()
	var rl *forge.RateLimitError
	if _, err := b.DefaultBranch(low, repo); !errors.As(err, &rl) || rl.Kind != forge.LimitLowQuota || !rl.Until.After(reset) {
		t.Fatalf("got %v", err)
	}
	if f.callCount() != calls {
		t.Fatal("a deferred call went out")
	}
	if _, err := b.DefaultBranch(ctx, repo); err != nil || f.callCount() != calls+1 {
		t.Fatalf("a normal call: %v", err)
	}
	// A process configured with 0 defers nothing.
	if _, err := a.DefaultBranch(low, repo); err != nil {
		t.Fatalf("low_quota_percent 0: %v", err)
	}

	clk.add(31 * time.Minute)
	if _, err := b.DefaultBranch(low, repo); err != nil {
		t.Fatalf("after the reset: %v", err)
	}
	if n := forge.RateLimitErrors.Snapshot()[[2]string{"github", forge.LimitLowQuota}]; n == deferrals {
		t.Fatal("the deferral wasn't counted")
	}
}

// Requests are counted by bucket and endpoint template, never by a path.
func TestRequestsAreCountedByEndpointTemplate(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	c.cfg.Name = "github-requests"
	// The counter is process-wide and earlier runs have added to it.
	before := forge.Requests.Snapshot()
	if _, err := c.DefaultBranch(ctx, repo); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for k, n := range forge.Requests.Snapshot() {
		if k.Provider == "github-requests" {
			got[k.Bucket+" "+k.Method+" "+k.Endpoint+" "+k.Status] += n - before[k]
		}
	}
	if got["installation 11 GET /repos/{owner}/{repo} 2xx"] != 1 || got["app 101 POST /app/installations/{}/access_tokens 2xx"] != 1 {
		t.Fatalf("counted %v", got)
	}

	for path, want := range map[string]string{
		"/repos/acme/app/pulls/12/reviews":                     "/repos/{owner}/{repo}/pulls/{}/reviews",
		"/repos/acme/app/contents/docs/a/b.md?ref=x":           "/repos/{owner}/{repo}/contents/{}",
		"/repos/acme/app/branches/devbot/12-x/protection":      "/repos/{owner}/{repo}/branches/{}/protection",
		"/repos/acme/app/git/ref/heads/devbot/12-x":            "/repos/{owner}/{repo}/git/ref/heads/{}",
		"/repos/acme/app/compare/abc...def?per_page=1":         "/repos/{owner}/{repo}/compare/{}",
		"/repos/acme/app/actions/jobs/99/logs":                 "/repos/{owner}/{repo}/actions/jobs/{}/logs",
		"/search/issues?q=label%3Adevbot%2Fready":              "/search/issues",
		"/installation/repositories?per_page=100&page=7":       "/installation/repositories",
		c.cfg.URL + "/repos/acme/app/issues/3/comments?page=2": "/repos/{owner}/{repo}/issues/{}/comments",
		c.graphqlURL(): "/graphql",
	} {
		u := path
		if !strings.HasPrefix(u, "http") {
			u = c.cfg.URL + u
		}
		if got := c.endpoint(u); got != want {
			t.Errorf("%s: %s, want %s", path, got, want)
		}
	}
	if forge.StatusClass(304) != "304" || forge.StatusClass(301) != "3xx" || forge.StatusClass(422) != "4xx" {
		t.Error("status classes")
	}
}

// A call made with a forge.WaitBudget waits out rate limits only within
// it, in all: a wait past what is left fails at once with the
// RateLimitError, as one past maxWait does, without a request.
func TestWaitBudget(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	clk, s := withClock(f, c)
	if _, err := c.DefaultBranch(ctx, repo); err != nil { // mint the token first
		t.Fatal(err)
	}
	reset := clk.now().Add(30 * time.Second).Unix()
	spent := func(w http.ResponseWriter) {
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.Header().Set("X-Ratelimit-Reset", fmt.Sprint(reset))
		writeJSON(w, 403, map[string]string{"message": "API rate limit exceeded for installation ID 11."})
	}
	rateLimited(f, "/repos/acme/app", spent)
	budget := forge.WaitBudget(ctx, 10*time.Second)
	var rl *forge.RateLimitError
	if _, err := c.DefaultBranch(budget, repo); !errors.As(err, &rl) || rl.Kind != forge.LimitPrimary || !rl.Until.After(time.Unix(reset, 0).Add(-time.Second)) {
		t.Fatalf("got %v", err)
	}
	// The limit is known spent now: the next call is held back before it
	// goes out, and fails without a request.
	calls := f.callCount()
	if _, err := c.DefaultBranch(budget, repo); !errors.As(err, &rl) || f.callCount() != calls {
		t.Fatalf("got %v after %d calls", err, f.callCount()-calls)
	}
	if len(s.waits) != 0 {
		t.Fatalf("waited %v past the budget", s.waits)
	}

	// Waits within the budget are made, and what they take is gone: two
	// of 6s don't fit in 10s.
	clk.add(time.Minute)
	retryAfter := func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "6")
		writeJSON(w, 429, map[string]string{"message": "slow down"})
	}
	rateLimited(f, "/repos/acme/app", retryAfter, retryAfter)
	budget = forge.WaitBudget(ctx, 10*time.Second)
	if _, err := c.DefaultBranch(budget, repo); !errors.As(err, &rl) || rl.Kind != forge.LimitSecondary {
		t.Fatalf("got %v", err)
	}
	if fmt.Sprint(s.waits) != fmt.Sprint([]time.Duration{6 * time.Second}) {
		t.Fatalf("waits %v, want one of 6s", s.waits)
	}
	// Without a budget the same limits are waited out.
	s.waits = nil
	rateLimited(f, "/repos/acme/app", retryAfter, retryAfter)
	if _, err := c.DefaultBranch(ctx, repo); err != nil || len(s.waits) != 2 {
		t.Fatalf("got %v, waits %v", err, s.waits)
	}
}

package harness

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// chain gives the implementer prime, then the fallbacks, and each model the
// stub mode in modes (done for one not named).
func (h *hEnv) chain(modes map[string]string, fallbacks ...string) {
	h.t.Helper()
	h.e.Cfg.Agents.Implement.Model, h.e.Cfg.Agents.Implement.FallbackModels = "prime", fallbacks
	h.e.Cfg.Agents.Review.Model, h.e.Cfg.Agents.Review.FallbackModels = "prime", fallbacks
	h.r.OverloadWait = time.Millisecond
	h.mode("per-model")
	h.models(modes)
}

func (h *hEnv) models(modes map[string]string) {
	for _, m := range []string{"prime", "second", "third", "banned"} {
		os.Remove(h.modeFile + ".model." + m)
	}
	for m, mode := range modes {
		os.WriteFile(h.modeFile+".model."+m, []byte(mode), 0o644)
	}
}

// ran is the models the stub ran on since the last call, in order.
func (h *hEnv) ran() []string {
	h.t.Helper()
	b, _ := os.ReadFile(h.modeFile + ".models")
	os.Remove(h.modeFile + ".models")
	return strings.Fields(string(b))
}

func (h *hEnv) failovers(ref forge.ItemRef) []core.Status {
	cs, _ := h.f.ListComments(context.Background(), ref)
	var out []core.Status
	for _, s := range core.Statuses(cs, h.e.Bots(ref.Repo)...) {
		if s.Kind == "model-failover" {
			out = append(out, s.Status)
		}
	}
	return out
}

func (h *hEnv) limited(model string) time.Time {
	at, _, err := h.e.Q.ModelLimit(context.Background(), queue.ModelKey("claude", "", model))
	if err != nil {
		h.t.Fatal(err)
	}
	return at
}

// A usage limit, an overload that outlasts its retries, and a model the
// provider doesn't serve each move the run to the next model, which does
// the work: no hold, no backoff, and the issue says so once.
func TestFallbackOnModelOut(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		mode  string
		class string
		ran   []string
	}{
		{"ratelimit", "rate_limited", []string{"prime", "second"}},
		{"overloaded", "overloaded", []string{"prime", "prime", "prime", "second"}},
		{"unavailable", "unavailable", []string{"prime", "second"}},
	} {
		t.Run(c.mode, func(t *testing.T) {
			h := newHEnv(t, yamlChecks)
			h.chain(map[string]string{"prime": c.mode}, "second", "third")
			ref := h.file("Say hello")
			if err := h.implement(ctx); err != nil {
				t.Fatal(err)
			}
			if s := h.state(ref); s != core.LInReview {
				t.Fatalf("want in-review, got %q\n%s", s, h.comments(ref))
			}
			if got := h.ran(); !slices.Equal(got, c.ran) {
				t.Fatalf("ran on %v, want %v", got, c.ran)
			}
			if hold, _ := h.e.Q.Hold(ctx); !hold.IsZero() {
				t.Fatalf("a fallback that served the run holds nothing: %v", hold)
			}
			fo := h.failovers(ref)
			if len(fo) != 1 || fo[0].Role != "implement" || fo[0].Model != "prime" || fo[0].To != "second" || fo[0].Mode != c.class {
				t.Fatalf("failovers %+v\n%s", fo, h.comments(ref))
			}
			if !strings.Contains(h.comments(ref), "moved to the fallback `second`") {
				t.Fatalf("the comment names the models:\n%s", h.comments(ref))
			}
			if !h.limited("prime").After(time.Now()) || !h.limited("second").IsZero() {
				t.Fatalf("prime should be remembered as out, second not: %v %v", h.limited("prime"), h.limited("second"))
			}
			stats, _ := h.e.Q.Stats(ctx)
			if stats["agent_fallbacks|claude|implement|prime|second|"+c.class] != 1 || stats["agent_runs|claude|second|implement|ok"] != 1 ||
				stats["agent_runs|claude|prime|implement|"+c.class] != float64(len(c.ran)-1) {
				t.Fatalf("stats %v", stats)
			}
			st, err := h.e.Status(ctx, core.Caller{Principal: "test"}, ref.String())
			if err != nil {
				t.Fatal(err)
			}
			if m := st.Models.Implement; m.Model != "prime" || m.Ran != "second" || !m.Fallback || m.FallbackReason != c.class {
				t.Fatalf("task_status models %+v", m)
			}
		})
	}
}

// Failures of the install or the work, rather than the model, keep their
// handling: a failed attempt, on the model the run started on.
func TestNoFallbackOnWorkFailures(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"auth-error", "max-turns", "sleep"} {
		t.Run(mode, func(t *testing.T) {
			h := newHEnv(t, yamlChecks+"timeouts:\n  implement: 2s\n")
			h.chain(map[string]string{"prime": mode}, "second")
			ref := h.file("Say hello")
			h.implement(ctx)
			if got := h.ran(); !slices.Equal(got, []string{"prime"}) {
				t.Fatalf("ran on %v, want prime alone", got)
			}
			if h.failedAttempts(ref) != 1 || len(h.failovers(ref)) != 0 || !h.limited("prime").IsZero() {
				t.Fatalf("want one failed attempt and no fallback:\n%s", h.comments(ref))
			}
		})
	}
}

// Once every model of the chain is out, the run backs off as a usage limit
// does today: the fleet holds until the first is back, and the job is parked
// without a failed attempt.
func TestFallbackChainExhausted(t *testing.T) {
	ctx := context.Background()
	h := newHEnv(t, yamlChecks)
	h.chain(map[string]string{"prime": "ratelimit", "second": "overloaded"}, "second")
	ref := h.file("Say hello")
	if err := h.implement(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.ran(); !slices.Equal(got, []string{"prime", "second", "second", "second"}) {
		t.Fatalf("ran on %v", got)
	}
	if s := h.state(ref); s != core.LReady || h.failedAttempts(ref) != 0 {
		t.Fatalf("want back to ready without a failure, got %q\n%s", s, h.comments(ref))
	}
	// second's overload passes sooner than prime's limit, which resets at 6pm.
	hold, _ := h.e.Q.Hold(ctx)
	if hold.IsZero() || hold.After(time.Now().Add(overloadedFor+time.Minute)) {
		t.Fatalf("hold %v, want until the overload is over", hold)
	}
	if j := h.parked(ref); j.RateLimitN != 1 {
		t.Fatalf("parked %+v", j)
	}
	// Past backoff.max_retries in a row, it stops as before; models already
	// out aren't run again meanwhile.
	h.e.Cfg.Backoff.MaxRetries = 1
	zs, _ := h.e.Q.R.ZRange(ctx, "devbot:q:delayed", 0, -1).Result()
	for _, m := range zs {
		h.e.Q.R.ZAdd(ctx, "devbot:q:delayed", redis.Z{Score: 0, Member: m})
	}
	h.e.Q.ReleaseDue(ctx)
	h.e.Q.R.Del(ctx, "devbot:ratelimit:until")
	if err := h.implement(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.ran(); len(got) != 0 {
		t.Fatalf("models still out ran: %v", got)
	}
	if s := h.state(ref); s != core.LNeedsHuman {
		t.Fatalf("want needs-human past max_retries, got %q\n%s", s, h.comments(ref))
	}
}

// A model one task found out is passed over by the next task, which goes
// straight to the fallback; once its reset passes, runs are back on it.
func TestLimitedModelSkippedAcrossTasks(t *testing.T) {
	ctx := context.Background()
	h := newHEnv(t, yamlChecks)
	h.chain(map[string]string{"prime": "ratelimit-soon"}, "second")
	first := h.file("Say hello")
	if err := h.implement(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.ran(); !slices.Equal(got, []string{"prime", "second"}) {
		t.Fatalf("ran on %v", got)
	}
	reset := h.limited("prime")
	if !reset.After(time.Now()) || reset.After(time.Now().Add(3*time.Second)) {
		t.Fatalf("prime out until %v, want the reset it gave", reset)
	}
	next := h.file("Say hello again")
	if err := h.implement(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.ran(); !slices.Equal(got, []string{"second"}) {
		t.Fatalf("ran on %v, want prime passed over", got)
	}
	if fo := h.failovers(next); len(fo) != 1 || fo[0].Model != "prime" || fo[0].Mode != "rate_limited" || !strings.Contains(h.comments(next), "out until") {
		t.Fatalf("a model passed over is said too: %+v\n%s", fo, h.comments(next))
	}
	time.Sleep(time.Until(reset) + 100*time.Millisecond)
	h.models(nil)
	third := h.file("Say hello once more")
	if err := h.implement(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.ran(); !slices.Equal(got, []string{"prime"}) {
		t.Fatalf("ran on %v, want prime back after its reset", got)
	}
	if len(h.failovers(third)) != 0 || len(h.failovers(first)) != 1 {
		t.Fatal("a run on the primary says nothing")
	}
	if st, _ := h.e.Status(ctx, core.Caller{Principal: "test"}, third.String()); st.Models.Implement.Ran != "prime" || st.Models.Implement.Fallback {
		t.Fatalf("task_status %+v", st.Models.Implement)
	}
}

// A fallback repos.limits.models doesn't allow is passed over, not fatal.
func TestFallbackSkipsDisallowedModels(t *testing.T) {
	ctx := context.Background()
	h := newHEnv(t, yamlChecks)
	h.chain(map[string]string{"prime": "unavailable"}, "banned", "second")
	h.e.Cfg.Repos.Limits.Models = []string{"prime", "second"}
	ref := h.file("Say hello")
	if err := h.implement(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.ran(); !slices.Equal(got, []string{"prime", "second"}) {
		t.Fatalf("ran on %v, want banned skipped", got)
	}
	if s := h.state(ref); s != core.LInReview {
		t.Fatalf("got %q\n%s", s, h.comments(ref))
	}
}

// The fallback starts from the work the run that hit the limit left, kept
// as a backoff keeps it: committed, so the next model carries on from it.
func TestFallbackCarriesTheWork(t *testing.T) {
	ctx := context.Background()
	h := newHEnv(t, yamlChecks)
	h.chain(map[string]string{"prime": "ratelimit-dirty", "second": "done-clean"}, "second")
	ref := h.file("Say hello")
	if err := h.implement(ctx); err != nil {
		t.Fatal(err)
	}
	if s := h.state(ref); s != core.LInReview {
		t.Fatalf("got %q\n%s", s, h.comments(ref))
	}
	if b, err := os.ReadFile(h.modeFile + ".dirty"); err != nil || strings.TrimSpace(string(b)) != "" {
		t.Fatalf("the fallback found the work uncommitted: %q %v", b, err)
	}
	pr := h.onlyPR()
	if files := git(t, h.bare, "ls-tree", "--name-only", pr.HeadBranch); !strings.Contains(files, "half.txt") || !strings.Contains(files, "hello.txt") {
		t.Fatalf("the PR has both runs' work: %s", files)
	}
}

// Said once per task and role: a later round that falls back again adds
// nothing, and the reviewer's fallback is said apart from the implementer's.
func TestFallbackSaidOncePerRole(t *testing.T) {
	ctx := context.Background()
	h := newHEnv(t, yamlChecks)
	h.chain(map[string]string{"prime": "unavailable", "second": "done"}, "second")
	ref := h.file("Say hello")
	if err := h.implement(ctx); err != nil {
		t.Fatal(err)
	}
	pr := h.onlyPR()
	sha := git(t, h.bare, "rev-parse", pr.HeadBranch)
	h.f.SetHead(repo, pr.Number, sha)
	h.f.SetStatus(sha, "ci", "success")
	if _, err := h.e.EvaluateItem(ctx, ref, core.SrcWebhook); err != nil {
		t.Fatal(err)
	}
	h.models(map[string]string{"prime": "ratelimit", "second": "approve"})
	h.e.Q.R.Del(ctx, "devbot:model:limit:"+queue.ModelKey("claude", "", "prime"))
	if err := h.review(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.ran(); !slices.Equal(got[len(got)-2:], []string{"prime", "second"}) {
		t.Fatalf("ran on %v", got)
	}
	fo := h.failovers(ref)
	if len(fo) != 2 || fo[1].Role != "review" || fo[1].Mode != "rate_limited" {
		t.Fatalf("failovers %+v\n%s", fo, h.comments(ref))
	}
	// A fix round that falls back again.
	it, _ := h.f.GetItem(ctx, ref)
	if err := h.e.Transition(ctx, it, core.LChanges, "", ""); err != nil {
		t.Fatal(err)
	}
	h.e.Q.Enqueue(ctx, queue.Job{Kind: queue.KindImplement, Ref: ref.String(), Mode: "fix", PR: pr.Number, Reason: "rebase"})
	h.models(map[string]string{"prime": "unavailable", "second": "rebase"})
	h.e.Q.R.Del(ctx, "devbot:model:limit:"+queue.ModelKey("claude", "", "prime"))
	if err := h.implement(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.ran(); !slices.Equal(got, []string{"prime", "second"}) {
		t.Fatalf("ran on %v", got)
	}
	if fo := h.failovers(ref); len(fo) != 2 {
		t.Fatalf("a later fallback is said again: %+v", fo)
	}
}

// Without fallbacks a role runs on its one model, as before: a usage limit
// backs off, a model the provider doesn't serve is a failed attempt.
func TestNoFallbacksConfigured(t *testing.T) {
	ctx := context.Background()
	h := newHEnv(t, yamlChecks)
	h.chain(map[string]string{"prime": "unavailable"})
	ref := h.file("Say hello")
	h.implement(ctx)
	if got := h.ran(); !slices.Equal(got, []string{"prime"}) {
		t.Fatalf("ran on %v", got)
	}
	if h.failedAttempts(ref) != 1 || len(h.failovers(ref)) != 0 {
		t.Fatalf("want a failed attempt:\n%s", h.comments(ref))
	}
}

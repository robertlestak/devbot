package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/agent/agenttest"
	"git.shdw.tech/rob/devbot/internal/agent/codex"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// fakeCodex stands in for codex exec --json: a first run leaves work whose
// checks fail, its resume fixes it, and a review approves. It prints a
// JSONL stream and writes the -o file the way the real CLI does.
const fakeCodex = `out=; resume=
for a in "$@"; do
  [ "$prev" = -o ] && out=$a
  [ "$a" = resume ] && resume=1
  prev=$a
done
cat >/dev/null
echo '{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}'
echo '{"type":"turn.started"}'
case "$(cat "$CODEX_FAKE_MODE")" in
implement)
  if [ -n "$resume" ]; then
    echo fixed > fixed.txt && git add fixed.txt && git commit -qm "fix the check"
    msg="Fixed the check."
  else
    echo hello > hello.txt && git add hello.txt && git commit -qm "add hello"
    msg="Added hello.txt."
  fi
  printf '%s' '{"status":"done","summary":"Added hello.txt","pr_title":"Add hello"}' > .devbot/result.json
  ;;
approve)
  printf '%s' '{"verdict":"approve","summary":"Looks right"}' > .devbot/result.json
  msg="Approved."
  ;;
esac
echo '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"'"$msg"'"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":800,"output_tokens":50,"reasoning_output_tokens":20}}'
printf '%s' "$msg" > "$out"
`

// The whole flow with harness: codex - implement, checks that fail, a fix
// round resuming the Codex session, the PR, and a review - against a fake
// CLI.
func TestCodexHarnessFlow(t *testing.T) {
	h := newHEnv(t, "version: 1\nenabled: true\nchecks:\n  - test -f fixed.txt\n  - test -z \"$CODEX_API_KEY\"\n")
	mode := filepath.Join(t.TempDir(), "mode")
	cli := agenttest.NewCLI(t, "codex", strings.ReplaceAll(fakeCodex, "$CODEX_FAKE_MODE", mode))
	// Were the sidecar to have the key, neither codex nor the checks get it.
	t.Setenv(codex.APIKeyEnv, "sk-codex")
	h.r.Agent = &agent.Local{Harnesses: map[string]agent.Harness{
		codex.Name: &codex.Harness{Command: cli.Path, Proxy: testProxy, HomeDir: t.TempDir()},
	}}
	h.e.Cfg.Agents = config.Agents{Implement: config.Agent{Harness: "codex", Model: "gpt-6.1-sol"}, Review: config.Agent{Harness: "codex", Model: "gpt-6.1-sol"}}

	os.WriteFile(mode, []byte("implement"), 0o644)
	ref := h.file("Say hello")
	if err := h.implement(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := h.state(ref); s != core.LInReview {
		t.Fatalf("want in-review, got %q\n%s", s, h.comments(ref))
	}
	calls := cli.Calls(t)
	if len(calls) != 2 {
		t.Fatalf("want a run and one fix round, got %d calls", len(calls))
	}
	first, fix := calls[0].Args, calls[1].Args
	if first[0] != "exec" || first[1] != "--json" || !slices.Contains(first, "danger-full-access") {
		t.Fatalf("first run %q", first)
	}
	if fix[1] != "resume" || !slices.Contains(fix, "0199a213-81c0-7800-8aa1-bbab2a035a53") ||
		!slices.Contains(fix, `sandbox_mode="danger-full-access"`) || !slices.Contains(fix, "gpt-6.1-sol") || !slices.Contains(fix, "-o") {
		t.Fatalf("the fix round must resume the session with its flags: %q", fix)
	}
	if !strings.Contains(calls[1].Stdin, "The checks failed") {
		t.Fatalf("fix prompt %q", calls[1].Stdin)
	}
	for _, c := range calls {
		if v, _ := c.Getenv(codex.APIKeyEnv); v != testProxy.Token {
			t.Fatalf("codex runs with CODEX_API_KEY=%q, not the placeholder", v)
		}
		if _, ok := c.Getenv("DEVBOT_IMPLEMENTER_TOKEN"); ok {
			t.Fatal("a forge credential reached codex")
		}
	}
	prs, _ := h.f.ListOpenPRs(context.Background(), repo)
	if len(prs) != 1 || !strings.Contains(git(t, h.bare, "ls-tree", "--name-only", prs[0].HeadBranch), "fixed.txt") {
		t.Fatalf("want one PR with the fix round's commit, got %+v", prs)
	}
	stats, _ := h.e.Q.Stats(context.Background())
	for k, want := range map[string]float64{
		"agent_runs|codex|gpt-6.1-sol|implement|ok":                                        2,
		"agent_tokens|codex|gpt-6.1-sol|implement|" + repo.String() + "|acme|input":        2000,
		"agent_tokens|codex|gpt-6.1-sol|implement|" + repo.String() + "|acme|cached_input": 1600,
		"agent_tokens|codex|gpt-6.1-sol|implement|" + repo.String() + "|acme|reasoning":    40,
	} {
		if got := stats[k]; got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	if _, ok := stats["agent_cost_usd|codex|gpt-6.1-sol|implement|"+repo.String()+"|acme"]; ok {
		t.Error("codex reports no cost, so there's no cost series")
	}

	sha := git(t, h.bare, "rev-parse", prs[0].HeadBranch)
	h.f.SetHead(repo, prs[0].Number, sha)
	h.f.SetStatus(sha, "ci", "success")
	if _, err := h.e.EvaluateItem(context.Background(), ref, core.SrcWebhook); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(mode, []byte("approve"), 0o644)
	if err := h.review(context.Background()); err != nil {
		t.Fatal(err)
	}
	rv, _ := h.f.Reviews(context.Background(), repo, prs[0].Number)
	if len(rv) != 1 || rv[0].Verdict != forge.VerdictApprove {
		t.Fatalf("want an approval, got %+v", rv)
	}
}

package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/agent/agenttest"
	"git.shdw.tech/rob/devbot/internal/agent/codex"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/queue"
)

func TestDropAIAttribution(t *testing.T) {
	for in, want := range map[string]string{
		"add hello":                          "add hello",
		"add hello\n\nGenerated with protoc": "add hello\n\nGenerated with protoc",
		"add hello\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\nCo-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>\n": "add hello",
		"add hello\n\nCo-authored-by: Alice <alice@example.com>\nCo-authored-by: Codex <noreply@openai.com>\nSigned-off-by: Bob <bob@example.com>": "add hello\n\nCo-authored-by: Alice <alice@example.com>\nSigned-off-by: Bob <bob@example.com>",
		"Co-authored-by: Claude <claude@anthropic.com>": "Co-authored-by: Claude <claude@anthropic.com>",
	} {
		if got := dropAIAttribution(in); got != want {
			t.Errorf("dropAIAttribution(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDropSelfCoAuthor(t *testing.T) {
	for in, want := range map[string]string{
		"add hello": "add hello",
		"add hello\n\nCo-authored-by: devbot <devbot@devbot.example>":                                                "add hello",
		"add hello\n\nCO-AUTHORED-BY: DevBot <DEVBOT@Devbot.example>\nSigned-off-by: devbot <devbot@devbot.example>": "add hello\n\nSigned-off-by: devbot <devbot@devbot.example>",
		"add hello\n\nCo-authored-by: Alice <alice@example.com>\nCo-authored-by: devbot <devbot@devbot.example>":     "add hello\n\nCo-authored-by: Alice <alice@example.com>",
		"add hello\n\nCo-authored-by: devbot <devbot@example.com>":                                                   "add hello\n\nCo-authored-by: devbot <devbot@example.com>",
		"add hello\n\nCo-Authored-By: Claude <noreply@anthropic.com>":                                                "add hello\n\nCo-Authored-By: Claude <noreply@anthropic.com>",
	} {
		if got := dropSelfCoAuthor(in, "devbot@devbot.example"); got != want {
			t.Errorf("dropSelfCoAuthor(%q) = %q, want %q", in, got, want)
		}
	}
}

// messages are the commit messages on branch that main doesn't have, oldest
// first, a "---" line after each.
func messages(t *testing.T, bare, branch string) string {
	t.Helper()
	return git(t, bare, "log", "--reverse", "--format=%B---", "main.."+branch)
}

// The pushed commits, the PR devbot opens and the agent CLI's own settings
// follow commits.co_authored_by: off drops the agent's co-author trailer and
// "Generated with" line and keeps a person's; on keeps the agent's trailer,
// or adds it; unset passes everything through as the agent wrote it. Whatever
// the setting, a co-author trailer naming devbot, the commits' own author,
// goes.
func TestCommitsCoAuthoredBy(t *testing.T) {
	const agents = "🤖 Generated with [Claude Code](https://claude.com/claude-code)"
	const anthropic = "<noreply@anthropic.com>"
	for _, c := range []struct {
		name, yaml, mode string
		settings         string // the --settings file Claude Code got
		check            func(t *testing.T, msgs, body string)
	}{
		{"off", "commits: {co_authored_by: false}\n", "attributed", `{"attribution":{"commit":"","pr":"","sessionUrl":false},"includeCoAuthoredBy":false}`,
			func(t *testing.T, msgs, body string) {
				if strings.Contains(msgs, anthropic) || strings.Contains(msgs, "Generated with") || !strings.Contains(msgs, "Co-authored-by: Alice <alice@example.com>") {
					t.Errorf("commits:\n%s", msgs)
				}
				if strings.Contains(body, "Generated with") || strings.Contains(body, anthropic) || !strings.Contains(body, "Added hello.txt") {
					t.Errorf("PR body:\n%s", body)
				}
			}},
		{"on, the agent's", "commits: {co_authored_by: true}\n", "attributed", `{"includeCoAuthoredBy":true}`,
			func(t *testing.T, msgs, body string) {
				if strings.Count(msgs, anthropic) != 1 || !strings.Contains(msgs, "Co-Authored-By: Claude Opus 5.5 "+anthropic) || !strings.Contains(msgs, "Alice") {
					t.Errorf("commits:\n%s", msgs)
				}
			}},
		{"on, added", "commits: {co_authored_by: true}\n", "done", `{"includeCoAuthoredBy":true}`,
			func(t *testing.T, msgs, body string) {
				if msgs != "add hello\n\nCo-Authored-By: Claude "+anthropic+"\n---" {
					t.Errorf("commits:\n%s", msgs)
				}
			}},
		{"unset", "", "attributed", "",
			func(t *testing.T, msgs, body string) {
				want := "add hello\n\n" + agents + "\n\nCo-authored-by: Alice <alice@example.com>\nCo-Authored-By: Claude Opus 5.5 " + anthropic + "\n---"
				if msgs != want {
					t.Errorf("commits:\n%s\nwant\n%s", msgs, want)
				}
				if !strings.Contains(body, agents) {
					t.Errorf("PR body:\n%s", body)
				}
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHEnv(t, yamlChecks+c.yaml)
			var settings []byte
			h.r.Agent = &spy{Runner: h.r.Agent, after: func(dir string) { settings, _ = os.ReadFile(filepath.Join(dir, ".devbot", "seen-settings.json")) }}
			h.mode(c.mode)
			ref := h.file("Say hello")
			if err := h.implement(context.Background()); err != nil {
				t.Fatal(err)
			}
			prs, _ := h.f.ListOpenPRs(context.Background(), repo)
			if len(prs) != 1 {
				t.Fatalf("want a PR, got %+v\n%s", prs, h.comments(ref))
			}
			if string(settings) != c.settings {
				t.Errorf("Claude Code's settings %s, want %s", settings, c.settings)
			}
			msgs := messages(t, h.bare, prs[0].HeadBranch)
			if strings.Contains(strings.ToLower(msgs), "devbot@example.com") {
				t.Errorf("devbot credited as its own co-author:\n%s", msgs)
			}
			c.check(t, msgs, prs[0].Body)
		})
	}
}

// fakeCodexCommit stands in for codex exec: it commits with its own
// co-author trailer when the codex_git_commit feature is on, as
// Codex does.
const fakeCodexCommit = `out=; trailer=
for a in "$@"; do
  [ "$prev" = -o ] && out=$a
  [ "$a" = features.codex_git_commit=true ] && trailer=1
  prev=$a
done
cat >/dev/null
echo hello > hello.txt && git add hello.txt
if [ -n "$trailer" ]; then git commit -qm "add hello" -m "Co-authored-by: Codex <noreply@openai.com>"
else git commit -qm "add hello" -m "Co-authored-by: Codex <noreply@openai.com>" -m "$CODEX_FAKE_EXTRA"; fi
printf '%s' '{"status":"done","summary":"Added hello.txt"}' > .devbot/result.json
echo '{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}'
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
printf done > "$out"
`

// With Codex, off drops Codex's trailer even when the CLI ignored its
// setting, and on has it once.
func TestCommitsCoAuthoredByCodex(t *testing.T) {
	for _, c := range []struct {
		yaml, want string
	}{
		{"commits: {co_authored_by: false}\n", "add hello\n\nignored\n---"},
		{"commits: {co_authored_by: true}\n", "add hello\n\nCo-authored-by: Codex <noreply@openai.com>\n---"},
	} {
		h := newHEnv(t, yamlChecks+c.yaml)
		t.Setenv("MISE_CODEX_FAKE_EXTRA", "ignored")
		cli := agenttest.NewCLI(t, "codex", strings.ReplaceAll(fakeCodexCommit, "$CODEX_FAKE_EXTRA", "$MISE_CODEX_FAKE_EXTRA"))
		h.r.Agent = &agent.Local{Harnesses: map[string]agent.Harness{codex.Name: &codex.Harness{Command: cli.Path, Proxy: testProxy, HomeDir: t.TempDir()}}}
		h.e.Cfg.Agents = config.Agents{Implement: config.Agent{Harness: "codex"}, Review: config.Agent{Harness: "codex"}}
		ref := h.file("Say hello")
		if err := h.implement(context.Background()); err != nil {
			t.Fatal(err)
		}
		prs, _ := h.f.ListOpenPRs(context.Background(), repo)
		if len(prs) != 1 {
			t.Fatalf("want a PR\n%s", h.comments(ref))
		}
		if got := messages(t, h.bare, prs[0].HeadBranch); got != c.want {
			t.Errorf("%s: commits:\n%s\nwant\n%s", c.yaml, got, c.want)
		}
	}
}

// commits.trailers are on every commit devbot pushes exactly once: one the
// agent already added isn't doubled, and a fix round and a rebase leave
// them so, without rewriting commits that already had them.
func TestCommitsTrailersOncePerCommit(t *testing.T) {
	const sob, ticket = "Signed-off-by: devbot <devbot@example.com>", "Ticket: APP-1"
	h := newHEnv(t, yamlChecks+"commits: {trailers: [\""+sob+"\", \""+ticket+"\"]}\n")
	ctx := context.Background()
	h.mode("signed")
	ref := h.file("Say hello")
	if err := h.implement(ctx); err != nil {
		t.Fatal(err)
	}
	prs, _ := h.f.ListOpenPRs(ctx, repo)
	if len(prs) != 1 {
		t.Fatalf("want a PR\n%s", h.comments(ref))
	}
	branch := prs[0].HeadBranch
	once := func(when string, n int) {
		t.Helper()
		msgs := strings.Split(strings.TrimSuffix(messages(t, h.bare, branch), "---"), "---")
		if len(msgs) != n {
			t.Fatalf("%s: want %d commits, got %q", when, n, msgs)
		}
		for _, m := range msgs {
			if strings.Count(m, sob) != 1 || strings.Count(m, ticket) != 1 {
				t.Errorf("%s: commit %q doesn't have each trailer once", when, m)
			}
		}
	}
	once("first push", 1)
	first := git(t, h.bare, "rev-parse", branch)

	round := func(mode string) {
		t.Helper()
		it, _ := h.f.GetItem(ctx, ref)
		h.e.Transition(ctx, it, core.LChanges, "", "")
		h.e.Q.Enqueue(ctx, queue.Job{Kind: queue.KindImplement, Ref: ref.String(), Mode: "fix", PR: prs[0].Number, Reason: "say it twice"})
		h.mode(mode)
		if err := h.implement(ctx); err != nil {
			t.Fatal(err)
		}
	}
	round("fix-context")
	once("fix round", 2)
	if git(t, h.bare, "rev-parse", branch+"^") != first {
		t.Error("the fix round rewrote the commit already pushed")
	}

	// main moves on; the round rebases onto it, and adds a commit.
	seed := filepath.Join(t.TempDir(), "seed")
	git(t, filepath.Dir(seed), "clone", "-q", h.bare, seed)
	os.WriteFile(filepath.Join(seed, "MAIN.md"), []byte("moved\n"), 0o644)
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-qm", "main moves")
	git(t, seed, "push", "-q", "origin", "main")
	round("rebase")
	once("rebase", 3)
	if git(t, h.bare, "merge-base", branch, "main") != git(t, h.bare, "rev-parse", "main") {
		t.Error("the rebase didn't reach the branch")
	}
}

// A carried PR's author's commits keep their messages; devbot's own on top
// of them get the trailers, pushed without rewriting the author's.
func TestCommitsTrailersLeaveAPersonsCommits(t *testing.T) {
	const sob = "Signed-off-by: devbot <devbot@example.com>"
	h := newHEnv(t, yamlChecks+"commits: {co_authored_by: false, trailers: [\""+sob+"\"]}\n")
	ref, pr := h.carried()
	bobs := pr.HeadSHA
	h.mode("done")
	if err := h.implement(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := h.state(ref); s != core.LInReview {
		t.Fatalf("want in-review, got %q\n%s", s, h.comments(ref))
	}
	if git(t, h.bare, "rev-parse", "feature^") != bobs {
		t.Fatal("bob's commit was rewritten")
	}
	if got := git(t, h.bare, "log", "-1", "--format=%B", "feature"); got != "add hello\n\n"+sob {
		t.Errorf("devbot's commit:\n%s", got)
	}
	if prThread := h.comments(forge.ItemRef{Repo: repo, Number: pr.Number}); strings.Contains(prThread, "force-pushed") {
		t.Errorf("the push rewrote the carried branch:\n%s", prThread)
	}
}

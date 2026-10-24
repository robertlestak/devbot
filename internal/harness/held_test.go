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

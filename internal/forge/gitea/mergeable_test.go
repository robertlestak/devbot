package gitea

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/changeset"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/forgetest"
)

// GetPR tells Gitea's false apart: a WIP title isn't a conflict, a false
// that turns true on a second read is mergeable, and one that stays false
// is a conflict.
func TestGetPRMergeable(t *testing.T) {
	for _, tc := range []struct {
		name               string
		title              string
		conflicted         bool
		checkReads         int
		mergeable, unknown bool
		wantReads          int
	}{
		{name: "clean", title: "t", mergeable: true, wantReads: 1},
		{name: "WIP:", title: "WIP: t", mergeable: true, wantReads: 1},
		{name: "[wip]", title: "[wip] t", mergeable: true, wantReads: 1},
		{name: "WIP conflicting", title: "wip: t", conflicted: true, mergeable: true, wantReads: 1},
		{name: "checking, then clean", title: "t", checkReads: 1, mergeable: true, wantReads: 2},
		{name: "conflict", title: "t", conflicted: true, mergeable: false, wantReads: 2},
		{name: "WIP in the middle", title: "t WIP: t", conflicted: true, mergeable: false, wantReads: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitea(t)
			fx := f.fixture(t)
			fx.Branch("feature", "f00d")
			n := fx.PR(forgetest.PRSeed{Author: "devbot", Title: tc.title, Head: "feature", HeadSHA: "f00d", Base: "main", MergeBase: "ba5e", Conflicted: tc.conflicted})
			f.mu.Lock()
			f.issues[n].checkReads = tc.checkReads
			f.calls = 0
			f.mu.Unlock()
			p, err := fx.P.GetPR(context.Background(), fx.Repo, n)
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			calls := f.calls
			f.mu.Unlock()
			// One more call reads the base branch, for Behind.
			if p.Mergeable != tc.mergeable || p.MergeableUnknown != tc.unknown || calls != tc.wantReads+1 {
				t.Fatalf("got mergeable %v, unknown %v in %d calls; want %v, %v in %d", p.Mergeable, p.MergeableUnknown, calls, tc.mergeable, tc.unknown, tc.wantReads+1)
			}
		})
	}
}

// A false in a list isn't read again, so it is unknown, as GitHub's lists
// are; a WIP title's false is no conflict.
func TestListedFalseIsUnknown(t *testing.T) {
	f := newFakeGitea(t)
	fx := f.fixture(t)
	for _, b := range []string{"a", "b", "c"} {
		fx.Branch(b, b+"0")
	}
	clean := fx.PR(forgetest.PRSeed{Author: "devbot", Title: "t", Head: "a", HeadSHA: "a0", Base: "main", MergeBase: "ba5e"})
	conflict := fx.PR(forgetest.PRSeed{Author: "devbot", Title: "t", Head: "b", HeadSHA: "b0", Base: "main", MergeBase: "ba5e", Conflicted: true})
	draft := fx.PR(forgetest.PRSeed{Author: "devbot", Title: "[WIP] t", Head: "c", HeadSHA: "c0", Base: "main", MergeBase: "ba5e"})
	prs, err := fx.P.ListOpenPRs(context.Background(), fx.Repo)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int][2]bool{clean: {true, false}, conflict: {true, true}, draft: {true, false}}
	for _, p := range prs {
		if w := want[p.Number]; p.Mergeable != w[0] || p.MergeableUnknown != w[1] {
			t.Errorf("PR #%d: got mergeable %v, unknown %v; want %v, %v", p.Number, p.Mergeable, p.MergeableUnknown, w[0], w[1])
		}
	}
}

// approvedEnv is the engine against the fake Gitea with an approved,
// green PR on its task, titled title, and the repo's config yaml.
func approvedEnv(t *testing.T, yaml, title string) (*timelineEnv, int) {
	v := newTimelineEnv(t)
	v.fx.File("main", ".devbot.yaml", []byte(yaml))
	v.e.Changes = func(context.Context, forge.Provider, forge.PR) ([]changeset.Change, error) {
		return []changeset.Change{{Status: "M", Path: "main.go", OldMode: "100644", NewMode: "100644"}}, nil
	}
	at := now().Add(-time.Hour)
	branch := fmt.Sprintf("devbot/%d-t", v.ref.Number)
	v.fx.Branch(branch, "sha1")
	pr := v.fx.PR(forgetest.PRSeed{Author: "devbot", Title: title, Body: core.RenderPRMeta(core.PRMeta{Issue: v.ref.Number}),
		Head: branch, HeadSHA: "sha1", Base: "main", MergeBase: "ba5e"})
	v.label("alice", core.LReady, true, at)
	v.label("devbot", core.LReady, false, at)
	v.label("devbot", core.LInReview, true, at)
	v.comment("opened "+core.RenderStatus(core.Status{Kind: "pr", PR: pr, Base: "main"}), at)
	v.fx.Status("sha1", "ci", "success")
	v.f.mu.Lock()
	v.f.issues[pr].reviews = append(v.f.issues[pr].reviews, fakeReview{user: "devbot-review", state: "APPROVED", sha: "sha1", at: now()})
	v.f.mu.Unlock()
	return v, pr
}

func (v *timelineEnv) labels(t *testing.T) []string {
	t.Helper()
	it, err := v.fx.P.GetItem(context.Background(), v.ref)
	if err != nil {
		t.Fatal(err)
	}
	return it.Labels
}

// noConflictSaid fails t if any comment on the task says its PR conflicts
// or asks for a rebase.
func (v *timelineEnv) noConflictSaid(t *testing.T) {
	t.Helper()
	for _, c := range v.comments(t) {
		if strings.Contains(c.Body, "conflicts") || strings.Contains(c.Body, `"mode":"rebase"`) {
			t.Fatalf("a comment says the PR conflicts or asks for a rebase: %q", c.Body)
		}
	}
	if slices.Contains(v.labels(t), core.LChanges) {
		t.Fatalf("the PR was sent back: labels %v", v.labels(t))
	}
}

// A PR titled as a work in progress, which Gitea calls unmergeable whatever
// it holds, isn't rebased as if it conflicted. Gitea refuses its merge, and
// a person hears why.
func TestWIPTitleIsNoConflict(t *testing.T) {
	for _, title := range []string{"WIP: t", "[wip] t"} {
		t.Run(title, func(t *testing.T) {
			v, _ := approvedEnv(t, "version: 1\nenabled: true\nautomerge: true\nrebase: eager\n", title)
			v.eval(t)
			v.noConflictSaid(t)
			cs := v.comments(t)
			if l := v.labels(t); !slices.Contains(l, core.LNeedsHuman) || !strings.Contains(cs[len(cs)-1].Body, "work in progress") {
				t.Fatalf("want the refused merge handed to a person, labels %v, last comment %q", l, cs[len(cs)-1].Body)
			}
			if v.eval(t) || len(v.comments(t)) != len(cs) {
				t.Fatal("second evaluation must be a no-op")
			}
		})
	}
}

// Gitea still checking a PR just after a push reads false, then true: the
// PR merges, and nothing says it conflicted.
func TestCheckingThenMergeableMerges(t *testing.T) {
	v, pr := approvedEnv(t, "version: 1\nenabled: true\nautomerge: true\nrebase: eager\n", "t")
	v.f.mu.Lock()
	v.f.issues[pr].checkReads = 1
	v.f.mu.Unlock()
	v.eval(t)
	v.noConflictSaid(t)
	p, err := v.fx.P.GetPR(context.Background(), v.fx.Repo, pr)
	if err != nil || !p.Merged {
		t.Fatalf("want the PR merged, got %+v %v", p, err)
	}
	if n := len(v.comments(t)); v.eval(t) || len(v.comments(t)) != n {
		t.Fatal("second evaluation must be a no-op")
	}
}

// A false that stays false is still a conflict: the PR is rebased.
func TestLastingFalseIsAConflict(t *testing.T) {
	v, pr := approvedEnv(t, "version: 1\nenabled: true\nautomerge: true\nrebase: eager\n", "t")
	v.f.mu.Lock()
	v.f.issues[pr].conflicted = true
	v.f.mu.Unlock()
	v.eval(t)
	if l := v.labels(t); !slices.Contains(l, core.LChanges) {
		t.Fatalf("want the conflicting PR sent back to be rebased, labels %v", l)
	}
}

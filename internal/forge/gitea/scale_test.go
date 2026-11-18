package gitea

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/forgetest"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// Every list is read to its end, however long: past each length devbot
// once stopped at, nothing is left out.
func TestListsHaveNoCeiling(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		n    int
		add  func(f *fakeGitea, pr int, n int)
		read func(p forge.Provider, ref forge.ItemRef) (int, error)
	}{
		{"timeline label events", 2050,
			func(f *fakeGitea, pr int, n int) {
				bug, _ := f.label(f.labelID("bug"))
				for range n {
					f.issues[pr].timeline = append(f.issues[pr].timeline, labelEvent{Type: "label", Body: "1", User: user{Login: "alice"}, Label: bug, CreatedAt: now()})
				}
			},
			func(p forge.Provider, ref forge.ItemRef) (int, error) {
				l, err := p.LabelEvents(ctx, ref)
				return len(l), err
			}},
		{"timeline title edits", 2050,
			func(f *fakeGitea, pr int, n int) {
				for range n {
					f.issues[pr].timeline = append(f.issues[pr].timeline, labelEvent{Type: "change_title", User: user{Login: "alice"}, CreatedAt: now()})
				}
			},
			func(p forge.Provider, ref forge.ItemRef) (int, error) {
				l, err := p.ContentEdits(ctx, ref)
				return len(l), err
			}},
		{"reviews", 1050,
			func(f *fakeGitea, pr int, n int) {
				for range n {
					f.issues[pr].reviews = append(f.issues[pr].reviews, fakeReview{user: "devbot-review", state: "COMMENT", sha: "abc", at: now()})
				}
			},
			func(p forge.Provider, ref forge.ItemRef) (int, error) {
				l, err := p.Reviews(ctx, ref.Repo, ref.Number)
				return len(l), err
			}},
		{"open PRs", 1050,
			func(f *fakeGitea, _ int, n int) {
				for range n {
					f.next++
					f.issues[f.next] = &fakeIssue{number: f.next, title: "t", author: "bob", state: "open", isPR: true,
						head: fmt.Sprintf("b%d", f.next), headSHA: "abc", base: "main", mergeBase: "ba5e", created: now(), updated: now()}
				}
			},
			func(p forge.Provider, ref forge.ItemRef) (int, error) {
				l, err := p.ListOpenPRs(ctx, ref.Repo)
				return len(l) - 1, err
			}},
		{"labelled issues in a search", 2550,
			func(f *fakeGitea, _ int, n int) {
				id := f.labelID(core.LWorking)
				for range n {
					f.next++
					f.issues[f.next] = &fakeIssue{number: f.next, title: "t", author: "alice", state: "open", labels: []int64{id}, created: now(), updated: now()}
				}
			},
			func(p forge.Provider, ref forge.ItemRef) (int, error) {
				l, err := p.SearchItems(ctx, core.LWorking, "open")
				return len(l), err
			}},
		{"repos", 2550,
			func(f *fakeGitea, _ int, n int) { f.moreRepos = n },
			func(p forge.Provider, ref forge.ItemRef) (int, error) {
				l, err := p.ListRepos(ctx)
				return len(l) - 1, err // acme/private is read-only
			}},
		{"branches", 5050,
			func(f *fakeGitea, _ int, n int) {
				for i := range n {
					f.branches[fmt.Sprintf("devbot/%d-x", i)] = "abc"
					f.branches[fmt.Sprintf("people/%d", i)] = "abc"
				}
			},
			func(p forge.Provider, ref forge.ItemRef) (int, error) {
				l, err := p.ListBranches(ctx, ref.Repo, "devbot/")
				return len(l), err
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitea(t)
			fx := f.fixture(t)
			fx.Branch("feature", "abc")
			pr := fx.PR(forgetest.PRSeed{Author: "devbot", Title: "t", Head: "feature", HeadSHA: "abc", Base: "main", MergeBase: "ba5e"})
			f.mu.Lock()
			tc.add(f, pr, tc.n)
			f.mu.Unlock()
			got, err := tc.read(fx.P, forge.ItemRef{Repo: fx.Repo, Number: pr})
			if err != nil || got != tc.n {
				t.Fatalf("got %d (%v), want all %d", got, err, tc.n)
			}
		})
	}
}

// More than 1,000 statuses on a commit are all read: the one failure
// among them, on the last page, fails the commit.
func TestCombinedChecksPastAThousand(t *testing.T) {
	f := newFakeGitea(t)
	fx := f.fixture(t)
	for i := range 1100 {
		fx.Status("abc", fmt.Sprintf("job-%04d", i), "success")
	}
	fx.Status("abc", "zz-last", "failure")
	st, details, err := fx.P.CombinedChecks(context.Background(), fx.Repo, "abc")
	if err != nil || st != forge.ChecksFailure || len(details) != 1101 {
		t.Fatalf("got %s with %d checks (%v), want failure with 1101", st, len(details), err)
	}
}

// A tree of more than 20,000 files is listed whole, a page at a time.
func TestListTreePastTwentyThousand(t *testing.T) {
	f := newFakeGitea(t)
	f.maxItems = 1000
	fx := f.fixture(t)
	f.mu.Lock()
	for d := range 41 {
		for i := range 500 {
			f.files[fmt.Sprintf("main:svc%02d/pkg/f%03d.go", d, i)] = []byte("package pkg\n")
		}
	}
	f.files["main:svc40/compose.yml"] = []byte("services: {}\n")
	f.mu.Unlock()
	files, err := fx.P.ListTree(context.Background(), fx.Repo, "main")
	if err != nil || len(files) != 41*500+1 || !slices.ContainsFunc(files, func(tf forge.TreeFile) bool { return tf.Path == "svc40/compose.yml" }) {
		t.Fatalf("got %d files (%v), want all %d", len(files), err, 41*500+1)
	}
}

// devbot's labels are read by name, so ensuring and applying them costs
// the same on a repo with more labels than 1,000 as on one with none of
// its own; one that drifted is repaired.
func TestLabelsCostTheSameOnABigRepo(t *testing.T) {
	ctx := context.Background()
	cost := func(others int) int {
		f := newFakeGitea(t)
		fx := f.fixture(t)
		f.mu.Lock()
		for i := range others {
			f.labelID(fmt.Sprintf("area/%05d", i))
		}
		f.labels = append(f.labels, label{ID: f.id(), Name: core.LReady, Color: "000000", Description: "old"})
		f.calls = 0
		f.mu.Unlock()
		if err := fx.P.EnsureLabels(ctx, fx.Repo, core.LabelDefs); err != nil {
			t.Fatal(err)
		}
		it, err := fx.P.CreateItem(ctx, fx.Repo, "t", "b", []string{core.LReady})
		if err != nil || !slices.Equal(it.Labels, []string{core.LReady}) {
			t.Fatalf("filed %+v (%v)", it, err)
		}
		if err := fx.P.SetLabels(ctx, it.Ref, []string{core.LWorking}, []string{core.LReady}); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, d := range core.LabelDefs {
			i := slices.IndexFunc(f.labels, func(l label) bool { return l.Name == d.Name })
			if i < 0 || f.labels[i].Color != d.Color || f.labels[i].Description != d.Description {
				t.Fatalf("%s: %+v", d.Name, f.labels)
			}
		}
		if n := len(f.labels); n != others+len(core.LabelDefs) {
			t.Fatalf("%d labels, want %d", n, others+len(core.LabelDefs))
		}
		n := f.calls
		f.calls = 0
		f.mu.Unlock()
		if err := fx.P.EnsureLabels(ctx, fx.Repo, core.LabelDefs); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		if f.calls != len(core.LabelDefs) {
			t.Fatalf("a second run made %d requests, want one read per label", f.calls)
		}
		return n
	}
	if small, big := cost(0), cost(1100); small != big {
		t.Fatalf("%d requests with no other labels, %d with 1,100", small, big)
	}
}

// A label named like a number is one Gitea would read as an ID: it is
// found by its name all the same.
func TestNumericLabelName(t *testing.T) {
	f := newFakeGitea(t)
	fx := f.fixture(t)
	n := fx.Issue("alice", "t", "")
	f.mu.Lock()
	f.labelID("2024")
	f.mu.Unlock()
	ref := forge.ItemRef{Repo: fx.Repo, Number: n}
	if err := fx.P.SetLabels(context.Background(), ref, []string{"2024"}, nil); err != nil {
		t.Fatal(err)
	}
	it, err := fx.P.GetItem(context.Background(), ref)
	if err != nil || !slices.Equal(it.Labels, []string{"2024"}) {
		t.Fatalf("labels %v (%v)", it.Labels, err)
	}
}

// devbot/ready behind more timeline events than devbot once read is still
// found and put down to who added it: the task starts, once.
func TestReadyBehindALongTimelineStarts(t *testing.T) {
	v := newTimelineEnv(t)
	start := now().Add(-3 * time.Hour)
	v.label("alice", core.LReady, true, start)
	v.churn(2050, start.Add(time.Second))
	if !v.eval(t) || v.eval(t) {
		t.Fatal("want the task started once")
	}
	if q, _ := v.e.Q.IsQueued(context.Background(), queue.KindImplement, v.ref.String()); !q {
		t.Fatal("the task should be queued")
	}
}

// file_task on a repo with more than 1,000 labels files the task, labelled.
func TestFileTaskOnARepoWithManyLabels(t *testing.T) {
	v := newTimelineEnv(t)
	v.f.mu.Lock()
	for i := range 1100 {
		v.f.labelID(fmt.Sprintf("area/%05d", i))
	}
	v.f.mu.Unlock()
	st, err := v.e.FileTask(context.Background(), core.FileRequest{Repo: v.fx.Repo.String(), Title: "t", AcceptanceCriteria: "- x"}, core.Caller{Principal: "client"})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := forge.ParseItem(st.Task)
	if err != nil {
		t.Fatal(err)
	}
	if it, err := v.fx.P.GetItem(context.Background(), ref); err != nil || !it.HasLabel(core.LReady) {
		t.Fatalf("filed %+v (%v), want it labelled %s", it, err, core.LReady)
	}
}

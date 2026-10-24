// Package forgetest is the forge.Provider contract, as forge.go documents
// it, written as tests that any adapter runs against a fake of its forge.
package forgetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// Fixture is one adapter wired to a fresh fake of its forge. Repo is a repo
// both bots can write to, with default branch "main". The seeding functions
// change the fake directly, the way people working on the forge would, so
// a case can set up what the Provider itself can't (or can't yet) create.
type Fixture struct {
	P    forge.Provider
	Repo forge.RepoRef
	// NoAccess is a repo on the same forge neither bot can write to.
	NoAccess forge.RepoRef

	// Issue files an issue as author and returns its number.
	Issue func(author, title, body string, labels ...string) int
	// Comment comments on issue or PR n as author.
	Comment func(n int, author, body string)
	// PR opens a pull request and returns its number.
	PR func(p PRSeed) int
	// Edit changes issue n's title and body to these, as editor.
	Edit func(n int, editor, title, body string)
	// Close closes issue or PR n (a PR without merging).
	Close func(n int)
	// Branch points branch at sha, creating it if need be.
	Branch func(name, sha string)
	// File commits data at path on branch ref.
	File func(ref, path string, data []byte)
	// LineReview submits a review of PR n as author, at sha, with comments
	// on the lines each of lines gives a Path, Line and Body for.
	LineReview func(n int, author, sha string, v forge.Verdict, body string, lines ...forge.ReviewComment)
	// Status reports check ctx on sha as "pending", "success" or "failure".
	Status func(sha, ctx, state string)
	// FailedJob reports check ctx on sha as a failed job of the forge's own
	// CI (Actions), whose log is log.
	FailedJob func(sha, ctx, log string)
	// CheckRun reports check ctx on sha as a failed check run of an App
	// other than the forge's own CI, with r's output and annotations. Nil
	// on a forge whose checks are all commit statuses, and the case that
	// needs it is skipped.
	CheckRun func(sha, ctx string, r forge.CheckReport)
	// Grant gives user perm on Repo.
	Grant func(user string, perm forge.Permission)
	// RequireChecks protects branch, requiring checks to pass before a merge.
	RequireChecks func(branch string, checks ...string)
	// SignIn gives login perm on Repo and returns their token for the
	// install's sign-in app, as OAuthExchange would. Nil when the fake
	// doesn't model signed-in people, and the User cases are skipped.
	SignIn func(login string, perm forge.Permission) string
	// Tag tags commit sha as name, the newest of the repo's tags.
	Tag func(name, sha string)
	// Descends makes commit sha a child of parents: its history has them,
	// and theirs.
	Descends func(sha string, parents ...string)
	// Calls counts the requests the adapter has made to the fake so far.
	Calls func() int
}

// PRSeed is a pull request someone opened. Base is at MergeBase when the
// PR is opened; moving the base branch on with Branch makes it Behind.
type PRSeed struct {
	Author, Title, Body string
	Head, HeadSHA       string
	Base, MergeBase     string
	// Conflicted makes the forge report the PR as not mergeable.
	Conflicted bool
	// Checking makes the forge still be working out whether the PR can
	// merge, and ask for a merge to be tried again later.
	Checking bool
	// Fork puts the head branch in a fork of the repo.
	Fork bool
	// MaintainerCanModify is the fork PR letting the repo's maintainers
	// push to its head branch.
	MaintainerCanModify bool
}

// Case is one clause of the contract.
type Case struct {
	Name string
	Run  func(t *testing.T, f *Fixture)
}

// Run runs every case against a fresh fixture.
func Run(t *testing.T, newFixture func(t *testing.T) *Fixture) {
	for _, c := range Cases {
		t.Run(c.Name, func(t *testing.T) {
			c.Run(t, newFixture(t))
		})
	}
}

var ctx = context.Background()

func item(f *Fixture, n int) forge.ItemRef { return forge.ItemRef{Repo: f.Repo, Number: n} }

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func labels(t *testing.T, f *Fixture, n int) []string {
	t.Helper()
	it, err := f.P.GetItem(ctx, item(f, n))
	must(t, err)
	return it.Labels
}

var devbotLabels = []forge.Label{
	{Name: "devbot/ready", Color: "0e8a16", Description: "queued"},
	{Name: "devbot/working", Color: "fbca04", Description: "working"},
	{Name: "bug", Color: "d73a4a"},
}

// Cases is the contract.
var Cases = []Case{
	{"identity", func(t *testing.T, f *Fixture) {
		if f.P.Name() != f.Repo.Provider {
			t.Errorf("Name() = %q, want the provider name in refs, %q", f.P.Name(), f.Repo.Provider)
		}
		impl, rev := f.P.BotUsers()
		if impl == "" || rev == "" || impl == rev {
			t.Errorf("BotUsers() = %q, %q: want two different logins", impl, rev)
		}
	}},

	{"GetItem reads an issue", func(t *testing.T, f *Fixture) {
		n := f.Issue("alice", "Export CSV", "Please.\n\n## Acceptance criteria\n- CSV", "bug", "devbot/ready")
		it, err := f.P.GetItem(ctx, item(f, n))
		must(t, err)
		if it.Ref != item(f, n) || it.Title != "Export CSV" || !strings.HasPrefix(it.Body, "Please.") || it.State != "open" || it.Author != "alice" {
			t.Errorf("got %+v", it)
		}
		if !sameSet(it.Labels, []string{"bug", "devbot/ready"}) || !it.HasLabel("devbot/ready") {
			t.Errorf("labels: %v", it.Labels)
		}
		if it.URL != f.P.IssueURL(it.Ref) {
			t.Errorf("URL %q, IssueURL %q", it.URL, f.P.IssueURL(it.Ref))
		}
		if it.CreatedAt.IsZero() || it.UpdatedAt.IsZero() {
			t.Errorf("times: %v %v", it.CreatedAt, it.UpdatedAt)
		}
	}},

	{"GetItem of a missing issue is ErrNotFound", func(t *testing.T, f *Fixture) {
		if _, err := f.P.GetItem(ctx, item(f, 999)); !errors.Is(err, forge.ErrNotFound) {
			t.Fatalf("got %v", err)
		}
	}},

	// Issues and PRs share numbers; there is no issue by a PR's.
	{"GetItem of a PR is ErrNotFound", func(t *testing.T, f *Fixture) {
		f.Branch("feature", "f00d")
		n := f.PR(PRSeed{Author: "alice", Title: "t", Head: "feature", HeadSHA: "f00d", Base: "main", MergeBase: "ba5e"})
		if it, err := f.P.GetItem(ctx, item(f, n)); !errors.Is(err, forge.ErrNotFound) {
			t.Fatalf("a PR read as an issue: %+v, %v", it, err)
		}
	}},

	{"ListComments pages through every comment, oldest first", func(t *testing.T, f *Fixture) {
		n := f.Issue("alice", "t", "b")
		for i := range 120 {
			f.Comment(n, []string{"alice", "bob"}[i%2], "comment "+strconv.Itoa(i))
		}
		cs, err := f.P.ListComments(ctx, item(f, n))
		must(t, err)
		if len(cs) != 120 {
			t.Fatalf("got %d comments, want 120", len(cs))
		}
		for i, c := range cs {
			if c.Body != "comment "+strconv.Itoa(i) || c.Author != []string{"alice", "bob"}[i%2] || c.ID == 0 || c.CreatedAt.IsZero() {
				t.Fatalf("comment %d: %+v", i, c)
			}
		}
		if _, err := f.P.ListComments(ctx, item(f, 999)); !errors.Is(err, forge.ErrNotFound) {
			t.Errorf("missing issue: got %v", err)
		}
	}},

	{"CreateItem files as the implementer", func(t *testing.T, f *Fixture) {
		must(t, f.P.EnsureLabels(ctx, f.Repo, devbotLabels))
		it, err := f.P.CreateItem(ctx, f.Repo, "Title", "Body", []string{"devbot/ready"})
		must(t, err)
		impl, _ := f.P.BotUsers()
		got, err := f.P.GetItem(ctx, it.Ref)
		must(t, err)
		if got.Title != "Title" || got.Body != "Body" || got.Author != impl || !sameSet(got.Labels, []string{"devbot/ready"}) {
			t.Fatalf("got %+v", got)
		}
	}},

	{"Comment posts as the implementer", func(t *testing.T, f *Fixture) {
		n := f.Issue("alice", "t", "b")
		must(t, f.P.Comment(ctx, item(f, n), "Working on it"))
		cs, err := f.P.ListComments(ctx, item(f, n))
		must(t, err)
		impl, _ := f.P.BotUsers()
		if len(cs) != 1 || cs[0].Body != "Working on it" || cs[0].Author != impl {
			t.Fatalf("got %+v", cs)
		}
	}},

	{"UpdateComment replaces the bot's comment, and only that one", func(t *testing.T, f *Fixture) {
		n := f.Issue("alice", "t", "b")
		must(t, f.P.Comment(ctx, item(f, n), "first"))
		must(t, f.P.Comment(ctx, item(f, n), "second"))
		cs, err := f.P.ListComments(ctx, item(f, n))
		must(t, err)
		must(t, f.P.UpdateComment(ctx, item(f, n), cs[0].ID, "first, edited"))
		cs, err = f.P.ListComments(ctx, item(f, n))
		must(t, err)
		if len(cs) != 2 || cs[0].Body != "first, edited" || cs[1].Body != "second" {
			t.Fatalf("got %+v", cs)
		}
	}},

	{"EnsureLabels can run twice", func(t *testing.T, f *Fixture) {
		must(t, f.P.EnsureLabels(ctx, f.Repo, devbotLabels))
		must(t, f.P.EnsureLabels(ctx, f.Repo, devbotLabels))
	}},

	{"SetLabels round-trips and leaves other labels alone", func(t *testing.T, f *Fixture) {
		must(t, f.P.EnsureLabels(ctx, f.Repo, devbotLabels))
		n := f.Issue("alice", "t", "b", "bug", "devbot/ready")
		must(t, f.P.SetLabels(ctx, item(f, n), []string{"devbot/working"}, []string{"devbot/ready"}))
		if got := labels(t, f, n); !sameSet(got, []string{"bug", "devbot/working"}) {
			t.Fatalf("after the change: %v", got)
		}
		// Already so: still fine, still the same.
		must(t, f.P.SetLabels(ctx, item(f, n), []string{"devbot/working"}, []string{"devbot/ready"}))
		if got := labels(t, f, n); !sameSet(got, []string{"bug", "devbot/working"}) {
			t.Fatalf("after the change again: %v", got)
		}
		must(t, f.P.SetLabels(ctx, item(f, n), nil, []string{"devbot/working"}))
		if got := labels(t, f, n); !sameSet(got, []string{"bug"}) {
			t.Fatalf("after a removal: %v", got)
		}
	}},

	{"LabelEvents records who added and removed what", func(t *testing.T, f *Fixture) {
		must(t, f.P.EnsureLabels(ctx, f.Repo, devbotLabels))
		n := f.Issue("alice", "t", "b", "devbot/ready")
		must(t, f.P.SetLabels(ctx, item(f, n), []string{"devbot/working"}, []string{"devbot/ready"}))
		evs, err := f.P.LabelEvents(ctx, item(f, n))
		must(t, err)
		impl, _ := f.P.BotUsers()
		var added, removed bool
		for _, e := range evs {
			added = added || (e.Label == "devbot/working" && e.Added && e.Actor == impl && !e.At.IsZero())
			removed = removed || (e.Label == "devbot/ready" && !e.Added && e.Actor == impl)
		}
		if !added || !removed {
			t.Fatalf("events: %+v", evs)
		}
	}},

	{"ContentEdits says who edited the title, and the body as far as it can", func(t *testing.T, f *Fixture) {
		quiet := f.Issue("alice", "t", "b")
		evs, err := f.P.ContentEdits(ctx, item(f, quiet))
		must(t, err)
		if len(evs) != 0 {
			t.Fatalf("an issue nobody edited: %+v", evs)
		}
		n := f.Issue("alice", "t", "b", "devbot/ready")
		f.Edit(n, "bob", "t2", "b")
		f.Edit(n, "carol", "t2", "b2")
		evs, err = f.P.ContentEdits(ctx, item(f, n))
		must(t, err)
		var title bool
		for i, e := range evs {
			if i > 0 && e.At.Before(evs[i-1].At) {
				t.Errorf("not oldest first: %+v", evs)
			}
			switch e.Field {
			case forge.FieldTitle:
				title = title || (e.Editor == "bob" && !e.At.IsZero())
				if e.Editor != "bob" {
					t.Errorf("title edit by %q, want bob", e.Editor)
				}
			case forge.FieldBody:
				// A forge may not say who edited a body, but must never
				// name the wrong person.
				if e.Editor != "carol" && e.Editor != "" {
					t.Errorf("body edit by %q, want carol or nobody", e.Editor)
				}
			default:
				t.Errorf("field %q", e.Field)
			}
		}
		if !title {
			t.Fatalf("no title edit by bob: %+v", evs)
		}
	}},

	{"EditItem replaces the title and body as the implementer", func(t *testing.T, f *Fixture) {
		n := f.Issue("alice", "t", "b", "devbot/ready")
		must(t, f.P.EditItem(ctx, item(f, n), "t2", "b2"))
		it, err := f.P.GetItem(ctx, item(f, n))
		must(t, err)
		if it.Title != "t2" || it.Body != "b2" || it.State != "open" || !it.HasLabel("devbot/ready") {
			t.Fatalf("got %+v", it)
		}
		evs, err := f.P.ContentEdits(ctx, item(f, n))
		must(t, err)
		impl, _ := f.P.BotUsers()
		for _, e := range evs {
			if e.Editor != "" && !forge.SameLogin(e.Editor, impl) {
				t.Errorf("%s edit by %q, want the implementer or nobody", e.Field, e.Editor)
			}
		}
	}},

	{"CloseItem closes", func(t *testing.T, f *Fixture) {
		n := f.Issue("alice", "t", "b")
		must(t, f.P.CloseItem(ctx, item(f, n)))
		it, err := f.P.GetItem(ctx, item(f, n))
		must(t, err)
		if it.State != "closed" {
			t.Fatalf("state %q", it.State)
		}
	}},

	{"SearchItems finds issues by label and state", func(t *testing.T, f *Fixture) {
		open := f.Issue("alice", "open", "b", "devbot/working")
		closed := f.Issue("alice", "closed", "b", "devbot/working")
		f.Close(closed)
		f.Issue("alice", "unlabelled", "b", "bug")
		find := func(state string) []int {
			items, err := f.P.SearchItems(ctx, "devbot/working", state)
			must(t, err)
			var ns []int
			for _, it := range items {
				if it.Ref.Repo == f.Repo {
					ns = append(ns, it.Ref.Number)
				}
			}
			slices.Sort(ns)
			return ns
		}
		if got := find("open"); !slices.Equal(got, []int{open}) {
			t.Errorf("open: %v, want [%d]", got, open)
		}
		if got := find("all"); !slices.Equal(got, []int{open, closed}) {
			t.Errorf("all: %v, want [%d %d]", got, open, closed)
		}
	}},

	{"ListItems finds one repo's issues by label and state", func(t *testing.T, f *Fixture) {
		open := f.Issue("alice", "open", "b", "devbot/working")
		closed := f.Issue("alice", "closed", "b", "devbot/working")
		f.Close(closed)
		f.Issue("alice", "unlabelled", "b", "bug")
		find := func(state string) []int {
			items, err := f.P.ListItems(ctx, f.Repo, "devbot/working", state)
			must(t, err)
			var ns []int
			for _, it := range items {
				if it.Ref.Repo != f.Repo {
					t.Errorf("listed %s, of another repo", it.Ref)
				}
				ns = append(ns, it.Ref.Number)
			}
			slices.Sort(ns)
			return ns
		}
		if got := find("open"); !slices.Equal(got, []int{open}) {
			t.Errorf("open: %v, want [%d]", got, open)
		}
		if got := find("all"); !slices.Equal(got, []int{open, closed}) {
			t.Errorf("all: %v, want [%d %d]", got, open, closed)
		}
	}},

	{"OpenPR opens as the implementer", func(t *testing.T, f *Fixture) {
		f.Branch("devbot/1-x", "c0ffee")
		p, err := f.P.OpenPR(ctx, f.Repo, "devbot/1-x", "main", "Title", "Closes #1")
		must(t, err)
		impl, _ := f.P.BotUsers()
		got, err := f.P.GetPR(ctx, f.Repo, p.Number)
		must(t, err)
		if got.Number != p.Number || got.Title != "Title" || got.Body != "Closes #1" || got.HeadBranch != "devbot/1-x" || got.HeadSHA != "c0ffee" ||
			got.BaseBranch != "main" || got.State != "open" || got.Author != impl || got.URL != f.P.PRURL(f.Repo, p.Number) {
			t.Fatalf("got %+v", got)
		}
		open, err := f.P.ListOpenPRs(ctx, f.Repo)
		must(t, err)
		if len(open) != 1 || open[0].Number != p.Number {
			t.Fatalf("open PRs: %+v", open)
		}
	}},

	{"UpdatePR changes title and body", func(t *testing.T, f *Fixture) {
		f.Branch("devbot/1-x", "c0ffee")
		p, err := f.P.OpenPR(ctx, f.Repo, "devbot/1-x", "main", "Title", "Body")
		must(t, err)
		must(t, f.P.UpdatePR(ctx, f.Repo, p.Number, "New title", "New body"))
		got, err := f.P.GetPR(ctx, f.Repo, p.Number)
		must(t, err)
		if got.Title != "New title" || got.Body != "New body" {
			t.Fatalf("got %+v", got)
		}
	}},

	{"SetPRBase moves a PR onto another branch, and Behind follows it", func(t *testing.T, f *Fixture) {
		f.Branch("devbot/1-x", "c0ffee")
		f.Branch("release/x", "5ea1")
		p, err := f.P.OpenPR(ctx, f.Repo, "devbot/1-x", "main", "Title", "Body")
		must(t, err)
		must(t, f.P.SetPRBase(ctx, f.Repo, p.Number, "release/x"))
		got, err := f.P.GetPR(ctx, f.Repo, p.Number)
		must(t, err)
		if got.BaseBranch != "release/x" || got.Title != "Title" || got.Body != "Body" || got.State != "open" || got.Behind {
			t.Fatalf("got %+v", got)
		}
		f.Branch("release/x", "5ea2")
		got, err = f.P.GetPR(ctx, f.Repo, p.Number)
		must(t, err)
		if !got.Behind {
			t.Errorf("the new base moved on, but the PR isn't behind: %+v", got)
		}
		if err := f.P.SetPRBase(ctx, f.Repo, p.Number, "no-such-branch"); err == nil {
			t.Error("moving a PR onto a missing branch succeeded")
		}
	}},

	{"BranchExists", func(t *testing.T, f *Fixture) {
		f.Branch("release/x", "5ea1")
		for b, want := range map[string]bool{"main": true, "release/x": true, "release/y": false} {
			got, err := f.P.BranchExists(ctx, f.Repo, b)
			if err != nil || got != want {
				t.Errorf("%q: got %v, %v; want %v", b, got, err, want)
			}
		}
	}},

	{"GetPR reads a PR", func(t *testing.T, f *Fixture) {
		f.Branch("main", "ba5e")
		f.Branch("feature", "f00d")
		n := f.PR(PRSeed{Author: "alice", Title: "Feature", Body: "Body", Head: "feature", HeadSHA: "f00d", Base: "main", MergeBase: "ba5e"})
		p, err := f.P.GetPR(ctx, f.Repo, n)
		must(t, err)
		if p.Repo != f.Repo || p.Number != n || p.Title != "Feature" || p.Body != "Body" || p.State != "open" || p.Merged ||
			p.HeadBranch != "feature" || p.HeadSHA != "f00d" || p.BaseBranch != "main" || !p.Mergeable || p.Behind ||
			p.Author != "alice" || p.URL != f.P.PRURL(f.Repo, n) || p.CreatedAt.IsZero() || p.Fork || !p.ClosedAt.IsZero() {
			t.Fatalf("got %+v", p)
		}
		if _, err := f.P.GetPR(ctx, f.Repo, 999); !errors.Is(err, forge.ErrNotFound) {
			t.Errorf("missing PR: got %v", err)
		}
	}},

	{"GetPR reports conflicts and a base that moved on", func(t *testing.T, f *Fixture) {
		f.Branch("main", "ba5e")
		f.Branch("conflict", "c0")
		f.Branch("behind", "be")
		c := f.PR(PRSeed{Author: "alice", Title: "c", Head: "conflict", HeadSHA: "c0", Base: "main", MergeBase: "ba5e", Conflicted: true})
		b := f.PR(PRSeed{Author: "alice", Title: "b", Head: "behind", HeadSHA: "be", Base: "main", MergeBase: "ba5e"})
		f.Branch("main", "5ec0d")
		p, err := f.P.GetPR(ctx, f.Repo, c)
		must(t, err)
		if p.Mergeable {
			t.Errorf("a conflicting PR is mergeable: %+v", p)
		}
		p, err = f.P.GetPR(ctx, f.Repo, b)
		must(t, err)
		if !p.Behind || !p.Mergeable {
			t.Errorf("a PR whose base moved on isn't behind: %+v", p)
		}
	}},

	{"ListOpenPRs lists only open PRs", func(t *testing.T, f *Fixture) {
		var ns []int
		for i := range 3 {
			b := "b" + strconv.Itoa(i)
			f.Branch(b, "5ha"+strconv.Itoa(i))
			ns = append(ns, f.PR(PRSeed{Author: "alice", Title: b, Head: b, HeadSHA: "5ha" + strconv.Itoa(i), Base: "main", MergeBase: "ba5e"}))
		}
		f.Close(ns[1])
		prs, err := f.P.ListOpenPRs(ctx, f.Repo)
		must(t, err)
		var got []int
		for _, p := range prs {
			if p.State != "open" || p.Repo != f.Repo {
				t.Errorf("listed %+v", p)
			}
			got = append(got, p.Number)
		}
		slices.Sort(got)
		if !slices.Equal(got, []int{ns[0], ns[2]}) {
			t.Fatalf("got %v, want %v", got, []int{ns[0], ns[2]})
		}
	}},

	// devbot/ready on a PR hands it to devbot, which reads the label and who
	// added it, comments on the PR and takes the label off again, by the
	// PR's number, and finds labelled PRs it missed with SearchPRs.
	{"a PR's labels, label events and comments, and SearchPRs", func(t *testing.T, f *Fixture) {
		must(t, f.P.EnsureLabels(ctx, f.Repo, devbotLabels))
		f.Branch("feature", "f00d")
		f.Branch("other", "0the")
		n := f.PR(PRSeed{Author: "alice", Title: "Feature", Body: "Body", Head: "feature", HeadSHA: "f00d", Base: "main", MergeBase: "ba5e"})
		quiet := f.PR(PRSeed{Author: "alice", Title: "Other", Head: "other", HeadSHA: "0the", Base: "main", MergeBase: "ba5e"})
		issue := f.Issue("alice", "an issue", "b", "devbot/ready")
		pr := item(f, n)
		must(t, f.P.SetLabels(ctx, pr, []string{"devbot/ready", "bug"}, nil))
		got, err := f.P.GetPR(ctx, f.Repo, n)
		must(t, err)
		if !sameSet(got.Labels, []string{"devbot/ready", "bug"}) || !got.HasLabel("devbot/ready") {
			t.Fatalf("labels: %v", got.Labels)
		}
		listed, err := f.P.SearchPRs(ctx, "devbot/ready")
		must(t, err)
		var ns []int
		for _, p := range listed {
			if p.Repo != f.Repo {
				continue
			}
			ns = append(ns, p.Number)
			if p.Number == n && (p.Title != "Feature" || p.URL != f.P.PRURL(f.Repo, n) || !p.HasLabel("devbot/ready")) {
				t.Errorf("listed %+v", p)
			}
		}
		if !slices.Equal(ns, []int{n}) {
			t.Errorf("SearchPRs: got %v, want only PR %d (not PR %d, nor issue %d)", ns, n, quiet, issue)
		}
		items, err := f.P.SearchItems(ctx, "devbot/ready", "open")
		must(t, err)
		for _, it := range items {
			if it.Ref == pr {
				t.Errorf("SearchItems listed PR %d as an issue", n)
			}
		}
		items, err = f.P.ListItems(ctx, f.Repo, "devbot/ready", "open")
		must(t, err)
		if len(items) != 1 || items[0].Ref.Number != issue {
			t.Errorf("ListItems: got %v, want only issue %d", items, issue)
		}
		must(t, f.P.Comment(ctx, pr, "Carrying it"))
		cs, err := f.P.ListComments(ctx, pr)
		must(t, err)
		impl, _ := f.P.BotUsers()
		if len(cs) != 1 || cs[0].Body != "Carrying it" || !forge.SameLogin(cs[0].Author, impl) {
			t.Fatalf("comments: %+v", cs)
		}
		must(t, f.P.SetLabels(ctx, pr, nil, []string{"devbot/ready"}))
		got, err = f.P.GetPR(ctx, f.Repo, n)
		must(t, err)
		if !sameSet(got.Labels, []string{"bug"}) {
			t.Fatalf("labels after a removal: %v", got.Labels)
		}
		evs, err := f.P.LabelEvents(ctx, pr)
		must(t, err)
		var added, removed bool
		for _, e := range evs {
			added = added || (e.Label == "devbot/ready" && e.Added && forge.SameLogin(e.Actor, impl) && !e.At.IsZero())
			removed = removed || (e.Label == "devbot/ready" && !e.Added && forge.SameLogin(e.Actor, impl))
		}
		if !added || !removed {
			t.Fatalf("events: %+v", evs)
		}
		if listed, err = f.P.SearchPRs(ctx, "devbot/ready"); err != nil || slices.ContainsFunc(listed, func(p forge.PR) bool { return p.Repo == f.Repo }) {
			t.Errorf("SearchPRs after the label came off: %+v, %v", listed, err)
		}
	}},

	// devbot carries a PR by pushing onto its head branch: the PR follows
	// the branch, and the implementer may push there.
	{"a push to a PR's head branch moves the PR's head", func(t *testing.T, f *Fixture) {
		f.Branch("main", "ba5e")
		f.Branch("feature", "f00d")
		n := f.PR(PRSeed{Author: "alice", Title: "Feature", Head: "feature", HeadSHA: "f00d", Base: "main", MergeBase: "ba5e"})
		ok, err := f.P.CanWrite(ctx, f.Repo, forge.RoleImplementer)
		if err != nil || !ok {
			t.Fatalf("CanWrite: %v, %v", ok, err)
		}
		f.Branch("feature", "f00e")
		p, err := f.P.GetPR(ctx, f.Repo, n)
		must(t, err)
		if p.HeadSHA != "f00e" || p.HeadBranch != "feature" || p.State != "open" || p.Fork {
			t.Fatalf("after the push: %+v", p)
		}
		if head, err := f.P.BranchHead(ctx, f.Repo, "feature"); err != nil || head != "f00e" {
			t.Errorf("BranchHead: %q, %v", head, err)
		}
	}},

	{"GetPR reports a fork head and when the PR closed", func(t *testing.T, f *Fixture) {
		f.Branch("feature", "f00d")
		own := f.PR(PRSeed{Author: "alice", Title: "own", Head: "feature", HeadSHA: "f00d", Base: "main", MergeBase: "ba5e"})
		fork := f.PR(PRSeed{Author: "mallory", Title: "fork", Head: "feature", HeadSHA: "bad", Base: "main", MergeBase: "ba5e", Fork: true})
		f.Close(own)
		p, err := f.P.GetPR(ctx, f.Repo, own)
		must(t, err)
		if p.Fork || p.State != "closed" || p.ClosedAt.IsZero() || p.HeadRepo != f.Repo {
			t.Errorf("own closed PR: %+v", p)
		}
		// The fork is named on the repo's own forge, whatever clone URL the
		// forge advertises for it.
		p, err = f.P.GetPR(ctx, f.Repo, fork)
		must(t, err)
		if !p.Fork || !p.ClosedAt.IsZero() || p.MaintainerCanModify || p.HeadRepo.Owner != "mallory" || p.HeadRepo.Name == "" ||
			p.HeadRepo.Provider != f.Repo.Provider {
			t.Errorf("open fork PR: %+v", p)
		}
	}},

	{"GetPR reads whether a fork PR lets maintainers push to it", func(t *testing.T, f *Fixture) {
		n := f.PR(PRSeed{Author: "mallory", Title: "fork", Head: "patch-1", HeadSHA: "bad", Base: "main", MergeBase: "ba5e", Fork: true, MaintainerCanModify: true})
		p, err := f.P.GetPR(ctx, f.Repo, n)
		must(t, err)
		if !p.Fork || !p.MaintainerCanModify {
			t.Errorf("fork PR with maintainer edits: %+v", p)
		}
	}},

	{"CloseItem closes a PR by its number", func(t *testing.T, f *Fixture) {
		n := f.PR(PRSeed{Author: "mallory", Title: "fork", Head: "patch-1", HeadSHA: "bad", Base: "main", MergeBase: "ba5e", Fork: true})
		must(t, f.P.CloseItem(ctx, item(f, n)))
		p, err := f.P.GetPR(ctx, f.Repo, n)
		must(t, err)
		if p.State != "closed" || p.Merged {
			t.Errorf("after CloseItem: %+v", p)
		}
	}},

	{"ListRepos lists the repos the implementer works in", func(t *testing.T, f *Fixture) {
		repos, err := f.P.ListRepos(ctx)
		must(t, err)
		if !slices.Contains(repos, f.Repo) || slices.Contains(repos, f.NoAccess) {
			t.Fatalf("got %v, want %v and not %v", repos, f.Repo, f.NoAccess)
		}
	}},

	{"ListBranches pages through the branches under a prefix", func(t *testing.T, f *Fixture) {
		want := map[string]string{}
		for i := range 60 {
			name, sha := fmt.Sprintf("devbot/%d-x", i), fmt.Sprintf("5ha%02d", i)
			f.Branch(name, sha)
			want[name] = sha
		}
		f.Branch("feature", "f00d")
		f.Branch("devbotx", "d0")
		bs, err := f.P.ListBranches(ctx, f.Repo, "devbot/")
		must(t, err)
		got := map[string]string{}
		for _, b := range bs {
			if b.CommittedAt.IsZero() {
				t.Errorf("%s has no commit date", b.Name)
			}
			got[b.Name] = b.SHA
		}
		if !maps.Equal(got, want) {
			t.Fatalf("got %v", got)
		}
		bs, err = f.P.ListBranches(ctx, f.Repo, "devbot/7-x")
		must(t, err)
		if len(bs) != 1 || bs[0].Name != "devbot/7-x" || bs[0].SHA != "5ha07" {
			t.Fatalf("by full name: %+v", bs)
		}
	}},

	{"DeleteBranch deletes, and a branch already gone is ErrNotFound", func(t *testing.T, f *Fixture) {
		f.Branch("devbot/1-x", "a1")
		f.Branch("devbot/2-y", "b2")
		must(t, f.P.DeleteBranch(ctx, f.Repo, "devbot/1-x"))
		bs, err := f.P.ListBranches(ctx, f.Repo, "devbot/")
		must(t, err)
		if len(bs) != 1 || bs[0].Name != "devbot/2-y" {
			t.Fatalf("after the delete: %+v", bs)
		}
		if err := f.P.DeleteBranch(ctx, f.Repo, "devbot/1-x"); !errors.Is(err, forge.ErrNotFound) {
			t.Fatalf("deleting it again: %v, want ErrNotFound", err)
		}
	}},

	{"CombinedChecks waits for every check", func(t *testing.T, f *Fixture) {
		state := func(sha string) (forge.CheckState, []forge.CheckDetail) {
			s, d, err := f.P.CombinedChecks(ctx, f.Repo, sha)
			must(t, err)
			return s, d
		}
		if s, _ := state("a1"); s != forge.ChecksNone {
			t.Errorf("nothing reported: %s", s)
		}
		f.Status("a1", "build", "success")
		f.Status("a1", "test", "pending")
		if s, d := state("a1"); s != forge.ChecksPending || len(d) != 2 {
			t.Errorf("one pending: %s %+v", s, d)
		}
		f.Status("a1", "test", "success")
		if s, _ := state("a1"); s != forge.ChecksSuccess {
			t.Errorf("all passed: %s", s)
		}
		f.Status("b2", "build", "success")
		f.Status("b2", "test", "failure")
		if s, d := state("b2"); s != forge.ChecksFailure || !slices.ContainsFunc(d, func(c forge.CheckDetail) bool { return c.Context == "test" }) {
			t.Errorf("one failed: %s %+v", s, d)
		}
	}},

	{"SetCommitStatus reports a check CombinedChecks lists, and replaces its own", func(t *testing.T, f *Fixture) {
		f.Status("e5", "build", "success")
		must(t, f.P.SetCommitStatus(ctx, f.Repo, "e5", forge.CommitStatus{Context: "devbot/integration", State: "pending", Description: "running"}))
		if s, ds, err := f.P.CombinedChecks(ctx, f.Repo, "e5"); err != nil || s != forge.ChecksPending || len(ds) != 2 {
			t.Fatalf("pending: %s %+v %v", s, ds, err)
		}
		must(t, f.P.SetCommitStatus(ctx, f.Repo, "e5", forge.CommitStatus{Context: "devbot/integration", State: "failure", Description: "make e2e failed", URL: "https://forge.example/issues/1"}))
		s, ds, err := f.P.CombinedChecks(ctx, f.Repo, "e5")
		must(t, err)
		i := slices.IndexFunc(ds, func(d forge.CheckDetail) bool { return d.Context == "devbot/integration" })
		if s != forge.ChecksFailure || len(ds) != 2 || i < 0 || !ds[i].Failed || ds[i].Description != "make e2e failed" || ds[i].URL != "https://forge.example/issues/1" {
			t.Fatalf("replaced: %s %+v", s, ds)
		}
	}},

	{"CheckReport tails a failed job's log, and a commit status has nothing more", func(t *testing.T, f *Fixture) {
		var log strings.Builder
		for i := 1; i <= 400; i++ {
			fmt.Fprintf(&log, "line %d\n", i)
		}
		f.FailedJob("c3", "build", log.String())
		f.FailedJob("c3", "lint", "one\ntwo\n")
		f.Status("c3", "external", "failure")
		s, ds, err := f.P.CombinedChecks(ctx, f.Repo, "c3")
		must(t, err)
		if s != forge.ChecksFailure || len(ds) != 3 {
			t.Fatalf("got %s %+v", s, ds)
		}
		for _, d := range ds {
			r, err := f.P.CheckReport(ctx, f.Repo, "c3", d, 150)
			must(t, err)
			switch d.Context {
			case "build":
				lines := strings.Split(r.Log, "\n")
				if !r.HasLog || !r.LogCut || len(lines) != 150 || lines[0] != "line 251" || lines[149] != "line 400" {
					t.Errorf("build: %d lines from %q to %q, cut %v, has %v", len(lines), lines[0], lines[len(lines)-1], r.LogCut, r.HasLog)
				}
			case "lint":
				if !r.HasLog || r.LogCut || r.Log != "one\ntwo" {
					t.Errorf("lint: %+v", r)
				}
			case "external":
				if !reflect.DeepEqual(r, forge.CheckReport{}) {
					t.Errorf("a commit status: %+v, want nothing", r)
				}
			}
		}
	}},

	{"CheckReport reads a check run's output and every page of its annotations", func(t *testing.T, f *Fixture) {
		if f.CheckRun == nil {
			t.Skip("this forge's checks are all commit statuses")
		}
		want := forge.CheckReport{Title: "3 findings", Summary: "SAST found **3** issues", Text: "details"}
		for i := 1; i <= 250; i++ {
			want.Annotations = append(want.Annotations, forge.Annotation{Path: fmt.Sprintf("f%d.go", i), StartLine: i, EndLine: i + 1, Level: "failure", Title: "t", Message: fmt.Sprintf("finding %d", i)})
		}
		f.CheckRun("d4", "sast", want)
		_, ds, err := f.P.CombinedChecks(ctx, f.Repo, "d4")
		must(t, err)
		if len(ds) != 1 {
			t.Fatalf("got %+v", ds)
		}
		r, err := f.P.CheckReport(ctx, f.Repo, "d4", ds[0], 150)
		must(t, err)
		if !reflect.DeepEqual(r, want) {
			t.Fatalf("got %q %q %q, %d annotations, more %v, log %v", r.Title, r.Summary, r.Text, len(r.Annotations), r.MoreAnnotations, r.HasLog)
		}
	}},

	{"Review posts as the reviewer, pinned to the SHA", func(t *testing.T, f *Fixture) {
		f.Branch("feature", "f00d")
		n := f.PR(PRSeed{Author: "alice", Title: "t", Head: "feature", HeadSHA: "f00d", Base: "main", MergeBase: "ba5e"})
		must(t, f.P.Review(ctx, f.Repo, n, "f00d", forge.VerdictRequestChanges, "Fix the test"))
		must(t, f.P.Review(ctx, f.Repo, n, "f00d", forge.VerdictApprove, "LGTM"))
		rs, err := f.P.Reviews(ctx, f.Repo, n)
		must(t, err)
		_, rev := f.P.BotUsers()
		if len(rs) != 2 || rs[0].Verdict != forge.VerdictRequestChanges || rs[1].Verdict != forge.VerdictApprove || rs[1].Body != "LGTM" {
			t.Fatalf("got %+v", rs)
		}
		for _, r := range rs {
			if r.Author != rev || r.CommitSHA != "f00d" || r.Dismissed {
				t.Errorf("review %+v: want by %s at f00d", r, rev)
			}
		}
	}},

	{"ReviewComments lists every review's line comments, across pages", func(t *testing.T, f *Fixture) {
		f.Branch("feature", "f00d")
		n := f.PR(PRSeed{Author: "alice", Title: "t", Head: "feature", HeadSHA: "f00d", Base: "main", MergeBase: "ba5e"})
		var many []forge.ReviewComment
		for i := range 120 {
			many = append(many, forge.ReviewComment{Path: fmt.Sprintf("pkg/f%d.go", i), Line: i + 1, Body: "nit " + strconv.Itoa(i)})
		}
		f.LineReview(n, "bob", "f00d", forge.VerdictRequestChanges, "Lots to fix", many...)
		f.LineReview(n, "alice", "f00d", forge.VerdictComment, "Just a note")
		f.LineReview(n, "carol", "f00d", forge.VerdictComment, "", forge.ReviewComment{Path: "main.go", Line: 7, Body: "why?"})
		rs, err := f.P.Reviews(ctx, f.Repo, n)
		must(t, err)
		ids := map[string]int64{}
		for _, r := range rs {
			ids[r.Author] = r.ID
		}
		if len(rs) != 3 || ids["bob"] == 0 || ids["carol"] == 0 || ids["bob"] == ids["carol"] {
			t.Fatalf("reviews: %+v", rs)
		}
		cs, err := f.P.ReviewComments(ctx, f.Repo, n)
		must(t, err)
		if len(cs) != 121 {
			t.Fatalf("got %d line comments, want 121", len(cs))
		}
		for i, c := range cs[:120] {
			if c.Author != "bob" || c.ReviewID != ids["bob"] || c.Path != fmt.Sprintf("pkg/f%d.go", i) || c.Line != i+1 || c.Body != "nit "+strconv.Itoa(i) || c.ID == 0 || c.CreatedAt.IsZero() {
				t.Fatalf("line comment %d: %+v", i, c)
			}
		}
		if c := cs[120]; c.Author != "carol" || c.ReviewID != ids["carol"] || c.Path != "main.go" || c.Line != 7 || c.Body != "why?" || c.CommitSHA != "f00d" {
			t.Fatalf("last line comment: %+v", c)
		}
	}},

	{"Merge merges only at the given head", func(t *testing.T, f *Fixture) {
		f.Branch("feature", "f00d")
		n := f.PR(PRSeed{Author: "alice", Title: "t", Head: "feature", HeadSHA: "f00d", Base: "main", MergeBase: "ba5e"})
		// The core re-reads the PR on a moved head, but hands a refusal to a
		// person, so the two must not be confused.
		if err := f.P.Merge(ctx, f.Repo, n, "0ld5ha", "squash"); !errors.Is(err, forge.ErrHeadMoved) || errors.Is(err, forge.ErrNotMergeable) {
			t.Fatalf("merge at a head the PR no longer has: got %v, want %v", err, forge.ErrHeadMoved)
		}
		if p, _ := f.P.GetPR(ctx, f.Repo, n); p.Merged {
			t.Fatal("merged anyway")
		}
		if p, _ := f.P.GetPR(ctx, f.Repo, n); p.MergeSHA != "" {
			t.Fatalf("an open PR has left no commit on its base: %q", p.MergeSHA)
		}
		must(t, f.P.Merge(ctx, f.Repo, n, "f00d", "squash"))
		p, err := f.P.GetPR(ctx, f.Repo, n)
		must(t, err)
		if !p.Merged || p.State != "closed" || p.MergeSHA == "" {
			t.Fatalf("got %+v", p)
		}
		// What a release gate waits for: a tag on that commit has it.
		f.Tag("v1.0.0", p.MergeSHA)
		if ok, err := f.P.Contains(ctx, f.Repo, "v1.0.0", p.MergeSHA); err != nil || !ok {
			t.Fatalf("a tag of the merge commit contains it: %v %v", ok, err)
		}
	}},

	{"ListTags lists the newest tags, and Contains follows history", func(t *testing.T, f *Fixture) {
		// c1 <- c2 <- c3, and x off c1.
		f.Descends("c2", "c1")
		f.Descends("c3", "c2")
		f.Descends("x", "c1")
		f.Tag("v1.0.0", "c1")
		f.Tag("v1.1.0", "c3")
		f.Tag("side", "x")
		tags, err := f.P.ListTags(ctx, f.Repo, 2)
		must(t, err)
		if want := []forge.Tag{{Name: "side", SHA: "x"}, {Name: "v1.1.0", SHA: "c3"}}; !slices.Equal(tags, want) {
			t.Fatalf("the newest 2 tags: got %+v, want %+v", tags, want)
		}
		if tags, err := f.P.ListTags(ctx, f.Repo, 10); err != nil || len(tags) != 3 || tags[2] != (forge.Tag{Name: "v1.0.0", SHA: "c1"}) {
			t.Fatalf("every tag, fewer than asked for: got %+v %v", tags, err)
		}
		for _, c := range []struct {
			ref, ancestor string
			want          bool
		}{
			{"c3", "c1", true},
			{"c3", "c3", true},
			{"v1.1.0", "c2", true},
			{"c1", "c3", false},
			{"x", "c2", false},
			{"v1.0.0", "c2", false},
			{"side", "c1", true},
		} {
			got, err := f.P.Contains(ctx, f.Repo, c.ref, c.ancestor)
			if err != nil || got != c.want {
				t.Errorf("Contains(%s, %s) = %v, %v; want %v", c.ref, c.ancestor, got, err, c.want)
			}
		}
	}},

	{"Merge is refused for now while the forge is still checking the PR", func(t *testing.T, f *Fixture) {
		f.Branch("feature", "f00d")
		n := f.PR(PRSeed{Author: "alice", Title: "t", Head: "feature", HeadSHA: "f00d", Base: "main", MergeBase: "ba5e", Checking: true})
		// The core retries a pending merge with backoff, but hands a refusal
		// to a person, so the two must not be confused.
		err := f.P.Merge(ctx, f.Repo, n, "f00d", "squash")
		if !errors.Is(err, forge.ErrMergePending) || errors.Is(err, forge.ErrNotMergeable) || errors.Is(err, forge.ErrHeadMoved) {
			t.Fatalf("got %v, want %v", err, forge.ErrMergePending)
		}
		open, err := f.P.ListOpenPRs(ctx, f.Repo)
		must(t, err)
		if !slices.ContainsFunc(open, func(p forge.PR) bool { return p.Number == n }) {
			t.Fatal("merged or closed anyway")
		}
	}},

	{"Protection names the checks a branch requires", func(t *testing.T, f *Fixture) {
		p, err := f.P.Protection(ctx, f.Repo, "main")
		must(t, err)
		if len(p.RequiredChecks)+len(p.Rules) > 0 || p.Exempt != "" {
			t.Fatalf("an unprotected branch: got %+v", p)
		}
		f.RequireChecks("main", "ci", "e2e")
		p, err = f.P.Protection(ctx, f.Repo, "main")
		must(t, err)
		if !sameSet(p.RequiredChecks, []string{"ci", "e2e"}) || len(p.Rules) == 0 || p.Exempt == "" {
			t.Fatalf("got %+v", p)
		}
		_, rev := f.P.BotUsers()
		if !strings.Contains(p.Exempt, rev) {
			t.Errorf("the way past the rules should name the reviewer, %s: %q", rev, p.Exempt)
		}
	}},

	{"DefaultBranch", func(t *testing.T, f *Fixture) {
		b, err := f.P.DefaultBranch(ctx, f.Repo)
		if err != nil || b != "main" {
			t.Fatalf("got %q, %v", b, err)
		}
	}},

	{"GetFile reads a file at a ref, and ErrNotFound when missing", func(t *testing.T, f *Fixture) {
		f.File("main", "docs/a b.md", []byte("on main"))
		f.File("feature", "docs/a b.md", []byte("on feature"))
		for ref, want := range map[string]string{"": "on main", "main": "on main", "feature": "on feature"} {
			b, err := f.P.GetFile(ctx, f.Repo, ref, "docs/a b.md")
			if err != nil || string(b) != want {
				t.Errorf("ref %q: got %q, %v", ref, b, err)
			}
		}
		if _, err := f.P.GetFile(ctx, f.Repo, "main", ".devbot.yaml"); !errors.Is(err, forge.ErrNotFound) {
			t.Errorf("missing file: got %v", err)
		}
	}},

	{"BranchHead is a branch's commit, and ErrNotFound for none", func(t *testing.T, f *Fixture) {
		f.Branch("release/x", "5ea1")
		if sha, err := f.P.BranchHead(ctx, f.Repo, "release/x"); err != nil || sha != "5ea1" {
			t.Errorf("got %q, %v", sha, err)
		}
		if _, err := f.P.BranchHead(ctx, f.Repo, "release/y"); !errors.Is(err, forge.ErrNotFound) {
			t.Errorf("missing branch: got %v", err)
		}
	}},

	{"ListTree lists every file at a branch or its commit", func(t *testing.T, f *Fixture) {
		f.Branch("feature", "f00d")
		for _, p := range []string{"go.mod", "svc/api/compose.yaml", "a/b/c/d.txt"} {
			f.File("feature", p, []byte("x"))
		}
		f.File("other", "go.mod", []byte("x"))
		for _, ref := range []string{"feature", "f00d"} {
			files, err := f.P.ListTree(ctx, f.Repo, ref)
			must(t, err)
			var got []string
			for _, tf := range files {
				got = append(got, tf.Path)
				if tf.Size != 1 {
					t.Errorf("%s: %s has size %d", ref, tf.Path, tf.Size)
				}
			}
			slices.Sort(got)
			if want := []string{"a/b/c/d.txt", "go.mod", "svc/api/compose.yaml"}; !slices.Equal(got, want) {
				t.Errorf("%s: got %q, want %q", ref, got, want)
			}
		}
	}},

	{"web URLs are built without a call", func(t *testing.T, f *Fixture) {
		before := f.Calls()
		issue, pr, branch := f.P.IssueURL(item(f, 12)), f.P.PRURL(f.Repo, 13), f.P.BranchURL(f.Repo, "devbot/12-a b")
		if f.Calls() != before {
			t.Errorf("URLs took %d calls", f.Calls()-before)
		}
		for _, u := range []string{issue, pr, branch} {
			p, err := url.Parse(u)
			if err != nil || p.Host == "" || !strings.Contains(p.Path, "/"+f.Repo.Owner+"/"+f.Repo.Name+"/") {
				t.Errorf("%q isn't a web URL of %s", u, f.Repo)
			}
		}
		if !strings.HasSuffix(issue, "/12") || !strings.HasSuffix(pr, "/13") ||
			!strings.HasSuffix(branch, "/devbot/12-a%20b") {
			t.Errorf("issue %s, PR %s, branch %s", issue, pr, branch)
		}
	}},

	{"GitAuth gives a clean clone URL and a header per role", func(t *testing.T, f *Fixture) {
		headers := map[forge.Role]string{}
		for _, role := range []forge.Role{forge.RoleImplementer, forge.RoleReviewer} {
			clone, header, err := f.P.GitAuth(ctx, f.Repo, role)
			must(t, err)
			u, err := url.Parse(clone)
			if err != nil || u.User != nil || u.RawQuery != "" || !strings.HasSuffix(u.Path, "/"+f.Repo.Owner+"/"+f.Repo.Name+".git") {
				t.Errorf("%s: clone URL %q must be the repo's, with no credentials", role, clone)
			}
			name, value, ok := strings.Cut(header, ": ")
			if !ok || name != "Authorization" || value == "" || strings.Contains(clone, value) {
				t.Errorf("%s: header %q", role, header)
			}
			headers[role] = header
		}
		if headers[forge.RoleImplementer] == headers[forge.RoleReviewer] {
			t.Error("both roles authenticate git as the same identity")
		}
	}},

	{"CanWrite asks as each bot", func(t *testing.T, f *Fixture) {
		for _, role := range []forge.Role{forge.RoleImplementer, forge.RoleReviewer} {
			if ok, err := f.P.CanWrite(ctx, f.Repo, role); !ok || err != nil {
				t.Errorf("%s on %s: %v, %v", role, f.Repo, ok, err)
			}
			if ok, err := f.P.CanWrite(ctx, f.NoAccess, role); ok || err != nil {
				t.Errorf("%s on %s: %v, %v; want false, nil", role, f.NoAccess, ok, err)
			}
		}
	}},

	{"Authorized compares the forge's permission", func(t *testing.T, f *Fixture) {
		f.Grant("wendy", forge.PermWrite)
		f.Grant("rita", forge.PermRead)
		f.Grant("adam", forge.PermAdmin)
		for _, c := range []struct {
			actor string
			min   forge.Permission
			want  bool
		}{
			{"wendy", forge.PermWrite, true}, {"wendy", forge.PermAdmin, false}, {"rita", forge.PermWrite, false},
			{"adam", forge.PermAdmin, true}, {"stranger", forge.PermWrite, false}, {"", forge.PermWrite, false},
		} {
			if got, err := f.P.Authorized(ctx, f.Repo, c.actor, c.min); got != c.want || err != nil {
				t.Errorf("%q at %s: %v, %v; want %v", c.actor, c.min, got, err, c.want)
			}
		}
	}},

	{"attachments are read back only from their own PR", func(t *testing.T, f *Fixture) {
		f.Branch("a", "aa")
		f.Branch("b", "bb")
		n := f.PR(PRSeed{Author: "alice", Title: "a", Head: "a", HeadSHA: "aa", Base: "main", MergeBase: "ba5e"})
		other := f.PR(PRSeed{Author: "alice", Title: "b", Head: "b", HeadSHA: "bb", Base: "main", MergeBase: "ba5e"})
		png := []byte("\x89PNG\r\n\x1a\nboard")
		u, err := f.P.AttachToPR(ctx, f.Repo, n, "board.png", png)
		must(t, err)
		got, err := f.P.GetAttachment(ctx, f.Repo, n, u, forge.RoleReviewer)
		if err != nil || !bytes.Equal(got, png) {
			t.Fatalf("read back: %q, %v", got, err)
		}
		if _, err := f.P.GetAttachment(ctx, f.Repo, other, u, forge.RoleReviewer); err == nil {
			t.Error("another PR's attachment was fetched")
		}
		if _, err := f.P.GetAttachment(ctx, f.Repo, n, "https://evil.example/steal", forge.RoleReviewer); err == nil {
			t.Error("a foreign URL was fetched")
		}
	}},

	{"releasing a PR's attachments is repeatable and leaves other PRs' alone", func(t *testing.T, f *Fixture) {
		f.Branch("a", "aa")
		f.Branch("b", "bb")
		n := f.PR(PRSeed{Author: "alice", Title: "a", Head: "a", HeadSHA: "aa", Base: "main", MergeBase: "ba5e"})
		other := f.PR(PRSeed{Author: "alice", Title: "b", Head: "b", HeadSHA: "bb", Base: "main", MergeBase: "ba5e"})
		must(t, f.P.ReleaseAttachments(ctx, f.Repo, n)) // nothing attached yet
		_, err := f.P.AttachToPR(ctx, f.Repo, n, "a.log", []byte("a\n"))
		must(t, err)
		u, err := f.P.AttachToPR(ctx, f.Repo, other, "b.log", []byte("b\n"))
		must(t, err)
		f.Close(n)
		must(t, f.P.ReleaseAttachments(ctx, f.Repo, n))
		must(t, f.P.ReleaseAttachments(ctx, f.Repo, n))
		if got, err := f.P.GetAttachment(ctx, f.Repo, other, u, forge.RoleReviewer); err != nil || string(got) != "b\n" {
			t.Fatalf("the other PR's attachment: %q, %v", got, err)
		}
	}},

	// What a signed-in person does through a connector, with their own token.

	{"User.Permission is the person's own access", func(t *testing.T, f *Fixture) {
		needSignIn(t, f)
		for _, p := range []forge.Permission{forge.PermRead, forge.PermWrite, forge.PermAdmin} {
			u := f.P.AsUser(f.SignIn("person-"+p.String(), p))
			if got, err := u.Permission(ctx, f.Repo); got != p || err != nil {
				t.Errorf("%s on %s: %s, %v", p, f.Repo, got, err)
			}
			if got, err := u.Permission(ctx, f.NoAccess); got != forge.PermNone || err != nil {
				t.Errorf("%s on %s: %s, %v; want none, nil", p, f.NoAccess, got, err)
			}
		}
	}},

	{"User.CanEditIssues: a writer may, a reader may not", func(t *testing.T, f *Fixture) {
		needSignIn(t, f)
		if ok, err := f.P.AsUser(f.SignIn("wendy", forge.PermWrite)).CanEditIssues(ctx, f.Repo); !ok || err != nil {
			t.Errorf("writer: %v, %v", ok, err)
		}
		if ok, err := f.P.AsUser(f.SignIn("rita", forge.PermRead)).CanEditIssues(ctx, f.Repo); ok || err != nil {
			t.Errorf("reader: %v, %v", ok, err)
		}
	}},

	{"User.CreateItem files as the person", func(t *testing.T, f *Fixture) {
		needSignIn(t, f)
		f.Issue("alice", "seed", "", "devbot/ready") // so the label exists
		it, err := f.P.AsUser(f.SignIn("wendy", forge.PermWrite)).CreateItem(ctx, f.Repo, "Title", "Body", []string{"devbot/ready"})
		must(t, err)
		got, err := f.P.GetItem(ctx, it.Ref)
		must(t, err)
		if got.Title != "Title" || got.Body != "Body" || got.Author != "wendy" || !sameSet(got.Labels, []string{"devbot/ready"}) || !it.HasLabel("devbot/ready") {
			t.Fatalf("got %+v, returned %+v", got, it)
		}
		// Someone who may not label either is refused or loses the label.
		it, err = f.P.AsUser(f.SignIn("rita", forge.PermRead)).CreateItem(ctx, f.Repo, "Title", "Body", []string{"devbot/ready"})
		if err == nil && it.HasLabel("devbot/ready") || err != nil && !errors.Is(err, forge.ErrForbidden) {
			t.Fatalf("a reader's labelled issue: %+v, %v", it, err)
		}
	}},

	{"User.CloseItem closes the person's own issue", func(t *testing.T, f *Fixture) {
		needSignIn(t, f)
		u := f.P.AsUser(f.SignIn("rita", forge.PermRead))
		it, err := u.CreateItem(ctx, f.Repo, "Title", "Body", nil)
		must(t, err)
		must(t, u.CloseItem(ctx, it.Ref))
		if got, err := f.P.GetItem(ctx, it.Ref); err != nil || got.State != "closed" {
			t.Fatalf("got %+v, %v", got, err)
		}
	}},

	{"User.Comment posts as the person", func(t *testing.T, f *Fixture) {
		needSignIn(t, f)
		n := f.Issue("alice", "t", "b")
		must(t, f.P.AsUser(f.SignIn("rita", forge.PermRead)).Comment(ctx, item(f, n), "An answer"))
		cs, err := f.P.ListComments(ctx, item(f, n))
		must(t, err)
		if len(cs) != 1 || cs[0].Body != "An answer" || cs[0].Author != "rita" {
			t.Fatalf("got %+v", cs)
		}
	}},

	{"User.SetLabels as the person, and a reader is refused", func(t *testing.T, f *Fixture) {
		needSignIn(t, f)
		f.Issue("alice", "seed", "", "devbot/working") // so the label exists
		n := f.Issue("alice", "t", "b", "bug", "devbot/ready")
		u := f.P.AsUser(f.SignIn("wendy", forge.PermWrite))
		must(t, u.SetLabels(ctx, item(f, n), []string{"devbot/working"}, []string{"devbot/ready"}))
		must(t, u.SetLabels(ctx, item(f, n), []string{"devbot/working"}, []string{"devbot/ready"}))
		if got := labels(t, f, n); !sameSet(got, []string{"bug", "devbot/working"}) {
			t.Fatalf("after the change: %v", got)
		}
		if err := f.P.AsUser(f.SignIn("rita", forge.PermRead)).SetLabels(ctx, item(f, n), nil, []string{"bug"}); !errors.Is(err, forge.ErrForbidden) {
			t.Fatalf("a reader's label change: %v", err)
		}
		if got := labels(t, f, n); !sameSet(got, []string{"bug", "devbot/working"}) {
			t.Fatalf("after the refusal: %v", got)
		}
	}},

	{"User calls on a repo the person can't see are ErrNotFound", func(t *testing.T, f *Fixture) {
		needSignIn(t, f)
		u := f.P.AsUser(f.SignIn("wendy", forge.PermWrite))
		if _, err := u.CreateItem(ctx, f.NoAccess, "Title", "Body", nil); !errors.Is(err, forge.ErrNotFound) {
			t.Errorf("CreateItem: %v", err)
		}
		if err := u.Comment(ctx, forge.ItemRef{Repo: f.NoAccess, Number: 1}, "x"); !errors.Is(err, forge.ErrNotFound) {
			t.Errorf("Comment: %v", err)
		}
	}},

	{"CurrentUser, and a token the forge doesn't accept", func(t *testing.T, f *Fixture) {
		needSignIn(t, f)
		if login, err := f.P.CurrentUser(ctx, f.SignIn("wendy", forge.PermWrite)); login != "wendy" || err != nil {
			t.Errorf("CurrentUser: %q, %v", login, err)
		}
		if _, err := f.P.CurrentUser(ctx, "not-a-token"); !errors.Is(err, forge.ErrUnauthorized) {
			t.Errorf("CurrentUser with a bad token: %v", err)
		}
		if _, err := f.P.AsUser("not-a-token").Permission(ctx, f.Repo); !errors.Is(err, forge.ErrUnauthorized) {
			t.Errorf("Permission with a bad token: %v", err)
		}
	}},
}

func needSignIn(t *testing.T, f *Fixture) {
	t.Helper()
	if f.SignIn == nil {
		t.Skip("this adapter's fake doesn't model signed-in people")
	}
}

package github

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/forgetest"
)

// seedPR opens a PR on acme/app the way a person would, from feature at
// f00d onto main at ba5e, and returns its number.
func (f *fakeGitHub) seedPR(t *testing.T, change func(is *fakeIssue)) int {
	fx := f.fixture(t)
	fx.Branch("feature", "f00d")
	n := fx.PR(forgetest.PRSeed{Author: "alice", Title: "t", Head: "feature", HeadSHA: "f00d", Base: "main", MergeBase: "ba5e"})
	if change != nil {
		f.mu.Lock()
		change(f.repos["acme/app"].issues[n])
		f.mu.Unlock()
	}
	return n
}

func TestGetPRMapsMergeableState(t *testing.T) {
	for _, c := range []struct {
		state                      string
		conflicted                 bool
		mergeable, behind, blocked bool
	}{
		{state: "clean", mergeable: true},
		{state: "has_hooks", mergeable: true},
		{state: "unstable", mergeable: true},
		{state: "draft", mergeable: true},
		{state: "behind", mergeable: true, behind: true},
		{state: "blocked", mergeable: true, blocked: true},
		{conflicted: true},
	} {
		t.Run(fmt.Sprint(c.state, c.conflicted), func(t *testing.T) {
			f := newFakeGitHub(t, "")
			n := f.seedPR(t, func(is *fakeIssue) { is.mergeState, is.conflicted = c.state, c.conflicted })
			p, err := f.client().GetPR(ctx, repo, n)
			if err != nil {
				t.Fatal(err)
			}
			if p.Mergeable != c.mergeable || p.Behind != c.behind || p.Blocked != c.blocked || p.MergeableUnknown {
				t.Errorf("got mergeable %v, behind %v, blocked %v, unknown %v", p.Mergeable, p.Behind, p.Blocked, p.MergeableUnknown)
			}
		})
	}
}

// GitHub computes mergeability in the background, reporting null until
// it's done: GetPR asks again a few times, then says it doesn't know yet,
// which isn't the same as a conflict.
func TestGetPRRetriesUnknownMergeable(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	_, s := withClock(f, c)
	n := f.seedPR(t, func(is *fakeIssue) { is.unknownFor, is.conflicted = 2, true })
	p, err := c.GetPR(ctx, repo, n)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mergeable || p.MergeableUnknown || !slices.Equal(s.waits, []time.Duration{time.Second, 2 * time.Second}) {
		t.Errorf("computed after two reads: mergeable %v, unknown %v, waits %v", p.Mergeable, p.MergeableUnknown, s.waits)
	}

	s.waits = nil
	n = f.seedPR(t, func(is *fakeIssue) { is.unknownFor = 100 })
	p, err = c.GetPR(ctx, repo, n)
	if err != nil {
		t.Fatal(err)
	}
	if !p.MergeableUnknown || !p.Mergeable || p.Blocked || len(s.waits) != mergeableTries-1 {
		t.Errorf("never computed: mergeable %v, unknown %v, waits %v", p.Mergeable, p.MergeableUnknown, s.waits)
	}
}

func TestOpenPRMergeabilityIsUnknown(t *testing.T) {
	f := newFakeGitHub(t, "")
	f.fixture(t).Branch("devbot/1-x", "c0ffee")
	p, err := f.client().OpenPR(ctx, repo, "devbot/1-x", "main", "T", "B")
	if err != nil {
		t.Fatal(err)
	}
	if !p.MergeableUnknown || !p.Mergeable || p.HeadSHA != "c0ffee" || p.Author != "devbot[bot]" {
		t.Errorf("got %+v", p)
	}
	if _, err := f.client().OpenPR(ctx, repo, "no-such-branch", "main", "T", "B"); err == nil {
		t.Error("opened a PR from a missing branch")
	}
}

func run(id int64, name, status, conclusion string) fakeRun {
	r := fakeRun{ID: id, Name: name, Status: status, HTMLURL: "https://github.example/run/" + fmt.Sprint(id)}
	if conclusion != "" {
		r.Conclusion = &conclusion
	}
	return r
}

func TestCombinedChecks(t *testing.T) {
	var many []fakeRun
	for i := range 150 {
		many = append(many, run(int64(i+1), fmt.Sprintf("job %03d", i), "completed", "success"))
	}
	for _, c := range []struct {
		name     string
		statuses []fakeStatus
		runs     []fakeRun
		want     forge.CheckState
		contexts []string
	}{
		{name: "nothing reported", want: forge.ChecksNone},
		{name: "Actions only", runs: []fakeRun{run(1, "build", "completed", "success"), run(2, "lint", "completed", "neutral"), run(3, "e2e", "completed", "skipped")},
			want: forge.ChecksSuccess, contexts: []string{"build", "lint", "e2e"}},
		{name: "statuses only", statuses: []fakeStatus{{Context: "ci/jenkins", State: "success"}, {Context: "ci/lint", State: "error"}},
			want: forge.ChecksFailure, contexts: []string{"ci/jenkins", "ci/lint"}},
		{name: "mixed, both passing", statuses: []fakeStatus{{Context: "ci/jenkins", State: "success"}}, runs: []fakeRun{run(1, "build", "completed", "success")},
			want: forge.ChecksSuccess, contexts: []string{"ci/jenkins", "build"}},
		{name: "mixed, a run failing", statuses: []fakeStatus{{Context: "ci/jenkins", State: "success"}}, runs: []fakeRun{run(1, "build", "completed", "timed_out")},
			want: forge.ChecksFailure, contexts: []string{"ci/jenkins", "build"}},
		{name: "mixed, a status failing", statuses: []fakeStatus{{Context: "ci/jenkins", State: "failure"}}, runs: []fakeRun{run(1, "build", "completed", "success")},
			want: forge.ChecksFailure, contexts: []string{"ci/jenkins", "build"}},
		{name: "pending status", statuses: []fakeStatus{{Context: "ci/jenkins", State: "pending"}}, runs: []fakeRun{run(1, "build", "completed", "failure")},
			want: forge.ChecksPending, contexts: []string{"ci/jenkins", "build"}},
		{name: "queued run", runs: []fakeRun{run(1, "build", "queued", ""), run(2, "lint", "completed", "success")}, want: forge.ChecksPending},
		{name: "running run", runs: []fakeRun{run(1, "build", "in_progress", "")}, want: forge.ChecksPending},
		{name: "a re-run supersedes the failure", runs: []fakeRun{run(7, "build", "completed", "success"), run(3, "build", "completed", "failure")},
			want: forge.ChecksSuccess, contexts: []string{"build"}},
		{name: "a re-run in progress", runs: []fakeRun{run(3, "build", "completed", "success"), run(7, "build", "in_progress", "")},
			want: forge.ChecksPending, contexts: []string{"build"}},
		{name: "every page", runs: append(slices.Clone(many), run(999, "late", "completed", "failure")), want: forge.ChecksFailure},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeGitHub(t, "")
			f.mu.Lock()
			f.repos["acme/app"].statuses["f00d"] = c.statuses
			f.repos["acme/app"].runs["f00d"] = c.runs
			f.mu.Unlock()
			got, details, err := f.client().CombinedChecks(ctx, repo, "f00d")
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("state %s, want %s: %+v", got, c.want, details)
			}
			var ctxs []string
			for _, d := range details {
				ctxs = append(ctxs, d.Context)
			}
			if c.contexts != nil && !slices.Equal(ctxs, c.contexts) {
				t.Errorf("details for %v, want %v", ctxs, c.contexts)
			}
		})
	}
}

// A cancelled Actions job is reported as cancelled, and re-running it
// re-runs the failed jobs of its workflow run, once however many of the
// run's jobs it names, as the implementer App with Actions write. A check
// run from another App has no workflow run to re-run.
func TestRerunCancelledChecks(t *testing.T) {
	jobs := func() []fakeRun {
		rs := []fakeRun{run(5, "build", "completed", "cancelled"), run(6, "test", "completed", "cancelled"),
			run(7, "lint", "completed", "skipped"), run(8, "other app", "completed", "cancelled")}
		rs[0].HTMLURL = "https://github.com/acme/app/actions/runs/900/job/5"
		// A check run's own page, with the job's under details_url.
		rs[1].HTMLURL = "https://github.com/acme/app/runs/6"
		rs[1].DetailsURL = "https://github.com/acme/app/actions/runs/900/job/6"
		rs[3].HTMLURL = "https://github.com/acme/app/runs/8"
		return rs
	}
	f := newFakeGitHub(t, "")
	f.mu.Lock()
	f.repos["acme/app"].runs["f00d"] = jobs()
	f.installs[11].perms["actions"] = "write"
	f.mu.Unlock()
	st, details, err := f.client().CombinedChecks(ctx, repo, "f00d")
	if err != nil || st != forge.ChecksFailure || len(details) != 4 {
		t.Fatalf("got %s, %+v, %v", st, details, err)
	}
	for i, d := range details {
		if want := i != 2; d.Failed != want || d.Cancelled != want {
			t.Errorf("%s: failed %v, cancelled %v; want %v", d.Context, d.Failed, d.Cancelled, want)
		}
	}
	if err := f.client().RerunChecks(ctx, repo, "f00d", details[:2]); err != nil {
		t.Fatal(err)
	}
	if got := f.repos["acme/app"].reruns; !slices.Equal(got, []string{"900"}) {
		t.Fatalf("re-ran %v, want run 900 once", got)
	}
	if err := f.client().RerunChecks(ctx, repo, "f00d", details[3:]); !errors.Is(err, forge.ErrNotRerunnable) {
		t.Fatalf("another App's check run: %v, want ErrNotRerunnable", err)
	}

	// Without Actions write the token isn't given it, and GitHub refuses.
	f = newFakeGitHub(t, "")
	f.mu.Lock()
	f.repos["acme/app"].runs["f00d"] = jobs()
	f.mu.Unlock()
	if err := f.client().RerunChecks(ctx, repo, "f00d", details[:1]); !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("without Actions write: %v, want ErrForbidden", err)
	}
}

func TestCheckRunConclusions(t *testing.T) {
	for _, c := range []struct{ status, conclusion, want string }{
		{"completed", "success", "success"}, {"completed", "neutral", "success"}, {"completed", "skipped", "success"},
		{"completed", "failure", "failure"}, {"completed", "cancelled", "failure"}, {"completed", "timed_out", "failure"},
		{"completed", "action_required", "failure"}, {"completed", "startup_failure", "failure"}, {"completed", "stale", "failure"},
		{"queued", "", "pending"}, {"in_progress", "", "pending"}, {"pending", "", "pending"}, {"waiting", "", "pending"},
	} {
		if got := runOutcome(c.status, c.conclusion); got != c.want {
			t.Errorf("%s/%s: %s, want %s", c.status, c.conclusion, got, c.want)
		}
	}
}

func TestReviewsMarkDismissedAndSkipDrafts(t *testing.T) {
	f := newFakeGitHub(t, "")
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	n := f.seedPR(t, func(is *fakeIssue) {
		is.reviews = []fakeReview{
			{ID: 1, User: fakeUser{"devbot-review[bot]"}, State: "DISMISSED", CommitID: "0ld", Body: "was approve", SubmittedAt: at},
			{ID: 2, User: fakeUser{"bob"}, State: "PENDING", CommitID: "f00d"},
			{ID: 3, User: fakeUser{"carol"}, State: "CHANGES_REQUESTED", CommitID: "f00d", Body: "no", SubmittedAt: at},
			{ID: 4, User: fakeUser{"dave"}, State: "COMMENTED", CommitID: "f00d", SubmittedAt: at},
		}
	})
	rs, err := f.client().Reviews(ctx, repo, n)
	if err != nil {
		t.Fatal(err)
	}
	want := []forge.Review{
		{ID: 1, Author: "devbot-review[bot]", Verdict: forge.VerdictComment, CommitSHA: "0ld", Body: "was approve", SubmittedAt: at, Dismissed: true},
		{ID: 3, Author: "carol", Verdict: forge.VerdictRequestChanges, CommitSHA: "f00d", Body: "no", SubmittedAt: at},
		{ID: 4, Author: "dave", Verdict: forge.VerdictComment, CommitSHA: "f00d", SubmittedAt: at},
	}
	if !slices.Equal(rs, want) {
		t.Errorf("got %+v", rs)
	}
}

func TestReviewIsPinnedToTheSHA(t *testing.T) {
	f := newFakeGitHub(t, "")
	n := f.seedPR(t, nil)
	c := f.client()
	if err := c.Review(ctx, repo, n, "0ld5ha", forge.VerdictApprove, "LGTM"); err != nil {
		t.Fatal(err)
	}
	if err := c.Review(ctx, repo, n, "f00d", "maybe", "?"); err == nil {
		t.Error("an unknown verdict was posted")
	}
	f.mu.Lock()
	rv := f.repos["acme/app"].issues[n].reviews
	f.mu.Unlock()
	if len(rv) != 1 || rv[0].CommitID != "0ld5ha" || rv[0].User.Login != "devbot-review[bot]" || rv[0].State != "APPROVED" {
		t.Errorf("got %+v", rv)
	}
}

// TestMerge also holds the 405s apart, on GitHub.com and GHES: a refusal
// goes to a person, and the ones GitHub means for now are retried.
func TestMerge(t *testing.T) {
	for _, base := range []string{"", "/api/v3"} {
		t.Run(cmp.Or(base, "github.com"), func(t *testing.T) { testMerge(t, base) })
	}
}

func testMerge(t *testing.T, base string) {
	f := newFakeGitHub(t, base)
	c := f.client()
	n := f.seedPR(t, nil)
	if err := c.Merge(ctx, repo, n, "f00d", "fast-forward"); err == nil {
		t.Error("merged with an unknown method")
	}
	if err := c.Merge(ctx, repo, n, "0ld5ha", "rebase"); !errors.Is(err, forge.ErrHeadMoved) || errors.Is(err, forge.ErrNotMergeable) {
		t.Errorf("head moved: got %v", err)
	}
	if err := c.Merge(ctx, repo, n, "f00d", "rebase"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	is := f.repos["acme/app"].issues[n]
	by, method := is.mergedBy, is.mergeMethod
	f.mu.Unlock()
	if by != "devbot-review[bot]" || method != "rebase" {
		t.Errorf("merged by %s with %s, want the reviewer with rebase", by, method)
	}

	for _, tc := range []struct {
		name   string
		change func(*fakeIssue)
		want   error
	}{
		{"base modified", func(is *fakeIssue) { is.baseModified = true }, forge.ErrMergePending},
		{"still computing", func(is *fakeIssue) { is.unknownFor = 100 }, forge.ErrMergePending},
		{"computing a conflict", func(is *fakeIssue) { is.unknownFor, is.conflicted = 100, true }, forge.ErrMergePending},
		{"conflict", func(is *fakeIssue) { is.conflicted = true }, forge.ErrNotMergeable},
		{"blocked", func(is *fakeIssue) { is.mergeState = "blocked" }, forge.ErrNotMergeable},
	} {
		n := f.seedPR(t, tc.change)
		err := c.Merge(ctx, repo, n, "f00d", "squash")
		if !errors.Is(err, tc.want) || errors.Is(err, forge.ErrHeadMoved) ||
			errors.Is(err, forge.ErrNotMergeable) == errors.Is(err, forge.ErrMergePending) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
}

// A job's log needs Actions read, which the Apps may not have: the check
// run's output comes back with the refusal. Past MaxAnnotations, the rest
// are left unread.
func TestCheckReportPartial(t *testing.T) {
	f := newFakeGitHub(t, "")
	fx := f.fixture(t)
	fx.FailedJob("c3", "build", "boom\n")
	many := forge.CheckReport{Title: "lint"}
	for i := range forge.MaxAnnotations + 150 {
		many.Annotations = append(many.Annotations, forge.Annotation{Path: "a.go", StartLine: i + 1, EndLine: i + 1, Level: "warning", Message: "m"})
	}
	fx.CheckRun("c3", "lint", many)
	f.mu.Lock()
	f.repos["acme/app"].outputs[1] = forge.CheckReport{Title: "Build failed", Summary: "exit 1"}
	for _, in := range f.installs {
		delete(in.perms, "actions")
	}
	f.mu.Unlock()
	_, ds, err := fx.P.CombinedChecks(t.Context(), fx.Repo, "c3")
	if err != nil || len(ds) != 2 {
		t.Fatalf("got %+v, %v", ds, err)
	}
	r, err := fx.P.CheckReport(t.Context(), fx.Repo, "c3", ds[0], 10)
	if !errors.Is(err, forge.ErrForbidden) || r.Title != "Build failed" || r.Summary != "exit 1" || !r.HasLog || r.Log != "" {
		t.Errorf("without Actions read: %+v, %v", r, err)
	}
	r, err = fx.P.CheckReport(t.Context(), fx.Repo, "c3", ds[1], 10)
	if err != nil || len(r.Annotations) != forge.MaxAnnotations || !r.MoreAnnotations || r.Annotations[forge.MaxAnnotations-1].StartLine != forge.MaxAnnotations {
		t.Errorf("got %d annotations, more %v, %v", len(r.Annotations), r.MoreAnnotations, err)
	}
}

// A compare GitHub answers "No common ancestor" for is a base rewritten
// under the PR: behind, and unrelated. Any other failed compare, a 404 for
// a missing commit or a 5xx, says nothing, and leaves the PR as it was.
func TestGetPRCompareWithoutACommonAncestor(t *testing.T) {
	for _, c := range []struct {
		name              string
		status            int
		body              string
		behind, unrelated bool
	}{
		{"no common ancestor", 404, `{"message":"No common ancestor between main and f00d.","status":"404"}`, true, true},
		{"not found", 404, `{"message":"Not Found","status":"404"}`, false, false},
		{"server error", 502, `{"message":"Server Error"}`, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeGitHub(t, "")
			n := f.seedPR(t, nil)
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.Contains(r.URL.Path, "/compare/") {
					return false
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				io.WriteString(w, c.body)
				return true
			}
			p, err := f.client().GetPR(ctx, repo, n)
			if err != nil {
				t.Fatal(err)
			}
			if p.Behind != c.behind || p.Unrelated != c.unrelated || !p.Mergeable {
				t.Errorf("got behind %v, unrelated %v, mergeable %v; want behind %v, unrelated %v", p.Behind, p.Unrelated, p.Mergeable, c.behind, c.unrelated)
			}
		})
	}
}

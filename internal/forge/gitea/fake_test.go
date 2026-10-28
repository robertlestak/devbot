package gitea

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/forgetest"
)

// fakeGitea is an httptest fake of the parts of Gitea's API the adapter
// uses, for the provider conformance suite. Tokens are users: impl-tok is
// devbot, rev-tok is devbot-review, and both can write to acme/app only.
// alice-tok is alice signed in, with the permission perms gives her.
type fakeGitea struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	calls    int
	next     int // issue and PR numbers, one sequence as in Gitea
	nextID   int64
	issues   map[int]*fakeIssue
	labels   []label
	branches map[string]string
	branchAt map[string]time.Time         // when each branch last moved
	files    map[string][]byte            // ref + ":" + path
	statuses map[string]map[string]string // sha -> context -> state
	details  map[string][2]string         // sha + " " + context -> description, target URL
	runs     []fakeRun                    // Actions runs
	jobs     []fakeJob                    // their jobs
	reruns   []int64                      // runs whose failed jobs were re-run, in order
	perms    map[string]string            // user -> permission on acme/app
	assets   map[int][]string             // issue -> attachment UUIDs
	blobs    map[string][]byte            // UUID -> content
	tags     [][2]string                  // name, commit; newest first
	history  map[string][]string          // commit -> its parents
	// protected branches, with the status check contexts each requires
	protected   map[string][]string
	maxItems    int  // MAX_RESPONSE_ITEMS: the largest page served
	moreRepos   int  // repos past acme/app and acme/private that /user/repos lists
	noTotal     bool // lists come without X-Total-Count
	noLink      bool // lists come without a Link header
	dropUnknown bool // label writes leave unknown IDs off instead of failing
}

type fakeIssue struct {
	number              int
	title, body, author string
	state               string
	labels              []int64
	created, updated    time.Time
	comments            []comment
	timeline            []labelEvent

	isPR               bool
	head, headSHA      string
	base, mergeBase    string
	conflicted, merged bool
	mergeSHA           string
	checking           bool // Gitea hasn't finished testing the merge
	checkReads         int  // how many more reads of the PR find it still checking
	fork               bool
	maintainerEdit     bool
	closed             *time.Time
	reviews            []fakeReview
}

// fakeRun is an Actions run: its ID, the commit it ran on, and its page.
type fakeRun struct {
	id           int64
	sha, htmlURL string
}

// fakeJob is a job of an Actions run, with its log.
type fakeJob struct {
	id, run      int64
	htmlURL, log string
	expired      bool // its log has been cleaned up
}

// failedJob adds a failed job named ctx to sha's Actions run, starting the
// run if sha has none.
func (f *fakeGitea) failedJob(sha, ctx, log string) {
	i := slices.IndexFunc(f.runs, func(run fakeRun) bool { return run.sha == sha })
	if i < 0 {
		f.runs = append(f.runs, fakeRun{id: f.id(), sha: sha, htmlURL: fmt.Sprintf("%s/acme/app/actions/runs/%d", f.srv.URL, 100+len(f.runs))})
		i = len(f.runs) - 1
	}
	run, n := f.runs[i], 0
	for _, j := range f.jobs {
		if j.run == run.id {
			n++
		}
	}
	u := fmt.Sprintf("%s/jobs/%d", run.htmlURL, n)
	f.jobs = append(f.jobs, fakeJob{id: f.id(), run: run.id, htmlURL: u, log: log})
	if f.statuses[sha] == nil {
		f.statuses[sha] = map[string]string{}
	}
	f.statuses[sha][ctx] = "failure"
	f.details[sha+" "+ctx] = [2]string{"Failing after 3s", u}
}

type fakeReview struct {
	user, state, sha, body string
	at                     time.Time
	lines                  []forge.ReviewComment
}

var fakeTokens = map[string]string{"impl-tok": "devbot", "rev-tok": "devbot-review", "alice-tok": "alice"}

func newFakeGitea(t *testing.T) *fakeGitea {
	f := &fakeGitea{
		t: t, issues: map[int]*fakeIssue{}, branches: map[string]string{"main": "ba5e"}, branchAt: map[string]time.Time{"main": time.Now()}, files: map[string][]byte{},
		statuses: map[string]map[string]string{}, details: map[string][2]string{}, perms: map[string]string{"devbot": "admin", "devbot-review": "write"},
		history: map[string][]string{}, assets: map[int][]string{}, blobs: map[string][]byte{}, protected: map[string][]string{}, maxItems: 50,
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitea) id() int64 { f.nextID++; return f.nextID }

// now is the time a comment, review or label event gets: to the second, as
// Gitea keeps them.
func now() time.Time { return time.Now().Truncate(time.Second) }

func (f *fakeGitea) labelID(name string) int64 {
	for _, l := range f.labels {
		if l.Name == name {
			return l.ID
		}
	}
	l := label{ID: f.id(), Name: name, Color: "ededed"}
	f.labels = append(f.labels, l)
	return l.ID
}

func (f *fakeGitea) label(id int64) (label, bool) {
	for _, l := range f.labels {
		if l.ID == id {
			return l, true
		}
	}
	return label{}, false
}

// recreateLabel deletes the label and creates it again under a new ID, as
// someone tidying labels in the UI might. Issues lose it, as in Gitea.
func (f *fakeGitea) recreateLabel(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	old := f.labelID(name)
	f.labels = slices.DeleteFunc(f.labels, func(l label) bool { return l.ID == old })
	for _, is := range f.issues {
		is.labels = slices.DeleteFunc(is.labels, func(id int64) bool { return id == old })
	}
	f.labelID(name)
}

// setLabels changes an issue's labels as who, keeping the timeline as
// Gitea does. An unknown label ID fails it, or with dropUnknown is left
// off, as older Gitea versions do.
func (f *fakeGitea) setLabels(is *fakeIssue, who string, ids []int64) bool {
	for _, id := range ids {
		if _, ok := f.label(id); !ok && !f.dropUnknown {
			return false
		}
	}
	ids = slices.DeleteFunc(slices.Clone(ids), func(id int64) bool { _, ok := f.label(id); return !ok })
	now := now()
	for _, id := range is.labels {
		if !slices.Contains(ids, id) {
			l, _ := f.label(id)
			is.timeline = append(is.timeline, labelEvent{Type: "label", User: user{Login: who}, Label: l, CreatedAt: now})
		}
	}
	var next []int64
	for _, id := range ids {
		if slices.Contains(next, id) {
			continue
		}
		if !slices.Contains(is.labels, id) {
			l, _ := f.label(id)
			is.timeline = append(is.timeline, labelEvent{Type: "label", Body: "1", User: user{Login: who}, Label: l, CreatedAt: now})
		}
		next = append(next, id)
	}
	is.labels = next
	return true
}

func (f *fakeGitea) fixture(t *testing.T) *forgetest.Fixture {
	locked := func(fn func()) {
		f.mu.Lock()
		defer f.mu.Unlock()
		fn()
	}
	newIssue := func(is *fakeIssue) int {
		f.next++
		is.number, is.state, is.created, is.updated = f.next, "open", now(), now()
		f.issues[is.number] = is
		return is.number
	}
	c := New(Config{Name: "gitea", URL: f.srv.URL, ImplementerUser: "devbot", ImplementerTok: "impl-tok",
		ReviewerUser: "devbot-review", ReviewerTok: "rev-tok"})
	c.recheck = time.Millisecond
	return &forgetest.Fixture{
		P:        c,
		Repo:     forge.RepoRef{Provider: "gitea", Owner: "acme", Name: "app"},
		NoAccess: forge.RepoRef{Provider: "gitea", Owner: "acme", Name: "private"},
		Issue: func(author, title, body string, labels ...string) (n int) {
			locked(func() {
				is := &fakeIssue{title: title, body: body, author: author}
				for _, l := range labels {
					is.labels = append(is.labels, f.labelID(l))
				}
				n = newIssue(is)
			})
			return n
		},
		Comment: func(n int, author, body string) {
			locked(func() {
				is := f.issues[n]
				is.comments = append(is.comments, comment{ID: f.id(), Body: body, User: user{Login: author}, CreatedAt: now()})
			})
		},
		PR: func(p forgetest.PRSeed) (n int) {
			locked(func() {
				if p.Unrelated {
					p.MergeBase = "" // Gitea's merge_base for histories that share no commit
				}
				n = newIssue(&fakeIssue{title: p.Title, body: p.Body, author: p.Author, isPR: true, head: p.Head, headSHA: p.HeadSHA,
					base: p.Base, mergeBase: p.MergeBase, conflicted: p.Conflicted, checking: p.Checking, fork: p.Fork, maintainerEdit: p.MaintainerCanModify})
			})
			return n
		},
		// Gitea keeps a title change in the timeline, and a body's history only
		// where its web UI shows it.
		Edit: func(n int, editor, title, body string) {
			locked(func() {
				is := f.issues[n]
				if title != is.title {
					is.timeline = append(is.timeline, labelEvent{Type: "change_title", User: user{Login: editor}, OldTitle: is.title, NewTitle: title, CreatedAt: time.Now()})
				}
				is.title, is.body, is.updated = title, body, time.Now()
			})
		},
		Close: func(n int) { locked(func() { f.close(f.issues[n]) }) },
		// A push to an open PR's head branch moves the PR's head.
		Branch: func(name, sha string) {
			locked(func() {
				f.branches[name], f.branchAt[name] = sha, time.Now()
				for _, is := range f.issues {
					if is.isPR && is.head == name && is.state == "open" && !is.fork {
						is.headSHA = sha
					}
				}
			})
		},
		File: func(ref, path string, data []byte) { locked(func() { f.files[ref+":"+path] = data }) },
		LineReview: func(n int, author, sha string, v forge.Verdict, body string, lines ...forge.ReviewComment) {
			locked(func() {
				state := map[forge.Verdict]string{forge.VerdictApprove: "APPROVED", forge.VerdictRequestChanges: "REQUEST_CHANGES", forge.VerdictComment: "COMMENT"}[v]
				rv := fakeReview{user: author, state: state, sha: sha, body: body, at: now()}
				for _, c := range lines {
					c.ID = f.id()
					rv.lines = append(rv.lines, c)
				}
				is := f.issues[n]
				is.reviews = append(is.reviews, rv)
			})
		},
		Status: func(sha, ctx, state string) {
			locked(func() {
				if f.statuses[sha] == nil {
					f.statuses[sha] = map[string]string{}
				}
				f.statuses[sha][ctx] = state
			})
		},
		// Each commit gets one Actions run, and each failed job its place in
		// it, which its status links to as Gitea's do.
		FailedJob: func(sha, ctx, log string) {
			locked(func() { f.failedJob(sha, ctx, log) })
		},
		Grant: func(u string, p forge.Permission) { locked(func() { f.perms[u] = p.String() }) },
		RequireChecks: func(branch string, checks ...string) {
			locked(func() { f.protected[branch] = checks })
		},
		Tag: func(name, sha string) {
			locked(func() { f.tags = append([][2]string{{name, sha}}, f.tags...) })
		},
		Descends: func(sha string, parents ...string) {
			locked(func() {
				f.history[sha] = append(f.history[sha], parents...)
				// A parent is a commit the forge knows, history or not.
				for _, p := range parents {
					if _, ok := f.history[p]; !ok {
						f.history[p] = nil
					}
				}
			})
		},
		Calls: func() (n int) {
			locked(func() { n = f.calls })
			return n
		},
	}
}

func TestConformance(t *testing.T) {
	forgetest.Run(t, func(t *testing.T) *forgetest.Fixture { return newFakeGitea(t).fixture(t) })
}

// Gitea's merge refusals: 409 is the head having moved, the "try again
// later" 405 is Gitea still checking the PR, and any other 405 is the PR
// not mergeable as it stands.
func TestMergeErrors(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name      string
		sha       string
		set       func(*fakeIssue)
		want, not []error
	}{
		{name: "head moved", sha: "0ld5ha", want: []error{forge.ErrHeadMoved}, not: []error{forge.ErrNotMergeable, forge.ErrMergePending}},
		{name: "checking", sha: "f00d", set: func(is *fakeIssue) { is.checking = true },
			want: []error{forge.ErrMergePending}, not: []error{forge.ErrNotMergeable, forge.ErrHeadMoved}},
		{name: "conflict", sha: "f00d", set: func(is *fakeIssue) { is.conflicted = true },
			want: []error{forge.ErrNotMergeable}, not: []error{forge.ErrMergePending, forge.ErrHeadMoved}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeGitea(t)
			fx := f.fixture(t)
			n := fx.PR(forgetest.PRSeed{Author: "alice", Title: "t", Head: "feature", HeadSHA: "f00d", Base: "main", MergeBase: "ba5e"})
			if c.set != nil {
				c.set(f.issues[n])
			}
			err := fx.P.Merge(ctx, fx.Repo, n, c.sha, "squash")
			for _, w := range c.want {
				if !errors.Is(err, w) {
					t.Errorf("got %v, want %v", err, w)
				}
			}
			for _, w := range c.not {
				if errors.Is(err, w) {
					t.Errorf("got %v, must not be %v", err, w)
				}
			}
			if f.issues[n].merged {
				t.Error("merged anyway")
			}
		})
	}
}

func (f *fakeGitea) issueJSON(is *fakeIssue) issue {
	out := issue{Number: is.number, Title: is.title, Body: is.body, State: is.state, User: user{Login: is.author},
		HTMLURL: f.srv.URL + "/acme/app/issues/" + strconv.Itoa(is.number), CreatedAt: is.created, UpdatedAt: is.updated,
		Labels: []label{}}
	out.Repository = &struct {
		FullName string `json:"full_name"`
	}{"acme/app"}
	for _, id := range is.labels {
		l, _ := f.label(id)
		out.Labels = append(out.Labels, l)
	}
	if is.isPR {
		out.PullRequest = &struct{}{}
	}
	return out
}

// A fork head is in mallory's copy of the repo.
func (f *fakeGitea) pullJSON(is *fakeIssue) pull {
	head := branchRef{Ref: is.head, SHA: is.headSHA, Repo: &repoName{FullName: "acme/app"}}
	if is.fork {
		head.Repo.FullName = "mallory/app"
	}
	return pull{Number: is.number, Title: is.title, Body: is.body, State: is.state, Merged: is.merged, Mergeable: !is.conflicted && !is.checking && is.checkReads == 0 && !wip(is.title),
		MergeBase: is.mergeBase, Head: head, Base: branchRef{Ref: is.base},
		HTMLURL: f.srv.URL + "/acme/app/pulls/" + strconv.Itoa(is.number), User: user{Login: is.author}, CreatedAt: is.created, ClosedAt: is.closed,
		Labels: f.issueJSON(is).Labels, AllowMaintainerEdit: is.maintainerEdit, MergeCommitID: is.mergeSHA}
}

func (f *fakeGitea) close(is *fakeIssue) {
	now := time.Now()
	is.state, is.closed = "closed", &now
}

// slice is Gitea's ?page=&limit= slice of all: 30 items by default, at most
// the server's MAX_RESPONSE_ITEMS. It sets X-Total-Count and, when there is
// more than one page, a Link header with next, last, first and prev, as
// Gitea does.
func slice[T any](f *fakeGitea, w http.ResponseWriter, r *http.Request, all []T) []T {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 30
	}
	limit = min(limit, f.maxItems)
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	n = max(n, 1)
	if !f.noTotal {
		w.Header().Set("X-Total-Count", strconv.Itoa(len(all)))
	}
	if last := max((len(all)+limit-1)/limit, 1); last > 1 && !f.noLink {
		at := func(p int, rel string) string {
			q := r.URL.Query()
			q.Set("page", strconv.Itoa(p))
			return fmt.Sprintf("<%s%s?%s>; rel=%q", f.srv.URL, r.URL.Path, q.Encode(), rel)
		}
		var links []string
		if n < last {
			links = append(links, at(n+1, "next"), at(last, "last"))
		}
		if n > 1 {
			links = append(links, at(1, "first"), at(n-1, "prev"))
		}
		w.Header().Set("Link", strings.Join(links, ","))
	}
	out := all[min((n-1)*limit, len(all)):min(n*limit, len(all))]
	if out == nil {
		out = []T{}
	}
	return out
}

// page serves a slice of a list.
func page[T any](f *fakeGitea, w http.ResponseWriter, r *http.Request, all []T) {
	writeJSON(w, 200, slice(f, w, r, all))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (f *fakeGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	who, ok := fakeTokens[strings.TrimPrefix(r.Header.Get("Authorization"), "token ")]
	if !ok {
		writeJSON(w, 401, map[string]string{"message": "token is required"})
		return
	}
	if id, ok := strings.CutPrefix(r.URL.Path, "/attachments/"); ok {
		b, found := f.blobs[id]
		if !found {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/api/v1/user/repos" {
		// Both bots see acme/private, but only read it.
		repo := func(name string, push bool) map[string]any {
			return map[string]any{"full_name": "acme/" + name, "name": name, "owner": map[string]string{"login": "acme"},
				"permissions": map[string]bool{"admin": false, "push": push, "pull": true}}
		}
		repos := []map[string]any{repo("app", true), repo("private", false)}
		for i := range f.moreRepos {
			repos = append(repos, repo(fmt.Sprintf("r%05d", i), true))
		}
		page(f, w, r, repos)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/api/v1/repos/issues/search" {
		var out []issue
		q := r.URL.Query()
		for _, n := range sortedKeys(f.issues) {
			is := f.issues[n]
			if is.isPR != (q.Get("type") == "pulls") || (q.Get("state") != "all" && is.state != q.Get("state")) || !slices.Contains(is.labels, f.labelID(q.Get("labels"))) {
				continue
			}
			out = append(out, f.issueJSON(is))
		}
		page(f, w, r, out)
		return
	}
	sub, ok := strings.CutPrefix(r.URL.Path, "/api/v1/repos/acme/app")
	if !ok {
		http.NotFound(w, r)
		return
	}
	// One label, by its ID or, for anything that isn't a number, its name:
	// a name's slashes come escaped, as one segment.
	if esc, ok := strings.CutPrefix(r.URL.EscapedPath(), "/api/v1/repos/acme/app/labels/"); ok && (r.Method == "GET" || r.Method == "PATCH") {
		key, _ := url.PathUnescape(esc)
		id, err := strconv.ParseInt(key, 10, 64)
		i := slices.IndexFunc(f.labels, func(l label) bool { return (err == nil && l.ID == id) || (err != nil && l.Name == key) })
		if i < 0 || strings.Contains(esc, "/") || (r.Method == "PATCH" && err != nil) {
			http.NotFound(w, r)
			return
		}
		if r.Method == "PATCH" {
			var in struct {
				Color, Description *string
			}
			json.NewDecoder(r.Body).Decode(&in)
			if in.Color != nil {
				f.labels[i].Color = strings.TrimPrefix(*in.Color, "#")
			}
			if in.Description != nil {
				f.labels[i].Description = *in.Description
			}
		}
		writeJSON(w, 200, f.labels[i])
		return
	}
	segs := strings.Split(strings.TrimPrefix(sub, "/"), "/")
	var is *fakeIssue
	if len(segs) >= 2 && (segs[0] == "issues" || segs[0] == "pulls") {
		n, _ := strconv.Atoi(segs[1])
		is = f.issues[n]
		if (is == nil || (segs[0] == "pulls" && !is.isPR)) && !(segs[1] == "" || n == 0) {
			http.NotFound(w, r)
			return
		}
	}
	var in map[string]any
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		json.NewDecoder(r.Body).Decode(&in)
	}
	ids := func() []int64 {
		var out []int64
		raw, _ := in["labels"].([]any)
		for _, v := range raw {
			out = append(out, int64(v.(float64)))
		}
		return out
	}
	str := func(k string) string { s, _ := in[k].(string); return s }
	if r.Method == "PATCH" && len(segs) == 3 && segs[0] == "issues" && segs[1] == "comments" {
		id, _ := strconv.ParseInt(segs[2], 10, 64)
		for _, is := range f.issues {
			for i := range is.comments {
				if c := &is.comments[i]; c.ID == id {
					c.Body, c.UpdatedAt = str("body"), now()
					writeJSON(w, 200, *c)
					return
				}
			}
		}
		http.NotFound(w, r)
		return
	}
	route := r.Method + " " + strings.Join(segs, "/")
	if is != nil {
		route = r.Method + " " + segs[0] + "/N"
		if len(segs) > 2 {
			route += "/" + strings.Join(segs[2:], "/")
		}
	}
	if len(segs) == 4 && r.Method == "GET" && segs[0] == "actions" && segs[1] == "runs" && segs[3] == "jobs" {
		id, _ := strconv.ParseInt(segs[2], 10, 64)
		var out []map[string]any
		for _, j := range f.jobs {
			if j.run == id {
				out = append(out, map[string]any{"id": j.id, "run_id": j.run, "html_url": j.htmlURL, "status": "completed", "conclusion": "failure"})
			}
		}
		writeJSON(w, 200, map[string]any{"total_count": len(out), "jobs": slice(f, w, r, out)})
		return
	}
	if len(segs) == 4 && r.Method == "GET" && segs[0] == "actions" && segs[1] == "jobs" && segs[3] == "logs" {
		id, _ := strconv.ParseInt(segs[2], 10, 64)
		i := slices.IndexFunc(f.jobs, func(j fakeJob) bool { return j.id == id })
		if i < 0 || f.jobs[i].expired {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, f.jobs[i].log)
		return
	}
	if route == "GET actions/runs" {
		var out []map[string]any
		for _, run := range f.runs {
			if run.sha == r.URL.Query().Get("head_sha") {
				out = append(out, map[string]any{"id": run.id, "html_url": run.htmlURL})
			}
		}
		writeJSON(w, 200, map[string]any{"total_count": len(out), "workflow_runs": slice(f, w, r, out)})
		return
	}
	if len(segs) == 4 && r.Method == "POST" && segs[0] == "actions" && segs[1] == "runs" && segs[3] == "rerun-failed-jobs" {
		id, _ := strconv.ParseInt(segs[2], 10, 64)
		if !slices.ContainsFunc(f.runs, func(run fakeRun) bool { return run.id == id }) {
			http.NotFound(w, r)
			return
		}
		if who != "devbot" {
			writeJSON(w, 403, map[string]string{"message": "user should have write permission"})
			return
		}
		f.reruns = append(f.reruns, id)
		w.WriteHeader(201)
		return
	}
	// A review's line comments, all in one list: Gitea doesn't page them.
	if is != nil && r.Method == "GET" && len(segs) == 5 && segs[2] == "reviews" && segs[4] == "comments" {
		id, _ := strconv.Atoi(segs[3])
		if id < 1 || id > len(is.reviews) {
			http.NotFound(w, r)
			return
		}
		rv := is.reviews[id-1]
		out := []map[string]any{}
		for _, c := range rv.lines {
			out = append(out, map[string]any{"id": c.ID, "pull_request_review_id": id, "user": user{Login: rv.user}, "body": c.Body,
				"path": c.Path, "position": c.Line, "original_position": 0, "commit_id": rv.sha, "created_at": rv.at})
		}
		writeJSON(w, 200, out)
		return
	}
	switch route {
	case "GET ":
		if who != "devbot" && who != "devbot-review" {
			// A signed-in person sees the repo as their permission allows.
			p := f.perms[who]
			if p == "" || p == "none" {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, 200, map[string]any{"full_name": "acme/app", "default_branch": "main",
				"permissions": map[string]bool{"admin": p == "admin", "push": p != "read", "pull": true}})
			return
		}
		writeJSON(w, 200, map[string]any{"full_name": "acme/app", "default_branch": "main", "permissions": map[string]bool{"push": true}})
	case "GET labels":
		page(f, w, r, f.labels)
	case "GET issues":
		var out []issue
		q := r.URL.Query()
		for _, n := range sortedKeys(f.issues) {
			is := f.issues[n]
			if is.isPR != (q.Get("type") == "pulls") || (q.Get("state") != "all" && is.state != q.Get("state")) || !slices.Contains(is.labels, f.labelID(q.Get("labels"))) {
				continue
			}
			out = append(out, f.issueJSON(is))
		}
		page(f, w, r, out)
	case "POST labels":
		// Gitea doesn't keep names unique: a second label of a name is
		// another label.
		l := label{ID: f.id(), Name: str("name"), Color: strings.TrimPrefix(str("color"), "#"), Description: str("description")}
		f.labels = append(f.labels, l)
		writeJSON(w, 201, l)
	case "POST issues":
		f.next++
		now := now()
		is := &fakeIssue{number: f.next, title: str("title"), body: str("body"), author: who, state: "open", created: now, updated: now}
		if !f.setLabels(is, who, ids()) {
			writeJSON(w, 422, map[string]string{"message": "no such label"})
			return
		}
		f.issues[is.number] = is
		writeJSON(w, 201, f.issueJSON(is))
	case "GET issues/N":
		writeJSON(w, 200, f.issueJSON(is))
	case "PATCH issues/N":
		if s := str("state"); s != "" {
			is.state = s
			if s == "closed" {
				f.close(is)
			}
		}
		if _, ok := in["title"]; ok {
			if t := str("title"); t != is.title {
				is.timeline = append(is.timeline, labelEvent{Type: "change_title", User: user{Login: who}, OldTitle: is.title, NewTitle: t, CreatedAt: time.Now()})
				is.title = t
			}
		}
		if _, ok := in["body"]; ok {
			is.body, is.updated = str("body"), time.Now()
		}
		writeJSON(w, 201, f.issueJSON(is))
	case "GET issues/N/comments":
		// Gitea ignores page and limit here and serves every comment, every
		// time, with the whole count as X-Total-Count.
		if !f.noTotal {
			w.Header().Set("X-Total-Count", strconv.Itoa(len(is.comments)))
		}
		writeJSON(w, 200, append([]comment{}, is.comments...))
	case "POST issues/N/comments":
		at := now()
		c := comment{ID: f.id(), Body: str("body"), User: user{Login: who}, CreatedAt: at, UpdatedAt: at}
		is.comments = append(is.comments, c)
		writeJSON(w, 201, c)
	case "GET issues/N/labels":
		out := []label{}
		for _, id := range is.labels {
			l, _ := f.label(id)
			out = append(out, l)
		}
		writeJSON(w, 200, out)
	case "PUT issues/N/labels", "POST issues/N/labels":
		want := ids()
		if r.Method == "POST" {
			want = append(slices.Clone(is.labels), want...)
		}
		if !f.setLabels(is, who, want) {
			writeJSON(w, 422, map[string]string{"message": "no such label"})
			return
		}
		writeJSON(w, 200, f.issueJSON(is).Labels)
	case "GET issues/N/timeline":
		since, _ := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
		var out []labelEvent
		for i, e := range is.timeline {
			e.ID = int64(i + 1)
			if !e.CreatedAt.Truncate(time.Second).Before(since) {
				out = append(out, e)
			}
		}
		// Gitea cuts the page first and then drops code comments (and
		// cross-references the token can't read), and its X-Total-Count is
		// what's left of the page, not the timeline's length. The timeline
		// mustn't depend on a Link header, so the fake sends none.
		out = slices.DeleteFunc(slices.Clone(slice(f, w, r, out)), func(e labelEvent) bool { return e.Type == "code" })
		w.Header().Del("Link")
		if !f.noTotal {
			w.Header().Set("X-Total-Count", strconv.Itoa(len(out)))
		}
		writeJSON(w, 200, out)
	case "POST issues/N/assets":
		file, _, err := r.FormFile("attachment")
		if err != nil {
			writeJSON(w, 400, map[string]string{"message": err.Error()})
			return
		}
		b, _ := io.ReadAll(file)
		uuid := fmt.Sprintf("0a1b2c3d-0000-4000-8000-%012d", f.id())
		f.blobs[uuid] = b
		f.assets[is.number] = append(f.assets[is.number], uuid)
		writeJSON(w, 201, map[string]any{"uuid": uuid, "browser_download_url": f.srv.URL + "/attachments/" + uuid})
	case "GET issues/N/assets":
		out := []map[string]string{}
		for _, u := range f.assets[is.number] {
			out = append(out, map[string]string{"uuid": u})
		}
		writeJSON(w, 200, out)
	case "POST pulls":
		sha, ok := f.branches[str("head")]
		if !ok {
			writeJSON(w, 404, map[string]string{"message": "no head branch"})
			return
		}
		f.next++
		now := now()
		is := &fakeIssue{number: f.next, title: str("title"), body: str("body"), author: who, state: "open", created: now, updated: now,
			isPR: true, head: str("head"), headSHA: sha, base: str("base"), mergeBase: f.branches[str("base")]}
		f.issues[is.number] = is
		writeJSON(w, 201, f.pullJSON(is))
	case "GET pulls":
		var out []pull
		for _, n := range sortedKeys(f.issues) {
			if p := f.issues[n]; p.isPR && p.state == r.URL.Query().Get("state") {
				out = append(out, f.pullJSON(p))
			}
		}
		page(f, w, r, out)
	case "GET pulls/N":
		writeJSON(w, 200, f.pullJSON(is))
		is.checkReads = max(is.checkReads-1, 0)
	case "PATCH pulls/N":
		if b, ok := in["base"].(string); ok {
			sha, known := f.branches[b]
			if !known {
				writeJSON(w, 404, map[string]string{"message": "no base branch"})
				return
			}
			is.base, is.mergeBase = b, sha
		}
		if _, ok := in["title"]; ok {
			is.title, is.body = str("title"), str("body")
		}
		writeJSON(w, 201, f.pullJSON(is))
	case "POST pulls/N/reviews":
		is.reviews = append(is.reviews, fakeReview{user: who, state: str("event"), sha: str("commit_id"), body: str("body"), at: now()})
		writeJSON(w, 200, map[string]any{})
	case "GET pulls/N/reviews":
		var out []map[string]any
		for i, rv := range is.reviews {
			out = append(out, map[string]any{"id": i + 1, "user": user{Login: rv.user}, "state": rv.state, "commit_id": rv.sha, "body": rv.body, "submitted_at": rv.at,
				"comments_count": len(rv.lines)})
		}
		page(f, w, r, out)
	case "POST pulls/N/merge":
		if who != "devbot-review" || str("head_commit_id") != is.headSHA || is.state != "open" {
			writeJSON(w, 409, map[string]string{"message": "head out of date"})
			return
		}
		if is.checking {
			writeJSON(w, 405, map[string]string{"message": "Please try again later"})
			return
		}
		if is.conflicted {
			writeJSON(w, 405, map[string]string{"message": "Merge conflicts"})
			return
		}
		if wip(is.title) {
			writeJSON(w, 405, map[string]string{"message": "The pull request is work in progress"})
			return
		}
		// A squash: one new commit on the base, with the base before it.
		is.merged, is.mergeSHA = true, "5qu45h-"+is.headSHA
		f.history[is.mergeSHA] = []string{f.branches[is.base]}
		f.branches[is.base] = is.mergeSHA
		f.close(is)
		w.WriteHeader(200)
	default:
		f.serveRest(w, r, segs, in)
	}
}

// serveRest is the routes keyed by a name rather than a number.
// in is the request's JSON body, which the caller has read.
func (f *fakeGitea) serveRest(w http.ResponseWriter, r *http.Request, segs []string, in map[string]any) {
	rest := strings.Join(segs[1:], "/")
	switch {
	case r.Method == "GET" && segs[0] == "branches" && rest == "":
		var out []map[string]any
		for _, name := range slices.Sorted(maps.Keys(f.branches)) {
			out = append(out, map[string]any{"name": name, "commit": map[string]any{"id": f.branches[name], "timestamp": f.branchAt[name]}})
		}
		page(f, w, r, out)
	case r.Method == "DELETE" && segs[0] == "branches":
		if _, ok := f.branches[rest]; !ok {
			http.NotFound(w, r)
			return
		}
		delete(f.branches, rest)
		w.WriteHeader(204)
	case r.Method == "GET" && segs[0] == "branches":
		sha, ok := f.branches[rest]
		if !ok {
			http.NotFound(w, r)
			return
		}
		b := map[string]any{"name": rest, "commit": map[string]string{"id": sha}, "protected": false, "user_can_merge": true}
		if checks, ok := f.protected[rest]; ok {
			b["protected"], b["effective_branch_protection_name"] = true, rest
			b["enable_status_check"], b["status_check_contexts"] = true, checks
		}
		writeJSON(w, 200, b)
	case r.Method == "POST" && segs[0] == "statuses" && len(segs) == 2:
		state, _ := in["state"].(string)
		c, _ := in["context"].(string)
		d, _ := in["description"].(string)
		u, _ := in["target_url"].(string)
		if f.statuses[segs[1]] == nil {
			f.statuses[segs[1]] = map[string]string{}
		}
		f.statuses[segs[1]][c] = state
		f.details[segs[1]+" "+c] = [2]string{d, u}
		writeJSON(w, 201, map[string]any{"context": c, "status": state})
	case r.Method == "GET" && segs[0] == "commits" && len(segs) == 3 && segs[2] == "status":
		var list []map[string]string
		for _, ctx := range slices.Sorted(maps.Keys(f.statuses[segs[1]])) {
			d := f.details[segs[1]+" "+ctx]
			list = append(list, map[string]string{"context": ctx, "status": f.statuses[segs[1]][ctx], "description": d[0], "target_url": d[1]})
		}
		// Gitea pages the combined status, and its state and total_count
		// are the page's alone.
		out := slice(f, w, r, list)
		st := map[string]any{"state": "success", "total_count": len(out), "statuses": out}
		for _, s := range out {
			if s["status"] != "success" {
				st["state"] = s["status"]
			}
		}
		writeJSON(w, 200, st)
	case r.Method == "GET" && segs[0] == "git" && len(segs) > 2 && segs[1] == "trees":
		// A ref or a branch's SHA, recursive, a page at a time and marked
		// truncated while more are to come, with the directories as entries
		// of their own.
		ref := strings.Join(segs[2:], "/")
		for name, sha := range f.branches {
			if sha == ref {
				ref = name
			}
		}
		if _, ok := f.branches[ref]; !ok || r.URL.Query().Get("recursive") != "true" {
			http.NotFound(w, r)
			return
		}
		var all []map[string]any
		dirs := map[string]bool{}
		for _, k := range slices.Sorted(maps.Keys(f.files)) {
			p, ok := strings.CutPrefix(k, ref+":")
			if !ok {
				continue
			}
			for d := path.Dir(p); d != "." && !dirs[d]; d = path.Dir(d) {
				dirs[d] = true
				all = append(all, map[string]any{"path": d, "type": "tree", "mode": "040000"})
			}
			all = append(all, map[string]any{"path": p, "type": "blob", "mode": "100644", "size": len(f.files[k])})
		}
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		per = min(max(per, 1), f.maxItems)
		n, _ := strconv.Atoi(r.URL.Query().Get("page"))
		lo := min((max(n, 1)-1)*per, len(all))
		hi := min(lo+per, len(all))
		writeJSON(w, 200, map[string]any{"sha": f.branches[ref], "tree": all[lo:hi], "truncated": hi < len(all), "page": max(n, 1), "total_count": len(all)})
	case r.Method == "GET" && segs[0] == "tags" && rest == "":
		var out []map[string]any
		for _, t := range f.tags {
			out = append(out, map[string]any{"name": t[0], "id": "7a9-" + t[0], "commit": map[string]string{"sha": t[1], "url": "https://gitea.example/c/" + t[1]}})
		}
		page(f, w, r, out)
	case r.Method == "GET" && segs[0] == "compare":
		// What head has that base doesn't, as Gitea lists it from their
		// merge base.
		base, head, ok := strings.Cut(rest, "...")
		base, head = f.commit(base), f.commit(head)
		if !ok || base == "" || head == "" {
			http.NotFound(w, r)
			return
		}
		mine := f.ancestry(base)
		var commits []map[string]string
		for _, c := range slices.Sorted(maps.Keys(f.ancestry(head))) {
			if !mine[c] {
				commits = append(commits, map[string]string{"sha": c})
			}
		}
		writeJSON(w, 200, map[string]any{"total_commits": len(commits), "commits": commits})
	case r.Method == "GET" && segs[0] == "raw":
		ref := r.URL.Query().Get("ref")
		if ref == "" {
			ref = "main"
		}
		b, ok := f.files[ref+":"+rest]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	case r.Method == "GET" && segs[0] == "collaborators" && len(segs) == 3 && segs[2] == "permission":
		p, ok := f.perms[segs[1]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, map[string]string{"permission": p})
	default:
		writeJSON(w, 404, map[string]string{"message": "the fake has no " + r.Method + " " + r.URL.Path})
	}
}

func sortedKeys[V any](m map[int]V) []int {
	ks := make([]int, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// A server whose MAX_RESPONSE_ITEMS is below the 50 devbot asks for serves
// short pages that aren't the end, with X-Total-Count and Link or without.
func TestListsPageAtTheServersPageSize(t *testing.T) {
	const n = 45
	for _, tc := range []struct {
		name     string
		maxItems int
		noTotal  bool
		noLink   bool
	}{{"50", 50, false, false}, {"20", 20, false, false}, {"20 without a total", 20, true, false},
		{"20 without a Link", 20, false, true}, {"20 without either", 20, true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitea(t)
			f.maxItems, f.noTotal, f.noLink = tc.maxItems, tc.noTotal, tc.noLink
			fx := f.fixture(t)
			ctx := context.Background()
			var pr int
			for i := range n {
				fx.Issue("alice", "t", "", "many", fmt.Sprintf("label-%d", i))
				pr = fx.PR(forgetest.PRSeed{Author: "devbot", Head: fmt.Sprintf("b%d", i), HeadSHA: "abc", Base: "main", MergeBase: "ba5e"})
			}
			issue := fx.Issue("alice", "t", "")
			f.mu.Lock()
			many, _ := f.label(f.labelID("many"))
			for range n {
				f.issues[issue].comments = append(f.issues[issue].comments, comment{ID: f.id(), Body: "hi", User: user{Login: "alice"}, CreatedAt: now()})
				f.issues[issue].timeline = append(f.issues[issue].timeline, labelEvent{Type: "label", Body: "1", User: user{Login: "alice"}, Label: many, CreatedAt: now()})
				f.issues[pr].reviews = append(f.issues[pr].reviews, fakeReview{user: "devbot-review", state: "COMMENT", sha: "abc", at: now()})
			}
			f.mu.Unlock()

			p, ref := fx.P, forge.ItemRef{Repo: fx.Repo, Number: issue}
			counts := map[string]func() (int, error){
				"SearchItems":  func() (int, error) { l, err := p.SearchItems(ctx, "many", "open"); return len(l), err },
				"ListComments": func() (int, error) { l, err := p.ListComments(ctx, ref); return len(l), err },
				"LabelEvents":  func() (int, error) { l, err := p.LabelEvents(ctx, ref); return len(l), err },
				"ListOpenPRs":  func() (int, error) { l, err := p.ListOpenPRs(ctx, fx.Repo); return len(l), err },
				"Reviews":      func() (int, error) { l, err := p.Reviews(ctx, fx.Repo, pr); return len(l), err },
			}
			for name, count := range counts {
				if got, err := count(); err != nil || got != n {
					t.Errorf("%s: got %d (%v), want %d", name, got, err, n)
				}
			}
			// The label on the last page is found too.
			if _, err := p.(*Client).labelIDs(ctx, fx.Repo, []string{fmt.Sprintf("label-%d", n-1)}); err != nil {
				t.Error(err)
			}
		})
	}
}

// A list ends at its total or, without one, at a Link with no rel="next";
// with neither it takes one more request, for the empty page.
func TestListsStopAtTheTotalOrTheLastLink(t *testing.T) {
	for _, tc := range []struct {
		name            string
		noTotal, noLink bool
		requests        int
	}{{"total and Link", false, false, 3}, {"Link only", true, false, 3}, {"total only", false, true, 3}, {"neither", true, true, 4}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitea(t)
			f.maxItems, f.noTotal, f.noLink = 20, tc.noTotal, tc.noLink
			fx := f.fixture(t)
			f.mu.Lock()
			for i := range 45 {
				f.labelID(fmt.Sprintf("label-%d", i))
			}
			f.calls = 0
			f.mu.Unlock()
			c := fx.P.(*Client)
			n := 0
			if err := list(context.Background(), c, c.token(c.acting()), repoPath(fx.Repo)+"/labels", func(label) { n++ }); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if n != 45 || f.calls != tc.requests {
				t.Fatalf("got %d labels in %d requests, want 45 in %d", n, f.calls, tc.requests)
			}
		})
	}
}

// Issue comments are one GET, total or not.
func TestCommentsAreOneRequest(t *testing.T) {
	for _, noTotal := range []bool{false, true} {
		f := newFakeGitea(t)
		f.noTotal = noTotal
		fx := f.fixture(t)
		issue := fx.Issue("alice", "t", "")
		f.mu.Lock()
		for range 120 {
			f.issues[issue].comments = append(f.issues[issue].comments, comment{ID: f.id(), Body: "hi", User: user{Login: "alice"}, CreatedAt: now()})
		}
		f.calls = 0
		f.mu.Unlock()
		cs, err := fx.P.ListComments(context.Background(), forge.ItemRef{Repo: fx.Repo, Number: issue})
		f.mu.Lock()
		calls := f.calls
		f.mu.Unlock()
		if err != nil || len(cs) != 120 || calls != 1 {
			t.Fatalf("noTotal %v: got %d comments (%v) in %d requests, want 120 in 1", noTotal, len(cs), err, calls)
		}
	}
}

// Gitea pages the combined status, 30 contexts by default: a failure past
// the first page still reads red.
func TestCombinedChecksReadsEveryPage(t *testing.T) {
	for _, maxItems := range []int{50, 20} {
		f := newFakeGitea(t)
		f.maxItems = maxItems
		fx := f.fixture(t)
		for i := range 35 {
			fx.Status("abc", fmt.Sprintf("check-%02d", i), "success")
		}
		fx.Status("abc", "zz-last", "failure")
		st, details, err := fx.P.CombinedChecks(context.Background(), fx.Repo, "abc")
		if err != nil || st != forge.ChecksFailure || len(details) != 36 {
			t.Errorf("MAX_RESPONSE_ITEMS %d: got %s with %d checks (%v), want failure with 36", maxItems, st, len(details), err)
		}
	}
}

// A cancelled Actions job's status reads as cancelled; other CI's, or a job
// that failed, doesn't. Re-running finds each job's run among those on the
// commit by its link, whether that has the run's ID or, from an older Gitea,
// its number, and re-runs it once.
func TestRerunCancelledChecks(t *testing.T) {
	f := newFakeGitea(t)
	fx := f.fixture(t)
	ctx := context.Background()
	runs := f.srv.URL + "/acme/app/actions/runs/"
	f.mu.Lock()
	f.runs = []fakeRun{{id: 40, sha: "other", htmlURL: runs + "6"}, {id: 41, sha: "abc", htmlURL: runs + "7"}, {id: 42, sha: "abc", htmlURL: runs + "8"}}
	f.statuses["abc"] = map[string]string{"build": "failure", "test": "failure", "lint": "failure", "drone": "failure", "e2e": "success"}
	f.details["abc build"] = [2]string{"Canceled after 0s", "/acme/app/actions/runs/7/jobs/0"}
	f.details["abc test"] = [2]string{"Has been cancelled", runs + "7/jobs/1"}
	f.details["abc lint"] = [2]string{"Failing after 3s", runs + "8/jobs/0"}
	f.details["abc drone"] = [2]string{"Cancelled", "https://drone.example/acme/app/5"}
	f.mu.Unlock()
	st, details, err := fx.P.CombinedChecks(ctx, fx.Repo, "abc")
	if err != nil || st != forge.ChecksFailure {
		t.Fatalf("got %s, %v", st, err)
	}
	byName := map[string]forge.CheckDetail{}
	for _, d := range details {
		byName[d.Context] = d
		if want := d.Context == "build" || d.Context == "test"; d.Cancelled != want || d.Failed != (d.Context != "e2e") {
			t.Errorf("%s: cancelled %v, failed %v", d.Context, d.Cancelled, d.Failed)
		}
	}
	if err := fx.P.RerunChecks(ctx, fx.Repo, "abc", []forge.CheckDetail{byName["build"], byName["test"]}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.reruns, []int64{41}) {
		t.Fatalf("re-ran %v, want run 41 once", f.reruns)
	}
	if err := fx.P.RerunChecks(ctx, fx.Repo, "abc", []forge.CheckDetail{byName["drone"]}); !errors.Is(err, forge.ErrNotRerunnable) {
		t.Fatalf("another CI's status: %v, want ErrNotRerunnable", err)
	}
	gone := forge.CheckDetail{Context: "old", URL: runs + "6/jobs/0"}
	if err := fx.P.RerunChecks(ctx, fx.Repo, "abc", []forge.CheckDetail{gone}); !errors.Is(err, forge.ErrNotRerunnable) {
		t.Fatalf("a run not on the commit: %v, want ErrNotRerunnable", err)
	}
	if !slices.Equal(f.reruns, []int64{41}) {
		t.Fatalf("re-ran %v after the refusals", f.reruns)
	}
}

// A label deleted and recreated under a new ID after the cache was filled
// still goes on, whether Gitea refuses the old ID or quietly leaves it off.
func TestRecreatedLabelStillApplies(t *testing.T) {
	for _, dropUnknown := range []bool{false, true} {
		f := newFakeGitea(t)
		f.dropUnknown = dropUnknown
		fx := f.fixture(t)
		ctx := context.Background()
		defs := []forge.Label{{Name: "devbot/ready", Color: "00ff00"}, {Name: "devbot/working", Color: "0000ff"}}
		if err := fx.P.EnsureLabels(ctx, fx.Repo, defs); err != nil {
			t.Fatal(err)
		}
		ref := forge.ItemRef{Repo: fx.Repo, Number: fx.Issue("alice", "t", "", "devbot/working")}
		f.recreateLabel("devbot/ready")
		if err := fx.P.SetLabels(ctx, ref, []string{"devbot/ready"}, []string{"devbot/working"}); err != nil {
			t.Fatalf("dropUnknown %v: %v", dropUnknown, err)
		}
		if it, _ := fx.P.GetItem(ctx, ref); !slices.Equal(it.Labels, []string{"devbot/ready"}) {
			t.Fatalf("dropUnknown %v: got %v", dropUnknown, it.Labels)
		}
		f.recreateLabel("devbot/working")
		it, err := fx.P.CreateItem(ctx, fx.Repo, "t", "", []string{"devbot/working"})
		if err != nil || !slices.Equal(it.Labels, []string{"devbot/working"}) {
			t.Fatalf("dropUnknown %v: filed with %v (%v)", dropUnknown, it.Labels, err)
		}
		if n := len(f.issues); n != 2 {
			t.Fatalf("dropUnknown %v: want the issue filed once, %d issues", dropUnknown, n)
		}
	}
}

// Gitea serves a recursive tree a page at a time, marked truncated until
// the last.
func TestListTreePages(t *testing.T) {
	f := newFakeGitea(t)
	f.maxItems = 20
	fx := f.fixture(t)
	for i := range 45 {
		fx.File("main", fmt.Sprintf("d/f%02d", i), []byte("x"))
	}
	before := fx.Calls()
	files, err := fx.P.ListTree(context.Background(), fx.Repo, "main")
	if err != nil || len(files) != 45 || files[44].Path != "d/f44" {
		t.Fatalf("got %d files, %v", len(files), err)
	}
	if n := fx.Calls() - before; n != 3 {
		t.Errorf("took %d calls, want 3 pages", n)
	}
}

// A log Gitea has cleaned up, or a Gitea from before 1.24 without the jobs
// API, is ErrNotFound; a status from another CI has nothing to read.
func TestCheckReportUnavailable(t *testing.T) {
	f := newFakeGitea(t)
	fx := f.fixture(t)
	ctx := context.Background()
	fx.FailedJob("c3", "build", "boom\n")
	f.mu.Lock()
	f.jobs[0].expired = true
	f.statuses["c3"]["drone"] = "failure"
	f.details["c3 drone"] = [2]string{"failed", "https://drone.example/acme/app/5"}
	f.mu.Unlock()
	_, ds, err := fx.P.CombinedChecks(ctx, fx.Repo, "c3")
	if err != nil || len(ds) != 2 {
		t.Fatalf("got %+v, %v", ds, err)
	}
	byName := map[string]forge.CheckDetail{}
	for _, d := range ds {
		byName[d.Context] = d
	}
	if r, err := fx.P.CheckReport(ctx, fx.Repo, "c3", byName["build"], 10); !errors.Is(err, forge.ErrNotFound) || !r.HasLog {
		t.Errorf("an expired log: %+v, %v", r, err)
	}
	if r, err := fx.P.CheckReport(ctx, fx.Repo, "c3", byName["drone"], 10); err != nil || r.HasLog {
		t.Errorf("another CI's status: %+v, %v", r, err)
	}
	f.mu.Lock()
	f.jobs = nil
	f.mu.Unlock()
	if _, err := fx.P.CheckReport(ctx, fx.Repo, "c3", byName["build"], 10); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("no such job: %v", err)
	}
}

// commit is the commit ref names: a tag's, a branch's, or a SHA the fake
// has seen; "" for none.
func (f *fakeGitea) commit(ref string) string {
	for _, t := range f.tags {
		if t[0] == ref {
			return t[1]
		}
	}
	if sha, ok := f.branches[ref]; ok {
		return sha
	}
	if _, ok := f.history[ref]; ok {
		return ref
	}
	for _, sha := range f.branches {
		if sha == ref {
			return ref
		}
	}
	return ""
}

// ancestry is sha and every commit in its history.
func (f *fakeGitea) ancestry(sha string) map[string]bool {
	seen := map[string]bool{}
	for todo := []string{sha}; len(todo) > 0; todo = todo[1:] {
		if c := todo[0]; !seen[c] {
			seen[c] = true
			todo = append(todo, f.history[c]...)
		}
	}
	return seen
}

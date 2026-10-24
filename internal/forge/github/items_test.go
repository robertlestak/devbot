package github

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// record logs every installation call the adapter makes to acme/app, as
// "METHOD /sub/path", and fails a PUT of an issue's labels while failPut
// is set.
type record struct {
	f       *fakeGitHub
	calls   []string
	failPut bool
}

func recordCalls(f *fakeGitHub) *record {
	rc := &record{f: f}
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		sub, ok := strings.CutPrefix(r.URL.Path, f.prefix+"/repos/acme/app")
		if !ok || strings.HasSuffix(sub, "/installation") {
			return false
		}
		f.mu.Lock()
		rc.calls = append(rc.calls, r.Method+" "+sub)
		fail := rc.failPut && r.Method == "PUT"
		f.mu.Unlock()
		if fail {
			writeJSON(w, 500, map[string]string{"message": "Server Error"})
		}
		return fail
	}
	return rc
}

func (rc *record) reset() {
	rc.f.mu.Lock()
	rc.calls = nil
	rc.f.mu.Unlock()
}

// writes are the calls that weren't reads.
func (rc *record) writes() []string {
	rc.f.mu.Lock()
	defer rc.f.mu.Unlock()
	var out []string
	for _, c := range rc.calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

// seed creates the labels the tests use and files issue 1 as alice, with
// labels.
func seed(f *fakeGitHub, labels ...string) forge.ItemRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos["acme/app"]
	for _, n := range []string{"devbot/ready", "devbot/working", "devbot/needs-human", "bug", "priority"} {
		r.labels[n] = fakeLabel{color: "ededed"}
	}
	r.next++
	is := &fakeIssue{number: r.next, title: "t", author: "alice", state: "open", created: f.now(), updated: f.now()}
	r.issues[is.number] = is
	f.setLabels(r, is, "alice", labels)
	return forge.ItemRef{Repo: repo, Number: is.number}
}

func issueLabels(f *fakeGitHub, ref forge.ItemRef) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.repos["acme/app"].issues[ref.Number].labels)
}

// labelAs changes the issue's labels as a person would, from inside the fake.
func labelAs(f *fakeGitHub, who string, ref forge.ItemRef, change func([]string) []string) {
	r := f.repos["acme/app"]
	is := r.issues[ref.Number]
	f.setLabels(r, is, who, change(slices.Clone(is.labels)))
}

// afterRead runs fn as the labels are read for the nth time from now.
func afterRead(f *fakeGitHub, n int, fn func(is *fakeIssue)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var next func(*fakeIssue)
	next = func(is *fakeIssue) {
		if n--; n > 0 {
			f.onLabelsRead = next
			return
		}
		fn(is)
	}
	f.onLabelsRead = next
}

func TestCreateCloseAndCommentAsTheImplementer(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	seed(f)
	it, err := c.CreateItem(ctx, repo, "Title", "Body", []string{"devbot/ready", "bug"})
	if err != nil {
		t.Fatal(err)
	}
	if it.Author != "devbot[bot]" || it.Title != "Title" || it.State != "open" || !slices.Equal(it.Labels, []string{"devbot/ready", "bug"}) ||
		it.URL != c.IssueURL(it.Ref) {
		t.Fatalf("created %+v", it)
	}
	if err := c.Comment(ctx, it.Ref, "Working on it"); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseItem(ctx, it.Ref); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	is := f.repos["acme/app"].issues[it.Ref.Number]
	f.mu.Unlock()
	if is.state != "closed" || len(is.comments) != 1 || is.comments[0].author != "devbot[bot]" {
		t.Fatalf("state %s, comments %+v", is.state, is.comments)
	}
	if err := c.CloseItem(ctx, forge.ItemRef{Repo: repo, Number: 999}); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("closing a missing issue: %v", err)
	}
}

// Adding alone is one POST, which leaves everything else alone, including
// a label someone added a moment before.
func TestSetLabelsAddIsOnePOST(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	ref := seed(f, "bug")
	rc := recordCalls(f)
	afterRead(f, 1, func(*fakeIssue) { labelAs(f, "alice", ref, func(l []string) []string { return append(l, "priority") }) })
	if err := c.SetLabels(ctx, ref, []string{"devbot/ready"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := issueLabels(f, ref); !slices.Equal(got, []string{"bug", "priority", "devbot/ready"}) {
		t.Fatalf("labels %v", got)
	}
	if w := rc.writes(); !slices.Equal(w, []string{"POST /issues/1/labels"}) {
		t.Fatalf("writes %v, want one POST", w)
	}
	// Already so: only a read.
	rc.reset()
	if err := c.SetLabels(ctx, ref, []string{"devbot/ready"}, []string{"devbot/working"}); err != nil {
		t.Fatal(err)
	}
	if len(rc.calls) != 1 || rc.calls[0] != "GET /issues/1/labels" {
		t.Fatalf("a change already made should only read: %v", rc.calls)
	}
}

// A state change is one PUT of the whole set: if it fails, the issue keeps
// the labels it had, never two state labels, and a person's own labels
// stay through it either way.
func TestSetLabelsIsOnePUT(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	ref := seed(f, "devbot/working", "bug")
	rc := recordCalls(f)
	rc.failPut = true
	if err := c.SetLabels(ctx, ref, []string{"devbot/needs-human"}, []string{"devbot/working", "devbot/ready"}); err == nil {
		t.Fatal("a failed replace must be an error")
	}
	if got := issueLabels(f, ref); !slices.Equal(got, []string{"devbot/working", "bug"}) {
		t.Fatalf("a failed change must leave the labels as they were, got %v", got)
	}
	if w := rc.writes(); !slices.Equal(w, []string{"PUT /issues/1/labels"}) {
		t.Fatalf("labels must change in one PUT, got %v", w)
	}

	rc.failPut = false
	rc.reset()
	if err := c.SetLabels(ctx, ref, []string{"devbot/needs-human"}, []string{"devbot/working", "devbot/ready"}); err != nil {
		t.Fatal(err)
	}
	if got := issueLabels(f, ref); !slices.Equal(got, []string{"bug", "devbot/needs-human"}) {
		t.Fatalf("labels after the change: %v", got)
	}
	if w := rc.writes(); !slices.Equal(w, []string{"PUT /issues/1/labels"}) {
		t.Fatalf("writes %v, want one PUT", w)
	}
}

// GitHub can't replace labels conditionally, so a label someone adds
// between devbot's read and its replace is dropped by the replace; devbot
// sees that in the issue's events and adds it back. A label devbot itself
// removed earlier, even in the same second, stays removed.
func TestSetLabelsKeepsAConcurrentAdd(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	ref := seed(f, "devbot/ready", "bug", "priority")
	// devbot took priority off a moment ago, as a replace that removed it.
	f.mu.Lock()
	labelAs(f, "devbot[bot]", ref, func([]string) []string { return []string{"devbot/ready", "bug"} })
	f.mu.Unlock()
	rc := recordCalls(f)

	// alice adds priority back just after devbot's last read.
	afterRead(f, 2, func(*fakeIssue) {
		labelAs(f, "alice", ref, func(l []string) []string { return append(l, "priority") })
	})
	if err := c.SetLabels(ctx, ref, []string{"devbot/working"}, []string{"devbot/ready"}); err != nil {
		t.Fatal(err)
	}
	if got := issueLabels(f, ref); !slices.Equal(got, []string{"bug", "devbot/working", "priority"}) {
		t.Fatalf("alice's label must survive the replace, got %v", got)
	}
	if w := rc.writes(); !slices.Equal(w, []string{"PUT /issues/1/labels", "POST /issues/1/labels"}) {
		t.Fatalf("want one replace and one add back, got %v", w)
	}

	// Again: nothing to do, so only a read.
	rc.reset()
	if err := c.SetLabels(ctx, ref, []string{"devbot/working"}, []string{"devbot/ready"}); err != nil {
		t.Fatal(err)
	}
	if len(rc.writes()) != 0 || len(rc.calls) != 1 {
		t.Fatalf("a change already made should only read: %v", rc.calls)
	}

	// In the same second, devbot takes priority off as asked, then makes
	// another change: priority stays off.
	if err := c.SetLabels(ctx, ref, nil, []string{"priority"}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetLabels(ctx, ref, []string{"devbot/needs-human"}, []string{"devbot/working"}); err != nil {
		t.Fatal(err)
	}
	if got := issueLabels(f, ref); !slices.Equal(got, []string{"bug", "devbot/needs-human"}) {
		t.Fatalf("labels %v", got)
	}
}

// devbot's labels are read by name, so ensuring them costs the same on a
// repo with more labels than 50 pages of 100 as on one with none of its own.
func TestEnsureLabelsCreatesAndRepairsOnlyItsOwn(t *testing.T) {
	for _, others := range []int{0, 5100} {
		t.Run(fmt.Sprintf("%d other labels", others), func(t *testing.T) {
			f := newFakeGitHub(t, "")
			c := f.client()
			f.mu.Lock()
			r := f.repos["acme/app"]
			r.labels["devbot/ready"] = fakeLabel{color: "000000", description: "old"}
			r.labels["devbot/working"] = fakeLabel{color: "FBCA04", description: "working"}
			r.labels["wontfix"] = fakeLabel{color: "ffffff", description: "people's"}
			for i := range others {
				r.labels[fmt.Sprintf("area/%05d", i)] = fakeLabel{color: "ededed"}
			}
			f.mu.Unlock()
			want := []forge.Label{
				{Name: "devbot/ready", Color: "0e8a16", Description: "queued"},
				{Name: "devbot/working", Color: "fbca04", Description: "working"},
				{Name: "devbot/needs-human", Color: "d93f0b", Description: "stuck"},
			}
			rc := recordCalls(f)
			if err := c.EnsureLabels(ctx, repo, want); err != nil {
				t.Fatal(err)
			}
			if w := rc.writes(); !slices.Equal(w, []string{"PATCH /labels/devbot/ready", "POST /labels"}) {
				t.Fatalf("writes %v: want the drifted label repaired, the missing one created, and the one only differing in case left", w)
			}
			if n := len(rc.calls); n != len(want)+2 {
				t.Fatalf("%d requests: want one read per label and the two writes, %v", n, rc.calls)
			}
			f.mu.Lock()
			got := map[string]fakeLabel{}
			for n, l := range r.labels {
				got[n] = l
			}
			f.mu.Unlock()
			if got["devbot/ready"] != (fakeLabel{"0e8a16", "queued"}) || got["devbot/needs-human"] != (fakeLabel{"d93f0b", "stuck"}) ||
				got["wontfix"] != (fakeLabel{"ffffff", "people's"}) {
				t.Fatalf("labels %+v", got)
			}
			rc.reset()
			if err := c.EnsureLabels(ctx, repo, want); err != nil {
				t.Fatal(err)
			}
			if w := rc.writes(); len(w) != 0 || len(rc.calls) != len(want) {
				t.Fatalf("a second run made %v", rc.calls)
			}
		})
	}
}

// A label someone creates between devbot's read and its create is taken
// as there, and repaired if it has drifted.
func TestEnsureLabelsCreatedMeanwhile(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	f.mu.Lock()
	r := f.repos["acme/app"]
	f.hook = func(w http.ResponseWriter, r2 *http.Request) bool {
		if r2.Method != "GET" || !strings.HasSuffix(r2.URL.EscapedPath(), "/labels/devbot%2Fready") {
			return false
		}
		f.mu.Lock()
		_, there := r.labels["devbot/ready"]
		r.labels["devbot/ready"] = fakeLabel{color: "000000"}
		f.mu.Unlock()
		if there {
			return false
		}
		notFound(w)
		return true
	}
	f.mu.Unlock()
	if err := c.EnsureLabels(ctx, repo, []forge.Label{{Name: "devbot/ready", Color: "0e8a16", Description: "queued"}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := r.labels["devbot/ready"]; got != (fakeLabel{"0e8a16", "queued"}) {
		t.Fatalf("label %+v", got)
	}
}

func TestLabelEventsPagesThroughAll(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	ref := seed(f)
	start := time.Now().UTC().Truncate(time.Second)
	f.mu.Lock()
	for i := range 150 {
		who := []string{"alice", "devbot[bot]"}[i%2]
		labelAs(f, who, ref, func(l []string) []string {
			if len(l) == 0 {
				return []string{"bug"}
			}
			return nil
		})
	}
	f.mu.Unlock()
	evs, err := c.LabelEvents(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 150 {
		t.Fatalf("got %d events, want 150", len(evs))
	}
	for i, e := range evs {
		if e.Label != "bug" || e.Added != (i%2 == 0) || e.Actor != []string{"alice", "devbot[bot]"}[i%2] || e.At.Before(start) {
			t.Fatalf("event %d: %+v", i, e)
		}
	}
}

func TestAuthorized(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	f.mu.Lock()
	r := f.repos["acme/app"]
	for _, role := range []string{"admin", "maintain", "write", "triage", "read"} {
		r.collabs[role] = role // the login is the role
	}
	r.collabs["auditor"] = "security-auditor" // a custom role, on read
	f.teams = map[string][]string{"core": {"tess"}}
	r.teamRoles["core"] = "maintain"
	r.collabs["tess"] = "read"
	f.mu.Unlock()
	perms := []forge.Permission{forge.PermRead, forge.PermWrite, forge.PermMaintain, forge.PermAdmin}
	for actor, has := range map[string]forge.Permission{
		"admin": forge.PermAdmin, "maintain": forge.PermMaintain, "write": forge.PermWrite,
		"triage": forge.PermRead, "read": forge.PermRead, "auditor": forge.PermRead,
		"tess": forge.PermMaintain, "stranger": forge.PermNone, "ghost": forge.PermNone, "": forge.PermNone,
	} {
		for _, min := range perms {
			got, err := c.Authorized(ctx, repo, actor, min)
			if err != nil || got != (has >= min) {
				t.Errorf("%q at %s: got %v, %v; want %v", actor, min, got, err, has >= min)
			}
		}
	}

	// A lookup the App can't make is a setup problem, not a no.
	for name, fn := range map[string]func(){
		"without metadata": func() { delete(f.installs[11].perms, "metadata") },
		"not installed":    func() {},
	} {
		target := repo
		if name == "not installed" {
			target = forge.RepoRef{Provider: "github", Owner: "acme", Name: "private"}
		}
		f.mu.Lock()
		fn()
		f.mu.Unlock()
		ok, err := c.Authorized(ctx, target, "admin", forge.PermRead)
		var fe *forge.LookupForbiddenError
		if ok || !errors.As(err, &fe) || !strings.Contains(fe.Need, "Metadata: read") || !strings.Contains(fe.Need, "devbot[bot]") {
			t.Errorf("%s: got %v, %v; want a LookupForbiddenError", name, ok, err)
		}
	}
}

// searchFake has the implementer App installed twice: on acme/app, and on
// two repos of another org, one of them with issues turned off.
func searchFake(t *testing.T) *fakeGitHub {
	f := newFakeGitHub(t, "")
	f.repos["tools/cli"] = newFakeRepo()
	f.repos["tools/wiki"] = newFakeRepo()
	f.repos["tools/wiki"].noIssues = true
	f.installs[33] = &fakeInstall{app: implAppID, repos: []string{"tools/cli", "tools/wiki"}, perms: map[string]string{"issues": "write", "metadata": "read"}}
	now := time.Now()
	add := func(full string, is fakeIssue) {
		r := f.repos[full]
		r.next++
		is.number, is.created, is.updated = r.next, now, now
		if is.state == "" {
			is.state = "open"
		}
		r.issues[is.number] = &is
	}
	add("acme/app", fakeIssue{title: "a", author: "alice", labels: []string{"devbot/working"}})
	add("acme/app", fakeIssue{title: "pr", author: "alice", labels: []string{"devbot/working"}, isPR: true})
	add("acme/app", fakeIssue{title: "other label", author: "alice", labels: []string{"bug"}})
	add("tools/cli", fakeIssue{title: "b", author: "bob", labels: []string{"bug", "devbot/working"}})
	add("tools/cli", fakeIssue{title: "closed", author: "bob", labels: []string{"devbot/working"}, state: "closed"})
	// Only the implementer's installations are read: this one isn't.
	f.repos["review/only"] = newFakeRepo()
	f.installs[44] = &fakeInstall{app: revAppID, repos: []string{"review/only"}}
	add("review/only", fakeIssue{title: "unseen", author: "carol", labels: []string{"devbot/working"}})
	return f
}

func titles(items []forge.Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Ref.Repo.FullName()+" "+it.Title)
	}
	slices.Sort(out)
	return out
}

// requests records the path of every request the fake gets from now on,
// before any hook already set sees it.
func (f *fakeGitHub) requests() func() []string {
	var paths []string
	f.mu.Lock()
	prev := f.hook
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		f.mu.Lock()
		paths = append(paths, r.URL.Path)
		f.mu.Unlock()
		return prev != nil && prev(w, r)
	}
	f.mu.Unlock()
	return func() []string {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Clone(paths)
	}
}

func TestSearchItemsCoversEveryInstallation(t *testing.T) {
	f := searchFake(t)
	c := f.client()
	seen := f.requests()
	items, err := c.SearchItems(ctx, "devbot/working", "open")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := titles(items), []string{"acme/app a", "tools/cli b"}; !slices.Equal(got, want) {
		t.Errorf("open: %v, want %v", got, want)
	}
	items, err = c.SearchItems(ctx, "devbot/working", "all")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := titles(items), []string{"acme/app a", "tools/cli b", "tools/cli closed"}; !slices.Equal(got, want) {
		t.Errorf("all: %v, want %v", got, want)
	}
	for _, p := range seen() {
		if strings.HasPrefix(p, "/repos/") || strings.Contains(p, "review/only") {
			t.Errorf("called %s", p)
		}
	}
	// What SearchItems learnt of each repo's installation saves a lookup.
	if _, err := c.GetItem(ctx, forge.ItemRef{Repo: c.ref("tools", "cli"), Number: 1}); err != nil {
		t.Fatal(err)
	}
	if p := seen(); strings.HasSuffix(p[len(p)-2], "/installation") {
		t.Errorf("looked the installation up again: %v", p[len(p)-2:])
	}
}

func TestSearchItemsAppliesAllowDenyFirst(t *testing.T) {
	f := searchFake(t)
	cfg := f.config()
	cfg.Allowed = func(r forge.RepoRef) bool { return r.Owner != "tools" }
	c, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	seen := f.requests()
	items, err := c.SearchItems(ctx, "devbot/working", "open")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := titles(items), []string{"acme/app a"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for _, p := range seen() {
		if strings.HasPrefix(p, "/repos/tools/") {
			t.Errorf("read a denied repo: %s", p)
		}
	}
}

// An org suspending the App, or removing it between the list and the token
// mint, mustn't stop reconcile finding work everywhere else.
func TestSearchItemsSkipsUnreachableInstallations(t *testing.T) {
	f := searchFake(t)
	f.repos["paused/app"] = newFakeRepo()
	f.installs[55] = &fakeInstall{app: implAppID, repos: []string{"paused/app"}, suspended: true}
	r := f.repos["paused/app"]
	r.next++
	r.issues[r.next] = &fakeIssue{number: r.next, title: "unseen", author: "dan", state: "open", labels: []string{"devbot/working"}}
	c := f.client()
	seen := f.requests()
	items, err := c.SearchItems(ctx, "devbot/working", "open")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := titles(items), []string{"acme/app a", "tools/cli b"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for _, p := range seen() {
		if strings.Contains(p, "/55/") || strings.Contains(p, "paused/") {
			t.Errorf("called %s for the suspended installation", p)
		}
	}

	// Uninstalled from tools since the list: its token mint is a 404.
	f.mu.Lock()
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/app/installations/33/access_tokens") {
			notFound(w)
			return true
		}
		return false
	}
	f.mu.Unlock()
	c = f.client()
	items, err = c.SearchItems(ctx, "devbot/working", "open")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := titles(items), []string{"acme/app a"}; !slices.Equal(got, want) {
		t.Errorf("after removal: %v, want %v", got, want)
	}
}

// An installation whose token or search errors is skipped, not the whole
// search: the others' items come back, with the error for reconcile to
// count.
func TestSearchItemsGoesOnPastAFailingInstallation(t *testing.T) {
	f := searchFake(t)
	fail := ""
	f.mu.Lock()
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		f.mu.Lock()
		failing := fail != "" && strings.HasPrefix(r.URL.Path+" "+r.Header.Get("Authorization"), fail)
		f.mu.Unlock()
		if failing {
			writeJSON(w, 500, map[string]string{"message": "Server Error"})
			return true
		}
		return false
	}
	f.mu.Unlock()
	for _, tc := range []struct{ fail, why, want string }{
		{"/app/installations/33/access_tokens", "installation 33", "acme/app a"},
		// Installation 11's search, by its token.
		{"/search/issues Bearer ghs_11_", "installation 11", "tools/cli b"},
	} {
		f.mu.Lock()
		fail = tc.fail
		f.mu.Unlock()
		items, err := f.client().SearchItems(ctx, "devbot/working", "open")
		if err == nil || !strings.Contains(err.Error(), tc.why) {
			t.Errorf("%s failing: want an error naming %s, got %v", tc.fail, tc.why, err)
		}
		if got := titles(items); !slices.Equal(got, []string{tc.want}) {
			t.Errorf("%s failing: got %v, want the others' %q", tc.fail, got, tc.want)
		}
	}
}

// A body's edit history comes from GraphQL, across pages, naming an App as
// BotUsers does and a deleted account as nobody; the body as filed isn't an
// edit. Title edits are the renamed events.
func TestContentEdits(t *testing.T) {
	f := newFakeGitHub(t, "/api/v3")
	f.editsPage = 2
	c := f.client()
	fx := f.fixture(t)
	n := fx.Issue("mallory", "t", "b0", "devbot/ready")
	at := time.Now().Add(time.Hour)
	f.mu.Lock()
	is := f.repos["acme/app"].issues[n]
	for i, who := range []string{"alice", "devbot[bot]", "", "mallory"} {
		is.edits = append(is.edits, fakeEdit{editor: who, at: at.Add(time.Duration(i) * time.Minute)})
	}
	f.mu.Unlock()
	fx.Edit(n, "bob", "t2", "b0")
	evs, err := c.ContentEdits(ctx, forge.ItemRef{Repo: fx.Repo, Number: n})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range evs {
		got = append(got, e.Field+":"+e.Editor)
	}
	if want := []string{"title:bob", "body:alice", "body:devbot[bot]", "body:", "body:mallory"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if _, err := c.ContentEdits(ctx, forge.ItemRef{Repo: fx.Repo, Number: 99}); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("a missing issue: %v", err)
	}
}

// A list longer than 50 pages of 100 is read to its end: comments, label
// events and open PRs past the 5,000th are all there.
func TestListsReadEveryPage(t *testing.T) {
	const n = 5050
	f := searchFake(t)
	c := f.client()
	f.mu.Lock()
	r := f.repos["acme/app"]
	is := r.issues[1]
	at := time.Now().UTC().Truncate(time.Second)
	for i := range n {
		is.comments = append(is.comments, fakeComment{id: int64(1000 + i), author: "alice", body: "+1", created: at})
		is.events = append(is.events, fakeEvent{id: int64(1000 + i), label: "bug", actor: "alice", added: i%2 == 0, at: at})
	}
	for range n {
		r.next++
		r.issues[r.next] = &fakeIssue{number: r.next, title: "pr", author: "devbot[bot]", state: "open", isPR: true,
			headRef: fmt.Sprintf("devbot/%d", r.next), headSHA: "abc", baseRef: "main", created: at, updated: at}
	}
	f.mu.Unlock()
	ref := forge.ItemRef{Repo: c.ref("acme", "app"), Number: 1}
	if cs, err := c.ListComments(ctx, ref); err != nil || len(cs) != n {
		t.Fatalf("ListComments: %v, %d comments", err, len(cs))
	}
	if evs, err := c.LabelEvents(ctx, ref); err != nil || len(evs) != n {
		t.Fatalf("LabelEvents: %v, %d events", err, len(evs))
	}
	if prs, err := c.ListOpenPRs(ctx, ref.Repo); err != nil || len(prs) < n {
		t.Fatalf("ListOpenPRs: %v, %d PRs", err, len(prs))
	}
}

// The body as filed is the oldest entry in the history, whatever time GitHub
// puts on it: taken for an edit, it would make the issue's author an editor.
func TestContentEditsDropTheOriginalByPlace(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shift time.Duration
		page  int
	}{
		{"stamped after the issue was created", 3 * time.Second, 0},
		{"stamped before the issue was created", -3 * time.Second, 0},
		{"alone on the last page", 3 * time.Second, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t, "")
			f.editsPage = tc.page
			c := f.client()
			fx := f.fixture(t)
			n := fx.Issue("mallory", "t", "b0", "devbot/ready")
			f.mu.Lock()
			is := f.repos["acme/app"].issues[n]
			is.original = is.created.Add(tc.shift)
			is.edits = []fakeEdit{{editor: "alice", at: is.created.Add(time.Minute)}}
			f.mu.Unlock()
			evs, err := c.ContentEdits(ctx, forge.ItemRef{Repo: fx.Repo, Number: n})
			if err != nil {
				t.Fatal(err)
			}
			if len(evs) != 1 || evs[0].Editor != "alice" {
				t.Fatalf("got %+v, want alice's edit alone", evs)
			}
		})
	}
}

// Collaborators can edit any comment on GitHub, devbot's included, and
// GitHub says when one was: an approval edited into one counts for nothing.
func TestEditedBotCommentsApprovalIsIgnored(t *testing.T) {
	f := searchFake(t)
	c := f.client()
	files := []core.HeldFile{{Path: ".github/workflows/ci.yml", Mode: "100644", Blob: "1111111111111111111111111111111111111111"}}
	hold := "Holding.\n\n" + core.RenderStatus(core.Status{Kind: "held", SHA: "aaaa111", Files: files})
	approval := "Approved.\n\n" + core.RenderStatus(core.Status{Kind: "approval", SHA: "aaaa111", Principal: "alice", Files: files})
	at := time.Now().UTC().Truncate(time.Second)
	f.mu.Lock()
	is := f.repos["acme/app"].issues[1]
	is.comments = append(is.comments,
		fakeComment{id: 9001, author: "devbot[bot]", body: hold, created: at},
		fakeComment{id: 9002, author: "devbot[bot]", body: approval, created: at, updated: at.Add(time.Minute)})
	f.mu.Unlock()
	cs, err := c.ListComments(ctx, forge.ItemRef{Repo: c.ref("acme", "app"), Number: 1})
	if err != nil || len(cs) != 2 || cs[0].Edited() || !cs[1].Edited() {
		t.Fatalf("got %+v, %v", cs, err)
	}
	ss := core.Statuses(cs, "devbot[bot]")
	if _, ok := core.LastHold(ss); !ok || core.Approved(ss, files) {
		t.Fatalf("the hold counts and the edited approval doesn't: %+v", ss)
	}
	// Unedited, the same approval counts.
	f.mu.Lock()
	is.comments[1].updated = time.Time{}
	f.mu.Unlock()
	cs, _ = c.ListComments(ctx, forge.ItemRef{Repo: c.ref("acme", "app"), Number: 1})
	if !core.Approved(core.Statuses(cs, "devbot[bot]"), files) {
		t.Fatal("an unedited approval doesn't count")
	}
}

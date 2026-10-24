package github

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// stateLabels are core's state labels, which reconcile searches for.
var stateLabels = []string{"devbot/ready", "devbot/working", "devbot/needs-info", "devbot/needs-human",
	"devbot/in-review", "devbot/changes-requested", "devbot/approved"}

// pass makes the calls one reconcile pass makes of the adapter: the PRs
// handed over, each state label open and closed (needs-human open only),
// and the repos for the branch sweep. It returns what it found, by
// "owner/name#n", and the errors.
func pass(t *testing.T, c *Client) (found []string, errs []error) {
	t.Helper()
	prs, err := c.SearchPRs(ctx, "devbot/ready")
	if err != nil {
		errs = append(errs, err)
	}
	for _, p := range prs {
		found = append(found, fmt.Sprintf("%s#%d pr", p.Repo.FullName(), p.Number))
	}
	for _, l := range stateLabels {
		for _, state := range []string{"open", "closed"} {
			if state == "closed" && l == "devbot/needs-human" {
				continue
			}
			items, err := c.SearchItems(ctx, l, state)
			if err != nil {
				errs = append(errs, err)
			}
			for _, it := range items {
				found = append(found, fmt.Sprintf("%s#%d", it.Ref.Repo.FullName(), it.Ref.Number))
			}
		}
	}
	if _, err := c.ListRepos(ctx); err != nil {
		errs = append(errs, err)
	}
	slices.Sort(found)
	return slices.Compact(found), errs
}

// countPaths counts the requests the fake gets from now on, by path.
func (f *fakeGitHub) countPaths() func(path string) int {
	seen := f.requests()
	return func(path string) int {
		n := 0
		for _, p := range seen() {
			if p == path {
				n++
			}
		}
		return n
	}
}

// installWebhook delivers an installation webhook for the implementer App
// to c, as GitHub does when the App is installed somewhere new.
func installWebhook(t *testing.T, c *Client) {
	t.Helper()
	r, body := delivery(t, "installation", "installation.created", setAppID(implAppID))
	r.Header.Set("X-GitHub-Hook-Installation-Target-ID", strconv.Itoa(implAppID))
	if _, err := c.ParseWebhook(r, body); err != nil {
		t.Fatal(err)
	}
}

// A pass lists the installations and each one's repos once, however many
// searches it makes, and the passes after it reuse that list until an
// installation webhook, a bump of the shared generation, or the TTL.
func TestReconcileListsTheInventoryOncePerPass(t *testing.T) {
	f := searchFake(t)
	cfg := f.config()
	cfg.WebhookSecret = "s3cret"
	gen := "1"
	cfg.InventoryGeneration = func(context.Context) (string, error) { return gen, nil }
	bumps := 0
	cfg.BumpInventory = func(context.Context) error { bumps++; return nil }
	c, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	clk, _ := withClock(f, c)
	lists := func(why string, want int) {
		t.Helper()
		count := f.countPaths()
		found, errs := pass(t, c)
		if len(errs) > 0 {
			t.Fatal(errs)
		}
		if want := []string{"acme/app#1", "tools/cli#1", "tools/cli#2"}; !slices.Equal(found, want) {
			t.Fatalf("%s: found %v, want %v", why, found, want)
		}
		// Installations 11 and 33 are the implementer's.
		if got := count("/app/installations"); got != want {
			t.Errorf("%s: listed the installations %d times, want %d", why, got, want)
		}
		if got := count("/installation/repositories"); got != 2*want {
			t.Errorf("%s: listed the installations' repos %d times, want %d", why, got, 2*want)
		}
	}
	lists("the first pass", 1)
	lists("the next pass", 0)
	installWebhook(t, c)
	if bumps != 1 {
		t.Errorf("the webhook bumped the shared generation %d times, want 1", bumps)
	}
	lists("after an installation webhook", 1)
	lists("the pass after that", 0)
	gen = "2" // another replica got the webhook
	lists("after the generation moved", 1)
	clk.add(inventoryTTL - time.Minute)
	lists("inside the TTL", 0)
	clk.add(2 * time.Minute)
	lists("past the TTL", 1)
}

// failOn makes the fake answer 500 to the requests match picks, until it
// is set to nil.
func failOn(f *fakeGitHub, match func(r *http.Request) bool) func(func(r *http.Request) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		f.mu.Lock()
		m := match
		f.mu.Unlock()
		if m != nil && m(r) {
			writeJSON(w, 500, map[string]string{"message": "Server Error"})
			return true
		}
		return false
	}
	return func(next func(r *http.Request) bool) {
		f.mu.Lock()
		match = next
		f.mu.Unlock()
	}
}

// A refresh that fails, in whole or part way through a list, keeps the
// inventory the last one built: its repos are still searched, and the
// error comes back once, from the search that refreshed. Nor is it retried
// for every search of the pass.
func TestFailedInventoryRefreshKeepsTheLastOne(t *testing.T) {
	f := searchFake(t)
	// Two pages of repos on installation 33.
	for i := range 150 {
		full := fmt.Sprintf("tools/r%03d", i)
		f.repos[full] = newFakeRepo()
		f.installs[33].repos = append(f.installs[33].repos, full)
	}
	c := f.client()
	clk, _ := withClock(f, c)
	want := []string{"acme/app#1", "tools/cli#1", "tools/cli#2"}
	if found, errs := pass(t, c); len(errs) > 0 || !slices.Equal(found, want) {
		t.Fatalf("first pass: %v, %v", found, errs)
	}
	repos, _ := c.ListRepos(ctx)
	fail := failOn(f, nil)
	for _, tc := range []struct {
		name, why string
		match     func(r *http.Request) bool
	}{
		{"the installations", "installations", func(r *http.Request) bool { return r.URL.Path == "/app/installations" }},
		{"one installation's repos", "installation 33", func(r *http.Request) bool {
			return r.URL.Path == "/installation/repositories" && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ghs_33_")
		}},
		{"the second page of them", "installation 33", func(r *http.Request) bool {
			return r.URL.Path == "/installation/repositories" && r.URL.Query().Get("page") == "2"
		}},
	} {
		fail(tc.match)
		clk.add(inventoryTTL)
		count := f.countPaths()
		found, errs := pass(t, c)
		fail(nil)
		if !slices.Equal(found, want) {
			t.Errorf("%s failing: found %v, want %v", tc.name, found, want)
		}
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), tc.why) {
			t.Errorf("%s failing: want one error naming %s, got %v", tc.name, tc.why, errs)
		}
		if got := count("/app/installations"); got != 1 {
			t.Errorf("%s failing: listed the installations %d times in a pass", tc.name, got)
		}
		if got, _ := c.ListRepos(ctx); !slices.Equal(got, repos) {
			t.Errorf("%s failing: ListRepos lost repos: %d, had %d", tc.name, len(got), len(repos))
		}
		// Tried again on the next pass after inventoryRetry, and fine now.
		clk.add(inventoryRetry)
		if _, errs := pass(t, c); len(errs) > 0 {
			t.Fatalf("%s failing: the pass after: %v", tc.name, errs)
		}
	}
}

// bigInstall installs the implementer App on n more repos of acme, after
// acme/app, and files a devbot/working issue in each repo of last.
func bigInstall(f *fakeGitHub, n int, last ...int) {
	for i := range n {
		full := fmt.Sprintf("acme/r%05d", i)
		f.repos[full] = newFakeRepo()
		f.installs[11].repos = append(f.installs[11].repos, full)
	}
	for _, i := range last {
		r := f.repos[fmt.Sprintf("acme/r%05d", i)]
		r.next++
		r.issues[r.next] = &fakeIssue{number: r.next, title: "deep", author: "alice", state: "open", labels: []string{"devbot/working"},
			created: time.Now().Add(-time.Hour), updated: time.Now()}
	}
}

// An installation on more than 5,000 repos (50 pages) is reconciled in
// full, past the 5,000th repo.
func TestInventoryHasNoCeiling(t *testing.T) {
	f := searchFake(t)
	bigInstall(f, 5080, 3, 5005, 5079)
	c := f.client()
	count := f.countPaths()
	items, err := c.SearchItems(ctx, "devbot/working", "open")
	if err != nil {
		t.Fatal(err)
	}
	got := titles(items)
	want := []string{"acme/app a", "acme/r00003 deep", "acme/r05005 deep", "acme/r05079 deep", "tools/cli b"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if n := count("/installation/repositories"); n != 52 {
		t.Errorf("%d pages of repos read, want installation 11's %d and installation 33's 1", n, 51)
	}
	repos, err := c.ListRepos(ctx)
	if err != nil || len(repos) != 1+5080+2 {
		t.Errorf("ListRepos: %d repos, %v", len(repos), err)
	}
}

// What label discovery costs doesn't grow with the repos: one query per
// installation for each search, whether it has one repo or hundreds, and
// no request to any repo.
func TestDiscoveryCostIsIndependentOfRepoCount(t *testing.T) {
	cost := func(n int) (searches, other int) {
		f := searchFake(t)
		bigInstall(f, n, n-1)
		c := f.client()
		if _, errs := pass(t, c); len(errs) > 0 {
			t.Fatal(errs)
		}
		count := f.requests()
		f.mu.Lock()
		f.searches = 0
		f.mu.Unlock()
		if _, errs := pass(t, c); len(errs) > 0 {
			t.Fatal(errs)
		}
		for _, p := range count() {
			if p != "/search/issues" {
				other++
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.searches, other
	}
	s1, o1 := cost(1)
	s2, o2 := cost(400)
	if s1 != s2 || o1 != 0 || o2 != 0 {
		t.Fatalf("a pass over 1 repo: %d searches, %d other requests; over 400: %d and %d", s1, o1, s2, o2)
	}
	// 13 label searches and one for PRs, in each of the two installations.
	if s1 != 2*14 {
		t.Errorf("%d searches a pass, want 28", s1)
	}
}

// A search with more results than the search API returns is split by
// when its items were created until every part fits, and returns them
// all. More than that in one second can't be split: what can be read of
// it comes back, with an error, and the rest of the search is whole.
func TestSearchSplitsPastTheCap(t *testing.T) {
	f := searchFake(t)
	r := f.repos["acme/app"]
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i := range 2500 {
		r.next++
		r.issues[r.next] = &fakeIssue{number: r.next, title: "many", author: "alice", state: "open", labels: []string{"devbot/working"},
			created: base.Add(-time.Duration(i) * 7 * time.Minute)}
	}
	c := f.client()
	items, err := c.SearchItems(ctx, "devbot/working", "open")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[forge.ItemRef]bool{}
	for _, it := range items {
		seen[it.Ref] = true
	}
	if len(items) != 2502 || len(seen) != 2502 {
		t.Fatalf("%d items, %d of them distinct; want 2502", len(items), len(seen))
	}

	for i := range searchCap {
		r.next++
		r.issues[r.next] = &fakeIssue{number: r.next, title: "burst", author: "alice", state: "open", labels: []string{"devbot/working"},
			created: base.Add(time.Duration(i%500) * time.Millisecond)}
	}
	items, err = c.SearchItems(ctx, "devbot/working", "open")
	if err == nil || !strings.Contains(err.Error(), "one second") {
		t.Fatalf("over the cap in one second: %d items, %v", len(items), err)
	}
	if !slices.ContainsFunc(items, func(it forge.Item) bool { return it.Ref.Repo.Owner == "tools" }) {
		t.Errorf("the other installation's items went too: %v", titles(items))
	}
	if n := len(items); n < 2502+searchCap-1 {
		t.Errorf("%d items: want the rest whole and the burst's first %d", n, searchCap)
	}
}

// A search that grows past what the search API returns while it is read
// is split as one that had that many from the start.
func TestSearchSplitsWhenItGrows(t *testing.T) {
	f := searchFake(t)
	r := f.repos["acme/app"]
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	add := func(n int, title string) {
		for i := range n {
			r.next++
			r.issues[r.next] = &fakeIssue{number: r.next, title: title, author: "alice", state: "open", labels: []string{"devbot/working"},
				created: base.Add(-time.Duration(i) * 3 * time.Minute)}
		}
	}
	add(900, "early")
	grown := false
	f.hook = func(w http.ResponseWriter, req *http.Request) bool {
		if strings.HasSuffix(req.URL.Path, "/search/issues") && req.URL.Query().Get("page") == "5" && !grown {
			f.mu.Lock()
			grown = true
			add(300, "late")
			f.mu.Unlock()
		}
		return false
	}
	items, err := f.client().SearchItems(ctx, "devbot/working", "open")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[forge.ItemRef]bool{}
	for _, it := range items {
		seen[it.Ref] = true
	}
	if !grown || len(seen) != 1202 {
		t.Fatalf("%d distinct items, want 1202", len(seen))
	}
}

// A search by account also finds its public repos the App isn't installed
// on; those aren't devbot's to reconcile. A user's account is searched as
// a user's.
func TestSearchKeepsToTheInventory(t *testing.T) {
	f := searchFake(t)
	f.repos["acme/public"] = newFakeRepo()
	f.repos["acme/public"].public = true
	f.repos["dana/dots"] = newFakeRepo()
	f.installs[66] = &fakeInstall{app: implAppID, user: true, repos: []string{"dana/dots"}, perms: map[string]string{"issues": "write", "metadata": "read"}}
	now := time.Now()
	for _, full := range []string{"acme/public", "dana/dots", "tools/wiki"} {
		r := f.repos[full]
		r.next++
		r.issues[r.next] = &fakeIssue{number: r.next, title: "x", author: "eve", state: "open", labels: []string{"devbot/working"}, created: now}
	}
	items, err := f.client().SearchItems(ctx, "devbot/working", "open")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := titles(items), []string{"acme/app a", "dana/dots x", "tools/cli b"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// After a Redis wipe (the shared generation is gone) or a restart, one pass
// finds every open task, whatever its state label, and every PR handed
// over with devbot/ready, across installations.
func TestOnePassFindsEveryTaskAfterAWipe(t *testing.T) {
	f := searchFake(t)
	f.installs[11].perms["pull_requests"] = "write"
	var want []string
	for i, l := range stateLabels {
		full := []string{"acme/app", "tools/cli"}[i%2]
		r := f.repos[full]
		r.next++
		r.issues[r.next] = &fakeIssue{number: r.next, title: l, author: "alice", state: "open", labels: []string{"bug", l}, created: time.Now()}
		want = append(want, fmt.Sprintf("%s#%d", full, r.next))
	}
	r := f.repos["acme/app"]
	r.next++
	r.issues[r.next] = &fakeIssue{number: r.next, title: "handed over", author: "bob", state: "open", labels: []string{"devbot/ready"}, isPR: true, created: time.Now()}
	want = append(want, fmt.Sprintf("acme/app#%d pr", r.next))
	// searchFake's own: an open working issue, a working PR, and a closed one.
	want = append(want, "acme/app#1", "tools/cli#1", "tools/cli#2")
	slices.Sort(want)

	cfg := f.config()
	gen := "7"
	cfg.InventoryGeneration = func(context.Context) (string, error) { return gen, nil }
	for _, restart := range []bool{true, false} {
		c, err := New(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !restart {
			pass(t, c)
			gen = "" // the wipe
		}
		found, errs := pass(t, c)
		if len(errs) > 0 {
			t.Fatal(errs)
		}
		if !slices.Equal(found, want) {
			t.Errorf("restart %v: found %v, want %v", restart, found, want)
		}
	}
}

// Listing one repo's tasks costs requests to that repo alone, the same
// number on an installation of more than 5,000 repos as on one of a few:
// no inventory, no search, no other repo.
func TestListItemsCostIsIndependentOfRepoCount(t *testing.T) {
	cost := func(n int) (got []string, paths []string) {
		f := searchFake(t)
		bigInstall(f, n, n-1)
		c := f.client()
		count := f.requests()
		target := forge.RepoRef{Provider: "github", Owner: "acme", Name: fmt.Sprintf("r%05d", n-1)}
		for _, l := range stateLabels {
			items, err := c.ListItems(ctx, target, l, "open")
			if err != nil {
				t.Fatal(err)
			}
			for _, it := range items {
				got = append(got, fmt.Sprintf("%s#%d %s", it.Ref.Repo.FullName(), it.Ref.Number, it.Title))
			}
		}
		for _, p := range count() {
			if !strings.HasPrefix(p, "/repos/"+target.FullName()+"/") && !strings.HasPrefix(p, "/app/installations/") {
				t.Errorf("over %d repos, a request to %s", n, p)
			}
		}
		return got, count()
	}
	got1, paths1 := cost(2)
	got2, paths2 := cost(5080)
	if !slices.Equal(got1, []string{"acme/r00001#1 deep"}) || !slices.Equal(got2, []string{fmt.Sprintf("acme/r%05d#1 deep", 5079)}) {
		t.Fatalf("found %v and %v", got1, got2)
	}
	if len(paths1) != len(paths2) {
		t.Fatalf("%d requests over 2 repos, %d over %d: %v", len(paths1), len(paths2), 5080, paths2)
	}
}

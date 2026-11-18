//go:build github_live

package github

// The live end-to-end test: devbot's engine and harness, with a stub in
// place of Claude, against real GitHub. It never runs by default; see
// "Running the live GitHub test" in docs/operating.md for the test org, the
// env vars and what each test leaves behind.
//
//	go test -tags github_live -count=1 -v -timeout 30m ./internal/forge/github/

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/agent/claude"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/harness"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// The stub stands in for claude -p, as in the harness's own tests, acting
// on the working tree as the mode file says. The mode file's path is put in
// when the stub is written, since the agent's environment is scrubbed.
const liveStub = `#!/bin/bash
cat >/dev/null
f=live/$(date +%s%N).txt
case "$(cat MODE_FILE)" in
green)
  mkdir -p live && echo "devbot live test" > "$f" && git add "$f" && git commit -qm "Add $f"
  mkdir -p .devbot/attachments
  printf '\x89PNG\r\n\x1a\nlive' > .devbot/attachments/board.png
  printf '%s' '{"status":"done","summary":"Added a file","pr_title":"Live test: green","checks":["sh test.sh"],"attachments":[{"path":"board.png","caption":"a stub screenshot"}]}' > .devbot/result.json
  ;;
plain)
  mkdir -p live && echo "devbot live test" > "$f" && git add "$f" && git commit -qm "Add $f"
  printf '%s' '{"status":"done","summary":"Added a file","pr_title":"Live test","checks":["sh test.sh"]}' > .devbot/result.json
  ;;
red)
  mkdir -p live && echo "devbot live test" > "$f" && touch FAIL && git add "$f" FAIL && git commit -qm "Add $f and FAIL"
  printf '%s' '{"status":"done","summary":"Added a file that fails CI","pr_title":"Live test: red"}' > .devbot/result.json
  ;;
carry)
  [ -e FAIL ] && git rm -q FAIL
  mkdir -p live && echo "devbot live test" > "$f" && git add "$f" && git commit -qm "Remove FAIL, add $f"
  printf '%s' '{"status":"done","summary":"Removed FAIL, so CI passes","pr_title":"Live test: carried","checks":["sh test.sh"]}' > .devbot/result.json
  ;;
approve)
  printf '{"verdict":"approve","summary":"Looks right. Attachments seen: %s"}' "$(ls .devbot/attachments 2>/dev/null | tr '\n' ' ')" > .devbot/result.json
  ;;
esac
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s1","num_turns":1,"usage":{"input_tokens":1,"output_tokens":1}}'
`

const livePNG = "\x89PNG\r\n\x1a\nlive"

// The stub never calls a model, but the harness won't run without a proxy
// to hand it; nothing listens here.
var liveProxy = &agent.ModelProxy{URL: "http://127.0.0.1:8462", Token: "devbot-proxy-test", Credential: agent.CredAPIKey}

type live struct {
	t        *testing.T
	ctx      context.Context
	c        *Client
	repo     forge.RepoRef
	e        *core.Engine
	pods     map[forge.Role]*core.Engine
	r        *harness.Runner
	modeFile string
}

// env reads the named variables, skipping the test when any is unset.
func env(t *testing.T, names ...string) []string {
	t.Helper()
	var vals, missing []string
	for _, n := range names {
		v := os.Getenv(n)
		if v == "" {
			missing = append(missing, n)
		}
		vals = append(vals, v)
	}
	if len(missing) > 0 {
		t.Skipf("live GitHub test: set %s (see docs/operating.md, Running the live GitHub test)", strings.Join(missing, ", "))
	}
	return vals
}

func liveRepo(t *testing.T, full string) forge.RepoRef {
	t.Helper()
	r, err := forge.ParseRepo("github:" + full)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type logWriter struct{ t *testing.T }

func (w logWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// newLive builds the adapter from the Apps' keys and the engine and harness
// around it, with Redis in memory. The keys are only ever in this process:
// the stub's environment is scrubbed of anything named like GITHUB.
func newLive(t *testing.T) *live {
	v := env(t, "DEVBOT_LIVE_GITHUB_IMPLEMENTER_APP_ID", "DEVBOT_LIVE_GITHUB_REVIEWER_APP_ID", "DEVBOT_LIVE_GITHUB_REPO",
		"GITHUB_APP_DEVBOT_KEY", "GITHUB_APP_REVIEW_KEY")
	implID, err1 := strconv.ParseInt(v[0], 10, 64)
	revID, err2 := strconv.ParseInt(v[1], 10, 64)
	if err := errors.Join(err1, err2); err != nil {
		t.Fatal(err)
	}
	api := os.Getenv("DEVBOT_LIVE_GITHUB_URL")
	if api == "" {
		api = "https://api.github.com"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	t.Cleanup(cancel)
	base := Config{
		Name: "github", URL: api,
		ImplementerAppID: implID, ImplementerKey: []byte(v[3]),
		ReviewerAppID: revID, ReviewerKey: []byte(v[4]),
	}
	serveCfg := base
	serveCfg.WebhookSecret = os.Getenv("GITHUB_WEBHOOK_SECRET")
	serveCfg.OAuthClientID, serveCfg.OAuthClientSecret = os.Getenv("GITHUB_OAUTH_CLIENT_ID"), os.Getenv("GITHUB_OAUTH_CLIENT_SECRET")
	c, err := New(ctx, serveCfg)
	if err != nil {
		t.Fatal(err)
	}
	impl, rev := c.BotUsers()
	t.Logf("implementer App %s, reviewer App %s", impl, rev)

	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	reg := forge.Registry{"github": c}
	log := slog.New(slog.NewTextHandler(logWriter{t}, nil))
	e := &core.Engine{Forges: reg, Q: queue.New(miniredis.RunT(t).Addr(), 0), Cfg: cfg, Repos: &config.RepoCache{Forges: reg}, Log: log}
	// Each job runs as its pod does: with its own App's key alone.
	implCfg, revCfg := base, base
	implCfg.ReviewerKey, implCfg.ReviewerLogin = nil, rev
	revCfg.ImplementerKey, revCfg.ImplementerLogin = nil, impl
	pods := map[forge.Role]*core.Engine{}
	for role, pc := range map[forge.Role]Config{forge.RoleImplementer: implCfg, forge.RoleReviewer: revCfg} {
		pcl, err := New(ctx, pc)
		if err != nil {
			t.Fatal(err)
		}
		preg := forge.Registry{"github": pcl}
		pods[role] = &core.Engine{Forges: preg, Q: e.Q, Cfg: cfg, Repos: &config.RepoCache{Forges: preg}, Log: log}
	}

	dir := t.TempDir()
	modeFile := filepath.Join(dir, "mode")
	stub := filepath.Join(dir, "claude")
	if err := os.WriteFile(stub, []byte(strings.ReplaceAll(liveStub, "MODE_FILE", modeFile)), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &harness.Runner{
		E: e, Owner: "live-test", Log: log,
		Workdir: filepath.Join(dir, "work"), PrivateDir: filepath.Join(dir, "priv"),
		Agent:    &agent.Local{Harnesses: map[string]agent.Harness{"claude": &claude.Harness{Command: stub, Proxy: liveProxy}}},
		Identity: func(string) (string, string) { return impl, impl + "@users.noreply.github.com" },
	}
	for _, d := range []string{r.Workdir, r.PrivateDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return &live{t: t, ctx: ctx, c: c, repo: liveRepo(t, v[2]), e: e, pods: pods, r: r, modeFile: modeFile}
}

func (l *live) mode(m string) {
	if err := os.WriteFile(l.modeFile, []byte(m), 0o644); err != nil {
		l.t.Fatal(err)
	}
}

// eval is one evaluation, as the reconcile loop would make it: the test
// gets no webhooks.
func (l *live) eval(ref forge.ItemRef) {
	if _, err := l.e.EvaluateItem(l.ctx, ref, core.SrcReconcile); err != nil {
		l.t.Logf("evaluate %s: %v", ref, err)
	}
}

func (l *live) item(ref forge.ItemRef) forge.Item {
	l.t.Helper()
	it, err := l.c.GetItem(l.ctx, ref)
	if err != nil {
		l.t.Fatal(err)
	}
	return it
}

// until evaluates ref every few seconds until ok, or fails the test.
func (l *live) until(what string, d time.Duration, ref forge.ItemRef, ok func() bool) {
	l.t.Helper()
	deadline := time.Now().Add(d)
	for {
		l.eval(ref)
		if ok() {
			return
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("%s: still waiting after %s (state %q)", what, d, core.State(l.item(ref)))
		}
		time.Sleep(10 * time.Second)
	}
}

// listed polls list until it holds what the test just made, and logs how
// long that took: GitHub's listings can lag a new issue by some seconds. It
// reports false when two minutes go by without it.
func (l *live) listed(what string, list func() bool) bool {
	l.t.Helper()
	start := time.Now()
	for !list() {
		if time.Since(start) > 2*time.Minute {
			return false
		}
		time.Sleep(5 * time.Second)
	}
	l.t.Logf("%s listed after %s", what, time.Since(start).Round(time.Second))
	return true
}

func (l *live) queued(kind string, ref forge.ItemRef) bool {
	q, err := l.e.Q.IsQueued(l.ctx, kind, ref.String())
	return err == nil && q
}

func (l *live) file(title string, c core.Caller) forge.ItemRef {
	l.t.Helper()
	return l.fileTask(core.FileRequest{Title: title, Automerge: true}, c)
}

// fileTask files req on the test repo, with acceptance criteria the stub
// meets, and closes the issue when the test ends.
func (l *live) fileTask(req core.FileRequest, c core.Caller) forge.ItemRef {
	l.t.Helper()
	req.Repo, req.AcceptanceCriteria = l.repo.String(), "- a file under live/ exists"
	st, err := l.e.FileTask(l.ctx, req, c)
	if err != nil {
		l.t.Fatal(err)
	}
	ref, err := forge.ParseItem(st.Task)
	if err != nil {
		l.t.Fatal(err)
	}
	l.t.Logf("filed %s", st.URL)
	l.t.Cleanup(func() {
		if it, err := l.c.GetItem(context.Background(), ref); err == nil && it.State == "open" {
			l.c.CloseItem(context.Background(), ref)
		}
	})
	return ref
}

// cleanupBranch deletes the branch when the test ends, so the test repo
// doesn't fill up with them. A branch devbot already deleted (after a merge)
// is fine; any other failure is an error: it's the only sign the repo needs
// tidying by hand.
func (l *live) cleanupBranch(r forge.RepoRef, branch string) {
	l.t.Cleanup(func() {
		ctx := context.Background()
		var err error
		if branch == attachBranch {
			// DeleteBranch refuses this one on purpose; only the test takes it away.
			var cr cred
			if cr, err = l.c.repoCred(ctx, forge.RoleImplementer, r); err == nil {
				_, err = l.c.do(ctx, cr, "DELETE", repoPath(r)+"/git/refs/heads/"+escapePath(branch), nil, nil)
			}
		} else {
			err = l.c.DeleteBranch(ctx, r, branch)
		}
		if err != nil && !errors.Is(err, forge.ErrNotFound) {
			l.t.Errorf("cleanup: deleting %s on %s: %v (remove it by hand)", branch, r, err)
		}
	})
}

// cleanupPR closes the PR when it's still open, then deletes its branch.
func (l *live) cleanupPR(r forge.RepoRef, pr forge.PR) {
	l.cleanupBranch(r, pr.HeadBranch)
	l.closeWhenDone(r, pr.Number)
}

// closeWhenDone closes the PR when the test ends, if it's still open.
// Cleanups run last-registered first, so register it after its branch's.
func (l *live) closeWhenDone(r forge.RepoRef, number int) {
	l.t.Cleanup(func() {
		ctx := context.Background()
		got, err := l.c.GetPR(ctx, r, number)
		if err == nil && got.State != "open" {
			return
		}
		cr, err := l.c.repoCred(ctx, forge.RoleImplementer, r)
		if err == nil {
			_, err = l.c.do(ctx, cr, "PATCH", fmt.Sprintf("%s/pulls/%d", repoPath(r), number), map[string]string{"state": "closed"}, nil)
		}
		if err != nil {
			l.t.Errorf("cleanup: closing PR #%d on %s: %v (close it by hand)", number, r, err)
		}
	})
}

// job runs one job in a pod of role, then the evaluation the pod asks serve
// for.
func (l *live) job(role forge.Role, run func(context.Context) error) error {
	l.r.E = l.pods[role]
	err := run(l.ctx)
	if _, eerr := l.e.EvaluateRequested(l.ctx); eerr != nil {
		l.t.Logf("evaluating after the job: %v", eerr)
	}
	return err
}

// implement runs the queued implement job and returns the PR it opened.
func (l *live) implement(ref forge.ItemRef) forge.PR {
	l.t.Helper()
	if !l.queued(queue.KindImplement, ref) {
		l.t.Fatalf("%s isn't queued for the implementer (state %q)", ref, core.State(l.item(ref)))
	}
	if err := l.job(forge.RoleImplementer, l.r.Implement); err != nil {
		l.t.Fatal(err)
	}
	it := l.item(ref)
	pr, err := l.e.FindPR(l.ctx, it)
	if err != nil || pr == nil {
		l.t.Fatalf("no PR for %s (state %q): %v", ref, core.State(it), err)
	}
	l.cleanupPR(ref.Repo, *pr)
	if s := core.State(it); s != core.LInReview {
		l.t.Fatalf("after the implementer: want %s, got %q", core.LInReview, s)
	}
	l.t.Logf("opened %s", pr.URL)
	return *pr
}

// approve waits for CI to pass on pr's head and a review to be queued, then
// runs the reviewer, which approves.
func (l *live) approve(ref forge.ItemRef, pr forge.PR) {
	l.t.Helper()
	// No checks at all also queues a review, so wait for CI itself: GitHub
	// may not have created the check suite yet.
	l.until("CI to pass and a review to be queued", 15*time.Minute, ref, func() bool {
		state, details, err := l.c.CombinedChecks(l.ctx, l.repo, pr.HeadSHA)
		l.t.Logf("checks on %s: %s %+v %v", pr.HeadSHA, state, details, err)
		return state == forge.ChecksSuccess && l.queued(queue.KindReview, ref)
	})
	l.mode("approve")
	if err := l.job(forge.RoleReviewer, l.r.Review); err != nil {
		l.t.Fatal(err)
	}
}

// statuses are the bots' status markers on ref.
func (l *live) statuses(ref forge.ItemRef) []core.StatusComment {
	l.t.Helper()
	cs, err := l.c.ListComments(l.ctx, ref)
	if err != nil {
		l.t.Fatal(err)
	}
	impl, rev := l.c.BotUsers()
	return core.Statuses(cs, impl, rev)
}

// branch has the implementer App make branch name at from's head on repo,
// and deletes it when the test ends.
func (l *live) branch(t *testing.T, repo forge.RepoRef, from, name string) string {
	t.Helper()
	cr, err := l.c.repoCred(l.ctx, forge.RoleImplementer, repo)
	if err != nil {
		t.Fatal(err)
	}
	l.branchAs(t, cr, repo, l.head(t, cr, repo, from), name)
	l.cleanupBranch(repo, name)
	return name
}

// head is the commit branch is at on repo, read as cr.
func (l *live) head(t *testing.T, cr cred, repo forge.RepoRef, branch string) string {
	t.Helper()
	var head struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if _, err := l.c.do(l.ctx, cr, "GET", repoPath(repo)+"/git/ref/heads/"+escapePath(branch), nil, &head); err != nil {
		t.Fatal(err)
	}
	return head.Object.SHA
}

// branchAs has cr make branch name at sha on repo.
func (l *live) branchAs(t *testing.T, cr cred, repo forge.RepoRef, sha, name string) {
	t.Helper()
	if _, err := l.c.do(l.ctx, cr, "POST", repoPath(repo)+"/git/refs", map[string]string{"ref": "refs/heads/" + name, "sha": sha}, nil); err != nil {
		t.Fatal(err)
	}
}

// commit has the implementer App commit a one-line file at path on branch,
// and returns the commit.
func (l *live) commit(t *testing.T, repo forge.RepoRef, branch, path, msg string) string {
	t.Helper()
	cr, err := l.c.repoCred(l.ctx, forge.RoleImplementer, repo)
	if err != nil {
		t.Fatal(err)
	}
	return l.commitAs(t, cr, repo, branch, path, msg)
}

// commitAs is commit as cr.
func (l *live) commitAs(t *testing.T, cr cred, repo forge.RepoRef, branch, path, msg string) string {
	t.Helper()
	var put struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if _, err := l.c.do(l.ctx, cr, "PUT", repoPath(repo)+"/contents/"+escapePath(path), map[string]string{
		"message": msg, "content": "ZGV2Ym90IGxpdmUgdGVzdAo=", "branch": branch,
	}, &put); err != nil {
		t.Fatal(err)
	}
	return put.Commit.SHA
}

func stamp() string { return strconv.FormatInt(time.Now().UnixNano(), 10) }

// Discovery, then a task from issue to merge: the implementer App pushes a
// branch and opens a PR with an attachment, CI passes, the reviewer App
// approves and merges, and the attachment is released.
func TestLiveIssueToMerge(t *testing.T) {
	l := newLive(t)
	ctx := l.ctx

	for _, role := range []forge.Role{forge.RoleImplementer, forge.RoleReviewer} {
		in, err := l.c.installation(ctx, l.c.app(role), l.repo)
		if err != nil {
			t.Fatalf("%s App on %s: %v", role, l.repo, err)
		}
		t.Logf("%s App: installation %d, permissions %v", role, in.ID, in.Permissions)
		for _, p := range []string{"contents", "pull_requests", "issues"} {
			if in.Permissions[p] != "write" {
				t.Errorf("%s App needs %s write, has %q", role, p, in.Permissions[p])
			}
		}
		if ok, err := l.c.CanWrite(ctx, l.repo, role); err != nil || !ok {
			t.Fatalf("%s App can't write to %s: %v", role, l.repo, err)
		}
	}

	// devbot makes its attachments branch on first use; when that's this
	// run, take it away again afterwards.
	cr, err := l.c.repoCred(ctx, forge.RoleImplementer, l.repo)
	if err != nil {
		t.Fatal(err)
	}
	if head, err := l.c.attachHead(ctx, cr, l.repo, false); err != nil {
		t.Fatal(err)
	} else if head == "" {
		l.cleanupBranch(l.repo, attachBranch)
	}

	l.mode("green")
	ref := l.file("Live test: issue to merge", core.Caller{Principal: "live-test"})

	// Install discovery: the reconcile loop's way of finding work, through
	// every installation of the implementer App.
	var found []forge.Item
	if !l.listed(ref.String(), func() bool {
		found, err = l.c.SearchItems(ctx, core.LReady, "open")
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(found, func(it forge.Item) bool { return it.Ref == ref })
	}) {
		t.Fatalf("SearchItems(%s) didn't find %s among %d items", core.LReady, ref, len(found))
	}

	pr := l.implement(ref)
	m, _ := core.ParsePRMeta(pr.Body)
	if len(m.Attachments) != 1 || m.Attachments[0].Name != "board.png" || !strings.Contains(pr.Body, m.Attachments[0].URL) {
		t.Fatalf("want board.png attached and linked in the body, got %+v", m.Attachments)
	}
	att := m.Attachments[0]
	got, err := l.c.GetAttachment(ctx, l.repo, pr.Number, att.URL, forge.RoleReviewer)
	if err != nil || string(got) != livePNG {
		t.Fatalf("reviewer reading %s: %q, %v", att.URL, got, err)
	}

	l.approve(ref, pr)
	reviews, err := l.c.Reviews(ctx, l.repo, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	_, reviewer := l.c.BotUsers()
	if !slices.ContainsFunc(reviews, func(r forge.Review) bool {
		return r.Author == reviewer && r.Verdict == forge.VerdictApprove && strings.Contains(r.Body, "board.png")
	}) {
		t.Fatalf("want the reviewer App's approval, having seen board.png: %+v", reviews)
	}

	l.until("the merge", 5*time.Minute, ref, func() bool {
		p, err := l.c.GetPR(ctx, l.repo, pr.Number)
		return err == nil && p.Merged
	})
	l.eval(ref) // the merge is recorded by the evaluation after it
	if it := l.item(ref); it.State != "closed" || core.State(it) != "" {
		t.Fatalf("after the merge: want the issue closed with no state label, got %s %q", it.State, core.State(it))
	}
	path, ok := l.c.attachedPath(l.repo, pr.Number, att.URL)
	if !ok {
		t.Fatalf("%s isn't an attachment of PR #%d", att.URL, pr.Number)
	}
	if _, err := l.c.GetFile(ctx, l.repo, attachBranch, path); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("%s on %s should be gone after the merge: %v", path, attachBranch, err)
	}

	// Branch cleanup: the task's branch goes with the merge, the attachments
	// branch, under devbot/ too but nobody's task, stays.
	if ok, err := l.c.BranchExists(ctx, l.repo, pr.HeadBranch); err != nil || ok {
		t.Fatalf("%s should be deleted after the merge: exists %v, %v", pr.HeadBranch, ok, err)
	}
	if ok, err := l.c.BranchExists(ctx, l.repo, attachBranch); err != nil || !ok {
		t.Fatalf("%s should be kept: exists %v, %v", attachBranch, ok, err)
	}
}

// A PR whose CI fails goes back to the implementer instead of to review.
func TestLiveFailingCI(t *testing.T) {
	l := newLive(t)
	l.mode("red")
	ref := l.file("Live test: failing CI", core.Caller{Principal: "live-test"})
	pr := l.implement(ref)
	l.until("CI to fail", 15*time.Minute, ref, func() bool {
		return core.State(l.item(ref)) == core.LChanges
	})
	state, details, err := l.c.CombinedChecks(l.ctx, l.repo, pr.HeadSHA)
	t.Logf("checks on %s: %s %+v %v", pr.HeadSHA, state, details, err)
	if state != forge.ChecksFailure {
		t.Fatalf("want failing checks, got %s", state)
	}
}

// A person signed in through the sign-in App files a task as themselves:
// the issue is theirs, they applied devbot/ready, and devbot takes it.
func TestLiveSignedInUser(t *testing.T) {
	l := newLive(t)
	v := env(t, "DEVBOT_LIVE_GITHUB_USER_TOKEN", "GITHUB_OAUTH_CLIENT_ID")
	ctx := l.ctx

	if _, err := l.c.OAuthURL("https://devbot.example.com/oauth/callback", "state", "challenge"); err != nil {
		t.Fatal(err)
	}
	login, err := l.c.CurrentUser(ctx, v[0])
	if err != nil {
		t.Fatal(err)
	}
	u := l.c.AsUser(v[0])
	perm, err := u.Permission(ctx, l.repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("signed in as %s, %s on %s", login, perm, l.repo)
	if perm < l.e.Cfg.Authorization.Min {
		t.Fatalf("%s needs %s on %s to direct devbot, has %s", login, l.e.Cfg.Authorization.Min, l.repo, perm)
	}

	ref := l.file("Live test: filed by a signed-in person", core.Caller{Principal: login, User: u, Provider: "github", Login: login})
	it := l.item(ref)
	if it.Author != login || !it.HasLabel(core.LReady) {
		t.Fatalf("want an issue by %s labelled %s, got one by %s with %v", login, core.LReady, it.Author, it.Labels)
	}
	var evs []forge.LabelEvent
	if !l.listed(core.LReady+" added by "+login, func() bool {
		evs, err = l.c.LabelEvents(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(evs, func(e forge.LabelEvent) bool { return e.Label == core.LReady && e.Added && e.Actor == login })
	}) {
		t.Fatalf("want %s added by %s, got %+v", core.LReady, login, evs)
	}
	if !l.queued(queue.KindImplement, ref) {
		t.Fatalf("devbot didn't take a task %s filed (state %q)", login, core.State(l.item(ref)))
	}
	if err := u.CloseItem(ctx, ref); err != nil {
		t.Fatalf("closing the issue as its author: %v", err)
	}
}

// A person who can direct devbot edits the body of an approved issue filed
// by someone else (here the implementer App): the history GitHub reports
// holds that one edit, by them, and not the body as filed, so the task
// carries on to a PR.
func TestLiveEditedByAnAuthorizedUser(t *testing.T) {
	l := newLive(t)
	v := env(t, "DEVBOT_LIVE_GITHUB_USER_TOKEN")
	ctx := l.ctx
	login, err := l.c.CurrentUser(ctx, v[0])
	if err != nil {
		t.Fatal(err)
	}

	l.mode("green")
	ref := l.file("Live test: edited after approval", core.Caller{Principal: "live-test"})
	it := l.item(ref)
	if _, err := l.c.do(ctx, userCred(v[0]), "PATCH", fmt.Sprintf("%s/issues/%d", repoPath(ref.Repo), ref.Number),
		map[string]string{"body": it.Body + "\n\nClarified by a maintainer."}, nil); err != nil {
		t.Fatalf("editing the body as %s: %v", login, err)
	}
	var edits []forge.ContentEdit
	if !l.listed("the body edit", func() bool {
		edits, err = l.c.ContentEdits(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		return len(edits) > 0
	}) {
		t.Fatal("no edit in the issue's history")
	}
	if len(edits) != 1 || edits[0].Field != forge.FieldBody || edits[0].Editor != login {
		t.Fatalf("want one body edit, by %s, got %+v", login, edits)
	}
	l.implement(ref)
}

// A person who can direct devbot edits the issue's body after devbot started
// on it: devbot pins the new text, saying who changed it, and the review
// carries on to an approval.
func TestLiveBodyEditAfterApproval(t *testing.T) {
	l := newLive(t)
	v := env(t, "DEVBOT_LIVE_GITHUB_USER_TOKEN")
	ctx := l.ctx
	login, err := l.c.CurrentUser(ctx, v[0])
	if err != nil {
		t.Fatal(err)
	}
	if perm, err := l.c.AsUser(v[0]).Permission(ctx, l.repo); err != nil || perm < l.e.Cfg.Authorization.Min {
		t.Fatalf("%s needs %s on %s to direct devbot, has %s (%v)", login, l.e.Cfg.Authorization.Min, l.repo, perm, err)
	}

	l.mode("plain")
	ref := l.fileTask(core.FileRequest{Title: "Live test: body edited after approval"}, core.Caller{Principal: "live-test"})
	pr := l.implement(ref)
	if _, ok := core.LastStatus(l.statuses(ref), "started"); !ok {
		t.Fatal("the implementer's started status, which pins the issue, is missing")
	}
	it := l.item(ref)
	if _, err := l.c.do(ctx, userCred(v[0]), "PATCH", fmt.Sprintf("%s/issues/%d", repoPath(l.repo), ref.Number),
		map[string]string{"body": it.Body + "\n- and a line " + login + " added"}, nil); err != nil {
		t.Fatalf("editing the body as %s: %v", login, err)
	}
	// Until GitHub's edit history has the edit, devbot would stop the task
	// for an edit nobody is named for.
	var edits []forge.ContentEdit
	if !l.listed("the body edit by "+login, func() bool {
		edits, err = l.c.ContentEdits(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(edits, func(e forge.ContentEdit) bool { return e.Field == forge.FieldBody && e.Editor == login })
	}) {
		t.Fatalf("want a body edit by %s, got %+v", login, edits)
	}

	l.approve(ref, pr)
	pinned, ok := core.LastStatus(l.statuses(ref), "pinned")
	if !ok || !strings.Contains(pinned.Body, "@"+login) || pinned.Pin == "" {
		t.Fatalf("want the new text pinned, naming @%s (state %q): %+v", login, core.State(l.item(ref)), pinned)
	}
	_, reviewer := l.c.BotUsers()
	reviews, err := l.c.Reviews(ctx, l.repo, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(reviews, func(r forge.Review) bool { return r.Author == reviewer && r.Verdict == forge.VerdictApprove }) {
		t.Fatalf("want the reviewer App's approval after the re-pin (state %q): %+v", core.State(l.item(ref)), reviews)
	}
}

// edit_task mid-task: devbot edits the issue as the implementer App and pins
// the new text, the PR goes back to the implementer with what changed, and
// the review that follows approves it against the new text.
func TestLiveEditTask(t *testing.T) {
	l := newLive(t)
	ctx := l.ctx
	l.mode("plain")
	ref := l.fileTask(core.FileRequest{Title: "Live test: edit_task"}, core.Caller{Principal: "live-test"})
	pr := l.implement(ref)

	ac := "- a file under live/ exists\n- and a second one, added after the edit"
	if _, err := l.e.EditTask(ctx, core.EditRequest{Ref: ref.String(), AcceptanceCriteria: ac, Note: "live test redirect"}, core.Caller{Principal: "live-test"}); err != nil {
		t.Fatal(err)
	}
	if it := l.item(ref); !strings.Contains(it.Body, ac) || core.State(it) != core.LChanges {
		t.Fatalf("want the edit made and the PR sent back: state %q, body %q", core.State(it), it.Body)
	}
	if pinned, ok := core.LastStatus(l.statuses(ref), "pinned"); !ok || pinned.Mode != "edit" || pinned.Pin != core.PinOf(l.item(ref)) {
		t.Fatalf("want the new text pinned: %+v", pinned)
	}
	if !l.queued(queue.KindImplement, ref) {
		t.Fatal("want the edit's fix round queued")
	}
	if err := l.job(forge.RoleImplementer, l.r.Implement); err != nil {
		t.Fatal(err)
	}
	if st := core.State(l.item(ref)); st != core.LInReview {
		t.Fatalf("after the fix round: want %s, got %q", core.LInReview, st)
	}
	got, err := l.c.GetPR(ctx, l.repo, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	l.approve(ref, got)
	_, reviewer := l.c.BotUsers()
	reviews, err := l.c.Reviews(ctx, l.repo, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(reviews, func(r forge.Review) bool {
		return r.Author == reviewer && r.Verdict == forge.VerdictApprove && r.CommitSHA == got.HeadSHA
	}) {
		t.Fatalf("want the reviewer App's approval of the new text (state %q): %+v", core.State(l.item(ref)), reviews)
	}
	if s, ok := core.LastStatus(l.statuses(ref), "needs-human"); ok {
		t.Fatalf("devbot's own edit stopped the task: %s", s.Body)
	}
}

// A task filed with a base opens its PR against that branch. Moving the PR
// onto another base on GitHub is adopted as a retarget, and a retarget
// through the connector moves it back; each starts the counts over.
func TestLiveBaseAndRetarget(t *testing.T) {
	l := newLive(t)
	ctx := l.ctx
	def, err := l.c.DefaultBranch(ctx, l.repo)
	if err != nil {
		t.Fatal(err)
	}
	s := stamp()
	a := l.branch(t, l.repo, def, "devbot-live/base-a-"+s)
	b := l.branch(t, l.repo, def, "devbot-live/base-b-"+s)

	l.mode("plain")
	ref := l.fileTask(core.FileRequest{Title: "Live test: base and retarget", Base: a}, core.Caller{Principal: "live-test"})
	// A failed attempt on record, for the retargets to start over from.
	if err := l.c.Comment(ctx, ref, "Live test: an earlier attempt failed.\n\n"+core.RenderStatus(core.Status{Kind: "failed", Role: "implement", Attempt: 1})); err != nil {
		t.Fatal(err)
	}
	pr := l.implement(ref)
	if pr.BaseBranch != a {
		t.Fatalf("want the PR against %s, got %s", a, pr.BaseBranch)
	}
	if n := core.FailedAttempts(l.statuses(ref)); n != 1 {
		t.Fatalf("want 1 failed attempt on record, got %d", n)
	}

	// startedOver checks the latest reset is a retarget onto base, and that
	// the counts start over from it.
	startedOver := func(base, says string) {
		t.Helper()
		ss := l.statuses(ref)
		reset, ok := core.LastStatus(ss, "reset")
		if !ok || reset.Mode != "retarget" || reset.Base != base || !strings.Contains(reset.Body, says) {
			t.Fatalf("want a retarget onto %s saying %q, got %+v", base, says, reset)
		}
		if core.FailedAttempts(ss) != 0 || core.ReviewRound(ss) != 1 {
			t.Fatalf("the counts should start over: %d failed, review round %d", core.FailedAttempts(ss), core.ReviewRound(ss))
		}
	}

	// Someone moves the PR onto b on GitHub.
	cr, err := l.c.repoCred(ctx, forge.RoleImplementer, l.repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.c.do(ctx, cr, "PATCH", fmt.Sprintf("%s/pulls/%d", repoPath(l.repo), pr.Number), map[string]string{"base": b}, nil); err != nil {
		t.Fatal(err)
	}
	l.eval(ref)
	startedOver(b, "changed on the forge from `"+a+"` to `"+b+"`")
	if st := core.State(l.item(ref)); st != core.LChanges || !l.queued(queue.KindImplement, ref) {
		t.Fatalf("want a rebase onto %s queued, got state %q", b, st)
	}

	// The connector moves it back.
	st, err := l.e.Retarget(ctx, ref.String(), a, core.Caller{Principal: "live-test"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.c.GetPR(ctx, l.repo, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseBranch != a || st.Base != a {
		t.Fatalf("want the PR back on %s: PR base %s, task base %q", a, got.BaseBranch, st.Base)
	}
	startedOver(a, "start over")
}

// Lazy rebase: without automerge, an approved PR whose base moves on is left
// approved and noted behind, and only rebased once someone asks.
func TestLiveLazyRebase(t *testing.T) {
	l := newLive(t)
	ctx := l.ctx
	def, err := l.c.DefaultBranch(ctx, l.repo)
	if err != nil {
		t.Fatal(err)
	}
	s := stamp()
	// A base of the test's own, so that moving it disturbs nothing else.
	base := l.branch(t, l.repo, def, "devbot-live/lazy-"+s)

	l.mode("plain")
	ref := l.fileTask(core.FileRequest{Title: "Live test: lazy rebase", Base: base}, core.Caller{Principal: "live-test"})
	pr := l.implement(ref)
	l.approve(ref, pr)
	l.until("the approval", 2*time.Minute, ref, func() bool {
		if p, err := l.c.GetPR(ctx, l.repo, pr.Number); err == nil && p.Merged {
			t.Fatalf("PR #%d merged: the test repo's .devbot.yaml must not turn automerge on", pr.Number)
		}
		return core.State(l.item(ref)) == core.LApproved
	})

	l.commit(t, l.repo, base, "live/"+s+"-base.txt", "Live test: move the base on")
	l.until("GitHub to see the PR behind its base", 5*time.Minute, ref, func() bool {
		p, err := l.c.GetPR(ctx, l.repo, pr.Number)
		return err == nil && p.Behind
	})
	l.eval(ref) // and once more, with nothing to do
	if st := core.State(l.item(ref)); st != core.LApproved || l.queued(queue.KindImplement, ref) {
		t.Fatalf("nothing should be rebased until asked: state %q, rebase queued %v", st, l.queued(queue.KindImplement, ref))
	}
	st, err := l.e.Status(ctx, core.Caller{Principal: "live-test"}, ref.String())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.Note, "behind `"+base+"`") {
		t.Errorf("want the task noted behind %s, got %q", base, st.Note)
	}

	if _, err := l.e.Rebase(ctx, ref.String(), core.Caller{Principal: "live-test"}); err != nil {
		t.Fatal(err)
	}
	if st := core.State(l.item(ref)); st != core.LChanges || !l.queued(queue.KindImplement, ref) {
		t.Fatalf("once asked, a rebase should be queued: state %q", st)
	}
}

// The required-reviews experiment: with the default branch requiring one
// approving review, does the reviewer App's approval make the PR mergeable?
// Run once under a ruleset and once under classic branch protection, each set
// up for the run and removed after it, even on failure. GitHub's free plan has
// neither on private repos, so the repo (which may be the test repo) has to be
// public for the run, or on a paid plan; each run skips when GitHub refuses.
// Setting the rules up takes a person's token with Administration write there.
// On GitHub.com on 2026-10-17 the approval counted under both, and the
// reviewer App merged; GitHub could change that, so the test keeps asking.
func TestLiveRequiredReviews(t *testing.T) {
	l := newLive(t)
	v := env(t, "DEVBOT_LIVE_GITHUB_PROTECTION_REPO", "DEVBOT_LIVE_GITHUB_ADMIN_TOKEN")
	repo := liveRepo(t, v[0])
	admin := userCred(v[1])
	ctx := l.ctx
	base, err := l.c.DefaultBranch(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}

	rules := map[string]func(t *testing.T) (undo func()){
		"ruleset": func(t *testing.T) func() {
			var rs struct {
				ID int64 `json:"id"`
			}
			_, err := l.c.do(ctx, admin, "POST", repoPath(repo)+"/rulesets", map[string]any{
				"name": "devbot-live-required-review", "target": "branch", "enforcement": "active",
				"conditions": map[string]any{"ref_name": map[string]any{"include": []string{"~DEFAULT_BRANCH"}, "exclude": []string{}}},
				"rules": []map[string]any{{"type": "pull_request", "parameters": map[string]any{
					"required_approving_review_count": 1, "dismiss_stale_reviews_on_push": false, "require_code_owner_review": false,
					"require_last_push_approval": false, "required_review_thread_resolution": false,
				}}},
			}, &rs)
			if errors.Is(err, forge.ErrForbidden) {
				t.Skipf("required-reviews experiment: %s can't have rulesets (make it public for the run, or use a paid plan): %v", repo, err)
			}
			if err != nil {
				t.Fatal(err)
			}
			return func() {
				if _, err := l.c.do(context.Background(), admin, "DELETE", fmt.Sprintf("%s/rulesets/%d", repoPath(repo), rs.ID), nil, nil); err != nil {
					t.Errorf("cleanup: deleting ruleset %d on %s: %v (remove it by hand)", rs.ID, repo, err)
				}
			}
		},
		"classic": func(t *testing.T) func() {
			p := repoPath(repo) + "/branches/" + escapePath(base) + "/protection"
			_, err := l.c.do(ctx, admin, "PUT", p, map[string]any{
				"required_status_checks": nil, "enforce_admins": false, "restrictions": nil,
				"required_pull_request_reviews": map[string]any{"required_approving_review_count": 1},
			}, nil)
			if errors.Is(err, forge.ErrForbidden) {
				t.Skipf("required-reviews experiment: %s can't have branch protection (make it public for the run, or use a paid plan): %v", repo, err)
			}
			if err != nil {
				t.Fatal(err)
			}
			return func() {
				if _, err := l.c.do(context.Background(), admin, "DELETE", p, nil, nil); err != nil {
					t.Errorf("cleanup: removing the protection on %s of %s: %v (remove it by hand)", base, repo, err)
				}
			}
		},
	}
	for _, kind := range []string{"ruleset", "classic"} {
		t.Run(kind, func(t *testing.T) {
			undo := rules[kind](t)
			defer undo()
			pr := l.protectedPR(t, repo, base, kind)
			prot, err := l.c.Protection(ctx, repo, base)
			t.Logf("rules devbot reads on %s: %+v %v", base, prot, err)

			before := l.settled(t, repo, pr.Number)
			if err := l.c.Review(ctx, repo, pr.Number, pr.HeadSHA, forge.VerdictApprove, "devbot live test: required-reviews experiment"); err != nil {
				t.Fatal(err)
			}
			time.Sleep(5 * time.Second) // GitHub recomputes mergeable_state after a review
			after := l.settled(t, repo, pr.Number)
			merr := l.c.Merge(ctx, repo, pr.Number, pr.HeadSHA, "squash")
			t.Logf("RESULT %s: blocked before the reviewer App's approval: %v; blocked after: %v; merge by the reviewer App: %v",
				kind, before.Blocked, after.Blocked, errOr(merr, "merged"))
			if !before.Blocked {
				t.Errorf("the PR wasn't blocked before any approval, so the %s isn't in force", kind)
			}
		})
	}
}

func errOr(err error, ok string) string {
	if err != nil {
		return err.Error()
	}
	return ok
}

// protectedPR has the implementer App push a one-file branch off base and
// open a PR from it.
func (l *live) protectedPR(t *testing.T, repo forge.RepoRef, base, kind string) forge.PR {
	t.Helper()
	s := stamp()
	branch := l.branch(t, repo, base, "devbot-live/required-reviews-"+kind+"-"+s)
	sha := l.commit(t, repo, branch, "live/"+s+".txt", "Live test: required reviews ("+kind+")")
	pr, err := l.c.OpenPR(l.ctx, repo, branch, base, "Live test: required reviews ("+kind+")", "devbot's live test. Safe to close.")
	if err != nil {
		t.Fatal(err)
	}
	l.closeWhenDone(repo, pr.Number)
	t.Logf("opened %s", pr.URL)
	if pr.HeadSHA != sha {
		t.Fatalf("PR head %s, pushed %s", pr.HeadSHA, sha)
	}
	return pr
}

// settled is the PR once GitHub has worked out whether it can merge.
func (l *live) settled(t *testing.T, repo forge.RepoRef, number int) forge.PR {
	t.Helper()
	for range 12 {
		pr, err := l.c.GetPR(l.ctx, repo, number)
		if err != nil {
			t.Fatal(err)
		}
		if !pr.MergeableUnknown {
			return pr
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("GitHub hasn't worked out whether PR #%d can merge", number)
	return forge.PR{}
}

// A same-repo PR whose CI fails, handed to devbot with devbot/ready: devbot
// opens one tracking issue for it, the stub's fix goes onto the PR's own
// branch, and the reviewer App approves it, after which the gate does what
// the test repo says (merges, or leaves it at devbot/approved).
func TestLiveCarrySameRepoPR(t *testing.T) {
	l := newLive(t)
	ctx := l.ctx
	def, err := l.c.DefaultBranch(ctx, l.repo)
	if err != nil {
		t.Fatal(err)
	}
	s := stamp()
	branch := l.branch(t, l.repo, def, "devbot-live/carry-"+s)
	start := l.commit(t, l.repo, branch, "live/"+s+"-carry.txt", "Live test: a change to carry")
	sha := l.commit(t, l.repo, branch, "FAIL", "Live test: fail CI")
	pr, err := l.c.OpenPR(ctx, l.repo, branch, def, "Live test: carry a same-repo PR", "devbot's live test. Safe to close.")
	if err != nil {
		t.Fatal(err)
	}
	l.closeWhenDone(l.repo, pr.Number)
	t.Logf("opened %s", pr.URL)

	// The label goes on as the person when there is one who can direct
	// devbot, else as the implementer App, which always can.
	impl, _ := l.c.BotUsers()
	labeller, as := impl, cred{}
	if tok := os.Getenv("DEVBOT_LIVE_GITHUB_USER_TOKEN"); tok != "" {
		login, err := l.c.CurrentUser(ctx, tok)
		if err != nil {
			t.Fatal(err)
		}
		if perm, err := l.c.AsUser(tok).Permission(ctx, l.repo); err == nil && perm >= l.e.Cfg.Authorization.Min {
			labeller, as = login, userCred(tok)
		} else {
			t.Logf("%s can't direct devbot on %s (%s, %v): labelling as the implementer App", login, l.repo, perm, err)
		}
	}
	ref := l.adopt(t, pr, labeller, as)
	if m := l.meta(ref); m.Carry != pr.Number || m.Supersedes != 0 {
		t.Fatalf("want the task carrying PR #%d on its own branch, got %+v", pr.Number, m)
	}

	l.carryRun(ref)
	got, err := l.c.GetPR(ctx, l.repo, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadBranch != branch || got.HeadSHA == sha {
		t.Fatalf("want the fix pushed onto %s past %s, got %s at %s", branch, sha, got.HeadBranch, got.HeadSHA)
	}
	if !l.inHistory(t, l.repo, sha, got.HeadSHA) || !l.inHistory(t, l.repo, start, got.HeadSHA) {
		t.Fatalf("the author's commits should be under devbot's on %s", branch)
	}
	if _, err := l.c.GetFile(ctx, l.repo, got.HeadSHA, "FAIL"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("FAIL should be gone from %s: %v", got.HeadSHA, err)
	}
	l.noOtherPR(t, ref, pr.Number)
	if !l.botSaid(forge.ItemRef{Repo: l.repo, Number: pr.Number}, fmt.Sprintf("to this PR for #%d", ref.Number)) {
		t.Fatalf("want devbot's comment on PR #%d saying what it pushed", pr.Number)
	}

	l.carriedApproved(ref, got)
}

// A fork's PR handed to devbot, with GitHub's "Allow edits by maintainers"
// on and off. On, with a fork the implementer App's token reaches: devbot
// pushes onto the fork's branch, and the PR is carried to approval like a
// same-repo one. On, with a private fork, which the App's token never
// reaches (the test setup's, a fork of the private test repo): devbot
// fetches the PR's code from the test repo's refs/pull/<N>/head and carries
// it on devbot/pr-<N>, saying why on the PR, as with edits off. Off: devbot
// carries it on devbot/pr-<N> in the test repo, from the contributor's
// commit, opens its own PR crediting theirs, and once that merges closes
// theirs as superseded. The fork belongs to someone else, whose token pushes
// to it and opens the PRs.
func TestLiveCarryForkPR(t *testing.T) {
	v := env(t, "DEVBOT_LIVE_GITHUB_FORK_REPO", "DEVBOT_LIVE_GITHUB_USER_TOKEN")
	fork, owner := liveRepo(t, v[0]), userCred(v[1])

	t.Run("maintainer-edits", func(t *testing.T) {
		l := newLive(t)
		var vis struct {
			Private bool `json:"private"`
		}
		if _, err := l.c.do(l.ctx, owner, "GET", repoPath(fork), nil, &vis); err != nil {
			t.Fatal(err)
		}
		pr, sha := l.forkPR(t, fork, owner, true)
		impl, _ := l.c.BotUsers()
		ref := l.adopt(t, pr, impl, cred{})
		if vis.Private {
			t.Logf("%s is private, out of the implementer App's reach: want PR #%d carried on %s", fork, pr.Number, core.SupersedeBranch(pr.Number))
			if m := l.meta(ref); m.Supersedes != pr.Number || m.Carry != 0 {
				t.Fatalf("want the task superseding PR #%d, got %+v", pr.Number, m)
			}
			if !l.botSaid(forge.ItemRef{Repo: l.repo, Number: pr.Number}, "devbot can't push to the fork") {
				t.Fatalf("want devbot's comment on PR #%d saying it can't push to the fork", pr.Number)
			}
			l.supersededToMerge(t, ref, pr, sha)
			return
		}
		t.Logf("%s is public: want the fix pushed onto PR #%d's branch there", fork, pr.Number)
		if m := l.meta(ref); m.Carry != pr.Number || m.Supersedes != 0 {
			t.Fatalf("want the task carrying PR #%d on the fork's branch, got %+v", pr.Number, m)
		}
		l.carryRun(ref)
		got, err := l.c.GetPR(l.ctx, l.repo, pr.Number)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Fork || got.HeadRepo != fork || got.HeadBranch != pr.HeadBranch || got.HeadSHA == pr.HeadSHA {
			t.Fatalf("want the fix pushed onto %s in %s past %s, got %s in %s at %s",
				pr.HeadBranch, fork, pr.HeadSHA, got.HeadBranch, got.HeadRepo, got.HeadSHA)
		}
		l.noOtherPR(t, ref, pr.Number)
		if !l.botSaid(forge.ItemRef{Repo: l.repo, Number: pr.Number}, fmt.Sprintf("to this PR for #%d", ref.Number)) {
			t.Fatalf("want devbot's comment on PR #%d saying what it pushed", pr.Number)
		}
		l.carriedApproved(ref, got)
	})

	t.Run("no-maintainer-edits", func(t *testing.T) {
		l := newLive(t)
		orig, sha := l.forkPR(t, fork, owner, false)
		impl, _ := l.c.BotUsers()
		ref := l.adopt(t, orig, impl, cred{})
		if m := l.meta(ref); m.Supersedes != orig.Number || m.Carry != 0 {
			t.Fatalf("want the task superseding PR #%d, got %+v", orig.Number, m)
		}
		l.supersededToMerge(t, ref, orig, sha)
	})
}

// supersededToMerge runs the implementer on a task carrying orig, a fork's
// PR, on devbot/pr-<N>, and checks devbot's own PR from there: crediting
// orig, with its contributor's commit sha, and linked from it. Then it has
// that PR approved and merged, whatever the test repo's automerge says, and
// orig closed as superseded.
func (l *live) supersededToMerge(t *testing.T, ref forge.ItemRef, orig forge.PR, sha string) {
	t.Helper()
	ctx := l.ctx
	impl, rev := l.c.BotUsers()
	l.carryRun(ref)
	pr, err := l.e.FindPR(ctx, l.item(ref))
	if err != nil || pr == nil {
		t.Fatalf("no PR of devbot's own for %s: %v", ref, err)
	}
	l.cleanupPR(l.repo, *pr)
	t.Logf("opened %s", pr.URL)
	if pr.Number == orig.Number || pr.Fork || pr.HeadBranch != core.SupersedeBranch(orig.Number) {
		t.Fatalf("want a PR of devbot's own from %s, got #%d from %s (fork %v)", core.SupersedeBranch(orig.Number), pr.Number, pr.HeadBranch, pr.Fork)
	}
	if !strings.Contains(pr.Body, fmt.Sprintf("Supersedes #%d", orig.Number)) || !strings.Contains(pr.Body, "@"+orig.Author) {
		t.Fatalf("want PR #%d crediting #%d by @%s: %q", pr.Number, orig.Number, orig.Author, pr.Body)
	}
	if !l.inHistory(t, l.repo, sha, pr.HeadSHA) {
		t.Fatalf("the contributor's commit %s should be under devbot's on %s", sha, pr.HeadBranch)
	}
	origRef := forge.ItemRef{Repo: l.repo, Number: orig.Number}
	if !l.botSaid(origRef, fmt.Sprintf("devbot opened #%d", pr.Number)) {
		t.Fatalf("want devbot's link to #%d on PR #%d", pr.Number, orig.Number)
	}

	// Merged whatever the test repo's automerge says: the original is
	// only closed once devbot's PR merges.
	if err := l.c.SetLabels(ctx, ref, []string{core.LAutomerge}, nil); err != nil {
		t.Fatal(err)
	}
	l.approve(ref, *pr)
	l.reviewedAt(pr.Number, pr.HeadSHA)
	l.until("the merge", 5*time.Minute, ref, func() bool {
		p, err := l.c.GetPR(ctx, l.repo, pr.Number)
		return err == nil && p.Merged
	})
	l.until("the original closed as superseded", 2*time.Minute, ref, func() bool {
		p, err := l.c.GetPR(ctx, l.repo, orig.Number)
		return err == nil && p.State == "closed"
	})
	if p, err := l.c.GetPR(ctx, l.repo, orig.Number); err != nil || p.Merged {
		t.Fatalf("PR #%d should be closed unmerged: %+v %v", orig.Number, p, err)
	}
	cs, err := l.c.ListComments(ctx, origRef)
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := core.LastStatus(core.Statuses(cs, impl, rev), "superseded"); !ok || s.PR != pr.Number || !strings.Contains(s.Body, "@"+orig.Author) {
		t.Fatalf("want PR #%d closed as superseded by #%d, thanking @%s: %+v", orig.Number, pr.Number, orig.Author, s)
	}
	if it := l.item(ref); it.State != "closed" || core.State(it) != "" {
		t.Fatalf("after the merge: want the issue closed with no state label, got %s %q", it.State, core.State(it))
	}
}

// forkPR has the fork's owner push a branch to the fork, at the test repo's
// default branch, with a file under live/ and a FAIL that fails CI, and open
// a PR from it onto the test repo, letting maintainers edit it or not. It
// returns the PR and the contributor's head commit. The branch is deleted and
// the PR closed when the test ends.
func (l *live) forkPR(t *testing.T, fork forge.RepoRef, owner cred, edits bool) (forge.PR, string) {
	t.Helper()
	ctx := l.ctx
	def, err := l.c.DefaultBranch(ctx, l.repo)
	if err != nil {
		t.Fatal(err)
	}
	cr, err := l.c.repoCred(ctx, forge.RoleImplementer, l.repo)
	if err != nil {
		t.Fatal(err)
	}
	s := stamp()
	// A fork shares its parent's objects, so its branch can start at the
	// test repo's head whatever the fork's own default branch is at.
	branch := fmt.Sprintf("devbot-live/fork-%v-%s", edits, s)
	l.branchAs(t, owner, fork, l.head(t, cr, l.repo, def), branch)
	t.Cleanup(func() {
		_, err := l.c.do(context.Background(), owner, "DELETE", repoPath(fork)+"/git/refs/heads/"+escapePath(branch), nil, nil)
		var ae *apiError
		if err != nil && !(errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity && strings.Contains(ae.Body, "Reference does not exist")) && !errors.Is(err, forge.ErrNotFound) {
			t.Errorf("cleanup: deleting %s on %s: %v (remove it by hand)", branch, fork, err)
		}
	})
	l.commitAs(t, owner, fork, branch, "live/"+s+"-fork.txt", "Live test: a contribution from a fork")
	sha := l.commitAs(t, owner, fork, branch, "FAIL", "Live test: fail CI")

	var p pull
	if _, err := l.c.do(ctx, owner, "POST", repoPath(l.repo)+"/pulls", map[string]any{
		"title": fmt.Sprintf("Live test: carry a fork's PR (maintainer edits %v)", edits), "head": fork.Owner + ":" + branch, "base": def,
		"body": "devbot's live test. Safe to close.", "maintainer_can_modify": edits,
	}, &p); err != nil {
		t.Fatal(err)
	}
	pr := l.c.pr(l.repo, p)
	l.closeWhenDone(l.repo, pr.Number)
	t.Logf("opened %s", pr.URL)
	if got, err := l.c.GetPR(ctx, l.repo, pr.Number); err != nil || !got.Fork || got.HeadRepo != fork || got.MaintainerCanModify != edits || got.HeadSHA != sha {
		t.Fatalf("want PR #%d from %s at %s, maintainer edits %v (GitHub allows them only on a fork a user owns), got %+v %v",
			pr.Number, fork, sha, edits, got, err)
	}
	return pr, sha
}

// adopt has labeller (as as, or the implementer App when as is zero) add
// devbot/ready to pr, and devbot take it up, as the PR's webhook would have
// it, twice: one tracking issue, "Carry PR #N", linked from the PR, which
// has the label from the PR, now off it, with labeller as its requester and
// the implementer queued. The issue is closed when the test ends.
func (l *live) adopt(t *testing.T, pr forge.PR, labeller string, as cred) forge.ItemRef {
	t.Helper()
	ctx := l.ctx
	prRef := forge.ItemRef{Repo: l.repo, Number: pr.Number}
	var err error
	if as == (cred{}) {
		err = l.c.SetLabels(ctx, prRef, []string{core.LReady}, nil)
	} else {
		err = l.c.setLabels(ctx, as, labeller, prRef, []string{core.LReady}, nil)
	}
	if err != nil {
		t.Fatalf("adding %s to PR #%d as %s: %v", core.LReady, pr.Number, labeller, err)
	}
	// devbot takes the requester from the label's event, and the PR's
	// author when there is none yet.
	var evs []forge.LabelEvent
	if !l.listed(core.LReady+" added by "+labeller, func() bool {
		evs, err = l.c.LabelEvents(ctx, prRef)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(evs, func(e forge.LabelEvent) bool { return e.Label == core.LReady && e.Added && e.Actor == labeller })
	}) {
		t.Fatalf("want %s added to PR #%d by %s, got %+v", core.LReady, pr.Number, labeller, evs)
	}
	impl, rev := l.c.BotUsers()
	var ref forge.ItemRef
	for range 2 { // and once more, with nothing to do
		if err := l.e.HandleEvent(ctx, forge.Event{Kind: forge.EventPR, Repo: l.repo, Number: pr.Number}); err != nil {
			t.Fatal(err)
		}
		cs, err := l.c.ListComments(ctx, prRef)
		if err != nil {
			t.Fatal(err)
		}
		var links []int
		for _, c := range cs {
			if forge.HasLogin([]string{impl, rev}, c.Author) {
				if cl, ok := core.ParseCarry(c.Body); ok {
					links = append(links, cl.Issue)
				}
			}
		}
		if len(links) != 1 {
			t.Fatalf("want one tracking issue linked from PR #%d, got %v", pr.Number, links)
		}
		if ref.Number == 0 {
			ref = forge.ItemRef{Repo: l.repo, Number: links[0]}
			t.Cleanup(func() {
				if it, err := l.c.GetItem(context.Background(), ref); err == nil && it.State == "open" {
					l.c.CloseItem(context.Background(), ref)
				}
			})
		}
	}
	it := l.item(ref)
	t.Logf("tracking issue %s", it.URL)
	if want := fmt.Sprintf("Carry PR #%d: %s", pr.Number, pr.Title); it.Title != want || it.State != "open" {
		t.Fatalf("want an open issue %q, got %s %q", want, it.State, it.Title)
	}
	if got, err := l.c.GetPR(ctx, l.repo, pr.Number); err != nil || got.HasLabel(core.LReady) {
		t.Fatalf("%s should be off PR #%d: %v %v", core.LReady, pr.Number, got.Labels, err)
	}
	if m := l.meta(ref); m.Requester != labeller {
		t.Fatalf("want @%s as the requester, got %+v", labeller, m)
	}
	if !l.queued(queue.KindImplement, ref) {
		t.Fatalf("%s isn't queued for the implementer (state %q)", ref, core.State(it))
	}
	return ref
}

// carryRun runs the queued implement job on a carried task with the stub
// fixing CI.
func (l *live) carryRun(ref forge.ItemRef) {
	l.t.Helper()
	l.mode("carry")
	if err := l.job(forge.RoleImplementer, l.r.Implement); err != nil {
		l.t.Fatal(err)
	}
	if s := core.State(l.item(ref)); s != core.LInReview {
		l.t.Fatalf("after the implementer: want %s, got %q", core.LInReview, s)
	}
}

// carriedApproved has the reviewer App approve pr's head, then waits for the
// gate: a merge, or devbot/approved when the test repo leaves automerge off.
func (l *live) carriedApproved(ref forge.ItemRef, pr forge.PR) {
	l.t.Helper()
	l.approve(ref, pr)
	l.reviewedAt(pr.Number, pr.HeadSHA)
	merged := false
	l.until("the merge gate", 5*time.Minute, ref, func() bool {
		p, err := l.c.GetPR(l.ctx, l.repo, pr.Number)
		merged = err == nil && p.Merged
		return merged || core.State(l.item(ref)) == core.LApproved
	})
	l.t.Logf("PR #%d: merged %v", pr.Number, merged)
}

// reviewedAt fails the test unless the reviewer App approved PR number at
// head.
func (l *live) reviewedAt(number int, head string) {
	l.t.Helper()
	reviews, err := l.c.Reviews(l.ctx, l.repo, number)
	if err != nil {
		l.t.Fatal(err)
	}
	_, reviewer := l.c.BotUsers()
	if !slices.ContainsFunc(reviews, func(r forge.Review) bool {
		return r.Author == reviewer && r.Verdict == forge.VerdictApprove && r.CommitSHA == head
	}) {
		l.t.Fatalf("want the reviewer App's approval of %s on PR #%d: %+v", head, number, reviews)
	}
}

// noOtherPR fails the test if the task has a PR other than carried, or any
// open PR of devbot's names it.
func (l *live) noOtherPR(t *testing.T, ref forge.ItemRef, carried int) {
	t.Helper()
	if pr, err := l.e.FindPR(l.ctx, l.item(ref)); err != nil || pr == nil || pr.Number != carried {
		t.Fatalf("want PR #%d as the task's, got %+v %v", carried, pr, err)
	}
	prs, err := l.c.ListOpenPRs(l.ctx, l.repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range prs {
		if m, ok := core.ParsePRMeta(p.Body); (ok && m.Issue == ref.Number) || strings.HasPrefix(p.HeadBranch, fmt.Sprintf("devbot/%d-", ref.Number)) {
			t.Fatalf("devbot opened PR #%d (%s) for %s instead of pushing to PR #%d", p.Number, p.HeadBranch, ref, carried)
		}
	}
}

// meta is the task's meta, from its comments.
func (l *live) meta(ref forge.ItemRef) core.Meta {
	l.t.Helper()
	cs, err := l.c.ListComments(l.ctx, ref)
	if err != nil {
		l.t.Fatal(err)
	}
	return l.e.Meta(l.ctx, l.item(ref), cs)
}

// botSaid reports whether a bot commented on ref with text in it.
func (l *live) botSaid(ref forge.ItemRef, text string) bool {
	l.t.Helper()
	cs, err := l.c.ListComments(l.ctx, ref)
	if err != nil {
		l.t.Fatal(err)
	}
	impl, rev := l.c.BotUsers()
	return slices.ContainsFunc(cs, func(c forge.Comment) bool {
		return forge.HasLogin([]string{impl, rev}, c.Author) && strings.Contains(c.Body, text)
	})
}

// inHistory reports whether sha is head or one of its ancestors on repo.
func (l *live) inHistory(t *testing.T, repo forge.RepoRef, sha, head string) bool {
	t.Helper()
	cr, err := l.c.repoCred(l.ctx, forge.RoleImplementer, repo)
	if err != nil {
		t.Fatal(err)
	}
	var cmp struct {
		Status string `json:"status"`
	}
	if _, err := l.c.do(l.ctx, cr, "GET", repoPath(repo)+"/compare/"+sha+"..."+head, nil, &cmp); err != nil {
		t.Fatal(err)
	}
	return cmp.Status == "ahead" || cmp.Status == "identical"
}

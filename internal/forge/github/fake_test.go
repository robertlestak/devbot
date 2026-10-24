package github

import (
	"cmp"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// fakeGitHub is an httptest fake of the parts of GitHub's REST API the
// adapter uses. It checks what GitHub checks: Apps sign in with a valid
// RS256 JWT, installation tokens expire, and a token only reaches the
// repos its installation covers.
type fakeGitHub struct {
	t      *testing.T
	srv    *httptest.Server
	prefix string // "/api/v3" for GHES

	mu    sync.Mutex
	now   func() time.Time
	calls int
	// searches counts the requests to the search API.
	searches int
	apps     map[int64]*fakeApp
	installs map[int64]*fakeInstall
	tokens   map[string]fakeToken
	mints    map[int64]int // installation ID -> tokens minted
	// mintBodies are the token requests' bodies, by installation.
	mintBodies map[int64][]map[string]any
	mintDelay  time.Duration
	tokenTTL   time.Duration
	repos      map[string]*fakeRepo
	// hook, when set, may answer a request itself (true) before the fake does.
	hook func(w http.ResponseWriter, r *http.Request) bool
	// onLabelsRead, when set, runs (locked) just after an issue's labels
	// are served, as someone editing them at that moment would.
	onLabelsRead func(is *fakeIssue)
	// teams are org teams' members, which a repo grants a role to.
	teams map[string][]string
	// lastEvent is the last issue event's ID, which GitHub hands out in order.
	lastEvent int64
	// editsPage is how many userContentEdits a GraphQL page holds; 0 is
	// the 100 devbot asks for.
	editsPage int
	// People's user access tokens through a sign-in App.
	oauth fakeOAuth
}

type fakeApp struct {
	slug string
	key  *rsa.PrivateKey
}

type fakeInstall struct {
	app   int64
	repos []string
	perms map[string]string
	// user is an installation on a user's account, not an org's.
	user bool
	// suspended is listed with suspended_at and can't mint a token.
	suspended bool
}

type fakeToken struct {
	install int64
	expires time.Time
	// repos and perms are what the mint asked for: nil is all the
	// installation has.
	repos []string
	perms map[string]string
}

type fakeRepo struct {
	defaultBranch string
	next          int
	issues        map[int]*fakeIssue // PRs too, as on GitHub
	noIssues      bool
	// public repos turn up in an account's searches whether or not the
	// App is installed on them.
	public    bool
	branches  map[string]string           // name -> sha
	files     map[string][]byte           // ref + ":" + path
	statuses  map[string][]fakeStatus     // sha -> the latest status per context
	runs      map[string][]fakeRun        // sha -> every check run, re-runs too
	outputs   map[int64]forge.CheckReport // check run -> its output and annotations
	logs      map[int64]string            // Actions job -> its log
	reruns    []string                    // workflow run IDs re-run, in order
	nextID    int64
	labels    map[string]fakeLabel
	collabs   map[string]string // login -> role_name
	teamRoles map[string]string // team -> role_name
	// The git objects behind branches that are made with the git data or
	// contents API (the attachments branch). Other branches point at
	// made-up SHAs, and their files are in files.
	objects   map[string]*fakeObject
	protected map[string]bool // branches a ruleset refuses writes to
	treeMax   int             // entries in a tree read past which it is truncated
	// commitAt is the commit date of a made-up SHA, as the git data API
	// reports it.
	commitAt map[string]time.Time
	// rules are the active ruleset rules on a branch, as "Get rules for a
	// branch" lists them; classic is a branch's classic protection, as
	// "Get branch protection" returns it.
	rules   map[string][]map[string]any
	classic map[string]map[string]any
	// freePlan is a private repo on GitHub Free, which has neither: their
	// endpoints answer 403, asking for an upgrade.
	freePlan bool
	// rulesForbidden refuses the rules read for any other reason.
	rulesForbidden bool
	// tags are the repo's, name and commit, newest first, and history each
	// commit's parents, for compare.
	tags    [][2]string
	history map[string][]string
}

// fakeStatus is a commit status, as the combined status lists it.
type fakeStatus struct {
	Context     string `json:"context"`
	State       string `json:"state"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
}

// fakeRun is a check run, with a null conclusion until it completes.
type fakeRun struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	Conclusion *string `json:"conclusion"`
	HTMLURL    string  `json:"html_url"`
	DetailsURL string  `json:"details_url,omitempty"`
}

type fakeReview struct {
	ID          int64     `json:"id"`
	User        fakeUser  `json:"user"`
	State       string    `json:"state"`
	CommitID    string    `json:"commit_id"`
	Body        string    `json:"body"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// fakeLine is a review comment on a line, as GitHub lists them.
type fakeLine struct {
	ID       int64     `json:"id"`
	ReviewID int64     `json:"pull_request_review_id"`
	User     fakeUser  `json:"user"`
	Body     string    `json:"body"`
	Path     string    `json:"path"`
	Line     *int      `json:"line"`
	OrigLine int       `json:"original_line"`
	CommitID string    `json:"commit_id"`
	Created  time.Time `json:"created_at"`
}

type fakeUser struct {
	Login string `json:"login"`
}

type fakeIssue struct {
	number              int
	title, body, author string
	state               string
	labels              []string
	created, updated    time.Time
	comments            []fakeComment
	events              []fakeEvent
	// edits are the body's edits, oldest first, as its userContentEdits
	// list them (newest first, after the body as filed).
	edits []fakeEdit
	// original is when userContentEdits says the body as filed was written:
	// created when zero, which nothing promises real GitHub matches.
	original time.Time

	// A PR is an issue with isPR set, as on GitHub.
	isPR               bool
	headRef, headSHA   string
	baseRef, mergeBase string
	conflicted, merged bool
	mergeSHA           string
	// fork puts the head in a fork, mallory's.
	fork bool
	// maintainerEdit is the fork PR's "Allow edits by maintainers".
	maintainerEdit bool
	// mergeState is the mergeable_state GitHub reports, "clean" when unset.
	// unknownFor is how many more reads report mergeable as null, as while
	// GitHub computes it.
	// A merge is refused with a 405 while unknownFor lasts, and while
	// baseModified, as when another merge lands on the base first.
	mergeState   string
	unknownFor   int
	baseModified bool
	mergedBy     string
	mergeMethod  string
	reviews      []fakeReview
	lines        []fakeLine
}

type fakeComment struct {
	id      int64
	author  string
	body    string
	created time.Time
	updated time.Time // zero: never edited
}

type fakeEdit struct {
	editor string
	at     time.Time
}

type fakeLabel struct{ color, description string }

type fakeEvent struct {
	id           int64
	label, actor string
	added        bool
	at           time.Time
	// A renamed event, from one title to another, instead of a label.
	rename *[2]string
}

var (
	testKeysOnce sync.Once
	testKeys     [3]*rsa.PrivateKey
)

// testKey is one of three RSA keys, generated once per test binary.
func testKey(i int) *rsa.PrivateKey {
	testKeysOnce.Do(func() {
		for j := range testKeys {
			k, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			testKeys[j] = k
		}
	})
	return testKeys[i]
}

func pemKey(k *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

const (
	implAppID = 101
	revAppID  = 202
)

// newFakeGitHub serves two Apps, devbot (101) and devbot-review (202),
// each installed on acme/app with contents and pull requests write.
func newFakeGitHub(t *testing.T, prefix string) *fakeGitHub {
	f := &fakeGitHub{
		t: t, prefix: prefix, now: time.Now, tokenTTL: time.Hour,
		apps: map[int64]*fakeApp{
			implAppID: {slug: "devbot", key: testKey(0)},
			revAppID:  {slug: "devbot-review", key: testKey(1)},
		},
		installs: map[int64]*fakeInstall{
			11: {app: implAppID, repos: []string{"acme/app"}, perms: map[string]string{"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read"}},
			22: {app: revAppID, repos: []string{"acme/app"}, perms: map[string]string{"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read"}},
		},
		tokens: map[string]fakeToken{}, mints: map[int64]int{}, mintBodies: map[int64][]map[string]any{},
		repos: map[string]*fakeRepo{"acme/app": newFakeRepo(), "acme/private": newFakeRepo()},
		oauth: newFakeOAuth(),
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{defaultBranch: "main", issues: map[int]*fakeIssue{}, branches: map[string]string{"main": "ba5e"}, files: map[string][]byte{},
		statuses: map[string][]fakeStatus{}, runs: map[string][]fakeRun{}, outputs: map[int64]forge.CheckReport{}, logs: map[int64]string{},
		labels: map[string]fakeLabel{}, collabs: map[string]string{}, teamRoles: map[string]string{},
		objects: map[string]*fakeObject{}, protected: map[string]bool{}, commitAt: map[string]time.Time{}, history: map[string][]string{},
		rules: map[string][]map[string]any{}, classic: map[string]map[string]any{}}
}

// stamp is the time a comment, review or label event gets: to the second,
// as GitHub keeps them.
func (f *fakeGitHub) stamp() time.Time { return f.now().UTC().Truncate(time.Second) }

func (f *fakeGitHub) URL() string { return f.srv.URL + f.prefix }

func (f *fakeGitHub) config() Config {
	return Config{
		Name: "github", URL: f.URL(),
		ImplementerAppID: implAppID, ImplementerKey: pemKey(testKey(0)),
		ReviewerAppID: revAppID, ReviewerKey: pemKey(testKey(1)),
		OAuthClientID: implClientID, OAuthClientSecret: implClientSecret,
	}
}

func (f *fakeGitHub) client() *Client {
	c, err := New(context.Background(), f.config())
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fakeGitHub) mintCount(id int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mints[id]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, 404, map[string]string{"message": "Not Found", "documentation_url": "https://docs.github.com/rest"})
}

// verifyJWT returns the App a JWT is signed by, as GitHub checks it: RS256,
// issued in the past, expiring in the future and at most ten minutes after
// it was issued.
func (f *fakeGitHub) verifyJWT(tok string) (int64, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return 0, false
	}
	var hdr struct{ Alg, Typ string }
	var claims struct {
		Iat, Exp, Iss int64
	}
	h, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	c, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || json.Unmarshal(h, &hdr) != nil || json.Unmarshal(c, &claims) != nil || hdr.Alg != "RS256" {
		return 0, false
	}
	a, ok := f.apps[claims.Iss]
	if !ok {
		return 0, false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&a.key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		return 0, false
	}
	now := f.now().Unix()
	if claims.Iat > now || claims.Exp <= now || claims.Exp-claims.Iat > 600+60 {
		return 0, false
	}
	return claims.Iss, true
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	hook := f.hook
	f.mu.Unlock()
	if rest, ok := strings.CutPrefix(r.URL.Path, "/_logs/"); ok {
		if r.Header.Get("Authorization") != "" {
			f.t.Errorf("the log's storage got the credential")
		}
		i := strings.LastIndex(rest, "/")
		n, _ := strconv.ParseInt(rest[i+1:], 10, 64)
		f.mu.Lock()
		defer f.mu.Unlock()
		io.WriteString(w, f.repos[rest[:i]].logs[n])
		return
	}
	if strings.HasPrefix(r.URL.Path, "/login/oauth/") {
		f.serveOAuth(w, r)
		return
	}
	if r.Header.Get("X-GitHub-Api-Version") != apiVersion || !strings.HasPrefix(r.Header.Get("Accept"), "application/vnd.github") || r.Header.Get("User-Agent") == "" {
		f.t.Errorf("%s %s: headers %v", r.Method, r.URL, r.Header)
	}
	if hook != nil && hook(w, r) {
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, f.prefix)
	if r.URL.Path == strings.TrimSuffix(f.prefix, "/v3")+"/graphql" {
		path, ok = "/graphql", true
	}
	if !ok {
		notFound(w)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		writeJSON(w, 401, map[string]string{"message": "Requires authentication"})
		return
	}

	// As an App, with a JWT.
	if strings.HasPrefix(path, "/app") || strings.HasSuffix(path, "/installation") {
		id, ok := f.verifyJWT(tok)
		if !ok {
			writeJSON(w, 401, map[string]string{"message": "A JSON web token could not be decoded"})
			return
		}
		f.serveApp(w, r, id, path)
		return
	}

	// As a person, with a user access token.
	if f.serveUser(w, r, tok, path) {
		return
	}

	// As an installation, with its token.
	t, ok := f.tokens[tok]
	if !ok || !f.now().Before(t.expires) {
		writeJSON(w, 401, map[string]string{"message": "Bad credentials"})
		return
	}
	if r.Method == "POST" && path == "/graphql" {
		f.serveGraphQL(w, r, f.installs[t.install], t)
		return
	}
	if r.Method == "GET" && path == "/installation/repositories" {
		var out []map[string]any
		for _, full := range f.installs[t.install].repos {
			if !t.reaches(full) {
				continue
			}
			out = append(out, map[string]any{"full_name": full, "has_issues": !f.repos[full].noIssues})
		}
		pageIn(f, w, r, "repositories", out)
		return
	}
	if r.Method == "GET" && path == "/search/issues" {
		f.serveSearch(w, r, t)
		return
	}
	rest, ok := strings.CutPrefix(path, "/repos/")
	if !ok {
		notFound(w)
		return
	}
	segs := strings.SplitN(rest, "/", 3)
	if len(segs) < 2 {
		notFound(w)
		return
	}
	full := segs[0] + "/" + segs[1]
	repo := f.repos[full]
	if repo == nil || !slices.Contains(f.installs[t.install].repos, full) || !t.reaches(full) {
		notFound(w)
		return
	}
	sub := ""
	if len(segs) == 3 {
		sub = segs[2]
	}
	in := f.installs[t.install]
	f.serveRepo(w, r, full, repo, sub, f.apps[in.app].slug+"[bot]", t.effective(in.perms))
}

func (f *fakeGitHub) serveApp(w http.ResponseWriter, r *http.Request, app int64, path string) {
	switch {
	case r.Method == "GET" && path == "/app":
		writeJSON(w, 200, map[string]any{"id": app, "slug": f.apps[app].slug})
	case r.Method == "GET" && path == "/app/installations":
		var out []map[string]any
		for _, id := range sortedIDs(f.installs) {
			if in := f.installs[id]; in.app == app {
				var suspended any
				if in.suspended {
					suspended = "2026-10-01T00:00:00Z"
				}
				owner, _, _ := strings.Cut(in.repos[0], "/")
				kind := "Organization"
				if in.user {
					kind = "User"
				}
				out = append(out, map[string]any{"id": id, "app_id": app, "account": map[string]any{"login": owner, "type": kind}, "suspended_at": suspended, "permissions": in.perms})
			}
		}
		page(f, w, r, out)
	case r.Method == "GET" && strings.HasPrefix(path, "/repos/") && strings.HasSuffix(path, "/installation"):
		full := strings.TrimSuffix(strings.TrimPrefix(path, "/repos/"), "/installation")
		for _, id := range sortedIDs(f.installs) {
			in := f.installs[id]
			if in.app == app && slices.Contains(in.repos, full) {
				writeJSON(w, 200, map[string]any{"id": id, "app_id": app, "permissions": in.perms})
				return
			}
		}
		notFound(w)
	case r.Method == "POST" && strings.HasPrefix(path, "/app/installations/") && strings.HasSuffix(path, "/access_tokens"):
		id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(path, "/app/installations/"), "/access_tokens"), 10, 64)
		in, ok := f.installs[id]
		if !ok || in.app != app {
			notFound(w)
			return
		}
		if in.suspended {
			writeJSON(w, 403, map[string]string{"message": "This installation has been suspended", "documentation_url": "https://docs.github.com/rest"})
			return
		}
		var req struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		var raw map[string]any
		b, _ := io.ReadAll(r.Body)
		if len(b) > 0 && (json.Unmarshal(b, &req) != nil || json.Unmarshal(b, &raw) != nil) {
			writeJSON(w, 400, map[string]string{"message": "Problems parsing JSON"})
			return
		}
		f.mintBodies[id] = append(f.mintBodies[id], raw)
		// GitHub refuses a token for more than the installation has.
		for _, full := range req.Repositories {
			if !slices.ContainsFunc(in.repos, func(s string) bool { return strings.HasSuffix(s, "/"+full) }) {
				writeJSON(w, 422, map[string]string{"message": "There is at least one repository that does not exist or is not accessible to the parent installation."})
				return
			}
		}
		for p, lvl := range req.Permissions {
			if fakeLevel[in.perms[p]] < fakeLevel[lvl] {
				writeJSON(w, 422, map[string]string{"message": "The permissions requested are not granted to this installation."})
				return
			}
		}
		f.mints[id]++
		if f.mintDelay > 0 {
			f.mu.Unlock()
			time.Sleep(f.mintDelay)
			f.mu.Lock()
		}
		tok := fmt.Sprintf("ghs_%d_%d", id, f.mints[id])
		exp := f.now().Add(f.tokenTTL).UTC().Truncate(time.Second)
		var repos []string
		for _, full := range in.repos {
			if req.Repositories == nil {
				break
			}
			if !slices.ContainsFunc(req.Repositories, func(n string) bool { return strings.HasSuffix(full, "/"+n) }) {
				continue
			}
			repos = append(repos, full)
		}
		f.tokens[tok] = fakeToken{install: id, expires: exp, repos: repos, perms: req.Permissions}
		// GitHub answers with what the token got: what it asked for, or all
		// the installation has when it asked for nothing in particular.
		perms := req.Permissions
		if perms == nil {
			perms = in.perms
		}
		writeJSON(w, 201, map[string]any{"token": tok, "expires_at": exp, "permissions": perms})
	default:
		notFound(w)
	}
}

func sortedIDs[V any](m map[int64]V) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// page serves one page of all as GitHub does, with a Link to the next.
func page[T any](f *fakeGitHub, w http.ResponseWriter, r *http.Request, all []T) {
	writeJSON(w, 200, pageOf(w, r, all))
}

// pageIn is page for an endpoint that wraps each page in an object, under
// key.
func pageIn[T any](f *fakeGitHub, w http.ResponseWriter, r *http.Request, key string, all []T) {
	writeJSON(w, 200, map[string]any{"total_count": len(all), key: pageOf(w, r, all)})
}

// pageOf is the page of all that r asks for, setting the Link to the next.
func pageOf[T any](w http.ResponseWriter, r *http.Request, all []T) []T {
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if per <= 0 {
		per = 30
	}
	per = min(per, 100)
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	n = max(n, 1)
	lo, hi := min((n-1)*per, len(all)), min(n*per, len(all))
	if hi < len(all) {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(n+1))
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s>; rel="next", <http://%s%s?page=99>; rel="last"`, r.Host, r.URL.Path, q.Encode(), r.Host, r.URL.Path))
	}
	out := all[lo:hi]
	if out == nil {
		out = []T{}
	}
	return out
}

// hasAll reports whether labels has every one of want, a comma-separated
// list as GitHub's labels filter takes it.
func hasAll(labels []string, want string) bool {
	for _, l := range strings.Split(want, ",") {
		if l != "" && !slices.Contains(labels, l) {
			return false
		}
	}
	return true
}

func (f *fakeGitHub) issueJSON(full string, is *fakeIssue) map[string]any {
	ls := []map[string]any{}
	for _, l := range is.labels {
		ls = append(ls, map[string]any{"name": l, "color": "ededed"})
	}
	m := map[string]any{
		"number": is.number, "title": is.title, "body": is.body, "state": is.state, "labels": ls,
		"user": map[string]any{"login": is.author}, "html_url": f.srv.URL + "/" + full + "/issues/" + strconv.Itoa(is.number),
		"created_at": is.created, "updated_at": is.updated,
	}
	if is.isPR {
		m["pull_request"] = map[string]any{"url": "x"}
	}
	return m
}

// The clone URLs it advertises are on a host of their own, as on a forge
// devbot reaches at another address than its public one: devbot never uses
// them.
func (f *fakeGitHub) pullJSON(full string, is *fakeIssue, single bool) map[string]any {
	m := map[string]any{
		"number": is.number, "title": is.title, "body": is.body, "state": is.state,
		"head":     map[string]any{"ref": is.headRef, "sha": is.headSHA, "repo": map[string]any{"full_name": full, "clone_url": "https://github.internal.example/" + full + ".git"}},
		"base":     map[string]any{"ref": is.baseRef, "sha": is.mergeBase},
		"html_url": f.srv.URL + "/" + full + "/pull/" + strconv.Itoa(is.number),
		"user":     map[string]any{"login": is.author}, "created_at": is.created, "merged_at": nil, "closed_at": nil,
		"labels": f.issueJSON(full, is)["labels"],
	}
	m["maintainer_can_modify"] = is.maintainerEdit
	if is.fork {
		m["head"].(map[string]any)["repo"] = map[string]any{"full_name": "mallory/" + is.headRef, "clone_url": "https://github.internal.example/mallory/" + is.headRef + ".git"}
	}
	if is.state == "closed" {
		m["closed_at"] = is.updated
	}
	if is.merged {
		m["merged_at"] = is.updated
		m["merge_commit_sha"] = is.mergeSHA
	}
	if single {
		m["merged"] = is.merged
		// Each read while GitHub is still computing mergeability counts down.
		switch {
		case is.unknownFor > 0:
			is.unknownFor--
			m["mergeable"], m["mergeable_state"] = nil, "unknown"
		case is.conflicted:
			m["mergeable"], m["mergeable_state"] = false, "dirty"
		default:
			m["mergeable"], m["mergeable_state"] = true, cmp.Or(is.mergeState, "clean")
		}
	}
	return m
}

// serveRepo answers a call on repo full made as actor, an App's bot login
// with the installation's permissions perms.
func (f *fakeGitHub) serveRepo(w http.ResponseWriter, r *http.Request, full string, repo *fakeRepo, sub, actor string, perms map[string]string) {
	segs := strings.Split(sub, "/")
	num := func(i int) *fakeIssue {
		if len(segs) <= i {
			return nil
		}
		n, _ := strconv.Atoi(segs[i])
		return repo.issues[n]
	}
	if f.serveItems(w, r, full, repo, segs, num(1), actor, perms) {
		return
	}
	switch {
	case r.Method == "GET" && sub == "":
		writeJSON(w, 200, map[string]any{"full_name": full, "default_branch": repo.defaultBranch})
	case r.Method == "GET" && sub == "issues":
		q := r.URL.Query()
		var out []map[string]any
		for _, n := range sortedKeys(repo.issues) {
			is := repo.issues[n]
			if (q.Get("state") == "all" || is.state == q.Get("state")) && hasAll(is.labels, q.Get("labels")) {
				out = append(out, f.issueJSON(full, is))
			}
		}
		page(f, w, r, out)
	case r.Method == "GET" && segs[0] == "issues" && len(segs) == 2:
		is := num(1)
		if is == nil {
			notFound(w)
			return
		}
		writeJSON(w, 200, f.issueJSON(full, is))
	case r.Method == "GET" && segs[0] == "issues" && len(segs) == 3 && segs[2] == "comments":
		is := num(1)
		if is == nil {
			notFound(w)
			return
		}
		var out []map[string]any
		for _, c := range is.comments {
			updated := c.updated
			if updated.IsZero() {
				updated = c.created
			}
			out = append(out, map[string]any{"id": c.id, "body": c.body, "user": map[string]any{"login": c.author}, "created_at": c.created, "updated_at": updated})
		}
		page(f, w, r, out)
	case r.Method == "GET" && sub == "pulls":
		var out []map[string]any
		for _, n := range sortedKeys(repo.issues) {
			is := repo.issues[n]
			if is.isPR && (r.URL.Query().Get("state") == "all" || is.state == r.URL.Query().Get("state")) {
				out = append(out, f.pullJSON(full, is, false))
			}
		}
		page(f, w, r, out)
	case r.Method == "GET" && segs[0] == "pulls" && len(segs) == 2:
		is := num(1)
		if is == nil || !is.isPR {
			notFound(w)
			return
		}
		writeJSON(w, 200, f.pullJSON(full, is, true))
	case r.Method == "POST" && sub == "pulls":
		var in struct{ Title, Head, Base, Body string }
		json.NewDecoder(r.Body).Decode(&in)
		head, okHead := repo.branches[in.Head]
		base, okBase := repo.branches[in.Base]
		if !okHead || !okBase || in.Title == "" {
			writeJSON(w, 422, map[string]any{"message": "Validation Failed"})
			return
		}
		repo.next++
		now := f.stamp()
		// The answer to the create has mergeability not computed yet.
		is := &fakeIssue{number: repo.next, title: in.Title, body: in.Body, author: actor, state: "open", created: now, updated: now,
			isPR: true, headRef: in.Head, headSHA: head, baseRef: in.Base, mergeBase: base, unknownFor: 1}
		repo.issues[is.number] = is
		writeJSON(w, 201, f.pullJSON(full, is, true))
	case r.Method == "PATCH" && segs[0] == "pulls" && len(segs) == 2:
		is := num(1)
		if is == nil || !is.isPR {
			notFound(w)
			return
		}
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if t, ok := in["title"]; ok {
			is.title = t
		}
		if b, ok := in["body"]; ok {
			is.body = b
		}
		if b, ok := in["base"]; ok {
			sha, known := repo.branches[b]
			if !known {
				writeJSON(w, 422, map[string]any{"message": "Validation Failed"})
				return
			}
			is.baseRef, is.mergeBase = b, sha
		}
		is.updated = f.stamp()
		writeJSON(w, 200, f.pullJSON(full, is, true))
	case r.Method == "GET" && segs[0] == "pulls" && len(segs) == 3 && segs[2] == "comments":
		is := num(1)
		if is == nil || !is.isPR {
			notFound(w)
			return
		}
		page(f, w, r, is.lines)
	case segs[0] == "pulls" && len(segs) == 3 && segs[2] == "reviews":
		is := num(1)
		if is == nil || !is.isPR {
			notFound(w)
			return
		}
		if r.Method == "GET" {
			page(f, w, r, is.reviews)
			return
		}
		var in struct {
			CommitID string `json:"commit_id"`
			Body     string `json:"body"`
			Event    string `json:"event"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		state := map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED", "COMMENT": "COMMENTED"}[in.Event]
		// GitHub won't let an author approve or reject their own PR.
		if r.Method != "POST" || state == "" || (actor == is.author && state != "COMMENTED") {
			writeJSON(w, 422, map[string]any{"message": "Unprocessable Entity"})
			return
		}
		repo.nextID++
		rv := fakeReview{ID: repo.nextID, User: fakeUser{actor}, State: state, CommitID: cmp.Or(in.CommitID, is.headSHA), Body: in.Body, SubmittedAt: f.stamp()}
		is.reviews = append(is.reviews, rv)
		writeJSON(w, 200, rv)
	case r.Method == "PUT" && segs[0] == "pulls" && len(segs) == 3 && segs[2] == "merge":
		is := num(1)
		if is == nil || !is.isPR {
			notFound(w)
			return
		}
		var in struct {
			SHA         string `json:"sha"`
			MergeMethod string `json:"merge_method"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		switch {
		case !slices.Contains([]string{"", "merge", "squash", "rebase"}, in.MergeMethod):
			writeJSON(w, 422, map[string]any{"message": "Validation Failed"})
		case is.mergeState == "blocked":
			writeJSON(w, 405, map[string]any{"message": "At least 1 approving review is required by reviewers with write access.",
				"documentation_url": "https://docs.github.com/articles/about-protected-branches"})
		case is.merged || is.state != "open" || is.conflicted:
			writeJSON(w, 405, map[string]any{"message": "Pull Request is not mergeable"})
		case in.SHA != "" && in.SHA != is.headSHA:
			writeJSON(w, 409, map[string]any{"message": "Head branch was modified. Review and try the merge again."})
		case is.baseModified:
			writeJSON(w, 405, map[string]any{"message": "Base branch was modified. Review and try the merge again."})
		case is.unknownFor > 0:
			writeJSON(w, 405, map[string]any{"message": "Pull Request is not mergeable"})
		default:
			is.merged, is.state, is.updated = true, "closed", f.stamp()
			is.mergedBy, is.mergeMethod = actor, cmp.Or(in.MergeMethod, "merge")
			is.mergeSHA, repo.history["m3rg3d"] = "m3rg3d", []string{repo.branches[is.baseRef], is.headSHA}
			repo.branches[is.baseRef] = "m3rg3d"
			writeJSON(w, 200, map[string]any{"sha": "m3rg3d", "merged": true, "message": "Pull Request successfully merged"})
		}
	case r.Method == "GET" && repo.freePlan && (strings.HasPrefix(sub, "rules/branches/") || (segs[0] == "branches" && len(segs) == 3 && segs[2] == "protection")):
		writeJSON(w, 403, map[string]any{"message": planMessage, "documentation_url": "https://docs.github.com/rest/repos/rules#get-rules-for-a-branch", "status": "403"})
	case r.Method == "GET" && repo.rulesForbidden && strings.HasPrefix(sub, "rules/branches/"):
		writeJSON(w, 403, map[string]any{"message": "Resource not accessible by integration"})
	case r.Method == "GET" && strings.HasPrefix(sub, "rules/branches/"):
		rules := repo.rules[strings.TrimPrefix(sub, "rules/branches/")]
		if rules == nil {
			rules = []map[string]any{}
		}
		page(f, w, r, rules)
	case r.Method == "GET" && segs[0] == "branches" && len(segs) == 3 && segs[2] == "protection":
		cl, ok := repo.classic[segs[1]]
		switch {
		case perms["administration"] == "":
			writeJSON(w, 403, map[string]any{"message": "Resource not accessible by integration"})
		case !ok:
			writeJSON(w, 404, map[string]any{"message": "Branch not protected"})
		default:
			writeJSON(w, 200, cl)
		}
	case r.Method == "GET" && segs[0] == "branches" && len(segs) >= 2 && !strings.HasSuffix(sub, "/protection"):
		name := strings.TrimPrefix(sub, "branches/")
		sha, ok := repo.branches[name]
		if !ok {
			notFound(w)
			return
		}
		// Only the required checks of classic protection come with a
		// branch; a ruleset alone marks it protected too.
		cl, classic := repo.classic[name]
		b := map[string]any{"name": name, "commit": map[string]any{"sha": sha}, "protected": classic || len(repo.rules[name]) > 0}
		if classic {
			rsc, ok := cl["required_status_checks"]
			if !ok {
				rsc = map[string]any{"enforcement_level": "off", "contexts": []string{}, "checks": []any{}}
			}
			b["protection"] = map[string]any{"enabled": true, "required_status_checks": rsc}
		}
		writeJSON(w, 200, b)
	case r.Method == "POST" && segs[0] == "statuses" && len(segs) == 2:
		var in struct {
			State       string `json:"state"`
			Context     string `json:"context"`
			Description string `json:"description"`
			TargetURL   string `json:"target_url"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		repo.statuses[segs[1]] = slices.DeleteFunc(repo.statuses[segs[1]], func(s fakeStatus) bool { return s.Context == in.Context })
		repo.statuses[segs[1]] = append(repo.statuses[segs[1]], fakeStatus{Context: in.Context, State: in.State, Description: in.Description, TargetURL: in.TargetURL})
		writeJSON(w, 201, in)
	case r.Method == "GET" && segs[0] == "commits" && len(segs) == 3 && segs[2] == "status":
		pageIn(f, w, r, "statuses", repo.statuses[segs[1]])
	case r.Method == "POST" && segs[0] == "actions" && len(segs) == 4 && segs[1] == "runs" && segs[3] == "rerun-failed-jobs":
		if perms["actions"] != "write" {
			writeJSON(w, 403, map[string]any{"message": "Resource not accessible by integration"})
			return
		}
		repo.reruns = append(repo.reruns, segs[2])
		w.WriteHeader(201)
	case r.Method == "GET" && segs[0] == "check-runs" && len(segs) == 2:
		id, _ := strconv.ParseInt(segs[1], 10, 64)
		out, ok := repo.outputs[id]
		if !ok {
			notFound(w)
			return
		}
		writeJSON(w, 200, map[string]any{"id": id, "output": map[string]any{"title": out.Title, "summary": out.Summary, "text": out.Text, "annotations_count": len(out.Annotations)}})
	case r.Method == "GET" && segs[0] == "check-runs" && len(segs) == 3 && segs[2] == "annotations":
		id, _ := strconv.ParseInt(segs[1], 10, 64)
		var out []map[string]any
		for _, a := range repo.outputs[id].Annotations {
			out = append(out, map[string]any{"path": a.Path, "start_line": a.StartLine, "end_line": a.EndLine, "annotation_level": a.Level,
				"title": a.Title, "message": a.Message, "blob_href": "https://github.example/acme/app/blob/x/" + a.Path})
		}
		page(f, w, r, out)
	case r.Method == "GET" && segs[0] == "actions" && len(segs) == 4 && segs[1] == "jobs" && segs[3] == "logs":
		if perms["actions"] == "" {
			writeJSON(w, 403, map[string]any{"message": "Resource not accessible by integration"})
			return
		}
		id, _ := strconv.ParseInt(segs[2], 10, 64)
		if _, ok := repo.logs[id]; !ok {
			notFound(w)
			return
		}
		// GitHub sends the log from storage on another host, by a signed URL.
		http.Redirect(w, r, strings.Replace(f.srv.URL, "127.0.0.1", "localhost", 1)+"/_logs/"+full+"/"+segs[2], http.StatusFound)
	case r.Method == "GET" && segs[0] == "commits" && len(segs) == 3 && segs[2] == "check-runs":
		// GitHub's filter=latest would drop superseded runs itself; the
		// fake lists them all, so the adapter is seen to drop them too.
		pageIn(f, w, r, "check_runs", repo.runs[segs[1]])
	case r.Method == "GET" && segs[0] == "compare":
		base, head, ok := strings.Cut(strings.TrimPrefix(sub, "compare/"), "...")
		var pr *fakeIssue
		for _, is := range repo.issues {
			if is.isPR && is.headSHA == head {
				pr = is
			}
		}
		if tip, known := repo.branches[base]; ok && pr != nil && known {
			behind := 0
			if tip != pr.mergeBase {
				behind = 1
			}
			writeJSON(w, 200, map[string]any{"status": "ahead", "ahead_by": 1, "behind_by": behind})
			return
		}
		// Any other two commits or tags: what each has that the other
		// doesn't, through history.
		b, h := repo.commit(base), repo.commit(head)
		if !ok || b == "" || h == "" {
			notFound(w)
			return
		}
		mb, mh := repo.ancestry(b), repo.ancestry(h)
		ahead, behind := 0, 0
		for c := range mh {
			if !mb[c] {
				ahead++
			}
		}
		for c := range mb {
			if !mh[c] {
				behind++
			}
		}
		status := map[[2]bool]string{{false, false}: "identical", {true, false}: "ahead", {false, true}: "behind", {true, true}: "diverged"}[[2]bool{ahead > 0, behind > 0}]
		writeJSON(w, 200, map[string]any{"status": status, "ahead_by": ahead, "behind_by": behind, "commits": []any{}})
	case r.Method == "GET" && sub == "tags":
		var out []map[string]any
		for _, t := range repo.tags {
			out = append(out, map[string]any{"name": t[0], "commit": map[string]string{"sha": t[1], "url": "https://api.github.example/c/" + t[1]}})
		}
		page(f, w, r, out)
	case segs[0] == "git" || (r.Method == "PUT" && segs[0] == "contents"):
		f.serveGit(w, r, repo, sub)
	case r.Method == "GET" && segs[0] == "contents":
		if r.Header.Get("Accept") != "application/vnd.github.raw+json" {
			f.t.Errorf("contents without the raw media type: %q", r.Header.Get("Accept"))
		}
		ref := r.URL.Query().Get("ref")
		if ref == "" {
			ref = repo.defaultBranch
		}
		p := strings.TrimPrefix(r.URL.Path[strings.Index(r.URL.Path, "/contents/"):], "/contents/")
		b, ok := repo.files[ref+":"+p]
		if c := repo.objects[repo.branches[ref]]; c != nil && c.kind == "commit" {
			b, ok = repo.blobAt(c.tree, p)
		}
		if !ok {
			notFound(w)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.github.raw+json")
		w.Write(b)
	default:
		notFound(w)
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

func labelsJSON(repo *fakeRepo, names []string) []map[string]any {
	out := []map[string]any{}
	for _, n := range names {
		l := repo.labels[n]
		out = append(out, map[string]any{"name": n, "color": l.color, "description": l.description})
	}
	return out
}

// setLabels gives the issue names as who, recording the events GitHub
// does. A label the repo doesn't have is created, as GitHub does.
func (f *fakeGitHub) setLabels(repo *fakeRepo, is *fakeIssue, who string, names []string) {
	now := f.stamp()
	var next []string
	for _, l := range is.labels {
		if slices.Contains(names, l) {
			next = append(next, l)
		} else {
			f.lastEvent++
			is.events = append(is.events, fakeEvent{id: f.lastEvent, label: l, actor: who, at: now})
		}
	}
	for _, n := range names {
		if slices.Contains(next, n) {
			continue
		}
		if _, ok := repo.labels[n]; !ok {
			repo.labels[n] = fakeLabel{color: "ededed"}
		}
		next = append(next, n)
		f.lastEvent++
		is.events = append(is.events, fakeEvent{id: f.lastEvent, label: n, actor: who, added: true, at: now})
	}
	is.labels = next
}

// edit changes the issue's title and body as who, recording a renamed
// event and a body edit as GitHub does.
func (f *fakeGitHub) edit(is *fakeIssue, who, title, body string) {
	now := f.now()
	if title != is.title {
		f.lastEvent++
		is.events = append(is.events, fakeEvent{id: f.lastEvent, actor: who, at: now, rename: &[2]string{is.title, title}})
	}
	if body != is.body {
		is.edits = append(is.edits, fakeEdit{editor: who, at: now})
	}
	is.title, is.body, is.updated = title, body, now
}

// role is login's role on repo, the highest of a direct grant and its
// teams', as GitHub reports it.
func (f *fakeGitHub) role(repo *fakeRepo, login string) string {
	rank := []string{"", "read", "triage", "write", "maintain", "admin"}
	best := repo.collabs[login]
	for team, role := range repo.teamRoles {
		if slices.Contains(f.teams[team], login) && slices.Index(rank, role) > slices.Index(rank, best) {
			best = role
		}
	}
	return best
}

// serveItems answers the issue, comment, label and collaborator writes
// and the reads they need. Writes to issues need issues: write, and a
// permission lookup metadata: read, as GitHub checks.
func (f *fakeGitHub) serveItems(w http.ResponseWriter, r *http.Request, full string, repo *fakeRepo, segs []string, is *fakeIssue, who string, perms map[string]string) bool {
	var in struct {
		Title       *string  `json:"title"`
		Body        *string  `json:"body"`
		State       *string  `json:"state"`
		Labels      []string `json:"labels"`
		Name        string   `json:"name"`
		Color       *string  `json:"color"`
		Description *string  `json:"description"`
	}
	// Other writes, to pulls for one, are serveRepo's to check and decode.
	if r.Method != "GET" && (segs[0] == "issues" || segs[0] == "labels") {
		if perms["issues"] != "write" {
			writeJSON(w, 403, map[string]string{"message": "Resource not accessible by integration"})
			return true
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"message": "Problems parsing JSON"})
			return true
		}
	}
	issueSub := func(name string) bool { return segs[0] == "issues" && len(segs) == 3 && segs[2] == name }
	switch {
	case r.Method == "POST" && len(segs) == 1 && segs[0] == "issues":
		repo.next++
		now := f.stamp()
		nis := &fakeIssue{number: repo.next, title: *in.Title, author: who, state: "open", created: now, updated: now}
		if in.Body != nil {
			nis.body = *in.Body
		}
		repo.issues[nis.number] = nis
		f.setLabels(repo, nis, who, in.Labels)
		writeJSON(w, 201, f.issueJSON(full, nis))
	case r.Method == "PATCH" && segs[0] == "issues" && len(segs) == 2:
		// Every PR is an issue too, which closes with it.
		if is == nil {
			notFound(w)
			return true
		}
		if in.State != nil {
			is.state, is.updated = *in.State, f.now()
		}
		title, body := is.title, is.body
		if in.Title != nil {
			title = *in.Title
		}
		if in.Body != nil {
			body = *in.Body
		}
		f.edit(is, who, title, body)
		writeJSON(w, 200, f.issueJSON(full, is))
	case r.Method == "PATCH" && segs[0] == "issues" && len(segs) == 3 && segs[1] == "comments":
		id, _ := strconv.ParseInt(segs[2], 10, 64)
		for _, is := range repo.issues {
			for i := range is.comments {
				if c := &is.comments[i]; c.id == id {
					c.body, c.updated = *in.Body, f.now()
					writeJSON(w, 200, map[string]any{"id": c.id, "body": c.body, "user": map[string]any{"login": c.author}, "created_at": c.created, "updated_at": c.updated})
					return true
				}
			}
		}
		notFound(w)
	case r.Method == "POST" && issueSub("comments"):
		if is == nil {
			notFound(w)
			return true
		}
		c := fakeComment{id: int64(1000*is.number + len(is.comments) + 1), author: who, body: *in.Body, created: f.now()}
		is.comments = append(is.comments, c)
		writeJSON(w, 201, map[string]any{"id": c.id, "body": c.body, "user": map[string]any{"login": who}, "created_at": c.created, "updated_at": c.created})
	case r.Method == "GET" && issueSub("labels"):
		if is == nil {
			notFound(w)
			return true
		}
		page(f, w, r, labelsJSON(repo, is.labels))
		if f.onLabelsRead != nil {
			read := f.onLabelsRead
			f.onLabelsRead = nil
			read(is)
		}
	case (r.Method == "POST" || r.Method == "PUT") && issueSub("labels"):
		if is == nil {
			notFound(w)
			return true
		}
		names := in.Labels
		if r.Method == "POST" {
			names = append(slices.Clone(is.labels), in.Labels...)
		}
		f.setLabels(repo, is, who, names)
		writeJSON(w, 200, labelsJSON(repo, is.labels))
	case r.Method == "GET" && issueSub("events"):
		if is == nil {
			notFound(w)
			return true
		}
		var out []map[string]any
		for _, e := range is.events {
			if e.rename != nil {
				out = append(out, map[string]any{"id": e.id, "event": "renamed", "actor": map[string]any{"login": e.actor},
					"rename": map[string]any{"from": e.rename[0], "to": e.rename[1]}, "created_at": e.at.UTC().Truncate(time.Second)})
				continue
			}
			ev := "unlabeled"
			if e.added {
				ev = "labeled"
			}
			out = append(out, map[string]any{"id": e.id, "event": ev, "actor": map[string]any{"login": e.actor},
				"label": map[string]any{"name": e.label, "color": repo.labels[e.label].color}, "created_at": e.at.UTC().Truncate(time.Second)})
		}
		page(f, w, r, out)
	case r.Method == "GET" && len(segs) == 1 && segs[0] == "labels":
		var names []string
		for n := range repo.labels {
			names = append(names, n)
		}
		slices.Sort(names)
		page(f, w, r, labelsJSON(repo, names))
	case r.Method == "POST" && len(segs) == 1 && segs[0] == "labels":
		if _, ok := repo.labels[in.Name]; ok {
			writeJSON(w, 422, map[string]any{"message": "Validation Failed", "errors": []map[string]string{{"resource": "Label", "code": "already_exists", "field": "name"}}})
			return true
		}
		l := fakeLabel{}
		if in.Color != nil {
			l.color = *in.Color
		}
		if in.Description != nil {
			l.description = *in.Description
		}
		repo.labels[in.Name] = l
		writeJSON(w, 201, labelsJSON(repo, []string{in.Name})[0])
	case (r.Method == "GET" || r.Method == "PATCH") && len(segs) >= 2 && segs[0] == "labels":
		// A name with a slash in it must arrive escaped, or it's a path.
		name := strings.Join(segs[1:], "/")
		l, ok := repo.labels[name]
		if !ok || (len(segs) > 2 && !strings.Contains(r.URL.EscapedPath(), "%2F")) {
			notFound(w)
			return true
		}
		if r.Method == "GET" {
			writeJSON(w, 200, labelsJSON(repo, []string{name})[0])
			return true
		}
		if in.Color != nil {
			l.color = *in.Color
		}
		if in.Description != nil {
			l.description = *in.Description
		}
		repo.labels[name] = l
		writeJSON(w, 200, labelsJSON(repo, []string{name})[0])
	case r.Method == "GET" && len(segs) == 3 && segs[0] == "collaborators" && segs[2] == "permission":
		if perms["metadata"] == "" {
			writeJSON(w, 403, map[string]string{"message": "Resource not accessible by integration"})
			return true
		}
		if strings.HasPrefix(segs[1], "ghost") { // no such user
			notFound(w)
			return true
		}
		role := f.role(repo, segs[1])
		base, ok := map[string]string{"admin": "admin", "maintain": "write", "write": "write", "triage": "read", "read": "read", "": "none"}[role]
		if !ok { // a custom role, here always built on read
			base = "read"
		}
		writeJSON(w, 200, map[string]any{"permission": base, "role_name": role, "user": map[string]any{"login": segs[1]}})
	default:
		return false
	}
	return true
}

// serveGraphQL answers the one GraphQL query the adapter makes, an issue
// body's userContentEdits, for the repos the installation in and token t cover.
func (f *fakeGitHub) serveGraphQL(w http.ResponseWriter, r *http.Request, in *fakeInstall, t fakeToken) {
	var q struct {
		Query     string `json:"query"`
		Variables struct {
			Owner  string  `json:"owner"`
			Name   string  `json:"name"`
			Number int     `json:"number"`
			After  *string `json:"after"`
		} `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil || !strings.Contains(q.Query, "userContentEdits") {
		writeJSON(w, 400, map[string]string{"message": "Problems parsing JSON"})
		return
	}
	// The variables are read from JSON, so a query that doesn't declare and
	// use them would pass here yet fail to parse on GitHub.
	query := strings.Join(strings.Fields(q.Query), " ")
	for _, want := range []string{
		"$owner: String!", "$name: String!", "$number: Int!", "$after: String",
		"owner: $owner", "name: $name", "number: $number", "after: $after",
	} {
		if !strings.Contains(query, want) {
			writeJSON(w, 200, map[string]any{"errors": []map[string]any{{"message": "Parse error: query lacks " + strconv.Quote(want)}}})
			return
		}
	}
	full := q.Variables.Owner + "/" + q.Variables.Name
	var is *fakeIssue
	if repo := f.repos[full]; repo != nil && slices.Contains(in.repos, full) && t.reaches(full) {
		is = repo.issues[q.Variables.Number]
	}
	if is == nil || is.isPR {
		writeJSON(w, 200, map[string]any{"data": map[string]any{"repository": nil},
			"errors": []map[string]any{{"type": "NOT_FOUND", "message": "Could not resolve to an Issue with the number of " + strconv.Itoa(q.Variables.Number) + "."}}})
		return
	}
	node := func(login string, at time.Time) map[string]any {
		var editor any
		switch {
		case strings.HasSuffix(login, "[bot]"):
			editor = map[string]any{"__typename": "Bot", "login": strings.TrimSuffix(login, "[bot]")}
		case login != "":
			editor = map[string]any{"__typename": "User", "login": login}
		}
		return map[string]any{"editedAt": at, "editor": editor}
	}
	var all []map[string]any
	for i := len(is.edits) - 1; i >= 0; i-- {
		all = append(all, node(is.edits[i].editor, is.edits[i].at))
	}
	if len(all) > 0 {
		all = append(all, node(is.author, cmp.Or(is.original, is.created)))
	}
	per := cmp.Or(f.editsPage, 100)
	lo := 0
	if q.Variables.After != nil {
		lo, _ = strconv.Atoi(*q.Variables.After)
	}
	lo = min(lo, len(all))
	hi := min(lo+per, len(all))
	nodes := all[lo:hi]
	if nodes == nil {
		nodes = []map[string]any{}
	}
	writeJSON(w, 200, map[string]any{"data": map[string]any{"repository": map[string]any{"issue": map[string]any{
		"createdAt": is.created,
		"userContentEdits": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": hi < len(all), "endCursor": strconv.Itoa(hi)},
			"nodes":    nodes,
		},
	}}}})
}

var fakeLevel = map[string]int{"read": 1, "write": 2, "admin": 3}

// reaches reports whether the token was minted for repo full.
func (t fakeToken) reaches(full string) bool {
	return t.repos == nil || slices.Contains(t.repos, full)
}

// effective is what the token may do, given what its installation has now.
func (t fakeToken) effective(granted map[string]string) map[string]string {
	if t.perms == nil {
		return granted
	}
	out := map[string]string{}
	for p, lvl := range t.perms {
		if g := granted[p]; fakeLevel[g] < fakeLevel[lvl] {
			lvl = g
		}
		if lvl != "" {
			out[p] = lvl
		}
	}
	return out
}

// serveSearch answers the search API's issue search as GitHub does for an
// installation token t: the issues and PRs that match q among the repos
// the token reaches and its account's public ones, oldest first, of which
// only the first searchCap can be read.
func (f *fakeGitHub) serveSearch(w http.ResponseWriter, r *http.Request, t fakeToken) {
	f.searches++
	invalid := func(why string) {
		writeJSON(w, 422, map[string]any{"message": "Validation Failed", "errors": []map[string]string{{"message": why}}})
	}
	q := r.URL.Query()
	if q.Get("sort") != "created" || q.Get("order") != "asc" {
		invalid("want sort=created&order=asc")
		return
	}
	var kind, state, lbl, account string
	var org bool
	var from, to time.Time
	for _, term := range strings.Fields(q.Get("q")) {
		k, v, _ := strings.Cut(term, ":")
		switch {
		case k == "is" && (v == "issue" || v == "pr"):
			kind = v
		case (k == "is" && v == "open") || k == "state":
			state = v
		case k == "label":
			l, err := strconv.Unquote(v)
			if err != nil {
				invalid("label " + v + " isn't quoted")
				return
			}
			lbl = l
		case k == "org" || k == "user":
			account, org = v, k == "org"
		case k == "created":
			a, b, _ := strings.Cut(v, "..")
			var err1, err2 error
			from, err1 = time.Parse(time.RFC3339, a)
			to, err2 = time.Parse(time.RFC3339, b)
			if err1 != nil || err2 != nil {
				invalid("created:" + v)
				return
			}
		default:
			invalid("unknown qualifier " + term)
			return
		}
	}
	// GitHub requires is:issue or is:pr, and the token is for one account.
	in := f.installs[t.install]
	if kind == "" || account == "" || lbl == "" {
		invalid("want is:, label: and org: or user:")
		return
	}
	if owner, _, _ := strings.Cut(in.repos[0], "/"); owner != account || org == in.user {
		invalid("The listed users and repositories cannot be searched either because the resources do not exist or you do not have permission to view them.")
		return
	}
	perms := t.effective(in.perms)
	type hit struct {
		at time.Time
		m  map[string]any
	}
	var hits []hit
	for _, full := range slices.Sorted(maps.Keys(f.repos)) {
		repo := f.repos[full]
		if owner, _, _ := strings.Cut(full, "/"); owner != account {
			continue
		}
		installed := slices.Contains(in.repos, full) && t.reaches(full)
		if !installed && !repo.public {
			continue
		}
		for _, n := range sortedKeys(repo.issues) {
			is := repo.issues[n]
			need := map[bool]string{false: "issues", true: "pull_requests"}[is.isPR]
			switch {
			case is.isPR != (kind == "pr"), state != "" && is.state != state, !slices.Contains(is.labels, lbl),
				!from.IsZero() && (is.created.Truncate(time.Second).Before(from) || is.created.Truncate(time.Second).After(to)),
				!repo.public && perms[need] == "":
				continue
			}
			m := f.issueJSON(full, is)
			m["repository_url"] = f.URL() + "/repos/" + full
			hits = append(hits, hit{is.created, m})
		}
	}
	slices.SortStableFunc(hits, func(a, b hit) int { return a.at.Compare(b.at) })
	var all []map[string]any
	for _, h := range hits {
		all = append(all, h.m)
	}
	per, _ := strconv.Atoi(q.Get("per_page"))
	n, _ := strconv.Atoi(q.Get("page"))
	if per <= 0 {
		per = 30
	}
	if (max(n, 1)-1)*per >= searchCap {
		invalid("Only the first 1000 search results are available")
		return
	}
	readable := all[:min(len(all), searchCap)]
	writeJSON(w, 200, map[string]any{"total_count": len(all), "incomplete_results": false, "items": pageOf(w, r, readable)})
}

// commit is the commit ref names: a tag's, a branch's, or a SHA the fake
// has seen; "" for none.
func (r *fakeRepo) commit(ref string) string {
	for _, t := range r.tags {
		if t[0] == ref {
			return t[1]
		}
	}
	if sha, ok := r.branches[ref]; ok {
		return sha
	}
	if _, ok := r.history[ref]; ok {
		return ref
	}
	for _, sha := range r.branches {
		if sha == ref {
			return ref
		}
	}
	return ""
}

// ancestry is sha and every commit in its history.
func (r *fakeRepo) ancestry(sha string) map[string]bool {
	seen := map[string]bool{}
	for todo := []string{sha}; len(todo) > 0; todo = todo[1:] {
		if c := todo[0]; !seen[c] {
			seen[c] = true
			todo = append(todo, r.history[c]...)
		}
	}
	return seen
}

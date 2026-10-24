package github

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/forge"
)

var (
	ctx  = context.Background()
	repo = forge.RepoRef{Provider: "github", Owner: "acme", Name: "app"}
)

// clock is a fake time shared by the client and the fake GitHub.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// sleeper records the waits a client asks for, passing the time on clk.
type sleeper struct {
	mu    sync.Mutex
	clk   *clock
	waits []time.Duration
}

func (s *sleeper) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	s.mu.Unlock()
	s.clk.add(d)
	return nil
}

func withClock(f *fakeGitHub, c *Client) (*clock, *sleeper) {
	clk := &clock{t: time.Now()}
	f.mu.Lock()
	f.now = clk.now
	f.mu.Unlock()
	s := &sleeper{clk: clk}
	c.now, c.sleep = clk.now, s.sleep
	return clk, s
}

// tokenOf is the installation token in a GitAuth header.
func tokenOf(t *testing.T, header string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Authorization: Basic "))
	user, tok, ok := strings.Cut(string(b), ":")
	if err != nil || !ok || user != "x-access-token" {
		t.Fatalf("header %q isn't x-access-token Basic auth", header)
	}
	return tok
}

func TestNewReadsTheAppsLogins(t *testing.T) {
	c := newFakeGitHub(t, "").client()
	if impl, rev := c.BotUsers(); impl != "devbot[bot]" || rev != "devbot-review[bot]" {
		t.Fatalf("BotUsers() = %q, %q", impl, rev)
	}
}

// A key that's missing, malformed or not the App's fails New, saying
// which App, without quoting the key.
func TestNewRejectsBadKeys(t *testing.T) {
	f := newFakeGitHub(t, "")
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKCS8PrivateKey(ec)
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(testKey(0))
	garbage := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("not a key at all")})
	for name, tc := range map[string]struct {
		key  []byte
		want string
	}{
		"empty":         {nil, "empty"},
		"not PEM":       {[]byte("MIIEvQIBADANBgkqhkiG9w0BAQEFAASC"), "not PEM"},
		"bad PKCS#1":    {garbage, "PKCS#1"},
		"EC key":        {pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}), "not an RSA key"},
		"another App's": {pemKey(testKey(2)), "401"},
	} {
		cfg := f.config()
		cfg.ReviewerKey = tc.key
		_, err := New(ctx, cfg)
		if err == nil || !strings.Contains(err.Error(), "reviewer App") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v", name, err)
			continue
		}
		if len(tc.key) > 10 && strings.Contains(err.Error(), string(tc.key[11:20])) {
			t.Errorf("%s: the error quotes the key: %v", name, err)
		}
	}
	cfg := f.config()
	cfg.ImplementerKey = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	if _, err := New(ctx, cfg); err != nil {
		t.Errorf("a PKCS#8 key: %v", err)
	}
}

func TestJWTClaims(t *testing.T) {
	a, err := newApp(forge.RoleImplementer, 42, pemKey(testKey(0)), "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	cr, err := a.jwt(now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(cr.token), ".")
	var hdr map[string]string
	var claims map[string]int64
	h, _ := base64.RawURLEncoding.DecodeString(parts[0])
	c, _ := base64.RawURLEncoding.DecodeString(parts[1])
	json.Unmarshal(h, &hdr)
	json.Unmarshal(c, &claims)
	if hdr["alg"] != "RS256" || claims["iss"] != 42 || claims["iat"] != now.Unix()-60 ||
		claims["exp"] <= now.Unix() || claims["exp"] > now.Add(10*time.Minute).Unix() {
		t.Fatalf("header %v, claims %v", hdr, claims)
	}
}

// One token per installation, reused until it's within the refresh margin
// of expiring.
func TestInstallationTokenCache(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	clk, _ := withClock(f, c)
	header := func() string {
		_, h, err := c.GitAuth(ctx, repo, forge.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		return tokenOf(t, h)
	}
	first := header()
	if header() != first || f.mintCount(11) != 1 {
		t.Fatalf("a fresh token was minted again: %d mints", f.mintCount(11))
	}
	clk.add(time.Hour - refreshMargin - time.Minute)
	if header() != first || f.mintCount(11) != 1 {
		t.Fatal("a token with more than the margin left was replaced")
	}
	clk.add(2 * time.Minute) // inside the margin
	second := header()
	if second == first || f.mintCount(11) != 2 {
		t.Fatalf("a token about to expire was handed out (%d mints)", f.mintCount(11))
	}
	clk.add(2 * time.Hour) // long expired
	if header() == second || f.mintCount(11) != 3 {
		t.Fatal("an expired token was handed out")
	}
	// The reads use the cached token too.
	if _, err := c.DefaultBranch(ctx, repo); err != nil || f.mintCount(11) != 3 {
		t.Fatalf("DefaultBranch: %v, %d mints", err, f.mintCount(11))
	}
}

// Whenever it's asked, GitAuth hands out a token with at least
// refreshMargin left, time enough for the fetch or push it is for.
func TestGitAuthTokenHasTheMarginLeft(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	clk, _ := withClock(f, c)
	for range 3 * 60 * 2 {
		for _, role := range []forge.Role{forge.RoleImplementer, forge.RoleReviewer} {
			_, h, err := c.GitAuth(ctx, repo, role)
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			tk, ok := f.tokens[tokenOf(t, h)]
			f.mu.Unlock()
			if left := tk.expires.Sub(clk.now()); !ok || left < refreshMargin {
				t.Fatalf("%s token with %v left (known %v), want at least %v", role, left, ok, refreshMargin)
			}
		}
		clk.add(30 * time.Second)
	}
	if f.mintCount(11) < 3 {
		t.Fatalf("three hours took %d mints", f.mintCount(11))
	}
}

func TestConcurrentCallersShareOneMint(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	f.mintDelay = 100 * time.Millisecond
	var wg sync.WaitGroup
	toks := make(chan string, 40)
	for i := range 40 {
		wg.Go(func() {
			role := []forge.Role{forge.RoleImplementer, forge.RoleReviewer}[i%2]
			_, h, err := c.GitAuth(ctx, repo, role)
			if err != nil {
				t.Error(err)
				return
			}
			toks <- string(role) + " " + tokenOf(t, h)
		})
	}
	wg.Wait()
	close(toks)
	seen := map[string]bool{}
	for tk := range toks {
		seen[tk] = true
	}
	if f.mintCount(11) != 1 || f.mintCount(22) != 1 || len(seen) != 2 {
		t.Fatalf("mints: implementer %d, reviewer %d; tokens %v", f.mintCount(11), f.mintCount(22), seen)
	}
}

// A caller that gives up doesn't fail the mint for those still waiting.
func TestAbandonedMintStillServesOthers(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	f.mintDelay = 100 * time.Millisecond
	short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if _, _, err := c.GitAuth(short, repo, forge.RoleImplementer); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if _, _, err := c.GitAuth(ctx, repo, forge.RoleImplementer); err != nil || f.mintCount(11) != 1 {
		t.Fatalf("%v, %d mints", err, f.mintCount(11))
	}
}

// The reviewer acts only with its own App's installation, and the
// implementer with its own.
func TestAppsAreSeparate(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	_, ih, _ := c.GitAuth(ctx, repo, forge.RoleImplementer)
	_, rh, _ := c.GitAuth(ctx, repo, forge.RoleReviewer)
	f.mu.Lock()
	defer f.mu.Unlock()
	if it, rt := f.tokens[tokenOf(t, ih)], f.tokens[tokenOf(t, rh)]; it.install != 11 || rt.install != 22 {
		t.Fatalf("implementer's token is installation %d's, reviewer's %d's", it.install, rt.install)
	}
}

// Each token is minted for the one repo it is used on, with what its role
// needs out of what the installation has, and nothing the App wasn't
// granted: the implementer's carries workflows when granted, the
// reviewer's never does.
func TestTokensAreForOneRepo(t *testing.T) {
	f := newFakeGitHub(t, "")
	f.repos["acme/other"] = newFakeRepo()
	f.installs[11].repos = append(f.installs[11].repos, "acme/other")
	f.installs[11].perms["workflows"] = "write"
	f.installs[11].perms["checks"] = "read"
	f.installs[22].perms["statuses"] = "read"
	f.installs[22].perms["workflows"] = "write"
	c := f.client()
	_, ih, err := c.GitAuth(ctx, repo, forge.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.GitAuth(ctx, repo, forge.RoleReviewer); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	bodies := map[int64][]map[string]any{11: f.mintBodies[11], 22: f.mintBodies[22]}
	tok := tokenOf(t, ih)
	f.mu.Unlock()
	want := map[int64]string{
		11: `{"permissions":{"checks":"read","contents":"write","issues":"write","metadata":"read","pull_requests":"write","workflows":"write"},"repositories":["app"]}`,
		22: `{"permissions":{"contents":"write","issues":"write","metadata":"read","pull_requests":"write","statuses":"read"},"repositories":["app"]}`,
	}
	for id, w := range want {
		if len(bodies[id]) != 1 {
			t.Fatalf("installation %d: %d mints", id, len(bodies[id]))
		}
		if got, _ := json.Marshal(bodies[id][0]); string(got) != w {
			t.Errorf("installation %d's token request:\n got %s\nwant %s", id, got, w)
		}
	}

	// The implementer's token for acme/app doesn't reach acme/other, and
	// acme/other gets a token of its own.
	req, _ := http.NewRequest("GET", f.URL()+"/repos/acme/other", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("acme/app's token read acme/other: %d", resp.StatusCode)
	}
	other := forge.RepoRef{Provider: "github", Owner: "acme", Name: "other"}
	if _, err := c.DefaultBranch(ctx, other); err != nil || f.mintCount(11) != 2 {
		t.Fatalf("acme/other: %v, %d mints", err, f.mintCount(11))
	}
}

// The implementer's token asks for as much of workflows as the installation
// grants, and CanPushWorkflows says whether that is write; the reviewer's
// asks for none, whatever its installation grants.
func TestWorkflowsFollowTheGrant(t *testing.T) {
	for _, grant := range []string{"write", "read", ""} {
		t.Run(cmp.Or(grant, "none"), func(t *testing.T) {
			f := newFakeGitHub(t, "")
			for _, id := range []int64{11, 22} {
				if grant != "" {
					f.installs[id].perms["workflows"] = grant
				}
			}
			c := f.client()
			for _, role := range []forge.Role{forge.RoleImplementer, forge.RoleReviewer} {
				ok, err := c.CanPushWorkflows(ctx, repo, role)
				if err != nil {
					t.Fatal(err)
				}
				if want := role == forge.RoleImplementer && grant == "write"; ok != want {
					t.Errorf("%s: CanPushWorkflows %v, want %v", role, ok, want)
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			impl := f.mintBodies[11][0]["permissions"].(map[string]any)
			if got, _ := impl["workflows"].(string); got != grant {
				t.Errorf("the implementer's token asked for workflows %q, want %q", got, grant)
			}
			if rev := f.mintBodies[22][0]["permissions"].(map[string]any); rev["workflows"] != nil {
				t.Errorf("the reviewer's token asked for workflows %v", rev["workflows"])
			}
		})
	}
}

// The tokens reconcile searches and lists repos with never ask for
// workflows, whatever the installations grant.
func TestSearchAndListingTokensLackWorkflows(t *testing.T) {
	f := newFakeGitHub(t, "")
	seed(f)
	for _, id := range []int64{11, 22} {
		f.installs[id].perms["workflows"] = "write"
	}
	c := f.client()
	if _, errs := pass(t, c); len(errs) > 0 {
		t.Fatal(errs)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	kinds := map[string]int{}
	for id, bodies := range f.mintBodies {
		for _, b := range bodies {
			perms := b["permissions"].(map[string]any)
			kind := "listing"
			if perms["issues"] != nil {
				kind = "search"
			}
			kinds[kind]++
			if b["repositories"] != nil {
				t.Errorf("installation %d: a repo's token minted by reconcile: %v", id, b)
			}
			if perms["workflows"] != nil {
				t.Errorf("installation %d: a %s token asked for workflows: %v", id, kind, perms)
			}
		}
	}
	if kinds["search"] == 0 || kinds["listing"] == 0 {
		t.Fatalf("minted %v, want search and listing tokens", kinds)
	}
}

// A job pod holds one App's key and is told the other's login: it acts as
// its own App in every call that takes no role, and refuses to act as the
// other.
func TestOneAppOnly(t *testing.T) {
	f := newFakeGitHub(t, "")
	ref := seed(f)
	cfg := f.config()
	cfg.ImplementerKey, cfg.ImplementerLogin = nil, "devbot[bot]"
	c, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if impl, rev := c.BotUsers(); impl != "devbot[bot]" || rev != "devbot-review[bot]" {
		t.Fatalf("BotUsers: %s, %s", impl, rev)
	}
	if err := c.Comment(ctx, ref, "hello"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	cs := f.repos["acme/app"].issues[ref.Number].comments
	f.mu.Unlock()
	if len(cs) != 1 || cs[0].author != "devbot-review[bot]" {
		t.Fatalf("comments: %+v", cs)
	}
	if _, err := c.GetItem(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.GitAuth(ctx, repo, forge.RoleReviewer); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.GitAuth(ctx, repo, forge.RoleImplementer); !errors.Is(err, forge.ErrNoCredential) {
		t.Fatalf("GitAuth as the implementer: %v", err)
	}
	if _, err := c.CanWrite(ctx, repo, forge.RoleImplementer); !errors.Is(err, forge.ErrNoCredential) {
		t.Fatalf("CanWrite as the implementer: %v", err)
	}
	if f.mintCount(11) != 0 {
		t.Fatal("the implementer's installation was asked for a token")
	}

	cfg.ReviewerKey = nil
	if _, err := New(ctx, cfg); err == nil {
		t.Fatal("a client with neither key")
	}
}

// An App installed with less than contents, pull requests and issues write
// can't work in the repo, and the error names the App, what it has and
// what it needs.
func TestCanWriteNeedsEveryPermission(t *testing.T) {
	for _, p := range []string{"contents", "pull_requests", "issues"} {
		f := newFakeGitHub(t, "")
		c := f.client()
		f.mu.Lock()
		f.installs[22].perms[p] = "read"
		f.mu.Unlock()
		ok, err := c.CanWrite(ctx, repo, forge.RoleReviewer)
		var me *forge.MissingAccessError
		if ok || !errors.As(err, &me) {
			t.Fatalf("%s read: got %v, %v", p, ok, err)
		}
		want := "the reviewer App devbot-review is installed with " + p + ": read; devbot needs " + p + ": write, so grant it in the App's settings and accept the change on the installation"
		if me.Need != want {
			t.Errorf("%s read:\n got %q\nwant %q", p, me.Need, want)
		}
	}
	f := newFakeGitHub(t, "")
	c := f.client()
	f.mu.Lock()
	delete(f.installs[11].perms, "issues")
	f.installs[11].perms["contents"] = "read"
	f.mu.Unlock()
	_, err := c.CanWrite(ctx, repo, forge.RoleImplementer)
	if err == nil || !strings.Contains(err.Error(), "the implementer App devbot is installed with contents: read, issues: none; devbot needs contents: write, issues: write") {
		t.Fatalf("implementer without issues: %v", err)
	}
	problems, err := c.CheckInstallations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.HasPrefix(problems[0], "on acme, the implementer App devbot is installed with contents: read, issues: none;") {
		t.Fatalf("CheckInstallations: %q", problems)
	}
}

// A repo with an App installed without what it needs isn't enabled, and
// says so in a way whoever runs the install can act on.
func TestNotEnabledNamesMissingPermission(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	f.mu.Lock()
	f.installs[22].perms["issues"] = "read"
	f.mu.Unlock()
	rc := &config.RepoCache{Forges: forge.Registry{"github": c}, Policy: config.RepoPolicy{}}
	_, err := rc.Get(ctx, repo)
	if !errors.Is(err, config.ErrNotOptedIn) || !strings.Contains(err.Error(), "the reviewer App devbot-review is installed with issues: read; devbot needs issues: write") {
		t.Fatalf("got %v", err)
	}
}

func TestCanWriteWithEveryPermission(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	if ok, err := c.CanWrite(ctx, repo, forge.RoleImplementer); !ok || err != nil {
		t.Fatalf("implementer: got %v, %v", ok, err)
	}
}

// An App reinstalled under a new ID is found again.
func TestReinstalledApp(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	if _, err := c.DefaultBranch(ctx, repo); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.installs[33] = f.installs[11]
	delete(f.installs, 11)
	f.tokens = map[string]fakeToken{}
	f.mu.Unlock()
	c.impl.tokens = map[tokenKey]*mint{} // the old token expired meanwhile
	if _, err := c.DefaultBranch(ctx, repo); err != nil || f.mintCount(33) != 1 {
		t.Fatalf("%v, %d mints", err, f.mintCount(33))
	}
}

// Neither a JWT nor a token appears in an error or a formatted value.
func TestSecretsAreNotPrinted(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	cr, err := c.repoCred(ctx, forge.RoleImplementer, repo)
	if err != nil {
		t.Fatal(err)
	}
	jwt, _ := c.appCred(c.impl)
	for _, s := range []string{fmt.Sprint(cr), fmt.Sprintf("%v %+v %#v %s", cr, cr, cr, cr.token), fmt.Sprintf("%+v", jwt)} {
		if strings.Contains(s, string(cr.token)) || strings.Contains(s, string(jwt.token)) {
			t.Fatalf("printed: %s", s)
		}
	}
	// A refused mint's error says what failed, not with what.
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			writeJSON(w, 403, map[string]string{"message": "suspended"})
			return true
		}
		return false
	}
	c.impl.tokens = map[tokenKey]*mint{}
	_, err = c.GetFile(ctx, repo, "", "x")
	if err == nil || !strings.Contains(err.Error(), "suspended") || strings.Contains(err.Error(), "eyJ") || !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestWebBase(t *testing.T) {
	for api, want := range map[string]string{
		"https://api.github.com":         "https://github.com",
		"https://ghe.example.com/api/v3": "https://ghe.example.com",
		"https://api.acme.ghe.com":       "https://acme.ghe.com",
	} {
		u, _ := url.Parse(api)
		if got := webBase(u); got != want {
			t.Errorf("%s: got %s, want %s", api, got, want)
		}
	}
	c, err := newClient(Config{Name: "github", URL: "https://ghe.example.com/api/v3/", ImplementerAppID: 1, ImplementerKey: pemKey(testKey(0)), ReviewerAppID: 2, ReviewerKey: pemKey(testKey(1))})
	if err != nil {
		t.Fatal(err)
	}
	for got, want := range map[string]string{
		c.IssueURL(forge.ItemRef{Repo: repo, Number: 12}): "https://ghe.example.com/acme/app/issues/12",
		c.PRURL(repo, 3):                   "https://ghe.example.com/acme/app/pull/3",
		c.BranchURL(repo, "devbot/12-a b"): "https://ghe.example.com/acme/app/tree/devbot/12-a%20b",
	} {
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

func TestNextLink(t *testing.T) {
	h := http.Header{}
	h.Add("Link", `<https://api.github.com/repositories/1/issues?page=1>; rel="prev", <https://api.github.com/repositories/1/issues?page=3>; rel="next"`)
	h.Add("Link", `<https://api.github.com/repositories/1/issues?page=9>; rel="last"`)
	if got := nextLink(h); got != "https://api.github.com/repositories/1/issues?page=3" {
		t.Fatalf("got %q", got)
	}
	if got := nextLink(http.Header{}); got != "" {
		t.Fatalf("no Link: got %q", got)
	}
}

// A Link to anywhere but the API isn't followed: the token would go with it.
func TestPaginationStaysOnTheAPI(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/pulls") {
			w.Header().Set("Link", `<https://evil.example/repos/acme/app/pulls?state=open&page=2>; rel="next"`)
			writeJSON(w, 200, []map[string]any{{"number": 1}})
			return true
		}
		return false
	}
	if _, err := c.ListOpenPRs(ctx, repo); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("got %v", err)
	}
}

// rateLimited answers the first calls to path with the given responses.
func rateLimited(f *fakeGitHub, path string, responses ...func(w http.ResponseWriter)) *int {
	n := new(int)
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != path || *n >= len(responses) {
			return false
		}
		responses[*n](w)
		*n++
		return true
	}
	return n
}

func waits(provider, kind string) forge.WaitStat {
	return forge.RateLimitWaits.Snapshot()[[2]string{provider, kind}]
}

func TestPrimaryRateLimit(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	c.cfg.Name = "github-primary"
	clk, s := withClock(f, c)
	before := waits("github-primary", "primary")
	if _, err := c.DefaultBranch(ctx, repo); err != nil { // mint the token first
		t.Fatal(err)
	}
	reset := clk.now().Add(30 * time.Second).Unix()
	spent := func(w http.ResponseWriter) {
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.Header().Set("X-Ratelimit-Reset", fmt.Sprint(reset))
		writeJSON(w, 403, map[string]string{"message": "API rate limit exceeded for installation ID 11."})
	}
	n := rateLimited(f, "/repos/acme/app", spent)
	if b, err := c.DefaultBranch(ctx, repo); err != nil || b != "main" || *n != 1 {
		t.Fatalf("got %q, %v after %d limited calls", b, err, *n)
	}
	if len(s.waits) != 1 || s.waits[0] < 30*time.Second || s.waits[0] > 32*time.Second {
		t.Fatalf("waits %v; want until the reset", s.waits)
	}
	if w := waits("github-primary", "primary"); w.Count-before.Count != 1 || w.Seconds-before.Seconds < 30 {
		t.Fatalf("metric: %+v", w)
	}

	// A success that spends the last call holds the next one back until
	// the reset, rather than send it to be refused.
	s.waits = nil
	reset = clk.now().Add(20 * time.Second).Unix()
	rateLimited(f, "/repos/acme/app", func(w http.ResponseWriter) {
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.Header().Set("X-Ratelimit-Reset", fmt.Sprint(reset))
		writeJSON(w, 200, map[string]string{"default_branch": "main"})
	})
	c.DefaultBranch(ctx, repo)
	if len(s.waits) != 0 {
		t.Fatalf("waited before the limit was spent: %v", s.waits)
	}
	if _, err := c.DefaultBranch(ctx, repo); err != nil || len(s.waits) != 1 || s.waits[0] < 20*time.Second {
		t.Fatalf("%v, waits %v", err, s.waits)
	}

	// A reset too far off fails the call instead of blocking it.
	s.waits = nil
	reset = clk.now().Add(time.Hour).Unix()
	rateLimited(f, "/repos/acme/app", spent)
	_, err := c.DefaultBranch(ctx, repo)
	var rl *forge.RateLimitError
	if !errors.As(err, &rl) || rl.Kind != "primary" || len(s.waits) != 0 {
		t.Fatalf("got %v, waits %v", err, s.waits)
	}
	// And the next call doesn't go out at all until then.
	calls := f.calls
	if _, err := c.DefaultBranch(ctx, repo); !errors.As(err, &rl) || f.calls != calls {
		t.Fatalf("got %v after %d calls", err, f.calls-calls)
	}
}

// GraphQL answers a rate limit 200, with an error of type RATE_LIMITED. It
// is waited out like a REST one, and holds back GraphQL calls only: REST
// has a budget of its own.
func TestGraphQLRateLimit(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	clk, s := withClock(f, c)
	fx := f.fixture(t)
	ref := forge.ItemRef{Repo: fx.Repo, Number: fx.Issue("alice", "t", "b", "devbot/ready")}
	if _, err := c.ContentEdits(ctx, ref); err != nil { // mint the token first
		t.Fatal(err)
	}
	reset := clk.now().Add(30 * time.Second).Unix()
	spent := func(w http.ResponseWriter) {
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.Header().Set("X-Ratelimit-Reset", fmt.Sprint(reset))
		w.Header().Set("X-Ratelimit-Resource", "graphql")
		writeJSON(w, 200, map[string]any{"errors": []map[string]any{{"type": "RATE_LIMITED", "message": "API rate limit already exceeded for installation ID 11."}}})
	}
	n := rateLimited(f, "/graphql", spent)
	if _, err := c.ContentEdits(ctx, ref); err != nil || *n != 1 {
		t.Fatalf("got %v after %d limited calls", err, *n)
	}
	if len(s.waits) != 1 || s.waits[0] < 30*time.Second || s.waits[0] > 32*time.Second {
		t.Fatalf("waits %v; want until the reset", s.waits)
	}

	// Spent for an hour: the call fails, and the next GraphQL call doesn't
	// go out, but REST calls do, without waiting.
	s.waits = nil
	reset = clk.now().Add(time.Hour).Unix()
	rateLimited(f, "/graphql", spent)
	var rl *forge.RateLimitError
	if _, err := c.ContentEdits(ctx, ref); !errors.As(err, &rl) || rl.Kind != "primary" {
		t.Fatalf("got %v", err)
	}
	graphqlCalls := 0
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/graphql" {
			graphqlCalls++
		}
		return false
	}
	if _, err := c.ContentEdits(ctx, ref); !errors.As(err, &rl) || graphqlCalls != 0 {
		t.Fatalf("got %v after %d GraphQL calls", err, graphqlCalls)
	}
	if _, err := c.DefaultBranch(ctx, repo); err != nil || len(s.waits) != 0 {
		t.Fatalf("REST: %v, waits %v", err, s.waits)
	}
	if _, err := c.GetItem(ctx, ref); err != nil || len(s.waits) != 0 {
		t.Fatalf("REST: %v, waits %v", err, s.waits)
	}
}

func TestSecondaryRateLimit(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	c.cfg.Name = "github-secondary"
	c.maxWait = 10 * time.Minute
	_, s := withClock(f, c)
	before := waits("github-secondary", "secondary")
	if _, err := c.DefaultBranch(ctx, repo); err != nil {
		t.Fatal(err)
	}
	secondary := func(w http.ResponseWriter) {
		writeJSON(w, 403, map[string]string{"message": "You have exceeded a secondary rate limit. Please wait a few minutes before you try again."})
	}
	retryAfter := func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "7")
		writeJSON(w, 429, map[string]string{"message": "slow down"})
	}
	n := rateLimited(f, "/repos/acme/app", secondary, secondary, retryAfter, secondary)
	if _, err := c.DefaultBranch(ctx, repo); err != nil || *n != 4 {
		t.Fatalf("%v after %d limited calls", err, *n)
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 7 * time.Second, 8 * time.Minute}
	if fmt.Sprint(s.waits) != fmt.Sprint(want) {
		t.Fatalf("waits %v, want %v", s.waits, want)
	}
	if w := waits("github-secondary", "secondary"); w.Count-before.Count != 4 {
		t.Fatalf("metric: %+v", w)
	}

	// Past the retries, the call fails.
	s.waits = nil
	rateLimited(f, "/repos/acme/app", retryAfter, retryAfter, retryAfter, retryAfter, retryAfter, retryAfter)
	var rl *forge.RateLimitError
	if _, err := c.DefaultBranch(ctx, repo); !errors.As(err, &rl) || rl.Kind != "secondary" || len(s.waits) != maxRetries {
		t.Fatalf("got %v after waits %v", err, s.waits)
	}
}

// A 403 that isn't a rate limit is the forge refusing: no retry.
func TestForbiddenIsNotRetried(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	_, s := withClock(f, c)
	c.DefaultBranch(ctx, repo)
	n := rateLimited(f, "/repos/acme/app", func(w http.ResponseWriter) {
		w.Header().Set("X-Ratelimit-Remaining", "4999")
		writeJSON(w, 403, map[string]string{"message": "Resource not accessible by integration"})
	})
	_, err := c.DefaultBranch(ctx, repo)
	var ae *apiError
	if !errors.Is(err, forge.ErrForbidden) || !errors.As(err, &ae) || ae.Status != 403 || !strings.Contains(ae.Body, "not accessible") || *n != 1 || len(s.waits) != 0 {
		t.Fatalf("got %v, %d calls, waits %v", err, *n, s.waits)
	}
}

// A tree GitHub cuts short is read a directory at a time, whole: more than
// 20,000 files, with the Compose file Docker detection looks for among
// them. Only a single directory GitHub won't list whole is no listing.
func TestListTreeTruncated(t *testing.T) {
	f := newFakeGitHub(t, "")
	f.mu.Lock()
	r := f.repos["acme/app"]
	r.treeMax = 1000
	for d := range 41 {
		for i := range 500 {
			r.files[fmt.Sprintf("main:svc%02d/pkg/f%03d.go", d, i)] = []byte("package pkg\n")
		}
	}
	r.files["main:svc07/compose.yml"] = []byte("services: {}\n")
	f.mu.Unlock()
	files, err := f.client().ListTree(ctx, repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	want := 0
	for k := range r.files {
		if strings.HasPrefix(k, "main:") {
			want++
		}
	}
	f.mu.Unlock()
	if len(files) != want || want <= 20000 || !slices.ContainsFunc(files, func(tf forge.TreeFile) bool { return tf.Path == "svc07/compose.yml" }) {
		t.Fatalf("%d files, want all %d", len(files), want)
	}

	f.mu.Lock()
	for i := range 1001 {
		r.files[fmt.Sprintf("main:flat/f%04d", i)] = nil
	}
	f.mu.Unlock()
	if _, err := f.client().ListTree(ctx, repo, "main"); !errors.Is(err, forge.ErrTreeTooBig) {
		t.Fatalf("got %v, want ErrTreeTooBig", err)
	}
}

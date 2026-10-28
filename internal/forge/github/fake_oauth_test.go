package github

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	implClientID     = "Iv1.devbot"
	implClientSecret = "devbot-client-secret"
)

// fakeOAuth is GitHub's web flow for an App's user access tokens: codes
// bound to their PKCE challenge, tokens that expire after 8 hours, and
// refresh tokens that rotate. A person's token reaches only the repos the
// App is installed on, and does only what both the person's role and the
// App's permissions allow.
type fakeOAuth struct {
	clients   map[string]fakeClient // client ID -> its App
	codes     map[string]fakeCode
	access    map[string]fakeUserToken
	refresh   map[string]fakeUserToken
	next      int
	refreshes int
	noExpiry  bool // the App has expiring user tokens turned off
	down      bool // the token endpoint answers 503
}

type fakeClient struct {
	secret string
	app    int64
}

type fakeCode struct {
	login, redirectURI, challenge string
	app                           int64
}

type fakeUserToken struct {
	login   string
	app     int64
	expires time.Time
}

func newFakeOAuth() fakeOAuth {
	return fakeOAuth{
		clients: map[string]fakeClient{implClientID: {secret: implClientSecret, app: implAppID}},
		codes:   map[string]fakeCode{},
		access:  map[string]fakeUserToken{}, refresh: map[string]fakeUserToken{},
	}
}

// roles are GitHub's repository roles, lowest first.
var roles = []string{"read", "triage", "write", "maintain", "admin"}

func atLeast(role, min string) bool { return slices.Index(roles, role) >= slices.Index(roles, min) }

// grant gives login role on repo full.
func (f *fakeGitHub) grant(login, full, role string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos[full].collabs[login] = role
}

// authorize is login approving the App on the page authURL points at. It
// returns the code GitHub sends to the callback.
func (f *fakeGitHub) authorize(authURL, login string) string {
	f.t.Helper()
	u, err := url.Parse(authURL)
	if err != nil || u.Scheme+"://"+u.Host != f.srv.URL || u.Path != "/login/oauth/authorize" {
		f.t.Fatalf("%q isn't GitHub's authorize page", authURL)
	}
	q := u.Query()
	cl, ok := f.oauth.clients[q.Get("client_id")]
	if !ok || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Has("scope") {
		f.t.Fatalf("authorize: %v", q)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.oauth.next++
	code := fmt.Sprintf("code-%d", f.oauth.next)
	f.oauth.codes[code] = fakeCode{login: login, redirectURI: q.Get("redirect_uri"), challenge: q.Get("code_challenge"), app: cl.app}
	return code
}

// issueUserToken is a new token pair for login through app. Called with
// f.mu held.
func (f *fakeGitHub) issueUserToken(login string, app int64) map[string]any {
	f.oauth.next++
	at, rt := fmt.Sprintf("ghu_%s_%d", login, f.oauth.next), fmt.Sprintf("ghr_%s_%d", login, f.oauth.next)
	if f.oauth.noExpiry {
		f.oauth.access[at] = fakeUserToken{login: login, app: app, expires: f.now().Add(100 * 365 * 24 * time.Hour)}
		return map[string]any{"access_token": at, "token_type": "bearer", "scope": ""}
	}
	f.oauth.access[at] = fakeUserToken{login: login, app: app, expires: f.now().Add(8 * time.Hour)}
	f.oauth.refresh[rt] = fakeUserToken{login: login, app: app, expires: f.now().Add(184 * 24 * time.Hour)}
	return map[string]any{"access_token": at, "expires_in": 28800, "refresh_token": rt, "refresh_token_expires_in": 15897600, "token_type": "bearer", "scope": ""}
}

// signIn is login's token through the implementer App, without the
// browser round trip.
func (f *fakeGitHub) signIn(login string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issueUserToken(login, implAppID)["access_token"].(string)
}

// serveOAuth is the token endpoint on the web host. Like GitHub, it
// answers a refusal with a 200 and an error code.
func (f *fakeGitHub) serveOAuth(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != "POST" || r.URL.Path != "/login/oauth/access_token" {
		notFound(w)
		return
	}
	if r.Header.Get("Accept") != "application/json" {
		f.t.Errorf("token endpoint: Accept %q; GitHub answers form-encoded unless asked for JSON", r.Header.Get("Accept"))
	}
	if f.oauth.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	r.ParseForm()
	refuse := func(code string) {
		writeJSON(w, 200, map[string]string{"error": code, "error_description": "secret detail"})
	}
	cl, ok := f.oauth.clients[r.PostForm.Get("client_id")]
	if !ok || cl.secret != r.PostForm.Get("client_secret") {
		refuse("incorrect_client_credentials")
		return
	}
	if r.PostForm.Get("grant_type") == "refresh_token" {
		rt, ok := f.oauth.refresh[r.PostForm.Get("refresh_token")]
		delete(f.oauth.refresh, r.PostForm.Get("refresh_token"))
		if !ok || rt.app != cl.app || !f.now().Before(rt.expires) {
			refuse("bad_refresh_token")
			return
		}
		f.oauth.refreshes++
		writeJSON(w, 200, f.issueUserToken(rt.login, rt.app))
		return
	}
	c, ok := f.oauth.codes[r.PostForm.Get("code")]
	delete(f.oauth.codes, r.PostForm.Get("code"))
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || c.app != cl.app || c.redirectURI != r.PostForm.Get("redirect_uri") || base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
		refuse("bad_verification_code")
		return
	}
	writeJSON(w, 200, f.issueUserToken(c.login, c.app))
}

// serveUser answers a call made with a user access token (true), or leaves
// tok to the installation tokens (false). Called with f.mu held.
func (f *fakeGitHub) serveUser(w http.ResponseWriter, r *http.Request, tok, path string) bool {
	ut, ok := f.oauth.access[tok]
	if !ok {
		return false
	}
	if !f.now().Before(ut.expires) {
		writeJSON(w, 401, map[string]string{"message": "Bad credentials"})
		return true
	}
	if r.Method == "GET" && path == "/user" {
		writeJSON(w, 200, map[string]any{"login": ut.login})
		return true
	}
	rest, _ := strings.CutPrefix(path, "/repos/")
	segs := strings.SplitN(rest, "/", 3)
	if len(segs) < 2 {
		notFound(w)
		return true
	}
	full := segs[0] + "/" + segs[1]
	var in *fakeInstall
	for _, id := range sortedIDs(f.installs) {
		if i := f.installs[id]; i.app == ut.app && slices.Contains(i.repos, full) {
			in = i
		}
	}
	repo, role := f.repos[full], ""
	if repo != nil {
		role = f.role(repo, ut.login)
	}
	// The repos are private: someone without a role, or through an App
	// that isn't installed there, can't tell them from missing ones.
	if repo == nil || in == nil || role == "" {
		notFound(w)
		return true
	}
	sub := ""
	if len(segs) == 3 {
		sub = segs[2]
	}
	f.serveRepoAsUser(w, r, full, repo, sub, ut.login, role, in)
	return true
}

// serveRepoAsUser applies what GitHub checks of the person on top of what
// serveRepo checks of the App: below triage, labels can't be changed, an
// issue only its author may close, and a new issue's labels are dropped
// silently.
func (f *fakeGitHub) serveRepoAsUser(w http.ResponseWriter, r *http.Request, full string, repo *fakeRepo, sub, login, role string, in *fakeInstall) {
	segs := strings.Split(sub, "/")
	if r.Method == "GET" && sub == "" {
		writeJSON(w, 200, map[string]any{"full_name": full, "default_branch": repo.defaultBranch, "permissions": map[string]bool{
			"admin": atLeast(role, "admin"), "maintain": atLeast(role, "maintain"), "push": atLeast(role, "write"),
			"triage": atLeast(role, "triage"), "pull": true,
		}})
		return
	}
	if r.Method != "GET" && in.perms["issues"] == "write" && !atLeast(role, "triage") && segs[0] == "issues" {
		forbidden := func() { writeJSON(w, 403, map[string]string{"message": "Must have triage access to the repository"}) }
		var n int
		if len(segs) >= 2 {
			fmt.Sscan(segs[1], &n)
		}
		switch {
		case r.Method == "POST" && len(segs) == 1:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, 400, map[string]string{"message": "Problems parsing JSON"})
				return
			}
			delete(body, "labels")
			b, _ := json.Marshal(body)
			r.Body = io.NopCloser(bytes.NewReader(b))
		case r.Method == "PATCH" && len(segs) == 2 && repo.issues[n] != nil && repo.issues[n].author != login:
			forbidden()
			return
		case len(segs) == 3 && segs[2] == "labels":
			forbidden()
			return
		}
	}
	f.serveRepo(w, r, full, repo, sub, login, in.perms)
}

func (f *fakeGitHub) refreshCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.oauth.refreshes
}

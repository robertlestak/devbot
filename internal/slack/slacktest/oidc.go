package slacktest

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// OIDC is a fake of Slack's OAuth: Sign in with Slack, its authorize page,
// which SignIn plays a person through, and openid.connect.token; and an
// app's install, its page, which Install plays a person through, and
// oauth.v2.access. Its Web API is under URL+"/api/".
type OIDC struct {
	*httptest.Server
	ClientID, ClientSecret string
	// Now is the clock ID tokens expire by. Default: time.Now.
	Now func() time.Time
	// Claims, when set, edits each ID token's claims as it is issued.
	Claims func(map[string]any)

	mu       sync.Mutex
	n        int
	codes    map[string]oidcCode
	installs map[string]install
}

// install is an install approved, waiting for oauth.v2.access.
type install struct {
	redirect string
	access   map[string]any
}

type oidcCode struct {
	redirect string
	claims   map[string]any
}

func NewOIDC(t testing.TB, clientID, clientSecret string) *OIDC {
	o := &OIDC{ClientID: clientID, ClientSecret: clientSecret, codes: map[string]oidcCode{}, installs: map[string]install{}}
	o.Server = httptest.NewServer(http.HandlerFunc(o.token))
	t.Cleanup(o.Close)
	return o
}

// AuthorizeURL is the fake's Sign in with Slack page.
func (o *OIDC) AuthorizeURL() string { return o.URL + "/openid/connect/authorize" }

// InstallURL is the fake's page that installs an app.
func (o *OIDC) InstallURL() string { return o.URL + "/oauth/v2/authorize" }

// APIURL is the fake's Web API base.
func (o *OIDC) APIURL() string { return o.URL + "/api/" }

// SignIn plays a person signing in at authorize, a URL to the authorize
// page, as user of team (in enterprise, when set), and returns where Slack
// sends them back to.
func (o *OIDC) SignIn(authorize, enterprise, team, user string) (string, error) {
	u, err := url.Parse(authorize)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if !strings.HasPrefix(authorize, o.AuthorizeURL()+"?") || q.Get("client_id") != o.ClientID || q.Get("response_type") != "code" ||
		!slices.Contains(strings.Fields(q.Get("scope")), "openid") || q.Get("redirect_uri") == "" {
		return "", errors.New("slacktest: not a Sign in with Slack request: " + authorize)
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	claims := map[string]any{"iss": "https://slack.com", "sub": user, "aud": o.ClientID, "exp": now().Add(5 * time.Minute).Unix(),
		"iat": now().Unix(), "nonce": q.Get("nonce"), "https://slack.com/user_id": user, "https://slack.com/team_id": team}
	if enterprise != "" {
		claims["https://slack.com/enterprise_id"] = enterprise
	}
	o.mu.Lock()
	o.n++
	code := "oidc-code-" + strconv.Itoa(o.n)
	o.codes[code] = oidcCode{redirect: q.Get("redirect_uri"), claims: claims}
	o.mu.Unlock()
	back := url.Values{"code": {code}, "state": {q.Get("state")}}
	return q.Get("redirect_uri") + "?" + back.Encode(), nil
}

// token is openid.connect.token, each code once, for the app, to the
// redirect it was issued for; it hands oauth.v2.access to access.
func (o *OIDC) token(w http.ResponseWriter, r *http.Request) {
	fail := func(code string) { json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": code}) }
	switch r.URL.Path {
	case "/api/openid.connect.token":
	case "/api/oauth.v2.access":
		o.access(w, r)
		return
	default:
		http.NotFound(w, r)
		return
	}
	if id, secret, ok := r.BasicAuth(); !ok || id != o.ClientID || secret != o.ClientSecret {
		fail("invalid_client")
		return
	}
	r.ParseForm()
	o.mu.Lock()
	c, ok := o.codes[r.PostForm.Get("code")]
	delete(o.codes, r.PostForm.Get("code"))
	edit := o.Claims
	o.mu.Unlock()
	if !ok {
		fail("invalid_code")
		return
	}
	if c.redirect != r.PostForm.Get("redirect_uri") {
		fail("bad_redirect_uri")
		return
	}
	if edit != nil {
		edit(c.claims)
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	// Unsigned: devbot takes the token from the endpoint, not the browser.
	id := enc(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." + enc(c.claims) + ".c2ln"
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "access_token": "xoxp-oidc", "token_type": "Bearer", "id_token": id})
}

// Install plays someone approving an install at install, a URL to the
// install page, into team, and returns where Slack sends them back to:
// oauth.v2.access then answers its code, once, with token as the bot
// token.
func (o *OIDC) Install(authorize, team, token string) (string, error) {
	u, err := url.Parse(authorize)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if !strings.HasPrefix(authorize, o.InstallURL()+"?") || q.Get("client_id") != o.ClientID || q.Get("redirect_uri") == "" || q.Get("scope") == "" {
		return "", errors.New("slacktest: not an install request: " + authorize)
	}
	access := map[string]any{"ok": true, "access_token": token, "token_type": "bot", "scope": q.Get("scope"), "bot_user_id": "UDEVBOT", "app_id": "A1",
		"team": map[string]any{"id": team, "name": "Acme"}, "enterprise": nil, "is_enterprise_install": false, "authed_user": map[string]any{"id": "UINSTALLER"}}
	o.mu.Lock()
	o.n++
	code := "install-code-" + strconv.Itoa(o.n)
	o.installs[code] = install{redirect: q.Get("redirect_uri"), access: access}
	o.mu.Unlock()
	return q.Get("redirect_uri") + "?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode(), nil
}

// access is oauth.v2.access: each code once, for the app, to the redirect
// it was issued for.
func (o *OIDC) access(w http.ResponseWriter, r *http.Request) {
	fail := func(code string) { json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": code}) }
	if id, secret, ok := r.BasicAuth(); !ok || id != o.ClientID || secret != o.ClientSecret {
		fail("invalid_client")
		return
	}
	r.ParseForm()
	o.mu.Lock()
	in, ok := o.installs[r.PostForm.Get("code")]
	delete(o.installs, r.PostForm.Get("code"))
	o.mu.Unlock()
	switch {
	case !ok:
		fail("invalid_code")
	case in.redirect != r.PostForm.Get("redirect_uri"):
		fail("bad_redirect_uri")
	default:
		json.NewEncoder(w).Encode(in.access)
	}
}

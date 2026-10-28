package fake

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// The fake's OAuth app: a login page that is a function call (SignIn), codes
// bound to their PKCE challenge, and tokens that expire and rotate like a
// real forge's.
type oauthState struct {
	codes    map[string]pendingCode
	access   map[string]userTok // access token -> who and until when
	refresh  map[string]string  // refresh token -> login
	tokenTTL time.Duration
	failRef  bool
	outage   bool
	// refreshes counts OAuthRefresh calls.
	refreshes int
	// oneGrant is Gitea's one grant per user and app: every token issued
	// bumps the user's grant counter, and only a refresh token minted
	// under the current counter refreshes.
	oneGrant  bool
	counter   map[string]int // login -> grant counter
	rtCounter map[string]int // refresh token -> counter it was minted under

	refuseWrites bool
	// issuesReadOnly are logins with write on the repo but not on its issues.
	issuesReadOnly map[string]bool
	dropLabels     bool
}

type pendingCode struct {
	login, redirectURI, challenge string
}

type userTok struct {
	login  string
	expiry time.Time
}

func newOAuthState() oauthState {
	return oauthState{codes: map[string]pendingCode{}, access: map[string]userTok{}, refresh: map[string]string{}, tokenTTL: time.Hour,
		counter: map[string]int{}, rtCounter: map[string]int{},
		issuesReadOnly: map[string]bool{}}
}

func randToken(prefix string) string {
	b := make([]byte, 16)
	rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

const authorizeURL = "https://fake/login/oauth/authorize"

func (f *Forge) OAuthURL(redirectURI, state, challenge string) (string, error) {
	q := url.Values{"redirect_uri": {redirectURI}, "state": {state}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "response_type": {"code"}}
	return authorizeURL + "?" + q.Encode(), nil
}

// SignIn is login approving devbot's app on the page authURL points at. It
// returns where the forge redirects the browser: the callback, with a code.
func (f *Forge) SignIn(authURL, login string) (string, error) {
	u, err := url.Parse(authURL)
	if err != nil || u.Scheme+"://"+u.Host+u.Path != authorizeURL {
		return "", fmt.Errorf("fake: %q isn't the fake's login page", authURL)
	}
	q := u.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	code := randToken("code-")
	f.oauth.codes[code] = pendingCode{login: login, redirectURI: q.Get("redirect_uri"), challenge: q.Get("code_challenge")}
	return q.Get("redirect_uri") + "?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode(), nil
}

// SetTokenTTL is how long access tokens issued from now on last.
func (f *Forge) SetTokenTTL(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.oauth.tokenTTL = d
}

// FailRefresh makes every refresh fail, as when a person revokes the app.
func (f *Forge) FailRefresh(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.oauth.failRef = fail
}

// OneGrantPerUser gives the fake Gitea's OAuth grants (with
// INVALIDATE_REFRESH_TOKENS): one per user, whose counter every code
// exchange and refresh moves on, so each new token invalidates the refresh
// tokens issued before it.
func (f *Forge) OneGrantPerUser(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.oauth.oneGrant = on
}

// Refreshes is how many token refreshes the fake has been asked for.
func (f *Forge) Refreshes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.oauth.refreshes
}

// UserTokens returns every access and refresh token the fake has issued, for
// tests that check none of them leaks.
func (f *Forge) UserTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for t := range f.oauth.access {
		out = append(out, t)
	}
	for t := range f.oauth.refresh {
		out = append(out, t)
	}
	return out
}

func (f *Forge) issue(login string) forge.UserToken {
	t := forge.UserToken{Access: randToken("fake-at-"), Refresh: randToken("fake-rt-"), Expiry: f.now().Add(f.oauth.tokenTTL)}
	f.oauth.access[t.Access] = userTok{login: login, expiry: t.Expiry}
	f.oauth.refresh[t.Refresh] = login
	f.oauth.counter[login]++
	f.oauth.rtCounter[t.Refresh] = f.oauth.counter[login]
	return t
}

func (f *Forge) OAuthExchange(_ context.Context, redirectURI, code, verifier string) (forge.UserToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.oauth.codes[code]
	delete(f.oauth.codes, code)
	sum := sha256.Sum256([]byte(verifier))
	if !ok || p.redirectURI != redirectURI || base64.RawURLEncoding.EncodeToString(sum[:]) != p.challenge {
		return forge.UserToken{}, errors.New("fake: OAuth code exchange: invalid_grant")
	}
	return f.issue(p.login), nil
}

func (f *Forge) OAuthRefresh(_ context.Context, refresh string) (forge.UserToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.oauth.refreshes++
	if f.oauth.outage {
		return forge.UserToken{}, errors.New("fake: OAuth refresh: 503 Service Unavailable")
	}
	login, ok := f.oauth.refresh[refresh]
	if !ok || f.oauth.failRef {
		return forge.UserToken{}, fmt.Errorf("fake: OAuth refresh: invalid_grant: %w", forge.ErrUnauthorized)
	}
	if f.oauth.oneGrant && f.oauth.rtCounter[refresh] != f.oauth.counter[login] {
		return forge.UserToken{}, fmt.Errorf("fake: OAuth refresh: unauthorized_client: token was already used: %w", forge.ErrUnauthorized)
	}
	delete(f.oauth.refresh, refresh) // rotates, like Gitea
	return f.issue(login), nil
}

// whoami resolves a person's access token. Called with f.mu held.
func (f *Forge) whoami(token string) (string, error) {
	t, ok := f.oauth.access[token]
	if !ok || !f.now().Before(t.expiry) {
		return "", forge.ErrUnauthorized
	}
	return t.login, nil
}

func (f *Forge) CurrentUser(_ context.Context, token string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.whoami(token)
}

func (f *Forge) AsUser(token string) forge.User { return &user{f: f, tok: token} }

// user is the fake as one person. It enforces what a forge would: read to
// file or comment, write to label, and nothing at all on a repo the person
// can't see.
type user struct {
	f   *Forge
	tok string
}

func (u *user) as(r forge.RepoRef, need forge.Permission) (string, error) {
	login, err := u.f.whoami(u.tok)
	if err != nil {
		return "", err
	}
	if u.f.oauth.refuseWrites {
		return "", fmt.Errorf("fake: refused: %w", forge.ErrForbidden)
	}
	switch p := u.f.perm(r, login); {
	case p == forge.PermNone:
		return "", forge.ErrNotFound
	case p < need:
		return "", fmt.Errorf("fake: %s needs %s on %s: %w", login, need, r, forge.ErrForbidden)
	}
	return login, nil
}

func (u *user) Permission(_ context.Context, r forge.RepoRef) (forge.Permission, error) {
	u.f.mu.Lock()
	defer u.f.mu.Unlock()
	login, err := u.f.whoami(u.tok)
	if err != nil {
		return forge.PermNone, err
	}
	return u.f.perm(r, login), nil
}

func (u *user) CanEditIssues(_ context.Context, r forge.RepoRef) (bool, error) {
	u.f.mu.Lock()
	defer u.f.mu.Unlock()
	login, err := u.f.whoami(u.tok)
	if err != nil {
		return false, err
	}
	return u.f.perm(r, login) >= forge.PermWrite && !u.f.oauth.issuesReadOnly[login], nil
}

// CreateItem drops the labels of someone who may not set them, as Gitea
// does, rather than refusing.
func (u *user) CreateItem(_ context.Context, r forge.RepoRef, title, body string, labels []string) (forge.Item, error) {
	u.f.mu.Lock()
	defer u.f.mu.Unlock()
	login, err := u.as(r, forge.PermRead)
	if err != nil {
		return forge.Item{}, err
	}
	if u.f.perm(r, login) < forge.PermWrite || u.f.oauth.issuesReadOnly[login] || u.f.oauth.dropLabels {
		labels = nil
	}
	return u.f.createItem(r, login, title, body, labels), nil
}

// CloseItem lets a person close their own issue, or any with write.
func (u *user) CloseItem(_ context.Context, ref forge.ItemRef) error {
	u.f.mu.Lock()
	defer u.f.mu.Unlock()
	login, err := u.as(ref.Repo, forge.PermRead)
	if err != nil {
		return err
	}
	it, ok := u.f.Items[ref.String()]
	if !ok {
		return forge.ErrNotFound
	}
	if it.Author != login && u.f.perm(ref.Repo, login) < forge.PermWrite {
		return fmt.Errorf("fake: %s may not close %s: %w", login, ref, forge.ErrForbidden)
	}
	it.State = "closed"
	return nil
}

func (u *user) Comment(_ context.Context, ref forge.ItemRef, body string) error {
	u.f.mu.Lock()
	defer u.f.mu.Unlock()
	login, err := u.as(ref.Repo, forge.PermRead)
	if err != nil {
		return err
	}
	u.f.addComment(ref, login, body)
	return nil
}

func (u *user) SetLabels(_ context.Context, ref forge.ItemRef, add, remove []string) error {
	u.f.mu.Lock()
	defer u.f.mu.Unlock()
	login, err := u.as(ref.Repo, forge.PermWrite)
	if err != nil {
		return err
	}
	if u.f.oauth.issuesReadOnly[login] {
		return fmt.Errorf("fake: %s may not label issues on %s: %w", login, ref.Repo, forge.ErrForbidden)
	}
	return u.f.setLabels(ref, login, add, remove)
}

// IssuesReadOnly gives login write on repos but only read on their issues,
// as a Gitea team's per-unit permissions can.
func (f *Forge) IssuesReadOnly(login string, ro bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.oauth.issuesReadOnly[login] = ro
}

// DropUserLabels makes the forge drop every label a person sets when filing,
// even when it said they may label: a permission check that disagrees with
// what the create does.
func (f *Forge) DropUserLabels(drop bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.oauth.dropLabels = drop
}

// TokenFor signs login in without the browser round trip, for tests that
// start from a signed-in person.
func (f *Forge) TokenFor(login string) forge.UserToken {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issue(login)
}

// RefuseUserWrites makes the forge turn down every write a person makes, as
// when its own rules (a locked issue tracker, say) say no where devbot's
// check said yes.
func (f *Forge) RefuseUserWrites(refuse bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.oauth.refuseWrites = refuse
}

// RefreshOutage makes refreshes fail the way a forge that's down does: an
// error that isn't a refusal.
func (f *Forge) RefreshOutage(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.oauth.outage = down
}

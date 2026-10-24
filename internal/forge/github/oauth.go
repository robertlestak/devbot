package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// People sign in to devbot with a GitHub App's user access tokens, not an
// OAuth App's: such a token can only do what both the person and the App
// may, on the repos the App is installed on, where an OAuth App's repo
// scope would reach every private repo the person has. The App's
// permissions take the place of scopes, so devbot asks for none. With
// expiring tokens (the App default), a token lasts 8 hours and its
// refresh token 6 months, and a refresh rotates both (docs: "Generating a
// user access token for a GitHub App", "Refreshing user access tokens").
// The flow is the web flow, with PKCE (S256 only).

func (c *Client) signInApp() error {
	if c.cfg.OAuthClientID == "" || c.cfg.OAuthClientSecret == "" {
		return errors.New("github: no sign-in App configured for " + c.cfg.Name)
	}
	return nil
}

func (c *Client) OAuthURL(redirectURI, state, challenge string) (string, error) {
	if err := c.signInApp(); err != nil {
		return "", err
	}
	q := url.Values{
		"client_id": {c.cfg.OAuthClientID}, "redirect_uri": {redirectURI}, "state": {state},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}
	return c.web + "/login/oauth/authorize?" + q.Encode(), nil
}

func (c *Client) OAuthExchange(ctx context.Context, redirectURI, code, verifier string) (forge.UserToken, error) {
	return c.oauthToken(ctx, "code exchange", url.Values{"code": {code}, "redirect_uri": {redirectURI}, "code_verifier": {verifier}})
}

// OAuthRefresh returns the rotated pair: the refresh token just used no
// longer works. Callers must not refresh one person's token twice at once
// (internal/auth holds the person's lock), or the loser signs them out.
func (c *Client) OAuthRefresh(ctx context.Context, refresh string) (forge.UserToken, error) {
	return c.oauthToken(ctx, "refresh", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
}

// oauthToken calls the token endpoint on the web host. GitHub answers a
// refusal with an error code, often with a 200; any code is a definite no
// (forge.ErrUnauthorized), while a failed call or a 5xx may be an outage.
// Only the code goes into the error: the answer is not for logs.
func (c *Client) oauthToken(ctx context.Context, what string, form url.Values) (forge.UserToken, error) {
	if err := c.signInApp(); err != nil {
		return forge.UserToken{}, err
	}
	form.Set("client_id", c.cfg.OAuthClientID)
	form.Set("client_secret", c.cfg.OAuthClientSecret)
	req, err := http.NewRequestWithContext(ctx, "POST", c.web+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return forge.UserToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "devbot")
	resp, err := c.http.Do(req)
	if err != nil {
		return forge.UserToken{}, fmt.Errorf("github: OAuth %s failed", what)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken  string `json:"access_token"`
		ExpiresIn    int64  `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	decodeErr := json.Unmarshal(b, &out)
	switch {
	case out.Error != "":
		return forge.UserToken{}, fmt.Errorf("github: OAuth %s: %s: %w", what, out.Error, forge.ErrUnauthorized)
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized:
		return forge.UserToken{}, fmt.Errorf("github: OAuth %s: %s: %w", what, resp.Status, forge.ErrUnauthorized)
	case resp.StatusCode != http.StatusOK:
		return forge.UserToken{}, fmt.Errorf("github: OAuth %s: %s", what, resp.Status)
	case decodeErr != nil || out.AccessToken == "":
		return forge.UserToken{}, fmt.Errorf("github: OAuth %s: no token in the answer", what)
	}
	t := forge.UserToken{Access: out.AccessToken, Refresh: out.RefreshToken}
	// No expires_in: the App has expiring tokens turned off.
	if out.ExpiresIn > 0 {
		t.Expiry = c.now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	return t, nil
}

// userCred is a person's token. Their calls count against their own rate
// limit, so they get a bucket of their own, named by a hash of the token.
func userCred(token string) cred {
	sum := sha256.Sum256([]byte(token))
	return cred{bucket: "user " + hex.EncodeToString(sum[:6]), token: secret(token)}
}

func (c *Client) CurrentUser(ctx context.Context, token string) (string, error) {
	var u user
	if _, err := c.do(ctx, userCred(token), "GET", "/user", nil, &u); err != nil {
		return "", err
	}
	if u.Login == "" {
		return "", errors.New("github: /user returned no login")
	}
	return u.Login, nil
}

func (c *Client) AsUser(token string) forge.User { return &asUser{c: c, cr: userCred(token)} }

// asUser is GitHub as one signed-in person, through the sign-in App.
type asUser struct {
	c  *Client
	cr cred
}

type repoPerms struct {
	Admin    bool `json:"admin"`
	Maintain bool `json:"maintain"`
	Push     bool `json:"push"`
	Triage   bool `json:"triage"`
	Pull     bool `json:"pull"`
}

// perms is the person's role on r (docs: "Get a repository", permissions).
// GitHub hides a repo the sign-in App isn't installed on from the token as
// if it didn't exist, so that is no access too.
func (u *asUser) perms(ctx context.Context, r forge.RepoRef) (repoPerms, error) {
	var rp struct {
		Permissions repoPerms `json:"permissions"`
	}
	_, err := u.c.do(ctx, u.cr, "GET", repoPath(r), nil, &rp)
	if errors.Is(err, forge.ErrNotFound) || errors.Is(err, forge.ErrForbidden) {
		return repoPerms{}, nil
	}
	return rp.Permissions, err
}

// Permission maps GitHub's roles onto devbot's: triage, which may manage
// issues but not push, is read.
func (u *asUser) Permission(ctx context.Context, r forge.RepoRef) (forge.Permission, error) {
	p, err := u.perms(ctx, r)
	switch {
	case err != nil:
		return forge.PermNone, err
	case p.Admin:
		return forge.PermAdmin, nil
	case p.Maintain:
		return forge.PermMaintain, nil
	case p.Push:
		return forge.PermWrite, nil
	case p.Triage, p.Pull:
		return forge.PermRead, nil
	}
	return forge.PermNone, nil
}

// CanEditIssues is triage or higher, the roles GitHub lets label issues.
func (u *asUser) CanEditIssues(ctx context.Context, r forge.RepoRef) (bool, error) {
	p, err := u.perms(ctx, r)
	return p.Triage || p.Push || p.Maintain || p.Admin, err
}

// GitHub drops the labels of someone below triage at create instead of
// refusing.
func (u *asUser) CreateItem(ctx context.Context, r forge.RepoRef, title, body string, labels []string) (forge.Item, error) {
	it, err := u.c.createItem(ctx, u.cr, r, title, body, labels)
	return it, explain(r, err)
}

func (u *asUser) CloseItem(ctx context.Context, ref forge.ItemRef) error {
	return explain(ref.Repo, u.c.closeItem(ctx, u.cr, ref))
}

func (u *asUser) Comment(ctx context.Context, ref forge.ItemRef, body string) error {
	return explain(ref.Repo, u.c.comment(ctx, u.cr, ref, body))
}

// SetLabels needs the person's login: a replace's own removals show in the
// issue's events as theirs, and are told apart by it.
func (u *asUser) SetLabels(ctx context.Context, ref forge.ItemRef, add, remove []string) error {
	var me user
	if _, err := u.c.do(ctx, u.cr, "GET", "/user", nil, &me); err != nil {
		return err
	}
	return explain(ref.Repo, u.c.setLabels(ctx, u.cr, me.Login, ref, add, remove))
}

// explain words the refusals a person's token gets because of the sign-in
// App rather than the person: GitHub hides a repo the App isn't installed
// on, and refuses what the App's permissions don't cover.
func explain(r forge.RepoRef, err error) error {
	var ae *apiError
	switch {
	case errors.Is(err, forge.ErrNotFound):
		return fmt.Errorf("github: %s, or what you asked for on it, isn't visible to you through devbot: either you can't see it, or devbot's sign-in GitHub App isn't installed on it: %w", r, err)
	case errors.As(err, &ae) && ae.Status == http.StatusForbidden && strings.Contains(ae.Body, "not accessible by integration"):
		return fmt.Errorf("github: devbot's sign-in GitHub App isn't allowed to do that on %s (it needs Issues read and write there): %w", r, err)
	}
	return err
}

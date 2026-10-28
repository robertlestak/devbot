package gitea

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"golang.org/x/oauth2"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// OAuthScopes is what devbot asks Gitea for when a person signs in: who they
// are (read:user), their access to a repo for filtering reads
// (read:repository), and filing issues, commenting and labelling as them
// (write:issue). Gitea 28 limits the token to exactly these (verified on
// 28.1.0; docs/operating.md).
var OAuthScopes = []string{"read:user", "read:repository", "write:issue"}

func (c *Client) oauth(redirectURI string) (*oauth2.Config, error) {
	if c.cfg.OAuthClientID == "" {
		return nil, errors.New("gitea: no OAuth app configured for " + c.cfg.Name)
	}
	return &oauth2.Config{
		ClientID: c.cfg.OAuthClientID, ClientSecret: c.cfg.OAuthClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL: c.cfg.URL + "/login/oauth/authorize", TokenURL: c.cfg.URL + "/login/oauth/access_token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
		RedirectURL: redirectURI, Scopes: OAuthScopes,
	}, nil
}

func (c *Client) OAuthURL(redirectURI, state, challenge string) (string, error) {
	cfg, err := c.oauth(redirectURI)
	if err != nil {
		return "", err
	}
	return cfg.AuthCodeURL(state, oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256")), nil
}

func (c *Client) OAuthExchange(ctx context.Context, redirectURI, code, verifier string) (forge.UserToken, error) {
	cfg, err := c.oauth(redirectURI)
	if err != nil {
		return forge.UserToken{}, err
	}
	t, err := cfg.Exchange(context.WithValue(ctx, oauth2.HTTPClient, c.http), code, oauth2.VerifierOption(verifier))
	if err != nil {
		return forge.UserToken{}, oauthErr("code exchange", err)
	}
	return forge.UserToken{Access: t.AccessToken, Refresh: t.RefreshToken, Expiry: t.Expiry}, nil
}

func (c *Client) OAuthRefresh(ctx context.Context, refresh string) (forge.UserToken, error) {
	cfg, err := c.oauth("")
	if err != nil {
		return forge.UserToken{}, err
	}
	t, err := cfg.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, c.http), &oauth2.Token{RefreshToken: refresh}).Token()
	if err != nil {
		return forge.UserToken{}, oauthErr("refresh", err)
	}
	return forge.UserToken{Access: t.AccessToken, Refresh: t.RefreshToken, Expiry: t.Expiry}, nil
}

// oauthErr keeps only the OAuth error code: a token endpoint's response body
// is not something to put in an error that may be logged. Gitea turning the
// grant down is forge.ErrUnauthorized; anything else may be an outage.
func oauthErr(what string, err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		code := re.ErrorCode
		if code == "" && re.Response != nil {
			code = re.Response.Status
		}
		switch {
		case re.ErrorCode == "invalid_grant" || re.ErrorCode == "unauthorized_client" || re.ErrorCode == "invalid_client",
			re.Response != nil && (re.Response.StatusCode == 400 || re.Response.StatusCode == 401):
			return fmt.Errorf("gitea: OAuth %s: %s: %w", what, code, forge.ErrUnauthorized)
		}
		return fmt.Errorf("gitea: OAuth %s: %s", what, code)
	}
	return fmt.Errorf("gitea: OAuth %s failed", what)
}

func (c *Client) CurrentUser(ctx context.Context, token string) (string, error) {
	var u user
	if err := c.doToken(ctx, token, "GET", "/user", nil, &u); err != nil {
		return "", err
	}
	if u.Login == "" {
		return "", errors.New("gitea: /user returned no login")
	}
	return u.Login, nil
}

func (c *Client) AsUser(token string) forge.User { return &userClient{c: c, tok: token} }

// userClient is Gitea as one signed-in person. Label IDs still come from the
// implementer's cache: they are the repo's, not anyone's.
type userClient struct {
	c   *Client
	tok string
}

func (u *userClient) Permission(ctx context.Context, r forge.RepoRef) (forge.Permission, error) {
	var rp struct {
		Permissions struct {
			Admin bool `json:"admin"`
			Push  bool `json:"push"`
			Pull  bool `json:"pull"`
		} `json:"permissions"`
	}
	err := u.c.doToken(ctx, u.tok, "GET", repoPath(r), nil, &rp)
	switch {
	case errors.Is(err, forge.ErrNotFound) || errors.Is(err, forge.ErrForbidden):
		return forge.PermNone, nil
	case err != nil:
		return forge.PermNone, err
	case rp.Permissions.Admin:
		return forge.PermAdmin, nil
	case rp.Permissions.Push:
		return forge.PermWrite, nil
	case rp.Permissions.Pull:
		return forge.PermRead, nil
	}
	return forge.PermNone, nil
}

func (u *userClient) CreateItem(ctx context.Context, r forge.RepoRef, title, body string, labels []string) (forge.Item, error) {
	return u.c.createItem(ctx, u.tok, r, title, body, labels)
}

func (u *userClient) Comment(ctx context.Context, ref forge.ItemRef, body string) error {
	return u.c.comment(ctx, u.tok, ref, body)
}

func (u *userClient) SetLabels(ctx context.Context, ref forge.ItemRef, add, remove []string) error {
	return u.c.setLabels(ctx, u.tok, ref, add, remove)
}

// CanEditIssues asks Gitea as the person, which has no API for a user's
// per-unit permission: editing label 0, which never exists, is refused with
// a 403 by the issue-or-PR writer check before Gitea looks the label up, and
// is a 404 for anyone who passes. Nothing is written either way. Someone who
// may write PRs but not issues still passes; FileTask catches that from the
// labels on the issue it gets back.
func (u *userClient) CanEditIssues(ctx context.Context, r forge.RepoRef) (bool, error) {
	err := u.c.doToken(ctx, u.tok, "PATCH", repoPath(r)+"/labels/0", map[string]any{}, nil)
	var ae *apiError
	switch {
	case err == nil, errors.Is(err, forge.ErrNotFound), errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity:
		return true, nil
	case errors.Is(err, forge.ErrForbidden):
		return false, nil
	}
	return false, err
}

func (u *userClient) CloseItem(ctx context.Context, ref forge.ItemRef) error {
	return u.c.closeItem(ctx, u.tok, ref)
}

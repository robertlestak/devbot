package slack

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/auth"
)

// Sign in with Slack (OpenID Connect) proves who opened a link to a Slack
// user: the App is the auth.Prover of "slack" subjects. A person's own
// Slack sign-in, in the browser that opened the link, is the only thing
// that shows the link reached the Slack user it was made for and not
// someone they sent it to.

// OIDCScopes are the user scopes Sign in with Slack asks for: only who the
// person is.
var OIDCScopes = []string{"openid"}

// oidcIssuer is the iss of Slack's ID tokens.
const oidcIssuer = "https://slack.com"

var _ auth.Prover = (*App)(nil)

// Subject is what auth links a Slack user as: one per user and workspace,
// within its Enterprise Grid org when there is one.
func Subject(ws Workspace, user string) string { return "slack:" + ws.key() + ":" + user }

// parseSubject is Subject's workspace and user.
func parseSubject(s string) (Workspace, string, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 4 || parts[0] != "slack" || parts[3] == "" || parts[1]+parts[2] == "" {
		return Workspace{}, "", false
	}
	return Workspace{Enterprise: parts[1], Team: parts[2]}, parts[3], true
}

// ProveURL sends the person to sign in with Slack, in l's workspace.
func (a *App) ProveURL(l auth.Link, redirect, state, nonce string) (string, error) {
	ws, _, ok := parseSubject(l.Subject)
	if !ok {
		return "", fmt.Errorf("slack: %q isn't a Slack user's link", l.Subject)
	}
	q := url.Values{"response_type": {"code"}, "scope": {strings.Join(OIDCScopes, " ")}, "client_id": {a.o.ClientID},
		"state": {state}, "nonce": {nonce}, "redirect_uri": {redirect}}
	if ws.Team != "" {
		// Signs them straight into the workspace the link is for.
		q.Set("team", ws.Team)
	}
	return a.o.OIDCURL + "?" + q.Encode(), nil
}

// idClaims are the claims of Slack's ID token that devbot reads.
type idClaims struct {
	Iss        string   `json:"iss"`
	Aud        audience `json:"aud"`
	Exp        int64    `json:"exp"`
	Nonce      string   `json:"nonce"`
	User       string   `json:"https://slack.com/user_id"`
	Team       string   `json:"https://slack.com/team_id"`
	Enterprise string   `json:"https://slack.com/enterprise_id"`
}

// audience is an ID token's aud: one string, or a list of them.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	return json.Unmarshal(b, (*[]string)(a))
}

// Proved exchanges code for the person's ID token, and returns the Slack
// user it names as a subject comparable with l's. The token comes straight
// from Slack's token endpoint over TLS, authenticated by the app's secret,
// which OpenID Connect accepts in place of checking its signature; its
// issuer, audience, expiry and nonce are checked all the same.
func (a *App) Proved(ctx context.Context, l auth.Link, code, redirect, nonce string) (string, error) {
	ws, _, ok := parseSubject(l.Subject)
	if !ok {
		return "", fmt.Errorf("slack: %q isn't a Slack user's link", l.Subject)
	}
	if code == "" {
		return "", errors.New("slack: no code to sign in with")
	}
	var resp struct {
		IDToken string `json:"id_token"`
	}
	client := &Client{URL: a.o.APIURL, HTTP: a.o.HTTP}
	if err := client.callBasic(ctx, "openid.connect.token", a.o.ClientID, a.o.ClientSecret, url.Values{"code": {code}, "redirect_uri": {redirect}}, &resp); err != nil {
		return "", err
	}
	parts := strings.Split(resp.IDToken, ".")
	if len(parts) != 3 {
		return "", errors.New("slack: openid.connect.token returned no ID token")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("slack: ID token: %w", err)
	}
	var c idClaims
	if err := json.Unmarshal(b, &c); err != nil {
		return "", fmt.Errorf("slack: ID token: %w", err)
	}
	switch {
	case c.Iss != oidcIssuer:
		return "", fmt.Errorf("slack: ID token issued by %q", c.Iss)
	case len(c.Aud) != 1 || c.Aud[0] != a.o.ClientID:
		return "", errors.New("slack: ID token for another app")
	case !a.o.Now().Before(time.Unix(c.Exp, 0)):
		return "", errors.New("slack: ID token expired")
	case nonce == "" || subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(nonce)) != 1:
		return "", errors.New("slack: ID token for another sign-in (nonce)")
	case c.User == "" || c.Team == "":
		return "", errors.New("slack: ID token names no user")
	}
	got := Workspace{Enterprise: c.Enterprise, Team: c.Team}
	if ws.Team == "" && c.Enterprise != "" {
		// An org-wide install's users are linked by org: whichever of its
		// workspaces they signed in to is theirs.
		got.Team = ""
	}
	return Subject(got, c.User), nil
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// Headless sign-in for an install in auth.mode oauth. `devbot login` runs the
// device flow once, as the person the caller will act for, and keeps devbot's
// tokens (never a forge token) in a file only its owner can read. The client
// commands then refresh them as needed; `devbot token` prints a fresh access
// token for scripts that call the HTTP API themselves.

type savedLogin struct {
	URL      string        `json:"url"`
	ClientID string        `json:"client_id"`
	Token    *oauth2.Token `json:"token"`
}

func tokenFile() (string, error) {
	if p := os.Getenv("DEVBOT_TOKEN_FILE"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "devbot", "token.json"), nil
}

func serverURL() string { return strings.TrimRight(envOr("DEVBOT_URL", "http://localhost:8080"), "/") }

// oauthConfig discovers devbot's endpoints, registering this CLI as a client
// when it has no client ID yet. It also returns the revocation endpoint.
func oauthConfig(ctx context.Context, base, clientID string) (*oauth2.Config, string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/.well-known/oauth-authorization-server", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	var m struct {
		Token  string `json:"token_endpoint"`
		Device string `json:"device_authorization_endpoint"`
		Reg    string `json:"registration_endpoint"`
		Revoke string `json:"revocation_endpoint"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&m) != nil || m.Device == "" {
		return nil, "", fmt.Errorf("%s doesn't offer device sign-in (is auth.mode oauth, with auth.dynamic_device_grant on?)", base)
	}
	if clientID == "" {
		if clientID, err = registerCLI(ctx, m.Reg); err != nil {
			return nil, "", err
		}
	}
	return &oauth2.Config{ClientID: clientID, Endpoint: oauth2.Endpoint{
		TokenURL: m.Token, DeviceAuthURL: m.Device, AuthStyle: oauth2.AuthStyleInParams,
	}}, m.Revoke, nil
}

func registerCLI(ctx context.Context, endpoint string) (string, error) {
	host, _ := os.Hostname()
	body, _ := json.Marshal(map[string]any{
		"client_name": "devbot CLI on " + host, "token_endpoint_auth_method": "none",
		"grant_types": []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token"},
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		ClientID string `json:"client_id"`
	}
	if resp.StatusCode != http.StatusCreated || json.NewDecoder(resp.Body).Decode(&out) != nil || out.ClientID == "" {
		return "", fmt.Errorf("registering with devbot failed: %s", resp.Status)
	}
	return out.ClientID, nil
}

func login(ctx context.Context) error {
	base := serverURL()
	cfg, _, err := oauthConfig(ctx, base, "")
	if err != nil {
		return err
	}
	res := oauth2.SetAuthURLParam("resource", base)
	da, err := cfg.DeviceAuth(ctx, res)
	if err != nil {
		return fmt.Errorf("starting sign-in: %w", err)
	}
	fmt.Fprintf(os.Stderr, "To sign in, open\n\n  %s\n\nand enter the code %s. You'll sign in to the forge as the person this machine will act for.\n", da.VerificationURI, da.UserCode)
	tok, err := cfg.DeviceAccessToken(ctx, da, res)
	if err != nil {
		return fmt.Errorf("sign-in: %w", err)
	}
	if err := saveLogin(savedLogin{URL: base, ClientID: cfg.ClientID, Token: tok}); err != nil {
		return err
	}
	p, _ := tokenFile()
	fmt.Fprintf(os.Stderr, "Signed in. devbot's tokens are in %s.\n", p)
	return nil
}

func saveLogin(l savedLogin) error {
	p, err := tokenFile()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(l, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func loadLogin() (*savedLogin, error) {
	p, err := tokenFile()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var l savedLogin
	if err := json.Unmarshal(b, &l); err != nil || l.Token == nil {
		return nil, fmt.Errorf("%s: unreadable; run devbot login again", p)
	}
	return &l, nil
}

// accessToken is the token the client commands send: $DEVBOT_TOKEN, or the
// saved login's, refreshed (and the refresh token rotated) when it expires.
func accessToken(ctx context.Context) (string, error) {
	if t := os.Getenv("DEVBOT_TOKEN"); t != "" {
		return t, nil
	}
	l, err := loadLogin()
	if err != nil || l == nil || l.URL != serverURL() {
		return "", err
	}
	if l.Token.Valid() && time.Until(l.Token.Expiry) > time.Minute {
		return l.Token.AccessToken, nil
	}
	cfg, _, err := oauthConfig(ctx, l.URL, l.ClientID)
	if err != nil {
		return "", err
	}
	l.Token.Expiry = time.Now() // refresh now rather than at the last second
	tok, err := cfg.TokenSource(ctx, l.Token).Token()
	if err != nil {
		return "", fmt.Errorf("refreshing devbot's token failed (%v); run devbot login again", err)
	}
	l.Token = tok
	return tok.AccessToken, saveLogin(*l)
}

func printToken(ctx context.Context) error {
	t, err := accessToken(ctx)
	if err == nil && t == "" {
		err = errors.New("not signed in: run devbot login")
	}
	if err != nil {
		return err
	}
	fmt.Println(t)
	return nil
}

func logout(ctx context.Context) error {
	l, err := loadLogin()
	if err != nil || l == nil {
		return err
	}
	// Revoking ends the grant on devbot's side, which deletes the forge token
	// it holds for this sign-in.
	if _, rev, err := oauthConfig(ctx, l.URL, l.ClientID); err == nil && rev != "" {
		form := url.Values{"token": {l.Token.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {l.ClientID}}
		req, _ := http.NewRequestWithContext(ctx, "POST", rev, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	p, _ := tokenFile()
	return os.Remove(p)
}

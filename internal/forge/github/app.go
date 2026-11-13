package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// refreshMargin is how long before an installation token expires that a
// new one is minted, so a token handed out has time to be used.
const refreshMargin = 5 * time.Minute

// app is one bot identity: a GitHub App, acting in each repo through the
// installation that covers it. The two Apps share nothing, so the reviewer
// never acts with the implementer's token or the other way round.
type app struct {
	role forge.Role
	id   int64
	// key is nil in a process that never acts as this App (a job pod of the
	// other role), which knows only its login.
	key   *rsa.PrivateKey
	login string // "<slug>[bot]", from GET /app

	mu       sync.Mutex
	installs map[string]installation // "owner/name" -> its installation
	tokens   map[tokenKey]*mint      // the latest token for each

	// inv is what the App is installed on, for reconcile's searches.
	inv inventory
}

// tokenKey is what a token is for: one repo of an installation, or, with
// repo empty, listing the installation's repos, or, with repo searchScope,
// searching its issues and PRs.
type tokenKey struct {
	install int64
	repo    string
}

// searchScope is the repo of a search token's tokenKey: no repo's name has
// a slash.
const searchScope = "/search"

// mint is one installation token, or the request for it while in flight:
// everyone who needs that installation's token meanwhile waits on done
// rather than mint another.
type mint struct {
	done    chan struct{}
	token   secret
	perms   map[string]string // what GitHub says the token may do
	expires time.Time
	err     error
}

// newApp loads an App's key. An App given a login and no key is one this
// process never acts as; only the key could read its login.
func newApp(role forge.Role, id int64, pemKey []byte, login string) (*app, error) {
	if id <= 0 {
		return nil, fmt.Errorf("github: the %s App needs an App ID", role)
	}
	a := &app{role: role, id: id, login: login, installs: map[string]installation{}, tokens: map[tokenKey]*mint{}}
	if len(pemKey) == 0 && login != "" {
		return a, nil
	}
	key, err := parseKey(pemKey)
	if err != nil {
		return nil, fmt.Errorf("github: the %s App's (%d) private key: %w", role, id, err)
	}
	a.key = key
	return a, nil
}

// parseKey reads an App's private key: PKCS#1, as GitHub generates it,
// or PKCS#8. Its errors never quote the key.
func parseKey(b []byte) (*rsa.PrivateKey, error) {
	if len(b) == 0 {
		return nil, errors.New("empty")
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("not PEM")
	}
	switch blk.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(blk.Bytes)
		if err != nil {
			return nil, errors.New("not a valid PKCS#1 RSA key")
		}
		return k, nil
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, errors.New("not a valid PKCS#8 key")
		}
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, errors.New("not an RSA key")
	}
	return nil, fmt.Errorf("PEM block %q, want an RSA private key", blk.Type)
}

// jwt is the App's own credential (docs: "Generating a JSON Web Token
// (JWT) for a GitHub App"): RS256, issued a minute in the past against
// clock drift, and expiring within GitHub's ten minute limit.
func (a *app) jwt(now time.Time) (cred, error) {
	enc := base64.RawURLEncoding
	claims, _ := json.Marshal(map[string]int64{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.id,
	})
	signing := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return cred{}, fmt.Errorf("github: sign the %s App's JWT: %w", a.role, err)
	}
	return cred{bucket: "app " + strconv.FormatInt(a.id, 10), token: secret(signing + "." + enc.EncodeToString(sig))}, nil
}

func (c *Client) appCred(a *app) (cred, error) {
	if a.key == nil {
		return cred{}, fmt.Errorf("github: the %s App: %w", a.role, forge.ErrNoCredential)
	}
	return a.jwt(c.now())
}

// identify reads the App's slug, which makes its login.
func (c *Client) identify(ctx context.Context, a *app) error {
	cr, err := c.appCred(a)
	if err != nil {
		return err
	}
	var out struct {
		Slug string `json:"slug"`
	}
	if _, err := c.do(ctx, cr, "GET", "/app", nil, &out); err != nil {
		return fmt.Errorf("github: the %s App (%d): GET /app: %w", a.role, a.id, err)
	}
	if out.Slug == "" {
		return fmt.Errorf("github: the %s App (%d) has no slug", a.role, a.id)
	}
	a.login = out.Slug + "[bot]"
	return nil
}

// installation is the App's installation on a repo.
type installation struct {
	ID          int64             `json:"id"`
	Permissions map[string]string `json:"permissions"`
}

// installation asks GitHub, as the App, for its installation on r, and
// remembers which it is. ErrNotFound: the App isn't installed there.
func (c *Client) installation(ctx context.Context, a *app, r forge.RepoRef) (installation, error) {
	cr, err := c.appCred(a)
	if err != nil {
		return installation{}, err
	}
	var in installation
	if _, err := c.do(ctx, cr, "GET", repoPath(r)+"/installation", nil, &in); err != nil {
		if errors.Is(err, forge.ErrNotFound) {
			a.mu.Lock()
			delete(a.installs, r.FullName())
			a.mu.Unlock()
		}
		return installation{}, err
	}
	a.mu.Lock()
	a.installs[r.FullName()] = in
	a.mu.Unlock()
	return in, nil
}

// repoCred is the App's installation token for r, and for r alone.
func (c *Client) repoCred(ctx context.Context, role forge.Role, r forge.RepoRef) (cred, error) {
	a := c.app(role)
	if a.key == nil {
		return cred{}, fmt.Errorf("github: the %s App: %w", role, forge.ErrNoCredential)
	}
	for try := 0; ; try++ {
		a.mu.Lock()
		in, ok := a.installs[r.FullName()]
		a.mu.Unlock()
		// The token asks for no more than the installation was granted, so
		// one found by listing needs its permissions read first.
		if !ok || in.Permissions == nil {
			var err error
			if in, err = c.installation(ctx, a, r); err != nil {
				return cred{}, err
			}
		}
		tok, perms, err := c.installToken(ctx, a, in, r.Name)
		// An installation that's gone (the App was uninstalled, or
		// reinstalled under a new ID) is looked up again, once.
		if errors.Is(err, forge.ErrNotFound) && ok && try == 0 {
			a.mu.Lock()
			delete(a.installs, r.FullName())
			a.mu.Unlock()
			continue
		}
		if err != nil {
			return cred{}, err
		}
		return cred{bucket: "installation " + strconv.FormatInt(in.ID, 10), token: tok, perms: perms}, nil
	}
}

// installToken is a token for repo, a repo of installation in, or with repo
// empty for listing in's repos, and its permissions: from the cache while it
// has more than refreshMargin left, else freshly minted. Concurrent callers
// share one mint. The mint isn't tied to any one caller's context, so a
// caller that gives up doesn't fail the others.
func (c *Client) installToken(ctx context.Context, a *app, in installation, repo string) (secret, map[string]string, error) {
	k := tokenKey{in.ID, repo}
	a.mu.Lock()
	m := a.tokens[k]
	if m != nil {
		select {
		case <-m.done:
			if m.err == nil && c.now().Before(m.expires.Add(-refreshMargin)) {
				a.mu.Unlock()
				return m.token, m.perms, nil
			}
			m = nil
		default: // in flight
		}
	}
	if m == nil {
		m = &mint{done: make(chan struct{})}
		a.tokens[k] = m
		go func() {
			mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.maxWait+time.Minute)
			defer cancel()
			m.token, m.perms, m.expires, m.err = c.mintToken(mctx, a, in, repo)
			close(m.done)
		}()
	}
	a.mu.Unlock()
	select {
	case <-m.done:
		return m.token, m.perms, m.err
	case <-ctx.Done():
		return "", nil, ctx.Err()
	}
}

// rolePermissions is the most a role's tokens ask for: what devbot does as
// that bot. A token gets the lesser of this and what the installation was
// granted, so a permission left off the App (administration, actions,
// workflows and statuses write are optional) is simply not asked for.
var rolePermissions = map[forge.Role]map[string]string{
	// GitHub refuses a push that changes .github/workflows from an App token
	// without workflows write. Whether the work may change them at all is
	// the repo's protected_paths, which keep them out by default.
	forge.RoleImplementer: {"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read",
		"checks": "read", "statuses": "write", "administration": "read", "actions": "write", "workflows": "write"},
	// A review pod has no implementer credential: the reviewer reads the
	// task and its CI itself, and records its own failures on the issue.
	// It never pushes, so it never asks for workflows.
	forge.RoleReviewer: {"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read",
		"checks": "read", "statuses": "read", "actions": "read"},
}

var accessLevel = map[string]int{"read": 1, "write": 2, "admin": 3}

// tokenPermissions is what a token for role asks for, given what the
// installation was granted.
func tokenPermissions(role forge.Role, granted map[string]string) map[string]string {
	return grantable(rolePermissions[role], granted)
}

// searchPermissions are a search token's: reading what reconcile looks for,
// and nothing it could change.
var searchPermissions = map[string]string{"issues": "read", "pull_requests": "read", "metadata": "read"}

// grantable is the lesser of want and granted, for each permission in both.
func grantable(want, granted map[string]string) map[string]string {
	out := map[string]string{}
	for p, w := range want {
		g, ok := accessLevel[granted[p]]
		if !ok {
			continue
		}
		if g < accessLevel[w] {
			w = granted[p]
		}
		out[p] = w
	}
	return out
}

// mintToken asks for a new installation token (docs: "Authenticating as a
// GitHub App installation"), restricted to the one repo it is for and to
// the role's permissions, so a token that leaks from a job reaches nothing
// else the App is installed on. A token for listing the installation's
// repos gets metadata read alone, and one for searching them (searchScope)
// read access to issues and PRs alone.
func (c *Client) mintToken(ctx context.Context, a *app, in installation, repo string) (secret, map[string]string, time.Time, error) {
	cr, err := c.appCred(a)
	if err != nil {
		return "", nil, time.Time{}, err
	}
	var want map[string]string
	body := map[string]any{}
	switch repo {
	case "":
		want = map[string]string{"metadata": "read"}
	case searchScope:
		want = grantable(searchPermissions, in.Permissions)
	default:
		want = tokenPermissions(a.role, in.Permissions)
		body["repositories"] = []string{repo}
	}
	body["permissions"] = want
	var out struct {
		Token       string            `json:"token"`
		ExpiresAt   time.Time         `json:"expires_at"`
		Permissions map[string]string `json:"permissions"`
	}
	if _, err := c.do(ctx, cr, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", in.ID), body, &out); err != nil {
		return "", nil, time.Time{}, fmt.Errorf("github: the %s App's token for installation %d: %w", a.role, in.ID, err)
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return "", nil, time.Time{}, fmt.Errorf("github: the %s App's token for installation %d: no token in the answer", a.role, in.ID)
	}
	// GitHub answers with what the token got. A mint that succeeds got what
	// it asked for, so an answer without them is read as that.
	if out.Permissions == nil {
		out.Permissions = want
	}
	return secret(out.Token), out.Permissions, out.ExpiresAt, nil
}

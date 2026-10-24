package github

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"git.shdw.tech/rob/devbot/internal/forge"
)

var appRepo = forge.RepoRef{Provider: "github", Owner: "acme", Name: "app"}

// The web flow, on GitHub.com and on GHES, whose login pages are on the
// host rather than under /api/v3: PKCE with S256, no scopes, and a token
// that lasts 8 hours with a refresh token.
func TestSignIn(t *testing.T) {
	for _, prefix := range []string{"", "/api/v3"} {
		f := newFakeGitHub(t, prefix)
		c := f.client()
		verifier := oauth2.GenerateVerifier()
		u, err := c.OAuthURL("https://devbot.example/oauth/callback", "st4te", oauth2.S256ChallengeFromVerifier(verifier))
		if err != nil {
			t.Fatal(err)
		}
		p, _ := url.Parse(u)
		q := p.Query()
		if p.Path != "/login/oauth/authorize" || q.Get("client_id") != implClientID || q.Get("state") != "st4te" ||
			q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != oauth2.S256ChallengeFromVerifier(verifier) ||
			q.Has("scope") || q.Get("redirect_uri") != "https://devbot.example/oauth/callback" {
			t.Fatalf("%s: authorize URL %s", prefix, u)
		}
		code := f.authorize(u, "alice")
		tok, err := c.OAuthExchange(context.Background(), "https://devbot.example/oauth/callback", code, verifier)
		if err != nil {
			t.Fatal(err)
		}
		if tok.Access == "" || tok.Refresh == "" || time.Until(tok.Expiry) < 7*time.Hour || time.Until(tok.Expiry) > 8*time.Hour {
			t.Fatalf("token %+v", tok)
		}
		if login, err := c.CurrentUser(context.Background(), tok.Access); login != "alice" || err != nil {
			t.Fatalf("CurrentUser: %q, %v", login, err)
		}
	}
}

// A code is good once, and only with the verifier its challenge came from.
// GitHub's refusal comes as a 200 with an error code; only the code reaches
// the error.
func TestSignInRefusals(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	ctx, cb := context.Background(), "https://devbot.example/oauth/callback"
	verifier := oauth2.GenerateVerifier()
	u, _ := c.OAuthURL(cb, "s", oauth2.S256ChallengeFromVerifier(verifier))
	code := f.authorize(u, "alice")
	_, err := c.OAuthExchange(ctx, cb, code, oauth2.GenerateVerifier())
	if !errors.Is(err, forge.ErrUnauthorized) || !strings.Contains(err.Error(), "bad_verification_code") || strings.Contains(err.Error(), "secret detail") {
		t.Fatalf("wrong verifier: %v", err)
	}
	if _, err := c.OAuthExchange(ctx, cb, code, verifier); !errors.Is(err, forge.ErrUnauthorized) {
		t.Fatalf("a used code: %v", err)
	}

	cfg := f.config()
	cfg.OAuthClientSecret = "wrong"
	wrong, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.OAuthRefresh(ctx, "ghr_x"); !errors.Is(err, forge.ErrUnauthorized) || !strings.Contains(err.Error(), "incorrect_client_credentials") {
		t.Fatalf("wrong client secret: %v", err)
	}

	cfg.OAuthClientID, cfg.OAuthClientSecret = "", ""
	none, _ := New(ctx, cfg)
	if _, err := none.OAuthURL(cb, "s", "c"); err == nil || !strings.Contains(err.Error(), "no sign-in App") {
		t.Fatalf("no sign-in App: %v", err)
	}
}

// A refresh rotates the pair: the new refresh token works, the used one is
// refused (sign in again), and an outage isn't a refusal.
func TestRefreshRotates(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	ctx := context.Background()
	f.mu.Lock()
	first := f.issueUserToken("alice", implAppID)
	f.mu.Unlock()
	old := first["refresh_token"].(string)
	tok, err := c.OAuthRefresh(ctx, old)
	if err != nil || tok.Refresh == "" || tok.Refresh == old || tok.Access == first["access_token"] || tok.Expiry.IsZero() {
		t.Fatalf("refresh: %+v, %v", tok, err)
	}
	if login, err := c.CurrentUser(ctx, tok.Access); login != "alice" || err != nil {
		t.Fatalf("the refreshed token: %q, %v", login, err)
	}
	if _, err := c.OAuthRefresh(ctx, old); !errors.Is(err, forge.ErrUnauthorized) || !strings.Contains(err.Error(), "bad_refresh_token") {
		t.Fatalf("the used refresh token: %v", err)
	}
	f.mu.Lock()
	f.oauth.down = true
	f.mu.Unlock()
	if _, err := c.OAuthRefresh(ctx, tok.Refresh); err == nil || errors.Is(err, forge.ErrUnauthorized) {
		t.Fatalf("an outage must not read as a refusal: %v", err)
	}
	f.mu.Lock()
	f.oauth.down = false
	f.mu.Unlock()
	if _, err := c.OAuthRefresh(ctx, tok.Refresh); err != nil {
		t.Fatalf("after the outage: %v", err)
	}
}

// Concurrent refreshes of one refresh token are what devbot's per-person
// lock prevents: GitHub honours the first and refuses the rest, so an
// unserialized second caller would sign the person out.
func TestConcurrentRefreshesOfOneTokenLoseAllButOne(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	f.mu.Lock()
	rt := f.issueUserToken("alice", implAppID)["refresh_token"].(string)
	f.mu.Unlock()
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, refused := 0, 0
	for range 5 {
		wg.Go(func() {
			_, err := c.OAuthRefresh(context.Background(), rt)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if errors.Is(err, forge.ErrUnauthorized) {
				refused++
			}
		})
	}
	wg.Wait()
	if ok != 1 || refused != 4 || f.refreshCount() != 1 {
		t.Fatalf("%d refreshed, %d refused, %d at GitHub", ok, refused, f.refreshCount())
	}
}

// With expiring user tokens turned off in the App's settings, a token
// doesn't expire and there is nothing to refresh.
func TestNonExpiringUserToken(t *testing.T) {
	f := newFakeGitHub(t, "")
	f.oauth.noExpiry = true
	c := f.client()
	verifier := oauth2.GenerateVerifier()
	u, _ := c.OAuthURL("https://devbot.example/cb", "s", oauth2.S256ChallengeFromVerifier(verifier))
	tok, err := c.OAuthExchange(context.Background(), "https://devbot.example/cb", f.authorize(u, "alice"), verifier)
	if err != nil || tok.Access == "" || tok.Refresh != "" || !tok.Expiry.IsZero() {
		t.Fatalf("got %+v, %v", tok, err)
	}
}

// Every GitHub role, as the person sees it, and who may edit issues.
func TestUserRoles(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	for _, tc := range []struct {
		role string
		want forge.Permission
		edit bool
	}{
		{"read", forge.PermRead, false}, {"triage", forge.PermRead, true}, {"write", forge.PermWrite, true},
		{"maintain", forge.PermMaintain, true}, {"admin", forge.PermAdmin, true},
	} {
		f.grant(tc.role+"-person", "acme/app", tc.role)
		u := c.AsUser(f.signIn(tc.role + "-person"))
		if p, err := u.Permission(context.Background(), appRepo); p != tc.want || err != nil {
			t.Errorf("%s: Permission %s, %v; want %s", tc.role, p, err, tc.want)
		}
		if ok, err := u.CanEditIssues(context.Background(), appRepo); ok != tc.edit || err != nil {
			t.Errorf("%s: CanEditIssues %v, %v", tc.role, ok, err)
		}
		if ok, err := c.Authorized(context.Background(), appRepo, tc.role+"-person", tc.want); !ok || err != nil {
			t.Errorf("%s: Authorized at %s: %v, %v", tc.role, tc.want, ok, err)
		}
	}
	// A triager may label, so filing with devbot/ready keeps the label.
	it, err := c.AsUser(f.signIn("triage-person")).CreateItem(context.Background(), appRepo, "t", "b", []string{"devbot/ready"})
	if err != nil || !it.HasLabel("devbot/ready") || it.Author != "triage-person" {
		t.Fatalf("triager's issue: %+v, %v", it, err)
	}
}

// A repo the sign-in App isn't installed on is invisible to the person's
// token even where they have a role, and a write there says why.
func TestUserRepoWithoutTheApp(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	f.grant("alice", "acme/private", "admin")
	u := c.AsUser(f.signIn("alice"))
	private := forge.RepoRef{Provider: "github", Owner: "acme", Name: "private"}
	if p, err := u.Permission(context.Background(), private); p != forge.PermNone || err != nil {
		t.Fatalf("Permission: %s, %v", p, err)
	}
	_, err := u.CreateItem(context.Background(), private, "t", "b", nil)
	if !errors.Is(err, forge.ErrNotFound) || !strings.Contains(err.Error(), "sign-in GitHub App isn't installed on it") {
		t.Fatalf("CreateItem: %v", err)
	}
}

// An App installed without Issues write can't act on issues for anyone,
// and the refusal says it's the App.
func TestUserAppWithoutIssuesWrite(t *testing.T) {
	f := newFakeGitHub(t, "")
	delete(f.installs[11].perms, "issues")
	c := f.client()
	f.grant("alice", "acme/app", "admin")
	_, err := c.AsUser(f.signIn("alice")).CreateItem(context.Background(), appRepo, "t", "b", nil)
	if !errors.Is(err, forge.ErrForbidden) || !strings.Contains(err.Error(), "needs Issues read and write") {
		t.Fatalf("got %v", err)
	}
}

// SetLabels makes one write: an add alone is GitHub's add, a removal a
// replace of the whole set, and a change already made no write at all.
func TestSetLabelsIsOneWrite(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	f.grant("wendy", "acme/app", "write")
	u := c.AsUser(f.signIn("wendy"))
	n := f.fixture(t).Issue("alice", "t", "b", "bug", "devbot/ready")
	ref := forge.ItemRef{Repo: appRepo, Number: n}
	var writes []string
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != "GET" {
			writes = append(writes, r.Method)
		}
		return false
	}
	for _, step := range []struct {
		add, remove []string
		want        string
	}{
		{[]string{"devbot/automerge"}, nil, "POST"},
		{[]string{"devbot/working"}, []string{"devbot/ready"}, "PUT"},
		{[]string{"devbot/working"}, []string{"devbot/ready"}, ""},
		{nil, []string{"bug", "devbot/working", "devbot/automerge"}, "PUT"},
	} {
		writes = nil
		if err := u.SetLabels(context.Background(), ref, step.add, step.remove); err != nil {
			t.Fatal(err)
		}
		if strings.Join(writes, ",") != step.want {
			t.Errorf("%v -%v: writes %v, want %q", step.add, step.remove, writes, step.want)
		}
	}
	if it, _ := c.GetItem(context.Background(), ref); len(it.Labels) != 0 {
		t.Errorf("labels left: %v", it.Labels)
	}
}

// A person's replace is theirs in the issue's events, so a label someone
// else adds between its read and its write is put back as for the bots.
func TestUserSetLabelsKeepsAConcurrentAdd(t *testing.T) {
	f := newFakeGitHub(t, "")
	f.grant("wendy", "acme/app", "write")
	u := f.client().AsUser(f.signIn("wendy"))
	ref := seed(f, "devbot/ready", "bug")
	afterRead(f, 2, func(*fakeIssue) {
		labelAs(f, "alice", ref, func(l []string) []string { return append(l, "priority") })
	})
	if err := u.SetLabels(context.Background(), ref, []string{"devbot/working"}, []string{"devbot/ready"}); err != nil {
		t.Fatal(err)
	}
	if got := issueLabels(f, ref); !slices.Equal(got, []string{"bug", "devbot/working", "priority"}) {
		t.Fatalf("alice's label must survive wendy's replace, got %v", got)
	}
}

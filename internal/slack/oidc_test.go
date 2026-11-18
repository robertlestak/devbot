package slack_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"git.shdw.tech/rob/devbot/internal/auth"
	"git.shdw.tech/rob/devbot/internal/slack"
	"git.shdw.tech/rob/devbot/internal/slack/slacktest"
)

// Proved names whoever signed in with Slack as a subject to compare with
// the link's: the same user of the same workspace, within the same org.
func TestSignInWithSlackProves(t *testing.T) {
	oidc := slacktest.NewOIDC(t, "client-id", "client-secret")
	sealer, err := auth.NewSealer([]byte("an-auth-key-for-tests-that-is-long-enough"), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	oidc.Now = func() time.Time { return now }
	app, err := slack.New(slack.Options{ClientID: "client-id", ClientSecret: "client-secret", SigningSecret: "s", ResourceURL: "https://devbot.example",
		Redis: redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()}), Sealer: sealer, APIURL: oidc.APIURL(), OIDCURL: oidc.AuthorizeURL(),
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	const redirect = "https://devbot.example/oauth/link/callback"
	prove := func(ws slack.Workspace, user, asEnterprise, asTeam, asUser string) (string, error) {
		l := auth.Link{Subject: slack.Subject(ws, user)}
		u, err := app.ProveURL(l, redirect, "st", "nonce-1")
		if err != nil {
			return "", err
		}
		if q, _ := url.Parse(u); ws.Team != "" && q.Query().Get("team") != ws.Team {
			t.Errorf("Sign in with Slack should be in the link's workspace: %s", u)
		}
		back, err := oidc.SignIn(u, asEnterprise, asTeam, asUser)
		if err != nil {
			return "", err
		}
		q, _ := url.Parse(back)
		return app.Proved(context.Background(), l, q.Query().Get("code"), redirect, "nonce-1")
	}

	grid := slack.Workspace{Enterprise: "E1", Team: "T1"}
	org := slack.Workspace{Enterprise: "E1"}
	for _, tc := range []struct {
		name            string
		ws              slack.Workspace
		ent, team, user string
		same            bool
	}{
		{"workspace", slack.Workspace{Team: "T1"}, "", "T1", "U1", true},
		{"another user", slack.Workspace{Team: "T1"}, "", "T1", "U2", false},
		{"another workspace", slack.Workspace{Team: "T1"}, "", "T2", "U1", false},
		{"grid workspace", grid, "E1", "T1", "W1", true},
		{"grid workspace, no org claim", grid, "", "T1", "W1", false},
		{"grid workspace, another org", grid, "E2", "T1", "W1", false},
		{"org-wide install, any of its workspaces", org, "E1", "T7", "W1", true},
		{"org-wide install, another org", org, "E2", "T7", "W1", false},
		{"org-wide install, no org claim", org, "", "T7", "W1", false},
	} {
		user := "U1"
		if tc.ws.Enterprise != "" {
			user = "W1"
		}
		got, err := prove(tc.ws, user, tc.ent, tc.team, tc.user)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if (got == slack.Subject(tc.ws, user)) != tc.same {
			t.Errorf("%s: proved %q for a link to %q", tc.name, got, slack.Subject(tc.ws, user))
		}
	}

	ws := slack.Workspace{Team: "T1"}
	for name, edit := range map[string]func(map[string]any){
		"issuer":   func(c map[string]any) { c["iss"] = "https://evil.example" },
		"audience": func(c map[string]any) { c["aud"] = "another-app" },
		"expired":  func(c map[string]any) { c["exp"] = now.Add(-time.Second).Unix() },
		"nonce":    func(c map[string]any) { c["nonce"] = "nonce-2" },
		"no user":  func(c map[string]any) { delete(c, "https://slack.com/user_id") },
	} {
		oidc.Claims = edit
		if got, err := prove(ws, "U1", "", "T1", "U1"); err == nil {
			t.Errorf("an ID token with a wrong %s proved %q", name, got)
		}
	}
	oidc.Claims = nil
	// A code works once.
	l := auth.Link{Subject: slack.Subject(ws, "U1")}
	u, _ := app.ProveURL(l, redirect, "st", "nonce-1")
	back, _ := oidc.SignIn(u, "", "T1", "U1")
	q, _ := url.Parse(back)
	if _, err := app.Proved(context.Background(), l, q.Query().Get("code"), redirect, "nonce-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Proved(context.Background(), l, q.Query().Get("code"), redirect, "nonce-1"); err == nil || !strings.Contains(err.Error(), "invalid_code") {
		t.Errorf("a code used twice: %v", err)
	}
}

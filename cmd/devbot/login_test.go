package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/fake"
	"git.shdw.tech/rob/devbot/internal/queue"
	"git.shdw.tech/rob/devbot/internal/server"
)

// devbot login against a serve in auth.mode oauth: the device flow, with a
// person approving it in a browser, then the client commands on the saved,
// refreshing login, then logout.
func TestLoginDeviceFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the device flow's poll interval")
	}
	mr := miniredis.RunT(t)
	f := fake.New()
	f.AddRepo(forge.RepoRef{Provider: "fake", Owner: "acme", Name: "app"}, "version: 1\n")
	ts := httptest.NewUnstartedServer(nil)
	defer ts.Close()
	base := "http://" + ts.Listener.Addr().String()
	cfg, _ := config.Load("")
	cfg.Auth = config.Auth{Mode: "oauth", ResourceURL: base, Provider: "fake", KeyEnv: "TEST_DEVBOT_AUTH_KEY", AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour, DynamicDeviceGrant: true}
	t.Setenv("TEST_DEVBOT_AUTH_KEY", "an-auth-key-for-tests-that-is-long-enough")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := forge.Registry{"fake": f}
	e := &core.Engine{Forges: reg, Q: queue.New(mr.Addr(), 0), Cfg: cfg, Repos: &config.RepoCache{Forges: reg}, Log: log}
	s, err := server.New(e, cfg, log, "test")
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = s.Handler()
	ts.Start()
	t.Setenv("DEVBOT_URL", base)
	t.Setenv("DEVBOT_TOKEN", "")
	t.Setenv("DEVBOT_TOKEN_FILE", t.TempDir()+"/token.json")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- login(ctx) }()

	// The person: find the code the device is showing, enter it, sign in.
	// The server stores the code before the device record it points at, and
	// a person only sees the code once both are written, so wait for both.
	var code string
	for device := false; code == "" || !device; {
		time.Sleep(20 * time.Millisecond)
		code, device = "", false
		for _, k := range mr.Keys() {
			if c, ok := strings.CutPrefix(k, "devbot:auth:usercode:"); ok {
				code = c
			}
			device = device || strings.HasPrefix(k, "devbot:auth:device:")
		}
	}
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("POST", base+"/oauth/device", strings.NewReader(url.Values{"user_code": {code}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", base)
	resp, err := browser.Do(req)
	if err != nil || resp.StatusCode != 302 {
		t.Fatalf("entering the code: %v %v", resp, err)
	}
	cb, err := f.SignIn(resp.Header.Get("Location"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	resp, err = browser.Get(cb)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("callback: %v %v", resp, err)
	}
	page, _ := io.ReadAll(resp.Body)
	m := regexp.MustCompile(`name="consent" value="([^"]+)"`).FindSubmatch(page)
	if m == nil || !strings.Contains(string(page), code) {
		t.Fatalf("consent page: %s", page)
	}
	req, _ = http.NewRequest("POST", base+"/oauth/consent", strings.NewReader(url.Values{"consent": {string(m[1])}, "decision": {"allow"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", base)
	if resp, err := browser.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("consent: %v %v", resp, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	l, err := loadLogin()
	if err != nil || l == nil || l.Token.RefreshToken == "" {
		t.Fatalf("saved login: %+v %v", l, err)
	}
	if st, _ := os.Stat(os.Getenv("DEVBOT_TOKEN_FILE")); st.Mode().Perm() != 0o600 {
		t.Fatalf("the token file must be private, got %v", st.Mode())
	}
	// An expired access token is refreshed, and the rotated refresh token saved.
	old := l.Token.RefreshToken
	l.Token.Expiry = time.Now().Add(-time.Minute)
	saveLogin(*l)
	if err := cli(ctx, "list", nil); err != nil {
		t.Fatalf("list with a refreshed token: %v", err)
	}
	if l2, _ := loadLogin(); l2.Token.RefreshToken == old || !l2.Token.Valid() {
		t.Fatal("the refreshed token should have been saved")
	}
	if err := logout(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cli(ctx, "list", nil); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("after logout, calls should be refused: %v", err)
	}
}

package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/fake"
	"git.shdw.tech/rob/devbot/internal/queue"
)

var slackPaths = []struct{ method, path string }{
	{"GET", "/slack/install"}, {"GET", "/slack/oauth/callback"}, {"POST", "/slack/events"}, {"POST", "/slack/interactivity"}, {"POST", "/slack/commands"},
}

func TestNoSlackRoutesWithoutSlack(t *testing.T) {
	s, _ := newServer(t, "client")
	h := s.Handler()
	for _, p := range slackPaths {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(p.method, p.path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d", p.method, p.path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.MetricsHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(w.Body.String(), "devbot_slack_") {
		t.Errorf("Slack metrics without Slack:\n%s", w.Body.String())
	}
}

// newSlackServer is serve with the Slack app on, in auth mode none with the
// anonymous opt-in, and what it logged.
func newSlackServer(t *testing.T, set func(*config.Config)) (*Server, *syncBuffer, error) {
	t.Setenv("TEST_SLACK_SECRET", "client-secret")
	t.Setenv("TEST_SLACK_SIGNING", "signing-secret")
	t.Setenv("TEST_DEVBOT_AUTH_KEY", "an-auth-key-for-tests-that-is-long-enough")
	cfg, _ := config.Load("")
	cfg.Auth.ResourceURL, cfg.Auth.KeyEnv = "https://devbot.example.com", "TEST_DEVBOT_AUTH_KEY"
	cfg.Slack = config.Slack{Enabled: true, ClientID: "1.2", ClientSecretEnv: "TEST_SLACK_SECRET", SigningSecretEnv: "TEST_SLACK_SIGNING",
		AllowedTeams: []string{"T0ACME"}, AllowAnonymous: true}
	set(cfg)
	f := fake.New()
	reg := forge.Registry{"fake": f}
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	e := &core.Engine{Forges: reg, Q: queue.New(miniredis.RunT(t).Addr(), 0), Cfg: cfg, Repos: &config.RepoCache{Forges: reg}, Log: log}
	s, err := New(e, cfg, log, "test")
	return s, logs, err
}

func TestSlackRoutesAndAnonymousWarning(t *testing.T) {
	s, logs, err := newSlackServer(t, func(*config.Config) {})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "slack.allow_anonymous") {
		t.Errorf("no startup warning for the anonymous opt-in: %s", logs.String())
	}
	h := s.Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/slack/install", nil))
	if w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), "https://slack.com/oauth/v2/authorize?") {
		t.Fatalf("install: %d %s", w.Code, w.Header().Get("Location"))
	}
	body := `{"type":"url_verification","challenge":"abc"}`
	for _, signed := range []bool{true, false} {
		r := httptest.NewRequest("POST", "/slack/events", strings.NewReader(body))
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte("signing-secret"))
		fmt.Fprintf(mac, "v0:%s:%s", ts, body)
		r.Header.Set("X-Slack-Request-Timestamp", ts)
		if signed {
			r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
		}
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if want := map[bool]int{true: 200, false: 401}[signed]; w.Code != want {
			t.Errorf("signed %v: %d, want %d", signed, w.Code, want)
		}
	}
	w = httptest.NewRecorder()
	s.MetricsHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	for _, m := range []string{`devbot_slack_requests_total{endpoint="events",result="verified"}`, `devbot_slack_requests_total{endpoint="events",result="rejected"}`,
		"# TYPE devbot_slack_installs_total counter", "# TYPE devbot_slack_api_errors_total counter"} {
		if !strings.Contains(w.Body.String(), m) {
			t.Errorf("metrics lack %s", m)
		}
	}
}

func TestSlackInOAuthModeDoesntWarn(t *testing.T) {
	_, logs, err := newSlackServer(t, func(c *config.Config) { c.Auth.Mode, c.Auth.Provider, c.Slack.AllowAnonymous = "oauth", "fake", false })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "slack") {
		t.Errorf("logged %s", logs.String())
	}
}

func TestSlackNeedsItsSecrets(t *testing.T) {
	for name, set := range map[string]func(*config.Config){
		"no signing secret": func(c *config.Config) { c.Slack.SigningSecretEnv = "TEST_SLACK_UNSET" },
		"no client secret":  func(c *config.Config) { c.Slack.ClientSecretEnv = "TEST_SLACK_UNSET" },
		"short auth key":    func(c *config.Config) { c.Auth.KeyEnv = "TEST_SLACK_SECRET" },
	} {
		if _, _, err := newSlackServer(t, set); err == nil {
			t.Errorf("%s: serve started", name)
		}
	}
}

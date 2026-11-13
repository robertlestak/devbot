package slack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"git.shdw.tech/rob/devbot/internal/audit/audittest"
	"git.shdw.tech/rob/devbot/internal/auth"
)

const (
	testSecret  = "a-signing-secret"
	testKey     = "an-auth-key-for-tests-that-is-long-enough"
	allowedTeam = "T0ALLOWED"
)

// fakeSlack answers oauth.v2.access with whatever access holds, and
// records the tokens auth.revoke was called with.
type fakeSlack struct {
	*httptest.Server
	mu      sync.Mutex
	access  map[string]any
	revoked []string
}

func newFakeSlack(t *testing.T) *fakeSlack {
	f := &fakeSlack{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/api/oauth.v2.access":
			if id, secret, ok := r.BasicAuth(); !ok || id != "client-id" || secret != "client-secret" {
				json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_client"})
				return
			}
			json.NewEncoder(w).Encode(f.access)
		case "/api/auth.revoke":
			f.revoked = append(f.revoked, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "revoked": true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// grant is oauth.v2.access's answer for an install into team (in
// enterprise, when set), with token.
func grant(team, enterprise, token string) map[string]any {
	m := map[string]any{"ok": true, "access_token": token, "token_type": "bot", "scope": "app_mentions:read,chat:write,commands,users:read,channels:history,groups:history,im:history,mpim:history",
		"bot_user_id": "UBOT", "app_id": "A1", "team": map[string]any{"id": team, "name": "Acme"}, "enterprise": nil,
		"is_enterprise_install": false, "authed_user": map[string]any{"id": "UINSTALLER"}}
	if enterprise != "" {
		m["enterprise"] = map[string]any{"id": enterprise, "name": "Acme Corp"}
	}
	return m
}

type env struct {
	t     *testing.T
	mr    *miniredis.Miniredis
	rdb   *redis.Client
	slack *fakeSlack
	app   *App
	srv   *httptest.Server
	audit *audittest.Recorder
	now   time.Time
}

func newEnv(t *testing.T, allowed ...string) *env {
	return newEnvOn(t, miniredis.RunT(t), allowed...)
}

// newEnvOn is an env on mr, so two can share one Redis as two replicas do.
func newEnvOn(t *testing.T, mr *miniredis.Miniredis, allowed ...string) *env {
	v := &env{t: t, mr: mr, rdb: redis.NewClient(&redis.Options{Addr: mr.Addr()}), slack: newFakeSlack(t), audit: audittest.New(t, "serve"), now: time.Now()}
	sealer, err := auth.NewSealer([]byte(testKey), nil)
	if err != nil {
		t.Fatal(err)
	}
	v.srv = httptest.NewUnstartedServer(nil)
	t.Cleanup(v.srv.Close)
	v.app, err = New(Options{ClientID: "client-id", ClientSecret: "client-secret", SigningSecret: testSecret,
		ResourceURL: "http://" + v.srv.Listener.Addr().String(), AllowedTeams: allowed, Redis: v.rdb, Sealer: sealer,
		Audit: v.audit.Logger, APIURL: v.slack.URL + "/api/", AuthorizeURL: v.slack.URL + "/oauth/v2/authorize",
		Log: slog.New(slog.DiscardHandler), Now: func() time.Time { return v.now }})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	v.app.Routes(mux)
	v.srv.Config.Handler = mux
	v.srv.Start()
	return v
}

var noRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// install runs an install through Slack's answer access, as one browser,
// and returns the callback's status.
func (v *env) install(access map[string]any) int {
	v.t.Helper()
	v.slack.mu.Lock()
	v.slack.access = access
	v.slack.mu.Unlock()
	hc := &http.Client{CheckRedirect: noRedirect}
	resp, err := hc.Get(v.srv.URL + "/slack/install")
	if err != nil {
		v.t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(loc.String(), v.slack.URL+"/oauth/v2/authorize?") {
		v.t.Fatalf("install: %d to %s", resp.StatusCode, loc)
	}
	q := loc.Query()
	if q.Get("client_id") != "client-id" || q.Get("scope") != "app_mentions:read,chat:write,commands,users:read,channels:history,groups:history,im:history,mpim:history" || q.Get("redirect_uri") != v.srv.URL+"/slack/oauth/callback" {
		v.t.Fatalf("authorize URL: %s", loc)
	}
	return v.callback(q.Get("state"), resp.Cookies())
}

func (v *env) callback(state string, cookies []*http.Cookie) int {
	v.t.Helper()
	req, _ := http.NewRequest("GET", v.srv.URL+"/slack/oauth/callback?code=the-code&state="+url.QueryEscape(state), nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		v.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// installKeys are the installs stored in Redis.
func (v *env) installKeys() []string {
	var out []string
	for _, k := range v.mr.Keys() {
		if strings.HasPrefix(k, prefix+"install:") {
			out = append(out, k)
		}
	}
	return out
}

func TestInstallStoresTheTokenSealed(t *testing.T) {
	v := newEnv(t, allowedTeam)
	if code := v.install(grant(allowedTeam, "", "xoxb-the-bot-token")); code != 200 {
		t.Fatalf("install: %d", code)
	}
	keys := v.installKeys()
	if len(keys) != 1 || keys[0] != prefix+"install::"+allowedTeam {
		t.Fatalf("stored %v", keys)
	}
	raw, _ := v.mr.Get(keys[0])
	if strings.Contains(raw, "xoxb") || strings.Contains(raw, "UBOT") {
		t.Fatalf("the install is stored readable: %s", raw)
	}
	in, err := v.app.Installation(context.Background(), Workspace{Team: allowedTeam})
	if err != nil || in.Token != "xoxb-the-bot-token" || in.BotUserID != "UBOT" || in.InstalledBy != "UINSTALLER" || in.TeamName != "Acme" {
		t.Fatalf("installation: %+v, %v", in, err)
	}
	// Moved to another workspace's key, it doesn't open.
	v.mr.Set(prefix+"install::T0OTHER", raw)
	if _, err := v.app.Installation(context.Background(), Workspace{Team: "T0OTHER"}); err == nil {
		t.Fatal("a sealed install opened under another workspace")
	}
	if r := v.audit.Of("slack.install"); len(r) != 1 || r[0]["outcome"] != "ok" || audittest.Detail(r[0])["team_id"] != allowedTeam {
		t.Fatalf("audit: %v", r)
	}
	if strings.Contains(v.audit.Raw(), "xoxb") {
		t.Fatal("the token is in the audit log")
	}
	// Installing again replaces it.
	if code := v.install(grant(allowedTeam, "", "xoxb-second")); code != 200 {
		t.Fatalf("second install: %d", code)
	}
	if in, _ := v.app.Installation(context.Background(), Workspace{Team: allowedTeam}); in.Token != "xoxb-second" || len(v.installKeys()) != 2 {
		t.Fatalf("after reinstalling: %q %v", in.Token, v.installKeys())
	}
}

func TestInstallRefusesTeamsNotAllowed(t *testing.T) {
	for name, allowed := range map[string][]string{"another team": {allowedTeam}, "empty allowlist": nil} {
		t.Run(name, func(t *testing.T) {
			v := newEnv(t, allowed...)
			if code := v.install(grant("T0STRANGER", "", "xoxb-stranger")); code != http.StatusForbidden {
				t.Fatalf("install: %d", code)
			}
			if k := v.installKeys(); len(k) != 0 {
				t.Fatalf("stored %v", k)
			}
			if _, err := v.app.Installation(context.Background(), Workspace{Team: "T0STRANGER"}); !errors.Is(err, ErrNotInstalled) {
				t.Fatalf("installation: %v", err)
			}
			if len(v.slack.revoked) != 1 || v.slack.revoked[0] != "xoxb-stranger" {
				t.Fatalf("revoked %v", v.slack.revoked)
			}
			if r := v.audit.Of("slack.install"); len(r) != 1 || r[0]["outcome"] != "refused" {
				t.Fatalf("audit: %v", r)
			}
		})
	}
}

func TestInstallByEnterprise(t *testing.T) {
	v := newEnv(t, "E0ORG")
	// A workspace of an allowed org.
	if code := v.install(grant("T0WORKSPACE", "E0ORG", "xoxb-ws")); code != 200 {
		t.Fatalf("workspace install: %d", code)
	}
	// An org-wide install: no workspace.
	org := grant("", "E0ORG", "xoxb-org")
	org["team"], org["is_enterprise_install"] = nil, true
	if code := v.install(org); code != 200 {
		t.Fatalf("org install: %d", code)
	}
	for ws, tok := range map[Workspace]string{{Enterprise: "E0ORG", Team: "T0WORKSPACE"}: "xoxb-ws", {Enterprise: "E0ORG"}: "xoxb-org"} {
		if in, err := v.app.Installation(context.Background(), ws); err != nil || in.Token != tok {
			t.Errorf("%+v: %+v, %v", ws, in, err)
		}
	}
	// Another org's workspace isn't allowed by this one.
	if code := v.install(grant("T0WORKSPACE", "E0OTHER", "xoxb-x")); code != http.StatusForbidden {
		t.Fatalf("another org: %d", code)
	}
}

func TestInstallStateIsSingleUseAndBoundToTheBrowser(t *testing.T) {
	v := newEnv(t, allowedTeam)
	v.slack.access = grant(allowedTeam, "", "xoxb-t")
	start := func() (string, []*http.Cookie) {
		resp, err := (&http.Client{CheckRedirect: noRedirect}).Get(v.srv.URL + "/slack/install")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		loc, _ := url.Parse(resp.Header.Get("Location"))
		return loc.Query().Get("state"), resp.Cookies()
	}
	state, cookies := start()
	other, _ := start()
	if code := v.callback(state, nil); code != 400 {
		t.Errorf("without the browser's cookie: %d", code)
	}
	if code := v.callback(other, cookies); code != 400 {
		t.Errorf("another install's state: %d", code)
	}
	if code := v.callback(state, cookies); code != 200 {
		t.Fatalf("the right state: %d", code)
	}
	if code := v.callback(state, cookies); code != 400 {
		t.Errorf("the state again: %d", code)
	}
	state, cookies = start()
	v.mr.FastForward(stateTTL + time.Second)
	if code := v.callback(state, cookies); code != 400 {
		t.Errorf("an expired state: %d", code)
	}
}

// sign signs body as Slack would at ts.
func sign(secret string, ts int64, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "v0:%d:%s", ts, body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// post posts body to path, with headers set by hdr.
func (v *env) post(path, ctype, body string, hdr func(http.Header)) (int, string) {
	v.t.Helper()
	req, _ := http.NewRequest("POST", v.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", ctype)
	hdr(req.Header)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		v.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// signed sets a valid signature of body, made now.
func (v *env) signed(body string, retry int) func(http.Header) {
	return func(h http.Header) {
		ts := v.now.Unix()
		h.Set("X-Slack-Request-Timestamp", strconv.FormatInt(ts, 10))
		h.Set("X-Slack-Signature", sign(testSecret, ts, body))
		if retry > 0 {
			h.Set("X-Slack-Retry-Num", strconv.Itoa(retry))
			h.Set("X-Slack-Retry-Reason", "http_timeout")
		}
	}
}

func (v *env) event(body string, retry int) int {
	v.t.Helper()
	code, _ := v.post("/slack/events", "application/json", body, v.signed(body, retry))
	return code
}

func eventBody(id, team, inner string) string {
	return `{"type":"event_callback","team_id":"` + team + `","event_id":"` + id + `","event":` + inner + `}`
}

func TestUnverifiedRequestsAreRefused(t *testing.T) {
	v := newEnv(t, allowedTeam)
	var handled atomic.Int32
	v.app.OnEvent("app_mention", func(context.Context, Event) error { handled.Add(1); return nil })
	v.app.OnInteraction(func(context.Context, Interaction) (any, error) { handled.Add(1); return nil, nil })
	v.app.OnCommand(func(context.Context, Command) (any, error) { handled.Add(1); return nil, nil })
	v.app.Save(context.Background(), Installation{Workspace: Workspace{Team: allowedTeam}, Token: "xoxb-keep"})
	uninstall := eventBody("Ev1", allowedTeam, `{"type":"app_uninstalled"}`)
	mention := eventBody("Ev2", allowedTeam, `{"type":"app_mention"}`)
	interaction := "payload=" + url.QueryEscape(`{"type":"block_actions","trigger_id":"tr1","team":{"id":"`+allowedTeam+`"},"user":{"id":"U1"}}`)
	command := url.Values{"team_id": {allowedTeam}, "user_id": {"U1"}, "command": {"/devbot"}, "trigger_id": {"tr2"}}.Encode()
	now := v.now.Unix()
	cases := map[string]func(body string) func(http.Header){
		"no signature": func(string) func(http.Header) {
			return func(h http.Header) { h.Set("X-Slack-Request-Timestamp", strconv.FormatInt(now, 10)) }
		},
		"no timestamp": func(b string) func(http.Header) {
			return func(h http.Header) { h.Set("X-Slack-Signature", sign(testSecret, now, b)) }
		},
		"wrong secret": func(b string) func(http.Header) {
			return func(h http.Header) {
				h.Set("X-Slack-Request-Timestamp", strconv.FormatInt(now, 10))
				h.Set("X-Slack-Signature", sign("another-secret", now, b))
			}
		},
		"another body": func(b string) func(http.Header) {
			return func(h http.Header) {
				h.Set("X-Slack-Request-Timestamp", strconv.FormatInt(now, 10))
				h.Set("X-Slack-Signature", sign(testSecret, now, b+" "))
			}
		},
		"another timestamp": func(b string) func(http.Header) {
			return func(h http.Header) {
				h.Set("X-Slack-Request-Timestamp", strconv.FormatInt(now-1, 10))
				h.Set("X-Slack-Signature", sign(testSecret, now, b))
			}
		},
		// Correctly signed, but too old to tell from a replay.
		"stale": func(b string) func(http.Header) {
			old := now - int64(MaxAge/time.Second) - 1
			return func(h http.Header) {
				h.Set("X-Slack-Request-Timestamp", strconv.FormatInt(old, 10))
				h.Set("X-Slack-Signature", sign(testSecret, old, b))
			}
		},
		"from the future": func(b string) func(http.Header) {
			later := now + int64(MaxAge/time.Second) + 1
			return func(h http.Header) {
				h.Set("X-Slack-Request-Timestamp", strconv.FormatInt(later, 10))
				h.Set("X-Slack-Signature", sign(testSecret, later, b))
			}
		},
	}
	for name, hdr := range cases {
		for _, c := range []struct{ path, ctype, body string }{
			{"/slack/events", "application/json", uninstall},
			{"/slack/events", "application/json", mention},
			{"/slack/events", "application/json", `{"type":"url_verification","challenge":"c"}`},
			{"/slack/interactivity", "application/x-www-form-urlencoded", interaction},
			{"/slack/commands", "application/x-www-form-urlencoded", command},
		} {
			if code, body := v.post(c.path, c.ctype, c.body, hdr(c.body)); code != http.StatusUnauthorized || strings.Contains(body, `"c"`) {
				t.Errorf("%s, %s: %d %s", name, c.path, code, body)
			}
		}
	}
	if n := handled.Load(); n != 0 {
		t.Errorf("handled %d unverified requests", n)
	}
	if _, err := v.app.Installation(context.Background(), Workspace{Team: allowedTeam}); err != nil {
		t.Errorf("an unverified app_uninstalled removed the token: %v", err)
	}
	if k := v.mr.Keys(); len(k) != 1 {
		t.Errorf("unverified requests wrote to Redis: %v", k)
	}
	// Signed and within 5 minutes either way is fine.
	v.now = v.now.Add(MaxAge - time.Second)
	if code, _ := v.post("/slack/events", "application/json", mention, func(h http.Header) {
		h.Set("X-Slack-Request-Timestamp", strconv.FormatInt(now, 10))
		h.Set("X-Slack-Signature", sign(testSecret, now, mention))
	}); code != 200 || handled.Load() != 1 {
		t.Errorf("a 5-minute-old request: %d, handled %d", code, handled.Load())
	}
}

func TestURLVerification(t *testing.T) {
	v := newEnv(t)
	body := `{"token":"x","challenge":"3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P","type":"url_verification"}`
	code, out := v.post("/slack/events", "application/json", body, v.signed(body, 0))
	var got map[string]string
	json.Unmarshal([]byte(out), &got)
	if code != 200 || got["challenge"] != "3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P" {
		t.Fatalf("%d %s", code, out)
	}
}

func TestRetriedEventsAreHandledOnceAcrossReplicas(t *testing.T) {
	mr := miniredis.RunT(t)
	a, b := newEnvOn(t, mr, allowedTeam), newEnvOn(t, mr, allowedTeam)
	var mu sync.Mutex
	var got []Event
	for _, v := range []*env{a, b} {
		v.app.OnEvent("app_mention", func(_ context.Context, e Event) error {
			mu.Lock()
			got = append(got, e)
			mu.Unlock()
			return nil
		})
	}
	body := eventBody("Ev123", allowedTeam, `{"type":"app_mention","text":"hi"}`)
	// Slack's first delivery to one replica and its retries to both, two
	// of them at once.
	if code := a.event(body, 0); code != 200 {
		t.Fatalf("first delivery: %d", code)
	}
	var wg sync.WaitGroup
	for i, v := range []*env{b, a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code := v.event(body, i+1); code != 200 {
				t.Errorf("retry %d: %d", i+1, code)
			}
		}()
	}
	wg.Wait()
	if len(got) != 1 || got[0].ID != "Ev123" || got[0].Workspace != (Workspace{Team: allowedTeam}) || got[0].Retry != 0 {
		t.Fatalf("handled %+v", got)
	}
	// Another event is its own.
	if b.event(eventBody("Ev124", allowedTeam, `{"type":"app_mention"}`), 0); len(got) != 2 {
		t.Fatalf("a second event: handled %d", len(got))
	}
}

func TestAFailedEventIsHandledOnItsRetry(t *testing.T) {
	v := newEnv(t, allowedTeam)
	fail := true
	var n int
	v.app.OnEvent("app_mention", func(context.Context, Event) error {
		n++
		if fail {
			return errors.New("not now")
		}
		return nil
	})
	body := eventBody("Ev9", allowedTeam, `{"type":"app_mention"}`)
	if code := v.event(body, 0); code != 500 {
		t.Fatalf("a failed event: %d", code)
	}
	fail = false
	if code := v.event(body, 1); code != 200 || n != 2 {
		t.Fatalf("its retry: %d, handled %d times", code, n)
	}
	if code := v.event(body, 2); code != 200 || n != 2 {
		t.Fatalf("a retry after it was handled: %d, handled %d times", code, n)
	}
}

func TestRetriedInteractionsAreHandledOnce(t *testing.T) {
	v := newEnv(t, "E0B")
	var got []Interaction
	v.app.OnInteraction(func(_ context.Context, i Interaction) (any, error) {
		got = append(got, i)
		return map[string]string{"response_action": "clear"}, nil
	})
	body := "payload=" + url.QueryEscape(`{"type":"view_submission","trigger_id":"tr-1","team":{"id":"T0A"},"enterprise":{"id":"E0B"},"user":{"id":"U1"}}`)
	code, out := v.post("/slack/interactivity", "application/x-www-form-urlencoded", body, v.signed(body, 0))
	if code != 200 || !strings.Contains(out, `"response_action":"clear"`) {
		t.Fatalf("%d %s", code, out)
	}
	if code, _ := v.post("/slack/interactivity", "application/x-www-form-urlencoded", body, v.signed(body, 1)); code != 200 {
		t.Fatalf("again: %d", code)
	}
	if len(got) != 1 || got[0].Type != "view_submission" || got[0].User != "U1" || got[0].Workspace != (Workspace{Enterprise: "E0B", Team: "T0A"}) {
		t.Fatalf("handled %+v", got)
	}
}

func TestUninstallEventsRemoveTheToken(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, inner string
		removed     bool
	}{
		{"app_uninstalled", `{"type":"app_uninstalled"}`, true},
		{"bot token revoked", `{"type":"tokens_revoked","tokens":{"bot":["UBOT"]}}`, true},
		{"user token revoked", `{"type":"tokens_revoked","tokens":{"oauth":["U1"]}}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newEnv(t, allowedTeam, "T0OTHER")
			if code := v.install(grant(allowedTeam, "", "xoxb-gone")); code != 200 {
				t.Fatalf("install: %d", code)
			}
			// Another workspace's install stays.
			v.app.Save(ctx, Installation{Workspace: Workspace{Team: "T0OTHER"}, Token: "xoxb-other"})
			var hooked []Workspace
			v.app.OnUninstall(func(_ context.Context, ws Workspace) error { hooked = append(hooked, ws); return nil })
			for i, id := range []string{"Ev1", "Ev1", "Ev2"} {
				// A retry, and then a second event of the same: nothing
				// left to do is no error.
				if code := v.event(eventBody(id, allowedTeam, c.inner), i); code != 200 {
					t.Fatalf("event %d: %d", i, code)
				}
			}
			_, err := v.app.Installation(ctx, Workspace{Team: allowedTeam})
			if c.removed != errors.Is(err, ErrNotInstalled) {
				t.Fatalf("installation after: %v", err)
			}
			if _, err := v.app.Installation(ctx, Workspace{Team: "T0OTHER"}); err != nil {
				t.Fatalf("the other workspace's: %v", err)
			}
			want := 0
			if c.removed {
				want = 2
			}
			if len(hooked) != want || (want > 0 && hooked[0] != Workspace{Team: allowedTeam}) {
				t.Fatalf("uninstall hooks ran for %v", hooked)
			}
			if r := v.audit.Of("slack.uninstall"); len(r) != want || (want > 0 && audittest.Detail(r[0])["had_token"] != true) {
				t.Fatalf("audit: %v", r)
			}
		})
	}
}

func TestOrgWideUninstall(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	org := Workspace{Enterprise: "E0ORG"}
	v.app.Save(ctx, Installation{Workspace: org, Token: "xoxb-org"})
	body := `{"type":"event_callback","team_id":"T0SOME","enterprise_id":"E0ORG","is_enterprise_install":true,"event_id":"Ev1","event":{"type":"app_uninstalled"}}`
	if code := v.event(body, 0); code != 200 {
		t.Fatal(code)
	}
	if _, err := v.app.Installation(ctx, org); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("org install after: %v", err)
	}
}

func TestAFailedUninstallHookIsRetried(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	ws := Workspace{Team: allowedTeam}
	v.app.Save(ctx, Installation{Workspace: ws, Token: "xoxb"})
	fail := true
	v.app.OnUninstall(func(context.Context, Workspace) error {
		if fail {
			return errors.New("redis blip")
		}
		return nil
	})
	body := eventBody("Ev1", allowedTeam, `{"type":"app_uninstalled"}`)
	if code := v.event(body, 0); code != 500 {
		t.Fatalf("failing hook: %d", code)
	}
	fail = false
	if code := v.event(body, 1); code != 200 {
		t.Fatalf("retry: %d", code)
	}
	if _, err := v.app.Installation(ctx, ws); !errors.Is(err, ErrNotInstalled) {
		t.Fatal(err)
	}
}

func TestInstallationResealsUnderANewKey(t *testing.T) {
	v := newEnv(t, allowedTeam)
	ctx := context.Background()
	ws := Workspace{Team: allowedTeam}
	v.app.Save(ctx, Installation{Workspace: ws, Token: "xoxb-old-key"})
	before, _ := v.mr.Get(installKey(ws))
	rotated, _ := auth.NewSealer([]byte("a-new-auth-key-for-tests-also-long-enough"), []byte(testKey))
	v.app.o.Sealer = rotated
	if in, err := v.app.Installation(ctx, ws); err != nil || in.Token != "xoxb-old-key" {
		t.Fatalf("under the previous key: %+v, %v", in, err)
	}
	after, _ := v.mr.Get(installKey(ws))
	if after == before {
		t.Fatal("not re-sealed")
	}
	v.app.o.Sealer, _ = auth.NewSealer([]byte("a-new-auth-key-for-tests-also-long-enough"), nil)
	if in, err := v.app.Installation(ctx, ws); err != nil || in.Token != "xoxb-old-key" {
		t.Fatalf("under the new key alone: %+v, %v", in, err)
	}
}

func TestAWorkspaceTakenOffTheListIsCutOff(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	before := newEnvOn(t, mr, allowedTeam, "T0STAYS")
	if code := before.install(grant(allowedTeam, "", "xoxb-cut-off")); code != 200 {
		t.Fatalf("install: %d", code)
	}
	// The operator takes it off the list: serve restarts with the narrower
	// one, over the same Redis.
	v := newEnvOn(t, mr, "T0STAYS")
	var logs strings.Builder
	v.app.o.Log = slog.New(slog.NewTextHandler(&logs, nil))
	var events []Event
	var interactions []Interaction
	v.app.OnEvent("app_mention", func(_ context.Context, e Event) error { events = append(events, e); return nil })
	v.app.OnInteraction(func(_ context.Context, i Interaction) (any, error) {
		interactions = append(interactions, i)
		return nil, nil
	})
	var hooked []Workspace
	v.app.OnUninstall(func(_ context.Context, ws Workspace) error { hooked = append(hooked, ws); return nil })
	counted := func(m string) bool { var b bytes.Buffer; WriteMetrics(&b); return strings.Contains(b.String(), m) }

	for i, id := range []string{"Ev1", "Ev2"} {
		if code := v.event(eventBody(id, allowedTeam, `{"type":"app_mention"}`), 0); code != 200 {
			t.Fatalf("event %d: %d", i, code)
		}
	}
	body := "payload=" + url.QueryEscape(`{"type":"block_actions","trigger_id":"tr-1","team":{"id":"`+allowedTeam+`"},"user":{"id":"U1"}}`)
	if code, _ := v.post("/slack/interactivity", "application/x-www-form-urlencoded", body, v.signed(body, 0)); code != 200 {
		t.Fatalf("interaction: %d", code)
	}
	if len(events) != 0 || len(interactions) != 0 {
		t.Fatalf("dispatched %v, %v", events, interactions)
	}
	if bot, err := v.app.Bot(ctx, Workspace{Team: allowedTeam}); !errors.Is(err, ErrNotInstalled) || bot != nil {
		t.Fatalf("bot: %v, %v", bot, err)
	}
	if !counted(`devbot_slack_disallowed_total{use="events"}`) || !counted(`devbot_slack_disallowed_total{use="interactivity"}`) || !counted(`devbot_slack_disallowed_total{use="token"}`) {
		t.Fatal("not counted")
	}
	if n := strings.Count(logs.String(), "slack.allowed_teams"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, logs.String())
	}
	// Its token is still stored, and goes when it uninstalls.
	if keys := v.installKeys(); len(keys) != 1 {
		t.Fatalf("stored %v", keys)
	}
	if code := v.event(eventBody("Ev3", allowedTeam, `{"type":"app_uninstalled"}`), 0); code != 200 {
		t.Fatalf("uninstall: %d", code)
	}
	if keys := v.installKeys(); len(keys) != 0 || len(hooked) != 1 {
		t.Fatalf("after uninstall: stored %v, hooks ran for %v", keys, hooked)
	}
	// A workspace still on the list is unaffected.
	v.event(eventBody("Ev4", "T0STAYS", `{"type":"app_mention"}`), 0)
	if len(events) != 1 {
		t.Fatalf("an allowed workspace's event: handled %d", len(events))
	}
}

func TestCommands(t *testing.T) {
	v := newEnv(t, allowedTeam, "E0ORG")
	var got []Command
	v.app.OnCommand(func(_ context.Context, c Command) (any, error) {
		got = append(got, c)
		return map[string]string{"response_type": "ephemeral", "text": "hi"}, nil
	})
	form := url.Values{"team_id": {allowedTeam}, "user_id": {"U1"}, "channel_id": {"C1"}, "command": {"/devbot"}, "text": {"  disconnect "}, "trigger_id": {"tr-1"}}
	body := form.Encode()
	if code, _ := v.post("/slack/commands", "application/x-www-form-urlencoded", body, func(http.Header) {}); code != http.StatusUnauthorized {
		t.Fatalf("unsigned: %d", code)
	}
	code, out := v.post("/slack/commands", "application/x-www-form-urlencoded", body, v.signed(body, 0))
	if code != 200 || !strings.Contains(out, `"text":"hi"`) {
		t.Fatalf("%d %s", code, out)
	}
	// The same trigger again is a replay, handled once.
	if code, _ := v.post("/slack/commands", "application/x-www-form-urlencoded", body, v.signed(body, 0)); code != 200 {
		t.Fatalf("again: %d", code)
	}
	// An org-wide install's command is the org's.
	form.Set("trigger_id", "tr-2")
	form.Set("enterprise_id", "E0ORG")
	form.Set("is_enterprise_install", "true")
	body = form.Encode()
	v.post("/slack/commands", "application/x-www-form-urlencoded", body, v.signed(body, 0))
	// A workspace that isn't allowed gets nothing handled.
	form = url.Values{"team_id": {"T0OTHER"}, "user_id": {"U1"}, "command": {"/devbot"}, "trigger_id": {"tr-3"}}
	body = form.Encode()
	if code, out := v.post("/slack/commands", "application/x-www-form-urlencoded", body, v.signed(body, 0)); code != 200 || out != "" {
		t.Fatalf("not allowed: %d %s", code, out)
	}
	want := []Command{
		{Workspace: Workspace{Team: allowedTeam}, User: "U1", Channel: "C1", Command: "/devbot", Text: "disconnect", TriggerID: "tr-1"},
		{Workspace: Workspace{Enterprise: "E0ORG"}, User: "U1", Channel: "C1", Command: "/devbot", Text: "disconnect", TriggerID: "tr-2"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("handled %+v\nwant %+v", got, want)
	}
}

// Commands and interactions carry when serve received them, from before
// their body was read, for the handler to answer within Slack's limit.
func TestRequestsCarryWhenReceived(t *testing.T) {
	v := newEnv(t, allowedTeam)
	var got []time.Time
	stamp := func(ctx context.Context) {
		if r := Received(ctx); r.IsZero() || r.After(time.Now()) {
			t.Errorf("received %v", r)
		} else {
			got = append(got, r)
		}
	}
	v.app.OnCommand(func(ctx context.Context, _ Command) (any, error) { stamp(ctx); return nil, nil })
	v.app.OnInteraction(func(ctx context.Context, _ Interaction) (any, error) { stamp(ctx); return nil, nil })
	start := time.Now()
	body := url.Values{"team_id": {allowedTeam}, "user_id": {"U1"}, "command": {"/devbot"}, "trigger_id": {"tr-1"}}.Encode()
	v.post("/slack/commands", "application/x-www-form-urlencoded", body, v.signed(body, 0))
	body = url.Values{"payload": {`{"type":"block_actions","trigger_id":"tr-2","team":{"id":"` + allowedTeam + `"},"user":{"id":"U1"}}`}}.Encode()
	v.post("/slack/interactivity", "application/x-www-form-urlencoded", body, v.signed(body, 0))
	if len(got) != 2 || got[0].Before(start) || got[1].Before(got[0]) {
		t.Fatalf("received %v, the first sent at %v", got, start)
	}
	if !Received(context.Background()).IsZero() {
		t.Fatal("a context that isn't a request's has no received time")
	}
}

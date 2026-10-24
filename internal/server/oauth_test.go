package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"golang.org/x/oauth2"

	"git.shdw.tech/rob/devbot/internal/audit"
	"git.shdw.tech/rob/devbot/internal/audit/audittest"
	"git.shdw.tech/rob/devbot/internal/auth"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/fake"
	"git.shdw.tech/rob/devbot/internal/queue"
)

var (
	appRepo    = forge.RepoRef{Provider: "fake", Owner: "acme", Name: "app"}
	secretRepo = forge.RepoRef{Provider: "fake", Owner: "acme", Name: "secret"}
)

// oauthEnv is serve in auth.mode oauth on a real listener, with the fake
// forge as identity provider, a clock the test moves, and every log line
// kept so the test can check no token leaks into them.
type oauthEnv struct {
	t    *testing.T
	f    *fake.Forge
	mr   *miniredis.Miniredis
	s    *Server
	url  string
	hc   *http.Client
	logs *syncBuffer
	// audit is every audit record serve wrote.
	audit *audittest.Recorder
	// lastCode is the token request login redeemed its code with.
	lastCode url.Values

	mu  sync.Mutex
	now time.Time
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func newOAuthEnv(t *testing.T) *oauthEnv { return newOAuthEnvWith(t, func(*config.Config) {}) }

// newOAuthEnvWith is newOAuthEnv with the config changed by set.
func newOAuthEnvWith(t *testing.T, set func(*config.Config)) *oauthEnv {
	v := &oauthEnv{t: t, mr: miniredis.RunT(t), f: fake.New(), logs: &syncBuffer{}, now: time.Now()}
	v.f.AddRepo(appRepo, "version: 1\nenabled: true\n")
	v.f.AddRepo(secretRepo, "version: 1\nenabled: true\n")
	v.f.Grant(secretRepo, "alice", forge.PermNone)

	ts := httptest.NewUnstartedServer(nil)
	t.Cleanup(ts.Close)
	v.url = "http://" + ts.Listener.Addr().String()

	cfg, _ := config.Load("")
	cfg.Auth = config.Auth{Mode: "oauth", ResourceURL: v.url, Provider: "fake", KeyEnv: "TEST_DEVBOT_AUTH_KEY",
		AccessTokenTTL: time.Hour, RefreshTokenTTL: 30 * 24 * time.Hour, DynamicDeviceGrant: true}
	set(cfg)
	t.Setenv("TEST_DEVBOT_AUTH_KEY", "an-auth-key-for-tests-that-is-long-enough")
	v.audit = audittest.New(t, "serve")
	reg := audit.Registry(forge.Registry{"fake": v.f}, v.audit.Logger)
	log := slog.New(slog.NewTextHandler(v.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := &core.Engine{Forges: reg, Q: queue.New(v.mr.Addr(), 0), Cfg: cfg, Repos: &config.RepoCache{Forges: reg}, Log: log, Audit: v.audit.Logger}
	s, err := New(e, cfg, log, "test")
	if err != nil {
		t.Fatal(err)
	}
	v.s = s
	// The same broker, on the test's clock.
	s.Auth = v.broker(v.url)
	ts.Config.Handler = s.Handler()
	ts.Start()
	jar, _ := cookiejar.New(nil)
	v.hc = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return v
}

func (v *oauthEnv) broker(resource string) *auth.Server {
	o := AuthOptions(v.s.Cfg, v.f, v.s.E.Q.R, slog.New(slog.NewTextHandler(v.logs, nil)))
	o.ResourceURL, o.Key, o.Now, o.Audit = resource, []byte("an-auth-key-for-tests-that-is-long-enough"), v.clock, v.s.E.Audit
	a, err := auth.New(o)
	if err != nil {
		v.t.Fatal(err)
	}
	return a
}

func (v *oauthEnv) clock() time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.now
}

func (v *oauthEnv) advance(d time.Duration) {
	v.mu.Lock()
	v.now = v.now.Add(d)
	v.mu.Unlock()
	v.mr.FastForward(d)
}

func (v *oauthEnv) do(req *http.Request) (*http.Response, string) {
	v.t.Helper()
	resp, err := v.hc.Do(req)
	if err != nil {
		v.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (v *oauthEnv) get(u string) (*http.Response, string) {
	req, _ := http.NewRequest("GET", u, nil)
	return v.do(req)
}

func (v *oauthEnv) postForm(path string, f url.Values) (int, map[string]any) {
	v.t.Helper()
	req, _ := http.NewRequest("POST", v.url+path, strings.NewReader(f.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, body := v.do(req)
	var out map[string]any
	json.Unmarshal([]byte(body), &out)
	return resp.StatusCode, out
}

func (v *oauthEnv) register(meta string) string {
	v.t.Helper()
	req, _ := http.NewRequest("POST", v.url+"/oauth/register", strings.NewReader(meta))
	resp, body := v.do(req)
	var out map[string]any
	json.Unmarshal([]byte(body), &out)
	if resp.StatusCode != 201 || out["client_id"] == "" {
		v.t.Fatalf("register: %d %s", resp.StatusCode, body)
	}
	return out["client_id"].(string)
}

// mcp calls a tool with a bearer token, returning the HTTP status and, on
// success, the tool's text and whether it was an error.
func (v *oauthEnv) mcp(token, tool, args string) (int, string, bool) {
	v.t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
	req, _ := http.NewRequest("POST", v.url+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, out := v.do(req)
	if resp.StatusCode != 200 {
		return resp.StatusCode, resp.Header.Get("WWW-Authenticate"), true
	}
	var r struct {
		Result struct {
			Content []struct{ Text string }
			IsError bool
		}
	}
	json.Unmarshal([]byte(out), &r)
	if len(r.Result.Content) == 0 {
		v.t.Fatalf("mcp %s: %s", tool, out)
	}
	return 200, r.Result.Content[0].Text, r.Result.IsError
}

// login runs the authorization code flow as login, through the fake forge's
// login page, and returns the token response.
func (v *oauthEnv) login(clientID, redirect, login string) map[string]any {
	v.t.Helper()
	verifier := oauth2.GenerateVerifier()
	q := url.Values{"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"}, "state": {"client-state"},
		"code_challenge": {oauth2.S256ChallengeFromVerifier(verifier)}, "code_challenge_method": {"S256"}, "resource": {v.url + "/mcp"}}
	resp, body := v.get(v.url + "/oauth/authorize?" + q.Encode())
	if resp.StatusCode != 302 || !strings.HasPrefix(resp.Header.Get("Location"), "https://fake/login/oauth/authorize?") {
		v.t.Fatalf("authorize should send the person to the forge: %d %s %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	cb, err := v.f.SignIn(resp.Header.Get("Location"), login)
	if err != nil {
		v.t.Fatal(err)
	}
	if !strings.HasPrefix(cb, v.url+"/oauth/callback?") {
		v.t.Fatalf("the forge should come back to devbot's callback: %s", cb)
	}
	resp = v.consent(cb, "allow", v.url)
	back, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != 302 || back.Query().Get("state") != "client-state" || back.Query().Get("code") == "" || back.Query().Get("iss") != v.url {
		v.t.Fatalf("consent should return a code to the client: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if want, _ := url.Parse(redirect); back.Host != want.Host || back.Path != want.Path {
		v.t.Fatalf("the code went to %s, not %s", back, redirect)
	}
	code := back.Query().Get("code")
	tokenForm := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "client_id": {clientID},
		"code_verifier": {verifier}, "resource": {v.url + "/mcp"}}
	st, tok := v.postForm("/oauth/token", tokenForm)
	if st != 200 || tok["access_token"] == nil || tok["refresh_token"] == nil || tok["token_type"] != "Bearer" {
		v.t.Fatalf("token: %d %v", st, tok)
	}
	v.lastCode = tokenForm
	return tok
}

// assertNoLeak checks that no token, devbot's or the forge's, is in Redis
// in the clear or in any log line or audit record.
func (v *oauthEnv) assertNoLeak(devbotTokens ...string) {
	v.t.Helper()
	secrets := append(v.f.UserTokens(), devbotTokens...)
	logs := v.logs.String() + v.audit.Raw()
	var stored strings.Builder
	for _, k := range v.mr.Keys() {
		stored.WriteString(k + "\n")
		if s, err := v.mr.Get(k); err == nil {
			stored.WriteString(s + "\n")
		}
	}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if strings.Contains(logs, s) {
			v.t.Errorf("a token is in the logs: %q", s[:12])
		}
		if strings.Contains(stored.String(), s) {
			v.t.Errorf("a token is stored in the clear: %q", s[:12])
		}
	}
}

func TestOAuthEndToEnd(t *testing.T) {
	v := newOAuthEnv(t)
	ctx := context.Background()

	// 1. No token: a 401 that points at the protected-resource metadata.
	st, challenge, _ := v.mcp("", "list_tasks", "{}")
	want := `Bearer resource_metadata="` + v.url + `/.well-known/oauth-protected-resource"`
	if st != 401 || challenge != want {
		t.Fatalf("want 401 with %s, got %d %q", want, st, challenge)
	}

	// 2. Discovery: RFC 9728, then RFC 8414.
	_, body := v.get(v.url + "/.well-known/oauth-protected-resource")
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	json.Unmarshal([]byte(body), &prm)
	if prm.Resource != v.url || len(prm.AuthorizationServers) != 1 || prm.AuthorizationServers[0] != v.url {
		t.Fatalf("protected resource metadata: %s", body)
	}
	_, body = v.get(v.url + "/.well-known/oauth-protected-resource/mcp")
	if !strings.Contains(body, `"resource":"`+v.url+`/mcp"`) {
		t.Fatalf("path-specific metadata should name that resource: %s", body)
	}
	_, body = v.get(v.url + "/.well-known/oauth-authorization-server")
	var asm map[string]any
	json.Unmarshal([]byte(body), &asm)
	for k, want := range map[string]string{"issuer": v.url, "authorization_endpoint": v.url + "/oauth/authorize", "token_endpoint": v.url + "/oauth/token",
		"registration_endpoint": v.url + "/oauth/register", "revocation_endpoint": v.url + "/oauth/revoke",
		"device_authorization_endpoint": v.url + "/oauth/device_authorization"} {
		if asm[k] != want {
			t.Errorf("metadata %s = %v, want %s", k, asm[k], want)
		}
	}
	if m, _ := json.Marshal(asm["code_challenge_methods_supported"]); string(m) != `["S256"]` {
		t.Errorf("only S256: %s", m)
	}

	// 3. Dynamic client registration.
	cid := v.register(`{"client_name":"test client","redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)

	// PKCE is required.
	q := url.Values{"client_id": {cid}, "redirect_uri": {"http://127.0.0.1:7777/callback"}, "response_type": {"code"}, "state": {"s"}}
	resp, _ := v.get(v.url + "/oauth/authorize?" + q.Encode())
	if loc, _ := url.Parse(resp.Header.Get("Location")); resp.StatusCode != 302 || loc.Query().Get("error") != "invalid_request" || loc.Query().Get("state") != "s" {
		t.Fatalf("authorize without PKCE must fail back to the client: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// An unregistered redirect never gets a redirect.
	q.Set("redirect_uri", "https://evil.example/callback")
	if resp, _ := v.get(v.url + "/oauth/authorize?" + q.Encode()); resp.StatusCode != 400 || resp.Header.Get("Location") != "" {
		t.Fatalf("unregistered redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	// 4. authorize -> forge login -> callback -> token. A loopback redirect
	// may use any port.
	tok := v.login(cid, "http://127.0.0.1:7777/callback", "alice")
	at, rt := tok["access_token"].(string), tok["refresh_token"].(string)

	// 5. An authenticated MCP call, done on the forge as alice.
	st, text, isErr := v.mcp(at, "file_task", `{"repo":"fake:acme/app","title":"t","acceptance_criteria":"- x","origin":"chat:1"}`)
	if st != 200 || isErr {
		t.Fatalf("file_task: %d %s", st, text)
	}
	var filed core.TaskStatus
	json.Unmarshal([]byte(text), &filed)
	ref, _ := forge.ParseItem(filed.Task)
	it, _ := v.f.GetItem(ctx, ref)
	if it.Author != "alice" || filed.Requester != "alice" {
		t.Fatalf("the issue should be alice's: author %q, requester %q", it.Author, filed.Requester)
	}
	// A repo alice can't read is invisible, in every read.
	hidden := v.f.NewIssue(secretRepo, "carol", "hidden", "", core.LReady)
	hiddenItem, _ := v.f.GetItem(ctx, hidden)
	v.s.E.Emit(ctx, hiddenItem, "task.filed", core.LReady, "hidden", "")
	if _, text, _ := v.mcp(at, "list_tasks", "{}"); strings.Contains(text, "secret") || !strings.Contains(text, filed.Task) {
		t.Fatalf("list_tasks should show only alice's readable repos: %s", text)
	}
	if _, text, _ := v.mcp(at, "events", "{}"); strings.Contains(text, "secret") || !strings.Contains(text, filed.Task) {
		t.Fatalf("events should show only alice's readable repos: %s", text)
	}
	if _, text, isErr := v.mcp(at, "task_status", `{"ref":"`+hidden.String()+`"}`); !isErr || !strings.Contains(text, "not found") {
		t.Fatalf("task_status on an unreadable repo should be not found: %s", text)
	}
	if _, text, isErr := v.mcp(at, "file_task", `{"repo":"fake:acme/secret","title":"t","acceptance_criteria":"- x"}`); !isErr || !strings.Contains(text, "not found") {
		t.Fatalf("filing on an unreadable repo should be not found: %s", text)
	}
	// The HTTP API is behind the same tokens, and says 403 for a refusal.
	v.f.Grant(appRepo, "dave", forge.PermRead)
	dave := v.login(cid, "http://127.0.0.1/callback", "dave")
	req, _ := http.NewRequest("POST", v.url+"/api/tasks", strings.NewReader(`{"repo":"fake:acme/app","title":"t","acceptance_criteria":"- x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+dave["access_token"].(string))
	if resp, body := v.do(req); resp.StatusCode != 403 || !strings.Contains(body, "doesn't have write access") {
		t.Fatalf("dave can read but not direct devbot: %d %s", resp.StatusCode, body)
	}

	// 6. Anything but a live devbot token for this resource is refused,
	// and nothing runs.
	forgeTok := v.f.TokenFor("alice").Access
	before := len(v.f.Items)
	for name, bad := range map[string]string{"raw forge token": forgeTok, "garbage": "devbot_at_nope", "refresh token as access": rt} {
		if st, ch, _ := v.mcp(bad, "file_task", `{"repo":"fake:acme/app","title":"t","acceptance_criteria":"- x"}`); st != 401 || !strings.Contains(ch, `error="invalid_token"`) {
			t.Errorf("%s: want 401 invalid_token, got %d %q", name, st, ch)
		}
	}
	if len(v.f.Items) != before {
		t.Fatal("a refused call must not run")
	}
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+at)
	if _, err := v.broker("https://elsewhere.example").Authenticate(r); err != auth.ErrInvalidToken {
		t.Fatalf("a token for another audience must be refused, got %v", err)
	}

	// 7. Refresh rotates both tokens.
	st, fresh := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {cid}})
	if st != 200 || fresh["access_token"] == at || fresh["refresh_token"] == rt {
		t.Fatalf("refresh: %d %v", st, fresh)
	}
	at2, rt2 := fresh["access_token"].(string), fresh["refresh_token"].(string)
	if st, _, _ := v.mcp(at, "list_tasks", "{}"); st != 401 {
		t.Fatalf("the old access token should be retired, got %d", st)
	}
	if st, text, isErr := v.mcp(at2, "list_tasks", "{}"); st != 200 || isErr {
		t.Fatalf("the new access token should work: %d %s", st, text)
	}
	// An access token expires.
	v.advance(61 * time.Minute)
	if st, _, _ := v.mcp(at2, "list_tasks", "{}"); st != 401 {
		t.Fatalf("an expired access token must be refused, got %d", st)
	}
	st, fresh = v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt2}, "client_id": {cid}})
	if st != 200 {
		t.Fatalf("refresh after expiry: %d %v", st, fresh)
	}
	at3, rt3 := fresh["access_token"].(string), fresh["refresh_token"].(string)
	if st, _, _ := v.mcp(at3, "list_tasks", "{}"); st != 200 {
		t.Fatalf("refreshed token should work, got %d", st)
	}

	// 8. Revoking ends the grant and deletes the stored forge tokens.
	v.assertNoLeak(at, rt, at2, rt2, at3, rt3)
	if st, _ := v.postForm("/oauth/revoke", url.Values{"token": {rt3}, "client_id": {cid}}); st != 200 {
		t.Fatalf("revoke: %d", st)
	}
	if st, ch, _ := v.mcp(at3, "list_tasks", "{}"); st != 401 || !strings.HasPrefix(ch, "Bearer resource_metadata=") {
		t.Fatalf("after revocation: want 401, got %d %q", st, ch)
	}
	if st, out := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt3}, "client_id": {cid}}); st != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("a revoked refresh token must fail: %d %v", st, out)
	}
	for _, k := range v.mr.Keys() {
		if s, _ := v.mr.Get(k); strings.HasPrefix(k, "devbot:auth:grant:") && strings.Contains(s, `"login":"alice"`) {
			t.Fatalf("alice's grant, with her forge token, should be gone: %s", k)
		}
	}
	v.assertNoLeak(at, rt, at2, rt2, at3, rt3)
}

// A forge refresh that fails ends the grant: the next call is a 401, and
// the client has to sign in again. One that works is invisible.
func TestOAuthForgeRefresh(t *testing.T) {
	v := newOAuthEnv(t)
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	v.f.SetTokenTTL(10 * time.Minute)
	tok := v.login(cid, "http://127.0.0.1/callback", "alice")
	at := tok["access_token"].(string)

	v.advance(9*time.Minute + 30*time.Second) // inside the refresh margin
	before := len(v.f.UserTokens())
	if st, text, isErr := v.mcp(at, "list_tasks", "{}"); st != 200 || isErr {
		t.Fatalf("a refreshable forge token should just be refreshed: %d %s", st, text)
	}
	if len(v.f.UserTokens()) == before {
		t.Fatal("devbot should have refreshed the forge token")
	}

	v.advance(9*time.Minute + 30*time.Second)
	v.f.FailRefresh(true)
	if st, ch, _ := v.mcp(at, "list_tasks", "{}"); st != 401 || !strings.Contains(ch, "resource_metadata=") {
		t.Fatalf("a failed forge refresh should be a 401, got %d %q", st, ch)
	}
	st, out := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}, "client_id": {cid}})
	if st != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("the grant is over: want invalid_grant, got %d %v", st, out)
	}
	v.assertNoLeak(at, tok["refresh_token"].(string))
}

// One person signed in from two clients, against a forge with Gitea's one
// OAuth grant per user and app, where every refresh invalidates the refresh
// tokens before it: they share one forge token, so both keep working as it
// is refreshed through one and then the other.
func TestOAuthSignInsShareForgeToken(t *testing.T) {
	v := newOAuthEnv(t)
	v.f.OneGrantPerUser(true)
	v.f.SetClock(v.clock)
	v.f.SetTokenTTL(10 * time.Minute)
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	first := v.login(cid, "http://127.0.0.1/callback", "alice")
	second := v.login(cid, "http://127.0.0.1/callback", "alice")
	for i, tok := range []map[string]any{first, second, first, second} {
		v.advance(9*time.Minute + 30*time.Second)
		if st, text, isErr := v.mcp(tok["access_token"].(string), "list_tasks", "{}"); st != 200 || isErr {
			t.Fatalf("call %d: %d %s", i, st, text)
		}
	}
	for _, tok := range []map[string]any{first, second} {
		if st, out := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}, "client_id": {cid}}); st != 200 {
			t.Fatalf("refresh: %d %v", st, out)
		}
	}
	if n := v.f.Refreshes(); n != 4 {
		t.Fatalf("want one forge refresh per expiry, got %d", n)
	}
	v.assertNoLeak(first["access_token"].(string), second["access_token"].(string))
}

// A rotated refresh token presented again after the grace window means it
// leaked: the grant ends.
func TestOAuthRefreshReuseEndsGrant(t *testing.T) {
	v := newOAuthEnv(t)
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	tok := v.login(cid, "http://127.0.0.1/callback", "alice")
	rt := tok["refresh_token"].(string)
	_, fresh := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {cid}})
	v.advance(time.Minute)
	if st, _ := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {cid}}); st != 400 {
		t.Fatalf("reuse must fail, got %d", st)
	}
	if st, _, _ := v.mcp(fresh["access_token"].(string), "list_tasks", "{}"); st != 401 {
		t.Fatalf("reuse should end the whole grant, got %d", st)
	}
}

// auth.refresh_reuse_grace: 0 is strict: any reuse ends the grant.
func TestOAuthRefreshReuseNoGrace(t *testing.T) {
	v := newOAuthEnvWith(t, func(c *config.Config) { var off time.Duration; c.Auth.RefreshReuseGrace = &off })
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	tok := v.login(cid, "http://127.0.0.1/callback", "alice")
	rt := tok["refresh_token"].(string)
	_, fresh := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {cid}})
	for _, k := range v.mr.Keys() {
		if strings.Contains(k, "rt-grace:") {
			t.Fatalf("no grace record without a grace window: %s", k)
		}
	}
	if st, _ := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {cid}}); st != 400 {
		t.Fatalf("reuse must fail, got %d", st)
	}
	if st, _, _ := v.mcp(fresh["access_token"].(string), "list_tasks", "{}"); st != 401 {
		t.Fatalf("reuse should end the whole grant, got %d", st)
	}
}

// Within the grace window, the same client presenting a just-rotated
// refresh token gets the same new tokens back, and the grant stands.
func TestOAuthRefreshReuseGrace(t *testing.T) {
	v := newOAuthEnv(t)
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	tok := v.login(cid, "http://127.0.0.1/callback", "alice")
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}, "client_id": {cid}}
	st, first := v.postForm("/oauth/token", form)
	if st != 200 {
		t.Fatalf("refresh: %d %v", st, first)
	}
	v.advance(30 * time.Second)
	st, again := v.postForm("/oauth/token", form)
	if st != 200 || again["access_token"] != first["access_token"] || again["refresh_token"] != first["refresh_token"] {
		t.Fatalf("a reuse within the grace window should get the same tokens: %d %v", st, again)
	}
	if ttl := again["expires_in"].(float64); ttl > 3570 || ttl < 3560 {
		t.Fatalf("expires_in should count from the first refresh, got %v", ttl)
	}
	if st, _, _ := v.mcp(first["access_token"].(string), "list_tasks", "{}"); st != 200 {
		t.Fatalf("the grant should stand: %d", st)
	}
	v.assertNoLeak(tok["access_token"].(string), tok["refresh_token"].(string), first["access_token"].(string), first["refresh_token"].(string))
	if strings.Contains(v.logs.String(), "reused") {
		t.Fatalf("a graced reuse isn't a leak: %s", v.logs.String())
	}

	// The tokens it got rotate normally, and then the grace is over: the
	// original token presented again ends the grant.
	st, next := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first["refresh_token"].(string)}, "client_id": {cid}})
	if st != 200 {
		t.Fatalf("refreshing the graced tokens: %d %v", st, next)
	}
	if st, _ := v.postForm("/oauth/token", form); st != 400 {
		t.Fatalf("a token two rotations back must fail, got %d", st)
	}
	if st, _, _ := v.mcp(next["access_token"].(string), "list_tasks", "{}"); st != 401 {
		t.Fatalf("and end the grant, got %d", st)
	}
	for _, k := range v.mr.Keys() {
		if strings.Contains(k, "rt-grace:") {
			t.Fatalf("an ended grant leaves no grace record: %s", k)
		}
	}
}

// The grace is for the client the token was issued to: another client
// presenting it within the window ends the grant.
func TestOAuthRefreshReuseOtherClient(t *testing.T) {
	v := newOAuthEnv(t)
	meta := `{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`
	cid, other := v.register(meta), v.register(meta)
	tok := v.login(cid, "http://127.0.0.1/callback", "alice")
	rt := tok["refresh_token"].(string)
	_, fresh := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {cid}})
	if st, out := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {other}}); st != 400 || out["access_token"] != nil {
		t.Fatalf("another client must not get the tokens: %d %v", st, out)
	}
	if st, _, _ := v.mcp(fresh["access_token"].(string), "list_tasks", "{}"); st != 401 {
		t.Fatalf("reuse by another client should end the grant, got %d", st)
	}
	if st, _ := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {cid}}); st != 400 {
		t.Fatalf("nor is there grace left for the client itself, got %d", st)
	}
}

// A confidential client must authenticate at the token endpoint.
func TestOAuthConfidentialClient(t *testing.T) {
	v := newOAuthEnv(t)
	req, _ := http.NewRequest("POST", v.url+"/oauth/register", strings.NewReader(`{"redirect_uris":["https://app.example/cb"],"grant_types":["authorization_code","refresh_token"]}`))
	_, body := v.do(req)
	var reg map[string]any
	json.Unmarshal([]byte(body), &reg)
	if reg["token_endpoint_auth_method"] != "client_secret_basic" || reg["client_secret"] == nil {
		t.Fatalf("default registration is confidential: %s", body)
	}
	if st, out := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"x"}, "client_id": {reg["client_id"].(string)}}); st != 401 || out["error"] != "invalid_client" {
		t.Fatalf("no secret: want invalid_client, got %d %v", st, out)
	}
	for _, bad := range []string{`{"redirect_uris":["http://app.example/cb"]}`, `{"redirect_uris":["javascript:alert(1)"]}`, `{"grant_types":["authorization_code"]}`, `{"grant_types":["password"],"redirect_uris":["https://a.example/cb"]}`} {
		req, _ := http.NewRequest("POST", v.url+"/oauth/register", strings.NewReader(bad))
		if resp, body := v.do(req); resp.StatusCode != 400 {
			t.Errorf("%s should be rejected: %d %s", bad, resp.StatusCode, body)
		}
	}
}

// Headless callers sign in once with the device flow, then live on
// refresh tokens.
func TestOAuthDeviceFlow(t *testing.T) {
	v := newOAuthEnv(t)
	cid := v.register(`{"client_name":"cron","token_endpoint_auth_method":"none","grant_types":["urn:ietf:params:oauth:grant-type:device_code","refresh_token"]}`)
	st, da := v.postForm("/oauth/device_authorization", url.Values{"client_id": {cid}})
	if st != 200 || da["device_code"] == nil || da["verification_uri"] != v.url+"/oauth/device" {
		t.Fatalf("device authorization: %d %v", st, da)
	}
	dc, uc := da["device_code"].(string), da["user_code"].(string)
	poll := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {dc}, "client_id": {cid}}
	if st, out := v.postForm("/oauth/token", poll); st != 400 || out["error"] != "authorization_pending" {
		t.Fatalf("before sign-in: %d %v", st, out)
	}
	if _, out := v.postForm("/oauth/token", poll); out["error"] != "slow_down" {
		t.Fatalf("polling too fast: %v", out)
	}

	// No link with the code in it: that is what a phisher would send.
	if _, ok := da["verification_uri_complete"]; ok {
		t.Fatalf("the response must not carry verification_uri_complete: %v", da)
	}
	// The person opens the page, types the code, signs in at the forge. A
	// code in the link is never filled in.
	resp, body := v.get(v.url + "/oauth/device?user_code=" + url.QueryEscape(uc))
	if resp.StatusCode != 200 || strings.Contains(body, uc) || !strings.Contains(body, "If someone sent you this code") {
		t.Fatalf("device page: %d %s", resp.StatusCode, body)
	}
	submit := func(origin string) *http.Response {
		req, _ := http.NewRequest("POST", v.url+"/oauth/device", strings.NewReader(url.Values{"user_code": {strings.ToLower(uc)}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, _ := v.do(req)
		return resp
	}
	if resp := submit("https://evil.example"); resp.StatusCode != 403 {
		t.Fatalf("a cross-site post must be refused, got %d", resp.StatusCode)
	}
	resp = submit(v.url)
	if resp.StatusCode != 302 {
		t.Fatalf("submitting the code should go to the forge: %d", resp.StatusCode)
	}
	cb, _ := v.f.SignIn(resp.Header.Get("Location"), "alice")
	_, body = v.get(cb)
	if !strings.Contains(body, uc) || !strings.Contains(body, "cron") || !strings.Contains(body, "Deny this unless you started the sign-in yourself") || consentToken.FindStringSubmatch(body) == nil {
		t.Fatalf("the consent page should name the client and the device's code, and warn: %s", body)
	}
	if resp := v.postConsent(consentToken.FindStringSubmatch(body)[1], "allow", v.url); resp.StatusCode != 200 {
		t.Fatalf("approving the device: %d", resp.StatusCode)
	}

	v.advance(6 * time.Second)
	st, tok := v.postForm("/oauth/token", poll)
	if st != 200 || tok["access_token"] == nil {
		t.Fatalf("after sign-in the device gets tokens: %d %v", st, tok)
	}
	if st, out := v.postForm("/oauth/token", poll); st != 400 || out["error"] != "expired_token" {
		t.Fatalf("a device code works once: %d %v", st, out)
	}
	if st, text, isErr := v.mcp(tok["access_token"].(string), "file_task", `{"repo":"fake:acme/app","title":"nightly","acceptance_criteria":"- x"}`); st != 200 || isErr {
		t.Fatalf("the device acts as alice: %d %s", st, text)
	}
	// It keeps working on refresh tokens.
	v.advance(2 * time.Hour)
	st, fresh := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}, "client_id": {cid}})
	if st != 200 {
		t.Fatalf("device refresh: %d %v", st, fresh)
	}
	if st, _, _ := v.mcp(fresh["access_token"].(string), "list_tasks", "{}"); st != 200 {
		t.Fatalf("refreshed device token: %d", st)
	}
	v.assertNoLeak(dc, tok["access_token"].(string), tok["refresh_token"].(string), fresh["access_token"].(string), fresh["refresh_token"].(string))
}

// auth.mode none is today's behaviour: the anonymous principal, no OAuth
// endpoints, and a bearer token is neither needed nor checked.
func TestNoneModeHasNoOAuth(t *testing.T) {
	s, _ := newServer(t, "anon")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/.well-known/oauth-protected-resource", nil))
	if w.Code != 404 {
		t.Fatalf("no OAuth metadata in mode none, got %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/api/tasks", nil)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("anonymous list: %d %s", w.Code, w.Body.String())
	}
}

var consentToken = regexp.MustCompile(`name="consent" value="([^"]+)"`)

// consent follows the forge's redirect back to devbot's callback, then
// answers devbot's consent page with decision, posted from origin. It
// returns the consent post's response.
func (v *oauthEnv) consent(cb, decision, origin string) *http.Response {
	v.t.Helper()
	resp, body := v.get(cb)
	m := consentToken.FindStringSubmatch(body)
	if resp.StatusCode != 200 || m == nil {
		v.t.Fatalf("the callback should ask for consent: %d %s", resp.StatusCode, body)
	}
	return v.postConsent(m[1], decision, origin)
}

func (v *oauthEnv) postConsent(token, decision, origin string) *http.Response {
	req, _ := http.NewRequest("POST", v.url+"/oauth/consent", strings.NewReader(url.Values{"consent": {token}, "decision": {decision}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", origin)
	resp, _ := v.do(req)
	return resp
}

// Anyone can register a client, so devbot asks the person before a client
// gets their sign-in, and only finishes a sign-in in the browser that
// started it.
func TestOAuthConsentAndBrowserBinding(t *testing.T) {
	v := newOAuthEnv(t)
	ctx := context.Background()
	evil := v.register(`{"client_name":"Claude Code","redirect_uris":["https://evil.example/cb"],"token_endpoint_auth_method":"none"}`)
	verifier := oauth2.GenerateVerifier()
	q := url.Values{"client_id": {evil}, "redirect_uri": {"https://evil.example/cb"}, "response_type": {"code"}, "state": {"x"},
		"code_challenge": {oauth2.S256ChallengeFromVerifier(verifier)}, "code_challenge_method": {"S256"}}
	authorize := v.url + "/oauth/authorize?" + q.Encode()

	// The victim follows the link: the consent page says where they'd go.
	resp, _ := v.get(authorize)
	cb, _ := v.f.SignIn(resp.Header.Get("Location"), "alice")
	_, body := v.get(cb)
	if !strings.Contains(body, "https://evil.example") || consentToken.FindStringSubmatch(body) == nil {
		t.Fatalf("consent should show where devbot would send the person: %s", body)
	}
	token := consentToken.FindStringSubmatch(body)[1]
	if resp := v.postConsent(token, "allow", "https://evil.example"); resp.StatusCode != 403 {
		t.Fatalf("a cross-site consent post must be refused, got %d", resp.StatusCode)
	}
	resp = v.postConsent(token, "deny", v.url)
	if loc, _ := url.Parse(resp.Header.Get("Location")); resp.StatusCode != 302 || loc.Query().Get("error") != "access_denied" || loc.Query().Get("code") != "" {
		t.Fatalf("deny should send an error, not a code: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, k := range v.mr.Keys() {
		if strings.HasPrefix(k, "devbot:auth:grant:") {
			t.Fatalf("a denied sign-in must leave no grant: %s", k)
		}
	}
	if resp := v.postConsent(token, "allow", v.url); resp.StatusCode != 400 {
		t.Fatalf("a consent is answered once, got %d", resp.StatusCode)
	}

	// A forge login link someone else started can't be finished in the
	// victim's browser: the device-code phishing case.
	attacker := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	cid := v.register(`{"client_name":"cron","token_endpoint_auth_method":"none","grant_types":["urn:ietf:params:oauth:grant-type:device_code"]}`)
	_, da := v.postForm("/oauth/device_authorization", url.Values{"client_id": {cid}})
	req, _ := http.NewRequestWithContext(ctx, "POST", v.url+"/oauth/device", strings.NewReader(url.Values{"user_code": {da["user_code"].(string)}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", v.url) // forged: it isn't a browser
	aresp, err := attacker.Do(req)
	if err != nil || aresp.StatusCode != 302 {
		t.Fatalf("attacker's submit: %v %v", aresp, err)
	}
	cb, _ = v.f.SignIn(aresp.Header.Get("Location"), "alice")
	if resp, body := v.get(cb); resp.StatusCode != 400 || !strings.Contains(body, "another browser") {
		t.Fatalf("a sign-in started elsewhere must not complete here: %d %s", resp.StatusCode, body)
	}
}

// A forge that is down isn't a forge saying no: nobody is signed out.
func TestOAuthForgeOutageKeepsGrant(t *testing.T) {
	v := newOAuthEnv(t)
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	v.f.SetTokenTTL(10 * time.Minute)
	tok := v.login(cid, "http://127.0.0.1/callback", "alice")
	v.advance(9*time.Minute + 30*time.Second)
	v.f.RefreshOutage(true)
	if st, _, _ := v.mcp(tok["access_token"].(string), "list_tasks", "{}"); st != 503 {
		t.Fatalf("an outage should be a 503, got %d", st)
	}
	if st, out := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}, "client_id": {cid}}); st != 503 {
		t.Fatalf("refresh during an outage should be retryable, got %d %v", st, out)
	}
	v.f.RefreshOutage(false)
	if st, _, _ := v.mcp(tok["access_token"].(string), "list_tasks", "{}"); st != 200 {
		t.Fatalf("after the outage the same sign-in works, got %d", st)
	}
	// The refresh token survived the failed attempt.
	if st, out := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}, "client_id": {cid}}); st != 200 {
		t.Fatalf("refresh after the outage: %d %v", st, out)
	}
}

// Concurrent redemptions of one refresh token by its client, as processes
// sharing a sign-in do: all of them get the same new tokens, which work.
func TestOAuthConcurrentRefresh(t *testing.T) {
	v := newOAuthEnv(t)
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	tok := v.login(cid, "http://127.0.0.1/callback", "alice")
	results := v.concurrentRefresh(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}, "client_id": {cid}})
	if len(results) != 8 {
		t.Fatalf("want 8 successful refreshes, got %d", len(results))
	}
	for _, r := range results[1:] {
		if r["access_token"] != results[0]["access_token"] || r["refresh_token"] != results[0]["refresh_token"] {
			t.Fatalf("every refresh should get the same tokens: %v vs %v", r, results[0])
		}
	}
	if results[0]["refresh_token"] == tok["refresh_token"] {
		t.Fatal("the refresh token should have rotated")
	}
	if st, _, _ := v.mcp(results[0]["access_token"].(string), "list_tasks", "{}"); st != 200 {
		t.Fatalf("the shared tokens should work: %d", st)
	}
	if st, out := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {results[0]["refresh_token"].(string)}, "client_id": {cid}}); st != 200 {
		t.Fatalf("and refresh: %d %v", st, out)
	}
}

// Without a grace window, exactly one of them gets tokens.
func TestOAuthConcurrentRefreshNoGrace(t *testing.T) {
	v := newOAuthEnvWith(t, func(c *config.Config) { var off time.Duration; c.Auth.RefreshReuseGrace = &off })
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	tok := v.login(cid, "http://127.0.0.1/callback", "alice")
	if n := len(v.concurrentRefresh(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}, "client_id": {cid}})); n != 1 {
		t.Fatalf("want exactly one successful refresh, got %d", n)
	}
}

// concurrentRefresh sends form to the token endpoint 8 times at once, and
// returns the successful responses.
func (v *oauthEnv) concurrentRefresh(form url.Values) []map[string]any {
	out := make(chan map[string]any, 8)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := http.PostForm(v.url+"/oauth/token", form)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			var m map[string]any
			if json.NewDecoder(resp.Body).Decode(&m) == nil && resp.StatusCode == 200 {
				out <- m
			}
		}()
	}
	close(start)
	wg.Wait()
	close(out)
	var res []map[string]any
	for m := range out {
		res = append(res, m)
	}
	return res
}

// An authorization code presented a second time was seen by someone else:
// the grant it produced ends, tokens and all.
func TestOAuthCodeReuseEndsGrant(t *testing.T) {
	v := newOAuthEnv(t)
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	tok := v.login(cid, "http://127.0.0.1/callback", "alice")
	if st, _, _ := v.mcp(tok["access_token"].(string), "list_tasks", "{}"); st != 200 {
		t.Fatalf("the first redemption's token works: %d", st)
	}
	if st, out := v.postForm("/oauth/token", v.lastCode); st != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("a replayed code must fail: %d %v", st, out)
	}
	if st, _, _ := v.mcp(tok["access_token"].(string), "list_tasks", "{}"); st != 401 {
		t.Fatalf("the replay should end the grant, got %d", st)
	}
	if st, out := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}, "client_id": {cid}}); st != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("its refresh token must be dead too: %d %v", st, out)
	}
	// Another sign-in of the same person is untouched.
	other := v.login(cid, "http://127.0.0.1/callback", "alice")
	if st, _, _ := v.mcp(other["access_token"].(string), "list_tasks", "{}"); st != 200 {
		t.Fatalf("another grant should still work: %d", st)
	}
	v.assertNoLeak(tok["access_token"].(string), tok["refresh_token"].(string), v.lastCode.Get("code"))
}

// auth.dynamic_device_grant: false: no device grant for any client (all of
// them register themselves), and a device's earlier sign-in can't refresh.
func TestOAuthDeviceGrantOff(t *testing.T) {
	v := newOAuthEnvWith(t, func(c *config.Config) { c.Auth.DynamicDeviceGrant = false })
	_, body := v.get(v.url + "/.well-known/oauth-authorization-server")
	if strings.Contains(body, "device_authorization_endpoint") || strings.Contains(body, "device_code") {
		t.Fatalf("metadata must not offer the device grant: %s", body)
	}
	req, _ := http.NewRequest("POST", v.url+"/oauth/register", strings.NewReader(`{"token_endpoint_auth_method":"none","grant_types":["urn:ietf:params:oauth:grant-type:device_code"]}`))
	if resp, body := v.do(req); resp.StatusCode != 400 {
		t.Fatalf("registering for the device grant must fail: %d %s", resp.StatusCode, body)
	}
	// A client registered before the switch can't start or finish one.
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	on, err := auth.New(auth.Options{ResourceURL: v.url, Forge: v.f, Redis: v.s.E.Q.R, Key: []byte("an-auth-key-for-tests-that-is-long-enough"),
		DeviceGrant: true, AccessTTL: time.Hour, RefreshTTL: 30 * 24 * time.Hour, Log: slog.New(slog.NewTextHandler(v.logs, nil)), Now: v.clock})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	on.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", v.url+"/oauth/register", strings.NewReader(`{"token_endpoint_auth_method":"none","grant_types":["urn:ietf:params:oauth:grant-type:device_code","refresh_token"]}`)))
	var reg map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &reg); rec.Code != 201 {
		t.Fatalf("registering a device client before the switch: %d %s", rec.Code, rec.Body)
	}
	dev := reg["client_id"].(string)
	if st, out := v.postForm("/oauth/device_authorization", url.Values{"client_id": {dev}}); st != 400 || out["error"] != "unauthorized_client" {
		t.Fatalf("device authorization must be refused: %d %v", st, out)
	}
	if st, out := v.postForm("/oauth/token", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {"x"}, "client_id": {dev}}); st != 400 || out["error"] != "unsupported_grant_type" {
		t.Fatalf("polling must be refused: %d %v", st, out)
	}
	if st, out := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"x"}, "client_id": {dev}}); st != 400 || out["error"] != "unauthorized_client" {
		t.Fatalf("a device client's refresh must be refused: %d %v", st, out)
	}
	if resp, _ := v.get(v.url + "/oauth/device"); resp.StatusCode != 404 {
		t.Fatalf("the device page should be off: %d", resp.StatusCode)
	}
	// The code flow is unaffected.
	if tok := v.login(cid, "http://127.0.0.1/callback", "alice"); tok["access_token"] == nil {
		t.Fatal("the authorization code flow should still work")
	}
}

// Entering device codes is rate-limited: a code tried too often is locked
// out and the device is told no; an address that gets too many wrong is
// turned away, right code or not.
func TestOAuthDeviceEntryRateLimited(t *testing.T) {
	enterFrom := func(v *oauthEnv, code, forwardedFor string) int {
		req, _ := http.NewRequest("POST", v.url+"/oauth/device", strings.NewReader(url.Values{"user_code": {code}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", v.url)
		if forwardedFor != "" {
			req.Header.Set("X-Forwarded-For", forwardedFor)
		}
		resp, _ := v.do(req)
		return resp.StatusCode
	}
	enter := func(v *oauthEnv, code string) int { return enterFrom(v, code, "") }
	start := func(v *oauthEnv) (cid, dc, uc string) {
		cid = v.register(`{"client_name":"cron","token_endpoint_auth_method":"none","grant_types":["urn:ietf:params:oauth:grant-type:device_code"]}`)
		_, da := v.postForm("/oauth/device_authorization", url.Values{"client_id": {cid}})
		return cid, da["device_code"].(string), da["user_code"].(string)
	}

	t.Run("per code", func(t *testing.T) {
		v := newOAuthEnv(t)
		cid, dc, uc := start(v)
		for i := range 5 {
			if st := enter(v, uc); st != 302 {
				t.Fatalf("attempt %d: %d", i+1, st)
			}
		}
		if st := enter(v, uc); st != 429 {
			t.Fatalf("a code tried too often should be locked out, got %d", st)
		}
		if st := enter(v, uc); st != 429 {
			t.Fatalf("and stay locked out, got %d", st)
		}
		st, out := v.postForm("/oauth/token", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {dc}, "client_id": {cid}})
		if st != 400 || out["error"] != "access_denied" {
			t.Fatalf("the device should hear no: %d %v", st, out)
		}
	})

	t.Run("per address", func(t *testing.T) {
		v := newOAuthEnv(t)
		_, _, uc := start(v)
		for i := range 10 {
			if st := enter(v, "BBBB-BBB"+string(userCodeChar(i))); st != 400 {
				t.Fatalf("wrong code %d: %d", i+1, st)
			}
		}
		if st := enter(v, uc); st != 429 {
			t.Fatalf("after too many wrong codes, even the right one is turned away: %d", st)
		}
		v.advance(11 * time.Minute)
		_, _, uc = start(v)
		if st := enter(v, uc); st != 302 {
			t.Fatalf("the limit lapses: %d", st)
		}
	})

	t.Run("forwarded for, from an untrusted peer", func(t *testing.T) {
		// By default no proxy is trusted, and the header is the client's to
		// choose, so it can't buy a fresh address.
		v := newOAuthEnv(t)
		_, _, uc := start(v)
		for i := range 10 {
			enterFrom(v, "BBBB-BBB"+string(userCodeChar(i)), "203.0.113."+string('0'+byte(i)))
		}
		if st := enterFrom(v, uc, "198.51.100.7"); st != 429 {
			t.Fatalf("X-Forwarded-For from an untrusted peer must be ignored: %d", st)
		}
	})

	t.Run("behind a trusted proxy", func(t *testing.T) {
		// The test's requests all come from 127.0.0.1, as they would from
		// the chart's Istio sidecar.
		v := newOAuthEnvWith(t, func(c *config.Config) { c.Proxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")} })
		_, _, uc := start(v)
		for i := range 10 {
			if st := enterFrom(v, "BBBB-BBB"+string(userCodeChar(i)), "203.0.113.5"); st != 400 {
				t.Fatalf("wrong code %d: %d", i+1, st)
			}
		}
		if st := enterFrom(v, "BBBB-BBBB", "203.0.113.5"); st != 429 {
			t.Fatalf("the guessing client is turned away: %d", st)
		}
		// A hop it made up in front of its own doesn't hide it.
		if st := enterFrom(v, "BBBB-BBBB", "198.51.100.7, 203.0.113.5"); st != 429 {
			t.Fatalf("a forged earlier hop must not escape the limit: %d", st)
		}
		if st := enterFrom(v, uc, "198.51.100.7"); st != 302 {
			t.Fatalf("another client must not be turned away: %d", st)
		}
		// A request through the proxy that names no client shares no
		// counter, so wrong codes there turn nobody away.
		for i := range 12 {
			if st := enter(v, "BBBB-BBC"+string(userCodeChar(i))); st != 400 {
				t.Fatalf("wrong code %d with no client address: %d", i+1, st)
			}
		}
		_, _, uc = start(v)
		if st := enter(v, uc); st != 302 {
			t.Fatalf("no client address means no shared lockout: %d", st)
		}
	})
}

// With dynamic_device_grant off, a device's sign-in ends at its next
// refresh even when its client is also registered for the code flow; the
// client's code-flow sign-ins carry on.
func TestOAuthDeviceGrantOffEndsDeviceSignIns(t *testing.T) {
	v := newOAuthEnv(t)
	cid := v.register(`{"client_name":"both","redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","urn:ietf:params:oauth:grant-type:device_code","refresh_token"]}`)
	_, da := v.postForm("/oauth/device_authorization", url.Values{"client_id": {cid}})
	req, _ := http.NewRequest("POST", v.url+"/oauth/device", strings.NewReader(url.Values{"user_code": {da["user_code"].(string)}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", v.url)
	resp, _ := v.do(req)
	cb, _ := v.f.SignIn(resp.Header.Get("Location"), "alice")
	_, body := v.get(cb)
	v.postConsent(consentToken.FindStringSubmatch(body)[1], "allow", v.url)
	v.advance(6 * time.Second)
	_, device := v.postForm("/oauth/token", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {da["device_code"].(string)}, "client_id": {cid}})
	if device["refresh_token"] == nil {
		t.Fatalf("device sign-in: %v", device)
	}
	browser := v.login(cid, "http://127.0.0.1/callback", "alice")

	// The install turns the device grant off and restarts.
	off, err := auth.New(auth.Options{ResourceURL: v.url, Forge: v.f, Redis: v.s.E.Q.R, Key: []byte("an-auth-key-for-tests-that-is-long-enough"),
		AccessTTL: time.Hour, RefreshTTL: 30 * 24 * time.Hour, Log: slog.New(slog.NewTextHandler(v.logs, nil)), Now: v.clock})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	off.Routes(mux)
	refresh := func(rt string) (int, string) {
		req := httptest.NewRequest("POST", v.url+"/oauth/token", strings.NewReader(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {cid}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if st, body := refresh(device["refresh_token"].(string)); st != 400 || !strings.Contains(body, "invalid_grant") {
		t.Fatalf("the device's refresh must be refused: %d %s", st, body)
	}
	if st, body := refresh(browser["refresh_token"].(string)); st != 200 {
		t.Fatalf("the code-flow sign-in should still refresh: %d %s", st, body)
	}
}

func userCodeChar(i int) byte { return "BCDFGHJKLMNPQRSTVWXZ"[i] }

// Each sign-in event, token issue, refresh, revocation and refusal is one
// audit record, with the request it came in and the person it was for; what
// a signed-in person does through a connector is recorded as theirs; and no
// token, code or secret is ever in one.
func TestOAuthAuditTrail(t *testing.T) {
	// No reuse grace, so the reused refresh token below is refused.
	v := newOAuthEnvWith(t, func(c *config.Config) { var none time.Duration; c.Auth.RefreshReuseGrace = &none })
	if st, _, _ := v.mcp("", "list_tasks", "{}"); st != 401 {
		t.Fatalf("want 401, got %d", st)
	}
	refused := v.audit.Of("authz.refused")
	if len(refused) != 1 || refused[0]["request_id"] == "" || audittest.Detail(refused[0])["action"] != "POST /mcp" {
		t.Fatalf("an unauthenticated call is a refusal: %v", v.audit.Records())
	}

	req, _ := http.NewRequest("POST", v.url+"/oauth/register", strings.NewReader(`{"client_name":"c","redirect_uris":["http://127.0.0.1/callback"],"grant_types":["authorization_code","refresh_token"]}`))
	req.Header.Set("X-Request-Id", "proxy-req-1")
	resp, body := v.do(req)
	var reg map[string]any
	json.Unmarshal([]byte(body), &reg)
	cid, secret := reg["client_id"].(string), reg["client_secret"].(string)
	if resp.Header.Get("X-Request-Id") != "proxy-req-1" {
		t.Fatalf("the proxy's request ID is echoed: %q", resp.Header.Get("X-Request-Id"))
	}
	if r := v.audit.Of("auth.client.register"); len(r) != 1 || r[0]["request_id"] != "proxy-req-1" || audittest.Detail(r[0])["client_id"] != cid {
		t.Fatalf("registration: %v", r)
	}

	// The confidential client's login, by hand: v.login sends no secret.
	verifier := oauth2.GenerateVerifier()
	q := url.Values{"client_id": {cid}, "redirect_uri": {"http://127.0.0.1/callback"}, "response_type": {"code"}, "state": {"s"},
		"code_challenge": {oauth2.S256ChallengeFromVerifier(verifier)}, "code_challenge_method": {"S256"}}
	resp, _ = v.get(v.url + "/oauth/authorize?" + q.Encode())
	cb, err := v.f.SignIn(resp.Header.Get("Location"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	back, _ := url.Parse(v.consent(cb, "allow", v.url).Header.Get("Location"))
	code := back.Query().Get("code")
	token := func(f url.Values) map[string]any {
		f.Set("client_id", cid)
		f.Set("client_secret", secret)
		st, out := v.postForm("/oauth/token", f)
		if st != 200 {
			t.Fatalf("token: %d %v", st, out)
		}
		return out
	}
	tok := token(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"http://127.0.0.1/callback"}, "code_verifier": {verifier}})
	at, rt := tok["access_token"].(string), tok["refresh_token"].(string)
	tok2 := token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}})
	at2, rt2 := tok2["access_token"].(string), tok2["refresh_token"].(string)
	// The old refresh token again: refused, and the grant ends.
	if st, _ := v.postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {cid}, "client_secret": {secret}}); st != 400 {
		t.Fatalf("reuse: %d", st)
	}

	for event, want := range map[string]int{"auth.sign_in": 1, "auth.token.issue": 1, "auth.token.refresh": 2, "auth.revoke": 1} {
		if got := v.audit.Of(event); len(got) != want {
			t.Errorf("%s: want %d records, got %d: %v", event, want, len(got), got)
		}
	}
	if r := v.audit.Of("auth.sign_in"); len(r) == 1 && (r[0]["login"] != "alice" || r[0]["outcome"] != "ok") {
		t.Errorf("sign-in: %v", r[0])
	}
	if r := v.audit.Of("auth.token.issue"); len(r) == 1 && (r[0]["login"] != "alice" || audittest.Detail(r[0])["client_id"] != cid) {
		t.Errorf("token issue: %v", r[0])
	}
	if r := v.audit.Of("auth.token.refresh"); len(r) == 2 && (r[0]["outcome"] != "ok" || r[1]["outcome"] != "refused" || audittest.Detail(r[1])["reason"] != "invalid_grant") {
		t.Errorf("refreshes: %v", r)
	}

	// What alice files through a connector, as herself.
	v2 := v.login(v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`), "http://127.0.0.1/callback", "alice")
	v.audit.Reset()
	if st, text, isErr := v.mcp(v2["access_token"].(string), "file_task", `{"repo":"fake:acme/app","title":"t","acceptance_criteria":"- x","origin":"chat:1"}`); st != 200 || isErr {
		t.Fatalf("file_task: %d %s", st, text)
	}
	created := v.audit.Of("forge.issue.create")
	if len(created) != 1 || created[0]["actor"] != "alice" || created[0]["actor_kind"] != "user" || created[0]["login"] != "alice" ||
		created[0]["origin"] != "chat:1" || created[0]["request_id"] == "" {
		t.Fatalf("alice's issue: %v", v.audit.Records())
	}
	for _, r := range v.audit.Of("forge.labels.ensure") {
		if r["actor_kind"] != "implementer" || r["principal"] != "alice" {
			t.Errorf("the bot's write on alice's behalf names both: %v", r)
		}
	}
	v.assertNoLeak(at, rt, at2, rt2, secret, code, v2["access_token"].(string), v2["refresh_token"].(string), v.lastCode.Get("code"))
}

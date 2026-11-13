package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
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

// Each endpoint anyone can reach answers 429 once one address goes over
// its limit, and lets it in again once the bucket refills.
func TestOAuthRateLimits(t *testing.T) {
	v := newOAuthEnvWith(t, func(c *config.Config) {
		two := config.RateLimit{PerMinute: 1, Burst: 2}
		c.Auth.RateLimits = config.RateLimits{Register: two, Authorize: two, DeviceAuthorization: two, Token: two, Device: two}
	})
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/cb"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token","urn:ietf:params:oauth:grant-type:device_code"]}`)
	authorize := v.url + "/oauth/authorize?" + url.Values{"client_id": {cid}, "response_type": {"code"}, "redirect_uri": {"http://127.0.0.1/cb"},
		"code_challenge": {strings.Repeat("a", 43)}, "code_challenge_method": {"S256"}}.Encode()
	cases := map[string]func() *http.Response{
		"register": func() *http.Response {
			req, _ := http.NewRequest("POST", v.url+"/oauth/register", strings.NewReader(`{"redirect_uris":["https://app.example/cb"]}`))
			resp, _ := v.do(req)
			return resp
		},
		"authorize": func() *http.Response { resp, _ := v.get(authorize); return resp },
		"device_authorization": func() *http.Response {
			req, _ := http.NewRequest("POST", v.url+"/oauth/device_authorization", strings.NewReader(url.Values{"client_id": {cid}}.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, _ := v.do(req)
			return resp
		},
		"token": func() *http.Response {
			req, _ := http.NewRequest("POST", v.url+"/oauth/token", strings.NewReader(url.Values{"client_id": {cid}, "grant_type": {"refresh_token"}, "refresh_token": {"nope"}}.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, _ := v.do(req)
			return resp
		},
		"device page": func() *http.Response { resp, _ := v.get(v.url + "/oauth/device"); return resp },
	}
	for name, call := range cases {
		// register already spent one of its two.
		n := 2
		if name == "register" {
			n = 1
		}
		for i := range n {
			if resp := call(); resp.StatusCode == 429 {
				t.Fatalf("%s: request %d is within the burst: 429", name, i+1)
			}
		}
		resp := call()
		if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
			t.Fatalf("%s: over the limit: %d, Retry-After %q", name, resp.StatusCode, resp.Header.Get("Retry-After"))
		}
	}
	v.advance(time.Minute)
	for name, call := range cases {
		if resp := call(); resp.StatusCode == 429 {
			t.Fatalf("%s: a refilled bucket lets the address in: 429", name)
		}
	}
}

// One address can register only so many clients a day, however slowly.
func TestOAuthClientsPerAddress(t *testing.T) {
	v := newOAuthEnvWith(t, func(c *config.Config) { c.Auth.RateLimits.ClientsPerAddress = 3 })
	for range 3 {
		v.register(`{"redirect_uris":["https://app.example/cb"]}`)
		v.advance(time.Minute)
	}
	req, _ := http.NewRequest("POST", v.url+"/oauth/register", strings.NewReader(`{"redirect_uris":["https://app.example/cb"]}`))
	if resp, body := v.do(req); resp.StatusCode != 429 {
		t.Fatalf("a fourth client from one address: %d %s", resp.StatusCode, body)
	}
	v.advance(25 * time.Hour)
	v.register(`{"redirect_uris":["https://app.example/cb"]}`)
}

func TestDeviceGrantWarnsWithoutTrustedProxies(t *testing.T) {
	for _, c := range []struct {
		name    string
		device  bool
		proxies []string
		warn    bool
	}{
		{"device grant, no proxies", true, nil, true},
		{"device grant, proxies", true, []string{"10.0.0.0/8"}, false},
		{"no device grant", false, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logs syncBuffer
			cfg, _ := config.Load("")
			cfg.Auth = config.Auth{Mode: "oauth", ResourceURL: "https://devbot.example", Provider: "fake", AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour, DynamicDeviceGrant: c.device}
			for _, p := range c.proxies {
				cfg.Proxies = append(cfg.Proxies, netip.MustParsePrefix(p))
			}
			reg := forge.Registry{"fake": fake.New()}
			e := &core.Engine{Forges: reg, Q: queue.New(miniredis.RunT(t).Addr(), 0), Cfg: cfg, Repos: &config.RepoCache{Forges: reg}}
			t.Setenv("TEST_DEVBOT_AUTH_KEY", "an-auth-key-for-tests-that-is-long-enough")
			cfg.Auth.KeyEnv = "TEST_DEVBOT_AUTH_KEY"
			if _, err := New(e, cfg, slog.New(slog.NewTextHandler(&logs, nil)), "test"); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(logs.String(), "level=WARN") && strings.Contains(logs.String(), "trusted_proxies"); got != c.warn {
				t.Fatalf("warned %v, want %v: %s", got, c.warn, logs.String())
			}
		})
	}
}

func TestHTTPServerTimeouts(t *testing.T) {
	srv := httpServer(":0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatalf("every timeout should be set: %+v", srv)
	}
}

// countingForge counts webhook parses, which is where the signature is
// checked.
type countingForge struct {
	*fake.Forge
	parsed int
}

func (f *countingForge) ParseWebhook(r *http.Request, b []byte) ([]forge.Event, error) {
	f.parsed++
	return nil, errors.New("bad signature")
}

func TestWebhookBodyLimit(t *testing.T) {
	s, _ := newServer(t, "")
	cf := &countingForge{Forge: fake.New()}
	s.E.Forges["counting"] = cf
	s.Cfg.WebhookMaxBytes = 1 << 10
	h := s.Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/webhooks/counting", strings.NewReader(strings.Repeat("x", 1<<10+1))))
	if w.Code != http.StatusRequestEntityTooLarge || cf.parsed != 0 {
		t.Fatalf("an oversized body is refused before its signature is checked: %d, parsed %d", w.Code, cf.parsed)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/webhooks/counting", strings.NewReader(strings.Repeat("x", 1<<10))))
	if w.Code != 401 || cf.parsed != 1 {
		t.Fatalf("a body at the limit is checked: %d, parsed %d", w.Code, cf.parsed)
	}
}

// A web page can't call the connectors through a browser: another
// origin is refused, and so is a POST that isn't JSON, which a page could
// send without a preflight.
func TestConnectorsRefuseBrowsers(t *testing.T) {
	s, _ := newServer(t, "client")
	s.Cfg.Auth.ResourceURL = "https://devbot.example"
	s.Cfg.AllowedOrigins = []string{"https://chat.example"}
	h := s.Handler()
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	for _, c := range []struct {
		name, path, origin, ctype string
		want                      int
	}{
		{"no origin", "/mcp", "", "application/json", 200},
		{"own origin", "/mcp", "https://devbot.example", "application/json", 200},
		{"allowed origin", "/mcp", "https://chat.example", "application/json; charset=utf-8", 200},
		{"foreign origin", "/mcp", "https://evil.example", "application/json", 403},
		{"rebound name", "/mcp", "http://devbot.example:8080", "application/json", 403},
		{"opaque origin", "/mcp", "null", "application/json", 403},
		{"text/plain", "/mcp", "", "text/plain", 415},
		{"form", "/mcp", "", "application/x-www-form-urlencoded", 415},
		{"no content type", "/mcp", "", "", 415},
		{"api foreign origin", "/api/task/cancel", "https://evil.example", "application/json", 403},
		{"api text/plain", "/api/task/cancel", "", "text/plain", 415},
	} {
		r := httptest.NewRequest("POST", c.path, strings.NewReader(ping))
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.ctype != "" {
			r.Header.Set("Content-Type", c.ctype)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: %d, want %d (%s)", c.name, w.Code, c.want, w.Body.String())
		}
	}
	r := httptest.NewRequest("GET", "/api/tasks", nil)
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("a cross-origin GET must be refused too: %d", w.Code)
	}
}

// DNS rebinding: a page on attacker.example points its name at serve, so
// its reads are same-origin, with no Origin header. In mode none only the
// Host header gives it away.
func TestNoneModeRefusesUnknownHosts(t *testing.T) {
	s, _ := newServer(t, "anonymous")
	s.Cfg.UI.Enabled = true
	s.Cfg.Auth.ResourceURL = "https://devbot.example"
	s.Cfg.AllowedOrigins = []string{"https://chat.example"}
	s.Cfg.AllowedHosts = []string{"devbot", "devbot.devbot.svc.cluster.local"}
	h := s.Handler()
	for _, c := range []struct {
		host string
		want int
	}{
		{"attacker.example", 421},
		{"attacker.example:8080", 421},
		{"devbot.example.attacker.example", 421},
		{"devbot.example", 200},
		{"DEVBOT.example:443", 200},
		{"chat.example", 200},
		{"devbot", 200},
		{"devbot.devbot.svc.cluster.local.", 200},
		{"10.0.0.7:8080", 200},
		{"[::1]:8080", 200},
		{"localhost:8080", 200},
	} {
		for _, p := range []string{"/api/tasks", "/ui/tasks", "/ui/static/app.js"} {
			r := httptest.NewRequest("GET", p, nil)
			r.Host = c.host
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Errorf("GET %s to %s: %d, want %d", p, c.host, w.Code, c.want)
			}
		}
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		r.Host = c.host
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("POST /mcp to %s: %d, want %d", c.host, w.Code, c.want)
		}
	}
	// Webhooks and probes answer whatever name the forge or kubelet uses.
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.Host = "attacker.example"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("/healthz: %d", w.Code)
	}
}

// brokenForge fails the way a forge or Redis does, with details a caller
// mustn't see.
type brokenForge struct{ *fake.Forge }

const internalDetail = "dial tcp 10.1.2.3:443: token ghs_secret"

func (brokenForge) SearchItems(context.Context, string, string) ([]forge.Item, error) {
	return nil, errors.New(internalDetail)
}

func TestErrorsHideInternals(t *testing.T) {
	var logs syncBuffer
	s, f := newServer(t, "client")
	s.Log = slog.New(slog.NewTextHandler(&logs, nil))
	s.E.Forges["fake"] = brokenForge{f}
	h := s.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/tasks", nil))
	id := w.Header().Get("X-Request-Id")
	if w.Code != 500 || strings.Contains(w.Body.String(), "ghs_secret") || id == "" || !strings.Contains(w.Body.String(), id) {
		t.Fatalf("API: %d %q (request %q)", w.Code, w.Body.String(), id)
	}
	if !strings.Contains(logs.String(), "request_id="+id) || !strings.Contains(logs.String(), "ghs_secret") {
		t.Fatalf("the error is logged with the request ID: %s", logs.String())
	}

	out := rpc(t, h, "", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_tasks","arguments":{}}}`)
	text := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if out["result"].(map[string]any)["isError"] != true || strings.Contains(text, "ghs_secret") || !strings.Contains(text, "request ID") {
		t.Fatalf("MCP: %v", out)
	}

	// The caller's own mistakes still say what was wrong.
	out = rpc(t, h, "", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"file_task","arguments":{"repo":"fake:acme/app","title":"t"}}}`)
	if text := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string); !strings.Contains(text, "acceptance_criteria is required") {
		t.Fatalf("a missing field is the caller's to read: %s", text)
	}
	r := httptest.NewRequest("POST", "/api/task/retry", strings.NewReader(`{"ref":"not a ref"}`))
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "want provider:owner/name#number") {
		t.Fatalf("a malformed ref: %d %s", w.Code, w.Body.String())
	}
}

// verify is a subscriber's side of sign, as docs/operating.md describes it.
func verify(secret []byte, header string, body []byte, now time.Time) bool {
	var ts, sig string
	for _, p := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(p, "=")
		switch k {
		case "t":
			ts = v
		case "sha256":
			sig = v
		}
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || now.Sub(time.Unix(t, 0)).Abs() > 5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts + "." + string(body)))
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(want))
}

func TestSubscriberSignature(t *testing.T) {
	secret, body := []byte("s3cret"), []byte(`{"id":"1-0","type":"task.filed"}`)
	at := time.Unix(1_800_000_000, 0)
	h := sign(secret, at, body)
	if !strings.HasPrefix(h, "t=1800000000,sha256=") {
		t.Fatalf("header: %s", h)
	}
	if !verify(secret, h, body, at.Add(time.Minute)) {
		t.Fatal("a fresh delivery verifies")
	}
	if verify(secret, h, body, at.Add(6*time.Minute)) {
		t.Fatal("a delivery replayed after the window must not verify")
	}
	if verify(secret, strings.Replace(h, "t=1800000000", "t=1800000300", 1), body, at.Add(6*time.Minute)) {
		t.Fatal("a replay with a new timestamp must not verify")
	}
	if verify(secret, h, []byte(`{"id":"1-0","type":"task.merged"}`), at) || verify([]byte("other"), h, body, at) {
		t.Fatal("another body or secret must not verify")
	}
}

// A subscriber gets a signed, timestamped delivery.
func TestDeliverSigns(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the delivery loop's first tick")
	}
	got := make(chan *http.Request, 1)
	bodies := make(chan []byte, 1)
	rx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case got <- r:
			bodies <- b
		default:
		}
	}))
	defer rx.Close()
	s, _ := newServer(t, "client")
	t.Setenv("TEST_SUB_SECRET", "s3cret")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Delivery runs on the leader, under its token.
	token, err := s.E.Q.TakeLeader(ctx, "test", time.Minute)
	if err != nil || token == "" {
		t.Fatalf("lease: %q %v", token, err)
	}
	go s.deliver(ctx, config.Subscriber{Name: "t", URL: rx.URL, SecretEnv: "TEST_SUB_SECRET"}, token)
	time.Sleep(100 * time.Millisecond) // the subscriber starts from now
	h := s.Handler()
	rpc(t, h, "", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"file_task","arguments":{"repo":"fake:acme/app","title":"t","acceptance_criteria":"- x"}}}`)
	select {
	case r := <-got:
		if b := <-bodies; !verify([]byte("s3cret"), r.Header.Get("X-Devbot-Signature"), b, time.Now()) {
			t.Fatalf("delivery doesn't verify: %q", r.Header.Get("X-Devbot-Signature"))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no delivery")
	}
}

func TestRedisMemoryMetrics(t *testing.T) {
	info := "# Memory\r\nused_memory:1048576\r\nused_memory_human:1.00M\r\nmaxmemory:201326592\r\nmaxmemory_policy:noeviction\r\n"
	want := "# TYPE devbot_redis_used_memory_bytes gauge\ndevbot_redis_used_memory_bytes 1048576\n" +
		"# TYPE devbot_redis_maxmemory_bytes gauge\ndevbot_redis_maxmemory_bytes 201326592\n"
	if got := redisMemory(info); got != want {
		t.Fatalf("got %q", got)
	}
}

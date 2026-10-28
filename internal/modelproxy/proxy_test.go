package modelproxy

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/agent/agenttest"
	"git.shdw.tech/rob/devbot/internal/agent/claude"
	"git.shdw.tech/rob/devbot/internal/agent/codex"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// upstream records what reached the model's API.
type upstream struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
	body []string
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs, u.body = append(u.reqs, r), append(u.body, string(b))
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) last(t *testing.T) (*http.Request, string) {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		t.Fatal("nothing reached the upstream")
	}
	return u.reqs[len(u.reqs)-1], u.body[len(u.body)-1]
}

// serve runs a proxy and returns what the agent reads from its file.
func serve(t *testing.T, up *Upstream) *agent.ModelProxy {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	state := filepath.Join(t.TempDir(), "proxy.json")
	go Serve(ctx, "127.0.0.1:0", state, &Proxy{Upstream: up, Token: NewToken(), Log: quiet})
	mp, err := agent.ReadModelProxy(ctx, state, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return mp
}

func env(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

// withBase sends what up forwards to base instead, and refuses the rest as
// up does.
func withBase(t *testing.T, up *Upstream, base string) *Upstream {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	real := up.Base
	up.Base = func(method, path string) *url.URL {
		if real(method, path) == nil {
			return nil
		}
		return u
	}
	return up
}

// The real credential replaces the placeholder, wherever the CLI sends it,
// and a request without the pod's token goes nowhere.
func TestProxyAddsTheCredential(t *testing.T) {
	cases := []struct {
		name, harness, path string
		env                 map[string]string
		cred                string
		send                func(r *http.Request, tok string)
		header, want        string
		absent              string
	}{
		{"claude oauth", claude.Name, "/v1/messages?beta=true", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-real", "ANTHROPIC_API_KEY": "sk-ant-api-real"},
			agent.CredOAuthToken, func(r *http.Request, tok string) { r.Header.Set("Authorization", "Bearer "+tok) }, "Authorization", "Bearer sk-ant-oat-real", "X-Api-Key"},
		{"claude api key", claude.Name, "/v1/messages?beta=true", map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api-real"},
			agent.CredAPIKey, func(r *http.Request, tok string) { r.Header.Set("X-Api-Key", tok) }, "X-Api-Key", "sk-ant-api-real", "Authorization"},
		{"codex api key", codex.Name, "/v1/responses?x=1", map[string]string{"CODEX_API_KEY": "sk-proj-real"},
			agent.CredAPIKey, func(r *http.Request, tok string) { r.Header.Set("Authorization", "Bearer "+tok) }, "Authorization", "Bearer sk-proj-real", "X-Api-Key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := newUpstream(t)
			up, err := For(c.harness, "", env(c.env))
			if err != nil {
				t.Fatal(err)
			}
			mp := serve(t, withBase(t, up, api.URL))
			if mp.Credential != c.cred {
				t.Fatalf("credential %q, want %q", mp.Credential, c.cred)
			}
			for k, v := range c.env {
				if strings.Contains(mp.Token, v) {
					t.Fatalf("the placeholder carries %s", k)
				}
			}

			req, _ := http.NewRequest("POST", mp.URL+c.path, strings.NewReader(`{"model":"m"}`))
			c.send(req, mp.Token)
			req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			got, body := api.last(t)
			if got.URL.RequestURI() != c.path || body != `{"model":"m"}` {
				t.Fatalf("forwarded %s %q", got.URL.RequestURI(), body)
			}
			if v := got.Header.Get(c.header); v != c.want {
				t.Fatalf("%s: %q, want %q", c.header, v, c.want)
			}
			if v := got.Header.Get(c.absent); v != "" {
				t.Fatalf("%s reached the API: %q", c.absent, v)
			}
			if got.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" {
				t.Fatal("the CLI's own headers must pass")
			}

			n := len(api.reqs)
			for _, tok := range []string{"", "wrong", c.env["ANTHROPIC_API_KEY"]} {
				req, _ := http.NewRequest("POST", mp.URL+c.path, strings.NewReader("{}"))
				c.send(req, tok)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 401 || !strings.Contains(string(b), "authentication_error") {
					t.Fatalf("token %q: %d %s", tok, resp.StatusCode, b)
				}
			}
			if len(api.reqs) != n {
				t.Fatal("a request without the pod's token was forwarded")
			}
		})
	}
}

func TestForNeedsACredential(t *testing.T) {
	for _, c := range []struct{ harness, region string }{{claude.Name, ""}, {codex.Name, ""}, {claude.Name, "us-east-1"}, {"nope", ""}} {
		if _, err := For(c.harness, c.region, env(nil)); err == nil {
			t.Errorf("%s %q: a proxy with no credential", c.harness, c.region)
		}
	}
}

// Bedrock: the placeholder (a Bedrock API key, to the CLI) becomes SigV4
// for the right service, with the path escaped as the SDKs do it.
func TestProxySignsForBedrock(t *testing.T) {
	keys := map[string]string{"AWS_ACCESS_KEY_ID": "AKIAEXAMPLE", "AWS_SECRET_ACCESS_KEY": "secret", "AWS_SESSION_TOKEN": "session"}
	for _, c := range []struct{ harness, path, service, wantPath string }{
		{claude.Name, "/model/us.anthropic.claude-opus-5-5-v1:0/invoke-with-response-stream", "bedrock", "/model/us.anthropic.claude-opus-5-5-v1%3A0/invoke-with-response-stream"},
		{codex.Name, "/openai/v1/responses", "bedrock-mantle", "/openai/v1/responses"},
	} {
		t.Run(c.harness, func(t *testing.T) {
			api := newUpstream(t)
			up, err := For(c.harness, "us-east-1", env(keys))
			if err != nil {
				t.Fatal(err)
			}
			mp := serve(t, withBase(t, up, api.URL))
			if mp.Credential != agent.CredAWS {
				t.Fatalf("credential %q", mp.Credential)
			}
			req, _ := http.NewRequest("POST", mp.URL+c.path, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+mp.Token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			got, _ := api.last(t)
			if got.RequestURI != c.wantPath {
				t.Fatalf("path %q, want %q", got.RequestURI, c.wantPath)
			}
			auth := got.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/") || !strings.Contains(auth, "/us-east-1/"+c.service+"/aws4_request") ||
				!strings.Contains(auth, "x-amz-security-token") {
				t.Fatalf("Authorization %q", auth)
			}
			if got.Header.Get("X-Amz-Security-Token") != "session" {
				t.Fatal("no session token")
			}
		})
	}
	up, _ := For(claude.Name, "eu-west-1", env(keys))
	if h := up.Base("POST", "/model/x/invoke").Host; h != "bedrock-runtime.eu-west-1.amazonaws.com" {
		t.Errorf("inference goes to %s", h)
	}
	if h := up.Base("GET", "/inference-profiles").Host; h != "bedrock.eu-west-1.amazonaws.com" {
		t.Errorf("the control plane is %s", h)
	}
	if h := up.Base("GET", "/inference-profiles/us.anthropic.claude-haiku-5-5").Host; h != "bedrock.eu-west-1.amazonaws.com" {
		t.Errorf("an inference profile is at %s", h)
	}
	up, _ = For(codex.Name, "eu-west-1", env(keys))
	if h := up.Base("POST", "/openai/v1/responses").Host; h != "bedrock-mantle.eu-west-1.api.aws" {
		t.Errorf("mantle is %s", h)
	}
}

// The pod's role may allow more of Bedrock's control plane than Claude Code
// needs, so the proxy signs nothing there but inference profiles.
func TestProxyRefusesOtherBedrockPaths(t *testing.T) {
	keys := map[string]string{"AWS_ACCESS_KEY_ID": "AKIAEXAMPLE", "AWS_SECRET_ACCESS_KEY": "secret"}
	up, err := For(claude.Name, "us-east-1", env(keys))
	if err != nil {
		t.Fatal(err)
	}
	api := newUpstream(t)
	mp := serve(t, withBase(t, up, api.URL))
	for path, want := range map[string]int{
		"/model/m/invoke":             200,
		"/inference-profiles":         200,
		"/inference-profiles/p":       200,
		"/guardrails":                 403,
		"/custom-models":              403,
		"/inference-profilesx":        403,
		"/":                           403,
		"/model-customization-jobs":   403,
		"/foundation-model-agreement": 403,
		"/model/arn%3Aaws%3Abedrock%3Aus-east-1%3A1%3Aapplication-inference-profile%2Fp/invoke": 200,
		"/inference-profiles/../guardrails":                                                     403,
		"/inference-profiles/%2e%2e/guardrails":                                                 403,
		"/inference-profiles/..%2fguardrails":                                                   403,
		"/inference-profiles/./p":                                                               403,
		"/model/../guardrails":                                                                  403,
	} {
		req, _ := http.NewRequest("GET", mp.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+mp.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: %d, want %d", path, resp.StatusCode, want)
		}
	}
	if n := len(api.reqs); n != 4 {
		t.Fatalf("%d requests reached Bedrock, want 4", n)
	}
}

// The rest of the vendor's API holds data of every workload the credential
// reaches (files, batches, vector stores, the organization), so the proxy
// forwards only the routes the CLI calls, and nothing else gets that far.
func TestProxyRefusesOtherAPIRoutes(t *testing.T) {
	cases := []struct {
		name, harness, region string
		env                   map[string]string
		routes                map[string]int
	}{
		{"claude", claude.Name, "", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-real"}, map[string]int{
			"POST /v1/messages":                         200,
			"POST /v1/messages?beta=true":               200,
			"POST /v1/messages/count_tokens?beta=true":  200,
			"GET /v1/models":                            200,
			"GET /v1/messages":                          403,
			"POST /v1/messages/":                        403,
			"POST /v1/messagesx":                        403,
			"GET /v1/files":                             403,
			"POST /v1/files":                            403,
			"GET /v1/files/x/content":                   403,
			"GET /v1/messages/batches":                  403,
			"POST /v1/messages/batches":                 403,
			"GET /v1/messages/batches/b/results":        403,
			"GET /v1/organizations/users":               403,
			"GET /v1/organizations/api_keys/k":          403,
			"GET /v1/models/claude-opus-5-5":            403,
			"GET /api/oauth/usage":                      403,
			"POST /v1/messages/../files":                403,
			"POST /v1/messages%2F..%2Ffiles":            403,
			"POST /v1/complete":                         403,
			"GET /":                                     403,
			"POST /v1/messages/count_tokens/../batches": 403,
		}},
		{"codex", codex.Name, "", map[string]string{"CODEX_API_KEY": "sk-proj-real"}, map[string]int{
			"POST /v1/responses":                 200,
			"GET /v1/responses":                  200,
			"GET /v1/models":                     200,
			"GET /v1/responses/resp_1":           403,
			"DELETE /v1/responses":               403,
			"GET /v1/files":                      403,
			"GET /v1/files/x/content":            403,
			"POST /v1/batches":                   403,
			"GET /v1/batches":                    403,
			"GET /v1/vector_stores":              403,
			"POST /v1/vector_stores/vs/files":    403,
			"POST /v1/chat/completions":          403,
			"GET /v1/organization/projects":      403,
			"POST /v1/responses/../files":        403,
			"POST /openai/v1/responses":          403,
			"GET /v1/conversations/c/items":      403,
			"POST /v1/uploads":                   403,
			"GET /v1/fine_tuning/jobs":           403,
			"GET /v1/assistants":                 403,
			"POST /v1/responses/input_items/../": 403,
		}},
		{"codex bedrock", codex.Name, "us-east-1", map[string]string{"AWS_ACCESS_KEY_ID": "AKIAEXAMPLE", "AWS_SECRET_ACCESS_KEY": "secret"}, map[string]int{
			"POST /openai/v1/responses": 200,
			"GET /openai/v1/responses":  200,
			"GET /openai/v1/models":     200,
			"POST /v1/responses":        403,
			"GET /openai/v1/files":      403,
			"POST /openai/v1/batches":   403,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up, err := For(c.harness, c.region, env(c.env))
			if err != nil {
				t.Fatal(err)
			}
			api := newUpstream(t)
			mp := serve(t, withBase(t, up, api.URL))
			forwarded := 0
			for route, want := range c.routes {
				method, path, _ := strings.Cut(route, " ")
				req, _ := http.NewRequest(method, mp.URL+path, strings.NewReader("{}"))
				req.Header.Set("Authorization", "Bearer "+mp.Token)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != want {
					t.Errorf("%s: %d %s, want %d", route, resp.StatusCode, b, want)
				}
				if want == 200 {
					forwarded++
				}
			}
			if n := len(api.reqs); n != forwarded {
				t.Fatalf("%d requests reached the API, want %d", n, forwarded)
			}
		})
	}
}

// Claude Code's connectivity check, a token-less HEAD /api/hello on every
// run, is answered here without a warning and goes no further; every other
// token-less or refused request still warns.
func TestProxyAnswersTheConnectivityCheck(t *testing.T) {
	api := newUpstream(t)
	up, err := For(claude.Name, "", env(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-real"}))
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	var mu sync.Mutex
	p := &Proxy{Upstream: withBase(t, up, api.URL), Token: NewToken(),
		Log: slog.New(slog.NewTextHandler(writerFunc(func(b []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			return logs.Write(b)
		}), &slog.HandlerOptions{Level: slog.LevelDebug}))}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	for _, c := range []struct {
		method, path, tok string
		status            int
		warn              bool
	}{
		{"HEAD", "/api/hello", "", 200, false},
		{"GET", "/api/hello", "", 401, true},
		{"HEAD", "/api/hello/x", "", 401, true},
		{"HEAD", "/v1/models", "", 401, true},
		{"POST", "/v1/messages", "", 401, true},
		{"POST", "/v1/files", p.Token, 403, true},
	} {
		mu.Lock()
		logs.Reset()
		mu.Unlock()
		req, _ := http.NewRequest(c.method, srv.URL+c.path, nil)
		if c.tok != "" {
			req.Header.Set("Authorization", "Bearer "+c.tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		mu.Lock()
		got := logs.String()
		mu.Unlock()
		if resp.StatusCode != c.status || strings.Contains(got, "level=WARN") != c.warn {
			t.Errorf("%s %s: %d, want %d; log %q", c.method, c.path, resp.StatusCode, c.status, got)
		}
	}
	if n := len(api.reqs); n != 0 {
		t.Fatalf("%d requests reached the API", n)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// AWS's own SigV4 test suite (get-vanilla, get-vanilla-query-order-key-case),
// and curl --aws-sigv4's signature of a Bedrock-style request with a ":" in
// the path, a repeated query parameter and a body.
func TestSign(t *testing.T) {
	creds := awsCreds{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	cases := []struct {
		method, url, contentType, body, date, service, want string
	}{
		{"GET", "https://example.amazonaws.com/", "", "", "20150830T123600Z", "service",
			"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"},
		{"GET", "https://example.amazonaws.com/?Param2=value2&Param1=value1", "", "", "20150830T123600Z", "service",
			"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500"},
		{"POST", "http://127.0.0.1:9916/model/us.anthropic.claude-x-v1:0/invoke?b=2&a=1&a=0", "application/json", `{"x":1}`, "20261017T141227Z", "bedrock",
			"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261017/us-east-1/bedrock/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=873187edbd32b0ff910ec99c87211c5fe8ac6c37b60c57d6304913c37d955a7d"},
	}
	for _, c := range cases {
		r, _ := http.NewRequest(c.method, c.url, nil)
		if c.contentType != "" {
			r.Header.Set("Content-Type", c.contentType)
		}
		now, _ := time.Parse("20060102T150405Z", c.date)
		sign(r, []byte(c.body), creds, "us-east-1", c.service, now)
		if got := r.Header.Get("Authorization"); got != c.want {
			t.Errorf("%s %s:\n got %s\nwant %s", c.method, c.url, got, c.want)
		}
	}
}

// IRSA: the projected token, read afresh, buys a temporary credential from
// STS, which is kept until it nears expiry.
func TestWebIdentity(t *testing.T) {
	calls := 0
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		r.ParseForm()
		if r.Form.Get("Action") != "AssumeRoleWithWebIdentity" || r.Form.Get("RoleArn") != "arn:aws:iam::1:role/devbot" || r.Form.Get("WebIdentityToken") != "jwt" {
			http.Error(w, "bad request "+r.Form.Encode(), 400)
			return
		}
		io.WriteString(w, `<AssumeRoleWithWebIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <AssumeRoleWithWebIdentityResult><Credentials>
    <AccessKeyId>ASIAWEB</AccessKeyId><SecretAccessKey>s</SecretAccessKey><SessionToken>tok</SessionToken>
    <Expiration>`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`</Expiration>
  </Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`)
	}))
	defer sts.Close()
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte("jwt\n"), 0o600)
	src, err := awsCredentials(env(map[string]string{"AWS_ROLE_ARN": "arn:aws:iam::1:role/devbot", "AWS_WEB_IDENTITY_TOKEN_FILE": file,
		"AWS_ENDPOINT_URL_STS": sts.URL}), "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		c, err := src(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if c.AccessKeyID != "ASIAWEB" || c.SessionToken != "tok" {
			t.Fatalf("got %+v", c)
		}
	}
	if calls != 1 {
		t.Fatalf("%d STS calls; the credential should be kept", calls)
	}
}

// Pod Identity: the node agent's endpoint, with the projected token as the
// Authorization header.
func TestContainerCredentials(t *testing.T) {
	ep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "pod-jwt" {
			http.Error(w, "no", 403)
			return
		}
		io.WriteString(w, `{"AccessKeyId":"ASIAPOD","SecretAccessKey":"s","Token":"t","Expiration":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
	}))
	defer ep.Close()
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte("pod-jwt"), 0o600)
	src, err := awsCredentials(env(map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": ep.URL, "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": file}), "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	c, err := src(context.Background())
	if err != nil || c.AccessKeyID != "ASIAPOD" || c.SessionToken != "t" {
		t.Fatalf("got %+v %v", c, err)
	}

	os.WriteFile(file, []byte("other"), 0o600)
	src, _ = awsCredentials(env(map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": ep.URL, "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": file}), "us-east-1")
	if _, err := src(context.Background()); err == nil || !strings.Contains(err.Error(), "failed to load AWS credentials") {
		t.Fatalf("want a credential error, got %v", err)
	}
}

// A credential the proxy can't get is an auth error to the CLI.
func TestProxyCredentialFailureIsAuth(t *testing.T) {
	api := newUpstream(t)
	up, err := For(codex.Name, "us-east-1", env(map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://127.0.0.1:1/creds"}))
	if err != nil {
		t.Fatal(err)
	}
	mp := serve(t, withBase(t, up, api.URL))
	req, _ := http.NewRequest("POST", mp.URL+"/openai/v1/responses", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+mp.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(string(b), "failed to load AWS credentials") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if class, _ := codex.Classify("unexpected status 401 Unauthorized: "+string(b), time.Now()); class != agent.ErrAuth {
		t.Fatalf("codex would classify it %q", class)
	}
}

// Codex opens its Responses stream as a WebSocket first: the upgrade, with
// the credential, and the frames both ways pass through.
func TestProxyPassesWebSockets(t *testing.T) {
	var gotAuth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "want an upgrade", 400)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		line, _ := rw.ReadString('\n')
		rw.WriteString("echo " + line)
		rw.Flush()
	}))
	defer api.Close()
	up, _ := For(codex.Name, "", env(map[string]string{"CODEX_API_KEY": "sk-real"}))
	mp := serve(t, withBase(t, up, api.URL))

	conn, err := net.Dial("tcp", strings.TrimPrefix(mp.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET /v1/responses HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer "+mp.Token+"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 101 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	io.WriteString(conn, "hello\n")
	line, _ := br.ReadString('\n')
	if line != "echo hello\n" {
		t.Fatalf("got %q", line)
	}
	if gotAuth != "Bearer sk-real" {
		t.Fatalf("upgrade went with Authorization %q", gotAuth)
	}
}

// End to end, for each harness: the CLI, set up by its harness, calls the
// proxy with what it was given, and the API sees the real credential that
// neither the CLI nor the checks ever had.
func TestHarnessesReachTheModelThroughTheProxy(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl isn't on the PATH")
	}
	secrets := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real", "CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-real", "CODEX_API_KEY": "sk-proj-real",
		"OPENAI_API_KEY": "sk-openai-real", "AWS_ACCESS_KEY_ID": "AKIAREAL", "AWS_SECRET_ACCESS_KEY": "aws-secret-real", "AWS_SESSION_TOKEN": "aws-session-real"}
	// The agent's own environment has them all, as if the chart had leaked
	// them there: none may reach a CLI or a check.
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	leaks := func(t *testing.T, what string, env []string) {
		t.Helper()
		for _, kv := range env {
			for k, v := range secrets {
				if strings.Contains(kv, v) {
					t.Errorf("%s's environment has %s: %s", what, k, kv)
				}
			}
		}
	}

	// A CLI that does one request the way the real one does it with the
	// environment and flags its harness gives it.
	const claudeCLI = `cat >/dev/null
if [ -n "$CLAUDE_CODE_USE_BEDROCK" ]; then
  curl -fsS -H "Authorization: Bearer $AWS_BEARER_TOKEN_BEDROCK" -d '{}' "$ANTHROPIC_BEDROCK_BASE_URL/model/us.anthropic.claude-opus-5-5-v1:0/invoke" >/dev/null || exit 1
elif [ -n "$CLAUDE_CODE_OAUTH_TOKEN" ]; then
  curl -fsS -H "Authorization: Bearer $CLAUDE_CODE_OAUTH_TOKEN" -d '{}' "$ANTHROPIC_BASE_URL/v1/messages" >/dev/null || exit 1
else
  curl -fsS -H "x-api-key: $ANTHROPIC_API_KEY" -d '{}' "$ANTHROPIC_BASE_URL/v1/messages" >/dev/null || exit 1
fi
echo '{"type":"result","subtype":"success","result":"done","session_id":"s1"}'
`
	const codexCLI = `cat >/dev/null
base=; prev=
for a in "$@"; do
  case "$a" in openai_base_url=*|model_providers.amazon-bedrock.base_url=*) base=${a#*=}; base=${base#\"}; base=${base%\"} ;; esac
done
tok=${CODEX_API_KEY:-$AWS_BEARER_TOKEN_BEDROCK}
curl -fsS -H "Authorization: Bearer $tok" -d '{}' "$base/responses" >/dev/null || exit 1
echo '{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`
	cases := []struct {
		name, harness, region string
		cred                  map[string]string // the proxy's
		path, header, want    string
	}{
		{"claude oauth", claude.Name, "", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-real"}, "/v1/messages", "Authorization", "Bearer sk-ant-oat-real"},
		{"claude api key", claude.Name, "", map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real"}, "/v1/messages", "X-Api-Key", "sk-ant-real"},
		{"claude bedrock", claude.Name, "us-east-1", map[string]string{"AWS_ACCESS_KEY_ID": "AKIAREAL", "AWS_SECRET_ACCESS_KEY": "aws-secret-real"},
			"/model/us.anthropic.claude-opus-5-5-v1%3A0/invoke", "Authorization", "AWS4-HMAC-SHA256 Credential=AKIAREAL/"},
		{"codex api key", codex.Name, "", map[string]string{"CODEX_API_KEY": "sk-proj-real"}, "/v1/responses", "Authorization", "Bearer sk-proj-real"},
		{"codex bedrock", codex.Name, "us-east-1", map[string]string{"AWS_ACCESS_KEY_ID": "AKIAREAL", "AWS_SECRET_ACCESS_KEY": "aws-secret-real"},
			"/openai/v1/responses", "Authorization", "AWS4-HMAC-SHA256 Credential=AKIAREAL/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := newUpstream(t)
			up, err := For(c.harness, c.region, env(c.cred))
			if err != nil {
				t.Fatal(err)
			}
			mp := serve(t, withBase(t, up, api.URL))
			var h agent.Harness
			var cli *agenttest.CLI
			if c.harness == claude.Name {
				cli = agenttest.NewCLI(t, "claude", claudeCLI)
				h = &claude.Harness{Command: cli.Path, Proxy: mp}
			} else {
				cli = agenttest.NewCLI(t, "codex", codexCLI)
				h = &codex.Harness{Command: cli.Path, Proxy: mp, HomeDir: t.TempDir(), Log: quiet}
			}
			l := &agent.Local{Harnesses: map[string]agent.Harness{c.harness: h}}
			res, err := l.Run(context.Background(), agent.RunRequest{Harness: c.harness, Dir: t.TempDir(), Prompt: "p", BedrockRegion: c.region})
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError {
				t.Fatalf("the run failed: %+v", res)
			}
			got, _ := api.last(t)
			if got.RequestURI != c.path || !strings.HasPrefix(got.Header.Get(c.header), c.want) {
				t.Fatalf("the API got %s with %s %q", got.RequestURI, c.header, got.Header.Get(c.header))
			}
			leaks(t, c.harness, cli.Last(t).Env)

			out, err := l.Exec(context.Background(), agent.ExecRequest{Dir: t.TempDir(), Script: "env"})
			if err != nil {
				t.Fatal(err)
			}
			leaks(t, "the checks", strings.Split(out.Output, "\n"))
			if strings.Contains(out.Output, mp.Token) {
				t.Error("the checks got the placeholder")
			}
		})
	}
}

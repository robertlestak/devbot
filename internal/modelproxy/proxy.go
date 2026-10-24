// Package modelproxy is the job pod's model proxy: the one container that
// holds the model's credential.
//
// The agent harness's CLI runs as root in the agent container, next to
// whatever a prompt injection makes it run, so a credential there is a
// credential anyone who can steer the model can send away. Instead the CLI
// is pointed at this proxy, on the pod's loopback, with a placeholder token
// minted per pod. The proxy checks the placeholder, puts the real credential
// in its place (or, for Bedrock, signs the request with SigV4) and forwards
// it to the one API its role's model runs on, if it is one of the few
// routes the CLI calls there. The placeholder is useless outside the pod,
// and the proxy reaches nothing else.
package modelproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/agent/claude"
	"git.shdw.tech/rob/devbot/internal/agent/codex"
)

// Upstream is the API a proxy forwards to, and how it authenticates there.
type Upstream struct {
	// Credential is the kind the proxy holds, one of agent's Cred constants.
	Credential string
	// Base is where a request for method and path goes: scheme and host,
	// and a path prefix if any. nil refuses the request.
	Base func(method, path string) *url.URL
	// Auth adds the credential to an outgoing request; body is its body.
	Auth func(ctx context.Context, r *http.Request, body []byte) error
}

// The vendor APIs. Variables, so tests can point them elsewhere.
var (
	AnthropicURL = "https://api.anthropic.com"
	OpenAIURL    = "https://api.openai.com"
)

// For is the upstream of a role that runs harness, on Bedrock in region when
// that is set, with the credential getenv finds: the variables the chart
// mounts from secrets.agents.<harness>, or those EKS injects.
func For(harness, region string, getenv func(string) string) (*Upstream, error) {
	if region != "" {
		creds, err := awsCredentials(getenv, region)
		if err != nil {
			return nil, err
		}
		switch harness {
		case claude.Name:
			// Claude Code calls the runtime for inference and the control
			// plane for inference profiles, both at its one Bedrock base URL.
			// Nothing else of the control plane is signed: the pod's role may
			// allow more there than a prompt-injected model should get.
			runtime := mustParse("https://bedrock-runtime." + region + ".amazonaws.com")
			control := mustParse("https://bedrock." + region + ".amazonaws.com")
			return &Upstream{Credential: agent.CredAWS, Auth: sigV4(creds, region, "bedrock"), Base: func(_, path string) *url.URL {
				switch {
				case strings.HasPrefix(path, "/model/"):
					return runtime
				case path == "/inference-profiles", strings.HasPrefix(path, "/inference-profiles/"):
					return control
				}
				return nil
			}}, nil
		case codex.Name:
			mantle := mustParse("https://bedrock-mantle." + region + ".api.aws")
			return &Upstream{Credential: agent.CredAWS, Auth: sigV4(creds, region, "bedrock-mantle"), Base: allow(mantle, codexRoutes("/openai/v1"))}, nil
		}
		return nil, fmt.Errorf("model proxy: no harness %q", harness)
	}
	switch harness {
	case claude.Name:
		base := allow(mustParse(AnthropicURL), claudeRoutes)
		if tok := getenv("CLAUDE_CODE_OAUTH_TOKEN"); tok != "" {
			return &Upstream{Credential: agent.CredOAuthToken, Base: base, Auth: header("Authorization", "Bearer "+tok)}, nil
		}
		if key := getenv("ANTHROPIC_API_KEY"); key != "" {
			return &Upstream{Credential: agent.CredAPIKey, Base: base, Auth: header("X-Api-Key", key)}, nil
		}
		return nil, errors.New("model proxy: neither CLAUDE_CODE_OAUTH_TOKEN nor ANTHROPIC_API_KEY is set")
	case codex.Name:
		if key := getenv(codex.APIKeyEnv); key != "" {
			return &Upstream{Credential: agent.CredAPIKey, Base: allow(mustParse(OpenAIURL), codexRoutes("/v1")), Auth: header("Authorization", "Bearer "+key)}, nil
		}
		return nil, fmt.Errorf("model proxy: %s is not set", codex.APIKeyEnv)
	}
	return nil, fmt.Errorf("model proxy: no harness %q", harness)
}

func mustParse(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// The routes each CLI calls on its vendor's API, recorded from the pinned
// versions' traffic through the proxy (TestPinnedCLIsThroughTheProxy), plus
// the token count and the model list, which both CLIs have code for outside
// a plain run and which carry nothing of anyone's. The rest of either API -
// files, batches, vector stores, an organization's admin endpoints, whatever
// else an OAuth token's scope reaches - is refused: a prompt-injected model
// could read or make another workload's data there with the install's
// credential.
var claudeRoutes = []string{"POST /v1/messages", "POST /v1/messages/count_tokens", "GET /v1/models"}

// codexRoutes are the Responses API under prefix: GET is its WebSocket,
// which Codex tries first, and compaction is a POST there too.
func codexRoutes(prefix string) []string {
	return []string{"GET " + prefix + "/responses", "POST " + prefix + "/responses", "GET " + prefix + "/models"}
}

// allow sends routes, each "METHOD /path", to base and refuses the rest. A
// path matches whole, so no suffix, trailing slash or dot segment passes.
func allow(base *url.URL, routes []string) func(string, string) *url.URL {
	return func(method, path string) *url.URL {
		if slices.Contains(routes, method+" "+path) {
			return base
		}
		return nil
	}
}

func header(k, v string) func(context.Context, *http.Request, []byte) error {
	return func(_ context.Context, r *http.Request, _ []byte) error {
		r.Header.Set(k, v)
		return nil
	}
}

// maxBody bounds a request the proxy reads whole: it must hash it for SigV4.
// A model request is a conversation, well below this.
const maxBody = 64 << 20

// Proxy forwards the agent harness's requests that carry Token to Upstream.
type Proxy struct {
	Upstream *Upstream
	Token    string
	// Transport reaches the upstream. nil: http.DefaultTransport, which
	// honours HTTPS_PROXY, for an install that sends egress through one.
	Transport http.RoundTripper
	Log       *slog.Logger // nil = slog.Default()
}

// NewToken mints a placeholder.
func NewToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "devbot-proxy-" + hex.EncodeToString(b)
}

func dotSegment(path string) bool {
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// placeholder is the token the request carries, wherever its CLI puts it.
func placeholder(r *http.Request) string {
	if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return v
	}
	return r.Header.Get("X-Api-Key")
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := p.Log
	if log == nil {
		log = slog.Default()
	}
	// Claude Code checks that its base URL answers before every run, with
	// no credential. Expected, so answered here and not a warning: one would
	// bury the refusals that matter.
	if r.Method == http.MethodHead && r.URL.Path == "/api/hello" {
		log.Debug("model proxy: connectivity check", "method", r.Method, "path", r.URL.Path)
		w.WriteHeader(http.StatusOK)
		return
	}
	if subtle.ConstantTimeCompare([]byte(placeholder(r)), []byte(p.Token)) != 1 {
		log.Warn("model proxy: a request without the pod's token", "method", r.Method, "path", r.URL.Path)
		fail(w, http.StatusUnauthorized, "the request doesn't carry this pod's model proxy token")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body) > maxBody {
		fail(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	// Base routes by prefix and an upstream may resolve dot segments, so
	// /inference-profiles/../guardrails would pass as one path and land on
	// another. r.URL.Path is decoded: %2e%2e and ..%2f show up as ".." too.
	// An encoded slash alone is fine: a Bedrock ARN model ID has one.
	var base *url.URL
	if !dotSegment(r.URL.Path) {
		base = p.Upstream.Base(r.Method, r.URL.Path)
	}
	if base == nil {
		log.Warn("model proxy: a request its model doesn't need", "method", r.Method, "path", r.URL.Path)
		fail(w, http.StatusForbidden, "the model proxy doesn't forward "+r.Method+" "+r.URL.Path)
		return
	}
	transport := p.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			u := *base
			u.Path = strings.TrimSuffix(base.Path, "/") + pr.In.URL.Path
			u.RawPath = strings.TrimSuffix(base.EscapedPath(), "/") + pr.In.URL.EscapedPath()
			u.RawQuery = pr.In.URL.RawQuery
			pr.Out.URL, pr.Out.Host = &u, u.Host
			// The placeholder goes no further.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Api-Key")
			pr.Out.Body, pr.Out.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		},
		Transport: authTransport{auth: p.Upstream.Auth, body: body, base: transport},
		// Responses stream (server-sent events, Bedrock's event stream).
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Warn("model proxy: upstream", "path", r.URL.Path, "err", err)
			var ae authError
			if errors.As(err, &ae) {
				fail(w, http.StatusUnauthorized, err.Error())
				return
			}
			fail(w, http.StatusBadGateway, err.Error())
		},
	}
	rp.ServeHTTP(w, r)
}

// authError is a failure to get the credential, as opposed to reaching the
// upstream: the CLI should report it as an auth error.
type authError struct{ error }

type authTransport struct {
	auth func(context.Context, *http.Request, []byte) error
	body []byte
	base http.RoundTripper
}

func (t authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := t.auth(r.Context(), r, t.body); err != nil {
		return nil, authError{err}
	}
	return t.base.RoundTrip(r)
}

// fail answers in a shape both Anthropic's and OpenAI's clients read: the
// message ends up in the CLI's error, where devbot classifies it.
func fail(w http.ResponseWriter, status int, msg string) {
	kind := "api_error"
	if status == http.StatusUnauthorized {
		kind = "authentication_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{
		"type": kind, "message": "devbot model proxy: " + msg}})
}

// Serve runs p on addr until ctx ends, after writing where it is and its
// token to state, for the agent (agent.ReadModelProxy).
func Serve(ctx context.Context, addr, state string, p *Proxy) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mp := agent.ModelProxy{URL: "http://" + ln.Addr().String(), Token: p.Token, Credential: p.Upstream.Credential}
	if err := writeState(state, mp); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: p, ReadHeaderTimeout: time.Minute}
	go func() { <-ctx.Done(); srv.Close() }()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// writeState replaces state whole, so the agent never reads half of it.
func writeState(path string, mp agent.ModelProxy) error {
	b, _ := json.Marshal(mp)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".proxy-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

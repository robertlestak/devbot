// Package agent is the half of a job pod the model runs in.
//
// A job pod has three containers. The harness holds the forge token and does
// every forge and git-push operation. The model proxy (package modelproxy)
// holds the model's credential and adds it to the agent harness's API
// requests. The agent container holds neither: it runs the agent harness
// (Claude Code, say) and shell commands on the harness's request over a Unix
// socket, and the CLI reaches its model through the proxy with a placeholder
// token (ModelProxy).
//
// The split is what keeps the model from holding any credential while it
// runs as root to install whatever a repo needs: root in the same container
// could read the others' environment from /proc, or plant a git hook or
// fsmonitor that the harness would then execute. Across a container boundary
// it can do neither - it can only leave files in the shared workspace, which
// the harness treats as data, and call the proxy, which only forwards to the
// model's API.
//
// "Harness" means two things here: devbot's harness container (package
// harness), and an agent harness, the CLI that drives the model (the Harness
// interface, implemented in subpackages such as agent/claude).
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Harness is one agent CLI behind devbot's neutral contract: a prompt in,
// the work left in Dir, a RunResult out. Everything specific to one CLI -
// flags, output parsing, rate-limit messages, the environment it needs -
// stays in its implementation.
type Harness interface {
	Name() string
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

// RunRequest asks for one model run.
type RunRequest struct {
	// Harness names which of the agent's harnesses runs it.
	Harness string `json:"harness"`
	Dir     string `json:"dir"`
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	// Resume is the SessionID of an earlier run to continue. Only the
	// harness that returned it knows what it means.
	Resume string `json:"resume,omitempty"`
	// IgnoreRepoConfig runs without the agent config Dir carries: its
	// instruction files, settings, hooks, MCP servers and skills. For a run
	// that judges the code it reads, which must not be able to steer it.
	IgnoreRepoConfig bool `json:"ignore_repo_config,omitempty"`
	// BedrockRegion, when set, runs the model on Amazon Bedrock in that
	// region, through a model proxy that signs for it.
	BedrockRegion string        `json:"bedrock_region,omitempty"`
	Timeout       time.Duration `json:"timeout"`
	OTELAttrs     string        `json:"otel_attrs,omitempty"` // appended to OTEL_RESOURCE_ATTRIBUTES
	// TranscriptBytes, when set, asks for the run's transcript (the CLI's
	// event stream) in RunResult.Transcript, up to this many bytes.
	TranscriptBytes int `json:"transcript_bytes,omitempty"`
	// MCPServers are the only MCP servers the run gets: none of the repo's
	// or the CLI's own configuration loads.
	MCPServers []MCPServer `json:"mcp_servers,omitempty"`
	// Env is exported to the CLI and so to every command the model runs:
	// the repo's .devbot.yaml env. The CLI's own settings win over it.
	Env map[string]string `json:"env,omitempty"`
	// CoAuthoredBy, when set, turns the CLI's own attribution of the
	// commits and PRs it writes on or off: the repo's commits.co_authored_by.
	// Unset leaves the CLI's default.
	CoAuthoredBy *bool `json:"co_authored_by,omitempty"`
}

// MCPServer is one remote MCP server for a run. It carries no credential:
// whatever is here, the model can read.
type MCPServer struct {
	Name string `json:"name"`
	Type string `json:"type"` // http (streamable) or sse
	URL  string `json:"url"`
	// AllowedTools, when set, are the only tools the run may call;
	// DisabledTools are tools it may not.
	AllowedTools  []string `json:"allowed_tools,omitempty"`
	DisabledTools []string `json:"disabled_tools,omitempty"`
}

// Usage is a run's token counts. Zero means the harness didn't report it.
type Usage struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	CacheWriteTokens  int `json:"cache_write_tokens"`
	ReasoningTokens   int `json:"reasoning_tokens"`
}

// Add accumulates u into t.
func (t *Usage) Add(u Usage) {
	t.InputTokens += u.InputTokens
	t.OutputTokens += u.OutputTokens
	t.CachedInputTokens += u.CachedInputTokens
	t.CacheWriteTokens += u.CacheWriteTokens
	t.ReasoningTokens += u.ReasoningTokens
}

// ErrorClass says why a run failed, in terms devbot can act on whichever
// harness ran it. Empty is success.
type ErrorClass string

const (
	ErrRateLimited ErrorClass = "rate_limited"  // a usage or rate limit the CLI gave up on; see ResetAt
	ErrAuth        ErrorClass = "auth"          // the credential is wrong or expired
	ErrTimeout     ErrorClass = "timeout"       // the run hit RunRequest.Timeout
	ErrHarness     ErrorClass = "harness_error" // the CLI failed without a usable result
	ErrModel       ErrorClass = "model_error"   // the CLI ran and reported that the run failed
)

// RunResult is one model run's outcome.
type RunResult struct {
	SessionID string        `json:"session_id"` // opaque; pass back as RunRequest.Resume
	Result    string        `json:"result"`     // the final message
	IsError   bool          `json:"is_error"`
	ExitCode  int           `json:"exit_code"`
	TimedOut  bool          `json:"timed_out"`
	Duration  time.Duration `json:"duration"`
	Stderr    string        `json:"stderr,omitempty"` // the tail
	Usage     Usage         `json:"usage"`
	// CostUSD is nil when the harness doesn't report cost.
	CostUSD     *float64   `json:"cost_usd,omitempty"`
	Turns       int        `json:"turns"` // 0 if unknown
	RateLimited bool       `json:"rate_limited"`
	ResetAt     time.Time  `json:"reset_at"` // zero if unknown
	ErrorClass  ErrorClass `json:"error_class,omitempty"`
	// Transcript is the CLI's event stream, a line each, when the request
	// asked for it. TranscriptTruncated says lines were dropped at the cap.
	Transcript          []string `json:"transcript,omitempty"`
	TranscriptTruncated bool     `json:"transcript_truncated,omitempty"`
	// MCPCalls counts the run's MCP tool calls by server, from the CLI's
	// stream.
	MCPCalls map[string]int `json:"mcp_calls,omitempty"`
	// MCPUnavailable are the servers the CLI reported it couldn't use,
	// each with why. The run went on without them.
	MCPUnavailable []string `json:"mcp_unavailable,omitempty"`
}

type ExecRequest struct {
	Dir     string        `json:"dir"`
	Script  string        `json:"script"`
	Timeout time.Duration `json:"timeout"`
	// Env is exported to the script, over the agent's own environment.
	Env map[string]string `json:"env,omitempty"`
}

type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"` // combined, tail-truncated
	TimedOut bool   `json:"timed_out"`
}

// Runner is what the harness needs from the agent side. The socket client
// implements it in pods; tests use Local in-process.
type Runner interface {
	Run(ctx context.Context, req RunRequest) (RunResult, error)
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)
}

// Local runs everything in this process. Used by tests and by `devbot agent`.
type Local struct {
	Harnesses map[string]Harness
	Env       []string // environment for scripts; nil = ScrubbedEnv()
	MaxOutput int
}

// ModelProxy is where an agent harness reaches its model: the job pod's
// model proxy, which holds the credential, listens on the pod's loopback,
// and accepts Token in its place. The proxy writes it to a file the agent
// reads (ReadModelProxy).
type ModelProxy struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	// Credential is the kind of credential the proxy adds, one of the Cred
	// constants, so the CLI can send the placeholder where it would send the
	// real one.
	Credential string `json:"credential"`
}

// The kinds of model credential a proxy holds.
const (
	CredAPIKey     = "api_key"     // a vendor API key
	CredOAuthToken = "oauth_token" // Claude Code's subscription token
	CredAWS        = "aws"         // an AWS credential, for Bedrock's SigV4
)

// ReadModelProxy reads the proxy's file, waiting up to max for it, for pods
// where the proxy may still be starting.
func ReadModelProxy(ctx context.Context, path string, max time.Duration) (*ModelProxy, error) {
	deadline := time.Now().Add(max)
	for {
		b, err := os.ReadFile(path)
		if err == nil {
			var p ModelProxy
			if err := json.Unmarshal(b, &p); err != nil {
				return nil, fmt.Errorf("model proxy file %s: %w", path, err)
			}
			if p.URL == "" || p.Token == "" {
				return nil, fmt.Errorf("model proxy file %s names no proxy", path)
			}
			return &p, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("no model proxy: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// modelSecrets are the variables a model credential travels in. The agent
// container has none, since the model proxy holds the credential; if one is
// set anyway, it isn't passed on. Nor is any AWS_ variable.
var modelSecrets = map[string]bool{"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true, "CLAUDE_CODE_OAUTH_TOKEN": true,
	"OPENAI_API_KEY": true, "CODEX_API_KEY": true}

// PassEnvVar names variables, comma-separated, that ScrubbedEnv keeps too.
// The chart lists agentEnv's keys in it, so that a registry mirror's
// settings, say, reach the model's commands and the checks.
const PassEnvVar = "DEVBOT_PASS_ENV"

// ScrubbedEnv keeps what toolchains and telemetry need, the variables
// PassEnvVar names and those with the given prefixes. It drops anything that
// looks like a forge or model credential, as a second line of defence behind
// the container split.
func ScrubbedEnv(keepPrefix ...string) []string {
	keepPrefix = append([]string{"OTEL_", "MISE_", "LC_"}, keepPrefix...)
	keepExact := map[string]bool{"PATH": true, "HOME": true, "LANG": true, "TZ": true, "TERM": true, "USER": true,
		"IS_SANDBOX": true, "HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "http_proxy": true,
		"https_proxy": true, "no_proxy": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "DEBIAN_FRONTEND": true, "TMPDIR": true}
	for _, k := range strings.Split(os.Getenv(PassEnvVar), ",") {
		if k = strings.TrimSpace(k); k != "" {
			keepExact[k] = true
		}
	}
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "DEVBOT_") || strings.Contains(k, "GITEA") || strings.Contains(k, "GITHUB") ||
			modelSecrets[k] || strings.HasPrefix(k, "AWS_") {
			continue
		}
		if keepExact[k] {
			out = append(out, kv)
			continue
		}
		for _, p := range keepPrefix {
			if strings.HasPrefix(k, p) {
				out = append(out, kv)
				break
			}
		}
	}
	return out
}

// WithEnv returns a copy of env with extra appended, in name order. A
// later entry wins, so whatever is appended after it overrides it.
func WithEnv(env []string, extra map[string]string) []string {
	env = append([]string(nil), env...)
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		env = append(env, k+"="+extra[k])
	}
	return env
}

// WithOTELAttrs returns a copy of env with extra appended to
// OTEL_RESOURCE_ATTRIBUTES.
func WithOTELAttrs(env []string, extra string) []string {
	env = append([]string(nil), env...)
	if extra == "" {
		return env
	}
	found := false
	for i, kv := range env {
		if strings.HasPrefix(kv, "OTEL_RESOURCE_ATTRIBUTES=") {
			if strings.TrimPrefix(kv, "OTEL_RESOURCE_ATTRIBUTES=") == "" {
				env[i] = "OTEL_RESOURCE_ATTRIBUTES=" + extra
			} else {
				env[i] = kv + "," + extra
			}
			found = true
		}
	}
	if !found {
		env = append(env, "OTEL_RESOURCE_ATTRIBUTES="+extra)
	}
	return env
}

func (l *Local) maxOut() int {
	if l.MaxOutput > 0 {
		return l.MaxOutput
	}
	return 64 << 10
}

func (l *Local) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	h, ok := l.Harnesses[req.Harness]
	if !ok {
		return RunResult{}, fmt.Errorf("this agent doesn't run harness %q", req.Harness)
	}
	return h.Run(ctx, req)
}

// RunGroup runs a harness's CLI in a process group of its own and kills the
// whole group as soon as the CLI exits or ctx ends, so nothing it started in
// the background (Codex's plugin sync, say) outlives the run and keeps
// writing to its home or the workspace. cmd's Stdout and Stderr, when set,
// must be different writers.
func RunGroup(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// Wait on exec's own pipes would wait for every process still holding
	// them, the stragglers included, before the group could be killed.
	var copies sync.WaitGroup
	var readers, writers []*os.File
	pipe := func(w io.Writer) io.Writer {
		if _, ok := w.(*os.File); ok || w == nil {
			return w
		}
		r, pw, err := os.Pipe()
		if err != nil {
			return w
		}
		readers, writers = append(readers, r), append(writers, pw)
		copies.Go(func() { _, _ = io.Copy(w, r) })
		return pw
	}
	cmd.Stdout, cmd.Stderr = pipe(cmd.Stdout), pipe(cmd.Stderr)
	err := cmd.Start()
	for _, w := range writers {
		w.Close()
	}
	if err == nil {
		err = cmd.Wait()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// A process that left the group (setsid) could hold a pipe forever.
	done := make(chan struct{})
	go func() { copies.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	for _, r := range readers {
		r.Close()
	}
	<-done
	return err
}

func (l *Local) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	if req.Timeout <= 0 {
		req.Timeout = 20 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", req.Script)
	cmd.Dir = req.Dir
	cmd.Env = l.Env
	if cmd.Env == nil {
		cmd.Env = ScrubbedEnv()
	}
	cmd.Env = WithEnv(cmd.Env, req.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	// Background processes a script starts (a database for the tests) keep
	// the pipes open; don't wait on them once bash itself has exited.
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	res := ExecResult{Output: Tail(out.String(), l.maxOut()), TimedOut: ctx.Err() != nil}
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
	} else if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	} else if err != nil {
		if res.TimedOut {
			res.ExitCode = 124
			return res, nil
		}
		return res, err
	}
	return res, nil
}

// Tail keeps the last n bytes of s, marking the cut.
func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...(truncated)...\n" + s[len(s)-n:]
}

// ---- socket server and client ----

// Serve runs the agent's API on a Unix socket until ctx ends.
func Serve(ctx context.Context, socket string, l *Local, log *slog.Logger) error {
	_ = os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	// The harness runs as another user, unprivileged, and connecting takes
	// write on the socket. Nothing else shares the pod's volume.
	if err := os.Chmod(socket, 0o666); err != nil {
		ln.Close()
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("POST /run", func(w http.ResponseWriter, r *http.Request) {
		var req RunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		log.Info("run", "harness", req.Harness, "dir", req.Dir, "model", req.Model, "resume", req.Resume != "")
		res, err := l.Run(r.Context(), req)
		reply(w, res, err)
	})
	mux.HandleFunc("POST /exec", func(w http.ResponseWriter, r *http.Request) {
		var req ExecRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		res, err := l.Exec(r.Context(), req)
		reply(w, res, err)
	})
	srv := &http.Server{Handler: mux}
	go func() { <-ctx.Done(); srv.Close() }()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// Client talks to an agent over its socket.
type Client struct {
	Socket string
	hc     *http.Client
}

func NewClient(socket string) *Client {
	return &Client{Socket: socket, hc: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
}

// Wait blocks until the agent answers, for pods where the sidecar may still
// be starting.
func (c *Client) Wait(ctx context.Context, max time.Duration) error {
	deadline := time.Now().Add(max)
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://agent/healthz", nil)
		resp, err := c.hc.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("agent at %s not answering: %v", c.Socket, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (c *Client) call(ctx context.Context, path string, in, out any) error {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, "POST", "http://agent"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("agent %s: %s", path, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

func (c *Client) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	var r RunResult
	err := c.call(ctx, "/run", req, &r)
	return r, err
}

func (c *Client) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	var r ExecResult
	err := c.call(ctx, "/exec", req, &r)
	return r, err
}

// Transcript splits a CLI's output into its non-empty lines, keeping them
// while they fit in max bytes. It reports whether it dropped any.
func Transcript(out []byte, max int) ([]string, bool) {
	var lines []string
	n := 0
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) == "" {
			continue
		}
		if n+len(l) > max {
			return lines, true
		}
		n += len(l)
		lines = append(lines, l)
	}
	return lines, false
}

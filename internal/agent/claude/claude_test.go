package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/agent/agenttest"
)

func TestDetectRateLimit(t *testing.T) {
	// Saturday 2026-10-17, 15:10 UTC: the session limits of issue #99.
	now := time.Date(2026, 10, 17, 15, 10, 0, 0, time.UTC)
	la, _ := time.LoadLocation("America/Los_Angeles")
	berlin, _ := time.LoadLocation("Europe/Berlin")
	cases := []struct {
		name, msg string
		now       time.Time
		reset     time.Time // zero: none parsed
	}{
		{"session limit", "You've hit your session limit · resets 6pm (UTC)", now, time.Date(2026, 10, 17, 18, 0, 0, 0, time.UTC)},
		{"session limit, reset past today", "You've hit your session limit · resets 6pm (UTC)", now.Add(4 * time.Hour), time.Date(2026, 10, 18, 18, 0, 0, 0, time.UTC)},
		{"weekly limit, date form", "You've hit your weekly limit · resets Oct 20, 9am (UTC)", now, time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)},
		{"date form past this year", "Weekly limit reached ∙ resets Jan 5, 9am (UTC)", now, time.Date(2027, 1, 5, 9, 0, 0, 0, time.UTC)},
		{"weekday form", "Opus weekly limit reached ∙ resets Mon 9am (UTC)", now, time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)},
		{"weekday at form", "You've hit your weekly limit · resets Monday at 9am (UTC)", now, time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)},
		{"model limit, IANA zone", "You've reached your Opus limit · resets 6:30pm (America/Los_Angeles)", now, time.Date(2026, 10, 17, 18, 30, 0, 0, la)},
		{"reset at, IANA zone past today", "Claude usage limit reached. Your limit will reset at 3pm (Europe/Berlin).", now, time.Date(2026, 10, 18, 15, 0, 0, 0, berlin)},
		{"5-hour limit, 24-hour clock", "5-hour limit reached · resets 18:00 (UTC)", now, time.Date(2026, 10, 17, 18, 0, 0, 0, time.UTC)},
		{"epoch", "Claude AI usage limit reached|1760000000", now, time.Unix(1760000000, 0)},
		{"relative", "API Error: 429 rate_limit_error, try again in 5 minutes", now, now.Add(5 * time.Minute)},
		{"no reset", "You've hit your session limit", now, time.Time{}},
		{"unknown zone", "You've hit your session limit · resets 6pm (Mars/Olympus)", now, time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, at := DetectRateLimit(c.msg, c.now)
			if !ok || !at.Equal(c.reset) {
				t.Fatalf("got %v %v, want true %v", ok, at, c.reset)
			}
		})
	}
	for _, msg := range []string{"TypeError: undefined is not a function", "API Error: 400 prompt is too long", "Reached max turns (40)",
		"Error: input exceeds the model limit of 200k tokens",
		"ValidationException: The maximum tokens you requested exceeds the model limit of 8192"} {
		if ok, _ := DetectRateLimit(msg, now); ok {
			t.Errorf("%q is not a rate limit", msg)
		}
	}
}

// The proxies runs go through. Nothing listens: the fake CLI doesn't call
// them.
var (
	apiProxy   = &agent.ModelProxy{URL: "http://127.0.0.1:8462", Token: "devbot-proxy-test", Credential: agent.CredAPIKey}
	oauthProxy = &agent.ModelProxy{URL: "http://127.0.0.1:8462", Token: "devbot-proxy-test", Credential: agent.CredOAuthToken}
	awsProxy   = &agent.ModelProxy{URL: "http://127.0.0.1:8462", Token: "devbot-proxy-test", Credential: agent.CredAWS}
)

func run(t *testing.T, script string, req agent.RunRequest) (agent.RunResult, *agenttest.CLI) {
	t.Helper()
	p := apiProxy
	if req.BedrockRegion != "" {
		p = awsProxy
	}
	return runVia(t, p, script, req)
}

func runVia(t *testing.T, p *agent.ModelProxy, script string, req agent.RunRequest) (agent.RunResult, *agenttest.CLI) {
	t.Helper()
	cli := agenttest.NewCLI(t, "claude", script)
	h := &Harness{Command: cli.Path, ExtraArgs: []string{"--debug"}, Proxy: p}
	if req.Dir == "" {
		req.Dir = t.TempDir()
	}
	res, err := h.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return res, cli
}

func TestRunFlagsPromptAndParsing(t *testing.T) {
	t.Setenv("DEVBOT_IMPLEMENTER_TOKEN", "secret")
	// Were the sidecar to have a credential, it stays there.
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-test")
	t.Setenv("ANTHROPIC_DEFAULT_HAIKU_MODEL", "claude-haiku-5-5")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "team=x")
	dir := t.TempDir()
	res, cli := run(t, `echo "some noise"
echo '{"type":"result","subtype":"success","is_error":false,"result":"all done","session_id":"s1","num_turns":3,"total_cost_usd":0.25,"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":20}}'
`, agent.RunRequest{Dir: dir, Model: "claude-opus-5-5", Prompt: "do the thing", Resume: "s0", OTELAttrs: "devbot.role=implement"})

	call := cli.Last(t)
	i := slices.Index(call.Args, "--mcp-config")
	if i < 0 || i+1 >= len(call.Args) {
		t.Fatalf("no --mcp-config in %q", call.Args)
	}
	want := []string{"-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions", "--model", "claude-opus-5-5", "--resume", "s0",
		"--mcp-config", call.Args[i+1], "--strict-mcp-config", "--debug"}
	if !slices.Equal(call.Args, want) {
		t.Fatalf("args %q, want %q", call.Args, want)
	}
	if call.Stdin != "do the thing" || call.Dir != dir {
		t.Fatalf("prompt %q in %q", call.Stdin, call.Dir)
	}
	if _, ok := call.Getenv("DEVBOT_IMPLEMENTER_TOKEN"); ok {
		t.Fatal("a forge credential reached claude's environment")
	}
	if v, _ := call.Getenv("ANTHROPIC_API_KEY"); v != apiProxy.Token {
		t.Fatalf("ANTHROPIC_API_KEY=%q, want the proxy's placeholder", v)
	}
	if v, _ := call.Getenv("ANTHROPIC_BASE_URL"); v != apiProxy.URL {
		t.Fatalf("ANTHROPIC_BASE_URL=%q, want the proxy", v)
	}
	if _, ok := call.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); ok {
		t.Fatal("an OAuth token with an API key proxy")
	}
	if v, _ := call.Getenv("ANTHROPIC_DEFAULT_HAIKU_MODEL"); v != "claude-haiku-5-5" {
		t.Fatal("Claude Code's settings must reach it")
	}
	if v, _ := call.Getenv("OTEL_RESOURCE_ATTRIBUTES"); v != "team=x,devbot.role=implement" {
		t.Fatalf("OTEL_RESOURCE_ATTRIBUTES=%q", v)
	}

	if res.IsError || res.ErrorClass != "" || res.SessionID != "s1" || res.Result != "all done" || res.Turns != 3 {
		t.Fatalf("got %+v", res)
	}
	if res.Usage != (agent.Usage{InputTokens: 10, OutputTokens: 5, CachedInputTokens: 100, CacheWriteTokens: 20}) {
		t.Fatalf("usage %+v", res.Usage)
	}
	if res.CostUSD == nil || *res.CostUSD != 0.25 {
		t.Fatalf("cost %v", res.CostUSD)
	}
}

func TestRunWithoutModelOrResume(t *testing.T) {
	_, cli := run(t, `echo '{"type":"result","result":"ok","session_id":"s1"}'`, agent.RunRequest{Prompt: "p"})
	if args := cli.Last(t).Args; slices.Contains(args, "--model") || slices.Contains(args, "--resume") {
		t.Fatalf("args %q", args)
	}
}

// The reviewer's run reads only user settings: a checkout's .claude/
// settings (hooks, env, apiKeyHelper), CLAUDE.md and commands are the
// implementer's, not instructions. Neither role loads a repo's .mcp.json.
func TestRunIgnoringRepoConfig(t *testing.T) {
	_, cli := run(t, `echo '{"type":"result","result":"ok","session_id":"s1"}'`, agent.RunRequest{Prompt: "p", IgnoreRepoConfig: true})
	want := []string{"-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions", "--setting-sources", "user", "--mcp-config"}
	if args := cli.Last(t).Args; !slices.Equal(args[:len(want)], want) || !slices.Contains(args, "--strict-mcp-config") {
		t.Fatalf("args %q, want %q then --strict-mcp-config", args, want)
	}
	_, cli = run(t, `echo '{"type":"result","result":"ok","session_id":"s1"}'`, agent.RunRequest{Prompt: "p"})
	if args := cli.Last(t).Args; slices.Contains(args, "--setting-sources") || !slices.Contains(args, "--strict-mcp-config") {
		t.Fatalf("the implementer follows the repo's own settings, and no MCP servers but devbot's: %q", args)
	}
}

func TestRunErrorClasses(t *testing.T) {
	cases := []struct {
		name, script string
		class        agent.ErrorClass
	}{
		{"rate limit", `echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Claude AI usage limit reached|4102444800","session_id":"s1"}'; exit 1`, agent.ErrRateLimited},
		{"rate limit on stderr", `echo "API Error: 429 too many requests" >&2; exit 1`, agent.ErrRateLimited},
		{"auth", `echo '{"type":"result","is_error":true,"result":"Invalid API key · Please run /login","session_id":"s1"}'; exit 1`, agent.ErrAuth},
		// Recorded from Claude Code 2.1.299 on Bedrock, with access keys AWS
		// doesn't know, and with no AWS credential.
		{"bedrock auth", `echo '{"type":"result","subtype":"success","is_error":true,"num_turns":1,"api_error_status":403,"api_error":"provider_credentials","result":"AWS authentication failed · refresh your AWS credentials (SSO sign-in, access keys, API key or proxy token) and retry · if credentials are current, check AWS permissions and model access · API Error: 403 The security token included in the request is invalid.","session_id":"d6263336-39f9-4979-9567-fc31d558e0aa","total_cost_usd":0}'`, agent.ErrAuth},
		{"bedrock no credentials", `echo '{"type":"result","subtype":"success","is_error":true,"num_turns":1,"result":"API Error: Could not load AWS credentials · Could not load credentials from any providers. Check or refresh your AWS credentials and try again.","session_id":"s1"}'`, agent.ErrAuth},
		{"model error", `echo '{"type":"result","subtype":"error_max_turns","is_error":true,"result":"","session_id":"s1"}'`, agent.ErrModel},
		{"no result", `echo "segfault" >&2; exit 139`, agent.ErrHarness},
		{"non-zero exit after a result", `echo '{"type":"result","result":"ok","session_id":"s1"}'; exit 2`, agent.ErrModel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, _ := run(t, c.script, agent.RunRequest{Prompt: "p"})
			if !res.IsError || res.ErrorClass != c.class {
				t.Fatalf("class %q, want %q: %+v", res.ErrorClass, c.class, res)
			}
			if res.RateLimited != (c.class == agent.ErrRateLimited) {
				t.Fatalf("RateLimited %v", res.RateLimited)
			}
		})
	}
	res, _ := run(t, cases[0].script, agent.RunRequest{Prompt: "p"})
	if res.ResetAt.Unix() != 4102444800 {
		t.Fatalf("reset %v", res.ResetAt)
	}
	if res.SessionID != "s1" || res.ExitCode != 1 {
		t.Fatalf("got %+v", res)
	}
}

// Only the CLI's own error frames are read for a limit: a model whose
// answer mentions one mustn't hold the fleet.
func TestRunRateLimitOnlyFromCLIErrors(t *testing.T) {
	cases := []struct {
		name, script string
		limited      bool
	}{
		{"session limit result", `echo '{"type":"result","subtype":"success","is_error":true,"result":"You'"'"'ve hit your session limit · resets 6pm (UTC)","session_id":"s1"}'; exit 1`, true},
		{"session limit on stderr", `echo "You've hit your session limit · resets 6pm (UTC)" >&2; exit 1`, true},
		{"successful answer", `echo '{"type":"result","subtype":"success","is_error":false,"result":"Fixed the usage limit check; Claude AI usage limit reached|4102444800 now parses.","session_id":"s1"}'`, false},
		{"answer, then a non-zero exit", `echo '{"type":"result","subtype":"success","is_error":false,"result":"The weekly limit reached state is handled.","session_id":"s1"}'; exit 2`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, _ := run(t, c.script, agent.RunRequest{Prompt: "p"})
			if res.RateLimited != c.limited || (res.ErrorClass == agent.ErrRateLimited) != c.limited {
				t.Fatalf("RateLimited %v, class %q, want limited=%v", res.RateLimited, res.ErrorClass, c.limited)
			}
			if c.limited && (res.ResetAt.IsZero() || res.ResetAt.UTC().Hour() != 18 || time.Until(res.ResetAt) > 24*time.Hour) {
				t.Fatalf("reset %v, want the next 18:00 UTC", res.ResetAt)
			}
		})
	}
}

func TestRunNoResultKeepsOutput(t *testing.T) {
	res, _ := run(t, `echo "not json at all"; exit 1`, agent.RunRequest{Prompt: "p"})
	if !strings.Contains(res.Result, "not json at all") || res.CostUSD != nil {
		t.Fatalf("got %+v", res)
	}
}

// A run that ends without a result keeps only the CLI's own noise: the
// stream before it is the model's messages and tool output, which mustn't
// be read for a limit or posted to the forge.
func TestRunNoResultDropsStream(t *testing.T) {
	const stream = `cat <<'JSON'
{"type":"assistant","session_id":"s1","message":{"content":[{"type":"text","text":"Claude AI usage limit reached|4102444800"}]}}
{"type":"user","session_id":"s1","message":{"content":[{"type":"tool_result","content":"You have hit your session limit · resets 6pm (UTC)"}]}}
JSON
echo "CLI noise"
printf '%s' '{"type":"user","message":{"content":[{"type":"tool_result","content":"weekly limit reached'
`
	cases := []struct {
		name, script string
		timeout      time.Duration
		class        agent.ErrorClass
	}{
		{"timeout", stream + "sleep 30", 500 * time.Millisecond, agent.ErrTimeout},
		{"non-zero exit", stream + "exit 1", 0, agent.ErrHarness},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, _ := run(t, c.script, agent.RunRequest{Prompt: "p", Timeout: c.timeout})
			if res.RateLimited || res.ErrorClass != c.class || !res.IsError {
				t.Fatalf("RateLimited %v, class %q, want %q", res.RateLimited, res.ErrorClass, c.class)
			}
			if strings.Contains(res.Result, "limit") || strings.TrimSpace(res.Result) != "CLI noise" {
				t.Fatalf("result %q, want only the CLI's noise", res.Result)
			}
		})
	}
}

func TestRunTimeout(t *testing.T) {
	start := time.Now()
	res, _ := run(t, `sleep 30`, agent.RunRequest{Prompt: "p", Timeout: 500 * time.Millisecond})
	if time.Since(start) > 10*time.Second {
		t.Fatal("the timeout didn't stop the CLI")
	}
	if !res.TimedOut || !res.IsError || res.ErrorClass != agent.ErrTimeout {
		t.Fatalf("got %+v", res)
	}
}

// On Bedrock, Claude Code gets CLAUDE_CODE_USE_BEDROCK, the region and the
// proxy, with the placeholder as its Bedrock API key; no AWS credential the
// sidecar might have reaches it; and its Anthropic-priced cost is dropped.
func TestRunOnBedrock(t *testing.T) {
	for name, creds := range map[string]map[string]string{
		"access keys":  {"AWS_ACCESS_KEY_ID": "AKIA", "AWS_SECRET_ACCESS_KEY": "s", "AWS_SESSION_TOKEN": "t"},
		"irsa":         {"AWS_ROLE_ARN": "arn:aws:iam::1:role/devbot", "AWS_WEB_IDENTITY_TOKEN_FILE": "/var/run/secrets/eks.amazonaws.com/serviceaccount/token"},
		"pod identity": {"AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://169.254.170.23/v1/credentials", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": "/var/run/secrets/pods.eks.amazonaws.com/serviceaccount/eks-pod-identity-token"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AWS_REGION", "eu-west-1")
			t.Setenv("DEVBOT_IMPLEMENTER_TOKEN", "secret")
			for k, v := range creds {
				t.Setenv(k, v)
			}
			res, cli := run(t, `echo '{"type":"result","subtype":"success","result":"done","session_id":"s1","total_cost_usd":1.5}'`,
				agent.RunRequest{Prompt: "p", Model: "us.anthropic.claude-opus-5-5-v1:0", BedrockRegion: "us-east-1"})
			call := cli.Last(t)
			for k, want := range map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "AWS_REGION": "us-east-1",
				"ANTHROPIC_BEDROCK_BASE_URL": awsProxy.URL, "AWS_BEARER_TOKEN_BEDROCK": awsProxy.Token} {
				if v := lastEnv(call.Env, k); v != want {
					t.Errorf("%s=%q, want %q", k, v, want)
				}
			}
			for k := range creds {
				if _, ok := call.Getenv(k); ok {
					t.Errorf("%s reached claude's environment", k)
				}
			}
			if _, ok := call.Getenv("DEVBOT_IMPLEMENTER_TOKEN"); ok {
				t.Fatal("a forge credential reached claude's environment")
			}
			if res.IsError || res.CostUSD != nil {
				t.Fatalf("want success with no cost, got %+v", res)
			}
		})
	}
	_, cli := run(t, `echo '{"type":"result","result":"ok","session_id":"s1"}'`, agent.RunRequest{Prompt: "p"})
	for _, kv := range cli.Last(t).Env {
		if strings.HasPrefix(kv, "AWS_") || strings.HasPrefix(kv, "CLAUDE_CODE_USE_BEDROCK=") {
			t.Errorf("off Bedrock, claude got %s", kv)
		}
	}
}

// A subscription token's placeholder goes where Claude Code reads one.
func TestRunWithOAuthProxy(t *testing.T) {
	_, cli := runVia(t, oauthProxy, `echo '{"type":"result","result":"ok","session_id":"s1"}'`, agent.RunRequest{Prompt: "p"})
	call := cli.Last(t)
	if v, _ := call.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); v != oauthProxy.Token {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN=%q", v)
	}
	if _, ok := call.Getenv("ANTHROPIC_API_KEY"); ok {
		t.Fatal("an API key with a subscription token proxy")
	}
}

// A run needs a proxy, and one for where it runs.
func TestRunNeedsItsProxy(t *testing.T) {
	cli := agenttest.NewCLI(t, "claude", `echo '{"type":"result","result":"ok","session_id":"s1"}'`)
	for _, c := range []struct {
		p      *agent.ModelProxy
		region string
	}{{nil, ""}, {awsProxy, ""}, {apiProxy, "us-east-1"}, {oauthProxy, "us-east-1"}} {
		h := &Harness{Command: cli.Path, Proxy: c.p}
		if _, err := h.Run(context.Background(), agent.RunRequest{Dir: t.TempDir(), Prompt: "p", BedrockRegion: c.region}); err == nil {
			t.Errorf("proxy %+v, region %q: want an error", c.p, c.region)
		}
	}
	if len(cli.Calls(t)) != 0 {
		t.Error("claude ran")
	}
}

// lastEnv is key's value as the process sees it: exec keeps the last of
// duplicates.
func lastEnv(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, _ := strings.Cut(kv, "="); k == key {
			v = val
		}
	}
	return v
}

func TestBedrockErrorClasses(t *testing.T) {
	cases := []struct {
		out   string
		class agent.ErrorClass
	}{
		{"API Error: 400 ThrottlingException: Too many requests, please wait before trying again.", agent.ErrRateLimited},
		{"API Error: Too many tokens, please wait before trying again.", agent.ErrRateLimited},
		{"API Error: 400 ServiceQuotaExceededException: You have exceeded your daily token quota", agent.ErrRateLimited},
		{"API Error: 403 The security token included in the request is invalid.", agent.ErrAuth},
		{"API Error: 403 UnrecognizedClientException", agent.ErrAuth},
		{"API Error: 400 ExpiredTokenException: The security token included in the request is expired", agent.ErrAuth},
		{"Could not load credentials from any providers", agent.ErrAuth},
		{"API Error: 403 You don't have access to the model with the specified model ID.", agent.ErrAuth},
		{"API Error: 403 AccessDeniedException: User: arn:aws:sts::1:assumed-role/devbot is not authorized to perform: bedrock:InvokeModelWithResponseStream", agent.ErrAuth},
	}
	for _, c := range cases {
		res, _ := run(t, "cat <<'EOF'\n{\"type\":\"result\",\"is_error\":true,\"result\":\""+c.out+"\",\"session_id\":\"s1\"}\nEOF\n",
			agent.RunRequest{Prompt: "p", BedrockRegion: "us-east-1"})
		if res.ErrorClass != c.class {
			t.Errorf("%q: class %q, want %q", c.out, res.ErrorClass, c.class)
		}
	}
}

// Asked for a transcript, claude streams its events; the run's outcome is
// still the result event, never an event that merely has a session ID.
func TestRunTranscript(t *testing.T) {
	res, cli := run(t, `echo '{"type":"system","subtype":"init","session_id":"s1"}'
echo '{"type":"assistant","session_id":"s1","message":{"content":[{"type":"text","text":"hi"}]}}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"all done","session_id":"s1","num_turns":2,"usage":{"input_tokens":7,"output_tokens":3}}'
`, agent.RunRequest{Prompt: "p", TranscriptBytes: 1 << 20})
	want := []string{"-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"}
	if call := cli.Last(t); !slices.Equal(call.Args[:len(want)], want) {
		t.Fatalf("args %q, want %q", call.Args, want)
	}
	if res.IsError || res.Result != "all done" || res.Usage.InputTokens != 7 || len(res.Transcript) != 3 || res.TranscriptTruncated {
		t.Fatalf("got %+v", res)
	}

	// Killed mid-stream: no result, so no success.
	res, _ = run(t, `echo '{"type":"system","subtype":"init","session_id":"s1"}'
echo '{"type":"assistant","session_id":"s1","message":{}}'
exit 1
`, agent.RunRequest{Prompt: "p", TranscriptBytes: 1 << 20})
	if !res.IsError || res.SessionID != "" || len(res.Transcript) != 2 {
		t.Fatalf("a stream without its result is a failure: %+v", res)
	}

	// Crashed without a result: the events mention a 429, a 401 and a
	// secret, but they're the model's tool output, not the CLI's error.
	res, _ = run(t, `echo '{"type":"system","subtype":"init","session_id":"s1"}'
echo '{"type":"user","session_id":"s1","message":{"content":[{"type":"tool_result","content":"HTTP 429 rate limit; API Error: 401; AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIsecret"}]}}'
echo 'panic: out of memory'
exit 1
`, agent.RunRequest{Prompt: "p", TranscriptBytes: 1 << 20})
	if !res.IsError || res.ErrorClass != agent.ErrHarness || res.RateLimited || len(res.Transcript) != 3 {
		t.Fatalf("got class %q, rate limited %v: %+v", res.ErrorClass, res.RateLimited, res)
	}
	if strings.TrimSpace(res.Result) != "panic: out of memory" {
		t.Fatalf("result carries the events: %q", res.Result)
	}
}

// mcpScript keeps a copy of the --mcp-config file the CLI was given, which
// the harness removes after the run, as mcp.json next to the working tree.
const mcpScript = `while [ $# -gt 0 ]; do [ "$1" = --mcp-config ] && cp "$2" ../mcp.json; shift; done
`

// The run gets exactly the servers asked for, from a file outside the
// workspace, and --strict-mcp-config so the repo's .mcp.json never loads;
// tool restrictions become permission flags.
func TestRunMCPConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":{"evil":{"type":"http","url":"https://evil.example"}}}`), 0o644)
	_, cli := run(t, mcpScript+`echo '{"type":"result","result":"ok","session_id":"s1"}'`, agent.RunRequest{Dir: dir, Prompt: "p", MCPServers: []agent.MCPServer{
		{Name: "context7", Type: "http", URL: "https://mcp.example.com/context7/mcp", DisabledTools: []string{"resolve-library-id"}},
		{Name: "playwright", Type: "sse", URL: "http://10.0.0.5:8931/sse", AllowedTools: []string{"browser_navigate", "browser_snapshot"}},
	}})
	args := cli.Last(t).Args
	i := slices.Index(args, "--mcp-config")
	if i < 0 || strings.HasPrefix(args[i+1], dir) || !slices.Contains(args, "--strict-mcp-config") {
		t.Fatalf("args %q", args)
	}
	if _, err := os.Stat(args[i+1]); !os.IsNotExist(err) {
		t.Fatalf("the MCP config outlived the run: %v", err)
	}
	for flag, want := range map[string]string{
		"--allowedTools":    "mcp__playwright__browser_navigate,mcp__playwright__browser_snapshot",
		"--disallowedTools": "mcp__context7__resolve-library-id",
	} {
		if j := slices.Index(args, flag); j < 0 || args[j+1] != want {
			t.Errorf("%s: args %q, want %q", flag, args, want)
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "..", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(b, &got)
	want := map[string]any{"mcpServers": map[string]any{
		"context7":   map[string]any{"type": "http", "url": "https://mcp.example.com/context7/mcp"},
		"playwright": map[string]any{"type": "sse", "url": "http://10.0.0.5:8931/sse"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mcp config %s", b)
	}

	// No servers: still strict, with an empty config, for either role.
	os.Remove(filepath.Join(dir, "..", "mcp.json"))
	_, cli = run(t, mcpScript+`echo '{"type":"result","result":"ok","session_id":"s1"}'`, agent.RunRequest{Dir: dir, Prompt: "p"})
	args = cli.Last(t).Args
	if !slices.Contains(args, "--strict-mcp-config") || slices.Contains(args, "--allowedTools") || slices.Contains(args, "--disallowedTools") {
		t.Fatalf("args %q", args)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "..", "mcp.json")); string(b) != `{"mcpServers":{}}` {
		t.Fatalf("mcp config %s", b)
	}
}

// A server Claude Code couldn't connect to doesn't fail the run: it's
// reported, and the run's MCP tool calls are counted by server.
func TestRunMCPStream(t *testing.T) {
	res, _ := run(t, `cat <<'JSON'
{"type":"system","subtype":"init","session_id":"s1","mcp_servers":[{"name":"context7","status":"connected"},{"name":"playwright","status":"failed"}]}
{"type":"assistant","session_id":"s1","message":{"content":[{"type":"text","text":"looking"},{"type":"tool_use","name":"mcp__context7__get-library-docs"}]}}
{"type":"user","session_id":"s1","message":{"content":[{"type":"tool_result","content":"docs"}]}}
{"type":"assistant","session_id":"s1","message":{"content":[{"type":"tool_use","name":"mcp__context7__resolve-library-id"},{"type":"tool_use","name":"Bash"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s1","num_turns":2}
JSON
`, agent.RunRequest{Prompt: "p"})
	if res.IsError || res.Result != "done" {
		t.Fatalf("got %+v", res)
	}
	if !reflect.DeepEqual(res.MCPCalls, map[string]int{"context7": 2}) || !slices.Equal(res.MCPUnavailable, []string{"playwright: failed"}) {
		t.Fatalf("calls %v, unavailable %q", res.MCPCalls, res.MCPUnavailable)
	}

	// A stream that stops before its result is no result, however many
	// messages carry a session.
	res, _ = run(t, `echo '{"type":"assistant","session_id":"s1","message":{"content":[]}}'; exit 1`, agent.RunRequest{Prompt: "p"})
	if !res.IsError || res.ErrorClass != agent.ErrHarness || res.SessionID != "" {
		t.Fatalf("got %+v", res)
	}
}

// The repo's env reaches the CLI, and so the model's commands, but can't
// take the place of where the CLI's requests go.
func TestRunWithRepoEnv(t *testing.T) {
	_, cli := run(t, `echo '{"type":"result","result":"ok","session_id":"s1"}'`,
		agent.RunRequest{Prompt: "p", Env: map[string]string{"DB_DSN": "postgres://localhost:54337", "ANTHROPIC_BASE_URL": "https://elsewhere"}})
	call := cli.Last(t)
	if v, _ := call.Getenv("DB_DSN"); v != "postgres://localhost:54337" {
		t.Fatalf("DB_DSN=%q", v)
	}
	if v := lastEnv(call.Env, "ANTHROPIC_BASE_URL"); v != apiProxy.URL {
		t.Fatalf("ANTHROPIC_BASE_URL=%q", v)
	}
}

// settingsScript keeps a copy of the --settings file the CLI was given, as
// settings.json next to the working tree.
const settingsScript = `while [ $# -gt 0 ]; do [ "$1" = --settings ] && cp "$2" ../settings.json; shift; done
echo '{"type":"result","result":"ok","session_id":"s1"}'
`

// commits.co_authored_by reaches Claude Code as a settings file of its own,
// outside the workspace; unset, there is none and Claude Code's default holds.
func TestRunCoAuthoredBySettings(t *testing.T) {
	on, off := true, false
	for _, c := range []struct {
		set  *bool
		want string
	}{
		{nil, ""},
		{&off, `{"attribution":{"commit":"","pr":"","sessionUrl":false},"includeCoAuthoredBy":false}`},
		{&on, `{"includeCoAuthoredBy":true}`},
	} {
		dir := filepath.Join(t.TempDir(), "repo")
		os.MkdirAll(dir, 0o755)
		_, cli := run(t, settingsScript, agent.RunRequest{Dir: dir, Prompt: "p", CoAuthoredBy: c.set})
		args := cli.Last(t).Args
		i := slices.Index(args, "--settings")
		b, _ := os.ReadFile(filepath.Join(dir, "..", "settings.json"))
		if c.set == nil {
			if i >= 0 || b != nil {
				t.Errorf("unset: args %q, settings %s", args, b)
			}
			continue
		}
		if i < 0 || strings.HasPrefix(args[i+1], dir) {
			t.Fatalf("%v: args %q", *c.set, args)
		}
		if string(b) != c.want {
			t.Errorf("%v: settings %s, want %s", *c.set, b, c.want)
		}
		if _, err := os.Stat(args[i+1]); !os.IsNotExist(err) {
			t.Errorf("the settings file outlived the run: %v", err)
		}
	}
}

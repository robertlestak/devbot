package codex

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/agent/agenttest"
)

// replay is a fake codex that prints a recorded stream, writes last (if
// any) to the -o file as the real one writes the last message, then runs
// tail.
func replay(t *testing.T, stream, last, tail string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", stream))
	if err != nil {
		t.Fatal(err)
	}
	return `out=
while [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done
cat >/dev/null
cat '` + p + `'
last='` + last + `'
if [ -n "$out" ] && [ -n "$last" ]; then printf '%s' "$last" > "$out"; fi
` + tail
}

// The proxies a fixture runs codex through. Nothing listens: the fake CLI
// doesn't call them.
var (
	apiProxy = &agent.ModelProxy{URL: "http://127.0.0.1:8462", Token: "devbot-proxy-test", Credential: agent.CredAPIKey}
	awsProxy = &agent.ModelProxy{URL: "http://127.0.0.1:8462", Token: "devbot-proxy-test", Credential: agent.CredAWS}
)

type fixture struct {
	h   *Harness
	cli *agenttest.CLI
	log *bytes.Buffer
}

func newFixture(t *testing.T, script string) *fixture {
	t.Helper()
	cli := agenttest.NewCLI(t, "codex", script)
	var log bytes.Buffer
	return &fixture{
		h:   &Harness{Command: cli.Path, Proxy: apiProxy, HomeDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(&log, nil))},
		cli: cli, log: &log,
	}
}

func (f *fixture) run(t *testing.T, req agent.RunRequest) agent.RunResult {
	t.Helper()
	if req.Dir == "" {
		req.Dir = t.TempDir()
	}
	f.h.Proxy = apiProxy
	if req.BedrockRegion != "" {
		f.h.Proxy = awsProxy
	}
	res, err := f.h.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRunFlagsEnvAndSuccess(t *testing.T) {
	t.Setenv("DEVBOT_IMPLEMENTER_TOKEN", "secret")
	// Were the sidecar to have an OpenAI key, it stays there.
	t.Setenv("OPENAI_API_KEY", "sk-not-ours")
	t.Setenv(APIKeyEnv, "sk-codex")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "team=x")
	f := newFixture(t, replay(t, "success.jsonl", "Added hello.txt and wrote .devbot/result.json.", ""))
	dir := t.TempDir()
	res := f.run(t, agent.RunRequest{Dir: dir, Model: "gpt-6.1-sol", Prompt: "do the thing", OTELAttrs: "devbot.role=implement"})

	call := f.cli.Last(t)
	home, _ := call.Getenv("CODEX_HOME")
	want := []string{"exec", "--json", "--sandbox", "danger-full-access", "--skip-git-repo-check", "--ignore-user-config",
		"-o", filepath.Join(home, "last-message.txt"), "-c", "features.plugins=false", "-c", "skills.bundled.enabled=false",
		"-m", "gpt-6.1-sol", "-c", `openai_base_url="http://127.0.0.1:8462/v1"`, "-"}
	if !slices.Equal(call.Args, want) {
		t.Fatalf("args %q, want %q", call.Args, want)
	}
	if call.Stdin != "do the thing" || call.Dir != dir {
		t.Fatalf("prompt %q in %q", call.Stdin, call.Dir)
	}
	if filepath.Dir(home) != f.h.HomeDir {
		t.Fatalf("CODEX_HOME %q isn't a per-run directory under %q", home, f.h.HomeDir)
	}
	if v, _ := call.Getenv(APIKeyEnv); v != apiProxy.Token {
		t.Fatalf("CODEX_API_KEY=%q, want the placeholder", v)
	}
	for _, k := range []string{"DEVBOT_IMPLEMENTER_TOKEN", "OPENAI_API_KEY"} {
		if _, ok := call.Getenv(k); ok {
			t.Errorf("%s reached codex's environment", k)
		}
	}
	if v, _ := call.Getenv("OTEL_RESOURCE_ATTRIBUTES"); v != "team=x,devbot.role=implement" {
		t.Fatalf("OTEL_RESOURCE_ATTRIBUTES=%q", v)
	}

	if res.IsError || res.ErrorClass != "" || res.SessionID != "0199a213-81c0-7800-8aa1-bbab2a035a53" || res.Turns != 1 {
		t.Fatalf("got %+v", res)
	}
	if res.Result != "Added hello.txt and wrote .devbot/result.json." {
		t.Fatalf("the last agent message should win: %q", res.Result)
	}
	if res.Usage != (agent.Usage{InputTokens: 24763, CachedInputTokens: 24448, OutputTokens: 122, ReasoningTokens: 64}) {
		t.Fatalf("usage %+v", res.Usage)
	}
	if res.CostUSD != nil {
		t.Fatal("codex doesn't report cost")
	}
	// The run's home is kept under the session's name, with no login in it.
	if _, err := os.Stat(filepath.Join(f.h.HomeDir, res.SessionID, "last-message.txt")); err != nil {
		t.Fatalf("the session's home: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.h.HomeDir, res.SessionID, "auth.json")); err == nil {
		t.Fatal("a login in CODEX_HOME")
	}
	if strings.Contains(f.log.String(), "differ") {
		t.Fatalf("the stream and the -o file agree: %s", f.log)
	}
}

func TestRunSumsUsageOverTurns(t *testing.T) {
	f := newFixture(t, replay(t, "multi-turn.jsonl", "", ""))
	res := f.run(t, agent.RunRequest{Prompt: "p"})
	if res.IsError || res.Turns != 2 || res.Result != "Second turn, done." {
		t.Fatalf("a reconnect a turn recovered from isn't an error: %+v", res)
	}
	if res.Usage != (agent.Usage{InputTokens: 4000, CachedInputTokens: 1100, CacheWriteTokens: 40, OutputTokens: 120, ReasoningTokens: 40}) {
		t.Fatalf("usage %+v", res.Usage)
	}
}

// The reviewer's run leaves out the checkout's AGENTS.md, skills, .codex/
// config, hooks and exec policies: they are the implementer's.
func TestRunIgnoringRepoConfig(t *testing.T) {
	f := newFixture(t, replay(t, "success.jsonl", "", ""))
	dir := t.TempDir()
	f.run(t, agent.RunRequest{Dir: dir, Prompt: "p", IgnoreRepoConfig: true})
	call := f.cli.Last(t)
	home, _ := call.Getenv("CODEX_HOME")
	want := []string{"exec", "--json", "--sandbox", "danger-full-access", "--skip-git-repo-check", "--ignore-user-config",
		"-o", filepath.Join(home, "last-message.txt"), "-c", "features.plugins=false", "-c", "skills.bundled.enabled=false",
		"--ignore-rules", "-c", `projects."` + dir + `".trust_level="untrusted"`, "-c", "project_doc_max_bytes=0", "-c", "skills.include_instructions=false",
		"-c", `openai_base_url="http://127.0.0.1:8462/v1"`, "-"}
	if !slices.Equal(call.Args, want) {
		t.Fatalf("args %q, want %q", call.Args, want)
	}
}

func TestResumeRepassesFlagsInTheSessionsHome(t *testing.T) {
	f := newFixture(t, replay(t, "success.jsonl", "", ""))
	first := f.run(t, agent.RunRequest{Model: "gpt-6.1-sol", Prompt: "p"})
	firstHome, _ := f.cli.Last(t).Getenv("CODEX_HOME")
	f.cli = agenttest.NewCLI(t, "codex", replay(t, "resume.jsonl", "", ""))
	f.h.Command = f.cli.Path
	res := f.run(t, agent.RunRequest{Model: "gpt-6.1-sol", Prompt: "fix it", Resume: first.SessionID})

	call := f.cli.Last(t)
	home, _ := call.Getenv("CODEX_HOME")
	// The same path, not just the same files: Codex's state database
	// records the rollout's absolute path.
	if home != firstHome {
		t.Fatalf("a resume must run in its session's CODEX_HOME %q, got %q", firstHome, home)
	}
	want := []string{"exec", "resume", "--json", "-c", `sandbox_mode="danger-full-access"`, "--skip-git-repo-check", "--ignore-user-config",
		"-o", filepath.Join(home, "last-message.txt"), "-c", "features.plugins=false", "-c", "skills.bundled.enabled=false",
		"-m", "gpt-6.1-sol", "-c", `openai_base_url="http://127.0.0.1:8462/v1"`, first.SessionID, "-"}
	if !slices.Equal(call.Args, want) {
		t.Fatalf("args %q, want %q", call.Args, want)
	}
	if call.Stdin != "fix it" || res.IsError || res.SessionID != first.SessionID || res.Result != "Fixed the failing check." {
		t.Fatalf("got %+v", res)
	}

	if _, err := f.h.Run(context.Background(), agent.RunRequest{Dir: t.TempDir(), Resume: "0199-unknown"}); err == nil {
		t.Fatal("resuming a session this agent never ran must fail")
	}
	if _, err := f.h.Run(context.Background(), agent.RunRequest{Dir: t.TempDir(), Resume: "../escape"}); err == nil {
		t.Fatal("a session id is never a path")
	}
}

func TestRunErrorClasses(t *testing.T) {
	cases := []struct {
		stream string
		class  agent.ErrorClass
		in     string
	}{
		{"usage-limit.jsonl", agent.ErrRateLimited, "usage limit"},
		{"rate-limit.jsonl", agent.ErrRateLimited, "Rate limit reached"},
		{"auth-invalid.jsonl", agent.ErrAuth, "Incorrect API key"},
		{"auth-missing.jsonl", agent.ErrAuth, "Missing bearer"},
		// Recorded from codex-cli 0.163.0 on amazon-bedrock: no AWS credential,
		// and access keys AWS doesn't know.
		{"bedrock-no-credentials.jsonl", agent.ErrAuth, "failed to load AWS credentials"},
		{"bedrock-invalid-keys.jsonl", agent.ErrAuth, "security token included in the request is invalid"},
		{"model-error.jsonl", agent.ErrModel, "context window"},
		{"unclassified.jsonl", agent.ErrModel, "agent loop died"},
	}
	for _, c := range cases {
		t.Run(c.stream, func(t *testing.T) {
			f := newFixture(t, replay(t, c.stream, "", "exit 1"))
			res := f.run(t, agent.RunRequest{Prompt: "p"})
			if !res.IsError || res.ErrorClass != c.class || !strings.Contains(res.Result, c.in) {
				t.Fatalf("class %q, want %q: %+v", res.ErrorClass, c.class, res)
			}
			if res.RateLimited != (c.class == agent.ErrRateLimited) || res.ExitCode != 1 || res.SessionID == "" {
				t.Fatalf("got %+v", res)
			}
			if logged := strings.Contains(f.log.String(), "unclassified"); logged != (c.stream == "unclassified.jsonl") {
				t.Fatalf("only an unclassified error is logged in full: %s", f.log)
			}
		})
	}
	f := newFixture(t, replay(t, "usage-limit.jsonl", "", "exit 1"))
	if res := f.run(t, agent.RunRequest{Prompt: "p"}); !res.ResetAt.Equal(time.Date(2099, 3, 4, 15, 45, 0, 0, time.Local)) {
		t.Fatalf("reset %v", res.ResetAt)
	}
	f = newFixture(t, replay(t, "rate-limit.jsonl", "", "exit 1"))
	if res := f.run(t, agent.RunRequest{Prompt: "p"}); time.Until(res.ResetAt) < 5*time.Minute || time.Until(res.ResetAt) > 7*time.Minute {
		t.Fatalf("reset %v", res.ResetAt)
	}
}

func TestClassify(t *testing.T) {
	// A reset without a zone is in local time: pin it west of UTC, so a
	// zoned reset read in local time fails here, and fix now in UTC so
	// the host's zone can't move the day.
	la, _ := time.LoadLocation("America/Los_Angeles")
	defer func(l *time.Location) { time.Local = l }(time.Local)
	time.Local = la
	now := time.Date(2026, 10, 17, 16, 0, 0, 0, time.UTC)
	cases := []struct {
		msg   string
		class agent.ErrorClass
		reset time.Time
	}{
		{"You’ve hit your usage limit. Try again at 5:30 PM.", agent.ErrRateLimited, time.Date(2026, 10, 17, 17, 30, 0, 0, time.Local)},
		{"You’ve hit your usage limit. Try again at 3:15 AM.", agent.ErrRateLimited, time.Date(2026, 10, 18, 3, 15, 0, 0, time.Local)},
		{"You’ve hit your usage limit. Try again later.", agent.ErrRateLimited, time.Time{}},
		{"You've hit your session limit · resets 6pm (UTC)", agent.ErrRateLimited, time.Date(2026, 10, 17, 18, 0, 0, 0, time.UTC)},
		{"You've hit your weekly limit. Try again at Oct 20th, 2026 9:00 AM.", agent.ErrRateLimited, time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)},
		{"You've reached your gpt-6.1-sol limit · resets Oct 20, 9am (UTC)", agent.ErrRateLimited, time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)},
		{"5h limit reached. Try again at 6:30 PM (America/Los_Angeles).", agent.ErrRateLimited, time.Date(2026, 10, 17, 18, 30, 0, 0, la)},
		{"exceeded retry limit, last status: 429 Too Many Requests, request id: r1", agent.ErrRateLimited, time.Time{}},
		{"Quota exceeded. Check your plan and billing details.", agent.ErrRateLimited, time.Time{}},
		{"Selected model is at capacity. Please try a different model.", agent.ErrRateLimited, time.Time{}},
		{"To use Codex with your ChatGPT plan, upgrade to Plus: https://chatgpt.com/explore/plus.", agent.ErrAuth, time.Time{}},
		{"Your access token could not be refreshed because your refresh token was already used. Please log out and sign in again.", agent.ErrAuth, time.Time{}},
		{`unexpected status 400 Bad Request: {"error":{"message":"The model 'gpt-nope' does not exist","code":"model_not_found"}}`, agent.ErrModel, time.Time{}},
		{"unexpected status 400 Bad Request: input exceeds the model limit of 272000 tokens", "", time.Time{}},
		{"sandbox error: command was killed by a signal", "", time.Time{}},
	}
	for _, c := range cases {
		class, reset := Classify(c.msg, now)
		if class != c.class || !reset.Equal(c.reset) {
			t.Errorf("%q: %q %v, want %q %v", c.msg, class, reset, c.class, c.reset)
		}
	}
}

func TestRunNoFinalMessage(t *testing.T) {
	f := newFixture(t, replay(t, "no-final-message.jsonl", "", ""))
	res := f.run(t, agent.RunRequest{Prompt: "p"})
	if res.IsError || res.Result != "" || res.SessionID == "" || res.Usage.InputTokens != 500 {
		t.Fatalf("a turn that completed without a message isn't a failure; the result file is what counts: %+v", res)
	}
	// The -o file fills in for a stream that lost the message.
	f = newFixture(t, replay(t, "no-final-message.jsonl", "from the file", ""))
	if res := f.run(t, agent.RunRequest{Prompt: "p"}); res.Result != "from the file" {
		t.Fatalf("got %q", res.Result)
	}
}

func TestRunNoStream(t *testing.T) {
	f := newFixture(t, `echo "Error: thread/resume: thread/resume failed: no rollout found" >&2; exit 1`)
	res := f.run(t, agent.RunRequest{Prompt: "p"})
	if !res.IsError || res.ErrorClass != agent.ErrHarness || !strings.Contains(res.StderrTail, "no rollout") {
		t.Fatalf("got %+v", res)
	}
}

func TestRunTimeout(t *testing.T) {
	f := newFixture(t, `echo '{"type":"thread.started","thread_id":"t1"}'; sleep 30`)
	f.h.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	start := time.Now()
	res := f.run(t, agent.RunRequest{Prompt: "p", Timeout: 500 * time.Millisecond})
	if time.Since(start) > 10*time.Second {
		t.Fatal("the timeout didn't stop the CLI")
	}
	if !res.TimedOut || !res.IsError || res.ErrorClass != agent.ErrTimeout || res.SessionID != "t1" {
		t.Fatalf("got %+v", res)
	}
}

// On Bedrock, codex runs Codex's amazon-bedrock provider at the model proxy
// in the role's region, with the placeholder as its Bedrock API key; no AWS
// credential the sidecar might have reaches it, on Bedrock or off.
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
			f := newFixture(t, replay(t, "success.jsonl", "", ""))
			res := f.run(t, agent.RunRequest{Model: "openai.gpt-6.1-sol", Prompt: "p", BedrockRegion: "us-east-1"})
			call := f.cli.Last(t)
			if args := strings.Join(call.Args, " "); !strings.Contains(args,
				`-m openai.gpt-6.1-sol -c model_provider="amazon-bedrock" -c model_providers.amazon-bedrock.base_url="http://127.0.0.1:8462/openai/v1" -`) {
				t.Fatalf("args %s", args)
			}
			if v, _ := call.Getenv("AWS_REGION"); v != "us-east-1" {
				t.Fatalf("AWS_REGION=%q", v)
			}
			if v, _ := call.Getenv("AWS_BEARER_TOKEN_BEDROCK"); v != awsProxy.Token {
				t.Fatalf("AWS_BEARER_TOKEN_BEDROCK=%q, want the placeholder", v)
			}
			for k := range creds {
				if _, ok := call.Getenv(k); ok {
					t.Errorf("%s reached codex's environment", k)
				}
			}
			if _, ok := call.Getenv("DEVBOT_IMPLEMENTER_TOKEN"); ok {
				t.Fatal("a forge credential reached codex's environment")
			}
			if res.IsError || res.CostUSD != nil {
				t.Fatalf("got %+v", res)
			}
		})
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA")
	f := newFixture(t, replay(t, "success.jsonl", "", ""))
	f.run(t, agent.RunRequest{Prompt: "p"})
	call := f.cli.Last(t)
	for _, kv := range call.Env {
		if strings.HasPrefix(kv, "AWS_") {
			t.Errorf("off Bedrock, codex got %s", kv)
		}
	}
	if slices.Contains(call.Args, `model_provider="amazon-bedrock"`) {
		t.Fatalf("off Bedrock, codex runs on it: %q", call.Args)
	}
}

// A run needs a proxy, and one for where it runs.
func TestRunNeedsItsProxy(t *testing.T) {
	f := newFixture(t, replay(t, "success.jsonl", "", ""))
	f.h.Proxy = nil
	if _, err := f.h.Run(context.Background(), agent.RunRequest{Dir: t.TempDir(), Prompt: "p"}); err == nil {
		t.Error("a run without a model proxy")
	}
	f.h.Proxy = awsProxy
	if _, err := f.h.Run(context.Background(), agent.RunRequest{Dir: t.TempDir(), Prompt: "p"}); err == nil {
		t.Error("a run off Bedrock through a Bedrock proxy")
	}
	f.h.Proxy = apiProxy
	if _, err := f.h.Run(context.Background(), agent.RunRequest{Dir: t.TempDir(), Prompt: "p", BedrockRegion: "us-east-1"}); err == nil {
		t.Error("a run on Bedrock through an API key proxy")
	}
	if len(f.cli.Calls(t)) != 0 {
		t.Error("codex ran")
	}
}

func TestClassifyBedrock(t *testing.T) {
	cases := []struct {
		msg   string
		class agent.ErrorClass
	}{
		{`unexpected status 400 Bad Request: {"__type":"ThrottlingException","message":"Too many requests, please wait before trying again."}`, agent.ErrRateLimited},
		{`unexpected status 400 Bad Request: {"message":"Too many tokens, please wait before trying again."}`, agent.ErrRateLimited},
		{`unexpected status 400 Bad Request: ServiceQuotaExceededException: daily tokens`, agent.ErrRateLimited},
		{"Fatal error: failed to resolve Amazon Bedrock auth: no credentials in the credential chain", agent.ErrAuth},
		{`unexpected status 403 Forbidden: {"message":"The security token included in the request is invalid."}`, agent.ErrAuth},
		{`unexpected status 403 Forbidden: {"__type":"AccessDeniedException","message":"User: arn:aws:sts::1:assumed-role/devbot/x is not authorized to perform: bedrock-mantle:CreateInference"}`, agent.ErrAuth},
		{"Amazon Bedrock rejected the request because its AWS signature has expired.", agent.ErrAuth},
	}
	for _, c := range cases {
		if class, _ := Classify(c.msg, time.Now()); class != c.class {
			t.Errorf("%q: %q, want %q", c.msg, class, c.class)
		}
	}
}

// Only what Codex itself reports as the failure is read for a limit: an
// agent message or a command's output quoting one mustn't hold the fleet.
func TestRunRateLimitOnlyFromCLIErrors(t *testing.T) {
	f := newFixture(t, `cat >/dev/null
echo '{"type":"thread.started","thread_id":"t1"}'
echo '{"type":"item.completed","item":{"type":"command_execution","aggregated_output":"You have hit your usage limit. Try again at 3:45 PM."}}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"Handled the usage limit message: rate limit, 429."}}'
echo '{"type":"turn.completed","usage":{"input_tokens":1}}'
exit 1`)
	res := f.run(t, agent.RunRequest{Prompt: "p"})
	if !res.IsError || res.RateLimited || res.ErrorClass == agent.ErrRateLimited {
		t.Fatalf("an agent message isn't a usage limit: %+v", res)
	}
	f = newFixture(t, `cat >/dev/null; echo '{"type":"thread.started","thread_id":"t1"}'; echo "You've hit your session limit · resets 6pm (UTC)" >&2; exit 1`)
	res = f.run(t, agent.RunRequest{Prompt: "p"})
	if !res.RateLimited || res.ResetAt.UTC().Hour() != 18 || time.Until(res.ResetAt) > 24*time.Hour {
		t.Fatalf("a limit on stderr: %+v", res)
	}
}

// The run's servers are config overrides, none required, with the tool
// restrictions as Codex's own; a fresh CODEX_HOME and --ignore-user-config
// leave no other MCP config, a repo's .codex/config.toml included, in play.
func TestRunMCPServers(t *testing.T) {
	f := newFixture(t, replay(t, "mcp.jsonl", "Done.", `echo "ERROR rmcp: MCP client for playwright failed to start" >&2`))
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".codex"), 0o755)
	os.WriteFile(filepath.Join(dir, ".codex", "config.toml"), []byte("[mcp_servers.evil]\nurl = \"https://evil.example\"\n"), 0o644)
	res := f.run(t, agent.RunRequest{Dir: dir, Prompt: "p", MCPServers: []agent.MCPServer{
		{Name: "context7", Type: "http", URL: "https://mcp.example.com/context7/mcp", DisabledTools: []string{"resolve-library-id"}},
		{Name: "playwright", Type: "http", URL: "http://10.0.0.5:8931/mcp", AllowedTools: []string{"browser_navigate", "browser_snapshot"}},
	}})
	call := f.cli.Last(t)
	var overrides []string
	for i, a := range call.Args {
		if a == "-c" && strings.HasPrefix(call.Args[i+1], "mcp_servers.") {
			overrides = append(overrides, call.Args[i+1])
		}
	}
	want := []string{
		`mcp_servers.context7.url="https://mcp.example.com/context7/mcp"`, `mcp_servers.context7.required=false`,
		`mcp_servers.context7.disabled_tools=["resolve-library-id"]`,
		`mcp_servers.playwright.url="http://10.0.0.5:8931/mcp"`, `mcp_servers.playwright.required=false`,
		`mcp_servers.playwright.enabled_tools=["browser_navigate","browser_snapshot"]`,
	}
	if !slices.Equal(overrides, want) || !slices.Contains(call.Args, "--ignore-user-config") {
		t.Fatalf("args %q", call.Args)
	}
	home, _ := call.Getenv("CODEX_HOME")
	if ents, _ := os.ReadDir(home); slices.ContainsFunc(ents, func(e os.DirEntry) bool { return e.Name() == "config.toml" }) {
		t.Fatal("a fresh CODEX_HOME has no config.toml")
	}

	// The server that didn't start doesn't fail the run.
	if res.IsError || res.Result != "Done." {
		t.Fatalf("got %+v", res)
	}
	if res.MCPCalls["context7"] != 2 || len(res.MCPCalls) != 1 || len(res.MCPUnavailable) != 1 || !strings.Contains(res.MCPUnavailable[0], "playwright") {
		t.Fatalf("calls %v, unavailable %q", res.MCPCalls, res.MCPUnavailable)
	}

	// No servers, no overrides.
	f.run(t, agent.RunRequest{Prompt: "p"})
	if args := strings.Join(f.cli.Last(t).Args, " "); strings.Contains(args, "mcp_servers") {
		t.Fatalf("args %q", args)
	}
}

// commits.co_authored_by reaches Codex as the codex_git_commit feature, and
// only that: commit_attribution is unknown to the pinned CLI. Unset, none.
func TestRunCoAuthoredBy(t *testing.T) {
	f := newFixture(t, replay(t, "success.jsonl", "Done.", ""))
	on, off := true, false
	for _, c := range []struct {
		set  *bool
		want []string
	}{
		{nil, nil},
		{&off, []string{"features.codex_git_commit=false"}},
		{&on, []string{"features.codex_git_commit=true"}},
	} {
		f.run(t, agent.RunRequest{Prompt: "p", CoAuthoredBy: c.set})
		args := f.cli.Last(t).Args
		var got []string
		for i, a := range args {
			if strings.Contains(a, "commit_attribution") {
				t.Errorf("%v: arg %q", c.set, a)
			}
			if a == "-c" && strings.HasPrefix(args[i+1], "features.codex_git_commit") {
				got = append(got, args[i+1])
			}
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%v: overrides %q, want %q", c.set, got, c.want)
		}
	}
}

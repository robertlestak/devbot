//go:build bedrock_live

package agent_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/agent/claude"
	"git.shdw.tech/rob/devbot/internal/agent/codex"
	"git.shdw.tech/rob/devbot/internal/modelproxy"
)

// TestLiveBedrock runs one small implement per harness against real Amazon
// Bedrock, the way a job pod runs it: through the agent, with the CLI
// pointed at a model proxy that holds static access keys from the
// environment, and no other credential. A pass means the harness works
// through the proxy and the proxy's SigV4 is right for its endpoint.
// docs/operating.md, "Running the live Bedrock test", says what it needs.
func TestLiveBedrock(t *testing.T) {
	region := os.Getenv("DEVBOT_LIVE_BEDROCK_REGION")
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" || os.Getenv("AWS_SECRET_ACCESS_KEY") == "" || region == "" {
		t.Skip("set AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY (and AWS_SESSION_TOKEN for temporary keys) and DEVBOT_LIVE_BEDROCK_REGION")
	}
	// Only the keys: no Bedrock API key, profile, config file or other
	// harness credential to fall back on.
	for _, k := range []string{"AWS_BEARER_TOKEN_BEDROCK", "AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "CODEX_API_KEY", "OPENAI_API_KEY"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")

	codexHome := t.TempDir()
	cases := []struct {
		harness, modelEnv, cmdEnv string
		h                         func(cmd string, mp *agent.ModelProxy) agent.Harness
	}{
		{claude.Name, "DEVBOT_LIVE_BEDROCK_CLAUDE_MODEL", "DEVBOT_LIVE_CLAUDE_COMMAND",
			func(cmd string, mp *agent.ModelProxy) agent.Harness { return &claude.Harness{Command: cmd, Proxy: mp} }},
		{codex.Name, "DEVBOT_LIVE_BEDROCK_CODEX_MODEL", "DEVBOT_LIVE_CODEX_COMMAND",
			func(cmd string, mp *agent.ModelProxy) agent.Harness {
				return &codex.Harness{Command: cmd, HomeDir: codexHome, Proxy: mp}
			}},
	}
	for _, c := range cases {
		t.Run(c.harness, func(t *testing.T) {
			model := os.Getenv(c.modelEnv)
			if model == "" {
				t.Skipf("set %s", c.modelEnv)
			}
			cmd := os.Getenv(c.cmdEnv)
			if cmd == "" {
				cmd = c.harness
			}
			if _, err := exec.LookPath(cmd); err != nil {
				t.Skipf("no %s on the PATH (or set %s)", cmd, c.cmdEnv)
			}
			dir := t.TempDir()
			if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			up, err := modelproxy.For(c.harness, region, os.Getenv)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			state := filepath.Join(t.TempDir(), "proxy.json")
			go modelproxy.Serve(ctx, "127.0.0.1:0", state, &modelproxy.Proxy{Upstream: up, Token: modelproxy.NewToken()})
			mp, err := agent.ReadModelProxy(ctx, state, 10*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			l := &agent.Local{Harnesses: map[string]agent.Harness{c.harness: c.h(cmd, mp)}}
			res, err := l.Run(context.Background(), agent.RunRequest{
				Harness: c.harness, Dir: dir, Model: model, BedrockRegion: region, Timeout: 10 * time.Minute,
				Prompt: "Create a file named hello.txt in the current directory containing exactly the word hello and a newline. Do nothing else.",
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("RESULT %s on Bedrock (%s, %s): error_class=%q turns=%d usage=%+v duration=%s",
				c.harness, region, model, res.ErrorClass, res.Turns, res.Usage, res.Took().Round(time.Second))
			if c.harness == codex.Name {
				// Whatever the outcome, Codex cloned no plugin catalogue: no
				// .tmp/plugins-clone-* mid-sync, no .tmp/plugins after.
				if m, _ := filepath.Glob(filepath.Join(codexHome, "*", ".tmp", "plugins*")); len(m) > 0 {
					t.Errorf("codex synced plugins: %q", m)
				}
			}
			if res.IsError {
				t.Fatalf("the run failed (%s): %s\n%s", res.ErrorClass, res.Result, res.StderrTail)
			}
			b, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
			if err != nil || strings.TrimSpace(string(b)) != "hello" {
				t.Fatalf("hello.txt: %q %v", b, err)
			}
			if res.CostUSD != nil {
				t.Errorf("cost %v on Bedrock; want none", *res.CostUSD)
			}
			if res.Usage.InputTokens == 0 {
				t.Error("no usage reported")
			}
		})
	}
}

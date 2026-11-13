//go:build model_live

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

// TestLiveModelAPI runs one small implement per harness against its vendor's
// API (api.anthropic.com, api.openai.com), the way a job pod runs it: through
// the agent, with the CLI given only the model proxy's address and
// placeholder, and the proxy holding the credential from the environment. A
// pass means a real model answered a real CLI through the proxy.
// docs/operating.md, "Running the live model API test", says what it needs.
func TestLiveModelAPI(t *testing.T) {
	// No Bedrock or OpenAI-SDK credential for a CLI to fall back on; the
	// proxy reads the ones it holds before the runs start.
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_BEARER_TOKEN_BEDROCK", "AWS_PROFILE", "OPENAI_API_KEY"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	codexHome := t.TempDir()
	cases := []struct {
		harness, cmdEnv, modelEnv string
		creds                     []string
		h                         func(cmd string, mp *agent.ModelProxy) agent.Harness
	}{
		{claude.Name, "DEVBOT_LIVE_CLAUDE_COMMAND", "DEVBOT_LIVE_CLAUDE_MODEL", []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"},
			func(cmd string, mp *agent.ModelProxy) agent.Harness { return &claude.Harness{Command: cmd, Proxy: mp} }},
		{codex.Name, "DEVBOT_LIVE_CODEX_COMMAND", "DEVBOT_LIVE_CODEX_MODEL", []string{codex.APIKeyEnv},
			func(cmd string, mp *agent.ModelProxy) agent.Harness {
				return &codex.Harness{Command: cmd, HomeDir: codexHome, Proxy: mp}
			}},
	}
	for _, c := range cases {
		t.Run(c.harness, func(t *testing.T) {
			up, err := modelproxy.For(c.harness, "", os.Getenv)
			if err != nil {
				t.Skipf("set one of %s: %v", strings.Join(c.creds, ", "), err)
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
				Harness: c.harness, Dir: dir, Model: os.Getenv(c.modelEnv), Timeout: 10 * time.Minute,
				Prompt: "Create a file named hello.txt in the current directory containing exactly the word hello and a newline. Do nothing else.",
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("RESULT %s through the proxy (%s): error_class=%q turns=%d usage=%+v duration=%s",
				c.harness, up.Credential, res.ErrorClass, res.Turns, res.Usage, res.Took().Round(time.Second))
			if res.IsError {
				t.Fatalf("the run failed (%s): %s\n%s", res.ErrorClass, res.Result, res.StderrTail)
			}
			b, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
			if err != nil || strings.TrimSpace(string(b)) != "hello" {
				t.Fatalf("hello.txt: %q %v", b, err)
			}
			if res.Usage.InputTokens == 0 {
				t.Error("no usage reported")
			}
		})
	}
}

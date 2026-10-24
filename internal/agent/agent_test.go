package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// echoHarness answers with what it was asked, to see a request cross the socket.
type echoHarness struct{}

func (echoHarness) Name() string { return "echo" }
func (echoHarness) Run(_ context.Context, req RunRequest) (RunResult, error) {
	cost := 0.5
	return RunResult{SessionID: req.Resume + "+1", Result: req.Model + ":" + req.Prompt, CostUSD: &cost,
		Usage: Usage{CachedInputTokens: 7, ReasoningTokens: 3}, ErrorClass: ErrModel, Duration: time.Second}, nil
}

func TestSocketRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "a.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, sock, &Local{Harnesses: map[string]Harness{"echo": echoHarness{}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c := NewClient(sock)
	if err := c.Wait(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	// The harness connects as another, unprivileged user.
	if st, err := os.Stat(sock); err != nil || st.Mode().Perm() != 0o666 {
		t.Fatalf("socket mode: %v, %v", st.Mode(), err)
	}
	t.Setenv("DEVBOT_IMPLEMENTER_TOKEN", "nope")
	res, err := c.Exec(ctx, ExecRequest{Dir: dir, Script: "echo hi; env | grep -c DEVBOT_ || true; exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || !strings.HasPrefix(res.Output, "hi\n0") {
		t.Fatalf("got %+v", res)
	}

	run, err := c.Run(ctx, RunRequest{Harness: "echo", Model: "m", Prompt: "p", Resume: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if run.SessionID != "s+1" || run.Result != "m:p" || run.CostUSD == nil || *run.CostUSD != 0.5 ||
		run.Usage.CachedInputTokens != 7 || run.Usage.ReasoningTokens != 3 || run.ErrorClass != ErrModel || run.Duration != time.Second {
		t.Fatalf("the neutral result didn't survive the socket: %+v", run)
	}
	if _, err := c.Run(ctx, RunRequest{Harness: "nope"}); err == nil {
		t.Fatal("a harness the agent doesn't have must be an error")
	}
}

func TestScrubbedEnv(t *testing.T) {
	t.Setenv("DEVBOT_IMPLEMENTER_TOKEN", "x")
	t.Setenv("GITHUB_APP_KEY", "x")
	t.Setenv("SOME_GITEA_TOKEN", "x")
	t.Setenv("FOO_CREDENTIAL", "x")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "x")
	t.Setenv("ACME_KEY", "x")
	// A model credential is dropped even with its prefix kept or its name
	// passed.
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"} {
		t.Setenv(k, "x")
	}
	t.Setenv("ANTHROPIC_DEFAULT_HAIKU_MODEL", "x")
	t.Setenv("GOPROXY", "x")
	t.Setenv("npm_config_registry", "x")
	t.Setenv(PassEnvVar, "GOPROXY, npm_config_registry,CODEX_API_KEY,AWS_SECRET_ACCESS_KEY")
	env := strings.Join(ScrubbedEnv("ACME_", "ANTHROPIC_", "CLAUDE_", "CODEX_"), "\n")
	for _, k := range []string{"DEVBOT_", "GITHUB_", "GITEA_", "FOO_", "_API_KEY", "_AUTH_TOKEN", "_OAUTH_TOKEN", "AWS_"} {
		if strings.Contains(env, k) {
			t.Errorf("%s survived the scrub:\n%s", k, env)
		}
	}
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT=", "ACME_KEY=", "ANTHROPIC_DEFAULT_HAIKU_MODEL=", "GOPROXY=", "npm_config_registry="} {
		if !strings.Contains(env, k) {
			t.Errorf("%s was dropped:\n%s", k, env)
		}
	}
}

func TestWithOTELAttrs(t *testing.T) {
	got := WithOTELAttrs([]string{"A=1", "OTEL_RESOURCE_ATTRIBUTES=x=1"}, "devbot.role=implement")
	if got[1] != "OTEL_RESOURCE_ATTRIBUTES=x=1,devbot.role=implement" {
		t.Fatalf("got %v", got)
	}
	got = WithOTELAttrs([]string{"A=1"}, "y=2")
	if got[len(got)-1] != "OTEL_RESOURCE_ATTRIBUTES=y=2" {
		t.Fatalf("got %v", got)
	}
}

// What a harness's CLI leaves running in the background dies when the CLI
// exits, whether it let go of the CLI's output or not.
func TestRunGroupKillsWhatTheCLILeavesBehind(t *testing.T) {
	for name, bg := range map[string]string{
		"detached":     `(sleep 2; touch "$1/late") >/dev/null 2>&1 &`,
		"holding pipe": `(sleep 2; touch "$1/late") &`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.CommandContext(context.Background(), "bash", "-c", bg+` echo done`, "bash", dir)
			var out strings.Builder
			cmd.Stdout = &out
			start := time.Now()
			if err := RunGroup(cmd); err != nil || out.String() != "done\n" {
				t.Fatalf("%v %q", err, out.String())
			}
			if d := time.Since(start); d > 10*time.Second {
				t.Fatalf("took %v", d)
			}
			time.Sleep(3 * time.Second)
			if _, err := os.Stat(filepath.Join(dir, "late")); err == nil {
				t.Fatal("a background process outlived the run")
			}
		})
	}
}

func TestExecDoesNotHangOnBackgroundProcess(t *testing.T) {
	l := &Local{}
	start := time.Now()
	res, err := l.Exec(context.Background(), ExecRequest{Dir: os.TempDir(), Script: "sleep 60 & echo started"})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 10*time.Second || !strings.Contains(res.Output, "started") {
		t.Fatalf("a background process must not hold the call open: %v %+v", time.Since(start), res)
	}
}

func TestExecWithEnv(t *testing.T) {
	l := &Local{Env: []string{"PATH=" + os.Getenv("PATH"), "A=agent"}}
	res, err := l.Exec(context.Background(), ExecRequest{Dir: os.TempDir(), Script: `echo "$A $DB_DSN"`, Env: map[string]string{"DB_DSN": "localhost:54337"}})
	if err != nil || strings.TrimSpace(res.Output) != "agent localhost:54337" {
		t.Fatalf("%+v %v", res, err)
	}
}

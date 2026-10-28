package modelproxy

import (
	"context"
	"io"
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
	"git.shdw.tech/rob/devbot/internal/agent/claude"
	"git.shdw.tech/rob/devbot/internal/agent/codex"
)

// TestPinnedCLIsThroughTheProxy runs the real CLIs, the versions
// docker/Dockerfile pins, through the proxy to a fake of their vendor's API
// that has the model make one tool call and then finish. It is where the
// routes the proxy forwards come from: a run has to complete with nothing
// refused. Without a CLI on the PATH (or at DEVBOT_LIVE_<HARNESS>_COMMAND)
// it skips.
func TestPinnedCLIsThroughTheProxy(t *testing.T) {
	cases := []struct {
		name, harness, region string
		cred                  map[string]string // the proxy's
		header, want          string
	}{
		{"claude oauth", claude.Name, "", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-real"}, "Authorization", "Bearer sk-ant-oat-real"},
		{"claude api key", claude.Name, "", map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real"}, "X-Api-Key", "sk-ant-real"},
		{"codex api key", codex.Name, "", map[string]string{"CODEX_API_KEY": "sk-proj-real"}, "Authorization", "Bearer sk-proj-real"},
		{"codex bedrock", codex.Name, "us-east-1", map[string]string{"AWS_ACCESS_KEY_ID": "AKIAREAL", "AWS_SECRET_ACCESS_KEY": "aws-secret-real"},
			"Authorization", "AWS4-HMAC-SHA256 Credential=AKIAREAL/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := os.Getenv("DEVBOT_LIVE_" + strings.ToUpper(c.harness) + "_COMMAND")
			if cmd == "" {
				cmd = c.harness
			}
			if _, err := exec.LookPath(cmd); err != nil {
				t.Skipf("no %s on the PATH", cmd)
			}
			api := newModelAPI(t)
			up, err := For(c.harness, c.region, env(c.cred))
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var refused []string
			u, _ := url.Parse(api.URL)
			real := up.Base
			up.Base = func(method, path string) *url.URL {
				if real(method, path) == nil {
					mu.Lock()
					refused = append(refused, method+" "+path)
					mu.Unlock()
					return nil
				}
				return u
			}
			mp := serve(t, up)

			// Nothing of the developer's own setup: their CLI config and
			// variables would change what the CLI calls.
			home := t.TempDir()
			cliEnv := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "IS_SANDBOX=1"}
			var h agent.Harness
			if c.harness == claude.Name {
				h = &claude.Harness{Command: cmd, Proxy: mp, Env: cliEnv}
			} else {
				h = &codex.Harness{Command: cmd, Proxy: mp, HomeDir: filepath.Join(home, "codex"), Env: cliEnv, Log: quiet}
			}
			dir := t.TempDir()
			l := &agent.Local{Harnesses: map[string]agent.Harness{c.harness: h}}
			res, err := l.Run(context.Background(), agent.RunRequest{Harness: c.harness, Dir: dir, Prompt: "Say done.",
				BedrockRegion: c.region, IgnoreRepoConfig: true, Timeout: 3 * time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError {
				t.Fatalf("the run failed (%s): %s\n%s", res.ErrorClass, res.Result, res.StderrTail)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(refused) > 0 {
				t.Errorf("the proxy refused %q", refused)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if api.turns < 2 {
				t.Errorf("%d model requests, want the tool call and its answer", api.turns)
			}
			if v := api.auth; !strings.HasPrefix(v, c.want) {
				t.Errorf("the API got %s %q", c.header, v)
			}
		})
	}
}

// modelAPI answers the Messages and Responses APIs, streaming, with a tool
// call the CLI doesn't have and then, once its error comes back, "done".
type modelAPI struct {
	*httptest.Server
	mu    sync.Mutex
	turns int
	auth  string
}

func newModelAPI(t *testing.T) *modelAPI {
	a := &modelAPI{}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		var stream string
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/messages" && strings.Contains(body, `"stream":true`):
			stream = messagesDone
			if !strings.Contains(body, "tool_result") {
				stream = messagesTool
			}
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/v1/responses"):
			stream = responsesDone
			if !strings.Contains(body, "function_call_output") {
				stream = responsesTool
			}
		default:
			// Anything else the CLI asks for, the WebSocket included, a
			// real API would answer differently; the run must not need it.
			http.Error(w, `{"type":"error","error":{"type":"not_found_error","message":"not here"}}`, http.StatusNotFound)
			return
		}
		a.mu.Lock()
		a.turns++
		a.auth = r.Header.Get("Authorization") + r.Header.Get("X-Api-Key")
		a.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, stream)
	}))
	t.Cleanup(a.Close)
	return a
}

func sse(events ...string) string {
	var b strings.Builder
	for i := 0; i < len(events); i += 2 {
		b.WriteString("event: " + events[i] + "\ndata: " + events[i+1] + "\n\n")
	}
	return b.String()
}

func messages(block, delta, stop string) string {
	return sse(
		"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":`+block+`}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":`+delta+`}`,
		"content_block_stop", `{"type":"content_block_stop","index":0}`,
		"message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stop+`"},"usage":{"output_tokens":1}}`,
		"message_stop", `{"type":"message_stop"}`)
}

func responses(item string) string {
	return sse(
		"response.created", `{"type":"response.created","response":{"id":"resp_1"}}`,
		"response.output_item.done", `{"type":"response.output_item.done","item":`+item+`}`,
		"response.completed", `{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":1,"input_tokens_details":null,"output_tokens":1,"output_tokens_details":null,"total_tokens":2}}}`)
}

var (
	messagesTool = messages(`{"type":"tool_use","id":"toolu_1","name":"NoSuchTool","input":{}}`,
		`{"type":"input_json_delta","partial_json":"{}"}`, "tool_use")
	messagesDone = messages(`{"type":"text","text":""}`, `{"type":"text_delta","text":"done"}`, "end_turn")

	responsesTool = responses(`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"no_such_tool","arguments":"{}"}`)
	responsesDone = responses(`{"type":"message","role":"assistant","id":"msg_1","content":[{"type":"output_text","text":"done"}]}`)
)

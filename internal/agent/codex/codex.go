// Package codex is the agent harness for OpenAI's Codex CLI: codex exec
// --json, its JSONL event stream, and its error messages.
//
// Flags are those of codex-cli 0.164.0 (`codex exec --help`, `codex exec
// resume --help`), the version docker/Dockerfile pins.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/agent"
)

// Name is the harness's name in config (agents.<role>.harness).
const Name = "codex"

// BedrockProvider is Codex's built-in model provider for Amazon Bedrock
// (codex-cli 0.128.0 and later).
const BedrockProvider = "amazon-bedrock"

// APIKeyEnv is where codex exec reads an OpenAI API key from: the real one
// in the model proxy's environment, the placeholder in codex's.
const APIKeyEnv = "CODEX_API_KEY"

// Harness runs codex exec.
type Harness struct {
	Command   string   // default "codex"
	ExtraArgs []string // appended to every invocation
	Env       []string // nil = agent.ScrubbedEnv()
	// Proxy is where Codex reaches its model. Required.
	Proxy *agent.ModelProxy
	// HomeDir holds a CODEX_HOME per session. Default: the user cache dir.
	HomeDir string
	Log     *slog.Logger // nil = slog.Default()
}

func (h *Harness) Name() string { return Name }

// event is one line of codex exec --json.
type event struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"` // type error
	Error    struct {
		Message string `json:"message"`
	} `json:"error"` // type turn.failed
	Item struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Server  string `json:"server"`  // type mcp_tool_call
		Message string `json:"message"` // type error: a warning, the run goes on
	} `json:"item"`
	Usage struct {
		InputTokens       int `json:"input_tokens"`
		CachedInputTokens int `json:"cached_input_tokens"`
		CacheWriteTokens  int `json:"cache_write_input_tokens"`
		OutputTokens      int `json:"output_tokens"`
		ReasoningTokens   int `json:"reasoning_output_tokens"`
	} `json:"usage"`
}

// Codex mints UUIDs; anything else would be a path in HomeDir.
var sessionRE = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

func (h *Harness) Run(ctx context.Context, req agent.RunRequest) (agent.RunResult, error) {
	if req.Timeout <= 0 {
		req.Timeout = time.Hour
	}
	log := h.Log
	if log == nil {
		log = slog.Default()
	}
	p := h.Proxy
	if p == nil {
		return agent.RunResult{}, errors.New("codex: no model proxy")
	}
	if (req.BedrockRegion != "") != (p.Credential == agent.CredAWS) {
		return agent.RunResult{}, fmt.Errorf("codex: the model proxy holds a %s credential, which doesn't fit a run with Bedrock region %q", p.Credential, req.BedrockRegion)
	}
	home, err := h.home(req.Resume)
	if err != nil {
		return agent.RunResult{}, err
	}
	lastMsg := filepath.Join(home, "last-message.txt")
	_ = os.Remove(lastMsg) // a resume mustn't read the previous run's

	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	cmdName := h.Command
	if cmdName == "" {
		cmdName = "codex"
	}
	cmd := exec.CommandContext(ctx, cmdName, h.args(req, lastMsg, p.URL)...)
	cmd.Dir = req.Dir
	env := h.Env
	if env == nil {
		env = agent.ScrubbedEnv()
	}
	env = append(agent.WithOTELAttrs(agent.WithEnv(env, req.Env), req.OTELAttrs), "CODEX_HOME="+home)
	if req.BedrockRegion != "" {
		// The placeholder as a Bedrock API key, which the proxy swaps for
		// SigV4 with the pod's AWS credential.
		env = append(env, "AWS_REGION="+req.BedrockRegion, "AWS_BEARER_TOKEN_BEDROCK="+p.Token)
	} else {
		env = append(env, APIKeyEnv+"="+p.Token)
	}
	cmd.Env = env
	cmd.Stdin = strings.NewReader(req.Prompt)
	var stdout, stderr bytes.Buffer
	exit, runErr := agent.RunCLI(cmd, &stdout, &stderr)

	res := agent.RunResult{Exit: exit}
	if ctx.Err() != nil {
		res.TimedOut = true
	}
	// A CLI that never ran is the agent's failure, not the model's.
	if ee := (*exec.ExitError)(nil); runErr != nil && !errors.As(runErr, &ee) && !res.TimedOut {
		return res, runErr
	}

	if req.TranscriptBytes > 0 {
		res.Transcript, res.TranscriptTruncated = agent.Transcript(stdout.Bytes(), req.TranscriptBytes)
	}
	s := parse(stdout.Bytes())
	res.SessionID, res.Result, res.Usage, res.Turns = s.thread, s.message, s.usage, s.turns
	res.MCPCalls, res.MCPUnavailable = s.mcpCalls, s.mcpUnavailable
	// The stream's last agent message and --output-last-message should
	// agree; the file is the fallback when the stream lost it.
	if b, err := os.ReadFile(lastMsg); err == nil {
		file := strings.TrimSpace(string(b))
		if res.Result == "" {
			res.Result = file
		} else if file != "" && file != strings.TrimSpace(res.Result) {
			log.Warn("codex: the stream's last message and --output-last-message differ; using the stream's", "session", res.SessionID)
		}
	}
	if s.thread != "" && sessionRE.MatchString(s.thread) && req.Resume == "" {
		// Name the home after the session, for the resume. A link, not a
		// rename: Codex's state database records the rollout's absolute path.
		if err := os.Symlink(filepath.Base(home), filepath.Join(h.base(), s.thread)); err != nil {
			log.Warn("codex: keeping CODEX_HOME for resume", "err", err)
		}
	}

	failMsg := s.failure()
	res.IsError = failMsg != "" || res.ExitCode != 0 || res.TimedOut || s.thread == ""
	if !res.IsError {
		return res, nil
	}
	if failMsg == "" && res.Result == "" {
		res.Result = agent.Tail(stdout.String(), 4<<10)
	} else if failMsg != "" {
		res.Result = failMsg
	}
	// What Codex itself said went wrong. Never the agent's messages or the
	// commands' output in the stream, which could quote a usage-limit
	// message and so hold the whole fleet; raw stdout only when no stream
	// started.
	out := failMsg
	if out == "" {
		out = res.StderrTail
		if s.thread == "" {
			out = res.Result + "\n" + out
		}
	}
	class, resetAt := Classify(out, time.Now())
	switch {
	case class == agent.ErrRateLimited:
		res.RateLimited, res.ResetAt = true, resetAt
	case res.TimedOut:
		class = agent.ErrTimeout
	case class != "":
	default:
		// Codex doesn't document its error messages, so an unknown one is
		// logged whole, to grow the table from.
		log.Warn("codex: unclassified error", "session", res.SessionID, "exit_code", res.ExitCode, "message", out)
		class = agent.ErrHarness
		if s.thread != "" {
			class = agent.ErrModel
		}
	}
	res.ErrorClass = class
	return res, nil
}

// args builds codex exec, or codex exec resume. resume has no --sandbox,
// and a resumed session doesn't keep its sandbox or model, so both are
// passed again, the sandbox as a config override.
func (h *Harness) args(req agent.RunRequest, lastMsg, proxyURL string) []string {
	args := []string{"exec"}
	if req.Resume != "" {
		args = append(args, "resume")
	}
	args = append(args, "--json")
	if req.Resume != "" {
		args = append(args, "-c", `sandbox_mode="danger-full-access"`)
	} else {
		args = append(args, "--sandbox", "danger-full-access")
	}
	args = append(args, "--skip-git-repo-check", "--ignore-user-config", "-o", lastMsg)
	// Without these, Codex clones OpenAI's plugin catalogue into
	// CODEX_HOME/.tmp in the background (a git child still running after
	// codex exec returns) and puts its bundled skills in the agent's
	// context: egress and instructions the task didn't ask for. The repo's
	// own skills still load.
	args = append(args, "-c", "features.plugins=false", "-c", "skills.bundled.enabled=false")
	if req.IgnoreRepoConfig {
		// A fresh CODEX_HOME trusts no project, so Dir's .codex/ config,
		// hooks and exec policies are already off; saying so keeps them off
		// whatever trust Codex would assume. AGENTS.md and the repo's skills
		// load even then, unless capped to nothing.
		args = append(args, "--ignore-rules", "-c", "projects."+strconv.Quote(req.Dir)+".trust_level=\"untrusted\"",
			"-c", "project_doc_max_bytes=0", "-c", "skills.include_instructions=false")
	}
	if req.Model != "" {
		args = append(args, "-m", req.Model)
	}
	if req.BedrockRegion != "" {
		// The built-in provider, Bedrock's Responses API, at the proxy: it
		// sends AWS_BEARER_TOKEN_BEDROCK as a bearer token, and the proxy
		// forwards to bedrock-mantle in the region with SigV4.
		args = append(args, "-c", "model_provider="+strconv.Quote(BedrockProvider),
			"-c", "model_providers."+BedrockProvider+".base_url="+strconv.Quote(proxyURL+"/openai/v1"))
	} else {
		args = append(args, "-c", "openai_base_url="+strconv.Quote(proxyURL+"/v1"))
	}
	args = append(args, mcpArgs(req.MCPServers)...)
	if req.CoAuthoredBy != nil {
		// Codex adds its Co-authored-by trailer only behind the
		// codex_git_commit feature, off by default; setting it either way holds
		// whatever that default becomes. Only keys the pinned CLI knows: it
		// warns about an unknown one on every run.
		args = append(args, "-c", "features.codex_git_commit="+strconv.FormatBool(*req.CoAuthoredBy))
	}
	args = append(args, h.ExtraArgs...)
	if req.Resume != "" {
		args = append(args, req.Resume)
	}
	// "-": the prompt is on stdin, out of argv and its length limit.
	return append(args, "-")
}

// mcpArgs gives the run its MCP servers as config overrides. They are the
// only ones it has: --ignore-user-config and a fresh CODEX_HOME leave no
// user config or trusted project, and Codex loads a repo's
// .codex/config.toml only for a trusted one. A server that doesn't start
// isn't required, so Codex goes on without it.
func mcpArgs(servers []agent.MCPServer) []string {
	var args []string
	for _, s := range servers {
		key := "mcp_servers." + s.Name + "."
		args = append(args, "-c", key+"url="+strconv.Quote(s.URL), "-c", key+"required=false")
		if len(s.AllowedTools) > 0 {
			args = append(args, "-c", key+"enabled_tools="+tomlList(s.AllowedTools))
		}
		if len(s.DisabledTools) > 0 {
			args = append(args, "-c", key+"disabled_tools="+tomlList(s.DisabledTools))
		}
	}
	return args
}

func tomlList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = strconv.Quote(s)
	}
	return "[" + strings.Join(q, ",") + "]"
}

func (h *Harness) base() string {
	if h.HomeDir != "" {
		return h.HomeDir
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "devbot", "codex")
	}
	return filepath.Join(os.TempDir(), "devbot-codex")
}

// home is the run's CODEX_HOME: a new one for a new session, so no stray
// config, history or login applies, or the resumed session's own, which
// holds its rollout.
func (h *Harness) home(resume string) (string, error) {
	base := h.base()
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	if resume != "" {
		if !sessionRE.MatchString(resume) {
			return "", fmt.Errorf("codex: %q isn't a session id", resume)
		}
		run, err := os.Readlink(filepath.Join(base, resume))
		if err != nil || !strings.HasPrefix(run, "run-") || strings.ContainsRune(run, filepath.Separator) {
			return "", fmt.Errorf("codex: no CODEX_HOME for session %s to resume (%v)", resume, err)
		}
		return filepath.Join(base, run), nil
	}
	dir, err := os.MkdirTemp(base, "run-")
	if err != nil {
		return "", err
	}
	return dir, nil
}

type stream struct {
	thread, message   string
	usage             agent.Usage
	turns             int
	turnFailed, error string
	mcpCalls          map[string]int
	mcpUnavailable    []string
}

// mcpWarningRE picks the warnings about an MCP server out of the stream's
// error items. Codex reports a server that didn't start in its log rather
// than the stream, so these are what the stream does say.
var mcpWarningRE = regexp.MustCompile(`(?i)\bmcp\b`)

func parse(out []byte) stream {
	var s stream
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var e event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Type {
		case "thread.started":
			s.thread = e.ThreadID
		case "item.completed":
			switch e.Item.Type {
			case "agent_message":
				s.message = e.Item.Text
			case "mcp_tool_call":
				if s.mcpCalls == nil {
					s.mcpCalls = map[string]int{}
				}
				s.mcpCalls[e.Item.Server]++
			case "error":
				if mcpWarningRE.MatchString(e.Item.Message) {
					s.mcpUnavailable = append(s.mcpUnavailable, e.Item.Message)
				}
			}
		case "turn.completed":
			s.turns++
			s.usage.Add(agent.Usage{
				InputTokens:       e.Usage.InputTokens,
				CachedInputTokens: e.Usage.CachedInputTokens,
				CacheWriteTokens:  e.Usage.CacheWriteTokens,
				OutputTokens:      e.Usage.OutputTokens,
				ReasoningTokens:   e.Usage.ReasoningTokens,
			})
			s.error = "" // a turn that completed recovered from it
		case "turn.failed":
			s.turnFailed = e.Error.Message
		case "error":
			s.error = e.Message
		}
	}
	return s
}

// failure is the message of what ended the run, or "". Codex reports the
// retries it makes ("Reconnecting... 2/5 (...)") as error events too; those
// alone aren't a failure.
func (s stream) failure() string {
	if s.turnFailed != "" {
		return s.turnFailed
	}
	if s.error != "" && !strings.HasPrefix(s.error, "Reconnecting...") {
		return s.error
	}
	return ""
}

// classes maps Codex's error messages to devbot's error classes, first
// match wins. The CLI doesn't document them; these are its messages as of
// 0.163.0 (codex-rs/protocol/src/error.rs) and the API's statuses.
var classes = []struct {
	class agent.ErrorClass
	re    *regexp.Regexp
}{
	// "You've hit your usage limit. ... Try again at 3:45 PM.", and the
	// plan's other windows: "5h limit", "weekly limit", "You've reached
	// your session limit".
	{agent.ErrRateLimited, regexp.MustCompile(`(?i)usage limit|\b(session|weekly|daily|monthly|\d+ ?h|\d+[ -]?hour) limit\b|(hit|reached) your [\w .-]{0,40}\blimit\b`)},
	// "rate limit exceeded: ...", "exceeded retry limit, last status: 429 Too Many Requests"
	{agent.ErrRateLimited, regexp.MustCompile(`(?i)rate[ _-]?limit|too many requests|\b429\b`)},
	{agent.ErrRateLimited, regexp.MustCompile(`(?i)quota exceeded|insufficient_quota`)},
	{agent.ErrRateLimited, regexp.MustCompile(`(?i)model is at capacity`)},
	// Bedrock's throttling: ThrottlingException, ServiceQuotaExceededException,
	// "Too many tokens, please wait before trying again."
	{agent.ErrRateLimited, regexp.MustCompile(`(?i)throttlingexception|servicequotaexceeded|too many tokens`)},
	// "unexpected status 401 Unauthorized: Incorrect API key provided ...",
	// "... Missing bearer or basic authentication in header"
	{agent.ErrAuth, regexp.MustCompile(`(?i)\b401 unauthorized|invalid_api_key|incorrect api key|missing bearer`)},
	{agent.ErrAuth, regexp.MustCompile(`(?i)refresh token|token_expired|not logged in|codex login`)},
	// UsageNotIncluded: the ChatGPT plan can't use Codex.
	{agent.ErrAuth, regexp.MustCompile(`(?i)with your chatgpt plan, upgrade`)},
	// On Bedrock: no AWS credential, a wrong or expired one, or a role
	// without access to the model (amazon_bedrock/auth.rs, error.rs).
	{agent.ErrAuth, regexp.MustCompile(`(?i)failed to (resolve amazon bedrock auth|load aws credentials)|aws signature has expired|` +
		`security token included in the request is (invalid|expired)|unrecognizedclientexception|invalidclienttokenid|expiredtoken|accessdeniedexception|is not authorized to perform`)},
	{agent.ErrModel, regexp.MustCompile(`(?i)model_not_found|does not exist|context window|content_filter|invalid_request_error`)},
}

var (
	// Codex prints a reset in the agent's local time: "3:45 PM" today, else
	// "Oct 18th, 2026 3:45 PM"; a zone, when one is named, in parentheses.
	resetAtRE = agent.ClockRE(`(?:try again at|\bresets?(?: at| on)?)`)
	// The API's "Please try again in 20s" / "in 1.5s" / "in 6m0s".
	resetInRE = regexp.MustCompile(`(?i)try again in (\d+(?:\.\d+)?(?:ms|s|m|h)(?:\d+(?:\.\d+)?(?:ms|s|m|h))*)`)
)

// Classify sorts an error message into an error class, "" when no pattern
// matches, and for a rate limit, when it resets if the message says.
func Classify(msg string, now time.Time) (agent.ErrorClass, time.Time) {
	for _, c := range classes {
		if !c.re.MatchString(msg) {
			continue
		}
		if c.class != agent.ErrRateLimited {
			return c.class, time.Time{}
		}
		return c.class, resetAt(msg, now)
	}
	return "", time.Time{}
}

func resetAt(msg string, now time.Time) time.Time {
	if m := resetAtRE.FindStringSubmatch(msg); m != nil {
		return agent.ClockTime(m, now)
	}
	if m := resetInRE.FindStringSubmatch(msg); m != nil {
		if d, err := time.ParseDuration(m[1]); err == nil {
			return now.Add(d)
		}
	}
	return time.Time{}
}

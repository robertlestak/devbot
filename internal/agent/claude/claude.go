// Package claude is the agent harness for Claude Code: claude -p, its
// stream-json output, and its usage-limit messages.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
const Name = "claude"

// envPrefixes are the variables Claude Code reads its settings from
// (ANTHROPIC_DEFAULT_HAIKU_MODEL, CLAUDE_CODE_ENABLE_TELEMETRY), kept through
// agent.ScrubbedEnv, which drops its credential if one is there.
var envPrefixes = []string{"CLAUDE_", "ANTHROPIC_"}

// Harness runs claude -p.
type Harness struct {
	Command   string   // default "claude"
	ExtraArgs []string // appended to every invocation
	Env       []string // nil = agent.ScrubbedEnv(envPrefixes...)
	// Proxy is where Claude Code reaches its model. Required.
	Proxy *agent.ModelProxy
}

func (h *Harness) Name() string { return Name }

// event is one line of claude -p --output-format stream-json: the session's
// init, each message, and last the result.
type event struct {
	Type string `json:"type"`
	// type system, subtype init: the MCP servers and how connecting went.
	MCPServers []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
	// type assistant: the model's turn, tool calls among its content.
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
	result
}

// result is the fields of the result event.
type result struct {
	SessionID string  `json:"session_id"`
	Result    string  `json:"result"`
	IsError   bool    `json:"is_error"`
	Subtype   string  `json:"subtype"`
	CostUSD   float64 `json:"total_cost_usd"`
	Turns     int     `json:"num_turns"`
	Usage     struct {
		InputTokens         int `json:"input_tokens"`
		OutputTokens        int `json:"output_tokens"`
		CacheReadTokens     int `json:"cache_read_input_tokens"`
		CacheCreationTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func (h *Harness) Run(ctx context.Context, req agent.RunRequest) (agent.RunResult, error) {
	if req.Timeout <= 0 {
		req.Timeout = time.Hour
	}
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	cmdName := h.Command
	if cmdName == "" {
		cmdName = "claude"
	}
	// The stream, not the single json result, so tool calls can be counted
	// and a transcript kept. -p prints it only with --verbose.
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Resume != "" {
		args = append(args, "--resume", req.Resume)
	}
	if req.IgnoreRepoConfig {
		// User settings only: no project or local settings.json (hooks, env,
		// apiKeyHelper), CLAUDE.md, commands, agents or skills from Dir. MCP
		// servers are --mcp-config's alone for every run.
		args = append(args, "--setting-sources", "user")
	}
	mcpDir, err := os.MkdirTemp("", "devbot-claude-mcp-")
	if err != nil {
		return agent.RunResult{}, err
	}
	defer os.RemoveAll(mcpDir)
	mcpArgs, err := writeMCPConfig(filepath.Join(mcpDir, "mcp.json"), req.MCPServers)
	if err != nil {
		return agent.RunResult{}, err
	}
	args = append(args, mcpArgs...)
	if req.CoAuthoredBy != nil {
		settingsArgs, err := writeSettings(filepath.Join(mcpDir, "settings.json"), *req.CoAuthoredBy)
		if err != nil {
			return agent.RunResult{}, err
		}
		args = append(args, settingsArgs...)
	}
	args = append(args, h.ExtraArgs...)
	p := h.Proxy
	if p == nil {
		return agent.RunResult{}, errors.New("claude: no model proxy")
	}
	if (req.BedrockRegion != "") != (p.Credential == agent.CredAWS) {
		return agent.RunResult{}, fmt.Errorf("claude: the model proxy holds a %s credential, which doesn't fit a run with Bedrock region %q", p.Credential, req.BedrockRegion)
	}
	env := h.Env
	if env == nil {
		env = agent.ScrubbedEnv(envPrefixes...)
	}
	env = agent.WithOTELAttrs(agent.WithEnv(env, req.Env), req.OTELAttrs)
	if req.BedrockRegion != "" {
		// Claude Code sends the placeholder as a Bedrock API key, and the
		// proxy signs the request with the pod's AWS credential instead.
		env = append(env, "CLAUDE_CODE_USE_BEDROCK=1", "AWS_REGION="+req.BedrockRegion,
			"ANTHROPIC_BEDROCK_BASE_URL="+p.URL, "AWS_BEARER_TOKEN_BEDROCK="+p.Token)
	} else {
		// The placeholder goes where the real credential would, so Claude
		// Code talks to the API as it would with that.
		env = append(env, "ANTHROPIC_BASE_URL="+p.URL)
		if p.Credential == agent.CredOAuthToken {
			env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+p.Token)
		} else {
			env = append(env, "ANTHROPIC_API_KEY="+p.Token)
		}
	}
	cmd := exec.CommandContext(ctx, cmdName, args...)
	cmd.Dir = req.Dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(req.Prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	runErr := agent.RunGroup(cmd)

	res := agent.RunResult{Duration: time.Since(start), Stderr: agent.Tail(stderr.String(), 8<<10)}
	if ctx.Err() != nil {
		res.TimedOut = true
	}
	if ee := (*exec.ExitError)(nil); errors.As(runErr, &ee) {
		res.ExitCode = ee.ExitCode()
	} else if runErr != nil && !res.TimedOut {
		return res, runErr
	}
	if req.TranscriptBytes > 0 {
		res.Transcript, res.TranscriptTruncated = agent.Transcript(stdout.Bytes(), req.TranscriptBytes)
	}
	s := parse(stdout.Bytes())
	res.MCPCalls, res.MCPUnavailable = s.mcpCalls, s.mcpUnavailable
	parsed := false
	if r := s.result; r != nil {
		res.SessionID, res.Result, res.IsError, res.Turns = r.SessionID, r.Result, r.IsError, r.Turns
		res.Usage = agent.Usage{
			InputTokens:       r.Usage.InputTokens,
			OutputTokens:      r.Usage.OutputTokens,
			CachedInputTokens: r.Usage.CacheReadTokens,
			CacheWriteTokens:  r.Usage.CacheCreationTokens,
		}
		// Claude Code prices a run at Anthropic's API rates, which aren't
		// what Bedrock bills.
		if req.BedrockRegion == "" {
			cost := r.CostUSD
			res.CostUSD = &cost
		}
		parsed = true
	}
	if res.SessionID == "" && res.Result == "" {
		res.IsError = true
		// Only the noise: the rest of the stream is the model's messages and
		// tool output, which would be read for a limit and posted to the
		// forge in the failure comment.
		res.Result = agent.Tail(string(s.noise), 4<<10)
		parsed = false
	}
	// out is what the CLI itself said went wrong: an is_error result, its
	// noise when there was no result, and stderr. Never the model's own
	// final message, which could quote a usage-limit message and so hold the
	// whole fleet.
	out := res.Stderr
	if res.IsError {
		out = res.Result + "\n" + out
	}
	if res.ExitCode != 0 {
		res.IsError = true
	}
	if res.IsError {
		res.RateLimited, res.ResetAt = DetectRateLimit(out, time.Now())
		switch {
		case res.RateLimited:
			res.ErrorClass = agent.ErrRateLimited
		case res.TimedOut:
			res.ErrorClass = agent.ErrTimeout
		case authRE.MatchString(out):
			res.ErrorClass = agent.ErrAuth
		case parsed:
			res.ErrorClass = agent.ErrModel
		default:
			res.ErrorClass = agent.ErrHarness
		}
	}
	return res, nil
}

var (
	// Claude Code names the limit it hit in many ways: "Claude AI usage limit
	// reached", "You've hit your session limit", "Weekly limit reached",
	// "You've reached your Opus limit", "5-hour limit reached". The last line
	// is Bedrock's throttling: ThrottlingException,
	// ServiceQuotaExceededException, "Too many tokens, please wait before
	// trying again."
	rateLimitRE = regexp.MustCompile(`(?i)(usage limit|rate[ _-]?limit|limit reached|too many requests|\b429\b|quota exceeded|out of extra usage|` +
		`\b(session|weekly|daily|monthly|hourly|\d+[ -]?hour|opus|sonnet|haiku|fable|plan) limit\b|(hit|reached) your [\w .-]{0,40}\blimit\b|` +
		`throttlingexception|servicequotaexceeded|too many tokens)`)
	resetEpochRE = regexp.MustCompile(`\|(\d{10})\b`)
	resetInRE    = regexp.MustCompile(`(?i)(?:try again|retry|resets?) in (\d+)\s*(second|minute|hour)`)
	// "resets 6pm (UTC)", "resets Oct 20, 9am", "will reset at 3pm".
	resetClockRE = agent.ClockRE(`\bresets?(?: at| on)?`)
	// authRE is how Claude Code reports a missing, wrong or expired
	// credential; the last line, an AWS one on Bedrock, or a role without
	// access to the model.
	authRE = regexp.MustCompile(`(?i)(authentication_error|invalid api key|invalid x-api-key|oauth token (has )?expired|please run /login|not logged in|API Error: 401\b|` +
		`security token included in the request is (invalid|expired)|unrecognizedclientexception|expiredtoken|could not load credentials|accessdeniedexception|is not authorized to perform|don't have access to the model)`)
)

// DetectRateLimit recognises a usage or rate limit in the CLI's own error
// output and, when the message says, when it resets. Claude Code retries
// transient 429s itself; what reaches here is a limit it gave up on.
func DetectRateLimit(s string, now time.Time) (bool, time.Time) {
	if !rateLimitRE.MatchString(s) {
		return false, time.Time{}
	}
	if m := resetEpochRE.FindStringSubmatch(s); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			return true, time.Unix(n, 0)
		}
	}
	if m := resetInRE.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[string]time.Duration{"second": time.Second, "minute": time.Minute, "hour": time.Hour}[strings.ToLower(m[2])]
		return true, now.Add(time.Duration(n) * unit)
	}
	if m := resetClockRE.FindStringSubmatch(s); m != nil {
		return true, agent.ClockTime(m, now)
	}
	return true, time.Time{}
}

type stream struct {
	result         *result // the last result event; nil if none
	mcpCalls       map[string]int
	mcpUnavailable []string
	// noise is what the CLI printed outside the stream, such as a startup
	// error.
	noise []byte
}

// parse reads the stream. Lines that aren't JSON events are the CLI's own
// noise. A line that starts like an event but doesn't parse was cut off by
// a kill: it carries the model's messages or tool output, so it's dropped
// rather than taken for noise.
func parse(out []byte) stream {
	var s stream
	// Split, not a Scanner: a tool's output can make one line longer than
	// any buffer, and the result comes after it.
	for line := range bytes.SplitSeq(out, []byte("\n")) {
		if t := bytes.TrimSpace(line); !bytes.HasPrefix(t, []byte("{")) {
			if len(t) > 0 {
				s.noise = append(append(s.noise, t...), '\n')
			}
			continue
		}
		var e event
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch e.Type {
		case "system":
			// Claude Code goes on without a server it can't connect to.
			for _, m := range e.MCPServers {
				if m.Status != "connected" && m.Status != "pending" {
					s.mcpUnavailable = append(s.mcpUnavailable, m.Name+": "+m.Status)
				}
			}
		case "assistant":
			for _, c := range e.Message.Content {
				name, ok := strings.CutPrefix(c.Name, "mcp__")
				server, _, isMCP := strings.Cut(name, "__")
				if c.Type == "tool_use" && ok && isMCP {
					if s.mcpCalls == nil {
						s.mcpCalls = map[string]int{}
					}
					s.mcpCalls[server]++
				}
			}
		case "result":
			r := e.result
			s.result = &r
		}
	}
	return s
}

// writeMCPConfig writes the run's --mcp-config to path and returns the
// flags that load it and nothing else: --strict-mcp-config ignores the
// repo's .mcp.json and every settings file's servers, so a repo can't give
// the model a server the install didn't.
func writeMCPConfig(path string, servers []agent.MCPServer) ([]string, error) {
	type entry struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	cfg := struct {
		MCPServers map[string]entry `json:"mcpServers"`
	}{MCPServers: map[string]entry{}}
	var allowed, disallowed []string
	for _, s := range servers {
		cfg.MCPServers[s.Name] = entry{Type: s.Type, URL: s.URL}
		for _, t := range s.AllowedTools {
			allowed = append(allowed, "mcp__"+s.Name+"__"+t)
		}
		for _, t := range s.DisabledTools {
			disallowed = append(disallowed, "mcp__"+s.Name+"__"+t)
		}
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	args := []string{"--mcp-config", path, "--strict-mcp-config"}
	if len(allowed) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowed, ","))
	}
	if len(disallowed) > 0 {
		args = append(args, "--disallowedTools", strings.Join(disallowed, ","))
	}
	return args, nil
}

// writeSettings writes the run's --settings file, which wins over every
// settings file but managed ones, a repo's .claude/settings.json among them.
// Off, Claude Code adds neither its Co-Authored-By trailer nor its "Generated
// with" line, to commits or PRs: empty attribution strings, which every
// version since attribution replaced includeCoAuthoredBy reads, and
// includeCoAuthoredBy for those before. On is Claude Code's default; the
// harness adds the trailer to any commit that still lacks it.
func writeSettings(path string, coAuthoredBy bool) ([]string, error) {
	settings := map[string]any{"includeCoAuthoredBy": coAuthoredBy}
	if !coAuthoredBy {
		settings["attribution"] = map[string]any{"commit": "", "pr": "", "sessionUrl": false}
	}
	b, err := json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	return []string{"--settings", path}, nil
}

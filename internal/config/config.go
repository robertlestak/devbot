// Package config holds the two kinds of configuration: the deployment's
// (one file per install, from Helm values - installs differ only here)
// and each repo's .devbot.yaml (policy that travels with the code).
package config

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// Config is one devbot install.
type Config struct {
	Providers []ProviderConfig `yaml:"providers"`

	Redis Redis `yaml:"redis"`

	// Authorization is who may direct devbot. The forge is the source of
	// truth: anyone it gives at least MinPermission on a repo, which a
	// repo's .devbot.yaml authorized_users can only narrow.
	Authorization struct {
		// MinPermission is write (default), maintain or admin.
		MinPermission string `yaml:"min_permission"`
		// Min is MinPermission parsed.
		Min forge.Permission `yaml:"-"`
	} `yaml:"authorization"`

	// Agents picks the agent harness and model each role runs.
	Agents Agents `yaml:"agents"`
	// MCPServers is the install's catalog of MCP servers, by name. A role
	// gets those its agents.<role>.mcp_servers names, and a repo can only
	// narrow that; nothing else defines a server.
	MCPServers map[string]MCPServer `yaml:"mcp_servers"`

	Listen string `yaml:"listen"` // serve's address, default :8080
	// MetricsAddr is where serve answers /metrics, default :9090: a listener
	// of its own, so the metrics (repo names among them) aren't on the
	// public connector address.
	MetricsAddr string `yaml:"metrics_addr"`
	// TrustedProxies are the addresses (CIDRs or IPs) of the proxies in
	// front of serve. Only a request from one of them has its client
	// address taken from X-Forwarded-For, for per-address limits.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// Proxies is TrustedProxies parsed.
	Proxies []netip.Prefix `yaml:"-"`
	// AllowedOrigins are the browser origins (scheme://host[:port]) whose
	// pages may call /mcp and /api, besides auth.resource_url's. A browser
	// request from any other origin is refused, so a web page can't drive
	// the connectors through someone's browser. A browser names the origin
	// on writes, not on same-origin reads: those are allowed_hosts' to stop.
	AllowedOrigins []string `yaml:"allowed_origins"`
	// AllowedHosts are the host names, besides auth.resource_url's and
	// allowed_origins', that requests to /api, /mcp and /ui may be sent to in
	// auth mode none. Any other Host is refused, so a page that rebinds its
	// own name to serve's address can't read them. IP addresses and
	// localhost are always allowed: a rebinding page's Host is its own name.
	AllowedHosts []string `yaml:"allowed_hosts"`
	// WebhookMaxBytes is the largest webhook body serve reads. A bigger one
	// is refused before its signature is checked.
	WebhookMaxBytes int64 `yaml:"webhook_max_bytes"`

	// ReconcileInterval is how often serve re-reads every devbot item. Webhooks
	// are the fast path; this repairs whatever they missed.
	ReconcileInterval time.Duration `yaml:"reconcile_interval"`
	// EventRetention trims the event feed. Clients whose cursor falls off the
	// end fall back to list_tasks.
	EventRetention time.Duration `yaml:"event_retention"`
	// EvalLockTTL bounds how long one evaluation holds its task's lock, so a
	// process that dies mid-evaluation doesn't block the task for long.
	EvalLockTTL time.Duration `yaml:"eval_lock_ttl"`
	// LeaderLease is how long serve's leader holds the lease that runs the
	// singleton loops without renewing it: how long a leader that died
	// leaves them stopped before another replica takes over.
	LeaderLease time.Duration `yaml:"leader_lease"`

	Backoff Backoff `yaml:"backoff"`

	// Claude configures the claude harness.
	Claude struct {
		// Command is the claude binary. Tests point it at a stub.
		Command string `yaml:"command"`
		// ExtraArgs are appended to every invocation.
		ExtraArgs []string `yaml:"extra_args"`
	} `yaml:"claude"`

	// Codex configures the codex harness.
	Codex struct {
		// Command is the codex binary. Tests point it at a stub.
		Command string `yaml:"command"`
		// ExtraArgs are appended to every invocation, before the prompt.
		ExtraArgs []string `yaml:"extra_args"`
	} `yaml:"codex"`

	// Auth is how connector callers (MCP, /api) are identified.
	Auth Auth `yaml:"auth"`

	// AnonymousPrincipal is the principal for connector calls in auth mode
	// none. Empty turns the connectors off. Only for an endpoint reachable
	// from a trusted network: no forge permission applies to these callers.
	AnonymousPrincipal string       `yaml:"anonymous_principal"`
	Subscribers        []Subscriber `yaml:"subscribers"`

	// UI is serve's optional read-only web UI, at /ui/ on the connector
	// listener. Off unless enabled; the routes don't exist then.
	UI struct {
		Enabled bool `yaml:"enabled"`
		// GrafanaURL, when set, is linked from every page: the install's
		// dashboard.
		GrafanaURL string `yaml:"grafana_url"`
	} `yaml:"ui"`

	// Repos decides which repos devbot works in, and the policy for those
	// without a .devbot.yaml.
	Repos RepoPolicy `yaml:"repos"`

	// Workdir is where agent pods clone. An emptyDir in the pod.
	Workdir string `yaml:"workdir"`

	// Docker is the per-run Docker daemon a repo can ask for (.devbot.yaml
	// docker). The chart runs it in the job pod; serve needs to know there
	// is one, to queue such a repo's jobs where a pod with it takes them.
	Docker struct {
		// Mode is one of DockerModes, or empty for none.
		Mode string `yaml:"mode"`
		// Detect gives a repo whose policy leaves docker unset a daemon when
		// its base branch shows it needs one (RepoCache.DetectDocker). Unset:
		// on when the install runs a daemon.
		Detect *bool `yaml:"detect"`
	} `yaml:"docker"`

	// Metrics picks the attribution labels on the cost, token and duration
	// metrics. Every label multiplies their series, so the install bounds it.
	Metrics Metrics `yaml:"metrics"`

	// PodLabels has serve label each running job pod with its job's repo,
	// task, role and requester, for cost tools that allocate by pod label.
	PodLabels struct {
		Enabled bool `yaml:"enabled"`
		// Instance is the release whose job pods serve labels: a pod
		// without app.kubernetes.io/instance set to it is left alone, so a
		// forged lease can't make serve touch anyone else's pod.
		Instance string `yaml:"instance"`
	} `yaml:"pod_labels"`

	// Services is how serve runs the service pods a repo's runs ask for
	// (.devbot.yaml services).
	Services Services `yaml:"services"`

	// CIFailures is how much of what CI said about a failing check the
	// implementer's fix round and the reviewer are shown.
	CIFailures struct {
		// LogLines is the most lines, the last ones, of one failed job's log.
		LogLines int `yaml:"log_lines"`
		// MaxBytes caps all of it together, every check's log, output and
		// annotations.
		MaxBytes int `yaml:"max_bytes"`
	} `yaml:"ci_failures"`

	// Audit is what devbot adds to its audit records (one JSON line per
	// action, on stdout) beyond the actions themselves.
	Audit struct {
		// Transcript writes every model run's transcript to the harness
		// container's stdout as audit lines, redacted. Off by default: it is
		// large, and holds whatever the model read.
		Transcript bool `yaml:"transcript"`
		// TranscriptMaxBytes caps one run's transcript; the rest is dropped.
		TranscriptMaxBytes int `yaml:"transcript_max_bytes"`
	} `yaml:"audit"`
}

// Metrics is which attribution labels devbot's cost metrics carry.
type Metrics struct {
	// CostLabels are among CostLabelNames. Unset: repo and owner.
	CostLabels []string `yaml:"cost_labels"`
	// StaticLabels are keys of repos' labels exported as label_<key>.
	StaticLabels []string `yaml:"static_labels"`
}

// CostLabelNames are the values metrics.cost_labels takes.
var CostLabelNames = []string{"repo", "owner", "requester"}

// AttributionLabels are the label names the cost, token and duration
// metrics carry after their own: the cost labels, then label_<key> for
// each static label.
func (m Metrics) AttributionLabels() []string {
	out := slices.Clone(m.CostLabels)
	for _, k := range m.StaticLabels {
		out = append(out, "label_"+k)
	}
	return out
}

func (m Metrics) validate() error {
	seen := map[string]bool{}
	for _, l := range m.CostLabels {
		if !slices.Contains(CostLabelNames, l) || seen[l] {
			return fmt.Errorf("metrics.cost_labels: %q; want each of %s at most once", l, strings.Join(CostLabelNames, ", "))
		}
		seen[l] = true
	}
	for _, k := range m.StaticLabels {
		if !labelKeyRE.MatchString(k) || seen["label_"+k] {
			return fmt.Errorf("metrics.static_labels: %q isn't a label key (lowercase letters, digits and _, at most 63), or is listed twice", k)
		}
		seen["label_"+k] = true
	}
	return nil
}

// Auth is connector authentication. In mode none, every caller is
// AnonymousPrincipal. In mode oauth, devbot is an OAuth authorization server
// for its connectors and an OAuth client of one forge, which is the identity
// provider: every caller is a person signed in to that forge, and what they
// start is done on the forge with their own token.
type Auth struct {
	Mode string `yaml:"mode"` // none (default) | oauth
	// ResourceURL is devbot's public base URL, as clients reach it: the
	// audience of its tokens and the base of its OAuth endpoints.
	ResourceURL string `yaml:"resource_url"`
	// Provider is the forge people sign in with. Default: the only one.
	Provider string `yaml:"provider"`
	// The forge OAuth app's credentials, and the key that encrypts people's
	// forge tokens at rest, come from the environment.
	ClientIDEnv     string `yaml:"client_id_env"`
	ClientSecretEnv string `yaml:"client_secret_env"`
	KeyEnv          string `yaml:"key_env"`
	// PreviousKeyEnv still decrypts while a key rotation finishes.
	PreviousKeyEnv  string        `yaml:"previous_key_env"`
	AccessTokenTTL  time.Duration `yaml:"access_token_ttl"`
	RefreshTokenTTL time.Duration `yaml:"refresh_token_ttl"`
	// RefreshReuseGrace is how long a just-rotated refresh token, presented
	// again by the same client, gets the same new tokens back instead of
	// ending the sign-in (default 1m; 0 turns it off). Processes sharing one
	// stored sign-in refresh at about the same time.
	RefreshReuseGrace *time.Duration `yaml:"refresh_reuse_grace"`
	// DynamicDeviceGrant lets dynamically registered clients use the device
	// grant (`devbot login`). Every client devbot knows registers itself,
	// and a device code can be phished (RFC 8628 5.4), so it is off unless
	// the install opts in.
	DynamicDeviceGrant bool `yaml:"dynamic_device_grant"`
	// RateLimits bound what one client address may ask of the OAuth
	// endpoints, which anyone can reach and most of which write to Redis.
	RateLimits RateLimits `yaml:"rate_limits"`
}

// RateLimits are per-address token buckets on the OAuth endpoints, and a
// cap on the clients one address may register.
type RateLimits struct {
	Register            RateLimit `yaml:"register"`
	Authorize           RateLimit `yaml:"authorize"`
	DeviceAuthorization RateLimit `yaml:"device_authorization"`
	Token               RateLimit `yaml:"token"`
	// Device is the verification page, /oauth/device.
	Device RateLimit `yaml:"device"`
	// ClientsPerAddress is how many clients one address may register a
	// day, the time an unused registration lives.
	ClientsPerAddress int `yaml:"clients_per_address"`
}

// RateLimit is a token bucket: PerMinute requests a minute on average, up
// to Burst at once.
type RateLimit struct {
	PerMinute float64 `yaml:"per_minute"`
	Burst     int     `yaml:"burst"`
}

func (l *RateLimit) orDefault(perMinute float64, burst int) {
	if l.PerMinute == 0 {
		l.PerMinute = perMinute
	}
	if l.Burst == 0 {
		l.Burst = burst
	}
}

// OAuth reports whether connector callers sign in through the forge.
func (a Auth) OAuth() bool { return a.Mode == "oauth" }

// ReuseGrace is refresh_reuse_grace, or its default when unset.
func (a Auth) ReuseGrace() time.Duration {
	if a.RefreshReuseGrace == nil {
		return time.Minute
	}
	return *a.RefreshReuseGrace
}

type ProviderConfig struct {
	Name string `yaml:"name"` // used in repo refs: "gitea" -> gitea:owner/name
	Type string `yaml:"type"` // "gitea" | "github"
	// URL is the forge's base URL; for GitHub, its API base
	// (https://api.github.com, or https://ghe.example.com/api/v3).
	URL string `yaml:"url"`

	// Gitea: each bot is a user with a token.
	ImplementerUser string `yaml:"implementer_user"`
	ReviewerUser    string `yaml:"reviewer_user"`
	// Secrets come from the environment, never from this file, so the config
	// can live in a ConfigMap.
	ImplementerTokenEnv string `yaml:"implementer_token_env"`
	ReviewerTokenEnv    string `yaml:"reviewer_token_env"`

	// GitHub: each bot is a GitHub App, whose login is its slug plus
	// "[bot]". The env vars hold the Apps' PEM private keys.
	ImplementerAppID  int64  `yaml:"implementer_app_id"`
	ImplementerKeyEnv string `yaml:"implementer_key_env"`
	ReviewerAppID     int64  `yaml:"reviewer_app_id"`
	ReviewerKeyEnv    string `yaml:"reviewer_key_env"`
	// LowQuotaPercent (GitHub) is the share of an installation's core
	// rate limit below which low-priority work waits for its reset:
	// reconcile's discovery, and webhook events no task stands behind.
	// Unset is 10; 0 never defers.
	LowQuotaPercent *int `yaml:"low_quota_percent"`

	WebhookSecretEnv string `yaml:"webhook_secret_env"`
}

// validate checks that p sets its type's identity keys and none of the
// other type's, so a key that would be ignored is an error instead.
func (p ProviderConfig) validate() error {
	userKeys := p.ImplementerUser != "" || p.ReviewerUser != "" || p.ImplementerTokenEnv != "" || p.ReviewerTokenEnv != ""
	appKeys := p.ImplementerAppID != 0 || p.ReviewerAppID != 0 || p.ImplementerKeyEnv != "" || p.ReviewerKeyEnv != ""
	switch p.Type {
	case "gitea":
		if p.LowQuotaPercent != nil {
			return fmt.Errorf("provider %q: low_quota_percent is for github providers: Gitea sets no rate limit devbot reads", p.Name)
		}
		if appKeys {
			return fmt.Errorf("provider %q: a gitea provider's bots are users: set *_user and *_token_env, not *_app_id or *_key_env", p.Name)
		}
		// The merge gate's two identities are only two if the bots are
		// different users, each with its own token.
		if p.ImplementerUser != "" && strings.EqualFold(p.ImplementerUser, p.ReviewerUser) {
			return fmt.Errorf("provider %q: the implementer and the reviewer must be different Gitea users", p.Name)
		}
		if p.ImplementerTokenEnv != "" && p.ImplementerTokenEnv == p.ReviewerTokenEnv {
			return fmt.Errorf("provider %q: implementer_token_env and reviewer_token_env must be different env vars, one per bot", p.Name)
		}
	case "github":
		if n := p.LowQuotaPercent; n != nil && (*n < 0 || *n >= 100) {
			return fmt.Errorf("provider %q: low_quota_percent %d; want 0 to 99", p.Name, *n)
		}
		if userKeys {
			return fmt.Errorf("provider %q: a github provider's bots are GitHub Apps: set *_app_id and *_key_env, not *_user or *_token_env", p.Name)
		}
		if p.ImplementerAppID <= 0 || p.ReviewerAppID <= 0 || p.ImplementerKeyEnv == "" || p.ReviewerKeyEnv == "" {
			return fmt.Errorf("provider %q: a github provider needs implementer_app_id, implementer_key_env, reviewer_app_id and reviewer_key_env", p.Name)
		}
		if p.ImplementerAppID == p.ReviewerAppID {
			return fmt.Errorf("provider %q: the implementer and the reviewer must be different GitHub Apps", p.Name)
		}
	default:
		return fmt.Errorf("provider %q: type %q; want gitea or github", p.Name, p.Type)
	}
	return nil
}

// Agents holds each role's agent settings.
type Agents struct {
	Implement Agent `yaml:"implement"`
	Review    Agent `yaml:"review"`
}

// Agent is which harness runs a role, with which model. The harness decides
// the agent pod's secret, so it is the install's alone; a repo can only
// change the model.
type Agent struct {
	Harness string `yaml:"harness"`
	Model   string `yaml:"model"`
	// AWS, when set, runs the role's model on Amazon Bedrock.
	AWS *AWS `yaml:"aws"`
	// MCPServers are the catalog servers the role's agent gets. Default none.
	MCPServers []string `yaml:"mcp_servers"`
}

// MCPServer is one MCP server in the install's catalog.
type MCPServer struct {
	Type string `yaml:"type"` // one of MCPTypes
	URL  string `yaml:"url"`
	// Headers maps a header to the environment variable holding its value.
	// Refused for now: the agent CLI would send it from the agent
	// container, where the model can read anything.
	Headers map[string]string `yaml:"headers"`
	// AllowedTools, when set, are the only tools of the server the agent
	// may call; DisabledTools are tools it may not.
	AllowedTools  []string `yaml:"allowed_tools"`
	DisabledTools []string `yaml:"disabled_tools"`
}

// MCPTypes are the transports mcp_servers.<name>.type takes. Both are
// remote: a stdio server would run inside the agent container.
var MCPTypes = []string{"http", "sse"}

// MCP server names become part of tool names (mcp__<server>__<tool>) and of
// config keys, so they are kept to what both harnesses take as they are.
var mcpNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,62}$`)

func (s MCPServer) validate(name string) error {
	if !mcpNameRE.MatchString(name) {
		return fmt.Errorf("mcp_servers: %q: a name is letters, digits and dashes", name)
	}
	switch {
	case s.Type == "stdio":
		return fmt.Errorf("mcp_servers.%s: stdio servers aren't supported; run the server as a service and give its http URL", name)
	case !slices.Contains(MCPTypes, s.Type):
		return fmt.Errorf("mcp_servers.%s.type is %q; want %s", name, s.Type, strings.Join(MCPTypes, " or "))
	}
	if u, err := url.Parse(s.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("mcp_servers.%s.url %q: want an http(s) URL", name, s.URL)
	}
	if len(s.Headers) > 0 {
		return fmt.Errorf("mcp_servers.%s.headers: devbot can't keep a header's value from the model, because the agent CLI would send it "+
			"from the agent container; use a server that needs no credential, reachable only from the job pods", name)
	}
	for _, t := range slices.Concat(s.AllowedTools, s.DisabledTools) {
		if t == "" || strings.ContainsAny(t, " ,\t\n") {
			return fmt.Errorf("mcp_servers.%s: tool %q: want a tool's name", name, t)
		}
	}
	return nil
}

// AWS is where a role reaches Bedrock, and with which credential.
type AWS struct {
	Region string `yaml:"region"`
	// Auth is how the role's agent sidecar gets its AWS credential, one of
	// AWSAuths. The chart acts on it; devbot only checks it.
	Auth string `yaml:"auth"`
}

// AWSAuths are the values agents.<role>.aws.auth takes.
var AWSAuths = []string{"irsa", "pod_identity", "access_keys"}

// DockerModes are the values docker.mode takes besides empty (no daemon):
// a rootless Docker daemon in a sidecar (docs/operating.md#docker).
var DockerModes = []string{"dind"}

// Harnesses are the agent harnesses devbot runs, with each one's default
// model.
var Harnesses = map[string]string{"claude": "claude-opus-5-5", "codex": "gpt-6.1-sol"}

type Backoff struct {
	Base       time.Duration `yaml:"base"`
	Cap        time.Duration `yaml:"cap"`
	MaxRetries int           `yaml:"max_retries"`
	MaxHold    time.Duration `yaml:"max_hold"`
}

// RepoPolicy is the install's rule for which repos devbot works in. A repo
// is enabled when it passes allow/deny, both bots can write to it, and either
// it has an enabled .devbot.yaml or OptIn is "any". A .devbot.yaml with
// enabled: false always opts a repo out.
type RepoPolicy struct {
	// OptIn is "any" (default): any repo the bots can write to, using
	// Defaults when it has no .devbot.yaml; or "file": only repos that
	// commit an enabled .devbot.yaml.
	OptIn string `yaml:"opt_in"`
	// Allow and Deny are globs over "provider:owner/name"
	// ("gitea:acme/*", "github:kubernetes/*"). Empty Allow means all; Deny wins.
	Allow []string `yaml:"allow"`
	Deny  []string `yaml:"deny"`
	// Defaults is the policy for a repo with no .devbot.yaml, written in
	// .devbot.yaml's own keys, over DefaultRepo's built-in values. Its
	// commits block is also the base of a repo's own (Repo.LayerCommits).
	Defaults map[string]any `yaml:"defaults"`
	// Limits cap what a .devbot.yaml may set: anyone who can push to a repo
	// can write one, and the install pays for its runs.
	Limits RepoLimits `yaml:"limits"`
}

// RepoLimits are the install's ceilings on a repo's policy. A zero one (nil
// check_retries) is no ceiling; Load fills them all in.
type RepoLimits struct {
	MaxAttempts     int  `yaml:"max_attempts"`
	CheckRetries    *int `yaml:"check_retries"`
	MaxReviewRounds int  `yaml:"max_review_rounds"`
	MaxRebaseRounds int  `yaml:"max_rebase_rounds"`
	Timeouts        struct {
		Implement time.Duration `yaml:"implement"`
		Review    time.Duration `yaml:"review"`
	} `yaml:"timeouts"`
	// Models are the models a repo may pick, for either role. Empty allows
	// any; the install's own are always allowed.
	Models []string `yaml:"models"`
	// Services are which images a repo's services may run, and how big.
	Services ServiceLimits `yaml:"services"`
	// Docker false gives no repo a Docker daemon, whatever its docker says.
	// Unset: whether the install runs one (docker.mode).
	Docker *bool `yaml:"docker"`
	// PullRequests.Enabled false lets no repo hand devbot a PR it didn't
	// open, whatever its pull_requests says, and PullRequests.Forks false
	// none from a fork.
	PullRequests PullRequests `yaml:"pull_requests"`
}

// Allowed reports whether repo passes allow/deny.
func (p RepoPolicy) Allowed(repo string) bool {
	for _, d := range p.Deny {
		if Glob(d, repo) {
			return false
		}
	}
	if len(p.Allow) == 0 {
		return true
	}
	for _, a := range p.Allow {
		if Glob(a, repo) {
			return true
		}
	}
	return false
}

// Redis is how every mode reaches Redis. Secrets come from the environment
// variables the *_env fields name.
type Redis struct {
	Addr string `yaml:"addr"`
	DB   int    `yaml:"db"`
	// The ACL user and its password, by the env vars that carry them.
	// serve and the job harness share this file but not the user, so
	// each pod sets the same vars to its own.
	UsernameEnv string `yaml:"username_env"`
	PasswordEnv string `yaml:"password_env"`
	TLS         struct {
		Enabled bool `yaml:"enabled"`
		// CAEnv names a variable holding PEM certificates to trust instead
		// of the system's.
		CAEnv      string `yaml:"ca_env"`
		ServerName string `yaml:"server_name"`
	} `yaml:"tls"`
	// Sentinel, when Master is set, finds the master through Sentinel,
	// and Addr is unused.
	Sentinel struct {
		Master      string   `yaml:"master"`
		Addrs       []string `yaml:"addrs"`
		Username    string   `yaml:"username"`
		PasswordEnv string   `yaml:"password_env"`
	} `yaml:"sentinel"`
}

type Subscriber struct {
	Name      string   `yaml:"name"`
	URL       string   `yaml:"url"`
	SecretEnv string   `yaml:"secret_env"`
	Types     []string `yaml:"types"`         // empty = all
	Origin    string   `yaml:"origin_prefix"` // empty = all
}

func Load(path string) (*Config, error) {
	c := &Config{}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	c.defaults()
	return c, c.validate()
}

func (c *Config) defaults() {
	if c.Redis.Addr == "" {
		c.Redis.Addr = "localhost:6379"
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.MetricsAddr == "" {
		c.MetricsAddr = ":9090"
	}
	if c.WebhookMaxBytes == 0 {
		c.WebhookMaxBytes = 5 << 20
	}
	if c.ReconcileInterval == 0 {
		c.ReconcileInterval = 2 * time.Minute
	}
	if c.EventRetention == 0 {
		c.EventRetention = 14 * 24 * time.Hour
	}
	if c.EvalLockTTL == 0 {
		c.EvalLockTTL = 5 * time.Minute
	}
	if c.LeaderLease == 0 {
		c.LeaderLease = 15 * time.Second
	}
	if c.Backoff.Base == 0 {
		c.Backoff.Base = time.Minute
	}
	if c.Backoff.Cap == 0 {
		c.Backoff.Cap = time.Hour
	}
	if c.Backoff.MaxRetries == 0 {
		c.Backoff.MaxRetries = 10
	}
	if c.Backoff.MaxHold == 0 {
		c.Backoff.MaxHold = 24 * time.Hour
	}
	if c.Claude.Command == "" {
		c.Claude.Command = "claude"
	}
	if c.Codex.Command == "" {
		c.Codex.Command = "codex"
	}
	if c.Workdir == "" {
		c.Workdir = "/work"
	}
	if c.Audit.TranscriptMaxBytes == 0 {
		c.Audit.TranscriptMaxBytes = 4 << 20
	}
	if c.CIFailures.LogLines == 0 {
		c.CIFailures.LogLines = 150
	}
	if c.CIFailures.MaxBytes == 0 {
		c.CIFailures.MaxBytes = 64 << 10
	}
	if c.Authorization.MinPermission == "" {
		c.Authorization.MinPermission = "write"
	}
	if c.Repos.OptIn == "" {
		c.Repos.OptIn = "any"
	}
	for i := range c.Providers {
		if p := &c.Providers[i]; p.Type == "github" && p.LowQuotaPercent == nil {
			n := 10
			p.LowQuotaPercent = &n
		}
	}
	// The timeouts stop short of the chart's pod deadlines
	// (jobs.<role>.activeDeadlineSeconds), so a run ends on its own terms.
	l := &c.Repos.Limits
	if l.MaxAttempts == 0 {
		l.MaxAttempts = 10
	}
	if l.CheckRetries == nil {
		n := 5
		l.CheckRetries = &n
	}
	if l.MaxReviewRounds == 0 {
		l.MaxReviewRounds = 10
	}
	if l.MaxRebaseRounds == 0 {
		l.MaxRebaseRounds = 30
	}
	if l.Timeouts.Implement == 0 {
		l.Timeouts.Implement = time.Hour
	}
	if l.Timeouts.Review == 0 {
		l.Timeouts.Review = 25 * time.Minute
	}
	c.servicesDefaults()
	if l.Docker == nil {
		on := c.Docker.Mode != ""
		l.Docker = &on
	}
	if l.PullRequests.Enabled == nil {
		on := true
		l.PullRequests.Enabled = &on
	}
	if l.PullRequests.Forks == nil {
		on := true
		l.PullRequests.Forks = &on
	}
	if c.Docker.Detect == nil {
		on := c.Docker.Mode != ""
		c.Docker.Detect = &on
	}
	if c.Auth.Mode == "" {
		c.Auth.Mode = "none"
	}
	if c.Metrics.CostLabels == nil {
		c.Metrics.CostLabels = []string{"repo", "owner"}
	}
	if c.Auth.Provider == "" && len(c.Providers) == 1 {
		c.Auth.Provider = c.Providers[0].Name
	}
	c.Auth.ResourceURL = strings.TrimRight(c.Auth.ResourceURL, "/")
	if c.Auth.AccessTokenTTL == 0 {
		c.Auth.AccessTokenTTL = time.Hour
	}
	if c.Auth.RefreshTokenTTL == 0 {
		c.Auth.RefreshTokenTTL = 30 * 24 * time.Hour
	}
	// Room for a few people behind one NAT; a script hammering an endpoint
	// runs into them within seconds.
	rl := &c.Auth.RateLimits
	rl.Register.orDefault(10, 20)
	rl.Authorize.orDefault(30, 30)
	rl.DeviceAuthorization.orDefault(10, 10)
	rl.Token.orDefault(60, 60) // a device polls every 5s while its person signs in
	rl.Device.orDefault(20, 20)
	if rl.ClientsPerAddress == 0 {
		rl.ClientsPerAddress = 50
	}
	impl, rev := &c.Agents.Implement, &c.Agents.Review
	if impl.Harness == "" {
		impl.Harness = "claude"
	}
	// Bedrock names models its own way, so a role on it has no default.
	if impl.Model == "" && impl.AWS == nil {
		impl.Model = Harnesses[impl.Harness]
	}
	if rev.Harness == "" {
		rev.Harness = "claude"
	}
	if rev.Model == "" && rev.Harness == impl.Harness && (rev.AWS == nil) == (impl.AWS == nil) {
		rev.Model = impl.Model
	} else if rev.Model == "" && rev.AWS == nil {
		rev.Model = Harnesses[rev.Harness]
	}
	// A repo may always pick what the install runs anyway.
	if len(l.Models) > 0 {
		for _, m := range []string{impl.Model, rev.Model} {
			if m != "" && !slices.Contains(l.Models, m) {
				l.Models = append(l.Models, m)
			}
		}
	}
}

func (c *Config) validate() error {
	if c.Audit.TranscriptMaxBytes < 0 {
		return fmt.Errorf("audit.transcript_max_bytes is %d; want a positive size in bytes", c.Audit.TranscriptMaxBytes)
	}
	if c.CIFailures.LogLines < 0 {
		return fmt.Errorf("ci_failures.log_lines is %d; want a positive number of lines", c.CIFailures.LogLines)
	}
	if c.CIFailures.MaxBytes < 0 {
		return fmt.Errorf("ci_failures.max_bytes is %d; want a positive size in bytes", c.CIFailures.MaxBytes)
	}
	for _, name := range slices.Sorted(maps.Keys(c.MCPServers)) {
		if err := c.MCPServers[name].validate(name); err != nil {
			return err
		}
	}
	for _, r := range []struct {
		role string
		a    Agent
	}{{"implement", c.Agents.Implement}, {"review", c.Agents.Review}} {
		if _, ok := Harnesses[r.a.Harness]; !ok {
			return fmt.Errorf("agents.%s.harness is %q; want %s", r.role, r.a.Harness, strings.Join(slices.Sorted(maps.Keys(Harnesses)), ", "))
		}
		for i, n := range r.a.MCPServers {
			s, ok := c.MCPServers[n]
			switch {
			case !ok:
				return fmt.Errorf("agents.%s.mcp_servers: %q isn't in mcp_servers", r.role, n)
			case slices.Contains(r.a.MCPServers[:i], n):
				return fmt.Errorf("agents.%s.mcp_servers: %q is listed twice", r.role, n)
			case s.Type == "sse" && r.a.Harness == "codex":
				return fmt.Errorf("agents.%s.mcp_servers: %q is an sse server, and codex speaks only streamable http", r.role, n)
			// Claude Code runs with --dangerously-skip-permissions, which
			// allows every tool, so an allow list would restrict nothing.
			case len(s.AllowedTools) > 0 && r.a.Harness == "claude":
				return fmt.Errorf("agents.%s.mcp_servers: %q sets allowed_tools, which claude doesn't enforce with permission prompts off; list the tools to keep away in disabled_tools", r.role, n)
			}
		}
		if aws := r.a.AWS; aws != nil {
			if aws.Region == "" || r.a.Model == "" {
				return fmt.Errorf("agents.%s.aws: a role on Bedrock needs aws.region and a model (a Bedrock model ID or inference profile)", r.role)
			}
			if !slices.Contains(AWSAuths, aws.Auth) {
				return fmt.Errorf("agents.%s.aws.auth is %q; want %s", r.role, aws.Auth, strings.Join(AWSAuths, ", "))
			}
		}
	}
	if p := c.AnonymousPrincipal; p != "" && !ValidName(p) {
		return fmt.Errorf("anonymous_principal is %q; want %s", p, NameRule)
	}
	if c.WebhookMaxBytes < 1 {
		return errors.New("webhook_max_bytes must be positive")
	}
	for _, o := range c.AllowedOrigins {
		if u, err := url.Parse(o); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("allowed_origins: %q isn't an origin like https://app.example.com", o)
		}
	}
	for _, h := range c.AllowedHosts {
		if h == "" || strings.ContainsAny(h, "/:@?#* ") {
			return fmt.Errorf("allowed_hosts: %q isn't a host name like devbot.example.com", h)
		}
	}
	if s := c.Redis.Sentinel; s.Master != "" && len(s.Addrs) == 0 {
		return errors.New("redis.sentinel.master needs redis.sentinel.addrs")
	} else if s.Master == "" && len(s.Addrs) > 0 {
		return errors.New("redis.sentinel.addrs needs redis.sentinel.master")
	}
	if c.LeaderLease < 0 {
		return errors.New("leader_lease must be positive")
	}
	if c.Repos.OptIn != "any" && c.Repos.OptIn != "file" {
		return fmt.Errorf("repos.opt_in is %q; want any or file", c.Repos.OptIn)
	}
	switch p, _ := forge.ParsePermission(c.Authorization.MinPermission); p {
	case forge.PermWrite, forge.PermMaintain, forge.PermAdmin:
		c.Authorization.Min = p
	default:
		// Below write, people who can't push could set an agent to work.
		return fmt.Errorf("authorization.min_permission is %q; want write, maintain or admin", c.Authorization.MinPermission)
	}
	if l := c.Repos.Limits; l.MaxAttempts < 1 || *l.CheckRetries < 0 || l.MaxReviewRounds < 1 || l.MaxRebaseRounds < 1 || l.Timeouts.Implement <= 0 || l.Timeouts.Review <= 0 {
		return errors.New("repos.limits: max_attempts and the rounds must be at least 1, check_retries at least 0, and the timeouts positive")
	}
	if m := c.Docker.Mode; m != "" && !slices.Contains(DockerModes, m) {
		return fmt.Errorf("docker.mode is %q; want %s, or empty for none", m, strings.Join(DockerModes, " or "))
	}
	if *c.Repos.Limits.Docker && c.Docker.Mode == "" {
		return errors.New("repos.limits.docker is true, but this install runs no Docker daemon (docker.mode)")
	}
	if err := c.Metrics.validate(); err != nil {
		return err
	}
	if c.PodLabels.Enabled && c.PodLabels.Instance == "" {
		return errors.New("pod_labels.enabled needs pod_labels.instance: the release whose job pods serve labels")
	}
	if err := c.validateServices(); err != nil {
		return err
	}
	d, err := DefaultRepo(c.Repos.Defaults)
	if err != nil {
		return fmt.Errorf("repos.defaults: %w", err)
	}
	if err := c.CheckServices(d); err != nil {
		return fmt.Errorf("repos.defaults: %w", err)
	}
	for _, role := range []string{"implement", "review"} {
		if _, err := d.Agent(role, c.Agents); err != nil {
			return fmt.Errorf("repos.defaults: %w", err)
		}
	}
	seen := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" || p.Type == "" || p.URL == "" {
			return fmt.Errorf("provider %+v: name, type and url are required", p)
		}
		if seen[p.Name] {
			return fmt.Errorf("provider %q configured twice", p.Name)
		}
		seen[p.Name] = true
		if err := p.validate(); err != nil {
			return err
		}
	}
	if g := c.UI.GrafanaURL; g != "" {
		if u, err := url.Parse(g); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("ui.grafana_url %q: want an http(s) URL", g)
		}
	}
	c.Proxies = nil
	for _, t := range c.TrustedProxies {
		p, err := netip.ParsePrefix(t)
		if err != nil {
			a, aerr := netip.ParseAddr(t)
			if aerr != nil {
				return fmt.Errorf("trusted_proxies: %q is neither a CIDR nor an address", t)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		c.Proxies = append(c.Proxies, p.Masked())
	}
	return c.Auth.validate(seen)
}

func (a Auth) validate(providers map[string]bool) error {
	switch a.Mode {
	case "none":
		return nil
	case "oauth":
	default:
		return fmt.Errorf("auth.mode is %q; want none or oauth", a.Mode)
	}
	u, err := url.Parse(a.ResourceURL)
	if a.ResourceURL == "" || err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("auth.resource_url %q: want devbot's public base URL, like https://devbot.example.com", a.ResourceURL)
	}
	// OAuth tokens and codes cross this URL; plain http only for local testing.
	if h := u.Hostname(); u.Scheme != "https" && !(u.Scheme == "http" && (h == "localhost" || h == "127.0.0.1" || h == "::1")) {
		return fmt.Errorf("auth.resource_url %q: must be https", a.ResourceURL)
	}
	if !providers[a.Provider] {
		return fmt.Errorf("auth.provider %q: want one of the configured providers' names", a.Provider)
	}
	if a.ClientIDEnv == "" || a.ClientSecretEnv == "" || a.KeyEnv == "" {
		return errors.New("auth.mode oauth needs auth.client_id_env, auth.client_secret_env and auth.key_env")
	}
	if a.AccessTokenTTL < time.Minute || a.RefreshTokenTTL < a.AccessTokenTTL {
		return errors.New("auth.access_token_ttl must be at least 1m, and refresh_token_ttl at least that")
	}
	if g := a.ReuseGrace(); g < 0 || g > a.AccessTokenTTL {
		return errors.New("auth.refresh_reuse_grace must be between 0 and access_token_ttl")
	}
	return nil
}

// Env reads a secret named by a *_env field.
func Env(name string) string {
	if name == "" {
		return ""
	}
	return os.Getenv(name)
}

// nameRE is what a principal, client label or requester handle may be.
// devbot writes them into comments under its own name, so nothing in one
// can open markup or a marker.
var nameRE = regexp.MustCompile("^" + NameRule + "$")

// ValidName reports whether s may be a principal, client label or handle.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// NameRule is ValidName's rule, for error messages.
const NameRule = "[A-Za-z0-9._-]{1,64}"

// DockerDetects reports whether a repo that leaves docker unset gets a
// daemon by what its base branch has: the install runs one, and
// docker.detect isn't off.
func (c *Config) DockerDetects() bool {
	return c.Docker.Mode != "" && (c.Docker.Detect == nil || *c.Docker.Detect)
}

// DockerSettled is whether r's runs get a Docker daemon as far as its policy
// and the install say; settled is false when that is left to what the base
// branch has (RepoCache.DetectDocker). repos.limits.docker false is already
// in r.Docker (Cap).
func (c *Config) DockerSettled(r *Repo) (on, settled bool) {
	if r.Docker != nil {
		return *r.Docker, true
	}
	return false, !c.DockerDetects()
}

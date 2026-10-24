// devbot turns issues into reviewed pull requests. One binary, several modes:
//
//	devbot serve                  webhooks, connectors (HTTP + MCP), reconcile loop
//	devbot run implement|review   one job from the queue (a KEDA ScaledJob pod's harness)
//	devbot agent implement|review   the model's side of a job pod (Unix-socket sidecar)
//	devbot model-proxy implement|review   a job pod's model proxy, holding the model credential
//	devbot reconcile              one reconcile pass, then exit
//	devbot file|project|status|list|answer|events|cancel|retry|rebase|approve|retarget|wait|edit   talk to a running serve
//	devbot coord                  in a job's agent container: the run's view of other tasks, and notes to them
//	devbot login|logout|token     sign in to a serve in auth.mode oauth (device flow)
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/agent/claude"
	"git.shdw.tech/rob/devbot/internal/agent/codex"
	"git.shdw.tech/rob/devbot/internal/audit"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/gitea"
	"git.shdw.tech/rob/devbot/internal/forge/github"
	"git.shdw.tech/rob/devbot/internal/harness"
	"git.shdw.tech/rob/devbot/internal/modelproxy"
	"git.shdw.tech/rob/devbot/internal/queue"
	"git.shdw.tech/rob/devbot/internal/server"
)

var version = "dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd := os.Args[1]; cmd {
	case "serve":
		err = serve(ctx, log)
	case "run":
		if len(os.Args) < 3 {
			usage()
		}
		err = run(ctx, log, os.Args[2])
	case "agent":
		if len(os.Args) < 3 {
			usage()
		}
		err = runAgent(ctx, log, os.Args[2])
	case "model-proxy":
		if len(os.Args) < 3 {
			usage()
		}
		err = runModelProxy(ctx, log, os.Args[2])
	case "reconcile":
		err = reconcile(ctx, log)
	case "file", "project", "status", "list", "answer", "events", "cancel", "retry", "rebase", "approve", "retarget", "wait", "edit":
		err = cli(ctx, cmd, os.Args[2:])
	case "login":
		err = login(ctx)
	case "logout":
		err = logout(ctx)
	case "token":
		err = printToken(ctx)
	case "coord":
		os.Exit(harness.CoordCommand(os.Args[2:], os.Stdout, os.Stderr))
	case "version":
		fmt.Println(version)
	default:
		usage()
	}
	if err != nil {
		log.Error(os.Args[1], "err", err)
		os.Exit(1)
	}
}

// usageText lists every command and flag. docs/connectors.md's CLI
// reference holds it as it is, and usage_test.go checks both against the
// commands and flags the code has.
const usageText = `usage: devbot serve | reconcile | run implement|review | agent implement|review | model-proxy implement|review
       devbot file -repo REPO -title TITLE -ac CRITERIA [-body BODY] [-origin ORIGIN] [-requester HANDLE] [-base BRANCH] [-parent PARENT] [-automerge]
       devbot project -repo REPO -title TITLE [-body BODY] [-ac CRITERIA] [-origin ORIGIN] [-plan -repos REPOS]
       devbot status REF | list [-repo REPO] [-state STATE] [-origin PREFIX] [-parent PARENT] | events [-cursor CURSOR] [-origin PREFIX]
       devbot answer REF TEXT | cancel REF | retry REF | rebase REF | approve REF SHA | retarget REF BRANCH | wait REF #N|REF2|none [until tagged [PATTERN][, build against]]
       devbot edit REF [-title TITLE] [-body BODY] [-ac CRITERIA] [-note NOTE]
       devbot login | logout | token | version
       devbot coord list | show TASK | note TASK TEXT|- | notes
config: DEVBOT_CONFIG (file); for the client commands DEVBOT_URL, and DEVBOT_TOKEN or a devbot login (DEVBOT_TOKEN_FILE)`

func usage() {
	fmt.Fprintln(os.Stderr, usageText)
	os.Exit(2)
}

// engine builds the engine for one mode, component, whose audit records go
// to stdout. It has both bots' credentials, or, for a job pod of role only,
// that bot's alone: its forges act as that bot, and the pod reads nothing
// else from its environment, whatever is there.
func engine(ctx context.Context, log *slog.Logger, component string, only forge.Role) (*core.Engine, *config.Config, error) {
	cfg, err := config.Load(os.Getenv("DEVBOT_CONFIG"))
	if err != nil {
		return nil, nil, err
	}
	q, err := redisStore(cfg.Redis)
	if err != nil {
		return nil, nil, err
	}
	secret := func(role forge.Role, env string) string {
		if only != "" && role != only {
			return ""
		}
		return config.Env(env)
	}
	al := audit.New(os.Stdout, component)
	al.Secret(secrets(cfg)...)
	reg := forge.Registry{}
	for _, p := range cfg.Providers {
		// Only serve takes webhooks and signs people in, and only the forge
		// people sign in with gets the OAuth app's credentials.
		var webhookSecret, clientID, clientSecret string
		if only == "" {
			webhookSecret = config.Env(p.WebhookSecretEnv)
			if cfg.Auth.OAuth() && p.Name == cfg.Auth.Provider {
				clientID, clientSecret = config.Env(cfg.Auth.ClientIDEnv), config.Env(cfg.Auth.ClientSecretEnv)
			}
		}
		switch p.Type {
		case "gitea":
			if only != "" {
				env := map[forge.Role]string{forge.RoleImplementer: p.ImplementerTokenEnv, forge.RoleReviewer: p.ReviewerTokenEnv}[only]
				if config.Env(env) == "" {
					return nil, nil, fmt.Errorf("provider %s: $%s is empty: it must hold the %s's token", p.Name, env, only)
				}
			} else if tok := config.Env(p.ImplementerTokenEnv); tok != "" && tok == config.Env(p.ReviewerTokenEnv) {
				// One token is one user, whatever the logins say.
				return nil, nil, fmt.Errorf("provider %s: $%s and $%s hold the same token: the implementer and the reviewer need one each", p.Name, p.ImplementerTokenEnv, p.ReviewerTokenEnv)
			}
			reg[p.Name] = gitea.New(gitea.Config{
				Name: p.Name, URL: p.URL,
				ImplementerUser: p.ImplementerUser, ImplementerTok: secret(forge.RoleImplementer, p.ImplementerTokenEnv),
				ReviewerUser: p.ReviewerUser, ReviewerTok: secret(forge.RoleReviewer, p.ReviewerTokenEnv),
				WebhookSecret: webhookSecret,
				OAuthClientID: clientID, OAuthClientSecret: clientSecret,
			})
		case "github":
			for _, k := range []struct {
				role forge.Role
				env  string
			}{{forge.RoleImplementer, p.ImplementerKeyEnv}, {forge.RoleReviewer, p.ReviewerKeyEnv}} {
				if (only == "" || k.role == only) && config.Env(k.env) == "" {
					return nil, nil, fmt.Errorf("provider %s: $%s is empty: it must hold the GitHub App's PEM private key", p.Name, k.env)
				}
			}
			gc := github.Config{
				Name: p.Name, URL: p.URL,
				ImplementerAppID: p.ImplementerAppID, ImplementerKey: []byte(secret(forge.RoleImplementer, p.ImplementerKeyEnv)),
				ReviewerAppID: p.ReviewerAppID, ReviewerKey: []byte(secret(forge.RoleReviewer, p.ReviewerKeyEnv)),
				WebhookSecret: webhookSecret,
				Allowed:       func(r forge.RepoRef) bool { return cfg.Repos.Allowed(r.String()) },
				OAuthClientID: clientID, OAuthClientSecret: clientSecret,
			}
			// Every replica and job pod shares what GitHub says of each
			// credential's rate limits, so none fires into one another found
			// spent.
			name := p.Name
			gc.LoadQuota = func(ctx context.Context, bucket string, res ...string) ([]forge.Quota, error) {
				return q.Quotas(ctx, name, bucket, res...)
			}
			gc.NoteQuota = func(ctx context.Context, bucket, res string, qt forge.Quota) error {
				return q.NoteQuota(ctx, name, bucket, res, qt)
			}
			if p.LowQuotaPercent != nil {
				gc.LowQuotaPercent = *p.LowQuotaPercent
			}
			if only == "" {
				// Whichever replica gets an installation webhook, the
				// leader's reconcile lists the repos again. A job pod's
				// inventory lives no longer than the pod.
				gc.InventoryGeneration = func(ctx context.Context) (string, error) { return q.InventoryGeneration(ctx, name) }
				gc.BumpInventory = func(ctx context.Context) error { return q.BumpInventory(ctx, name) }
			}
			if only != "" {
				// The other App's login takes its key to read; serve records
				// both on each reconcile pass. A pod uses it only to tell the
				// bots' comments apart, and serve, which gates every merge,
				// reads its own from GitHub.
				impl, rev, err := q.Bots(ctx, p.Name)
				if err != nil {
					return nil, nil, err
				}
				if impl == "" || rev == "" {
					return nil, nil, fmt.Errorf("provider %s: serve hasn't recorded the bots' logins yet (it does on each reconcile pass)", p.Name)
				}
				gc.ImplementerLogin, gc.ReviewerLogin = impl, rev
			}
			c, err := github.New(ctx, gc)
			if err != nil {
				return nil, nil, fmt.Errorf("provider %s: %w", p.Name, err)
			}
			// An App short of a permission otherwise shows only as repos
			// devbot isn't enabled for. Warned, not fatal: other
			// installations may be fine.
			if only == "" {
				problems, err := c.CheckInstallations(ctx)
				if err != nil {
					log.Warn("reading the GitHub Apps' installations", "provider", p.Name, "err", err)
				}
				for _, s := range problems {
					log.Warn("GitHub App installation lacks a permission devbot needs", "provider", p.Name, "problem", s)
				}
			}
			reg[p.Name] = c
		default:
			return nil, nil, fmt.Errorf("provider %s: unsupported type %q", p.Name, p.Type)
		}
	}
	reg = audit.Registry(reg, al)
	return &core.Engine{Forges: reg, Q: q, Cfg: cfg, Repos: &config.RepoCache{Forges: reg, Policy: cfg.Repos, Generation: q.RepoConfigGeneration}, Log: log, Audit: al}, cfg, nil
}

// redisStore connects to Redis as config.redis says, with its secrets from
// the environment.
func redisStore(r config.Redis) (*queue.Store, error) {
	o := queue.Options{
		Addr: r.Addr, DB: r.DB, Username: config.Env(r.UsernameEnv), Password: config.Env(r.PasswordEnv),
		SentinelMaster: r.Sentinel.Master, SentinelAddrs: r.Sentinel.Addrs,
		SentinelUsername: r.Sentinel.Username, SentinelPassword: config.Env(r.Sentinel.PasswordEnv),
	}
	for key, env := range map[string]string{"username_env": r.UsernameEnv, "password_env": r.PasswordEnv, "sentinel.password_env": r.Sentinel.PasswordEnv} {
		if env != "" && config.Env(env) == "" {
			return nil, fmt.Errorf("redis.%s: $%s is empty", key, env)
		}
	}
	if r.TLS.Enabled {
		o.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: r.TLS.ServerName}
		if r.TLS.CAEnv != "" {
			o.TLS.RootCAs = x509.NewCertPool()
			if !o.TLS.RootCAs.AppendCertsFromPEM([]byte(config.Env(r.TLS.CAEnv))) {
				return nil, fmt.Errorf("redis.tls.ca_env: $%s holds no PEM certificate", r.TLS.CAEnv)
			}
		}
	}
	return queue.Open(o), nil
}

// secrets are the values of every credential the config names, which no
// audit record may contain.
func secrets(cfg *config.Config) []string {
	envs := []string{cfg.Auth.ClientSecretEnv, cfg.Auth.KeyEnv, cfg.Auth.PreviousKeyEnv, cfg.Redis.PasswordEnv, cfg.Redis.Sentinel.PasswordEnv}
	for _, p := range cfg.Providers {
		envs = append(envs, p.ImplementerTokenEnv, p.ReviewerTokenEnv, p.ImplementerKeyEnv, p.ReviewerKeyEnv, p.WebhookSecretEnv)
	}
	for _, s := range cfg.Subscribers {
		envs = append(envs, s.SecretEnv)
	}
	var out []string
	for _, e := range envs {
		if e != "" {
			out = append(out, config.Env(e))
		}
	}
	return out
}

func serve(ctx context.Context, log *slog.Logger) error {
	e, cfg, err := engine(ctx, log, "serve", "")
	if err != nil {
		return err
	}
	s, err := server.New(e, cfg, log, version)
	if err != nil {
		return err
	}
	return s.Run(ctx)
}

func reconcile(ctx context.Context, log *slog.Logger) error {
	e, _, err := engine(ctx, log, "reconcile", "")
	if err != nil {
		return err
	}
	n, err := e.Reconcile(ctx)
	log.Info("reconciled", "acted", n)
	return err
}

func run(ctx context.Context, log *slog.Logger, kind string) error {
	role, ok := map[string]forge.Role{queue.KindImplement: forge.RoleImplementer, queue.KindReview: forge.RoleReviewer}[kind]
	if !ok {
		return fmt.Errorf("unknown job kind %q", kind)
	}
	e, cfg, err := engine(ctx, log, "harness", role)
	if err != nil {
		return err
	}
	socket := envOr("DEVBOT_AGENT_SOCKET", "/run/devbot/agent.sock")
	var ag agent.Runner
	if socket == "local" {
		// Single-container mode, for development only: the model shares this
		// process's container and could read the forge token, and the model
		// credential its in-process proxy holds.
		if err := checkHarnessCLI(cfg, kind); err != nil {
			return err
		}
		mp, err := localModelProxy(ctx, log, cfg, kind)
		if err != nil {
			return err
		}
		ag = localAgent(cfg, mp)
	} else {
		c := agent.NewClient(socket)
		if err := c.Wait(ctx, 2*time.Minute); err != nil {
			return err
		}
		ag = c
	}
	idents := map[string][2]string{}
	for name, p := range e.Forges {
		impl, _ := p.BotUsers() // on GitHub, the App's "<slug>[bot]"
		idents[name] = [2]string{impl, envOr("DEVBOT_COMMIT_EMAIL_"+strings.ToUpper(name), impl+"@users.noreply.local")}
	}
	r := &harness.Runner{
		E: e, Agent: ag, Owner: harness.Hostname(), Log: log, Workdir: cfg.Workdir, PrivateDir: os.Getenv("DEVBOT_PRIVATE_DIR"),
		Pod: os.Getenv("DEVBOT_POD_NAME"), OTELAttrs: os.Getenv("DEVBOT_OTEL_ATTRS"), Docker: os.Getenv("DEVBOT_DOCKER") == "true",
		Identity: func(provider string) (string, string) {
			id := idents[provider]
			return id[0], id[1]
		},
	}
	if role == forge.RoleReviewer {
		return r.Review(ctx)
	}
	return r.Implement(ctx)
}

func runAgent(ctx context.Context, log *slog.Logger, kind string) error {
	cfg, err := config.Load(os.Getenv("DEVBOT_CONFIG"))
	if err != nil {
		return err
	}
	if err := checkHarnessCLI(cfg, kind); err != nil {
		return err
	}
	mp, err := agent.ReadModelProxy(ctx, envOr("DEVBOT_MODEL_PROXY_STATE", defaultProxyState), 2*time.Minute)
	if err != nil {
		return err
	}
	socket := envOr("DEVBOT_AGENT_SOCKET", "/run/devbot/agent.sock")
	return agent.Serve(ctx, socket, localAgent(cfg, mp), log)
}

const defaultProxyState = "/run/devbot-model/proxy.json"

// localAgent runs every harness devbot knows, through mp. A job pod's proxy
// only serves the harness its role runs, so only that one can work there.
func localAgent(cfg *config.Config, mp *agent.ModelProxy) *agent.Local {
	return &agent.Local{
		Harnesses: map[string]agent.Harness{
			claude.Name: &claude.Harness{Command: cfg.Claude.Command, ExtraArgs: cfg.Claude.ExtraArgs, Proxy: mp},
			codex.Name:  &codex.Harness{Command: cfg.Codex.Command, ExtraArgs: cfg.Codex.ExtraArgs, Proxy: mp},
		},
	}
}

// checkHarnessCLI fails the agent at start, not at its first run, when its
// image lacks the CLI of the harness kind's role runs: the base image has
// none, and each harness has an image of its own.
func checkHarnessCLI(cfg *config.Config, kind string) error {
	a, err := roleAgent(cfg, kind)
	if err != nil {
		return err
	}
	cli := map[string]string{claude.Name: cfg.Claude.Command, codex.Name: cfg.Codex.Command}[a.Harness]
	if _, err := exec.LookPath(cli); err != nil {
		return fmt.Errorf("the %s role runs harness %s, but %q isn't on this container's PATH: run it from the devbot-%s image (the chart's image.agents.%s, docs/operating.md#agent-images)", kind, a.Harness, cli, a.Harness, a.Harness)
	}
	return nil
}

// roleAgent is the agent settings of a job kind.
func roleAgent(cfg *config.Config, kind string) (config.Agent, error) {
	switch kind {
	case queue.KindImplement:
		return cfg.Agents.Implement, nil
	case queue.KindReview:
		return cfg.Agents.Review, nil
	}
	return config.Agent{}, fmt.Errorf("unknown job kind %q", kind)
}

func modelProxy(cfg *config.Config, kind string) (*modelproxy.Proxy, error) {
	a, err := roleAgent(cfg, kind)
	if err != nil {
		return nil, err
	}
	region := ""
	if a.AWS != nil {
		region = a.AWS.Region
	}
	up, err := modelproxy.For(a.Harness, region, os.Getenv)
	if err != nil {
		return nil, err
	}
	return &modelproxy.Proxy{Upstream: up, Token: modelproxy.NewToken()}, nil
}

// runModelProxy is a job pod's model proxy for its role, on the pod's
// loopback, where only the pod's containers reach it.
func runModelProxy(ctx context.Context, log *slog.Logger, kind string) error {
	cfg, err := config.Load(os.Getenv("DEVBOT_CONFIG"))
	if err != nil {
		return err
	}
	p, err := modelProxy(cfg, kind)
	if err != nil {
		return err
	}
	p.Log = log
	addr := envOr("DEVBOT_MODEL_PROXY_ADDR", "127.0.0.1:8462")
	log.Info("model proxy", "addr", addr, "credential", p.Upstream.Credential)
	return modelproxy.Serve(ctx, addr, envOr("DEVBOT_MODEL_PROXY_STATE", defaultProxyState), p)
}

// localModelProxy runs the role's model proxy in this process, for
// single-container mode.
func localModelProxy(ctx context.Context, log *slog.Logger, cfg *config.Config, kind string) (*agent.ModelProxy, error) {
	p, err := modelProxy(cfg, kind)
	if err != nil {
		return nil, err
	}
	p.Log = log
	dir, err := os.MkdirTemp("", "devbot-model-proxy-")
	if err != nil {
		return nil, err
	}
	state := filepath.Join(dir, "proxy.json")
	go func() {
		if err := modelproxy.Serve(ctx, "127.0.0.1:0", state, p); err != nil {
			log.Error("model proxy", "err", err)
		}
	}()
	return agent.ReadModelProxy(ctx, state, 10*time.Second)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---- client commands ----

func cli(ctx context.Context, cmd string, args []string) error {
	base := serverURL()
	token, err := accessToken(ctx)
	if err != nil {
		return err
	}
	call := func(method, path string, in any) error {
		var body io.Reader
		if in != nil {
			b, _ := json.Marshal(in)
			body = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, base+path, body)
		if err != nil {
			return err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("X-Devbot-Client", "cli")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			return errors.New(strings.TrimSpace(string(b)))
		}
		var pretty bytes.Buffer
		if json.Indent(&pretty, b, "", "  ") == nil {
			b = pretty.Bytes()
		}
		fmt.Println(string(b))
		return nil
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	switch cmd {
	case "file":
		var r core.FileRequest
		fs.StringVar(&r.Repo, "repo", "", "provider:owner/name")
		fs.StringVar(&r.Title, "title", "", "title")
		fs.StringVar(&r.Body, "body", "", "body")
		fs.StringVar(&r.AcceptanceCriteria, "ac", "", "acceptance criteria")
		fs.StringVar(&r.Origin, "origin", "", "opaque origin")
		fs.StringVar(&r.RequesterHandle, "requester", "", "forge handle to @mention")
		fs.BoolVar(&r.Automerge, "automerge", false, "merge once the merge gate passes (devbot/automerge)")
		fs.StringVar(&r.Base, "base", "", "branch to work from and open the PR against (default: the repo's base_branch, else its default branch)")
		fs.StringVar(&r.Parent, "parent", "", "parent task to file this one under, as one part of a change across repos")
		fs.Parse(args)
		return call("POST", "/api/tasks", r)
	case "project":
		var r core.ProjectRequest
		fs.StringVar(&r.Repo, "repo", "", "provider:owner/name")
		fs.StringVar(&r.Title, "title", "", "title")
		fs.StringVar(&r.Body, "body", "", "body")
		fs.StringVar(&r.AcceptanceCriteria, "ac", "", "acceptance criteria for the whole change (required with -plan)")
		fs.StringVar(&r.Origin, "origin", "", "opaque origin")
		fs.BoolVar(&r.Plan, "plan", false, "have devbot plan the child tasks, for a person to approve")
		repos := fs.String("repos", "", "with -plan: the repos the change may touch, comma-separated")
		fs.Parse(args)
		for _, s := range strings.Split(*repos, ",") {
			if s = strings.TrimSpace(s); s != "" {
				r.Repos = append(r.Repos, s)
			}
		}
		return call("POST", "/api/projects", r)
	case "list":
		repo := fs.String("repo", "", "")
		state := fs.String("state", "", "")
		origin := fs.String("origin", "", "")
		parent := fs.String("parent", "", "")
		fs.Parse(args)
		q := url.Values{"repo": {*repo}, "state": {*state}, "origin_prefix": {*origin}, "parent": {*parent}}
		return call("GET", "/api/tasks?"+q.Encode(), nil)
	case "events":
		cursor := fs.String("cursor", "", "")
		origin := fs.String("origin", "", "")
		fs.Parse(args)
		q := url.Values{"cursor": {*cursor}, "origin_prefix": {*origin}}
		return call("GET", "/api/events?"+q.Encode(), nil)
	case "edit":
		var r core.EditRequest
		fs.StringVar(&r.Title, "title", "", "new title")
		fs.StringVar(&r.Body, "body", "", "new body (context: what and why)")
		fs.StringVar(&r.AcceptanceCriteria, "ac", "", "new acceptance criteria")
		fs.StringVar(&r.Note, "note", "", "one-line reason, shown in devbot's comment")
		// REF comes first, as in the other task commands, or after the flags.
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			r.Ref, args = args[0], args[1:]
		}
		fs.Parse(args)
		if r.Ref == "" && fs.NArg() > 0 {
			r.Ref = fs.Arg(0)
		}
		if r.Ref == "" {
			usage()
		}
		return call("POST", "/api/task/edit", r)
	}
	if len(args) < 1 {
		usage()
	}
	switch cmd {
	case "status":
		return call("GET", "/api/task?ref="+url.QueryEscape(args[0]), nil)
	case "answer":
		if len(args) < 2 {
			usage()
		}
		return call("POST", "/api/task/answer", map[string]string{"ref": args[0], "text": strings.Join(args[1:], " ")})
	case "cancel", "retry", "rebase":
		return call("POST", "/api/task/"+cmd, map[string]string{"ref": args[0]})
	case "approve":
		if len(args) < 2 {
			usage()
		}
		return call("POST", "/api/task/approve", map[string]string{"ref": args[0], "sha": args[1]})
	case "retarget":
		if len(args) < 2 {
			usage()
		}
		return call("POST", "/api/task/retarget", map[string]string{"ref": args[0], "base": args[1]})
	case "wait":
		if len(args) < 2 {
			usage()
		}
		// The rest is the task and any release gate: REF2 until tagged v*, and
		// after it, ", build against".
		return call("POST", "/api/task/wait", map[string]string{"ref": args[0], "on": strings.Join(args[1:], " ")})
	}
	return nil
}

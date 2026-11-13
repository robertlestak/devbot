package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// A github provider whose App key is missing or isn't a key stops devbot
// at startup, saying which.
func TestGitHubProviderNeedsItsKeys(t *testing.T) {
	p := t.TempDir() + "/config.yaml"
	os.WriteFile(p, []byte(`providers:
- name: github
  type: github
  url: https://api.github.com
  implementer_app_id: 1
  implementer_key_env: TEST_IMPL_KEY
  reviewer_app_id: 2
  reviewer_key_env: TEST_REV_KEY
`), 0o644)
	t.Setenv("DEVBOT_CONFIG", p)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Setenv("TEST_IMPL_KEY", "")
	if _, _, err := engine(context.Background(), log, "serve", ""); err == nil || !strings.Contains(err.Error(), "$TEST_IMPL_KEY is empty") {
		t.Errorf("missing key: %v", err)
	}
	t.Setenv("TEST_IMPL_KEY", "-----BEGIN RSA PRIVATE KEY-----\nbm90IGEga2V5\n-----END RSA PRIVATE KEY-----\n")
	t.Setenv("TEST_REV_KEY", "x")
	if _, _, err := engine(context.Background(), log, "serve", ""); err == nil || !strings.Contains(err.Error(), "provider github: github: the implementer App's (1) private key") {
		t.Errorf("bad key: %v", err)
	}
}

// Every harness config accepts is one the agent can run, and under its name.
func TestLocalAgentRunsEveryHarness(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	l := localAgent(cfg, &agent.ModelProxy{})
	for name := range config.Harnesses {
		if h, ok := l.Harnesses[name]; !ok || h.Name() != name {
			t.Errorf("config accepts harness %q, but the agent doesn't run it", name)
		}
	}
	if len(l.Harnesses) != len(config.Harnesses) {
		t.Errorf("the agent runs harnesses config doesn't accept")
	}
}

// An agent container without its role's CLI, the base image say, stops at
// start and says which image it needs.
func TestAgentNeedsItsHarnessCLI(t *testing.T) {
	bin := t.TempDir()
	os.WriteFile(bin+"/codex", []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", bin)
	p := t.TempDir() + "/config.yaml"
	os.WriteFile(p, []byte("agents:\n  implement: {harness: codex}\n  review: {harness: claude}\n"), 0o644)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkHarnessCLI(cfg, "implement"); err != nil {
		t.Errorf("codex is on the PATH: %v", err)
	}
	err = checkHarnessCLI(cfg, "review")
	if err == nil || !strings.Contains(err.Error(), `the review role runs harness claude, but "claude" isn't on this container's PATH: run it from the devbot-claude image (the chart's image.agents.claude, docs/operating.md#agent-images)`) {
		t.Errorf("no claude: %v", err)
	}
	t.Setenv("DEVBOT_CONFIG", p)
	if err := runAgent(context.Background(), slog.New(slog.DiscardHandler), "review"); err == nil || !strings.Contains(err.Error(), `"claude" isn't on`) {
		t.Errorf("devbot agent review: %v", err)
	}
	if err := checkHarnessCLI(cfg, "deploy"); err == nil {
		t.Error("an unknown kind")
	}
}

// A job pod reads its own bot's credential and nothing else: not the other
// bot's, nor the webhook secret, even with them in its environment.
func TestJobPodTakesItsOwnCredential(t *testing.T) {
	p := t.TempDir() + "/config.yaml"
	os.WriteFile(p, []byte(`providers:
- name: gitea
  type: gitea
  url: https://gitea.example
  implementer_user: devbot
  implementer_token_env: TEST_IMPL_TOKEN
  reviewer_user: devbot-review
  reviewer_token_env: TEST_REV_TOKEN
  webhook_secret_env: TEST_HOOK
`), 0o644)
	t.Setenv("DEVBOT_CONFIG", p)
	t.Setenv("TEST_IMPL_TOKEN", "impl-tok")
	t.Setenv("TEST_REV_TOKEN", "rev-tok")
	t.Setenv("TEST_HOOK", "hook")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	repo := forge.RepoRef{Provider: "gitea", Owner: "acme", Name: "app"}
	for _, own := range []forge.Role{forge.RoleImplementer, forge.RoleReviewer} {
		other := map[forge.Role]forge.Role{forge.RoleImplementer: forge.RoleReviewer, forge.RoleReviewer: forge.RoleImplementer}[own]
		e, _, err := engine(ctx, log, "harness", own)
		if err != nil {
			t.Fatal(err)
		}
		pr := e.Forges["gitea"]
		if _, h, err := pr.GitAuth(ctx, repo, own); err != nil || !strings.Contains(h, map[forge.Role]string{forge.RoleImplementer: "impl-tok", forge.RoleReviewer: "rev-tok"}[own]) {
			t.Errorf("%s pod, as itself: %q, %v", own, h, err)
		}
		if _, _, err := pr.GitAuth(ctx, repo, other); !errors.Is(err, forge.ErrNoCredential) {
			t.Errorf("%s pod, as the %s: %v", own, other, err)
		}
		req := httptest.NewRequest("POST", "/webhooks/gitea", strings.NewReader("{}"))
		if _, err := pr.ParseWebhook(req, []byte("{}")); err == nil {
			t.Errorf("%s pod took a webhook", own)
		}
	}
	t.Setenv("TEST_REV_TOKEN", "")
	if _, _, err := engine(ctx, log, "harness", forge.RoleReviewer); err == nil || !strings.Contains(err.Error(), "$TEST_REV_TOKEN is empty") {
		t.Errorf("review pod without its token: %v", err)
	}
	// Two env vars holding one token are one user acting as both bots.
	t.Setenv("TEST_REV_TOKEN", "impl-tok")
	if _, _, err := engine(ctx, log, "serve", ""); err == nil || !strings.Contains(err.Error(), "hold the same token") {
		t.Errorf("serve with one token for both bots: %v", err)
	}
}

// Every harness config accepts has a model proxy, on its vendor's API and
// on Bedrock, each with the credential the chart gives it.
func TestModelProxyForEveryHarness(t *testing.T) {
	for _, k := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "CODEX_API_KEY", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		t.Setenv(k, "x")
	}
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for name := range config.Harnesses {
		for _, aws := range []*config.AWS{nil, {Region: "us-east-1", Auth: "access_keys"}} {
			cfg.Agents.Review = config.Agent{Harness: name, AWS: aws}
			p, err := modelProxy(cfg, "review")
			if err != nil {
				t.Errorf("%s (aws %v): %v", name, aws, err)
				continue
			}
			if want := aws != nil; (p.Upstream.Credential == agent.CredAWS) != want {
				t.Errorf("%s (aws %v): a %s proxy", name, aws, p.Upstream.Credential)
			}
		}
	}
	if _, err := modelProxy(cfg, "nope"); err == nil {
		t.Error("a proxy for no role")
	}
}

// Redis secrets come from the environment, and one that's named but
// missing or unusable stops devbot at startup instead of failing later.
func TestRedisStoreNeedsItsSecrets(t *testing.T) {
	var r config.Redis
	r.Addr = "redis.example.com:6380"
	r.UsernameEnv, r.PasswordEnv = "TEST_REDIS_USERNAME", "TEST_REDIS_PASSWORD"
	t.Setenv("TEST_REDIS_USERNAME", "devbot-serve")
	t.Setenv("TEST_REDIS_PASSWORD", "")
	if _, err := redisStore(r); err == nil || !strings.Contains(err.Error(), "$TEST_REDIS_PASSWORD is empty") {
		t.Errorf("missing password: %v", err)
	}
	t.Setenv("TEST_REDIS_PASSWORD", "hunter2")
	r.TLS.Enabled, r.TLS.CAEnv = true, "TEST_REDIS_CA"
	t.Setenv("TEST_REDIS_CA", "not a certificate")
	if _, err := redisStore(r); err == nil || !strings.Contains(err.Error(), "$TEST_REDIS_CA holds no PEM certificate") {
		t.Errorf("bad CA: %v", err)
	}
	r.TLS.CAEnv = ""
	q, err := redisStore(r)
	if err != nil {
		t.Fatal(err)
	}
	o := q.R.Options()
	if o.Username != "devbot-serve" || o.Password != "hunter2" || o.TLSConfig == nil || o.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("options: user %q, password %q, TLS %+v", o.Username, o.Password, o.TLSConfig)
	}
}

// devbot edit sends REF and the parts to replace, with REF before or after
// the flags.
func TestCLIEdit(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(b))
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	t.Setenv("DEVBOT_URL", srv.URL)
	t.Setenv("DEVBOT_TOKEN", "t")
	for _, args := range [][]string{
		{"gitea:acme/app#3", "-ac", "- y", "-note", "why"},
		{"-ac", "- y", "-note", "why", "gitea:acme/app#3"},
	} {
		if err := cli(context.Background(), "edit", args); err != nil {
			t.Fatal(err)
		}
	}
	want := `POST /api/task/edit {"ref":"gitea:acme/app#3","acceptance_criteria":"- y","note":"why"}`
	if len(got) != 2 || got[0] != want || got[1] != want {
		t.Fatalf("got %q, want %q twice", got, want)
	}
}

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"git.shdw.tech/rob/devbot/internal/changeset"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/fake"
)

func TestGlob(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{".devbot.yaml", ".devbot.yaml", true},
		{".gitea/**", ".gitea/workflows/pr.yaml", true},
		{".gitea/**", "x/.gitea/a", false},
		{"deploy/**", "deploy", true},
		{"**/*.sql", "db/migrations/001.sql", true},
		{"*.sql", "db/migrations/001.sql", true},
		{"hack/asc*/**", "hack/ascapi/main.go", true},
		{"hack/asc*/**", "hack/e2e/run.sh", false},
		{"app/app.json", "app/app.json", true},
		{"app/app.json", "app/src/app.json", false},
		{"store/**", "internal/store/x.go", false},
	}
	for _, c := range cases {
		if got := Glob(c.pat, c.name); got != c.want {
			t.Errorf("Glob(%q, %q) = %v, want %v", c.pat, c.name, got, c.want)
		}
	}
}

func TestParseRepoDefaults(t *testing.T) {
	r, err := ParseRepo([]byte("version: 1\nenabled: true\nprotected_paths: [deploy/**]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !*r.RequireChecks || r.MaxAttempts != 3 || r.MaxReviewRounds != 3 || *r.CheckRetries != 2 || r.MergeMethod != "squash" || r.Rebase != RebaseLazy {
		t.Fatalf("defaults not applied: %+v", r)
	}
	if !*r.BranchCleanup || r.BranchCleanupGrace != time.Hour || r.OrphanBranchGrace != 24*time.Hour {
		t.Fatalf("branch cleanup defaults not applied: %+v", r)
	}
	if got := r.Protected([]string{"main.go", ".devbot.yaml", "deploy/helm/x.yaml"}); len(got) != 2 {
		t.Fatalf(".devbot.yaml must always be protected, got %v", got)
	}
	// The files the agent CLIs load as instructions, settings or MCP config.
	agentConfig := []string{".devbot/result.json", ".claude/settings.json", ".codex/config.toml", ".mcp.json", "CLAUDE.md", "CLAUDE.local.md", "AGENTS.md",
		"internal/AGENTS.md", "tools/.claude/settings.json", "a/b/.codex/config.toml", "sub/.devbot/result.json", "sub/.mcp.json", "sub/CLAUDE.local.md"}
	if got := r.Protected(agentConfig); len(got) != len(agentConfig) {
		t.Fatalf("agent config must always be protected, got %v", got)
	}
	d, err := DefaultRepo(map[string]any{"protected_paths": []string{"deploy/**"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Protected(agentConfig); len(got) != len(agentConfig) {
		t.Fatalf("agent config must be protected under the install's defaults too, got %v", got)
	}
	if a, _ := r.Agent("review", Agents{Implement: Agent{Harness: "claude", Model: "a"}, Review: Agent{Harness: "claude", Model: "b"}}); a.Model != "b" || a.Harness != "claude" {
		t.Fatalf("review agent = %+v", a)
	}
}

func TestLoadRenderedConfig(t *testing.T) {
	p := t.TempDir() + "/config.yaml"
	os.WriteFile(p, []byte("reconcile_interval: 2m\nbackoff:\n  cap: 30m\nproviders:\n- name: gitea\n  type: gitea\n  url: https://x\n"), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.ReconcileInterval != 2*time.Minute || c.Backoff.Cap != 30*time.Minute || c.Backoff.Base != time.Minute || !reflect.DeepEqual(c.Agents.Review, Agent{Harness: "claude", Model: "claude-opus-5-5"}) {
		t.Fatalf("got %+v", c)
	}
}

func TestEnabledDefaultsOn(t *testing.T) {
	r, _ := ParseRepo([]byte("version: 1\nchecks: [make test]\n"))
	if !r.IsEnabled() {
		t.Fatal("a .devbot.yaml without enabled should be on")
	}
	r, _ = ParseRepo([]byte("enabled: false\n"))
	if r.IsEnabled() {
		t.Fatal("enabled: false must opt out")
	}
}

func TestMinPermission(t *testing.T) {
	p := t.TempDir() + "/config.yaml"
	c, err := Load("")
	if err != nil || c.Authorization.Min != forge.PermWrite {
		t.Fatalf("default should be write: %v, %v", c.Authorization.Min, err)
	}
	os.WriteFile(p, []byte("authorization:\n  min_permission: admin\n"), 0o644)
	if c, err := Load(p); err != nil || c.Authorization.Min != forge.PermAdmin {
		t.Fatalf("admin: %v, %v", c.Authorization.Min, err)
	}
	for _, bad := range []string{"read", "none", "root"} {
		os.WriteFile(p, []byte("authorization:\n  min_permission: "+bad+"\n"), 0o644)
		if _, err := Load(p); err == nil {
			t.Errorf("min_permission %s should be rejected", bad)
		}
	}
}

func TestTrustedProxies(t *testing.T) {
	if c, err := Load(""); err != nil || len(c.Proxies) != 0 {
		t.Fatalf("by default no proxy should be trusted: %v, %v", c.Proxies, err)
	}
	p := t.TempDir() + "/config.yaml"
	os.WriteFile(p, []byte("trusted_proxies: [127.0.0.0/8, 10.1.2.3, \"fd00::/8\"]\n"), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(c.Proxies) != "[127.0.0.0/8 10.1.2.3/32 fd00::/8]" {
		t.Fatalf("parsed: %v", c.Proxies)
	}
	os.WriteFile(p, []byte("trusted_proxies: [istio-gateway]\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Error("a name instead of an address should be rejected")
	}
}

func TestAuthMode(t *testing.T) {
	c, err := Load("")
	if err != nil || c.Auth.Mode != "none" || c.Auth.OAuth() {
		t.Fatalf("default should be none: %+v, %v", c.Auth, err)
	}
	p := t.TempDir() + "/config.yaml"
	base := "providers:\n- {name: gitea, type: gitea, url: https://x}\nauth:\n  mode: oauth\n"
	good := base + "  resource_url: https://devbot.example.com/\n  client_id_env: ID\n  client_secret_env: SECRET\n  key_env: KEY\n"
	os.WriteFile(p, []byte(good), 0o644)
	c, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Auth.OAuth() || c.Auth.Provider != "gitea" || c.Auth.ResourceURL != "https://devbot.example.com" || c.Auth.AccessTokenTTL != time.Hour || c.Auth.ReuseGrace() != time.Minute ||
		c.Auth.DynamicDeviceGrant || c.Auth.RateLimits.Register.PerMinute == 0 || c.Auth.RateLimits.ClientsPerAddress == 0 {
		t.Fatalf("oauth defaults: %+v", c.Auth)
	}
	for name, bad := range map[string]string{
		"no resource url":      base + "  client_id_env: ID\n  client_secret_env: SECRET\n  key_env: KEY\n",
		"plain http":           strings.Replace(good, "https://devbot", "http://devbot", 1),
		"no key":               strings.Replace(good, "  key_env: KEY\n", "", 1),
		"unknown provider":     good + "  provider: github\n",
		"unknown mode":         strings.Replace(good, "mode: oauth", "mode: tokens", 1),
		"access ttl too short": good + "  access_token_ttl: 1s\n",
		"negative reuse grace": good + "  refresh_reuse_grace: -1s\n",
		"reuse grace too long": good + "  refresh_reuse_grace: 2h\n",
	} {
		os.WriteFile(p, []byte(bad), 0o644)
		if _, err := Load(p); err == nil {
			t.Errorf("%s: should be rejected", name)
		}
	}
	// 0 is strict reuse detection, not the default.
	os.WriteFile(p, []byte(good+"  refresh_reuse_grace: 0s\n"), 0o644)
	if c, err = Load(p); err != nil || c.Auth.ReuseGrace() != 0 {
		t.Fatalf("refresh_reuse_grace: 0: %v %v", err, c.Auth.RefreshReuseGrace)
	}
}

func TestProviderIdentityKeys(t *testing.T) {
	p := t.TempDir() + "/config.yaml"
	gh := "providers:\n- name: github\n  type: github\n  url: https://api.github.com\n  implementer_app_id: 1\n  implementer_key_env: IMPL_KEY\n  reviewer_app_id: 2\n  reviewer_key_env: REV_KEY\n"
	os.WriteFile(p, []byte(gh), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if g := c.Providers[0]; g.ImplementerAppID != 1 || g.ReviewerKeyEnv != "REV_KEY" || *g.LowQuotaPercent != 10 {
		t.Fatalf("parsed: %+v", g)
	}
	for name, bad := range map[string]string{
		"github with a token":       gh + "  implementer_token_env: TOK\n",
		"github with a user":        gh + "  reviewer_user: devbot\n",
		"github without a key":      strings.Replace(gh, "  reviewer_key_env: REV_KEY\n", "", 1),
		"github without an app id":  strings.Replace(gh, "  implementer_app_id: 1\n", "", 1),
		"github with one app twice": strings.Replace(gh, "reviewer_app_id: 2", "reviewer_app_id: 1", 1),
		"gitea with an app":         "providers:\n- {name: gitea, type: gitea, url: https://x, implementer_app_id: 1}\n",
		"gitea with one user twice": "providers:\n- {name: gitea, type: gitea, url: https://x, implementer_user: devbot, reviewer_user: DevBot}\n",
		"gitea with one token env":  "providers:\n- {name: gitea, type: gitea, url: https://x, implementer_token_env: TOK, reviewer_token_env: TOK}\n",
		"unknown type":              "providers:\n- {name: gl, type: gitlab, url: https://x}\n",
		"github paced at 100%":      gh + "  low_quota_percent: 100\n",
		"gitea paced":               "providers:\n- {name: gitea, type: gitea, url: https://x, low_quota_percent: 5}\n",
	} {
		os.WriteFile(p, []byte(bad), 0o644)
		if _, err := Load(p); err == nil {
			t.Errorf("%s: should be rejected", name)
		}
	}
	os.WriteFile(p, []byte(gh+"  low_quota_percent: 0\n"), 0o644)
	if c, err := Load(p); err != nil || *c.Providers[0].LowQuotaPercent != 0 {
		t.Fatalf("low_quota_percent 0 turns pacing off: %v", err)
	}
}

// The chart's example installs must be configs devbot accepts.
func TestExampleValuesLoad(t *testing.T) {
	files, _ := filepath.Glob("../../deploy/helm/devbot/values.example-*.yaml")
	if len(files) == 0 {
		t.Fatal("no example values")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var v struct {
			Config yaml.Node `yaml:"config"`
		}
		if err := yaml.Unmarshal(b, &v); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out, _ := yaml.Marshal(&v.Config)
		p := t.TempDir() + "/config.yaml"
		os.WriteFile(p, out, 0o644)
		if _, err := Load(p); err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
		}
	}
}

func TestAgents(t *testing.T) {
	p := t.TempDir() + "/config.yaml"
	os.WriteFile(p, []byte("agents:\n  implement: {model: claude-sonnet-5-5}\n"), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Agents.Implement, Agent{Harness: "claude", Model: "claude-sonnet-5-5"}) || !reflect.DeepEqual(c.Agents.Review, Agent{Harness: "claude", Model: "claude-sonnet-5-5"}) {
		t.Fatalf("got %+v", c.Agents)
	}
	r, _ := ParseRepo([]byte("agents:\n  review: {model: claude-haiku-5-5}\n"))
	if a, _ := r.Agent("review", c.Agents); !reflect.DeepEqual(a, Agent{Harness: "claude", Model: "claude-haiku-5-5"}) {
		t.Fatalf("the repo's model override: %+v", a)
	}
	if a, _ := r.Agent("implement", c.Agents); !reflect.DeepEqual(a, c.Agents.Implement) {
		t.Fatalf("no override: %+v", a)
	}

	// A role on another harness gets that harness's default model.
	os.WriteFile(p, []byte("agents:\n  implement: {harness: codex}\n"), 0o644)
	if c, err = Load(p); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Agents.Implement, Agent{Harness: "codex", Model: "gpt-6.1-sol"}) || !reflect.DeepEqual(c.Agents.Review, Agent{Harness: "claude", Model: "claude-opus-5-5"}) || c.Codex.Command != "codex" {
		t.Fatalf("got %+v", c.Agents)
	}

	os.WriteFile(p, []byte("agents:\n  review: {harness: nope}\n"), 0o644)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "agents.review.harness") {
		t.Fatalf("an unknown harness must fail at startup, got %v", err)
	}
}

func TestAgentsOnBedrock(t *testing.T) {
	p := t.TempDir() + "/config.yaml"
	os.WriteFile(p, []byte(`agents:
  implement: {harness: codex, model: openai.gpt-6.1-sol, aws: {region: us-east-1, auth: irsa}}
  review: {harness: codex}
`), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if a := c.Agents.Implement; a.AWS == nil || *a.AWS != (AWS{Region: "us-east-1", Auth: "irsa"}) {
		t.Fatalf("implement %+v", a)
	}
	// Off Bedrock, the reviewer doesn't inherit a Bedrock model ID.
	if a := c.Agents.Review; a.AWS != nil || a.Model != "gpt-6.1-sol" {
		t.Fatalf("review %+v", a)
	}
	r, _ := ParseRepo([]byte("agents:\n  implement: {model: openai.gpt-5.6-luna}\n"))
	if a, _ := r.Agent("implement", c.Agents); a.Model != "openai.gpt-5.6-luna" || a.AWS != c.Agents.Implement.AWS {
		t.Fatalf("a repo changes only the model: %+v", a)
	}

	// Both on Bedrock with one harness: the reviewer takes the implementer's model.
	os.WriteFile(p, []byte(`agents:
  implement: {model: us.anthropic.claude-opus-5-5-v1:0, aws: {region: us-east-1, auth: access_keys}}
  review: {aws: {region: us-east-1, auth: access_keys}}
`), 0o644)
	if c, err = Load(p); err != nil || c.Agents.Review.Model != "us.anthropic.claude-opus-5-5-v1:0" {
		t.Fatalf("review %+v, %v", c.Agents.Review, err)
	}

	for _, bad := range []struct{ yaml, want string }{
		{"agents:\n  implement: {aws: {region: us-east-1, auth: irsa}}\n", "agents.implement.aws"},
		{"agents:\n  review: {model: m, aws: {auth: irsa}}\n", "agents.review.aws"},
		{"agents:\n  review: {model: m, aws: {region: us-east-1, auth: api_key}}\n", "agents.review.aws.auth"},
		{"agents:\n  review: {model: m, aws: {region: us-east-1}}\n", "agents.review.aws.auth"},
	} {
		os.WriteFile(p, []byte(bad.yaml), 0o644)
		if _, err := Load(p); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%q: want an error naming %s, got %v", bad.yaml, bad.want, err)
		}
	}
}

func TestAnonymousPrincipalIsAName(t *testing.T) {
	p := t.TempDir() + "/config.yaml"
	os.WriteFile(p, []byte("anonymous_principal: chat-relay.v2\n"), 0o644)
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`"a b"`, `"<!--"`, "a/b"} {
		os.WriteFile(p, []byte("anonymous_principal: "+bad+"\n"), 0o644)
		if _, err := Load(p); err == nil {
			t.Errorf("anonymous_principal %s should be rejected", bad)
		}
	}
}

// human_checks match check names with the glob syntax, from a file or from
// repos.defaults, and required_reviewers and trusted_reviewers are read from
// either.
func TestMergeGateKeys(t *testing.T) {
	file, err := ParseRepo([]byte("version: 1\nrequired_reviewers: [secbot]\nhuman_checks: [cla/*, sign-off]\ntrusted_reviewers: [LintBot]\n"))
	if err != nil {
		t.Fatal(err)
	}
	defs, err := DefaultRepo(map[string]any{"required_reviewers": []any{"secbot"}, "human_checks": []any{"cla/*", "sign-off"}, "trusted_reviewers": []any{"LintBot"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []*Repo{file, defs} {
		if !slices.Equal(r.RequiredReviewers, []string{"secbot"}) {
			t.Errorf("%s: required_reviewers %v", r.Source, r.RequiredReviewers)
		}
		if !slices.Equal(r.TrustedReviewers, []string{"LintBot"}) {
			t.Errorf("%s: trusted_reviewers %v", r.Source, r.TrustedReviewers)
		}
		for name, want := range map[string]bool{"cla/check": true, "sign-off": true, "ci/sign-off": true, "ci/test": false, "cla": false} {
			if r.HumanCheck(name) != want {
				t.Errorf("%s: HumanCheck(%q) = %v, want %v", r.Source, name, !want, want)
			}
		}
	}
}

func TestProtectedPathsDefault(t *testing.T) {
	for _, c := range []struct {
		yaml string
		want int
	}{
		{"version: 1\n", len(DefaultProtectedPaths) + len(AlwaysProtected)},
		{"version: 1\nprotected_paths: []\n", len(AlwaysProtected)},
		{"version: 1\nprotected_paths: [deploy/**]\n", 1 + len(AlwaysProtected)},
	} {
		r, err := ParseRepo([]byte(c.yaml))
		if err != nil {
			t.Fatal(err)
		}
		if len(r.ProtectedPaths) != c.want || !slices.Equal(r.ProtectedPaths[c.want-len(AlwaysProtected):], AlwaysProtected) {
			t.Errorf("%q: got %v", c.yaml, r.ProtectedPaths)
		}
	}
}

// repos.limits caps what a .devbot.yaml sets, and the install's own
// repos.defaults has to stay within it.
func TestRepoLimits(t *testing.T) {
	repo := forge.RepoRef{Provider: "fake", Owner: "acme", Name: "app"}
	p := t.TempDir() + "/config.yaml"
	os.WriteFile(p, []byte(`
repos:
  limits:
    max_attempts: 4
    check_retries: 0
    timeouts: {implement: 30m}
    models: [claude-sonnet-5-5]
`), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New()
	f.AddRepo(repo, `
max_attempts: 50
check_retries: 9
max_review_rounds: 500
max_rebase_rounds: 2
timeouts: {implement: 10h, review: 5m}
agents:
  implement: {model: claude-mythos-9}
  review: {model: claude-sonnet-5-5}
`)
	rc, err := (&RepoCache{Forges: forge.Registry{"fake": f}, Policy: c.Repos}).Get(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if rc.MaxAttempts != 4 || *rc.CheckRetries != 0 || rc.MaxReviewRounds != 10 || rc.Timeouts.Implement != 30*time.Minute {
		t.Fatalf("not capped: %+v", rc)
	}
	if rc.MaxRebaseRounds != 2 || rc.Timeouts.Review != 5*time.Minute {
		t.Fatalf("values under the ceiling stand: %+v", rc)
	}
	if a, _ := rc.Agent("implement", c.Agents); a.Model != "claude-opus-5-5" {
		t.Fatalf("a model the install doesn't allow falls back to the install's: %s", a.Model)
	}
	if a, _ := rc.Agent("review", c.Agents); a.Model != "claude-sonnet-5-5" {
		t.Fatalf("an allowed model stands: %s", a.Model)
	}

	// The install's own model is always allowed.
	if !slices.Contains(c.Repos.Limits.Models, "claude-opus-5-5") {
		t.Fatalf("models: %v", c.Repos.Limits.Models)
	}

	// repos.defaults is capped the same way, for a repo without the file.
	os.WriteFile(p, []byte("repos:\n  limits: {max_attempts: 4}\n  defaults: {max_attempts: 9}\n"), 0o644)
	if c, err = Load(p); err != nil {
		t.Fatal(err)
	}
	bare := forge.RepoRef{Provider: "fake", Owner: "acme", Name: "bare"}
	f.AddRepo(bare, "")
	if rc, err := (&RepoCache{Forges: forge.Registry{"fake": f}, Policy: c.Repos}).Get(t.Context(), bare); err != nil || rc.Source != "defaults" || rc.MaxAttempts != 4 {
		t.Fatalf("defaults: %+v %v", rc, err)
	}

	os.WriteFile(p, []byte("repos:\n  limits: {max_attempts: -1}\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Error("a negative ceiling should be rejected")
	}
}

func TestRepoLabels(t *testing.T) {
	r, err := ParseRepo([]byte("version: 1\nlabels: {team: payments, cost_center: 1234}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Labels["team"] != "payments" || r.Labels["cost_center"] != "1234" {
		t.Fatalf("labels %v", r.Labels)
	}
	for _, bad := range []string{
		"labels: {Team: payments}",
		"labels: {team-name: payments}",
		"labels: {team: pay ments}",
		"labels: {team: -payments}",
		"labels: {team: " + strings.Repeat("a", 64) + "}",
	} {
		if _, err := ParseRepo([]byte("version: 1\n" + bad + "\n")); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
	// repos.defaults is .devbot.yaml's keys, so it's held to the same rules.
	d, err := DefaultRepo(map[string]any{"labels": map[string]any{"cost_center": 1234}})
	if err != nil || d.Labels["cost_center"] != "1234" {
		t.Fatalf("defaults labels %v, %v", d, err)
	}
	if _, err := DefaultRepo(map[string]any{"labels": map[string]any{"team": "a/b"}}); err == nil {
		t.Error("repos.defaults.labels with an invalid value: want an error")
	}
}

func TestMetricsAndPodLabelsConfig(t *testing.T) {
	load := func(y string) (*Config, error) {
		p := filepath.Join(t.TempDir(), "config.yaml")
		os.WriteFile(p, []byte(y), 0o644)
		return Load(p)
	}
	c, err := load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Metrics.AttributionLabels(); strings.Join(got, ",") != "repo,owner" {
		t.Fatalf("default attribution labels %v", got)
	}
	c, err = load("metrics: {cost_labels: [], static_labels: [team]}\n")
	if err != nil || strings.Join(c.Metrics.AttributionLabels(), ",") != "label_team" {
		t.Fatalf("no cost labels, one static: %v, %v", c, err)
	}
	c, err = load("metrics: {cost_labels: [requester, repo]}\n")
	if err != nil || strings.Join(c.Metrics.AttributionLabels(), ",") != "requester,repo" {
		t.Fatalf("configured cost labels: %v, %v", c, err)
	}
	for _, bad := range []string{
		"metrics: {cost_labels: [task]}",
		"metrics: {cost_labels: [repo, repo]}",
		"metrics: {static_labels: [Team]}",
		"pod_labels: {enabled: true}",
	} {
		if _, err := load(bad + "\n"); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
	if _, err := load("pod_labels: {enabled: true, instance: devbot}\n"); err != nil {
		t.Error(err)
	}
}

func TestCIFailuresConfig(t *testing.T) {
	load := func(y string) (*Config, error) {
		p := filepath.Join(t.TempDir(), "config.yaml")
		os.WriteFile(p, []byte(y), 0o644)
		return Load(p)
	}
	c, err := load("")
	if err != nil || c.CIFailures.LogLines != 150 || c.CIFailures.MaxBytes != 64<<10 {
		t.Fatalf("defaults: %+v, %v", c.CIFailures, err)
	}
	c, err = load("ci_failures: {log_lines: 40, max_bytes: 8192}\n")
	if err != nil || c.CIFailures.LogLines != 40 || c.CIFailures.MaxBytes != 8192 {
		t.Fatalf("set: %+v, %v", c.CIFailures, err)
	}
	for _, bad := range []string{"ci_failures: {log_lines: -1}", "ci_failures: {max_bytes: -1}"} {
		if _, err := load(bad + "\n"); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}

func TestParseRepoRebase(t *testing.T) {
	r, err := ParseRepo([]byte("version: 1\nrebase: eager\n"))
	if err != nil || r.Rebase != RebaseEager {
		t.Fatalf("got %+v, %v", r, err)
	}
	if _, err := ParseRepo([]byte("version: 1\nrebase: sometimes\n")); err == nil {
		t.Fatal("an unknown rebase value should be refused")
	}
	if _, err := DefaultRepo(map[string]any{"rebase": "never"}); err == nil {
		t.Fatal("an unknown rebase value in the install's defaults should be refused")
	}
}

// base_branch takes a plain branch name, the rule file_task's base has, from
// a file and from the install's defaults alike.
func TestParseRepoBaseBranch(t *testing.T) {
	r, err := ParseRepo([]byte("version: 1\nbase_branch: release/2.4\n"))
	if err != nil || r.BaseBranch != "release/2.4" {
		t.Fatalf("got %+v, %v", r, err)
	}
	if r, err := ParseRepo([]byte("version: 1\n")); err != nil || r.BaseBranch != "" {
		t.Fatalf("unset: got %q, %v; want empty, for the default branch", r.BaseBranch, err)
	}
	for _, b := range []string{"-x", "dev;rm -rf /", "$(id)", ".hidden", "a b"} {
		if _, err := ParseRepo([]byte(fmt.Sprintf("version: 1\nbase_branch: %q\n", b))); err == nil || !strings.Contains(err.Error(), "base_branch") {
			t.Errorf("base_branch %q gave %v, want an error naming it", b, err)
		}
	}
	if d, err := DefaultRepo(map[string]any{"base_branch": "develop"}); err != nil || d.BaseBranch != "develop" {
		t.Fatalf("repos.defaults: got %+v, %v", d, err)
	}
	if _, err := DefaultRepo(map[string]any{"base_branch": "dev`id`"}); err == nil {
		t.Fatal("a bad base_branch in the install's defaults should be refused")
	}
}

// 0 means the default; a negative count or timeout is refused rather than read as one.
func TestParseRepoCounts(t *testing.T) {
	r, err := ParseRepo([]byte("max_attempts: 0\nmax_review_rounds: 0\nmax_rebase_rounds: 0\nreview_budget_kib: 0\n"))
	if err != nil || r.MaxAttempts != 3 || r.MaxReviewRounds != 3 || r.MaxRebaseRounds != 10 || r.ReviewBudgetKiB != 512 {
		t.Fatalf("got %+v, %v", r, err)
	}
	for _, k := range []string{"max_attempts", "max_review_rounds", "max_rebase_rounds", "check_retries", "review_budget_kib"} {
		if _, err := ParseRepo([]byte(k + ": -1\n")); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s: -1 gave %v, want an error naming it", k, err)
		}
	}
	if _, err := ParseRepo([]byte("checks_pending_timeout: -1h\n")); err == nil || !strings.Contains(err.Error(), "checks_pending_timeout") {
		t.Errorf("checks_pending_timeout: -1h gave %v, want an error naming it", err)
	}
}

func TestBranchCleanupKeys(t *testing.T) {
	r, err := ParseRepo([]byte("branch_cleanup: false\nbranch_cleanup_grace: 30m\norphan_branch_grace: 72h\n"))
	if err != nil {
		t.Fatal(err)
	}
	if *r.BranchCleanup || r.BranchCleanupGrace != 30*time.Minute || r.OrphanBranchGrace != 72*time.Hour {
		t.Fatalf("got %v %v %v", *r.BranchCleanup, r.BranchCleanupGrace, r.OrphanBranchGrace)
	}
	d, err := DefaultRepo(map[string]any{"branch_cleanup": false})
	if err != nil {
		t.Fatal(err)
	}
	if *d.BranchCleanup {
		t.Fatal("repos.defaults branch_cleanup: false not applied")
	}
}

const mcpCatalog = `mcp_servers:
  context7: {type: http, url: "https://mcp.example.com/context7/mcp", disabled_tools: [resolve-library-id]}
  searxng: {type: http, url: "https://mcp.example.com/searxng/mcp"}
  playwright: {type: sse, url: "http://10.0.0.5:8931/sse", disabled_tools: [browser_install]}
agents:
  implement: {mcp_servers: [context7, searxng, playwright]}
  review: {mcp_servers: [context7]}
`

func TestMCPServers(t *testing.T) {
	p := t.TempDir() + "/config.yaml"
	os.WriteFile(p, []byte(mcpCatalog), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if s := c.MCPServers["playwright"]; s.Type != "sse" || !reflect.DeepEqual(s.DisabledTools, []string{"browser_install"}) {
		t.Fatalf("playwright %+v", s)
	}
	if !reflect.DeepEqual(c.Agents.Review.MCPServers, []string{"context7"}) {
		t.Fatalf("review %+v", c.Agents.Review)
	}

	// Codex enforces an allow list; Claude Code, with permission prompts
	// off, wouldn't (below).
	os.WriteFile(p, []byte("mcp_servers:\n  playwright: {type: http, url: \"https://x/mcp\", allowed_tools: [browser_navigate, browser_snapshot]}\nagents:\n  review: {harness: codex, mcp_servers: [playwright]}\n"), 0o644)
	if c, err := Load(p); err != nil || !reflect.DeepEqual(c.MCPServers["playwright"].AllowedTools, []string{"browser_navigate", "browser_snapshot"}) {
		t.Fatalf("codex allowed_tools: %+v, %v", c, err)
	}

	// Without the role key, a role gets none, and a repo's own list is
	// checked against what the install gives that role.
	os.WriteFile(p, []byte("mcp_servers:\n  context7: {type: http, url: \"https://x/mcp\"}\n"), 0o644)
	if c, err := Load(p); err != nil || c.Agents.Implement.MCPServers != nil || c.Agents.Review.MCPServers != nil {
		t.Fatalf("default: %+v, %v", c.Agents, err)
	}

	for _, bad := range []struct{ yaml, want string }{
		{"mcp_servers:\n  x: {type: stdio, url: \"https://x\"}\n", "stdio servers aren't supported"},
		{"mcp_servers:\n  x: {type: websocket, url: \"https://x\"}\n", "mcp_servers.x.type"},
		{"mcp_servers:\n  x: {url: \"https://x\"}\n", "mcp_servers.x.type"},
		{"mcp_servers:\n  x: {type: http, url: \"ftp://x\"}\n", "mcp_servers.x.url"},
		{"mcp_servers:\n  x: {type: http}\n", "mcp_servers.x.url"},
		{"mcp_servers:\n  x_y: {type: http, url: \"https://x\"}\n", "letters, digits and dashes"},
		{"mcp_servers:\n  x: {type: http, url: \"https://x\", allowed_tools: [\"a b\"]}\n", "tool"},
		{"mcp_servers:\n  x: {type: http, url: \"https://x\", headers: {Authorization: MCP_TOKEN}}\n", "mcp_servers.x.headers"},
		{"agents:\n  review: {mcp_servers: [nope]}\n", `agents.review.mcp_servers: "nope" isn't in mcp_servers`},
		{"mcp_servers:\n  x: {type: http, url: \"https://x\"}\nagents:\n  review: {mcp_servers: [x, x]}\n", "listed twice"},
		{"mcp_servers:\n  x: {type: sse, url: \"https://x\"}\nagents:\n  implement: {harness: codex, mcp_servers: [x]}\n", "codex speaks only streamable http"},
		{"mcp_servers:\n  x: {type: http, url: \"https://x\", allowed_tools: [a]}\nagents:\n  review: {mcp_servers: [x]}\n", "list the tools to keep away in disabled_tools"},
		{mcpCatalog + "repos:\n  defaults: {agents: {review: {mcp_servers: [searxng]}}}\n", "repos.defaults: agents.review.mcp_servers"},
	} {
		os.WriteFile(p, []byte(bad.yaml), 0o644)
		if _, err := Load(p); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%q: want an error naming %s, got %v", bad.yaml, bad.want, err)
		}
	}
}

// A repo picks a subset of the install's servers for a role, or none, and
// can't name another.
func TestRepoMCPServers(t *testing.T) {
	p := t.TempDir() + "/config.yaml"
	os.WriteFile(p, []byte(mcpCatalog), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, yaml, role string
		want             []string
		err              string
	}{
		{"unset keeps the install's", "version: 1\n", "implement", []string{"context7", "searxng", "playwright"}, ""},
		{"subset, in the install's order", "agents:\n  implement: {mcp_servers: [playwright, context7, context7]}\n", "implement", []string{"context7", "playwright"}, ""},
		{"empty turns them off", "agents:\n  implement: {mcp_servers: []}\n", "implement", nil, ""},
		{"another role's server", "agents:\n  review: {mcp_servers: [playwright]}\n", "review", nil,
			`agents.review.mcp_servers: "playwright" isn't one of the MCP servers this install gives the review role (context7)`},
		{"outside the catalog", "agents:\n  implement: {mcp_servers: [github]}\n", "implement", nil, `"github" isn't one of the MCP servers`},
		{"a definition is no name", "agents:\n  implement: {mcp_servers: [{name: evil, url: \"https://evil\"}]}\n", "implement", nil, "cannot unmarshal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := ParseRepo([]byte(tc.yaml))
			if err == nil {
				var a Agent
				a, err = r.Agent(tc.role, c.Agents)
				if err == nil && !reflect.DeepEqual(a.MCPServers, tc.want) {
					t.Fatalf("got %q, want %q", a.MCPServers, tc.want)
				}
			}
			if (err != nil) != (tc.err != "") || (err != nil && !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("err %v, want %q", err, tc.err)
			}
		})
	}
}

// The repo's own docker wins, then repos.defaults' for a repo without a
// .devbot.yaml; neither leaves it to the base branch, unless the install
// runs no daemon, turns detection off, or repos.limits.docker is false.
func TestRepoDocker(t *testing.T) {
	f := fake.New()
	refs := map[string]forge.RepoRef{}
	for name, yml := range map[string]string{"on": "docker: true\n", "off": "docker: false\n", "unset": "version: 1\n", "bare": ""} {
		refs[name] = forge.RepoRef{Provider: "fake", Owner: "acme", Name: name}
		f.AddRepo(refs[name], yml)
	}
	type want struct{ on, settled bool }
	detect, no, yes := want{false, false}, want{false, true}, want{true, true}
	p := t.TempDir() + "/config.yaml"
	for _, c := range []struct {
		config string
		want   map[string]want
	}{
		{"", map[string]want{"on": no, "off": no, "unset": no, "bare": no}},
		{"docker: {mode: dind}\n", map[string]want{"on": yes, "off": no, "unset": detect, "bare": detect}},
		{"docker: {mode: dind, detect: false}\n", map[string]want{"on": yes, "off": no, "unset": no, "bare": no}},
		{"docker: {mode: dind}\nrepos: {defaults: {docker: true}}\n", map[string]want{"on": yes, "off": no, "unset": detect, "bare": yes}},
		{"docker: {mode: dind}\nrepos: {defaults: {docker: false}}\n", map[string]want{"on": yes, "off": no, "unset": detect, "bare": no}},
		{"docker: {mode: dind}\nrepos: {limits: {docker: false}, defaults: {docker: true}}\n", map[string]want{"on": no, "off": no, "unset": no, "bare": no}},
	} {
		os.WriteFile(p, []byte(c.config), 0o644)
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("%q: %v", c.config, err)
		}
		cache := &RepoCache{Forges: forge.Registry{"fake": f}, Policy: cfg.Repos}
		for name, w := range c.want {
			rc, err := cache.Get(t.Context(), refs[name])
			if err != nil {
				t.Fatalf("%q: %s: %v", c.config, name, err)
			}
			if on, settled := cfg.DockerSettled(rc); on != w.on || settled != w.settled {
				t.Errorf("%q: %s: got on %v settled %v, want %+v", c.config, name, on, settled, w)
			}
		}
	}
	for _, bad := range []string{"docker: {mode: podman}\n", "repos: {limits: {docker: true}}\n"} {
		os.WriteFile(p, []byte(bad), 0o644)
		if _, err := Load(p); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

// Each signal, in its ecosystem's own file, at the root or two directories
// down, turns Docker on, naming the file; the same words elsewhere, deeper
// or in something that isn't the manifest don't.
func TestDetectDocker(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
		want  string // the file, or "" for none
	}{
		{"go", map[string]string{"go.mod": "module x\n\nrequire (\n\tgithub.com/testcontainers/testcontainers-go v0.40.0\n)\n"}, "go.mod"},
		{"go module under it", map[string]string{"go.mod": "module x\n\nrequire github.com/testcontainers/testcontainers-go/modules/postgres v0.40.0\n"}, "go.mod"},
		{"go comment", map[string]string{"go.mod": "module x\n\n// github.com/testcontainers/testcontainers-go v0.40.0\nrequire example.com/y v1.0.0 // github.com/testcontainers/testcontainers-go\n"}, ""},
		{"node", map[string]string{"package.json": `{"devDependencies": {"testcontainers": "^10"}}`}, "package.json"},
		{"node scoped", map[string]string{"web/package.json": `{"dependencies": {"@testcontainers/postgresql": "^10"}}`}, "web/package.json"},
		{"node elsewhere", map[string]string{"package.json": `{"description": "testcontainers", "dependencies": {"testcontainers-ish": "1"}}`}, ""},
		{"pyproject", map[string]string{"pyproject.toml": "[project]\ndependencies = [\"testcontainers[postgres]>=4\"]\n"}, "pyproject.toml"},
		{"requirements", map[string]string{"requirements-dev.txt": "pytest\ntestcontainers==4.8\n"}, "requirements-dev.txt"},
		{"setup.cfg", map[string]string{"setup.cfg": "[options]\ninstall_requires =\n    testcontainers\n"}, "setup.cfg"},
		{"Pipfile", map[string]string{"Pipfile": "[dev-packages]\ntestcontainers = \"*\"\n"}, "Pipfile"},
		{"maven", map[string]string{"pom.xml": "<groupId>org.testcontainers</groupId>"}, "pom.xml"},
		{"gradle", map[string]string{"app/build.gradle": "testImplementation 'org.testcontainers:postgresql:1.20.0'"}, "app/build.gradle"},
		{"gradle kts", map[string]string{"build.gradle.kts": `testImplementation("org.testcontainers:junit-jupiter:1.20.0")`}, "build.gradle.kts"},
		{"csproj", map[string]string{"tests/Api.Tests/Api.Tests.csproj": `<PackageReference Include="Testcontainers.PostgreSql" Version="4.0.0" />`}, "tests/Api.Tests/Api.Tests.csproj"},
		{"central packages", map[string]string{"Directory.Packages.props": `<PackageVersion Include="Testcontainers" Version="4.0.0" />`}, "Directory.Packages.props"},
		{"cargo", map[string]string{"Cargo.toml": "[dev-dependencies]\ntestcontainers = \"0.23\"\n"}, "Cargo.toml"},
		{"compose at the root", map[string]string{"compose.yaml": "services: {}\n"}, "compose.yaml"},
		{"compose at depth 2", map[string]string{"deploy/local/docker-compose.yml": "services: {}\n"}, "deploy/local/docker-compose.yml"},
		{"compose too deep", map[string]string{"a/b/c/compose.yml": "services: {}\n"}, ""},
		{"manifest too deep", map[string]string{"a/b/c/go.mod": "require github.com/testcontainers/testcontainers-go v0.40.0\n"}, ""},
		{"not a manifest", map[string]string{"README.md": "uses org.testcontainers and testcontainers-go"}, ""},
		{"nothing", map[string]string{"go.mod": "module x\n", "package.json": "{}"}, ""},
		{"the shallowest wins", map[string]string{"x/compose.yaml": "", "y/z/docker-compose.yaml": ""}, "x/compose.yaml"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := fake.New()
			repo := forge.RepoRef{Provider: "fake", Owner: "acme", Name: "app"}
			f.AddRepo(repo, "")
			for p, b := range c.files {
				f.Files[repo.String()+":"+p] = []byte(b)
			}
			cache := &RepoCache{Forges: forge.Registry{"fake": f}}
			s, err := cache.DetectDocker(t.Context(), repo, "main")
			if err != nil || s.Path != c.want {
				t.Fatalf("got %+v, %v; want %q", s, err, c.want)
			}
			if c.want != "" && !strings.HasPrefix(s.String(), "`"+c.want+"` ") {
				t.Errorf("the signal doesn't name the file: %q", s)
			}
		})
	}
}

// Detection costs one tree listing and at most dockerReads reads per repo
// and commit, and none of either again until the branch moves. A tree that
// can't be listed is an error, and isn't remembered.
func TestDetectDockerCost(t *testing.T) {
	f := fake.New()
	repo := forge.RepoRef{Provider: "fake", Owner: "acme", Name: "app"}
	f.AddRepo(repo, "")
	for i := range 40 {
		f.Files[fmt.Sprintf("%s:svc%02d/go.mod", repo, i)] = []byte("module x\n")
	}
	cache := &RepoCache{Forges: forge.Registry{"fake": f}}
	for range 3 {
		if s, err := cache.DetectDocker(t.Context(), repo, "main"); err != nil || s.Path != "" {
			t.Fatalf("got %+v, %v", s, err)
		}
	}
	if f.Calls["ListTree"] != 1 || f.Calls["GetFile"] != dockerReads || f.Calls["BranchHead"] != 3 {
		t.Fatalf("calls %v, want one listing and %d reads", f.Calls, dockerReads)
	}

	f.Files[repo.String()+":compose.yml"] = nil
	f.Push(repo, "main", "sha-2")
	if s, err := cache.DetectDocker(t.Context(), repo, "main"); err != nil || s.Path != "compose.yml" {
		t.Fatalf("the moved branch: got %+v, %v", s, err)
	}
	if f.Calls["ListTree"] != 2 || f.Calls["GetFile"] != dockerReads {
		t.Fatalf("calls %v: a Compose file needs no read", f.Calls)
	}

	f.Push(repo, "main", "sha-3")
	f.TreeErr = forge.ErrTreeTooBig
	if _, err := cache.DetectDocker(t.Context(), repo, "main"); err == nil {
		t.Fatal("an unlisted tree must be an error, not no signal")
	}
	f.TreeErr = nil
	if s, err := cache.DetectDocker(t.Context(), repo, "main"); err != nil || s.Path != "compose.yml" || f.Calls["ListTree"] != 4 {
		t.Fatalf("after the failure: got %+v, %v, calls %v", s, err, f.Calls)
	}
}

// PR adoption is on unless the repo's pull_requests.enabled turns it off,
// or repos.defaults' for a repo without a .devbot.yaml; repos.limits'
// false turns it off everywhere, and a repo can't turn it back on. Every
// install default, install limit and repo setting.
func TestRepoAdoptsPRs(t *testing.T) {
	f := fake.New()
	refs := map[string]forge.RepoRef{}
	for name, yml := range map[string]string{"on": "pull_requests: {enabled: true}\n", "off": "pull_requests: {enabled: false}\n", "unset": "version: 1\n", "bare": ""} {
		refs[name] = forge.RepoRef{Provider: "fake", Owner: "acme", Name: name}
		f.AddRepo(refs[name], yml)
	}
	p := t.TempDir() + "/config.yaml"
	for _, def := range []string{"", "true", "false"} {
		for _, limit := range []string{"", "true", "false"} {
			config := "repos: {"
			if def != "" {
				config += "defaults: {pull_requests: {enabled: " + def + "}}, "
			}
			if limit != "" {
				config += "limits: {pull_requests: {enabled: " + limit + "}}"
			}
			config += "}\n"
			want := map[string]bool{"on": true, "off": false, "unset": true, "bare": def != "false"}
			if limit == "false" {
				want = map[string]bool{"on": false, "off": false, "unset": false, "bare": false}
			}
			os.WriteFile(p, []byte(config), 0o644)
			cfg, err := Load(p)
			if err != nil {
				t.Fatalf("%q: %v", config, err)
			}
			if got := *cfg.Repos.Limits.PullRequests.Enabled; got != (limit != "false") {
				t.Errorf("%q: repos.limits.pull_requests.enabled is %v", config, got)
			}
			cache := &RepoCache{Forges: forge.Registry{"fake": f}, Policy: cfg.Repos}
			for name, w := range want {
				rc, err := cache.Get(t.Context(), refs[name])
				if err != nil {
					t.Fatalf("%q: %s: %v", config, name, err)
				}
				if got := rc.AdoptsPRs(); got != w {
					t.Errorf("%q: %s: adopts PRs %v, want %v", config, name, got, w)
				}
			}
		}
	}
}

// Fork PRs are adopted where PRs are and the repo's pull_requests.forks,
// or repos.defaults' for a repo without a .devbot.yaml, doesn't turn them
// off; repos.limits.pull_requests.forks false turns them off everywhere,
// and so does any setting that turns PR adoption off. Every install
// default, install limit and repo setting.
func TestRepoAdoptsForks(t *testing.T) {
	f := fake.New()
	refs := map[string]forge.RepoRef{}
	for name, yml := range map[string]string{
		"on":       "pull_requests: {forks: true}\n",
		"off":      "pull_requests: {forks: false}\n",
		"prs off":  "pull_requests: {enabled: false, forks: true}\n",
		"unset":    "version: 1\n",
		"bare":     "",
		"only prs": "pull_requests: {enabled: true}\n",
	} {
		refs[name] = forge.RepoRef{Provider: "fake", Owner: "acme", Name: strings.ReplaceAll(name, " ", "-")}
		f.AddRepo(refs[name], yml)
	}
	p := t.TempDir() + "/config.yaml"
	for _, def := range []string{"", "forks: true", "forks: false", "enabled: false"} {
		for _, limit := range []string{"", "forks: true", "forks: false", "enabled: false"} {
			config := "repos: {"
			if def != "" {
				config += "defaults: {pull_requests: {" + def + "}}, "
			}
			if limit != "" {
				config += "limits: {pull_requests: {" + limit + "}}"
			}
			config += "}\n"
			bare := def == "" || def == "forks: true"
			want := map[string]bool{"on": true, "off": false, "prs off": false, "unset": true, "bare": bare, "only prs": true}
			if limit == "forks: false" || limit == "enabled: false" {
				want = map[string]bool{"on": false, "off": false, "prs off": false, "unset": false, "bare": false, "only prs": false}
			}
			os.WriteFile(p, []byte(config), 0o644)
			cfg, err := Load(p)
			if err != nil {
				t.Fatalf("%q: %v", config, err)
			}
			if got := *cfg.Repos.Limits.PullRequests.Forks; got != (limit != "forks: false") {
				t.Errorf("%q: repos.limits.pull_requests.forks is %v", config, got)
			}
			cache := &RepoCache{Forges: forge.Registry{"fake": f}, Policy: cfg.Repos}
			for name, w := range want {
				rc, err := cache.Get(t.Context(), refs[name])
				if err != nil {
					t.Fatalf("%q: %s: %v", config, name, err)
				}
				if got := rc.AdoptsForks(); got != w {
					t.Errorf("%q: %s: adopts forks %v, want %v", config, name, got, w)
				}
				if rc.AdoptsForks() && !rc.AdoptsPRs() {
					t.Errorf("%q: %s: adopts forks but not PRs", config, name)
				}
			}
		}
	}
}

// A repo's commits keys override repos.defaults.commits one by one, whether
// the repo has a .devbot.yaml or not; both unset leaves commits alone.
func TestRepoCommits(t *testing.T) {
	f := fake.New()
	refs := map[string]forge.RepoRef{}
	for name, yml := range map[string]string{
		"bare":     "",
		"unset":    "version: 1\n",
		"off":      "commits: {co_authored_by: false}\n",
		"trailers": "commits: {trailers: [\"Reviewed-by: Rev <rev@example.com>\"]}\n",
		"none":     "commits: {co_authored_by: true, trailers: []}\n",
	} {
		refs[name] = forge.RepoRef{Provider: "fake", Owner: "acme", Name: name}
		f.AddRepo(refs[name], yml)
	}
	const sob = "Signed-off-by: devbot <devbot@example.com>"
	const rev = "Reviewed-by: Rev <rev@example.com>"
	on, off := true, false
	p := t.TempDir() + "/config.yaml"
	for _, c := range []struct {
		defaults string
		want     map[string]Commits
	}{
		{"", map[string]Commits{
			"bare": {}, "unset": {}, "off": {CoAuthoredBy: &off},
			"trailers": {Trailers: []string{rev}}, "none": {CoAuthoredBy: &on, Trailers: []string{}},
		}},
		{"defaults: {commits: {co_authored_by: true, trailers: [\"" + sob + "\"]}}", map[string]Commits{
			"bare": {CoAuthoredBy: &on, Trailers: []string{sob}}, "unset": {CoAuthoredBy: &on, Trailers: []string{sob}},
			"off":      {CoAuthoredBy: &off, Trailers: []string{sob}},
			"trailers": {CoAuthoredBy: &on, Trailers: []string{rev}}, "none": {CoAuthoredBy: &on, Trailers: []string{}},
		}},
	} {
		os.WriteFile(p, []byte("repos: {"+c.defaults+"}\n"), 0o644)
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("%q: %v", c.defaults, err)
		}
		cache := &RepoCache{Forges: forge.Registry{"fake": f}, Policy: cfg.Repos}
		for name, want := range c.want {
			rc, err := cache.Get(t.Context(), refs[name])
			if err != nil {
				t.Fatalf("%q: %s: %v", c.defaults, name, err)
			}
			if !reflect.DeepEqual(rc.Commits, want) {
				t.Errorf("%q: %s: commits %+v, want %+v", c.defaults, name, rc.Commits, want)
			}
			if rc.Commits.IsZero() != (want.CoAuthoredBy == nil && len(want.Trailers) == 0) {
				t.Errorf("%q: %s: IsZero %v", c.defaults, name, rc.Commits.IsZero())
			}
		}
	}
}

func TestRepoCommitTrailers(t *testing.T) {
	r, err := ParseRepo([]byte("commits: {trailers: [\"Signed-off-by: A B <a@example.com>\", \"Ticket: ABC-1\"]}\n"))
	if err != nil || len(r.Commits.Trailers) != 2 {
		t.Fatalf("%+v, %v", r, err)
	}
	for _, bad := range []string{"Signed-off-by", "Signed-off-by:", "Signed off by: x", ": x", "-x: y", "Key: a\\nb", "Key x"} {
		if _, err := ParseRepo([]byte("commits: {trailers: [\"" + bad + "\"]}\n")); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
	if _, err := DefaultRepo(map[string]any{"commits": map[string]any{"trailers": []any{"nope"}}}); err == nil {
		t.Error("repos.defaults.commits.trailers with an invalid trailer: want an error")
	}
	p := t.TempDir() + "/config.yaml"
	os.WriteFile(p, []byte("repos: {defaults: {commits: {trailers: [nope]}}}\n"), 0o644)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "commits.trailers") {
		t.Errorf("Load with an invalid repos.defaults trailer: %v", err)
	}
}

// approvable_paths defaults to the CI/CD entries of the default
// protected_paths; [] approves nothing; repos.defaults sets it for repos
// without a file; and naming an always-protected path makes the policy
// invalid, whatever glob names it.
func TestApprovablePaths(t *testing.T) {
	for _, c := range []struct {
		yaml string
		want []string
	}{
		{"version: 1\n", DefaultApprovablePaths},
		{"version: 1\napprovable_paths: []\n", []string{}},
		{"version: 1\napprovable_paths: [.github/workflows/**]\n", []string{".github/workflows/**"}},
	} {
		r, err := ParseRepo([]byte(c.yaml))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(r.ApprovablePaths, c.want) {
			t.Errorf("%q: got %v", c.yaml, r.ApprovablePaths)
		}
	}
	if slices.Contains(DefaultApprovablePaths, ".pre-commit-config.yaml") || slices.Contains(DefaultApprovablePaths, "CODEOWNERS") {
		t.Fatal("only CI/CD is approvable by default")
	}
	for _, p := range DefaultApprovablePaths {
		if !slices.Contains(DefaultProtectedPaths, p) {
			t.Errorf("%s is approvable by default but not protected", p)
		}
	}
	d, err := DefaultRepo(map[string]any{"approvable_paths": []string{}})
	if err != nil || d.ApprovablePaths == nil || len(d.ApprovablePaths) != 0 {
		t.Fatalf("repos.defaults: got %v, %v", d.ApprovablePaths, err)
	}
	for _, bad := range []string{".devbot.yaml", "AGENTS.md", "CLAUDE.md", "CLAUDE.local.md", ".mcp.json", "**/.claude/**", ".codex/**", "sub/.devbot/**", "**", "*.md", "**/*.yaml"} {
		if _, err := ParseRepo([]byte("version: 1\napprovable_paths: ['" + bad + "']\n")); err == nil || !strings.Contains(err.Error(), "always protected") {
			t.Errorf("%s: want the policy refused, got %v", bad, err)
		}
		if _, err := DefaultRepo(map[string]any{"approvable_paths": []string{bad}}); err == nil {
			t.Errorf("%s in repos.defaults: want it refused", bad)
		}
	}
}

// The implementer is told a pattern needs approval only when changing it
// would actually be held: approvable and protected.
func TestPathRules(t *testing.T) {
	for _, c := range []struct {
		yaml             string
		hard, approvable []string
	}{
		{"version: 1\nprotected_paths: [.github/**, CODEOWNERS]\n", []string{"CODEOWNERS"}, []string{".github/**"}},
		{"version: 1\nprotected_paths: []\n", nil, nil},
		{"version: 1\nprotected_paths: [.github/**]\napprovable_paths: []\n", []string{".github/**"}, nil},
		{"version: 1\nprotected_paths: [.github/workflows/**, Jenkinsfile]\napprovable_paths: [.github/**]\n", []string{"Jenkinsfile"}, []string{".github/workflows/**"}},
		{"version: 1\nprotected_paths: ['**']\napprovable_paths: [.gitea/**]\n", []string{"**"}, []string{".gitea/**"}},
	} {
		r, err := ParseRepo([]byte(c.yaml))
		if err != nil {
			t.Fatal(err)
		}
		hard, approvable := r.PathRules()
		if !slices.Equal(hard, append(c.hard, AlwaysProtected...)) || !slices.Equal(approvable, c.approvable) {
			t.Errorf("%q: got %v, %v", c.yaml, hard, approvable)
		}
	}
}

// An approvable_paths entry that names an always-protected path, or one
// inside one, is refused as much as a glob covering one.
func TestApprovablePathsRefuseAlwaysProtectedFiles(t *testing.T) {
	for _, bad := range []string{".claude/settings.json", ".devbot/a/y", "sub/.claude/x", "a/b/.codex/c.toml", "docs/AGENTS.md", "sub/.devbot.yaml", ".claude", "**/.claude/*.json"} {
		if _, err := ParseRepo([]byte("version: 1\napprovable_paths: ['" + bad + "']\n")); err == nil || !strings.Contains(err.Error(), "always protected") {
			t.Errorf("%s: want the policy refused, got %v", bad, err)
		}
	}
	for _, ok := range []string{".github/**", ".github/workflows/*", ".gitea/workflows/ci.yml", "Jenkinsfile", "deploy/**"} {
		if _, err := ParseRepo([]byte("version: 1\napprovable_paths: ['" + ok + "']\n")); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}

// CODEOWNERS and .gitattributes, at any depth, are never held for approval,
// whatever approvable_paths covers: a change to them stops for a person.
func TestNeverApprovable(t *testing.T) {
	for _, yaml := range []string{"version: 1\n", "version: 1\nprotected_paths: ['**']\napprovable_paths: ['.github/**', 'docs/**', 'src/**']\n"} {
		r, err := ParseRepo([]byte(yaml))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{".github/CODEOWNERS", ".gitea/CODEOWNERS", "docs/CODEOWNERS", ".github/.gitattributes", ".github/workflows/.gitattributes", "src/.gitattributes", ".github/.claude/x"} {
			held, hard := r.HeldChanges([]changeset.Change{{Status: "M", Path: p, OldMode: "100644", NewMode: "100644"}})
			if len(held) != 0 || len(hard) != 1 {
				t.Errorf("%q, %s: held %v, hard %v", yaml, p, held, hard)
			}
		}
		if held, _ := r.HeldChanges([]changeset.Change{{Status: "M", Path: ".github/workflows/ci.yml", OldMode: "100644", NewMode: "100644"}}); len(held) != 1 {
			t.Errorf("%q: a workflow isn't approvable", yaml)
		}
	}
}

// integration needs a command; its timeout and retries default, its repos
// are full refs that each make one directory of the workspace, and the
// install's implement timeout caps its timeout, since it runs in an
// implement pod.
func TestParseRepoIntegration(t *testing.T) {
	r, err := ParseRepo([]byte("version: 1\n"))
	if err != nil || r.Integration.On() {
		t.Fatalf("unset: got %+v, %v", r.Integration, err)
	}
	r, err = ParseRepo([]byte("version: 1\nintegration:\n  command: make e2e\n  repos: [gitea:acme/schema, github:acme/proto]\n"))
	if err != nil || r.Integration.Command != "make e2e" || r.Integration.Timeout != 30*time.Minute || *r.Integration.Retries != 2 || len(r.Integration.Repos) != 2 {
		t.Fatalf("defaults: got %+v, %v", r.Integration, err)
	}
	r, err = ParseRepo([]byte("version: 1\nintegration: {command: make e2e, timeout: 2h, retries: 0}\n"))
	if err != nil || r.Integration.Timeout != 2*time.Hour || *r.Integration.Retries != 0 {
		t.Fatalf("set: got %+v, %v", r.Integration, err)
	}
	r.Cap(RepoLimits{Timeouts: struct {
		Implement time.Duration `yaml:"implement"`
		Review    time.Duration `yaml:"review"`
	}{Implement: time.Hour}})
	if r.Integration.Timeout != time.Hour {
		t.Fatalf("capped at the implement limit: %s", r.Integration.Timeout)
	}
	for _, bad := range []string{
		"integration: {timeout: 5m}",
		"integration: {command: ' '}",
		"integration: {command: x, timeout: -1m}",
		"integration: {command: x, retries: -1}",
		"integration: {command: x, retries: 11}",
		"integration: {command: x, repos: [acme/schema]}",
		"integration: {command: x, repos: ['gitea:acme/..']}",
		"integration: {command: x, repos: [gitea:acme/a, gitea:acme/a]}",
	} {
		if _, err := ParseRepo([]byte("version: 1\n" + bad + "\n")); err == nil || !strings.Contains(err.Error(), "integration.") {
			t.Errorf("%s: got %v, want an error naming the key", bad, err)
		}
	}
}

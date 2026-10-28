// Package deploy holds the Helm chart's render tests.
package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"gopkg.in/yaml.v3"

	"git.shdw.tech/rob/devbot/internal/auth"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/forge/fake"
	"git.shdw.tech/rob/devbot/internal/queue"
)

type obj = map[string]any

// forgeValues is the provider every render starts from, since the chart refuses
// to render without one; values naming their own replace it.
const forgeValues = `
config:
  providers:
    - {name: gitea, type: gitea, url: https://gitea.example.com, implementer_user: devbot, reviewer_user: devbot-review,
       implementer_token_env: DEVBOT_TOKEN, reviewer_token_env: DEVBOT_REVIEW_TOKEN, webhook_secret_env: GITEA_WEBHOOK_SECRET}
`

// claudeImage is under every render's values, since the chart has no
// default claude image to run the default harness from.
const claudeImage = `
image:
  agents:
    claude: {repository: registry.example.com/devbot-claude}
`

// render runs helm template over the chart, as release devbot, with values
// and any more helm args, and returns its objects by "Kind/name", or helm's
// error.
func render(t *testing.T, values string, args ...string) (map[string]obj, error) {
	t.Helper()
	return renderAs(t, "devbot", values, args...)
}

// renderAs is render as release name.
func renderAs(t *testing.T, name, values string, args ...string) (map[string]obj, error) {
	t.Helper()
	out, err := helm(t, append([]string{"template", name, "helm/devbot"}, args...), values)
	if err != nil {
		return nil, err
	}
	objs := map[string]obj{}
	dec := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var o obj
		if err := dec.Decode(&o); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if o == nil {
			continue
		}
		objs[o["kind"].(string)+"/"+dig(o, "metadata", "name").(string)] = o
	}
	return objs, nil
}

// helm runs helm with args, over forgeValues, claudeImage and then values, and returns its
// output, or its error.
func helm(t *testing.T, args []string, values string) ([]byte, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm isn't on the PATH")
	}
	dir := t.TempDir()
	for name, v := range map[string]string{"forge.yaml": forgeValues, "claude-image.yaml": claudeImage, "values.yaml": values} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var stderr bytes.Buffer
	cmd := exec.Command("helm", append(args, "-f", filepath.Join(dir, "forge.yaml"), "-f", filepath.Join(dir, "claude-image.yaml"), "-f", filepath.Join(dir, "values.yaml"))...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, errors.New(stderr.String())
	}
	return out, nil
}

func mustRender(t *testing.T, values string, args ...string) map[string]obj {
	t.Helper()
	objs, err := render(t, values, args...)
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

func dig(o any, path ...string) any {
	for _, p := range path {
		m, ok := o.(obj)
		if !ok {
			return nil
		}
		o = m[p]
	}
	return o
}

// container is the job pod's container (or init container) name.
func container(t *testing.T, pod obj, name string) obj {
	t.Helper()
	for _, key := range []string{"initContainers", "containers"} {
		cs, _ := pod[key].([]any)
		for _, c := range cs {
			if c.(obj)["name"] == name {
				return c.(obj)
			}
		}
	}
	t.Fatalf("no container %s", name)
	return nil
}

func secretRefs(c obj) []string {
	var out []string
	refs, _ := c["envFrom"].([]any)
	for _, r := range refs {
		out = append(out, dig(r, "secretRef", "name").(string))
	}
	return out
}

func envNames(c obj) []string {
	var out []string
	env, _ := c["env"].([]any)
	for _, e := range env {
		out = append(out, e.(obj)["name"].(string))
	}
	return out
}

const base = `
secrets:
  forge: devbot-forge
  agents: {claude: devbot-claude, codex: devbot-codex}
networkPolicy:
  modelAPI: {codex: [192.0.2.0/24], bedrock: [198.51.100.0/24]}
`

// Every credential mode, per role: the role's pods run as their own
// ServiceAccount, only the model proxy container can get the model
// credential (its Secret, or the AWS credential EKS injects), and the forge
// Secret never reaches it or the agent.
func TestChartCredentialModes(t *testing.T) {
	cases := []struct {
		name, agents, jobs string
		// proxySecret is the Secret the implement model proxy gets, "" for none.
		proxySecret string
		saAnnotated bool
		podIdentity bool
	}{
		{name: "no bedrock", agents: `implement: {harness: claude}`, proxySecret: "devbot-claude"},
		{name: "access keys", agents: `implement: {harness: codex, model: openai.gpt-6.1-sol, aws: {region: us-east-1, auth: access_keys}}`,
			proxySecret: "devbot-codex"},
		{name: "irsa", agents: `implement: {harness: claude, model: us.anthropic.claude-opus-5-5-v1:0, aws: {region: us-east-1, auth: irsa}}`,
			jobs: `implement: {serviceAccount: {annotations: {eks.amazonaws.com/role-arn: "arn:aws:iam::111122223333:role/devbot-implement"}}}`, saAnnotated: true},
		{name: "pod identity", agents: `implement: {harness: codex, model: openai.gpt-6.1-sol, aws: {region: us-east-1, auth: pod_identity}}`,
			podIdentity: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			values := base + "config:\n  agents:\n    " + c.agents + "\n"
			if c.jobs != "" {
				values += "jobs:\n  " + c.jobs + "\n"
			}
			objs := mustRender(t, values)

			for _, kind := range []string{"implement", "review"} {
				sa := objs["ServiceAccount/devbot-"+kind]
				if sa == nil {
					t.Fatalf("no ServiceAccount for %s", kind)
				}
				if sa["automountServiceAccountToken"] != false {
					t.Errorf("%s's ServiceAccount mounts an API token", kind)
				}
				pod := dig(objs["ScaledJob/devbot-"+kind], "spec", "jobTargetRef", "template").(obj)
				spec := pod["spec"].(obj)
				if spec["serviceAccountName"] != "devbot-"+kind {
					t.Errorf("%s pods run as %v", kind, spec["serviceAccountName"])
				}
				if dig(pod, "metadata", "annotations", "eks.amazonaws.com/skip-containers") != "harness,agent" {
					t.Errorf("%s pods let EKS give the harness or the agent an AWS credential", kind)
				}
				proxy, agent, harness := container(t, spec, "model-proxy"), container(t, spec, "agent"), container(t, spec, "harness")
				if refs := secretRefs(agent); len(refs) != 0 {
					t.Errorf("%s's agent gets Secrets %v", kind, refs)
				}
				if refs := secretRefs(proxy); slices.Contains(refs, "devbot-forge") {
					t.Errorf("%s's model proxy gets the forge Secret: %v", kind, refs)
				}
				if refs := secretRefs(harness); len(refs) != 0 {
					t.Errorf("%s's harness gets whole Secrets: %v", kind, refs)
				}
				for _, c := range []obj{agent, harness} {
					for _, n := range envNames(c) {
						if strings.HasPrefix(n, "AWS_") || strings.Contains(n, "API_KEY") || strings.Contains(n, "OAUTH_TOKEN") {
							t.Errorf("%s's %s has %s", kind, c["name"], n)
						}
					}
				}
				// The proxy starts before the agent, which reads its address.
				inits := spec["initContainers"].([]any)
				if inits[0].(obj)["name"] != "model-proxy" || inits[1].(obj)["name"] != "agent" {
					t.Errorf("%s's sidecars start in the wrong order", kind)
				}
				if ro := mount(t, agent, "/run/devbot-model")["readOnly"]; ro != true {
					t.Errorf("%s's agent can write the model proxy's file", kind)
				}
				for _, m := range proxy["volumeMounts"].([]any) {
					if p := m.(obj)["mountPath"]; p == "/work" || p == "/run/devbot" {
						t.Errorf("%s's model proxy mounts %s", kind, p)
					}
				}
			}

			spec := dig(objs["ScaledJob/devbot-implement"], "spec", "jobTargetRef", "template", "spec").(obj)
			got := secretRefs(container(t, spec, "model-proxy"))
			if c.proxySecret == "" && len(got) != 0 || c.proxySecret != "" && !slices.Equal(got, []string{c.proxySecret}) {
				t.Errorf("implement model proxy gets Secrets %v, want %q", got, c.proxySecret)
			}
			arn := dig(objs["ServiceAccount/devbot-implement"], "metadata", "annotations", "eks.amazonaws.com/role-arn")
			if (arn != nil) != c.saAnnotated {
				t.Errorf("implement ServiceAccount's role-arn: %v", arn)
			}
			if dig(objs["ServiceAccount/devbot-review"], "metadata", "annotations") != nil {
				t.Error("the review ServiceAccount has the implementer's annotations")
			}

			// serve runs as neither role's ServiceAccount.
			serve := dig(objs["Deployment/devbot"], "spec", "template", "spec").(obj)
			if sa, _ := serve["serviceAccountName"].(string); strings.HasPrefix(sa, "devbot-implement") || strings.HasPrefix(sa, "devbot-review") {
				t.Errorf("serve runs as %s", sa)
			}

			np, _ := yaml.Marshal(objs["NetworkPolicy/devbot-implement"])
			if strings.Contains(string(np), "169.254.170.23/32") != c.podIdentity {
				t.Errorf("the Pod Identity agent's endpoint allowed: %v, want %v\n%s", !c.podIdentity, c.podIdentity, np)
			}
			if np, _ := yaml.Marshal(objs["NetworkPolicy/devbot-review"]); strings.Contains(string(np), "169.254.170.23/32") {
				t.Error("the review role, not on Pod Identity, may reach its agent")
			}
		})
	}
}

func mount(t *testing.T, c obj, path string) obj {
	t.Helper()
	for _, m := range c["volumeMounts"].([]any) {
		if m.(obj)["mountPath"] == path {
			return m.(obj)
		}
	}
	t.Fatalf("%s doesn't mount %s", c["name"], path)
	return nil
}

// egress is a policy's rules, each as its destinations and ports, flattened
// to strings: "cidr:10.1.2.3/32", "ns:gitea", "pods:kube-dns", "except:...",
// "port:443".
func egress(t *testing.T, np obj) [][]string {
	t.Helper()
	if np == nil {
		t.Fatal("no such NetworkPolicy")
	}
	var out [][]string
	rules, _ := dig(np, "spec", "egress").([]any)
	for _, r := range rules {
		var rule []string
		to, _ := r.(obj)["to"].([]any)
		for _, d := range to {
			if c := dig(d, "ipBlock", "cidr"); c != nil {
				rule = append(rule, "cidr:"+c.(string))
				ex, _ := dig(d, "ipBlock", "except").([]any)
				for _, e := range ex {
					rule = append(rule, "except:"+e.(string))
				}
			}
			if n := dig(d, "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name"); n != nil {
				rule = append(rule, "ns:"+n.(string))
			}
			if l, _ := dig(d, "podSelector", "matchLabels").(obj); l != nil {
				for _, k := range slices.Sorted(maps.Keys(l)) {
					rule = append(rule, "pods:"+k+"="+l[k].(string))
				}
			}
		}
		ports, _ := r.(obj)["ports"].([]any)
		for _, p := range ports {
			rule = append(rule, fmt.Sprint("port:", p.(obj)["port"]))
		}
		out = append(out, rule)
	}
	return out
}

func flat(rules [][]string) []string {
	var out []string
	for _, r := range rules {
		out = append(out, r...)
	}
	return out
}

// By default job pods reach DNS, Redis and Anthropic's API, and nothing
// else: no public or private address beyond them. Nothing reaches them.
func TestChartNetworkPolicyDefault(t *testing.T) {
	objs := mustRender(t, "secrets: {agents: {claude: devbot-claude}}\n")
	for _, kind := range []string{"implement", "review"} {
		np := objs["NetworkPolicy/devbot-"+kind]
		if got := dig(np, "spec", "podSelector", "matchLabels", "app.kubernetes.io/component"); got != kind {
			t.Fatalf("%s's policy selects %v", kind, got)
		}
		// Nothing connects in to a job pod: the harness dials out to its
		// service pod, and a Docker pod's containers publish on the pod.
		if types := toStrings(dig(np, "spec", "policyTypes")); !slices.Equal(types, []string{"Ingress", "Egress"}) || dig(np, "spec", "ingress") != nil {
			t.Fatalf("policyTypes %v, ingress %v", types, dig(np, "spec", "ingress"))
		}
		rules := egress(t, np)
		want := [][]string{
			{"pods:k8s-app=kube-dns", "port:53", "port:53"},
			{"pods:app.kubernetes.io/component=redis", "pods:app.kubernetes.io/instance=devbot"},
			{"cidr:160.79.104.0/23", "cidr:2607:6bc0::/48", "port:443"},
		}
		if len(rules) != len(want) {
			t.Fatalf("%s: rules %q, want %q", kind, rules, want)
		}
		for i := range want {
			if !slices.Equal(rules[i], want[i]) {
				t.Errorf("%s: rule %d is %q, want %q", kind, i, rules[i], want[i])
			}
		}
		spec := dig(objs["ScaledJob/devbot-"+kind], "spec", "jobTargetRef", "template", "spec").(obj)
		for _, c := range []string{"model-proxy", "agent", "harness"} {
			if slices.Contains(envNames(container(t, spec, c)), "HTTPS_PROXY") {
				t.Errorf("%s has an egress proxy nobody configured", c)
			}
		}
	}
}

// An allowlist: CIDRs (private ones too, by listing them), namespaces, an
// egress proxy the job containers are pointed at, and the internet minus
// the private ranges.
func TestChartNetworkPolicyAllowlist(t *testing.T) {
	objs := mustRender(t, `
secrets: {agents: {claude: devbot-claude, codex: devbot-codex}}
config:
  agents:
    review: {harness: codex}
agentEnv: {GOPROXY: "https://mirror.example.com/go", PIP_INDEX_URL: "https://mirror.example.com/pypi/simple"}
networkPolicy:
  egressProxy: {url: "http://squid.egress:3128", namespace: egress, port: 3128, noProxy: [mirror.example.com]}
  allow:
    - {cidr: 10.20.0.5/32, ports: [443]}
    - {namespace: gitea}
`)
	impl := flat(egress(t, objs["NetworkPolicy/devbot-implement"]))
	for _, want := range []string{"cidr:160.79.104.0/23", "cidr:10.20.0.5/32", "ns:gitea", "ns:egress", "port:3128"} {
		if !slices.Contains(impl, want) {
			t.Errorf("implement policy lacks %s: %q", want, impl)
		}
	}
	// codex has no default addresses; its model proxy goes through the
	// egress proxy.
	rev := flat(egress(t, objs["NetworkPolicy/devbot-review"]))
	if slices.Contains(rev, "cidr:160.79.104.0/23") || !slices.Contains(rev, "ns:egress") {
		t.Errorf("review policy %q", rev)
	}
	for _, d := range append(impl, rev...) {
		if d == "cidr:0.0.0.0/0" || d == "cidr:::/0" {
			t.Errorf("the internet is open without networkPolicy.internet")
		}
	}

	spec := dig(objs["ScaledJob/devbot-review"], "spec", "jobTargetRef", "template", "spec").(obj)
	for _, name := range []string{"model-proxy", "agent", "harness"} {
		env := map[string]string{}
		for _, e := range container(t, spec, name)["env"].([]any) {
			v, _ := e.(obj)["value"].(string)
			env[e.(obj)["name"].(string)] = v
		}
		if env["HTTPS_PROXY"] != "http://squid.egress:3128" || env["https_proxy"] != env["HTTPS_PROXY"] {
			t.Errorf("%s: HTTPS_PROXY=%q", name, env["HTTPS_PROXY"])
		}
		// The model proxy is on loopback: the CLIs mustn't go round by the
		// egress proxy to reach it.
		for _, h := range []string{"127.0.0.1", "localhost", "169.254.170.23", "mirror.example.com"} {
			if !slices.Contains(strings.Split(env["NO_PROXY"], ","), h) {
				t.Errorf("%s: NO_PROXY=%q lacks %s", name, env["NO_PROXY"], h)
			}
		}
		if name == "agent" && env["DEVBOT_PASS_ENV"] != "GOPROXY,PIP_INDEX_URL" {
			t.Errorf("agentEnv isn't passed on to the model's commands: DEVBOT_PASS_ENV=%q", env["DEVBOT_PASS_ENV"])
		}
	}

	objs = mustRender(t, `
secrets: {agents: {claude: devbot-claude}}
networkPolicy: {internet: true, privateCIDRs: [10.0.0.0/8, "fc00::/7"]}
`)
	got := flat(egress(t, objs["NetworkPolicy/devbot-implement"]))
	for _, want := range []string{"cidr:0.0.0.0/0", "except:10.0.0.0/8", "cidr:::/0", "except:fc00::/7"} {
		if !slices.Contains(got, want) {
			t.Errorf("with internet, the policy lacks %s: %q", want, got)
		}
	}

	objs = mustRender(t, "secrets: {agents: {claude: devbot-claude}}\nnetworkPolicy: {enabled: false}\n")
	if objs["NetworkPolicy/devbot-implement"] != nil {
		t.Error("a policy while disabled")
	}
}

func TestChartRefusesIncompleteModes(t *testing.T) {
	const bedrockAPI = "networkPolicy: {modelAPI: {bedrock: [198.51.100.0/24]}}\n"
	cases := []struct{ name, values, want string }{
		{"irsa without a role", bedrockAPI + `
config:
  agents:
    implement: {harness: claude, model: m, aws: {region: us-east-1, auth: irsa}}
`, "eks.amazonaws.com/role-arn"},
		{"access keys without a Secret", bedrockAPI + `
secrets: {agents: {}}
config:
  agents:
    implement: {harness: codex, model: m, aws: {region: us-east-1, auth: access_keys}}
    review: {harness: codex, model: m, aws: {region: us-east-1, auth: pod_identity}}
`, "secrets.agents.codex"},
		// A default-deny policy with no way to the model would fail every run.
		{"codex without its API's addresses", `
secrets: {agents: {codex: devbot-codex}}
config:
  agents:
    review: {harness: codex}
`, "networkPolicy.modelAPI.codex"},
		{"bedrock without its addresses", `
config:
  agents:
    implement: {harness: claude, model: m, aws: {region: us-east-1, auth: pod_identity}}
`, "networkPolicy.modelAPI.bedrock"},
		{"an egress proxy with nowhere to reach it", `
networkPolicy: {egressProxy: {url: "http://squid:3128"}}
`, "networkPolicy.egressProxy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := render(t, c.values)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error naming %s, got %v", c.want, err)
			}
		})
	}
}

// redisScript is the Redis container's startup script, with its ACL.
func redisScript(t *testing.T, objs map[string]obj) string {
	t.Helper()
	spec := dig(objs["Deployment/devbot-redis"], "spec", "template", "spec").(obj)
	args, _ := container(t, spec, "redis")["args"].([]any)
	if len(args) != 1 {
		t.Fatalf("redis args: %v", args)
	}
	return args[0].(string)
}

// aclUser returns a user's line of the ACL in the script.
func aclUser(t *testing.T, script, user string) []string {
	t.Helper()
	for _, l := range strings.Split(script, "\n") {
		if f := strings.Fields(l); len(f) > 2 && f[0] == "user" && f[1] == user {
			return f[2:]
		}
	}
	t.Fatalf("no ACL user %s in\n%s", user, script)
	return nil
}

// globMatch is Redis's key glob, for the '*' the chart uses.
func globMatch(pattern, key string) bool {
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$"
	return regexp.MustCompile(re).MatchString(key)
}

// Redis has no anonymous user; serve and the harness each connect as their
// own, from the Secret, and nothing reaches the agent.
func TestChartRedisAuth(t *testing.T) {
	cases := []struct{ name, values, secret string }{
		{"generated", "", "devbot-redis"},
		{"existing", "redis: {existingSecret: my-redis}\n", "my-redis"},
		{"bring your own", "redis: {enabled: false, existingSecret: my-redis}\n", "my-redis"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			objs := mustRender(t, base+c.values)
			if gen := objs["Secret/devbot-redis"] != nil; gen != (c.name == "generated") {
				t.Errorf("the chart generated a Secret: %v", gen)
			}
			if c.name != "bring your own" {
				// Off, with no password, so no connection is it; allowed
				// everything, for replaying the append-only file.
				if u := aclUser(t, redisScript(t, objs), "default"); !slices.Equal(u, []string{"off", "~*", "&*", "+@all"}) {
					t.Errorf("Redis's default user: %v", u)
				}
			}
			redisEnv := func(c obj) (user, secret, key string) {
				env, _ := c["env"].([]any)
				for _, e := range env {
					e := e.(obj)
					switch e["name"] {
					case "DEVBOT_REDIS_USERNAME":
						user, _ = e["value"].(string)
					case "DEVBOT_REDIS_PASSWORD":
						secret, _ = dig(e, "valueFrom", "secretKeyRef", "name").(string)
						key, _ = dig(e, "valueFrom", "secretKeyRef", "key").(string)
					}
				}
				return
			}
			serve := dig(objs["Deployment/devbot"], "spec", "template", "spec").(obj)
			if u, s, k := redisEnv(container(t, serve, "serve")); u != "devbot-serve" || s != c.secret || k != "serve-password" {
				t.Errorf("serve connects as %s with %s/%s", u, s, k)
			}
			for _, kind := range []string{"implement", "review"} {
				pod := dig(objs["ScaledJob/devbot-"+kind], "spec", "jobTargetRef", "template", "spec").(obj)
				if u, s, k := redisEnv(container(t, pod, "harness")); u != "devbot-harness" || s != c.secret || k != "harness-password" {
					t.Errorf("%s's harness connects as %s with %s/%s", kind, u, s, k)
				}
				agent, _ := yaml.Marshal(container(t, pod, "agent"))
				if strings.Contains(string(agent), "REDIS") || strings.Contains(string(agent), c.secret) {
					t.Errorf("%s's agent has a Redis credential:\n%s", kind, agent)
				}
				vols, _ := yaml.Marshal(pod["volumes"])
				if strings.Contains(string(vols), "secret") {
					t.Errorf("%s pods mount a Secret:\n%s", kind, vols)
				}
				trig := dig(objs["ScaledJob/devbot-"+kind], "spec", "triggers").([]any)[0]
				if dig(trig, "authenticationRef", "name") != "devbot-redis" {
					t.Errorf("%s's KEDA trigger has no credential: %v", kind, trig)
				}
			}
			ta, _ := yaml.Marshal(objs["TriggerAuthentication/devbot-redis"])
			if !strings.Contains(string(ta), c.secret) || !strings.Contains(string(ta), "keda-password") {
				t.Errorf("KEDA's credential:\n%s", ta)
			}
		})
	}
	if _, err := render(t, base+"redis: {enabled: false}\n"); err == nil || !strings.Contains(err.Error(), "redis.existingSecret") {
		t.Fatalf("a Redis of one's own without its Secret: want an error naming redis.existingSecret, got %v", err)
	}
}

// Sample keys: what internal/auth keeps, and what a job uses.
var (
	authKeys = []string{"devbot:auth:at:0f3a", "devbot:auth:grant:gabc", "devbot:auth:client:devbot_c_x", "devbot:auth:forge:gitea:alice",
		"devbot:auth:rt:0f3a", "devbot:auth:session:0f3a", "devbot:auth:lock:person:gitea:alice", "devbot:auth:person-grants:gitea:alice"}
	jobKeys = []string{"devbot:queued", "devbot:q:implement", "devbot:q:review", "devbot:q:implement-docker", "devbot:q:review-docker", "devbot:q:integrate", "devbot:q:integrate-docker", "devbot:q:plan", "devbot:q:delayed", "devbot:ratelimit:until",
		"devbot:quota:github", "devbot:events", "devbot:stats", "devbot:lease:implement:gitea:acme/app#1", "devbot:lease-info:implement:gitea:acme/app#1",
		"devbot:merge:gitea:acme/app", "devbot:merge-retry:gitea:acme/app#1", "devbot:eval:gitea:acme/app#1", "devbot:eval-dirty:gitea:acme/app#1", "devbot:once:x",
		"devbot:eval-requests"}
)

// The harness's user reaches a job's keys and none of the sign-ins; KEDA's
// only the queue lengths.
func TestChartRedisACL(t *testing.T) {
	script := redisScript(t, mustRender(t, base))
	patterns := func(user string) []string {
		var out []string
		for _, r := range aclUser(t, script, user) {
			if p, ok := strings.CutPrefix(r, "~"); ok {
				out = append(out, p)
			}
		}
		return out
	}
	allowed := func(ps []string, key string) bool {
		return slices.ContainsFunc(ps, func(p string) bool { return globMatch(p, key) })
	}
	harness := patterns("devbot-harness")
	for _, k := range authKeys {
		if allowed(harness, k) {
			t.Errorf("the harness can reach %s", k)
		}
	}
	for _, k := range jobKeys {
		if !allowed(harness, k) {
			t.Errorf("the harness can't reach %s", k)
		}
	}
	// The bots' logins are serve's to write; a pod only reads them.
	if allowed(harness, "devbot:bots:github") || !slices.Contains(aclUser(t, script, "devbot-harness"), "%R~devbot:bots:*") {
		t.Errorf("the harness's access to the bots' logins: %v", aclUser(t, script, "devbot-harness"))
	}
	// Its engine reads the repo config generation that serve bumps on a push.
	if allowed(harness, "devbot:repo-config:gitea:acme/app") || !slices.Contains(aclUser(t, script, "devbot-harness"), "%R~devbot:repo-config:*") {
		t.Errorf("the harness's access to the repo config generation: %v", aclUser(t, script, "devbot-harness"))
	}
	// Its services' status is serve's to write; the job waits on it.
	if allowed(harness, "devbot:services:devbot-implement-a") || !slices.Contains(aclUser(t, script, "devbot-harness"), "%R~devbot:services:*") {
		t.Errorf("the harness's access to its services' status: %v", aclUser(t, script, "devbot-harness"))
	}
	// The leader lease and what only the leader writes are serve's.
	for _, k := range []string{"devbot:leader", "devbot:leader-term", "devbot:last-reconcile", "devbot:sub:relay"} {
		if allowed(harness, k) {
			t.Errorf("the harness can reach %s", k)
		}
	}
	keda := patterns("devbot-keda")
	for _, k := range append(authKeys, jobKeys...) {
		if allowed(keda, k) != (strings.HasPrefix(k, "devbot:q:") && k != "devbot:q:delayed") {
			t.Errorf("KEDA reaching %s: %v", k, allowed(keda, k))
		}
	}
	// KEDA runs kedaListLength, which needs EVAL and, on a list or no key,
	// TYPE and LLEN. Nothing else: no category beyond connecting, and not
	// SCRIPT or FUNCTION, which would let it flush or load what serve and the
	// harness run. TestChartRedisLive runs the script itself.
	rules := aclUser(t, script, "devbot-keda")
	for _, r := range []string{"-@all", "+@connection", "-@admin", "-@dangerous", "+type", "+llen", "+eval", "+evalsha", "-script", "-function"} {
		if !slices.Contains(rules, r) {
			t.Errorf("KEDA's ACL lacks %s: %v", r, rules)
		}
	}
	for _, r := range rules {
		if strings.HasPrefix(r, "+") && !slices.Contains([]string{"+@connection", "+type", "+llen", "+eval", "+evalsha"}, r) ||
			strings.HasPrefix(r, "&") || strings.HasPrefix(r, "%") || r == "allkeys" || r == "allcommands" || r == "allchannels" {
			t.Errorf("KEDA's ACL grants %s: %v", r, rules)
		}
	}
}

// kedaListLength is the script KEDA's Redis list scaler runs (EVAL, with the
// list as KEYS[1]) to read a queue's length: kedacore/keda v2.21.0,
// pkg/scalers/redis_scaler.go, NewRedisScaler.
const kedaListLength = `
		local listName = KEYS[1]
		local listType = redis.call('type', listName).ok
		if listType == 'zset' then
			return redis.call('zcard', listName)
		elseif listType == 'set' then
			return redis.call('scard', listName)
		elseif listType == 'hash' then
			return redis.call('hlen', listName)
		elseif listType == 'list' or listType == 'none' then
			return redis.call('llen', listName)
		else
			return redis.error_reply('ERR unsupported key type: ' .. listType)
		end
	`

// The rendered Redis, run for real: nobody gets in without a password, and
// the harness can do a job's work and nothing to sign-ins.
func TestChartRedisLive(t *testing.T) {
	addr, _, _ := startRedis(t, redisScript(t, mustRender(t, base)), t.TempDir())
	ctx := context.Background()
	serve := queue.Open(queue.Options{Addr: addr, Username: "devbot-serve", Password: "serve pw"}).R
	var err error

	if err := queue.New(addr, 0).R.Ping(ctx).Err(); err == nil || !strings.Contains(err.Error(), "NOAUTH") {
		t.Fatalf("an unauthenticated PING: want NOAUTH, got %v", err)
	}
	if err := queue.Open(queue.Options{Addr: addr, Username: "devbot-harness", Password: "wrong"}).R.Ping(ctx).Err(); err == nil {
		t.Fatal("a wrong password got in")
	}
	// The default user may do anything, for replaying the append-only file,
	// but is off: no password makes a connection it.
	for _, pw := range []string{"", "x", "serve pw"} {
		if err := queue.Open(queue.Options{Addr: addr, Username: "default", Password: pw}).R.Ping(ctx).Err(); err == nil {
			t.Fatalf("signed in as the default user with %q", pw)
		}
	}

	// A job's whole round, as the harness.
	h := queue.Open(queue.Options{Addr: addr, Username: "devbot-harness", Password: "harness-pw"})
	ref := "gitea:acme/app#1"
	step := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("harness %s: %v", what, err)
		}
	}
	_, err = h.Enqueue(ctx, queue.Job{Kind: queue.KindImplement, Ref: ref})
	step("enqueue", err)
	_, err = h.IsQueued(ctx, queue.KindImplement, ref)
	step("is queued", err)
	_, err = h.Hold(ctx)
	step("hold", err)
	j, err := h.Pop(ctx, queue.KindImplement, false)
	step("pop", err)
	if j == nil {
		t.Fatal("popped nothing")
	}
	_, err = h.Enqueue(ctx, queue.Job{Kind: queue.KindReview, Ref: ref})
	step("enqueue", err)
	_, _, err = h.Claim(ctx, queue.KindReview, false, "pod", time.Minute)
	step("claim", err)
	_, err = h.Acquire(ctx, queue.KindImplement, ref, "pod", time.Minute)
	step("acquire", err)
	_, err = h.Renew(ctx, queue.KindImplement, ref, "pod", time.Minute)
	step("renew", err)
	step("describe", h.Describe(ctx, queue.KindImplement, ref, "pod", queue.LeaseDetail{Attempt: 1}))
	_, err = h.Holder(ctx, queue.KindImplement, ref)
	step("holder", err)
	_, err = h.LockEval(ctx, ref, "pod", time.Minute)
	step("lock eval", err)
	_, err = h.UnlockEval(ctx, ref, "pod")
	step("unlock eval", err)
	_, err = h.LockMerge(ctx, "gitea:acme/app", "pod")
	step("lock merge", err)
	step("unlock merge", h.UnlockMerge(ctx, "gitea:acme/app", "pod"))
	step("set merge retry", h.SetMergeRetry(ctx, ref, queue.MergeRetry{Head: "abc", N: 1, Next: time.Now().Add(time.Minute)}))
	_, err = h.MergeRetry(ctx, ref)
	step("merge retry", err)
	step("clear merge retry", h.ClearMergeRetry(ctx, ref))
	_, err = h.Emit(ctx, queue.Event{Type: "task.working", Task: ref}, time.Hour)
	step("emit", err)
	_, _, err = h.EmitOnce(ctx, queue.Event{Type: "task.merged", Task: ref}, "x", time.Hour)
	step("emit once", err)
	h.Count(ctx, "jobs", []string{"implement"}, 1)
	step("observe", h.Observe(ctx, "job_duration_seconds", []string{"implement"}, []float64{60}, 30))
	step("set hold", h.SetHold(ctx, time.Now().Add(time.Minute)))
	_, err = h.Delay(ctx, *j, time.Now().Add(time.Minute), "pod")
	step("delay", err)
	step("release", h.Release(ctx, queue.KindImplement, ref, "pod"))
	step("request eval", h.RequestEval(ctx, ref))
	if stats, _ := serve.HGetAll(ctx, "devbot:stats").Result(); stats["jobs|implement"] != "1" {
		t.Errorf("the harness's counter didn't land: %v", stats)
	}
	if refs, _ := queue.Open(queue.Options{Addr: addr, Username: "devbot-serve", Password: "serve pw"}).TakeEvalRequests(ctx, 10); !slices.Equal(refs, []string{ref}) {
		t.Errorf("the harness's evaluation request didn't land: %v", refs)
	}

	// The bots' logins serve records, which a pod reads and can't change.
	if err := queue.Open(queue.Options{Addr: addr, Username: "devbot-serve", Password: "serve pw"}).SetBots(ctx, "github", "impl[bot]", "rev[bot]"); err != nil {
		t.Fatal(err)
	}
	if impl, rev, err := h.Bots(ctx, "github"); err != nil || impl != "impl[bot]" || rev != "rev[bot]" {
		t.Errorf("the harness reading the bots' logins: %q %q %v", impl, rev, err)
	}
	if err := h.SetBots(ctx, "github", "evil", "evil"); !aclRefused(err) {
		t.Errorf("the harness rewrote the bots' logins: %v", err)
	}

	// The repo config generation serve bumps, which a pod's engine reads.
	sq := queue.Open(queue.Options{Addr: addr, Username: "devbot-serve", Password: "serve pw"})
	step("bump repo config", sq.BumpRepoConfig(ctx, "gitea:acme/app"))
	if g, err := h.RepoConfigGeneration(ctx, "gitea:acme/app"); err != nil || g == "" {
		t.Errorf("the harness reading the repo config generation: %q %v", g, err)
	}
	if err := h.BumpRepoConfig(ctx, "gitea:acme/app"); !aclRefused(err) {
		t.Errorf("the harness bumped the repo config generation: %v", err)
	}
	// The leader lease is serve's alone.
	if tok, err := h.TakeLeader(ctx, "pod", time.Minute); !aclRefused(err) || tok != "" {
		t.Errorf("the harness took the leader lease: %q %v", tok, err)
	}

	// Nothing of sign-ins, and nothing that lists, flushes or reconfigures.
	serve.Set(ctx, "devbot:auth:at:0f3a", "record", 0)
	for _, args := range [][]any{
		{"GET", "devbot:auth:at:0f3a"}, {"SET", "devbot:auth:at:0f3a", "forged"}, {"SET", "devbot:auth:grant:g", "forged"},
		{"DEL", "devbot:auth:at:0f3a"}, {"EVAL", "return redis.call('GET', KEYS[1])", 1, "devbot:auth:at:0f3a"},
		{"EVAL", "return redis.call('GET', 'devbot:auth:at:0f3a')", 0}, {"SCAN", 0}, {"KEYS", "*"}, {"FLUSHALL"},
		{"CONFIG", "GET", "*"}, {"ACL", "LIST"}, {"MIGRATE", "x", 1, "devbot:auth:at:0f3a", 0, 1}, {"FUNCTION", "FLUSH"},
	} {
		if err := h.R.Do(ctx, args...).Err(); !aclRefused(err) {
			t.Errorf("the harness ran %v: %v", args, err)
		}
	}
	if v, _ := serve.Get(ctx, "devbot:auth:at:0f3a").Result(); v != "record" {
		t.Errorf("the sign-in record changed: %q", v)
	}

	keda := queue.Open(queue.Options{Addr: addr, Username: "devbot-keda", Password: "keda-pw"}).R
	if err := keda.LLen(ctx, "devbot:q:implement").Err(); err != nil {
		t.Errorf("KEDA's LLEN: %v", err)
	}
	if err := keda.Type(ctx, "devbot:q:review").Err(); err != nil {
		t.Errorf("KEDA's TYPE: %v", err)
	}
	// What KEDA's scaler actually sends: its script, by EVAL, and by EVALSHA
	// once serve or anyone has loaded it.
	serve.Del(ctx, "devbot:q:implement")
	serve.RPush(ctx, "devbot:q:implement", "a", "b")
	if n, err := keda.Eval(ctx, kedaListLength, []string{"devbot:q:implement"}).Int64(); err != nil || n != 2 {
		t.Errorf("KEDA's list length of devbot:q:implement: %d %v", n, err)
	}
	if n, err := keda.Eval(ctx, kedaListLength, []string{"devbot:q:review"}).Int64(); err != nil || n != 0 {
		t.Errorf("KEDA's list length of devbot:q:review: %d %v", n, err)
	}
	sha, err := serve.ScriptLoad(ctx, kedaListLength).Result()
	if err != nil {
		t.Fatal(err)
	}
	if n, err := keda.EvalSha(ctx, sha, []string{"devbot:q:implement"}).Int64(); err != nil || n != 2 {
		t.Errorf("KEDA's list length of devbot:q:implement by EVALSHA: %d %v", n, err)
	}
	for _, k := range []string{"devbot:auth:x", "devbot:auth:at:0f3a", "devbot:queued", "devbot:q:delayed"} {
		if err := keda.Eval(ctx, kedaListLength, []string{k}).Err(); !aclRefused(err) {
			t.Errorf("KEDA's list length of %s: %v", k, err)
		}
	}
	for _, args := range [][]any{
		{"GET", "devbot:auth:at:0f3a"}, {"LPUSH", "devbot:q:implement", "x"}, {"LLEN", "devbot:queued"},
		{"EVAL", "return redis.call('GET', KEYS[1])", 1, "devbot:auth:at:0f3a"},
		{"EVAL", "return redis.call('LPOP', KEYS[1])", 1, "devbot:q:implement"},
		{"EVAL", "return redis.call('LLEN', 'devbot:queued')", 0},
		{"SCRIPT", "LOAD", "return 1"}, {"SCRIPT", "FLUSH"}, {"FUNCTION", "LIST"}, {"FUNCTION", "FLUSH"},
		{"FCALL", "x", 0}, {"KEYS", "*"}, {"FLUSHALL"}, {"CONFIG", "GET", "*"},
	} {
		if err := keda.Do(ctx, args...).Err(); !aclRefused(err) {
			t.Errorf("KEDA ran %v: %v", args, err)
		}
	}
	if n, _ := serve.LLen(ctx, "devbot:q:implement").Result(); n != 2 {
		t.Errorf("KEDA changed devbot:q:implement: %d", n)
	}
}

// aclRefused reports whether err is Redis refusing a command for its ACL.
// The wording depends on the version and the path: NOPERM for a command or
// a key, and inside a script, on 7.0, "The user executing the script" and
// "can't access at least one of the keys", "can't run this command" or
// "lacking the permissions".
func aclRefused(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "NOPERM") || strings.Contains(msg, "permission") || strings.Contains(msg, "The user executing the script can't")
}

func TestACLRefused(t *testing.T) {
	for msg, want := range map[string]bool{
		"NOPERM User devbot-harness has no permissions to run the 'keys' command": true,
		"NOPERM No permissions to access a key":                                   true,
		"ERR The user executing the script can't access at least one of the keys mentioned in the command arguments script: 9851de72, on @user_script:1.": true,
		"ERR The user executing the script can't run this command or subcommand script: 900b6d02, on @user_script:1.":                                     true,
		"ERR The user executing the script lacking the permissions for the command":                                                                       true,
		"ERR Error compiling script (new function): user_script:1: unexpected symbol near 'x'":                                                            false,
		"WRONGTYPE Operation against a key holding the wrong kind of value":                                                                               false,
		"ERR unknown command 'NOPE', with args beginning with: ":                                                                                          false,
	} {
		if got := aclRefused(errors.New(msg)); got != want {
			t.Errorf("aclRefused(%q) = %v, want %v", msg, got, want)
		}
	}
	if aclRefused(nil) {
		t.Error("aclRefused(nil): a command that ran counted as refused")
	}
}

// startRedis runs the chart's Redis script for real, with its data in dir,
// and waits until it answers serve. It returns Redis's address, its log
// file, and a stop that kills it the way a lost node would: no shutdown and
// no last save. Started again on the same dir, it is the same Redis
// restarted.
func startRedis(t *testing.T, script, dir string) (addr, logFile string, stop func()) {
	t.Helper()
	server, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server isn't on the PATH: install Redis 7.4 to run the live Redis tests")
	}
	port, bin := freePort(t), t.TempDir()
	os.WriteFile(filepath.Join(bin, "redis-server"), []byte("#!/bin/sh\nexec "+server+" \"$@\" --port "+port+" --bind 127.0.0.1\n"), 0o755)
	// Redis changes to its dir as it reads the option, so it has to be dir
	// there rather than overridden after.
	script = strings.ReplaceAll(script, "--dir /data ", "--dir "+dir+" ")
	logFile = filepath.Join(bin, "redis.log")
	log, err := os.Create(logFile)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "SERVE_PASSWORD=serve pw", "HARNESS_PASSWORD=harness-pw", "KEDA_PASSWORD=keda-pw")
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop = func() { once.Do(func() { cmd.Process.Kill(); cmd.Wait() }) }
	t.Cleanup(stop)
	addr = "127.0.0.1:" + port
	r := queue.Open(queue.Options{Addr: addr, Username: "devbot-serve", Password: "serve pw"}).R
	defer r.Close()
	deadline := time.Now().Add(10 * time.Second)
	for r.Ping(context.Background()).Err() != nil {
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(logFile)
			t.Fatalf("Redis didn't come up:\n%s", b)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return addr, logFile, stop
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// The chart's Redis holds sign-ins and the queue together, so it must
// refuse writes when full rather than evict either.
func TestChartRedisDoesNotEvict(t *testing.T) {
	script := redisScript(t, mustRender(t, base))
	if !strings.Contains(script, "--maxmemory '192mb' --maxmemory-policy noeviction") {
		t.Fatalf("redis script:\n%s", script)
	}
}

// secretKeys is every "Secret/key" a container's env takes one key from.
func secretKeys(c obj) []string {
	var out []string
	env, _ := c["env"].([]any)
	for _, e := range env {
		if ref, ok := dig(e, "valueFrom", "secretKeyRef").(obj); ok {
			out = append(out, ref["name"].(string)+"/"+ref["key"].(string))
		}
	}
	slices.Sort(out)
	return out
}

// Each job pod's harness gets its own bot's credential and nothing else of
// the forge Secret, beside its Redis password: an implement pod can't
// approve or merge, a review pod can't push as the implementer, and neither
// can forge a webhook. serve gets the whole Secret.
func TestChartForgeCredentialsPerRole(t *testing.T) {
	objs := mustRender(t, base+`
config:
  providers:
    - {name: gitea, type: gitea, url: https://gitea.example, implementer_user: devbot, reviewer_user: devbot-review,
       implementer_token_env: DEVBOT_TOKEN, reviewer_token_env: DEVBOT_REVIEW_TOKEN, webhook_secret_env: GITEA_WEBHOOK_SECRET}
    - {name: github, type: github, url: https://api.github.com, implementer_app_id: 1, reviewer_app_id: 2,
       implementer_key_env: GITHUB_APP_DEVBOT_KEY, reviewer_key_env: GITHUB_APP_REVIEW_KEY, webhook_secret_env: GITHUB_WEBHOOK_SECRET}
`)
	keys := map[string][]string{
		"implement": {"devbot-forge/DEVBOT_TOKEN", "devbot-forge/GITHUB_APP_DEVBOT_KEY", "devbot-redis/harness-password"},
		"review":    {"devbot-forge/DEVBOT_REVIEW_TOKEN", "devbot-forge/GITHUB_APP_REVIEW_KEY", "devbot-redis/harness-password"},
	}
	for kind, w := range keys {
		spec := dig(objs["ScaledJob/devbot-"+kind], "spec", "jobTargetRef", "template", "spec").(obj)
		for _, name := range []string{"agent", "harness"} {
			c := container(t, spec, name)
			got, want := secretKeys(c), w
			if name == "agent" {
				want = nil
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s's %s gets %v, want %v", kind, name, got, want)
			}
			if refs := secretRefs(c); slices.Contains(refs, "devbot-forge") {
				t.Errorf("%s's %s gets the whole forge Secret", kind, name)
			}
		}
	}
	serve := container(t, dig(objs["Deployment/devbot"], "spec", "template", "spec").(obj), "serve")
	if refs := secretRefs(serve); !slices.Contains(refs, "devbot-forge") {
		t.Errorf("serve gets %v, want the forge Secret with the webhook secrets", refs)
	}
}

// Both job containers run with the hardening defaults, in a user namespace
// and under the runtime's seccomp profile.
func TestChartJobPodSecurity(t *testing.T) {
	objs := mustRender(t, base)
	for _, kind := range []string{"implement", "review"} {
		spec := dig(objs["ScaledJob/devbot-"+kind], "spec", "jobTargetRef", "template", "spec").(obj)
		if spec["hostUsers"] != false {
			t.Errorf("%s: hostUsers %v", kind, spec["hostUsers"])
		}
		if p := dig(spec, "securityContext", "seccompProfile", "type"); p != "RuntimeDefault" {
			t.Errorf("%s: seccomp profile %v", kind, p)
		}
		for _, k := range []string{"runtimeClassName", "tolerations", "affinity"} {
			if _, ok := spec[k]; ok {
				t.Errorf("%s: %s set by default", kind, k)
			}
		}
		h := container(t, spec, "harness")["securityContext"].(obj)
		if h["runAsNonRoot"] != true || h["readOnlyRootFilesystem"] != true || h["allowPrivilegeEscalation"] != false ||
			!slices.Equal(dig(h, "capabilities", "drop").([]any), []any{"ALL"}) || dig(h, "capabilities", "add") != nil {
			t.Errorf("%s's harness: %v", kind, h)
		}
		a := container(t, spec, "agent")["securityContext"].(obj)
		drop, _ := dig(a, "capabilities", "drop").([]any)
		if a["allowPrivilegeEscalation"] != false || !slices.Contains(drop, any("NET_RAW")) || !slices.Contains(drop, any("MKNOD")) {
			t.Errorf("%s's agent: %v", kind, a)
		}
	}

	objs = mustRender(t, base+`
jobs:
  userNamespaces: false
  runtimeClassName: gvisor
  nodeSelector: {devbot.example/jobs: "true"}
  tolerations: [{key: devbot.example/jobs, operator: Exists, effect: NoSchedule}]
  affinity: {nodeAffinity: {requiredDuringSchedulingIgnoredDuringExecution: {nodeSelectorTerms: [{matchExpressions: [{key: devbot.example/jobs, operator: In, values: ["true"]}]}]}}}
`)
	for _, kind := range []string{"implement", "review"} {
		spec := dig(objs["ScaledJob/devbot-"+kind], "spec", "jobTargetRef", "template", "spec").(obj)
		if _, ok := spec["hostUsers"]; ok {
			t.Errorf("%s: hostUsers with userNamespaces off", kind)
		}
		if spec["runtimeClassName"] != "gvisor" || dig(spec, "nodeSelector", "devbot.example/jobs") != "true" ||
			len(spec["tolerations"].([]any)) != 1 || dig(spec, "affinity", "nodeAffinity") == nil {
			t.Errorf("%s: %v", kind, spec)
		}
	}
	// serve stays where it was.
	if s := dig(objs["Deployment/devbot"], "spec", "template", "spec").(obj); s["runtimeClassName"] != nil || s["tolerations"] != nil {
		t.Errorf("serve: %v", s)
	}
}

// pod_labels: serve, as its own ServiceAccount, may get and patch pods and
// nothing else; the job pods still mount no API token, and tell serve their
// name.
func TestChartPodLabelsRBAC(t *testing.T) {
	objs := mustRender(t, base)
	role := objs["Role/devbot-serve"]
	if role == nil {
		t.Fatal("no Role for serve")
	}
	rules, _ := yaml.Marshal(role["rules"])
	if string(rules) != "- apiGroups:\n    - \"\"\n  resources:\n    - pods\n  verbs:\n    - get\n    - patch\n" {
		t.Errorf("serve's Role allows\n%s", rules)
	}
	rb := objs["RoleBinding/devbot-serve"]
	if dig(rb, "roleRef", "kind") != "Role" || dig(rb, "roleRef", "name") != "devbot-serve" {
		t.Errorf("RoleBinding binds %v", rb["roleRef"])
	}
	subjects, _ := rb["subjects"].([]any)
	if len(subjects) != 1 || dig(subjects[0], "kind") != "ServiceAccount" || dig(subjects[0], "name") != "devbot-serve" {
		t.Errorf("RoleBinding subjects %v", subjects)
	}
	serve := dig(objs["Deployment/devbot"], "spec", "template", "spec").(obj)
	if serve["serviceAccountName"] != "devbot-serve" || serve["automountServiceAccountToken"] != true {
		t.Errorf("serve runs as %v, token mounted: %v", serve["serviceAccountName"], serve["automountServiceAccountToken"])
	}
	for _, kind := range []string{"implement", "review"} {
		spec := dig(objs["ScaledJob/devbot-"+kind], "spec", "jobTargetRef", "template", "spec").(obj)
		if spec["automountServiceAccountToken"] != false {
			t.Errorf("%s pods mount an API token", kind)
		}
		if !slices.Contains(envNames(container(t, spec, "harness")), "DEVBOT_POD_NAME") {
			t.Errorf("%s's harness doesn't know its pod's name", kind)
		}
	}
	var cfg obj
	yaml.Unmarshal([]byte(dig(objs["ConfigMap/devbot-config"], "data", "config.yaml").(string)), &cfg)
	if dig(cfg, "pod_labels", "enabled") != true || dig(cfg, "pod_labels", "instance") != "devbot" {
		t.Errorf("config.pod_labels: %v", cfg["pod_labels"])
	}

	// Off: no Role, and serve has no token.
	objs = mustRender(t, base+"podLabels: {enabled: false}\n")
	if objs["Role/devbot-serve"] != nil || objs["RoleBinding/devbot-serve"] != nil {
		t.Error("podLabels off still grants serve a Role")
	}
	if dig(objs["Deployment/devbot"], "spec", "template", "spec", "automountServiceAccountToken") != false {
		t.Error("podLabels off still mounts serve's token")
	}
	yaml.Unmarshal([]byte(dig(objs["ConfigMap/devbot-config"], "data", "config.yaml").(string)), &cfg)
	if dig(cfg, "pod_labels", "enabled") != false {
		t.Errorf("config.pod_labels: %v", cfg["pod_labels"])
	}
}

// serve's pod patches are held by an admission policy wherever the cluster
// has the API, and the chart won't label pods on one without it unless told
// to. helm without a cluster knows no kinds; --api-versions v1/Pod stands in
// for a cluster's discovery.
func TestChartPodLabelsPolicy(t *testing.T) {
	const (
		cluster = "v1/Pod"
		vap     = "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy"
		keda    = "keda.sh/v1alpha1/ScaledJob"
		name    = "ai-devbot-serve-pods"
	)
	policy := func(objs map[string]obj) bool {
		p, b := objs["ValidatingAdmissionPolicy/"+name], objs["ValidatingAdmissionPolicyBinding/"+name]
		if (p == nil) != (b == nil) {
			t.Fatalf("policy %v without its binding %v, or the other way round", p != nil, b != nil)
		}
		return p != nil
	}
	for _, c := range []struct {
		name, values string
		args         []string
		policy, role bool
		fails        string
	}{
		{name: "with the API", args: []string{"--api-versions", cluster, "--api-versions", vap}, policy: true, role: true},
		{name: "without a cluster", policy: true, role: true},
		{name: "without the API", args: []string{"--api-versions", cluster}, fails: "podLabels.allowUnrestrictedPatch true to accept that"},
		{name: "allowed without the API", values: "podLabels: {allowUnrestrictedPatch: true}\n", args: []string{"--api-versions", cluster}, role: true},
		{name: "allowed without a cluster", values: "podLabels: {allowUnrestrictedPatch: true}\n", role: true},
		{name: "allowed but with the API", values: "podLabels: {allowUnrestrictedPatch: true}\n", args: []string{"--api-versions", cluster, "--api-versions", vap}, policy: true, role: true},
		{name: "off without the API", values: "podLabels: {enabled: false}\n", args: []string{"--api-versions", cluster}},
		{name: "off in config", values: "config: {pod_labels: {enabled: false}}\n", args: []string{"--api-versions", cluster}},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"--namespace", "ai"}, c.args...)
			if slices.Contains(c.args, cluster) {
				// A cluster without KEDA refuses before this is checked.
				args = append(args, "--api-versions", keda)
			}
			objs, err := render(t, base+c.values, args...)
			if c.fails != "" {
				if err == nil || !strings.Contains(err.Error(), c.fails) || !strings.Contains(err.Error(), "namespace ai, image included") {
					t.Fatalf("want the chart to refuse, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := policy(objs); got != c.policy {
				t.Errorf("policy rendered: %v, want %v", got, c.policy)
			}
			if got := objs["Role/devbot-serve"] != nil; got != c.role {
				t.Errorf("Role rendered: %v, want %v", got, c.role)
			}
		})
	}

	objs := mustRender(t, base, "--namespace", "ai")
	spec := objs["ValidatingAdmissionPolicy/"+name]["spec"].(obj)
	if dig(objs["ValidatingAdmissionPolicyBinding/"+name], "spec", "policyName") != name ||
		!slices.Equal(dig(objs["ValidatingAdmissionPolicyBinding/"+name], "spec", "validationActions").([]any), []any{"Deny"}) {
		t.Errorf("binding: %v", objs["ValidatingAdmissionPolicyBinding/"+name]["spec"])
	}
	if spec["failurePolicy"] != "Fail" {
		t.Errorf("failurePolicy %v", spec["failurePolicy"])
	}
	rules, _ := yaml.Marshal(dig(spec, "matchConstraints", "resourceRules"))
	if string(rules) != "- apiGroups:\n    - \"\"\n  apiVersions:\n    - '*'\n  operations:\n    - CREATE\n    - UPDATE\n    - DELETE\n    - CONNECT\n  resources:\n    - pods\n    - pods/*\n" {
		t.Errorf("the policy matches\n%s", rules)
	}
	conds := spec["matchConditions"].([]any)
	if len(conds) != 1 || dig(conds[0], "expression") != `request.userInfo.username == "system:serviceaccount:ai:devbot-serve"` {
		t.Errorf("the policy applies to %v", conds)
	}
	// What each rule must say; TestChartPodLabelsPolicyLive runs them.
	var exprs []string
	for _, v := range spec["validations"].([]any) {
		exprs = append(exprs, strings.Join(strings.Fields(dig(v, "expression").(string)), " "))
		if dig(v, "reason") != "Forbidden" {
			t.Errorf("%v denies as %v", dig(v, "expression"), dig(v, "reason"))
		}
	}
	for _, want := range []string{
		`variables.update`,
		`variables.oldLabels["app.kubernetes.io/instance"] == "devbot" && "app.kubernetes.io/component" in variables.oldLabels && variables.oldLabels["app.kubernetes.io/component"] in ["implement", "review"]`,
		`object.spec == oldObject.spec`,
		`object.metadata.ownerReferences == oldObject.metadata.ownerReferences`,
		`object.metadata.finalizers == oldObject.metadata.finalizers`,
		`variables.oldLabels.all(k, k.startsWith("devbot.io/") || k.startsWith("labels.devbot.io/") || (k in variables.newLabels && variables.newLabels[k] == variables.oldLabels[k]))`,
		`variables.newLabels.all(k, k.startsWith("devbot.io/") || k.startsWith("labels.devbot.io/") || k in variables.oldLabels)`,
		`variables.oldAnnotations.all(k, k.startsWith("devbot.io/") || k.startsWith("labels.devbot.io/") || (k in variables.newAnnotations && variables.newAnnotations[k] == variables.oldAnnotations[k]))`,
		`variables.newAnnotations.all(k, k.startsWith("devbot.io/") || k.startsWith("labels.devbot.io/") || k in variables.oldAnnotations)`,
	} {
		if !slices.ContainsFunc(exprs, func(e string) bool { return strings.Contains(e, want) }) {
			t.Errorf("no validation says %s", want)
		}
	}
}

// serve by default: one replica, no PDB, probes split between Redis
// (readiness) and the process (liveness), and rollouts that overlap old
// and new pods, which the leader lease makes safe.
func TestChartServeDefaults(t *testing.T) {
	objs := mustRender(t, base)
	d := objs["Deployment/devbot"]
	if dig(d, "spec", "replicas") != 1 {
		t.Errorf("replicas: %v", dig(d, "spec", "replicas"))
	}
	if dig(d, "spec", "strategy", "type") != "RollingUpdate" || dig(d, "spec", "strategy", "rollingUpdate", "maxUnavailable") != 0 {
		t.Errorf("strategy: %v", dig(d, "spec", "strategy"))
	}
	if objs["PodDisruptionBudget/devbot"] != nil {
		t.Error("a PDB for a single replica blocks every node drain")
	}
	spec := dig(d, "spec", "template", "spec").(obj)
	if spec["topologySpreadConstraints"] != nil {
		t.Error("one replica has nothing to spread")
	}
	serve := container(t, spec, "serve")
	if dig(serve, "readinessProbe", "httpGet", "path") != "/readyz" || dig(serve, "livenessProbe", "httpGet", "path") != "/healthz" {
		t.Errorf("probes: %v %v", serve["readinessProbe"], serve["livenessProbe"])
	}
	// The leader lease is in Redis: serve's only API access is pod_labels'
	// (TestChartPodLabelsRBAC), with no Lease RBAC.
	for k, o := range objs {
		if strings.HasPrefix(k, "Role/") || strings.HasPrefix(k, "ClusterRole/") {
			if rules, _ := yaml.Marshal(o["rules"]); strings.Contains(string(rules), "leases") {
				t.Errorf("%s grants Leases:\n%s", k, rules)
			}
		}
	}
	redis := objs["Deployment/devbot-redis"]
	if redis == nil || objs["PersistentVolumeClaim/devbot-redis"] != nil {
		t.Fatal("want the bundled Redis, without persistence")
	}
	if s := redisScript(t, objs); !strings.Contains(s, `--save "" --appendonly no `) {
		t.Errorf("redis script:\n%s", s)
	}
}

// PR adoption's install default and limit, for PRs and for forks, go
// through config.repos to the config serve loads, beside the repos keys the
// chart sets itself.
func TestChartPullRequests(t *testing.T) {
	for _, c := range []struct {
		values                string
		def, capped           bool
		forksDef, forksCapped bool
	}{
		{"", true, false, true, false},
		{"config: {repos: {opt_in: any, defaults: {pull_requests: {enabled: false}}}}\n", false, false, false, false},
		{"config: {repos: {opt_in: any, limits: {pull_requests: {enabled: false}}}}\n", true, true, true, false},
		{"config: {repos: {opt_in: any, defaults: {pull_requests: {forks: false}}}}\n", true, false, false, false},
		{"config: {repos: {opt_in: any, limits: {pull_requests: {forks: false}}}}\n", true, false, true, true},
	} {
		objs := mustRender(t, base+c.values)
		p := filepath.Join(t.TempDir(), "config.yaml")
		os.WriteFile(p, []byte(dig(objs["ConfigMap/devbot-config"], "data", "config.yaml").(string)), 0o644)
		cfg, err := config.Load(p)
		if err != nil {
			t.Fatalf("%q: %v", c.values, err)
		}
		d, err := config.DefaultRepo(cfg.Repos.Defaults)
		if err != nil {
			t.Fatal(err)
		}
		l := cfg.Repos.Limits.PullRequests
		if d.AdoptsPRs() != c.def || *l.Enabled == c.capped || cfg.Repos.OptIn != "any" {
			t.Errorf("%q: default %v, limit %v, opt_in %q", c.values, d.AdoptsPRs(), *l.Enabled, cfg.Repos.OptIn)
		}
		if d.AdoptsForks() != c.forksDef || *l.Forks == c.forksCapped {
			t.Errorf("%q: forks default %v, limit %v", c.values, d.AdoptsForks(), *l.Forks)
		}
	}
}

// The install's commits default goes through config.repos to the config
// serve loads, and an invalid trailer stops serve there.
func TestChartCommits(t *testing.T) {
	load := func(values string) (*config.Config, error) {
		t.Helper()
		objs := mustRender(t, base+values)
		p := filepath.Join(t.TempDir(), "config.yaml")
		os.WriteFile(p, []byte(dig(objs["ConfigMap/devbot-config"], "data", "config.yaml").(string)), 0o644)
		return config.Load(p)
	}
	cfg, err := load("config: {repos: {defaults: {commits: {co_authored_by: false, trailers: [\"Signed-off-by: devbot <devbot@example.com>\"]}}}}\n")
	if err != nil {
		t.Fatal(err)
	}
	d, err := config.DefaultRepo(cfg.Repos.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if c := d.Commits; c.CoAuthoredBy == nil || *c.CoAuthoredBy || !slices.Equal(c.Trailers, []string{"Signed-off-by: devbot <devbot@example.com>"}) {
		t.Errorf("commits %+v", c)
	}
	if cfg, err = load(""); err != nil {
		t.Fatal(err)
	}
	if d, _ := config.DefaultRepo(cfg.Repos.Defaults); !d.Commits.IsZero() {
		t.Errorf("by default, commits %+v", d.Commits)
	}
	if _, err := load("config: {repos: {defaults: {commits: {trailers: [nope]}}}}\n"); err == nil {
		t.Error("an invalid trailer: want an error")
	}
}

// More than one replica: a PDB, and spread across nodes and zones unless
// the install gives its own constraints; affinity goes through as it is.
func TestChartServeHA(t *testing.T) {
	objs := mustRender(t, base+`
serve:
  replicas: 3
  podDisruptionBudget: {minAvailable: 2}
`)
	pdb := objs["PodDisruptionBudget/devbot"]
	if pdb == nil || dig(pdb, "spec", "minAvailable") != 2 {
		t.Fatalf("PDB: %v", pdb)
	}
	if dig(pdb, "spec", "selector", "matchLabels", "app.kubernetes.io/component") != "serve" {
		t.Errorf("the PDB selects %v", dig(pdb, "spec", "selector"))
	}
	d := objs["Deployment/devbot"]
	if dig(d, "spec", "replicas") != 3 {
		t.Errorf("replicas: %v", dig(d, "spec", "replicas"))
	}
	tsc, _ := dig(d, "spec", "template", "spec", "topologySpreadConstraints").([]any)
	var keys []string
	for _, c := range tsc {
		keys = append(keys, c.(obj)["topologyKey"].(string))
		if dig(c, "labelSelector", "matchLabels", "app.kubernetes.io/component") != "serve" || c.(obj)["whenUnsatisfiable"] != "ScheduleAnyway" {
			t.Errorf("spread constraint: %v", c)
		}
	}
	if !slices.Equal(keys, []string{"kubernetes.io/hostname", "topology.kubernetes.io/zone"}) {
		t.Errorf("spread over %v", keys)
	}

	objs = mustRender(t, base+`
serve:
  replicas: 2
  topologySpreadConstraints:
    - {maxSkew: 1, topologyKey: kubernetes.io/hostname, whenUnsatisfiable: DoNotSchedule, labelSelector: {matchLabels: {app.kubernetes.io/component: serve}}}
  affinity:
    podAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - {topologyKey: kubernetes.io/hostname, labelSelector: {matchLabels: {app.kubernetes.io/component: serve}}}
`)
	spec := dig(objs["Deployment/devbot"], "spec", "template", "spec").(obj)
	tsc, _ = spec["topologySpreadConstraints"].([]any)
	if len(tsc) != 1 || tsc[0].(obj)["whenUnsatisfiable"] != "DoNotSchedule" {
		t.Errorf("the install's constraints should replace the default: %v", tsc)
	}
	if dig(spec, "affinity", "podAntiAffinity") == nil {
		t.Error("affinity not passed through")
	}
}

// An external Redis with TLS and Sentinel: devbot and KEDA both reach it,
// each as its own user from redis.existingSecret, the CA and Sentinel's
// password go from secrets.redis to serve, the harness and KEDA but never
// the agent, and the bundled Redis isn't deployed.
func TestChartExternalRedis(t *testing.T) {
	objs := mustRender(t, base+`
redis: {enabled: false, existingSecret: my-redis}
secrets: {forge: devbot-forge, agents: {claude: devbot-claude}, redis: devbot-redis-extra}
config:
  redis:
    db: 2
    tls: {enabled: true, ca_env: REDIS_CA}
    sentinel: {master: mymaster, addrs: ["s1:26379", "s2.example.com:26379"], username: sentinel-user, password_env: REDIS_SENTINEL_PASSWORD}
`)
	if objs["Deployment/devbot-redis"] != nil || objs["Service/devbot-redis"] != nil || objs["Secret/devbot-redis"] != nil {
		t.Error("the bundled Redis is deployed")
	}
	extra := []string{"devbot-redis-extra/REDIS_CA", "devbot-redis-extra/REDIS_SENTINEL_PASSWORD"}
	hasExtra := func(c obj) bool {
		keys := secretKeys(c)
		return slices.Contains(keys, extra[0]) && slices.Contains(keys, extra[1])
	}
	serve := container(t, dig(objs["Deployment/devbot"], "spec", "template", "spec").(obj), "serve")
	if !hasExtra(serve) || slices.Contains(secretRefs(serve), "devbot-redis-extra") {
		t.Errorf("serve gets %v, %v", secretKeys(serve), secretRefs(serve))
	}
	for _, kind := range []string{"implement", "review"} {
		sj := objs["ScaledJob/devbot-"+kind]
		spec := dig(sj, "spec", "jobTargetRef", "template", "spec").(obj)
		if h := container(t, spec, "harness"); !hasExtra(h) || !slices.Contains(secretKeys(h), "my-redis/harness-password") {
			t.Errorf("%s's harness gets %v", kind, secretKeys(h))
		}
		if agent, _ := yaml.Marshal(container(t, spec, "agent")); strings.Contains(string(agent), "devbot-redis-extra") || strings.Contains(string(agent), "my-redis") {
			t.Errorf("%s's agent gets a Redis Secret:\n%s", kind, agent)
		}
		trig := dig(sj, "spec", "triggers").([]any)[0].(obj)
		md := trig["metadata"].(obj)
		if trig["type"] != "redis-sentinel" || md["addresses"] != "s1.default.svc.cluster.local:26379,s2.example.com:26379" || md["sentinelMaster"] != "mymaster" || md["sentinelUsername"] != "sentinel-user" ||
			md["databaseIndex"] != "2" || md["enableTLS"] != "true" || md["listName"] != "devbot:q:"+kind {
			t.Errorf("%s trigger: %v", kind, trig)
		}
		if dig(trig, "authenticationRef", "name") != "devbot-redis" {
			t.Errorf("%s trigger isn't authenticated: %v", kind, trig)
		}
	}
	refs := map[string]string{}
	for _, r := range dig(objs["TriggerAuthentication/devbot-redis"], "spec", "secretTargetRef").([]any) {
		refs[r.(obj)["parameter"].(string)] = r.(obj)["name"].(string) + "/" + r.(obj)["key"].(string)
	}
	want := map[string]string{"username": "devbot-keda-redis-user/username", "password": "my-redis/keda-password",
		"sentinelPassword": "devbot-redis-extra/REDIS_SENTINEL_PASSWORD", "ca": "devbot-redis-extra/REDIS_CA"}
	if !maps.Equal(refs, want) {
		t.Errorf("TriggerAuthentication: %v", refs)
	}

	// One endpoint, no TLS, nothing beyond the users' passwords.
	objs = mustRender(t, base+`
redis: {enabled: false, existingSecret: my-redis}
config:
  redis: {addr: "redis.example.com:6380"}
`)
	trig := dig(objs["ScaledJob/devbot-implement"], "spec", "triggers").([]any)[0].(obj)
	if trig["type"] != "redis" || dig(trig, "metadata", "address") != "redis.example.com:6380" || dig(trig, "metadata", "enableTLS") != nil {
		t.Errorf("trigger: %v", trig)
	}
	if n := len(dig(objs["TriggerAuthentication/devbot-redis"], "spec", "secretTargetRef").([]any)); n != 2 {
		t.Errorf("TriggerAuthentication has %d refs, want the user and password", n)
	}
}

func TestChartRedisSecretsNeedTheirSecret(t *testing.T) {
	cases := []struct{ name, values, want string }{
		{"named without a Secret", `
config:
  redis: {addr: "r:6379", tls: {enabled: true, ca_env: REDIS_CA}}
`, "secrets.redis: config.redis names secrets"},
		{"a Secret naming nothing", `
secrets: {redis: devbot-redis-extra}
`, "secrets.redis is set, but config.redis names none"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := render(t, c.values)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

// Persistence keeps the bundled Redis's data, sign-ins included, on a
// volume that outlives the pod.
func TestChartRedisPersistence(t *testing.T) {
	objs := mustRender(t, base+`
redis: {persistence: {enabled: true, size: 2Gi, storageClass: fast}}
`)
	pvc := objs["PersistentVolumeClaim/devbot-redis"]
	if pvc == nil || dig(pvc, "spec", "resources", "requests", "storage") != "2Gi" || dig(pvc, "spec", "storageClassName") != "fast" {
		t.Fatalf("PVC: %v", pvc)
	}
	spec := dig(objs["Deployment/devbot-redis"], "spec", "template", "spec").(obj)
	redis := container(t, spec, "redis")
	if s := redisScript(t, objs); !strings.Contains(s, `--save "" --appendonly yes --appendfsync everysec --dir /data `) {
		t.Errorf("redis script:\n%s", s)
	}
	if m, _ := redis["volumeMounts"].([]any); len(m) != 1 || dig(m[0], "mountPath") != "/data" {
		t.Errorf("volume mounts: %v", m)
	}
	vols, _ := spec["volumes"].([]any)
	if len(vols) != 1 || dig(vols[0], "persistentVolumeClaim", "claimName") != "devbot-redis" {
		t.Errorf("volumes: %v", vols)
	}
}

// The bundled Redis takes connections from this release's serve and job
// pods and KEDA's operator alone, on its port: not another release's pods,
// nor a service pod, nor anything else in the cluster, password or not.
func TestChartRedisNetworkPolicy(t *testing.T) {
	objs := mustRender(t, base+"services: {enabled: true}\nconfig: {repos: {limits: {services: {images: [registry.example/]}}}}\n")
	np := objs["NetworkPolicy/devbot-redis"]
	if np == nil {
		t.Fatal("no policy for Redis")
	}
	sel := dig(np, "spec", "podSelector", "matchLabels").(obj)
	redisLabels := dig(objs["Deployment/devbot-redis"], "spec", "template", "metadata", "labels").(obj)
	for k, v := range sel {
		if redisLabels[k] != v {
			t.Errorf("the policy selects %v, the Redis pod is %v", sel, redisLabels)
		}
	}
	if sel["app.kubernetes.io/instance"] != "devbot" || sel["app.kubernetes.io/component"] != "redis" {
		t.Errorf("the policy selects %v", sel)
	}
	if !slices.Equal(toStrings(dig(np, "spec", "policyTypes")), []string{"Ingress"}) || dig(np, "spec", "egress") != nil {
		t.Errorf("the policy's types: %v", np["spec"])
	}
	rules := dig(np, "spec", "ingress").([]any)
	if len(rules) != 1 {
		t.Fatalf("Redis's ingress: %v", rules)
	}
	if b, _ := json.Marshal(dig(rules[0], "ports")); string(b) != `[{"port":6379,"protocol":"TCP"}]` {
		t.Errorf("Redis's ports: %s", b)
	}
	from := dig(rules[0], "from").([]any)
	if len(from) != 2 {
		t.Fatalf("Redis's peers: %v", from)
	}
	ours, keda := from[0].(obj), from[1].(obj)
	if ours["namespaceSelector"] != nil || ours["ipBlock"] != nil {
		t.Errorf("Redis's own peers reach past the namespace: %v", ours)
	}
	// Whether a pod with these labels is let in.
	admits := func(labels obj) bool {
		ps := ours["podSelector"].(obj)
		for k, v := range ps["matchLabels"].(obj) {
			if labels[k] != v {
				return false
			}
		}
		for _, e := range ps["matchExpressions"].([]any) {
			e := e.(obj)
			if e["operator"] != "In" || !slices.Contains(toStrings(e["values"]), fmt.Sprint(labels[e["key"].(string)])) {
				return false
			}
		}
		return true
	}
	for _, name := range []string{"Deployment/devbot", "ScaledJob/devbot-implement", "ScaledJob/devbot-review"} {
		tmpl := dig(objs[name], "spec", "template", "metadata", "labels")
		if tmpl == nil {
			tmpl = dig(objs[name], "spec", "jobTargetRef", "template", "metadata", "labels")
		}
		if !admits(tmpl.(obj)) {
			t.Errorf("Redis doesn't admit %s: %v", name, tmpl)
		}
	}
	for _, labels := range []obj{
		{"app.kubernetes.io/name": "devbot", "app.kubernetes.io/instance": "other", "app.kubernetes.io/component": "serve"},
		{"app.kubernetes.io/name": "devbot", "app.kubernetes.io/instance": "devbot", "app.kubernetes.io/component": "services", "devbot.io/run": "x"},
		{"app.kubernetes.io/name": "devbot", "app.kubernetes.io/instance": "devbot", "app.kubernetes.io/component": "tun-plugin"},
		{"app.kubernetes.io/component": "serve"},
	} {
		if admits(labels) {
			t.Errorf("Redis admits %v", labels)
		}
	}
	if dig(keda, "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name") != "keda" ||
		dig(keda, "podSelector", "matchLabels", "app.kubernetes.io/name") != "keda-operator" || len(dig(keda, "podSelector", "matchLabels").(obj)) != 1 {
		t.Errorf("KEDA's peer: %v", keda)
	}

	// KEDA where the install has it, its default label dropped.
	objs = mustRender(t, base+"networkPolicy: {keda: {namespace: platform, podLabels: {app.kubernetes.io/name: null, app: keda-operator}}}\n")
	keda = dig(objs["NetworkPolicy/devbot-redis"], "spec", "ingress").([]any)[0].(obj)["from"].([]any)[1].(obj)
	if dig(keda, "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name") != "platform" || dig(keda, "podSelector", "matchLabels", "app") != "keda-operator" ||
		dig(keda, "podSelector", "matchLabels", "app.kubernetes.io/name") != nil {
		t.Errorf("KEDA's peer: %v", keda)
	}

	// None without the bundled Redis, or without policies.
	for _, v := range []string{"redis: {enabled: false, existingSecret: my-redis}\n", "networkPolicy: {enabled: false}\n"} {
		if objs := mustRender(t, base+v); objs["NetworkPolicy/devbot-redis"] != nil {
			t.Errorf("with %s: a policy for Redis", strings.TrimSpace(v))
		}
	}
}

// The bundled Redis's pod goes where redis.nodeSelector, .tolerations and
// .affinity say, as the jobs' do, and by default anywhere.
func TestChartRedisScheduling(t *testing.T) {
	spec := func(values string) obj {
		return dig(mustRender(t, base+values)["Deployment/devbot-redis"], "spec", "template", "spec").(obj)
	}
	def := spec("")
	for _, k := range []string{"nodeSelector", "tolerations", "affinity"} {
		if def[k] != nil {
			t.Errorf("Redis's pod has %s %v by default", k, def[k])
		}
	}
	values := `
redis:
  nodeSelector: {disk: ssd}
  tolerations: [{key: dedicated, operator: Equal, value: redis, effect: NoSchedule}]
  affinity: {nodeAffinity: {requiredDuringSchedulingIgnoredDuringExecution: {nodeSelectorTerms: [{matchExpressions: [{key: devbot.example/redis, operator: In, values: ["true"]}]}]}}}
`
	var want struct{ Redis obj }
	if err := yaml.Unmarshal([]byte(values), &want); err != nil {
		t.Fatal(err)
	}
	got := spec(values)
	for _, k := range []string{"nodeSelector", "tolerations", "affinity"} {
		a, _ := yaml.Marshal(got[k])
		b, _ := yaml.Marshal(want.Redis[k])
		if string(a) != string(b) {
			t.Errorf("Redis's pod %s:\n%s\nwant:\n%s", k, a, b)
		}
	}
}

// With persistence, a Redis restart keeps everything devbot wrote. Redis
// replays the append-only file as its default user and checks each EXEC
// against that user's ACL, so every transaction (a sign-in's records, an
// event with its counter) depends on the default user being allowed it.
func TestChartRedisPersistenceSurvivesRestart(t *testing.T) {
	script := redisScript(t, mustRender(t, base+"redis: {persistence: {enabled: true}}\n"))
	dir := t.TempDir()
	addr, _, stop := startRedis(t, script, dir)
	ctx := context.Background()
	serveStore := func(addr string) *queue.Store {
		return queue.Open(queue.Options{Addr: addr, Username: "devbot-serve", Password: "serve pw"})
	}

	// A sign-in through the broker's own endpoints, as serve: its grant,
	// tokens and the person's forge credential.
	f := fake.New()
	ts := httptest.NewUnstartedServer(nil)
	t.Cleanup(ts.Close)
	resource := "http://" + ts.Listener.Addr().String()
	broker := func(q *queue.Store) *auth.Server {
		a, err := auth.New(auth.Options{ResourceURL: resource, Forge: f, Redis: q.R, Key: []byte("an-auth-key-for-tests-that-is-long-enough"),
			AccessTTL: time.Hour, RefreshTTL: time.Hour, Log: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	mux := http.NewServeMux()
	broker(serveStore(addr)).Routes(mux)
	ts.Config.Handler = mux
	ts.Start()
	token := signIn(t, f, resource, "alice")

	// An event with its counter, a job queued, and a plain key.
	sq := serveStore(addr)
	if _, err := sq.Emit(ctx, queue.Event{Type: "task.working", Task: "gitea:acme/app#1"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := sq.Enqueue(ctx, queue.Job{Kind: queue.KindImplement, Ref: "gitea:acme/app#1"}); err != nil {
		t.Fatal(err)
	}
	if err := sq.SetBots(ctx, "gitea", "impl", "rev"); err != nil {
		t.Fatal(err)
	}
	// And a job pod's lease, as the harness.
	h := queue.Open(queue.Options{Addr: addr, Username: "devbot-harness", Password: "harness-pw"})
	if ok, err := h.Acquire(ctx, queue.KindImplement, "gitea:acme/app#2", "pod", time.Hour); err != nil || !ok {
		t.Fatalf("acquire: %v %v", ok, err)
	}
	before, err := sq.R.Keys(ctx, "*").Result()
	if err != nil || len(before) == 0 {
		t.Fatalf("keys before the restart: %v %v", before, err)
	}
	slices.Sort(before)

	// The append-only file is written once a second; a node lost after that
	// loses nothing.
	time.Sleep(1500 * time.Millisecond)
	stop()
	addr, logFile, _ := startRedis(t, script, dir)

	log, _ := os.ReadFile(logFile)
	if !strings.Contains(string(log), "DB loaded from append only file") {
		t.Errorf("Redis didn't load its append-only file:\n%s", log)
	}
	for _, bad := range []string{"NOPERM", "CRITICAL", "Bad file format", "Unrecoverable error"} {
		if strings.Contains(string(log), bad) {
			t.Errorf("Redis's restart log has %q:\n%s", bad, log)
		}
	}
	sq = serveStore(addr)
	after, _ := sq.R.Keys(ctx, "*").Result()
	slices.Sort(after)
	if !slices.Equal(after, before) {
		t.Errorf("keys after the restart:\n%v\nbefore:\n%v", after, before)
	}
	if !slices.ContainsFunc(after, func(k string) bool { return strings.HasPrefix(k, "devbot:auth:grant:") }) {
		t.Errorf("no grant after the restart: %v", after)
	}
	if evs, _, err := sq.Events(ctx, "", 10); err != nil || len(evs) != 1 || evs[0].Type != "task.working" {
		t.Errorf("events after the restart: %v %v", evs, err)
	}
	if stats, _ := sq.R.HGetAll(ctx, "devbot:stats").Result(); stats["events|task.working"] != "1" {
		t.Errorf("stats after the restart: %v", stats)
	}
	if queued, err := sq.IsQueued(ctx, queue.KindImplement, "gitea:acme/app#1"); err != nil || !queued {
		t.Errorf("the job after the restart: %v %v", queued, err)
	}
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if id, err := broker(sq).Authenticate(r); err != nil || id.Login != "alice" {
		t.Errorf("the token after the restart: %+v %v", id, err)
	}
}

// signIn runs the authorization code flow at the broker at resource as
// login, through the fake forge's login page, and returns the access token.
func signIn(t *testing.T, f *fake.Forge, resource, login string) string {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(req *http.Request) (*http.Response, string) {
		t.Helper()
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	post := func(path, contentType, body string) (*http.Response, string) {
		req, _ := http.NewRequest("POST", resource+path, strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Origin", resource)
		return do(req)
	}
	const redirect = "http://127.0.0.1/callback"
	resp, body := post("/oauth/register", "application/json",
		`{"redirect_uris":["`+redirect+`"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	var reg struct {
		ClientID string `json:"client_id"`
	}
	if json.Unmarshal([]byte(body), &reg); resp.StatusCode != 201 || reg.ClientID == "" {
		t.Fatalf("register: %d %s", resp.StatusCode, body)
	}
	verifier := oauth2.GenerateVerifier()
	q := url.Values{"client_id": {reg.ClientID}, "redirect_uri": {redirect}, "response_type": {"code"}, "state": {"s"},
		"code_challenge": {oauth2.S256ChallengeFromVerifier(verifier)}, "code_challenge_method": {"S256"}}
	req, _ := http.NewRequest("GET", resource+"/oauth/authorize?"+q.Encode(), nil)
	resp, body = do(req)
	cb, err := f.SignIn(resp.Header.Get("Location"), login)
	if err != nil {
		t.Fatalf("authorize: %v: %d %s", err, resp.StatusCode, body)
	}
	req, _ = http.NewRequest("GET", cb, nil)
	resp, body = do(req)
	m := regexp.MustCompile(`name="consent" value="([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the callback should ask for consent: %d %s", resp.StatusCode, body)
	}
	resp, body = post("/oauth/consent", "application/x-www-form-urlencoded", url.Values{"consent": {m[1]}, "decision": {"allow"}}.Encode())
	back, _ := url.Parse(resp.Header.Get("Location"))
	code := back.Query().Get("code")
	if code == "" {
		t.Fatalf("consent: %d %s %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	resp, body = post("/oauth/token", "application/x-www-form-urlencoded", url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {redirect}, "client_id": {reg.ClientID}, "code_verifier": {verifier}}.Encode())
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal([]byte(body), &tok); resp.StatusCode != 200 || tok.AccessToken == "" {
		t.Fatalf("token: %d %s", resp.StatusCode, body)
	}
	return tok.AccessToken
}

// Each role's NetworkPolicy lets its pods reach the MCP servers that role
// is given whose URL names an address, on its port. A hostname needs
// networkPolicy.allow, and a server the role doesn't use gets no rule.
func TestChartNetworkPolicyAllowsMCPServers(t *testing.T) {
	objs := mustRender(t, base+`
config:
  mcp_servers:
    context7: {type: http, url: "https://mcp.example.com/context7/mcp"}
    playwright: {type: http, url: "http://10.0.0.5:8931/mcp"}
    searxng: {type: http, url: "https://203.0.113.10/mcp"}
    v6: {type: http, url: "http://[fd00::5]:9000/mcp"}
    unused: {type: http, url: "http://10.9.9.9/mcp"}
  agents:
    implement: {mcp_servers: [context7, playwright, v6]}
    review: {mcp_servers: [playwright, searxng]}
`)
	want := map[string][][]string{
		"implement": {{"cidr:10.0.0.5/32", "port:8931"}, {"cidr:fd00::5/128", "port:9000"}},
		"review":    {{"cidr:10.0.0.5/32", "port:8931"}, {"cidr:203.0.113.10/32", "port:443"}},
	}
	for kind, w := range want {
		var got [][]string
		for _, r := range egress(t, objs["NetworkPolicy/devbot-"+kind]) {
			if len(r) == 2 && strings.HasPrefix(r[0], "cidr:") && r[0] != "cidr:160.79.104.0/23" {
				got = append(got, r)
			}
		}
		if !slices.EqualFunc(got, w, slices.Equal) {
			t.Errorf("%s: MCP egress %q, want %q", kind, got, w)
		}
	}
}

// Job pods request what a job really uses and spread across nodes, so a
// burst of jobs doesn't pile onto one.
func TestChartJobPodScheduling(t *testing.T) {
	cases := []struct {
		name, jobs string
		// spread is the constraints' labelSelector, nil for no constraints.
		spread   any
		affinity any
	}{
		{name: "defaults", spread: obj{
			"matchLabels": obj{"app.kubernetes.io/name": "devbot", "app.kubernetes.io/instance": "devbot"},
			"matchExpressions": []any{obj{"key": "app.kubernetes.io/component", "operator": "In",
				"values": []any{"implement", "review"}}},
		}},
		{name: "no spread with affinity", jobs: `
  topologySpreadConstraints: []
  affinity: {nodeAffinity: {requiredDuringSchedulingIgnoredDuringExecution: {nodeSelectorTerms: [{matchExpressions: [{key: kubernetes.io/hostname, operator: NotIn, values: [node-a]}]}]}}}`,
			affinity: "node-a"},
		{name: "own selector", jobs: `
  topologySpreadConstraints: [{maxSkew: 1, topologyKey: kubernetes.io/hostname, whenUnsatisfiable: ScheduleAnyway, labelSelector: {matchLabels: {x: "y"}}}]`,
			spread: obj{"matchLabels": obj{"x": "y"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			values := base
			if c.jobs != "" {
				values += "jobs:" + c.jobs + "\n"
			}
			objs := mustRender(t, values)
			for _, kind := range []string{"implement", "review"} {
				spec := dig(objs["ScaledJob/devbot-"+kind], "spec", "jobTargetRef", "template", "spec").(obj)
				res := container(t, spec, "agent")["resources"]
				if cpu := dig(res, "requests", "cpu"); cpu != "2" {
					t.Errorf("%s agent requests cpu %v", kind, cpu)
				}
				if cpu := dig(res, "limits", "cpu"); cpu != "4" {
					t.Errorf("%s agent's cpu limit is %v", kind, cpu)
				}

				tsc, _ := spec["topologySpreadConstraints"].([]any)
				if c.spread == nil {
					if tsc != nil {
						t.Errorf("%s pods have topologySpreadConstraints %v", kind, tsc)
					}
				} else if len(tsc) != 1 {
					t.Errorf("%s pods have topologySpreadConstraints %v", kind, tsc)
				} else {
					got := tsc[0].(obj)
					if got["topologyKey"] != "kubernetes.io/hostname" || got["maxSkew"] != 1 || got["whenUnsatisfiable"] != "ScheduleAnyway" {
						t.Errorf("%s pods' constraint: %v", kind, got)
					}
					a, _ := yaml.Marshal(got["labelSelector"])
					b, _ := yaml.Marshal(c.spread)
					if string(a) != string(b) {
						t.Errorf("%s pods' labelSelector:\n%s\nwant:\n%s", kind, a, b)
					}
				}

				aff, _ := yaml.Marshal(spec["affinity"])
				if (spec["affinity"] != nil) != (c.affinity != nil) || c.affinity != nil && !strings.Contains(string(aff), c.affinity.(string)) {
					t.Errorf("%s pods' affinity: %s", kind, aff)
				}
			}
		})
	}
}

// services: serve gets the Role to run service pods (and their
// NetworkPolicies) and nothing more, the service pods a ServiceAccount
// with no token and a policy that admits no one by itself, job pods a way
// to reach them, and serve the settings it makes them with, from the
// values job pods already have.
func TestChartServices(t *testing.T) {
	objs := mustRender(t, base+`
services: {enabled: true, readyTimeout: 3m, resources: {limits: {cpu: "2", memory: 2Gi}}}
jobs: {runtimeClassName: gvisor, tolerations: [{key: devbot, operator: Exists}]}
config: {repos: {limits: {services: {images: [registry.example.com/]}}}}
`)
	rules, _ := yaml.Marshal(objs["Role/devbot-serve"]["rules"])
	want := "- apiGroups:\n    - \"\"\n  resources:\n    - pods\n  verbs:\n    - get\n    - patch\n    - list\n    - create\n    - delete\n" +
		"- apiGroups:\n    - networking.k8s.io\n  resources:\n    - networkpolicies\n  verbs:\n    - list\n    - create\n    - delete\n"
	if string(rules) != want {
		t.Errorf("serve's Role allows\n%s", rules)
	}
	if sa := objs["ServiceAccount/devbot-services"]; sa == nil || sa["automountServiceAccountToken"] != false {
		t.Errorf("the services' ServiceAccount: %v", sa)
	}
	np := objs["NetworkPolicy/devbot-services"]
	if dig(np, "spec", "podSelector", "matchLabels", "app.kubernetes.io/component") != "services" || len(dig(np, "spec", "ingress").([]any)) != 0 {
		t.Errorf("the service pods' policy: %v", np["spec"])
	}
	if r := egress(t, np); len(r) != 1 || r[0][0] != "pods:k8s-app=kube-dns" {
		t.Errorf("service pods reach %q", r)
	}
	for _, kind := range []string{"implement", "review"} {
		if r := egress(t, objs["NetworkPolicy/devbot-"+kind]); !slices.ContainsFunc(r, func(rule []string) bool {
			return slices.Contains(rule, "pods:app.kubernetes.io/component=services") && slices.Contains(rule, "pods:app.kubernetes.io/instance=devbot")
		}) {
			t.Errorf("%s pods can't reach service pods: %q", kind, r)
		}
	}
	var cfg obj
	yaml.Unmarshal([]byte(dig(objs["ConfigMap/devbot-config"], "data", "config.yaml").(string)), &cfg)
	svc := cfg["services"].(obj)
	for k, want := range map[string]any{"enabled": true, "instance": "devbot", "ready_timeout": "3m", "service_account": "devbot-services",
		"user_namespaces": true, "network_policy": true, "runtime_class_name": "gvisor"} {
		if svc[k] != want {
			t.Errorf("config.services.%s = %v, want %v", k, svc[k], want)
		}
	}
	if dig(svc, "active_deadline", "implement") != "4200s" || dig(svc, "active_deadline", "review") != "1800s" ||
		dig(svc, "resources", "limits", "memory") != "2Gi" || dig(svc, "resources", "requests", "cpu") != "100m" || svc["tolerations"] == nil {
		t.Errorf("config.services: %v", svc)
	}
	if dig(objs["Deployment/devbot"], "spec", "template", "spec", "automountServiceAccountToken") != true {
		t.Error("serve has no token to run service pods with")
	}
	// The policy repeats the image allow-list and the job pods' runtime.
	var exprs []string
	for _, v := range dig(objs["ValidatingAdmissionPolicy/default-devbot-serve-pods"], "spec", "validations").([]any) {
		exprs = append(exprs, strings.Join(strings.Fields(dig(v, "expression").(string)), " "))
	}
	for _, want := range []string{`["registry.example.com/"].exists(p, c.image == p || (p.endsWith("/") || p.endsWith(":") || p.endsWith("@")) && c.image.startsWith(p))`, `object.spec.runtimeClassName == "gvisor"`,
		`object.spec.hostUsers == false`, `variables.update || variables.servicePod`} {
		if !slices.ContainsFunc(exprs, func(e string) bool { return strings.Contains(e, want) }) {
			t.Errorf("no validation says %s", want)
		}
	}
	if objs["ValidatingAdmissionPolicy/default-devbot-serve-networkpolicies"] == nil || objs["ValidatingAdmissionPolicyBinding/default-devbot-serve-networkpolicies"] == nil {
		t.Error("no policy for serve's NetworkPolicies")
	}
	// Placement reads the RuntimeClass, whose scheduling admission adds to
	// the pod, and is refused when it's gone.
	if dig(objs["ValidatingAdmissionPolicy/default-devbot-serve-placement"], "spec", "paramKind", "kind") != "RuntimeClass" ||
		dig(objs["ValidatingAdmissionPolicyBinding/default-devbot-serve-placement"], "spec", "paramRef", "name") != "gvisor" ||
		dig(objs["ValidatingAdmissionPolicyBinding/default-devbot-serve-placement"], "spec", "paramRef", "parameterNotFoundAction") != "Deny" {
		t.Errorf("placement with RuntimeClass gvisor: %v %v", objs["ValidatingAdmissionPolicy/default-devbot-serve-placement"], objs["ValidatingAdmissionPolicyBinding/default-devbot-serve-placement"])
	}
	objs = mustRender(t, base+"services: {enabled: true}\nconfig: {repos: {limits: {services: {images: [\"*\"]}}}}\n")
	if objs["ValidatingAdmissionPolicy/default-devbot-serve-placement"] == nil || dig(objs["ValidatingAdmissionPolicy/default-devbot-serve-placement"], "spec", "paramKind") != nil ||
		dig(objs["ValidatingAdmissionPolicyBinding/default-devbot-serve-placement"], "spec", "paramRef") != nil {
		t.Errorf("placement without a RuntimeClass: %v", objs["ValidatingAdmissionPolicy/default-devbot-serve-placement"])
	}

	// "*" allows every image: no image rule, the pod-shape rules as before.
	// Beside prefixes the chart refuses it, as serve's config would.
	objs = mustRender(t, base+"services: {enabled: true}\njobs: {runtimeClassName: gvisor}\nconfig: {repos: {limits: {services: {images: [\"*\"]}}}}\n")
	exprs = nil
	for _, v := range dig(objs["ValidatingAdmissionPolicy/default-devbot-serve-pods"], "spec", "validations").([]any) {
		exprs = append(exprs, strings.Join(strings.Fields(dig(v, "expression").(string)), " "))
	}
	if slices.ContainsFunc(exprs, func(e string) bool { return strings.Contains(e, "c.image.startsWith") }) {
		t.Errorf("an image rule with images [\"*\"]: %q", exprs)
	}
	for _, want := range []string{`object.spec.runtimeClassName == "gvisor"`, `object.spec.hostUsers == false`, `variables.update || variables.servicePod`,
		`!has(object.spec.nodeName)`, `c.ports.all(p, (!has(p.hostPort)`, `!has(c.envFrom)`} {
		if !slices.ContainsFunc(exprs, func(e string) bool { return strings.Contains(e, want) }) {
			t.Errorf("with images [\"*\"], no validation says %s", want)
		}
	}
	if _, err := render(t, base+"services: {enabled: true}\nconfig: {repos: {limits: {services: {images: [\"*\", registry.example.com/]}}}}\n"); err == nil || !strings.Contains(err.Error(), "list it alone") {
		t.Errorf("\"*\" beside a prefix: %v", err)
	}
	// A prefix has to stop where a name does, as serve's config insists.
	for _, bad := range []string{"ghcr.io/acme", "registry.example.com"} {
		if _, err := render(t, base+"services: {enabled: true}\nconfig: {repos: {limits: {services: {images: [registry.example.com/, "+bad+"]}}}}\n"); err == nil || !strings.Contains(err.Error(), "end a prefix with /, : or @") {
			t.Errorf("images entry %s: %v", bad, err)
		}
	}
	for _, good := range []string{"ghcr.io/acme/db:", "ghcr.io/acme/db:16", "ghcr.io/acme/db@sha256:abc"} {
		if _, err := render(t, base+"services: {enabled: true}\nconfig: {repos: {limits: {services: {images: [\""+good+"\"]}}}}\n"); err != nil {
			t.Errorf("images entry %s: %v", good, err)
		}
	}

	// Without labelling, the Role has no patch; without NetworkPolicies,
	// nothing for them.
	objs = mustRender(t, base+"services: {enabled: true}\npodLabels: {enabled: false}\nnetworkPolicy: {enabled: false}\n")
	rules, _ = yaml.Marshal(objs["Role/devbot-serve"]["rules"])
	if string(rules) != "- apiGroups:\n    - \"\"\n  resources:\n    - pods\n  verbs:\n    - get\n    - list\n    - create\n    - delete\n" {
		t.Errorf("serve's Role allows\n%s", rules)
	}
	if objs["ValidatingAdmissionPolicy/default-devbot-serve-networkpolicies"] != nil || objs["NetworkPolicy/devbot-services"] != nil {
		t.Error("NetworkPolicy objects without networkPolicy")
	}

	// Off by default: none of it.
	objs = mustRender(t, base)
	if objs["ServiceAccount/devbot-services"] != nil || objs["NetworkPolicy/devbot-services"] != nil {
		t.Error("services off still renders their objects")
	}
	yaml.Unmarshal([]byte(dig(objs["ConfigMap/devbot-config"], "data", "config.yaml").(string)), &cfg)
	if dig(cfg, "services", "enabled") != false {
		t.Errorf("config.services: %v", cfg["services"])
	}

	// A cluster without the admission API can't hold serve to service pods.
	if _, err := render(t, base+"services: {enabled: true}\npodLabels: {enabled: false}\n", "--api-versions", "v1/Pod", "--api-versions", "keda.sh/v1alpha1/ScaledJob"); err == nil || !strings.Contains(err.Error(), "services.enabled") {
		t.Errorf("want the chart to refuse, got %v", err)
	}
}

// Each role's agent runs the image of its harness, the CLI in it: codex's is
// published, claude's is the deployment's own, and nothing falls back to the
// base image, which has neither. serve, the harness and the model proxy run
// the base image.
func TestChartAgentImages(t *testing.T) {
	images := func(objs map[string]obj, kind string) (agent, harness, proxy string) {
		spec := jobSpec(t, objs, "devbot-"+kind)
		return container(t, spec, "agent")["image"].(string), container(t, spec, "harness")["image"].(string),
			container(t, spec, "model-proxy")["image"].(string)
	}
	codex := base + `
config:
  agents: {implement: {harness: codex}, review: {harness: codex}}
`
	objs := mustRender(t, codex+"image: {agents: {claude: {repository: \"\"}}}\n")
	for _, kind := range []string{"implement", "review"} {
		agent, harness, proxy := images(objs, kind)
		if agent != "devbot-codex:0.1.0" || harness != "devbot:0.1.0" || proxy != "devbot:0.1.0" {
			t.Errorf("codex %s: agent %s, harness %s, model proxy %s", kind, agent, harness, proxy)
		}
		args := container(t, jobSpec(t, objs, "devbot-"+kind), "agent")["args"].([]any)
		if len(args) != 2 || args[0] != "agent" || args[1] != kind {
			t.Errorf("codex %s: the agent's args are %v", kind, args)
		}
	}
	if img := dig(objs["Deployment/devbot"], "spec", "template", "spec", "containers").([]any)[0].(obj)["image"]; img != "devbot:0.1.0" {
		t.Errorf("serve runs %v", img)
	}

	// The published chart's names, and one tag for all of them.
	objs = mustRender(t, codex+`
image:
  repository: docker.io/robertlestak/devbot
  tag: v1.2.3
  agents: {codex: {repository: docker.io/robertlestak/devbot-codex}}
`)
	if agent, harness, _ := images(objs, "implement"); agent != "docker.io/robertlestak/devbot-codex:v1.2.3" || harness != "docker.io/robertlestak/devbot:v1.2.3" {
		t.Errorf("published: agent %s, harness %s", agent, harness)
	}

	// A role on claude, without an image with Claude Code: the chart says
	// where to read how to build one, for that role.
	for _, c := range []struct{ name, values, role string }{
		{"default harness", base + "image: {agents: {claude: {repository: \"\"}}}\n", "implement"},
		{"review on claude", base + `
image: {agents: {claude: {repository: ""}}}
config:
  agents: {implement: {harness: codex}, review: {harness: claude}}
`, "review"},
	} {
		_, err := render(t, c.values)
		want := "image.agents.claude.repository: the " + c.role + " role runs harness claude"
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "Anthropic's terms") ||
			!strings.Contains(err.Error(), "docs/operating.md#agent-images") {
			t.Errorf("%s: want an error naming %q and the build docs, got %v", c.name, want, err)
		}
	}
	if _, err := render(t, codex+"image: {agents: {codex: {repository: \"\"}}}\n"); err == nil || !strings.Contains(err.Error(), "image.agents.codex.repository") {
		t.Errorf("codex without an image: %v", err)
	}

	// The deployment's own claude image, and tag.
	objs = mustRender(t, base+`
image:
  repository: registry.example.com/ai/devbot
  tag: abc12345
  agents: {claude: {repository: registry.example.com/ai/devbot-claude, tag: abc12345-2.1.299}}
config:
  agents: {implement: {harness: claude}, review: {harness: codex}}
`)
	if agent, harness, _ := images(objs, "implement"); agent != "registry.example.com/ai/devbot-claude:abc12345-2.1.299" || harness != "registry.example.com/ai/devbot:abc12345" {
		t.Errorf("custom claude: agent %s, harness %s", agent, harness)
	}
	if agent, _, _ := images(objs, "review"); agent != "devbot-codex:abc12345" {
		t.Errorf("custom claude: the codex role's agent %s", agent)
	}
}

// Claude Code never updates itself in a job pod: the agent sidecar of a
// role on claude has DISABLE_AUTOUPDATER, and passes it on to the CLI.
func TestChartClaudeAutoUpdaterOff(t *testing.T) {
	objs := mustRender(t, base+`
agentEnv: {GOPROXY: https://proxy.example.com, DISABLE_AUTOUPDATER: "0"}
config:
  agents: {implement: {harness: claude}, review: {harness: codex}}
`)
	agent := container(t, jobSpec(t, objs, "devbot-implement"), "agent")
	if v := envValue(agent, "DISABLE_AUTOUPDATER"); v != "1" {
		t.Errorf("claude: DISABLE_AUTOUPDATER=%v", v)
	}
	if v := envValue(agent, "DEVBOT_PASS_ENV"); v != "DISABLE_AUTOUPDATER,GOPROXY" {
		t.Errorf("claude: DEVBOT_PASS_ENV=%v", v)
	}
	agent = container(t, jobSpec(t, objs, "devbot-review"), "agent")
	if v := envValue(agent, "DISABLE_AUTOUPDATER"); v != "0" {
		t.Errorf("codex: DISABLE_AUTOUPDATER=%v, want agentEnv's", v)
	}
}

// imagePullSecrets reaches every pod the chart runs, as the pod spec has
// it, and serve's config for the service pods it makes; the policy holding
// serve's service pods lets those names through and no other. No container
// mounts or reads one: only the kubelet does.
func TestChartImagePullSecrets(t *testing.T) {
	pods := map[string][]string{
		"Deployment/devbot":           {"spec", "template", "spec"},
		"Deployment/devbot-redis":     {"spec", "template", "spec"},
		"DaemonSet/devbot-tun-plugin": {"spec", "template", "spec"},
	}
	for _, sj := range []string{"implement", "review", "implement-docker", "review-docker"} {
		pods["ScaledJob/devbot-"+sj] = []string{"spec", "jobTargetRef", "template", "spec"}
	}
	pullRender := func(values string) (map[string]obj, obj) {
		objs := mustRender(t, dind+"services: {enabled: true}\n"+values)
		var cfg obj
		yaml.Unmarshal([]byte(dig(objs["ConfigMap/devbot-config"], "data", "config.yaml").(string)), &cfg)
		return objs, cfg
	}
	pullRule := func(objs map[string]obj) string {
		for _, v := range dig(objs["ValidatingAdmissionPolicy/default-devbot-serve-pods"], "spec", "validations").([]any) {
			if e := dig(v, "expression").(string); strings.Contains(e, "imagePullSecrets") {
				return strings.Join(strings.Fields(e), " ")
			}
		}
		t.Fatal("no validation reads imagePullSecrets")
		return ""
	}

	objs, cfg := pullRender("")
	for name, path := range pods {
		if objs[name] == nil {
			t.Fatalf("no %s", name)
		}
		if spec := dig(objs[name], path...).(obj); spec["imagePullSecrets"] != nil {
			t.Errorf("%s has imagePullSecrets by default: %v", name, spec["imagePullSecrets"])
		}
	}
	if dig(cfg, "services", "image_pull_secrets") != nil {
		t.Errorf("config.services.image_pull_secrets by default: %v", dig(cfg, "services", "image_pull_secrets"))
	}
	if r := pullRule(objs); !strings.Contains(r, "s.name in []") {
		t.Errorf("by default the policy lets serve name pull Secrets: %s", r)
	}

	// Either shape, {name: x} or x.
	for _, values := range []string{"imagePullSecrets: [{name: regcred}]\n", "imagePullSecrets: [regcred]\n"} {
		objs, cfg = pullRender(values)
		for name, path := range pods {
			spec := dig(objs[name], path...).(obj)
			if got, _ := yaml.Marshal(spec["imagePullSecrets"]); string(got) != "- name: regcred\n" {
				t.Errorf("%s: %s: imagePullSecrets\n%s", values, name, got)
			}
			for _, key := range []string{"initContainers", "containers"} {
				cs, _ := spec[key].([]any)
				for _, c := range cs {
					b, _ := json.Marshal(c)
					if strings.Contains(string(b), "regcred") {
						t.Errorf("%s: %s's container %s reads the pull secret: %s", values, name, dig(c, "name"), b)
					}
				}
			}
		}
		if got := fmt.Sprint(dig(cfg, "services", "image_pull_secrets")); got != "[regcred]" {
			t.Errorf("%s: config.services.image_pull_secrets: %s", values, got)
		}
		if r := pullRule(objs); !strings.Contains(r, `s.name in ["regcred"]`) {
			t.Errorf("%s: the policy: %s", values, r)
		}
	}

	// config.services set outright: the policy follows what serve reads.
	objs, _ = pullRender("imagePullSecrets: [regcred]\nconfig: {services: {enabled: true, instance: devbot, image_pull_secrets: [ci-images]}}\n")
	if r := pullRule(objs); !strings.Contains(r, `s.name in ["ci-images"]`) {
		t.Errorf("with config.services: the policy: %s", r)
	}

	for _, bad := range []string{"imagePullSecrets: [{name: \"\"}]\n", "imagePullSecrets: [{secret: regcred}]\n", "imagePullSecrets: regcred\n"} {
		if _, err := render(t, base+bad); err == nil || !strings.Contains(err.Error(), "imagePullSecrets") {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

// Implement pods take parents' integration checks too, and those without a
// Docker daemon their plan jobs, from lists of their own, and KEDA starts one
// for each item of any; review pods don't.
func TestChartImplementPodsRunParentJobs(t *testing.T) {
	objs := mustRender(t, base+"docker: {mode: dind, dind: {acceptLooserSandbox: true}}\n")
	for name, want := range map[string][]string{
		"devbot-implement":        {"devbot:q:implement", "devbot:q:integrate", "devbot:q:plan"},
		"devbot-implement-docker": {"devbot:q:implement-docker", "devbot:q:integrate-docker"},
		"devbot-review":           {"devbot:q:review"},
		"devbot-review-docker":    {"devbot:q:review-docker"},
	} {
		sj := objs["ScaledJob/"+name]
		var lists []string
		for _, trig := range dig(sj, "spec", "triggers").([]any) {
			lists = append(lists, dig(trig, "metadata", "listName").(string))
		}
		if !slices.Equal(lists, want) {
			t.Errorf("%s scales on %v, want %v", name, lists, want)
		}
		if got := dig(sj, "spec", "scalingStrategy", "multipleScalersCalculation"); got != "sum" {
			t.Errorf("%s: multipleScalersCalculation is %v, want sum", name, got)
		}
	}
}

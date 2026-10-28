package deploy

import (
	"bytes"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// configYAML is the config.yaml a render's ConfigMap carries.
func configYAML(t *testing.T, objs map[string]obj, release string) obj {
	t.Helper()
	var cfg obj
	if err := yaml.Unmarshal([]byte(dig(objs["ConfigMap/"+release+"-config"], "data", "config.yaml").(string)), &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Whatever the release is called, serve, the harness and KEDA dial its own
// Redis, and an address of one's own is left as it is.
func TestChartRedisAddrFollowsRelease(t *testing.T) {
	for _, release := range []string{"devbot", "myrel"} {
		objs, err := renderAs(t, release, base, "--namespace", "ai")
		if err != nil {
			t.Fatal(err)
		}
		if got := dig(configYAML(t, objs, release), "redis", "addr"); got != release+"-redis:6379" {
			t.Errorf("%s: config.redis.addr is %v", release, got)
		}
		if objs["Service/"+release+"-redis"] == nil {
			t.Errorf("%s: no Service %s-redis", release, release)
		}
		for _, kind := range []string{"implement", "review"} {
			trig := dig(objs["ScaledJob/"+release+"-"+kind], "spec", "triggers").([]any)[0]
			if got, want := dig(trig, "metadata", "address"), release+"-redis.ai.svc.cluster.local:6379"; got != want {
				t.Errorf("%s: KEDA dials %v, want %s", release, got, want)
			}
		}
	}

	// KEDA resolves names from its own namespace, so only it gets a
	// single-label host qualified with the release's.
	for addr, keda := range map[string]string{
		"devbot-redis:6379":      "devbot-redis.ai.svc.cluster.local:6379",
		"redis.example.com:6379": "redis.example.com:6379",
		"10.0.0.5:6379":          "10.0.0.5:6379",
	} {
		objs, err := renderAs(t, "myrel", base+"config: {redis: {addr: \""+addr+"\"}}\nredis: {enabled: false, existingSecret: my-redis}\n", "--namespace", "ai")
		if err != nil {
			t.Fatal(err)
		}
		if got := dig(configYAML(t, objs, "myrel"), "redis", "addr"); got != addr {
			t.Errorf("%s: config.redis.addr is %v", addr, got)
		}
		for _, kind := range []string{"implement", "review"} {
			sj := objs["ScaledJob/myrel-"+kind]
			if got := dig(sj, "spec", "triggers").([]any)[0]; dig(got, "metadata", "address") != keda {
				t.Errorf("%s: %s's KEDA dials %v, want %s", addr, kind, dig(got, "metadata", "address"), keda)
			}
		}
	}
	objs, err := renderAs(t, "myrel", base+"config: {redis: {sentinel: {master: m, addrs: [\"s1:26379\"]}}}\nredis: {enabled: false, existingSecret: my-redis}\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := dig(configYAML(t, objs, "myrel"), "redis", "addr"); got != nil {
		t.Errorf("through Sentinel: config.redis.addr is %v", got)
	}
}

// selectors is every pod selector a render has, by where it is: workloads',
// Services', PDBs', topology spreads', NetworkPolicies' own and the pods
// their rules name in the namespace.
func selectors(objs map[string]obj) map[string]map[string]any {
	out := map[string]map[string]any{}
	add := func(where string, sel any) {
		if m, ok := sel.(obj); ok && len(m) > 0 {
			out[where] = m
		}
	}
	for k, o := range objs {
		switch o["kind"] {
		case "Deployment", "DaemonSet", "StatefulSet", "PodDisruptionBudget":
			add(k, dig(o, "spec", "selector", "matchLabels"))
		case "Service":
			add(k, dig(o, "spec", "selector"))
		case "NetworkPolicy":
			add(k, dig(o, "spec", "podSelector", "matchLabels"))
			for _, dir := range []string{"egress", "ingress"} {
				rules, _ := dig(o, "spec", dir).([]any)
				for i, r := range rules {
					peers, _ := r.(obj)["to"].([]any)
					for j, p := range peers {
						// Another namespace's pods (kube-dns) aren't a release's.
						if p.(obj)["namespaceSelector"] == nil {
							add(fmt.Sprintf("%s %s[%d].to[%d]", k, dir, i, j), dig(p, "podSelector", "matchLabels"))
						}
					}
				}
			}
		}
		for _, spec := range []any{dig(o, "spec", "template", "spec"), dig(o, "spec", "jobTargetRef", "template", "spec")} {
			tsc, _ := dig(spec, "topologySpreadConstraints").([]any)
			for i, c := range tsc {
				add(fmt.Sprintf("%s topologySpread[%d]", k, i), dig(c, "labelSelector", "matchLabels"))
			}
		}
	}
	return out
}

// pods is the labels of every pod a render makes, by its workload.
func pods(objs map[string]obj) map[string]map[string]any {
	out := map[string]map[string]any{}
	for k, o := range objs {
		for _, l := range []any{dig(o, "spec", "template", "metadata", "labels"), dig(o, "spec", "jobTargetRef", "template", "metadata", "labels")} {
			if m, ok := l.(obj); ok {
				out[k] = m
			}
		}
	}
	return out
}

func matches(sel, labels map[string]any) bool {
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// Two releases in one namespace: every selector of each carries its
// instance, so none of them picks the other's pods, while each workload
// and Service still picks its own.
func TestChartReleasesDontSelectEachOther(t *testing.T) {
	values := base + `
serve: {replicas: 2}
services: {enabled: true}
docker: {mode: dind, dind: {acceptLooserSandbox: true}}
`
	renders := map[string]map[string]obj{}
	for _, release := range []string{"one", "two"} {
		objs, err := renderAs(t, release, values)
		if err != nil {
			t.Fatal(err)
		}
		renders[release] = objs
	}
	for release, objs := range renders {
		other := map[string]string{"one": "two", "two": "one"}[release]
		own, theirs := pods(objs), pods(renders[other])
		sels := selectors(objs)
		for where, sel := range sels {
			if sel["app.kubernetes.io/instance"] != release {
				t.Errorf("%s: %s doesn't select by instance: %v", release, where, sel)
			}
			for pod, labels := range theirs {
				if matches(sel, labels) {
					t.Errorf("%s: %s selects %s's %s pods", release, where, other, pod)
				}
			}
		}
		for _, k := range []string{"Deployment/" + release, "Deployment/" + release + "-redis", "Service/" + release, "Service/" + release + "-redis",
			"PodDisruptionBudget/" + release, "DaemonSet/" + release + "-tun-plugin"} {
			sel := sels[k]
			if sel == nil {
				t.Fatalf("%s: no selector for %s", release, k)
			}
			if !slices.ContainsFunc(slices.Collect(maps.Values(own)), func(l map[string]any) bool { return matches(sel, l) }) {
				t.Errorf("%s: %s selects none of its own pods: %v", release, k, sel)
			}
		}
	}
}

// serve's port follows config.listen: the container's, which its probes and
// the Service reach by name.
func TestChartListenPort(t *testing.T) {
	for _, c := range []struct {
		values string
		port   int
	}{{"", 8080}, {"config: {listen: \":3000\"}\n", 3000}, {"config: {listen: \"0.0.0.0:8443\"}\n", 8443}} {
		objs := mustRender(t, base+c.values)
		serve := container(t, dig(objs["Deployment/devbot"], "spec", "template", "spec").(obj), "serve")
		ports := map[string]any{}
		for _, p := range serve["ports"].([]any) {
			ports[p.(obj)["name"].(string)] = p.(obj)["containerPort"]
		}
		if ports["http"] != c.port || ports["metrics"] != 9090 {
			t.Errorf("listen %q: ports %v, want http %d", c.values, ports, c.port)
		}
		for _, probe := range []string{"readinessProbe", "livenessProbe"} {
			if p := dig(serve, probe, "httpGet", "port"); p != "http" && p != c.port {
				t.Errorf("listen %q: %s on port %v", c.values, probe, p)
			}
		}
		svc := dig(objs["Service/devbot"], "spec", "ports").([]any)[0].(obj)
		if tp := svc["targetPort"]; tp != "http" && tp != c.port {
			t.Errorf("listen %q: the Service targets %v", c.values, tp)
		}
	}
}

// The image is multi-arch, so job pods, like serve and Redis, go on any node
// unless the install says otherwise.
func TestChartNoArchByDefault(t *testing.T) {
	objs := mustRender(t, base+"docker: {mode: dind, dind: {acceptLooserSandbox: true}}\n")
	for k, o := range objs {
		for _, spec := range []any{dig(o, "spec", "template", "spec"), dig(o, "spec", "jobTargetRef", "template", "spec")} {
			if ns := dig(spec, "nodeSelector"); ns != nil {
				t.Errorf("%s has nodeSelector %v", k, ns)
			}
		}
	}
	if ns := dig(configYAML(t, objs, "devbot"), "services", "node_selector"); ns != nil {
		t.Errorf("service pods have node_selector %v", ns)
	}
}

// A first install without a forge, or with a value the chart doesn't have,
// is refused with what to fix, before anything reaches the cluster.
func TestChartRefusesBadValues(t *testing.T) {
	for _, c := range []struct{ values, fails string }{
		{"config: {providers: []}\n", "config.providers"},
		{"config: {providers: null}\n", "config.providers"},
		{"jobs: {nodeselector: {a: b}}\n", "'nodeselector' not allowed"},
		{"serve: {replicas: two}\n", "/serve/replicas"},
		{"config: {providers: [{name: gitea, type: gitlab}]}\n", "/config/providers/0/type"},
		{"ingress: {enabled: true}\n", "ingress.host"},
	} {
		if _, err := render(t, base+c.values); err == nil || !strings.Contains(err.Error(), c.fails) {
			t.Errorf("%q: want an error naming %s, got %v", c.values, c.fails, err)
		}
	}
}

// The schema holds every value the chart and its examples set, and lint,
// which renders the defaults, has nothing to say about any of them.
func TestChartLint(t *testing.T) {
	examples, _ := filepath.Glob("helm/devbot/values.example-*.yaml")
	if len(examples) != 2 {
		t.Fatalf("examples: %v", examples)
	}
	for _, args := range [][]string{{}, {"-f", examples[0]}, {"-f", examples[1]}} {
		if _, err := exec.LookPath("helm"); err != nil {
			t.Skip("helm isn't on the PATH")
		}
		out, err := exec.Command("helm", append([]string{"lint", "--strict", "helm/devbot"}, args...)...).CombinedOutput()
		if err != nil || strings.Contains(string(out), "[INFO]") || strings.Contains(string(out), "[WARNING]") || strings.Contains(string(out), "level=") {
			t.Errorf("helm lint %v: %v\n%s", args, err, out)
		}
	}
	for _, f := range examples {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var v obj
		yaml.Unmarshal(b, &v)
		if v["image"] != nil {
			t.Errorf("%s sets image, which the released chart already has", f)
		}
		if !strings.Contains(string(b), "--wait=legacy") || !strings.Contains(string(b), "oci://registry-1.docker.io/") {
			t.Errorf("%s's install command isn't docs/operating.md's", f)
		}
	}
	var chart obj
	b, _ := os.ReadFile("helm/devbot/Chart.yaml")
	yaml.Unmarshal(b, &chart)
	for _, k := range []string{"kubeVersion", "home", "sources", "maintainers", "keywords", "icon"} {
		if chart[k] == nil {
			t.Errorf("Chart.yaml has no %s", k)
		}
	}
}

// What a stranger sees after helm install: where serve's webhooks, MCP and
// UI are, the Secrets to create with their keys, and how to check it.
func TestChartNotes(t *testing.T) {
	notes := func(values string) string {
		t.Helper()
		out, err := helm(t, []string{"install", "myrel", "helm/devbot", "--dry-run=client", "--namespace", "ai"}, base+values)
		if err != nil {
			t.Fatal(err)
		}
		_, n, ok := strings.Cut(string(out), "\nNOTES:\n")
		if !ok {
			t.Fatalf("no NOTES:\n%s", out)
		}
		return n
	}
	n := notes("")
	for _, want := range []string{
		"Webhook:   http://myrel.ai.svc.cluster.local/webhooks/gitea",
		"MCP:       http://myrel.ai.svc.cluster.local/mcp",
		"in the cluster only",
		"devbot-forge: DEVBOT_REVIEW_TOKEN, DEVBOT_TOKEN, GITEA_WEBHOOK_SECRET",
		"devbot-claude: CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY",
		"kubectl -n ai rollout status deployment/myrel",
		"kubectl -n ai get scaledjobs -l app.kubernetes.io/instance=myrel",
		"curl -fsS http://localhost:8080/readyz",
	} {
		if !strings.Contains(n, want) {
			t.Errorf("default NOTES have no %q:\n%s", want, n)
		}
	}
	if strings.Contains(n, "/ui/") {
		t.Errorf("NOTES name a UI that's off:\n%s", n)
	}

	// A second secrets: in one document would replace base's, so these name theirs again.
	n = notes(`
ui: {enabled: true}
ingress: {enabled: true, host: devbot.example.com}
config:
  agents: {review: {harness: codex}}
  auth: {mode: oauth, client_id_env: CID, client_secret_env: CSECRET, key_env: KEY}
secrets: {forge: devbot-forge, agents: {claude: devbot-claude, codex: devbot-codex}, auth: devbot-auth}
redis: {existingSecret: my-redis}
`)
	for _, want := range []string{
		"Webhook:   https://devbot.example.com/webhooks/gitea",
		"MCP:       https://devbot.example.com/mcp",
		"Web UI:    https://devbot.example.com/ui/",
		"devbot-claude: CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY",
		"devbot-codex: CODEX_API_KEY",
		"devbot-auth: CID, CSECRET, KEY",
		"my-redis: serve-password, harness-password, keda-password",
	} {
		if !strings.Contains(n, want) {
			t.Errorf("NOTES with an ingress have no %q:\n%s", want, n)
		}
	}
	if strings.Contains(n, "in the cluster only") {
		t.Errorf("NOTES say an exposed serve is in the cluster only:\n%s", n)
	}

	if n := notes("virtualService: {enabled: true, host: devbot.example.com, prefix: /devbot/}\n"); !strings.Contains(n, "MCP:       https://devbot.example.com/devbot/mcp") {
		t.Errorf("NOTES behind a VirtualService prefix:\n%s", n)
	}
	if n := notes(""); !strings.Contains(n, "WARNING: networkPolicy") {
		t.Errorf("NOTES don't warn that job pods can't reach the forge:\n%s", n)
	}
	if n := notes("networkPolicy: {allow: [{namespace: gitea}]}\n"); strings.Contains(n, "WARNING") {
		t.Errorf("NOTES warn with nothing wrong:\n%s", n)
	}
}

// Off by default; on, a plain Ingress at the root of its host, to serve's
// Service, with the class, annotations and certificate it is given.
func TestChartIngress(t *testing.T) {
	objs := mustRender(t, base)
	for k := range objs {
		if strings.HasPrefix(k, "Ingress/") || strings.HasPrefix(k, "VirtualService/") {
			t.Errorf("%s by default", k)
		}
	}
	objs = mustRender(t, base+`
ingress:
  enabled: true
  className: nginx
  host: devbot.example.com
  annotations: {cert-manager.io/cluster-issuer: letsencrypt}
  tls: {secretName: devbot-tls}
`)
	ing := objs["Ingress/devbot"]
	if ing == nil {
		t.Fatal("no Ingress")
	}
	if dig(ing, "spec", "ingressClassName") != "nginx" || dig(ing, "metadata", "annotations", "cert-manager.io/cluster-issuer") != "letsencrypt" {
		t.Errorf("Ingress: %v", ing)
	}
	tls := dig(ing, "spec", "tls").([]any)[0].(obj)
	if tls["secretName"] != "devbot-tls" || !slices.Equal(tls["hosts"].([]any), []any{"devbot.example.com"}) {
		t.Errorf("tls: %v", tls)
	}
	rule := dig(ing, "spec", "rules").([]any)[0].(obj)
	path := dig(rule, "http", "paths").([]any)[0].(obj)
	if rule["host"] != "devbot.example.com" || path["path"] != "/" || path["pathType"] != "Prefix" ||
		dig(path, "backend", "service", "name") != "devbot" || dig(path, "backend", "service", "port", "name") != "http" {
		t.Errorf("rule: %v", rule)
	}
	objs = mustRender(t, base+"ingress: {enabled: true, host: devbot.example.com}\n")
	if spec := dig(objs["Ingress/devbot"], "spec").(obj); spec["tls"] != nil || spec["ingressClassName"] != nil {
		t.Errorf("an Ingress with neither class nor certificate: %v", spec)
	}

	// OAuth wants serve at the root of its host.
	objs = mustRender(t, base+"virtualService: {enabled: true, host: devbot.example.com}\n")
	match := dig(objs["VirtualService/devbot"], "spec", "http").([]any)[0].(obj)["match"].([]any)[0]
	if p := dig(match, "uri", "prefix"); p != "/" {
		t.Errorf("the VirtualService's prefix is %v", p)
	}
}

// In auth.mode none serve answers /api, /mcp and /ui only at allowed_hosts,
// so the Ingress's public host is among them, as the VirtualService's is; a
// wildcard names no one host and is left out.
func TestChartIngressHostAllowed(t *testing.T) {
	hosts := func(values string) []any {
		t.Helper()
		h, _ := configYAML(t, mustRender(t, base+values), "devbot")["allowed_hosts"].([]any)
		return h
	}
	if h := hosts("ingress: {enabled: true, host: devbot.example.com}\n"); !slices.Contains(h, any("devbot.example.com")) {
		t.Errorf("allowed_hosts with an Ingress: %v", h)
	}
	if h := hosts("ingress: {enabled: false, host: devbot.example.com}\n"); slices.Contains(h, any("devbot.example.com")) {
		t.Errorf("allowed_hosts without an Ingress: %v", h)
	}
	if h := hosts("ingress: {enabled: true, host: '*.example.com'}\n"); slices.Contains(h, any("*.example.com")) {
		t.Errorf("allowed_hosts with a wildcard Ingress: %v", h)
	}
}

// Without a cluster helm knows no kinds, so the chart can't check for KEDA;
// with one (here --api-versions), it refuses where there's none.
func TestChartNeedsKEDA(t *testing.T) {
	_, err := render(t, base, "--api-versions", "v1/Pod", "--api-versions", "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy")
	if err == nil || !strings.Contains(err.Error(), "KEDA 2.16 or later") {
		t.Errorf("a cluster without KEDA: %v", err)
	}
	if _, err := render(t, base, "--api-versions", "v1/Pod", "--api-versions", "keda.sh/v1alpha1/ScaledJob",
		"--api-versions", "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy"); err != nil {
		t.Errorf("a cluster with KEDA: %v", err)
	}
}

// kubeconform, strict, over what the chart renders: by default, from each
// example, and with everything optional on. Only where kubeconform is on
// the PATH; CRDs' schemas come from the datreeio catalog.
func TestChartKubeconform(t *testing.T) {
	if _, err := exec.LookPath("kubeconform"); err != nil {
		t.Skip("kubeconform isn't on the PATH")
	}
	examples, _ := filepath.Glob("helm/devbot/values.example-*.yaml")
	everything := `
docker: {mode: dind, dind: {acceptLooserSandbox: true}}
services: {enabled: true}
ui: {enabled: true}
ingress: {enabled: true, host: devbot.example.com, className: nginx, tls: {secretName: devbot-tls}}
virtualService: {enabled: true, host: devbot.example.com}
serve: {replicas: 2}
redis: {persistence: {enabled: true}}
`
	cases := map[string]string{"defaults": "", "everything": base + everything}
	for _, f := range examples {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		cases[filepath.Base(f)] = string(b)
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := helm(t, []string{"template", "devbot", "helm/devbot"}, values)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("kubeconform", "-strict", "-summary", "-schema-location", "default",
				"-schema-location", "https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json", "-")
			cmd.Stdin = bytes.NewReader(out)
			if res, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(res), "Invalid: 0, Errors: 0, Skipped: 0") {
				t.Errorf("%v\n%s", err, res)
			}
		})
	}
}

// crd is a CustomResourceDefinition for one of KEDA's kinds that keeps
// whatever it's given, with the pod template's appArmorProfile, as KEDA
// 2.16's has, or without it, as older ones are.
func crd(kind, plural string, appArmor bool) obj {
	open := func(props obj) obj {
		o := obj{"type": "object", "x-kubernetes-preserve-unknown-fields": true}
		if props != nil {
			o["properties"] = props
		}
		return o
	}
	schema := open(nil)
	if appArmor {
		schema = open(obj{"spec": open(obj{"jobTargetRef": open(obj{"template": open(obj{"spec": open(obj{
			"securityContext": open(obj{"appArmorProfile": open(nil)})})})})})})
	}
	return obj{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": obj{"name": plural + ".keda.sh"},
		"spec": obj{"group": "keda.sh", "scope": "Namespaced",
			"names":    obj{"kind": kind, "plural": plural, "singular": strings.ToLower(kind), "listKind": kind + "List"},
			"versions": []any{obj{"name": "v1alpha1", "served": true, "storage": true, "schema": obj{"openAPIV3Schema": schema}}}}}
}

// Against a real API server, as helm install does: without KEDA the chart
// refuses, naming the version it needs, and with docker.mode it reads
// KEDA's ScaledJob CRD and refuses one that would drop the Docker
// sidecar's appArmorProfile.
func TestChartKEDALive(t *testing.T) {
	s := startAPIServer(t, nil)
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	os.WriteFile(kubeconfig, []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters: [{name: test, cluster: {server: %q, insecure-skip-tls-verify: true}}]
users: [{name: admin, user: {token: admin-token}}]
contexts: [{name: test, context: {cluster: test, user: admin, namespace: default}}]
current-context: test
`, s.url)), 0o600)
	install := func(values string) error {
		t.Helper()
		_, err := helm(t, []string{"install", "devbot", "helm/devbot", "--dry-run=server", "--kubeconfig", kubeconfig}, base+values)
		return err
	}
	const docker = "docker: {mode: dind, dind: {acceptLooserSandbox: true}}\n"

	if err := install(""); err == nil || !strings.Contains(err.Error(), "KEDA 2.16 or later") {
		t.Fatalf("without KEDA: %v", err)
	}

	const crds = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions"
	for _, c := range []obj{crd("ScaledJob", "scaledjobs", false), crd("TriggerAuthentication", "triggerauthentications", false)} {
		if code, body := s.do(t, http.MethodPost, crds, c); code != http.StatusCreated {
			t.Fatalf("creating %s: %d %s", dig(c, "metadata", "name"), code, body)
		}
	}
	// Until discovery serves keda.sh, helm still finds no KEDA.
	deadline := time.Now().Add(30 * time.Second)
	for {
		code, body := s.do(t, http.MethodGet, "/apis/keda.sh/v1alpha1", nil)
		if code == http.StatusOK && strings.Contains(body, "scaledjobs") && strings.Contains(body, "triggerauthentications") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("keda.sh/v1alpha1 never served")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := install(""); err != nil {
		t.Errorf("with KEDA, without Docker: %v", err)
	}
	if err := install(docker); err == nil || !strings.Contains(err.Error(), "appArmorProfile") || !strings.Contains(err.Error(), "KEDA to 2.16") {
		t.Errorf("Docker on a KEDA older than 2.16: %v", err)
	}

	code, body := s.do(t, http.MethodGet, crds+"/scaledjobs.keda.sh", nil)
	if code != http.StatusOK {
		t.Fatalf("reading the CRD: %d %s", code, body)
	}
	var cur obj
	yaml.Unmarshal([]byte(body), &cur)
	newer := crd("ScaledJob", "scaledjobs", true)
	newer["metadata"].(obj)["resourceVersion"] = dig(cur, "metadata", "resourceVersion")
	if code, body := s.do(t, http.MethodPut, crds+"/scaledjobs.keda.sh", newer); code != http.StatusOK {
		t.Fatalf("updating the CRD: %d %s", code, body)
	}
	if err := install(docker); err != nil {
		t.Errorf("Docker on KEDA 2.16: %v", err)
	}
}

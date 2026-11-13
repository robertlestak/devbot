package deploy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/fake"
	"git.shdw.tech/rob/devbot/internal/kube"
	"git.shdw.tech/rob/devbot/internal/queue"
	"git.shdw.tech/rob/devbot/internal/server"
)

// Every placement value is set here rather than left to the chart's defaults,
// so each placement refusal below has something to differ from.
const servicesValues = base + `
services: {enabled: true}
imagePullSecrets: [{name: regcred}]
jobs:
  nodeSelector: {kubernetes.io/arch: amd64}
  tolerations: [{key: devbot.io/jobs, operator: Equal, value: "true", effect: NoSchedule}]
  affinity: {nodeAffinity: {preferredDuringSchedulingIgnoredDuringExecution: [{weight: 10, preference: {matchExpressions: [{key: devbot.io/jobs, operator: Exists}]}}]}}
config:
  repos:
    limits:
      services: {images: [registry.example.com/, docker.io/openbao/]}
`

const serveToken = "serve-token"

// The chart's RBAC and admission policies for services, on a real API
// server, with serve's own code making the pods: what serve creates for two
// runs at once is allowed, each run's NetworkPolicy admits its own job pod
// alone, both go when the runs end, and serve can create nothing else.
func TestChartServicesLive(t *testing.T) {
	s, srv, e := startServicesLive(t, servicesValues, "")
	ctx := context.Background()
	srv.RunServices(ctx)
	srv.RunServices(ctx)
	for _, pod := range []string{"devbot-implement-a", "devbot-review-b"} {
		st, ok, _ := e.Q.Services(ctx, pod)
		if !ok || st.State != queue.ServicesStarting {
			t.Fatalf("%s's services: %+v (%v)", pod, st, ok)
		}
		sp := s.pod(t, pod+"-services")
		if dig(sp, "metadata", "ownerReferences") == nil || dig(sp, "spec", "hostUsers") != false {
			t.Errorf("%s's service pod: %v", pod, sp["metadata"])
		}
		code, body := s.do(t, http.MethodGet, "/apis/networking.k8s.io/v1/namespaces/default/networkpolicies/"+pod+"-services", nil)
		if code != http.StatusOK || !strings.Contains(body, `"batch.kubernetes.io/controller-uid":"0f3a-`+pod+`"`) {
			t.Errorf("%s's NetworkPolicy: %d %s", pod, code, body)
		}
	}

	// The pods serve made run where job pods do.
	sp := s.pod(t, "devbot-review-b-services")
	if dig(sp, "spec", "nodeSelector", "kubernetes.io/arch") != "amd64" || dig(sp, "spec", "affinity", "nodeAffinity") == nil ||
		!strings.Contains(string(mustJSON(t, dig(sp, "spec", "tolerations"))), `"key":"devbot.io/jobs"`) {
		t.Errorf("a service pod's placement: %v", sp["spec"])
	}
	// With the install's pull secret, which the policy lets through.
	if got := string(mustJSON(t, dig(sp, "spec", "imagePullSecrets"))); got != `[{"name":"regcred"}]` {
		t.Errorf("a service pod's imagePullSecrets: %s", got)
	}
	if code, body := s.as(t, serveToken, http.MethodPost, "/api/v1/namespaces/default/pods", with(servicePodObj("pull", "registry.example.com/x:1"), "spec", "imagePullSecrets",
		[]any{obj{"name": "regcred"}})); code != http.StatusCreated {
		t.Errorf("serve: a service pod with the configured pull secret: %d %s", code, body)
	}
	s.do(t, http.MethodDelete, "/api/v1/namespaces/default/pods/pull", nil)

	// One run ends: its lease goes, then its job pod finishes, and its pod and
	// policy go, the other's stay, and the chart's policy for every service
	// pod is left alone. Until the job pod finishes serve keeps them, as it
	// would for a lease lost to a Redis blip; envtest has no kubelet to
	// finish it, so the test sets its phase.
	e.Q.Release(ctx, "implement", "fake:acme/app#1", "devbot-implement-a")
	srv.RunServices(ctx)
	for _, still := range []string{"/api/v1/namespaces/default/pods/devbot-implement-a-services", "/apis/networking.k8s.io/v1/namespaces/default/networkpolicies/devbot-implement-a-services"} {
		if code, _ := s.do(t, http.MethodGet, still, nil); code != http.StatusOK {
			t.Errorf("with its job pod still running, %s: %d", still, code)
		}
	}
	jp := s.pod(t, "devbot-implement-a")
	jp["status"] = obj{"phase": "Succeeded"}
	if code, body := s.do(t, http.MethodPut, "/api/v1/namespaces/default/pods/devbot-implement-a/status", jp); code != http.StatusOK {
		t.Fatalf("finishing the job pod: %d %s", code, body)
	}
	srv.RunServices(ctx)
	if code, _ := s.do(t, http.MethodGet, "/api/v1/namespaces/default/pods/devbot-implement-a-services", nil); code != http.StatusNotFound {
		t.Errorf("an ended run's service pod: %d", code)
	}
	if code, _ := s.do(t, http.MethodGet, "/apis/networking.k8s.io/v1/namespaces/default/networkpolicies/devbot-implement-a-services", nil); code != http.StatusNotFound {
		t.Errorf("an ended run's NetworkPolicy: %d", code)
	}
	for _, still := range []string{"/api/v1/namespaces/default/pods/devbot-review-b-services", "/apis/networking.k8s.io/v1/namespaces/default/networkpolicies/devbot-services"} {
		if code, _ := s.do(t, http.MethodGet, still, nil); code != http.StatusOK {
			t.Errorf("%s: %d", still, code)
		}
	}

	// What serve's client never sends, as serve.
	denied := []struct {
		name, method, path string
		body               any
		why                string
	}{
		{"an image the install doesn't allow", "POST", "/api/v1/namespaces/default/pods", servicePodObj("x1", "docker.io/evil/miner:1"), "images"},
		{"a pod that isn't a service pod", "POST", "/api/v1/namespaces/default/pods", testPod("x2", map[string]any{"app.kubernetes.io/instance": "devbot", "app.kubernetes.io/component": "implement"}), "service pods"},
		{"another ServiceAccount", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x3", "registry.example.com/x:1"), "spec", "serviceAccountName", "devbot-serve"), "no API token"},
		{"a token", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x4", "registry.example.com/x:1"), "spec", "automountServiceAccountToken", true), "no API token"},
		{"the node's network", "POST", "/api/v1/namespaces/default/pods", with(with(servicePodObj("x5", "registry.example.com/x:1"), "spec", "hostUsers", nil), "spec", "hostPID", true), "host namespaces"},
		{"a volume", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x6", "registry.example.com/x:1"), "spec", "volumes", []any{obj{"name": "h", "hostPath": obj{"path": "/"}}}), "volumes"},
		{"a capability", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x7", "registry.example.com/x:1"), "spec", "containers", []any{obj{"name": "c", "image": "registry.example.com/x:1",
			"securityContext": obj{"allowPrivilegeEscalation": false, "capabilities": obj{"add": []any{"SYS_ADMIN"}}}}}), "added capabilities"},
		{"no owner", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x8", "registry.example.com/x:1"), "metadata", "ownerReferences", nil), "owned by its run's Job"},
		{"outside the user namespace", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x9", "registry.example.com/x:1"), "spec", "hostUsers", true), "user namespace"},
		{"a Secret in the env", "POST", "/api/v1/namespaces/default/pods", withContainer(servicePodObj("x10", "registry.example.com/x:1"), "env",
			[]any{obj{"name": "K", "valueFrom": obj{"secretKeyRef": obj{"name": "devbot", "key": "anthropic"}}}}), "literal values"},
		{"a Secret as the env", "POST", "/api/v1/namespaces/default/pods", withContainer(servicePodObj("x11", "registry.example.com/x:1"), "envFrom",
			[]any{obj{"secretRef": obj{"name": "devbot"}}}), "literal values"},
		{"an image pull Secret", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x12", "registry.example.com/x:1"), "spec", "imagePullSecrets",
			[]any{obj{"name": "devbot"}}), "literal values"},
		{"another pull Secret beside the install's", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x12b", "registry.example.com/x:1"), "spec", "imagePullSecrets",
			[]any{obj{"name": "regcred"}, obj{"name": "devbot-forge"}}), "image pull Secrets"},
		{"a host port", "POST", "/api/v1/namespaces/default/pods", withContainer(servicePodObj("x13", "registry.example.com/x:1"), "ports",
			[]any{obj{"containerPort": 5432, "hostPort": 5432}}), "host ports"},
		{"a container without seccomp", "POST", "/api/v1/namespaces/default/pods", withContainer(servicePodObj("x14", "registry.example.com/x:1"), "securityContext",
			obj{"allowPrivilegeEscalation": false, "seccompProfile": obj{"type": "Unconfined"}}), "loosen seccomp"},
		{"a container without AppArmor", "POST", "/api/v1/namespaces/default/pods", withContainer(servicePodObj("x15", "registry.example.com/x:1"), "securityContext",
			obj{"allowPrivilegeEscalation": false, "appArmorProfile": obj{"type": "Unconfined"}}), "loosen seccomp"},
		{"an SELinux type", "POST", "/api/v1/namespaces/default/pods", withContainer(servicePodObj("x16", "registry.example.com/x:1"), "securityContext",
			obj{"allowPrivilegeEscalation": false, "seLinuxOptions": obj{"type": "spc_t"}}), "loosen seccomp"},
		{"an unmasked /proc", "POST", "/api/v1/namespaces/default/pods", withContainer(servicePodObj("x17", "registry.example.com/x:1"), "securityContext",
			obj{"allowPrivilegeEscalation": false, "procMount": "Unmasked"}), "loosen seccomp"},
		{"a sysctl", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x18", "registry.example.com/x:1"), "spec", "securityContext",
			obj{"seccompProfile": obj{"type": "RuntimeDefault"}, "sysctls": []any{obj{"name": "net.ipv4.ip_unprivileged_port_start", "value": "0"}}}), "loosen seccomp"},
		{"an HTTP probe from the node", "POST", "/api/v1/namespaces/default/pods", withContainer(servicePodObj("x19", "registry.example.com/x:1"), "readinessProbe",
			obj{"httpGet": obj{"host": "10.0.0.1", "port": 80}}), "host ports"},
		{"a lifecycle hook", "POST", "/api/v1/namespaces/default/pods", withContainer(servicePodObj("x20", "registry.example.com/x:1"), "lifecycle",
			obj{"preStop": obj{"httpGet": obj{"host": "10.0.0.1", "port": 80}}}), "host ports"},
		{"a node of its own", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x21", "registry.example.com/x:1"), "spec", "nodeName", "control-plane"), "pick its node"},
		{"a priority class", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x22", "registry.example.com/x:1"), "spec", "priorityClassName", "system-node-critical"), "pick its node"},
		{"a toleration of its own", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x23", "registry.example.com/x:1"), "spec", "tolerations",
			[]any{obj{"key": "node-role.kubernetes.io/control-plane", "operator": "Exists", "effect": "NoSchedule"}}), "job pods' nodeSelector"},
		{"tolerating everything", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x24", "registry.example.com/x:1"), "spec", "tolerations",
			[]any{obj{"operator": "Exists"}}), "job pods' nodeSelector"},
		{"another nodeSelector", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x25", "registry.example.com/x:1"), "spec", "nodeSelector",
			obj{"node-role.kubernetes.io/control-plane": ""}), "job pods' nodeSelector"},
		{"no nodeSelector", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x26", "registry.example.com/x:1"), "spec", "nodeSelector", nil), "job pods' nodeSelector"},
		{"another affinity", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x27", "registry.example.com/x:1"), "spec", "affinity",
			obj{"nodeAffinity": obj{"requiredDuringSchedulingIgnoredDuringExecution": obj{"nodeSelectorTerms": []any{
				obj{"matchExpressions": []any{obj{"key": "node-role.kubernetes.io/control-plane", "operator": "Exists"}}}}}}}), "job pods' nodeSelector"},
		{"no affinity", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x28", "registry.example.com/x:1"), "spec", "affinity", nil), "job pods' nodeSelector"},
		{"a scheduler of its own", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x29", "registry.example.com/x:1"), "spec", "schedulerName", "mine"), "default scheduler"},
		{"no resources", "POST", "/api/v1/namespaces/default/pods", withContainer(servicePodObj("x30", "registry.example.com/x:1"), "resources", obj{}), "ephemeral-storage limits"},
		{"no disk limit", "POST", "/api/v1/namespaces/default/pods", withContainer(servicePodObj("x31", "registry.example.com/x:1"), "resources",
			obj{"limits": obj{"cpu": "1", "memory": "1Gi"}}), "ephemeral-storage limits"},
		{"an AppArmor annotation", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x32", "registry.example.com/x:1"), "metadata", "annotations",
			obj{"container.apparmor.security.beta.kubernetes.io/c": "unconfined"}), "AppArmor or seccomp annotations"},
		{"a harmless AppArmor annotation", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x33", "registry.example.com/x:1"), "metadata", "annotations",
			obj{"container.apparmor.security.beta.kubernetes.io/c": "runtime/default"}), "AppArmor or seccomp annotations"},
		{"a seccomp annotation", "POST", "/api/v1/namespaces/default/pods", with(servicePodObj("x34", "registry.example.com/x:1"), "metadata", "annotations",
			obj{"container.seccomp.security.alpha.kubernetes.io/c": "unconfined"}), "AppArmor or seccomp annotations"},
		{"deleting a job pod", "DELETE", "/api/v1/namespaces/default/pods/devbot-review-b", nil, "service pods"},
		{"deleting the chart's policy", "DELETE", "/apis/networking.k8s.io/v1/namespaces/default/networkpolicies/devbot-services", nil, "service NetworkPolicies"},
		{"a policy opening other pods", "POST", "/apis/networking.k8s.io/v1/namespaces/default/networkpolicies", obj{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
			"metadata": obj{"name": "open", "labels": obj{"app.kubernetes.io/instance": "devbot", "app.kubernetes.io/component": "services", "devbot.io/run": "x"}},
			"spec":     obj{"podSelector": obj{"matchLabels": obj{"app.kubernetes.io/component": "redis"}}, "policyTypes": []any{"Ingress"}, "ingress": []any{obj{}}}}, "one run's service pod"},
	}
	for _, c := range denied {
		code, body := s.as(t, serveToken, c.method, c.path, c.body)
		if code != http.StatusForbidden || !strings.Contains(body, c.why) {
			t.Errorf("serve: %s: want denied (%s), got %d %s", c.name, c.why, code, body)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// servicePodObj is a service pod as the policy wants one, to vary.
func servicePodObj(name, image string) obj {
	return obj{"apiVersion": "v1", "kind": "Pod",
		"metadata": obj{"name": name, "labels": obj{"app.kubernetes.io/instance": "devbot", "app.kubernetes.io/component": "services", "devbot.io/run": "0f3a"},
			"ownerReferences": []any{obj{"apiVersion": "batch/v1", "kind": "Job", "name": "job-x", "uid": "0f3a"}}},
		"spec": obj{"serviceAccountName": "devbot-services", "automountServiceAccountToken": false, "hostUsers": false, "restartPolicy": "Never",
			"securityContext": obj{"seccompProfile": obj{"type": "RuntimeDefault"}},
			"nodeSelector":    obj{"kubernetes.io/arch": "amd64"},
			"tolerations":     []any{obj{"key": "devbot.io/jobs", "operator": "Equal", "value": "true", "effect": "NoSchedule"}},
			"affinity": obj{"nodeAffinity": obj{"preferredDuringSchedulingIgnoredDuringExecution": []any{
				obj{"weight": 10, "preference": obj{"matchExpressions": []any{obj{"key": "devbot.io/jobs", "operator": "Exists"}}}}}}},
			"containers": []any{obj{"name": "c", "image": image, "securityContext": obj{"allowPrivilegeEscalation": false},
				"resources": obj{"limits": obj{"cpu": "1", "memory": "1Gi", "ephemeral-storage": "2Gi"}}}}}}
}

func with(o obj, section, key string, v any) obj {
	if v == nil {
		delete(o[section].(obj), key)
	} else {
		o[section].(obj)[key] = v
	}
	return o
}

// as sends a request as the user token stands for.
func (s *apiServer) as(t *testing.T, token, method, path string, body any) (int, string) {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = strings.NewReader(string(mustJSON(t, body)))
	}
	req, _ := http.NewRequest(method, s.url+path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// withContainer sets a field of servicePodObj's one container.
func withContainer(o obj, key string, v any) obj {
	o["spec"].(obj)["containers"].([]any)[0].(obj)[key] = v
	return o
}

// A RuntimeClass with scheduling, as GKE Sandbox's gvisor is: the API server
// adds its nodeSelector and tolerations to serve's pods before the policies
// see them, and those, and only those, are let through on top of jobs.*.
func TestChartServicesRuntimeClassLive(t *testing.T) {
	s, srv, e := startServicesLive(t, strings.Replace(servicesValues, "jobs:\n", "jobs:\n  runtimeClassName: sandboxed\n", 1), "sandboxed")
	ctx := context.Background()
	srv.RunServices(ctx)
	sp := s.pod(t, "devbot-review-b-services")
	if dig(sp, "spec", "nodeSelector", "sandbox.example.com/runtime") != "sandboxed" || dig(sp, "spec", "nodeSelector", "kubernetes.io/arch") != "amd64" ||
		!strings.Contains(string(mustJSON(t, dig(sp, "spec", "tolerations"))), `"key":"sandbox.example.com/runtime"`) {
		t.Errorf("a service pod in RuntimeClass sandboxed: %v", sp["spec"])
	}
	if st, ok, _ := e.Q.Services(ctx, "devbot-implement-a"); !ok || st.State != queue.ServicesStarting {
		t.Errorf("devbot-implement-a's services: %+v (%v)", st, ok)
	}

	denied := []struct {
		name string
		body obj
	}{
		{"a toleration of its own", with(inRuntimeClass(servicePodObj("x1", "registry.example.com/x:1"), "sandboxed"), "spec", "tolerations",
			[]any{obj{"key": "node-role.kubernetes.io/control-plane", "operator": "Exists", "effect": "NoSchedule"}})},
		{"the RuntimeClass's toleration, wider", with(inRuntimeClass(servicePodObj("x2", "registry.example.com/x:1"), "sandboxed"), "spec", "tolerations",
			[]any{obj{"key": "sandbox.example.com/runtime", "operator": "Exists"}})},
		{"another nodeSelector", with(inRuntimeClass(servicePodObj("x3", "registry.example.com/x:1"), "sandboxed"), "spec", "nodeSelector",
			obj{"kubernetes.io/arch": "amd64", "node-role.kubernetes.io/control-plane": ""})},
	}
	for _, c := range denied {
		code, body := s.as(t, serveToken, http.MethodPost, "/api/v1/namespaces/default/pods", c.body)
		if code != http.StatusForbidden || !strings.Contains(body, "RuntimeClass sandboxed's scheduling") {
			t.Errorf("serve: %s: want denied, got %d %s", c.name, code, body)
		}
	}
}

// startServicesLive starts an API server with the chart's objects for
// services rendered from values, and RuntimeClass rc (with scheduling) when
// it's set; serve, outside the cluster and with the chart's config; and two
// job pods with leases asking for services, as the Job controller and the
// harness would leave them.
func startServicesLive(t *testing.T, values, rc string) (*apiServer, *server.Server, *core.Engine) {
	t.Helper()
	s := startAPIServer(t, map[string]string{serveToken: "system:serviceaccount:default:devbot-serve"})
	if rc != "" {
		if code, body := s.do(t, http.MethodPost, "/apis/node.k8s.io/v1/runtimeclasses", obj{"apiVersion": "node.k8s.io/v1", "kind": "RuntimeClass",
			"metadata": obj{"name": rc}, "handler": "runsc", "scheduling": obj{
				"nodeSelector": obj{"sandbox.example.com/runtime": rc},
				"tolerations":  []any{obj{"key": "sandbox.example.com/runtime", "operator": "Equal", "value": rc, "effect": "NoSchedule"}}}}); code != http.StatusCreated {
			t.Fatalf("creating RuntimeClass %s: %d %s", rc, code, body)
		}
	}
	objs := mustRender(t, values)
	paths := map[string]string{"NetworkPolicy": "/apis/networking.k8s.io/v1/namespaces/default/networkpolicies"}
	for _, k := range []string{"ValidatingAdmissionPolicy/default-devbot-serve-pods", "ValidatingAdmissionPolicyBinding/default-devbot-serve-pods",
		"ValidatingAdmissionPolicy/default-devbot-serve-placement", "ValidatingAdmissionPolicyBinding/default-devbot-serve-placement",
		"ValidatingAdmissionPolicy/default-devbot-serve-networkpolicies", "ValidatingAdmissionPolicyBinding/default-devbot-serve-networkpolicies",
		"ServiceAccount/devbot-serve", "ServiceAccount/devbot-services", "Role/devbot-serve", "RoleBinding/devbot-serve", "NetworkPolicy/devbot-services"} {
		if objs[k] == nil {
			t.Fatalf("the chart renders no %s", k)
		}
		if p, ok := paths[objs[k]["kind"].(string)]; ok {
			if code, body := s.do(t, http.MethodPost, p, objs[k]); code != http.StatusCreated {
				t.Fatalf("creating %s: %d %s", k, code, body)
			}
			continue
		}
		s.create(t, objs[k])
	}
	var cfgMap obj
	if err := json.Unmarshal(mustJSON(t, objs["ConfigMap/devbot-config"]), &cfgMap); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte(dig(cfgMap, "data", "config.yaml").(string)), 0o644)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}

	// Two job pods, as the Job controller would make them.
	jobPod := func(name, kind string) {
		uid := "0f3a-" + name
		o := testPod(name, map[string]any{"app.kubernetes.io/name": "devbot", "app.kubernetes.io/instance": "devbot", "app.kubernetes.io/component": kind,
			"batch.kubernetes.io/job-name": "job-" + name, "batch.kubernetes.io/controller-uid": uid})
		o["metadata"].(obj)["ownerReferences"] = []any{obj{"apiVersion": "batch/v1", "kind": "Job", "name": "job-" + name, "uid": uid, "controller": true}}
		s.create(t, o)
	}
	jobPod("devbot-implement-a", "implement")
	jobPod("devbot-review-b", "review")

	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte(serveToken), 0o600)
	k := &kube.Client{URL: s.url, Namespace: "default", TokenFile: tokenFile, HTTP: s.http}
	mr := miniredis.RunT(t)
	reg := forge.Registry{"fake": fake.New()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := &core.Engine{Forges: reg, Q: queue.New(mr.Addr(), 0), Cfg: cfg, Repos: &config.RepoCache{Forges: reg}, Log: log}
	// Outside a cluster, so New finds no in-cluster API for either.
	srv, err := server.New(e, func() *config.Config { c := *cfg; c.Services.Enabled, c.PodLabels.Enabled = false, false; return &c }(), log, "test")
	if err != nil {
		t.Fatal(err)
	}
	srv.Cfg, srv.ServicePods = cfg, k
	ctx := context.Background()
	svcs := []config.Service{
		{Name: "openbao", Image: "openbao/openbao:2.4.1", Env: map[string]string{"BAO_DEV_ROOT_TOKEN_ID": "root"}, Ports: []int{18237}, Ready: config.ServiceReady{TCP: 18237}},
		{Name: "pg", Image: "registry.example.com/ci-postgres:17", Ports: []int{54337}, Ready: config.ServiceReady{Exec: []string{"pg_isready", "-p", "54337"}}},
	}
	for _, l := range []struct{ kind, ref, pod string }{{"implement", "fake:acme/app#1", "devbot-implement-a"}, {"review", "fake:acme/app#2", "devbot-review-b"}} {
		e.Q.Acquire(ctx, l.kind, l.ref, l.pod, time.Minute)
		e.Q.Describe(ctx, l.kind, l.ref, l.pod, queue.LeaseDetail{Pod: l.pod, Services: svcs})
	}

	// The policies take a moment to load; until then the probes are let in.
	// One is each policy's alone to refuse.
	probes := []obj{inRuntimeClass(servicePodObj("probe", "evil:1"), rc),
		with(inRuntimeClass(servicePodObj("probe-placement", "registry.example.com/x:1"), rc), "spec", "tolerations", []any{obj{"operator": "Exists"}})}
	deadline := time.Now().Add(30 * time.Second)
	for _, probe := range probes {
		for {
			code, _ := s.as(t, serveToken, http.MethodPost, "/api/v1/namespaces/default/pods", probe)
			if code == http.StatusForbidden {
				break
			}
			if code == http.StatusCreated {
				s.do(t, http.MethodDelete, "/api/v1/namespaces/default/pods/"+dig(probe, "metadata", "name").(string), nil)
			}
			if time.Now().After(deadline) {
				t.Fatalf("the policies never denied serve %s", dig(probe, "metadata", "name"))
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	return s, srv, e
}

// inRuntimeClass puts a servicePodObj in RuntimeClass rc, if it's set.
func inRuntimeClass(o obj, rc string) obj {
	if rc == "" {
		return o
	}
	return with(o, "spec", "runtimeClassName", rc)
}

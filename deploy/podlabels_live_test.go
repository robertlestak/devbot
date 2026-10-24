package deploy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/kube"
)

// apiServer is a kube-apiserver of its own, on etcd of its own, that the
// test reaches as an admin.
type apiServer struct {
	url  string
	http *http.Client
}

// startAPIServer runs etcd and kube-apiserver from the PATH (envtest's
// binaries will do), and returns once the API server is ready. tokens are
// extra users, by token.
func startAPIServer(t *testing.T, tokens map[string]string) *apiServer {
	t.Helper()
	etcd, err1 := exec.LookPath("etcd")
	apiserver, err2 := exec.LookPath("kube-apiserver")
	if err1 != nil || err2 != nil {
		t.Skip("etcd or kube-apiserver isn't on the PATH")
	}
	dir := t.TempDir()
	start := func(name string, args ...string) {
		var log bytes.Buffer
		cmd := exec.Command(name, args...)
		cmd.Stdout, cmd.Stderr = &log, &log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cmd.Process.Kill()
			cmd.Wait()
			if t.Failed() {
				out := log.String()
				t.Logf("%s, the end of its log:\n%s", filepath.Base(name), out[max(0, len(out)-4096):])
			}
		})
	}
	etcdURL := "http://127.0.0.1:" + freePort(t)
	start(etcd, "--data-dir", filepath.Join(dir, "etcd"), "--listen-client-urls", etcdURL, "--advertise-client-urls", etcdURL,
		"--listen-peer-urls", "http://127.0.0.1:"+freePort(t))

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "sa.key")
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600)
	csv := "admin-token,admin,admin,system:masters\n"
	for token, user := range tokens {
		csv += token + "," + user + "," + user + "\n"
	}
	tokenFile := filepath.Join(dir, "tokens.csv")
	os.WriteFile(tokenFile, []byte(csv), 0o600)
	port := freePort(t)
	start(apiserver, "--etcd-servers", etcdURL, "--bind-address", "127.0.0.1", "--advertise-address", "127.0.0.1",
		"--secure-port", port, "--cert-dir", filepath.Join(dir, "certs"), "--token-auth-file", tokenFile,
		"--authorization-mode", "RBAC", "--service-cluster-ip-range", "10.0.0.0/24",
		"--service-account-issuer", "https://kubernetes.default.svc", "--service-account-key-file", keyFile,
		"--service-account-signing-key-file", keyFile, "--disable-admission-plugins", "ServiceAccount")

	s := &apiServer{url: "https://127.0.0.1:" + port, http: &http.Client{Timeout: 10 * time.Second,
		// Its own self-signed certificate, on loopback.
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if code, _ := s.do(t, http.MethodGet, "/readyz", nil); code == http.StatusOK {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatal("kube-apiserver didn't become ready")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (s *apiServer) do(t *testing.T, method, path string, body any) (int, string) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.url+path, r)
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// create creates a rendered object, by kind.
func (s *apiServer) create(t *testing.T, o obj) {
	t.Helper()
	paths := map[string]string{
		"ValidatingAdmissionPolicy":        "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicies",
		"ValidatingAdmissionPolicyBinding": "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicybindings",
		"ServiceAccount":                   "/api/v1/namespaces/default/serviceaccounts",
		"Role":                             "/apis/rbac.authorization.k8s.io/v1/namespaces/default/roles",
		"RoleBinding":                      "/apis/rbac.authorization.k8s.io/v1/namespaces/default/rolebindings",
		"Pod":                              "/api/v1/namespaces/default/pods",
	}
	if code, body := s.do(t, http.MethodPost, paths[o["kind"].(string)], o); code != http.StatusCreated {
		t.Fatalf("creating %s %s: %d %s", o["kind"], dig(o, "metadata", "name"), code, body)
	}
}

func (s *apiServer) pod(t *testing.T, name string) obj {
	t.Helper()
	code, body := s.do(t, http.MethodGet, "/api/v1/namespaces/default/pods/"+name, nil)
	if code != http.StatusOK {
		t.Fatalf("reading pod %s: %d %s", name, code, body)
	}
	var o obj
	json.Unmarshal([]byte(body), &o)
	return o
}

func testPod(name string, labels map[string]any) obj {
	return obj{"apiVersion": "v1", "kind": "Pod", "metadata": obj{"name": name, "labels": labels,
		"annotations": obj{"kubectl.kubernetes.io/default-container": "harness"}},
		"spec": obj{"containers": []any{obj{"name": "harness", "image": "devbot:1"}}}}
}

// The chart's admission policy, on a real API server: serve's ServiceAccount
// can put devbot's labels and annotations on this release's job pods, as
// serve's own client does, and nothing else to any pod.
func TestChartPodLabelsPolicyLive(t *testing.T) {
	const serveToken = "serve-token"
	s := startAPIServer(t, map[string]string{serveToken: "system:serviceaccount:default:devbot-serve"})
	objs := mustRender(t, base)
	for _, k := range []string{"ValidatingAdmissionPolicy/default-devbot-serve-pods", "ValidatingAdmissionPolicyBinding/default-devbot-serve-pods",
		"ServiceAccount/devbot-serve", "Role/devbot-serve", "RoleBinding/devbot-serve"} {
		if objs[k] == nil {
			t.Fatalf("the chart renders no %s", k)
		}
		s.create(t, objs[k])
	}
	job := map[string]any{"app.kubernetes.io/name": "devbot", "app.kubernetes.io/instance": "devbot", "app.kubernetes.io/component": "implement",
		"batch.kubernetes.io/job-name": "devbot-implement-x7k2p", "batch.kubernetes.io/controller-uid": "0f3a"}
	s.create(t, testPod("implement", job))
	s.create(t, testPod("review", map[string]any{"app.kubernetes.io/instance": "devbot", "app.kubernetes.io/component": "review"}))
	s.create(t, testPod("serve", map[string]any{"app.kubernetes.io/instance": "devbot", "app.kubernetes.io/component": "serve"}))
	s.create(t, testPod("other-release", map[string]any{"app.kubernetes.io/instance": "other", "app.kubernetes.io/component": "implement"}))
	s.create(t, testPod("unrelated", map[string]any{"app": "payments"}))

	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte(serveToken), 0o600)
	serve := &kube.Client{URL: s.url, Namespace: "default", TokenFile: tokenFile, HTTP: s.http}
	ctx := context.Background()
	attribution := kube.PodMeta{
		Labels:      map[string]string{"devbot.io/repo": "app", "devbot.io/owner": "acme", "labels.devbot.io/team": "payments"},
		Annotations: map[string]string{"devbot.io/repo": "acme/app", "devbot.io/origin": "connector:mcp"},
	}

	// The policy takes a moment to load; until it denies, nothing is checked.
	deadline := time.Now().Add(30 * time.Second)
	for serve.PatchPodMeta(ctx, "unrelated", kube.PodMeta{Labels: map[string]string{"devbot.io/repo": "probe"}}) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the policy never denied serve labelling an unrelated pod")
		}
		time.Sleep(200 * time.Millisecond)
	}
	// What the probes may have left before it loaded.
	s.do(t, http.MethodDelete, "/api/v1/namespaces/default/pods/unrelated", nil)
	s.create(t, testPod("unrelated", map[string]any{"app": "payments"}))

	// What serve does, twice: the second changes nothing, and is allowed too.
	for _, name := range []string{"implement", "review", "implement", "review"} {
		if err := serve.PatchPodMeta(ctx, name, attribution); err != nil {
			t.Errorf("serve labelling %s: %v", name, err)
		}
	}
	if m, err := serve.PodMeta(ctx, "implement"); err != nil || m.Labels["devbot.io/repo"] != "app" || m.Labels["labels.devbot.io/team"] != "payments" ||
		m.Labels["batch.kubernetes.io/job-name"] != "devbot-implement-x7k2p" || m.Annotations["devbot.io/origin"] != "connector:mcp" {
		t.Errorf("the implement pod after labelling: %+v %v", m, err)
	}
	// Changing a value it set before is fine.
	if err := serve.PatchPodMeta(ctx, "implement", kube.PodMeta{Labels: map[string]string{"devbot.io/repo": "app2"}}); err != nil {
		t.Errorf("serve relabelling: %v", err)
	}

	// Raw patches serve's client never sends, as serve.
	patch := func(token, name, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPatch, s.url+"/api/v1/namespaces/default/pods/"+name, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/merge-patch+json")
		resp, err := s.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	denied := []struct{ name, pod, patch, why string }{
		{"another release's pod", "other-release", `{"metadata":{"labels":{"devbot.io/repo":"app"}}}`, "implement and review pods"},
		{"serve's own pod", "serve", `{"metadata":{"labels":{"devbot.io/repo":"app"}}}`, "implement and review pods"},
		{"an unrelated pod", "unrelated", `{"metadata":{"labels":{"devbot.io/repo":"app"}}}`, "implement and review pods"},
		{"the image", "implement", `{"spec":{"containers":[{"name":"harness","image":"evil:1"}]}}`, "spec"},
		{"the deadline", "implement", `{"spec":{"activeDeadlineSeconds":1}}`, "spec"},
		{"the Job selector label", "implement", `{"metadata":{"labels":{"batch.kubernetes.io/job-name":"other"}}}`, "labels"},
		{"removing a label", "implement", `{"metadata":{"labels":{"batch.kubernetes.io/controller-uid":null}}}`, "labels"},
		{"the component", "implement", `{"metadata":{"labels":{"app.kubernetes.io/component":"review"}}}`, "labels"},
		{"a new label", "implement", `{"metadata":{"labels":{"team":"payments"}}}`, "labels"},
		{"a look-alike label", "implement", `{"metadata":{"labels":{"evil.devbot.io/x":"y"}}}`, "labels"},
		{"a new annotation", "implement", `{"metadata":{"annotations":{"example.com/x":"y"}}}`, "annotations"},
		{"an annotation removed", "implement", `{"metadata":{"annotations":{"kubectl.kubernetes.io/default-container":null}}}`, "annotations"},
		{"a finalizer", "implement", `{"metadata":{"finalizers":["example.com/hold"]}}`, "finalizers"},
		{"an owner", "implement", `{"metadata":{"ownerReferences":[{"apiVersion":"v1","kind":"ConfigMap","name":"x","uid":"6a2c"}]}}`, "owners"},
	}
	for _, c := range denied {
		code, body := patch(serveToken, c.pod, c.patch)
		if code != http.StatusForbidden || !strings.Contains(body, "ValidatingAdmissionPolicy") || !strings.Contains(body, c.why) {
			t.Errorf("serve patching %s: want denied by the policy (%s), got %d %s", c.name, c.why, code, body)
		}
	}
	if spec := dig(s.pod(t, "implement"), "spec", "containers").([]any); dig(spec[0], "image") != "devbot:1" {
		t.Errorf("the implement pod's image is %v", dig(spec[0], "image"))
	}

	// RBAC stops these first; the policy would too.
	req, _ := http.NewRequest(http.MethodDelete, s.url+"/api/v1/namespaces/default/pods/implement", nil)
	req.Header.Set("Authorization", "Bearer "+serveToken)
	if resp, err := s.http.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("serve deleting a job pod: %v %v", resp.Status, err)
	}

	// The policy binds serve alone: anyone else may still change the pod.
	if code, body := patch("admin-token", "implement", `{"metadata":{"labels":{"team":"payments"}}}`); code != http.StatusOK {
		t.Errorf("the admin labelling the pod: %s", body)
	}
}

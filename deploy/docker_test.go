package deploy

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const dind = base + `
docker:
  mode: dind
  dind: {acceptLooserSandbox: true}
`

func jobSpec(t *testing.T, objs map[string]obj, name string) obj {
	t.Helper()
	sj := objs["ScaledJob/"+name]
	if sj == nil {
		t.Fatalf("no ScaledJob %s", name)
	}
	return dig(sj, "spec", "jobTargetRef", "template", "spec").(obj)
}

func envValue(c obj, name string) any {
	env, _ := c["env"].([]any)
	for _, e := range env {
		if e.(obj)["name"] == name {
			return e.(obj)["value"]
		}
	}
	return nil
}

func hasContainer(spec obj, name string) bool {
	for _, key := range []string{"initContainers", "containers"} {
		cs, _ := spec[key].([]any)
		for _, c := range cs {
			if c.(obj)["name"] == name {
				return true
			}
		}
	}
	return false
}

// No daemon by default: no ScaledJobs for Docker repos, no sidecar, and
// serve is told there is none, so no job is queued where nothing takes it.
func TestChartDockerOffByDefault(t *testing.T) {
	objs := mustRender(t, base)
	for name := range objs {
		if strings.Contains(name, "docker") || strings.Contains(name, "dind") {
			t.Errorf("%s rendered without docker.mode", name)
		}
	}
	for _, kind := range []string{"implement", "review"} {
		if hasContainer(jobSpec(t, objs, "devbot-"+kind), "docker") {
			t.Errorf("%s pods have a Docker sidecar", kind)
		}
	}
	var cfg struct {
		Docker struct{ Mode *string } `yaml:"docker"`
	}
	yaml.Unmarshal([]byte(dig(objs["ConfigMap/devbot-config"], "data", "config.yaml").(string)), &cfg)
	if cfg.Docker.Mode == nil || *cfg.Docker.Mode != "" {
		t.Errorf("config.docker.mode: %v", cfg.Docker.Mode)
	}
}

// With docker.mode, Docker repos' jobs run from ScaledJobs of their own, on
// the Docker lists, and other repos' stay as they were. No container of a
// Docker job pod gets an API token, and nothing lets the pod reach the API
// server: the daemon's sandbox is looser, so nothing it can be talked into
// holds a credential.
func TestChartDockerRouting(t *testing.T) {
	objs := mustRender(t, dind)
	for _, kind := range []string{"implement", "review"} {
		plain := objs["ScaledJob/devbot-"+kind]
		if hasContainer(jobSpec(t, objs, "devbot-"+kind), "docker") || dig(plain, "spec", "jobTargetRef", "template", "metadata", "labels", "docker.devbot.io/mode") != nil {
			t.Errorf("the %s pods of repos without docker have a sidecar", kind)
		}
		if l := dig(plain["spec"].(obj)["triggers"].([]any)[0], "metadata", "listName"); l != "devbot:q:"+kind {
			t.Errorf("%s scales on %v", kind, l)
		}
		sj := objs["ScaledJob/devbot-"+kind+"-docker"]
		if l := dig(sj["spec"].(obj)["triggers"].([]any)[0], "metadata", "listName"); l != "devbot:q:"+kind+"-docker" {
			t.Errorf("%s-docker scales on %v", kind, l)
		}
		tmpl := dig(sj, "spec", "jobTargetRef", "template").(obj)
		spec := tmpl["spec"].(obj)
		if dig(tmpl, "metadata", "labels", "docker.devbot.io/mode") != "dind" || dig(tmpl, "metadata", "annotations", "eks.amazonaws.com/skip-containers") != "harness,agent,docker" {
			t.Errorf("%s-docker's metadata: %v", kind, tmpl["metadata"])
		}
		if spec["automountServiceAccountToken"] != false {
			t.Errorf("%s-docker: automountServiceAccountToken %v", kind, spec["automountServiceAccountToken"])
		}
		for _, v := range spec["volumes"].([]any) {
			if b, _ := json.Marshal(v); strings.Contains(string(b), "serviceAccountToken") {
				t.Errorf("%s-docker mounts an API token: %s", kind, b)
			}
		}
		for _, c := range append(spec["initContainers"].([]any), spec["containers"].([]any)...) {
			for _, m := range c.(obj)["volumeMounts"].([]any) {
				if strings.HasPrefix(m.(obj)["mountPath"].(string), "/var/run/secrets/kubernetes.io") {
					t.Errorf("%s mounts %v", c.(obj)["name"], m)
				}
			}
		}
		a := container(t, spec, "agent")
		if envValue(a, "DOCKER_HOST") != "unix:///devbot-docker/docker.sock" || envValue(a, "TESTCONTAINERS_RYUK_DISABLED") != "true" {
			t.Errorf("%s-docker's agent env: %v", kind, a["env"])
		}
		if pass := strings.Split(envValue(a, "DEVBOT_PASS_ENV").(string), ","); !slices.Contains(pass, "DOCKER_HOST") {
			t.Errorf("DOCKER_HOST doesn't reach the model's commands: %v", pass)
		}
		mount(t, a, "/devbot-docker")
		if envValue(container(t, spec, "harness"), "DEVBOT_DOCKER") != "true" {
			t.Errorf("%s-docker's harness doesn't take Docker jobs", kind)
		}
	}

	// Nothing connects in to a Docker pod, whose containers publish ports on
	// it: its role's policy, which every job pod has, takes no ingress. And
	// no policy lets a job pod reach the API server.
	if objs["NetworkPolicy/devbot-docker"] != nil {
		t.Error("Docker pods have a policy of their own, beside their role's")
	}
	for _, kind := range []string{"implement", "review"} {
		np := objs["NetworkPolicy/devbot-"+kind]
		if !slices.Contains(toStrings(dig(np, "spec", "policyTypes")), "Ingress") || dig(np, "spec", "ingress") != nil {
			t.Errorf("%s pods, Docker's too, take ingress: %v", kind, np["spec"])
		}
	}
	for name, o := range objs {
		if !strings.HasPrefix(name, "NetworkPolicy/") {
			continue
		}
		if got := flat(egress(t, o)); slices.Contains(got, "port:6443") {
			t.Errorf("%s reaches the API server: %v", name, got)
		}
	}
	for detect, want := range map[string]bool{"": true, "false": false} {
		args := []string{}
		if detect != "" {
			args = append(args, "--set", "docker.detect="+detect)
		}
		var cfg struct {
			Docker map[string]any `yaml:"docker"`
		}
		yaml.Unmarshal([]byte(dig(mustRender(t, dind, args...)["ConfigMap/devbot-config"], "data", "config.yaml").(string)), &cfg)
		if len(cfg.Docker) != 2 || cfg.Docker["mode"] != "dind" || cfg.Docker["detect"] != want {
			t.Errorf("docker.detect %q: serve's docker config: %v", detect, cfg.Docker)
		}
	}
}

// A rootless daemon in the pod: the job pod keeps its user namespace, no
// container is privileged, and the daemon's sandbox is loosened only as
// docs/operating.md lists.
func TestChartDockerDind(t *testing.T) {
	objs := mustRender(t, dind)
	if objs["ConfigMap/devbot-dind"] == nil {
		t.Fatal("no subordinate IDs for the daemon")
	}
	for _, kind := range []string{"implement", "review"} {
		spec := jobSpec(t, objs, "devbot-"+kind+"-docker")
		if spec["hostUsers"] != false {
			t.Errorf("%s-docker: hostUsers %v", kind, spec["hostUsers"])
		}
		for _, c := range append(spec["initContainers"].([]any), spec["containers"].([]any)...) {
			if dig(c, "securityContext", "privileged") == true {
				t.Errorf("%s is privileged", c.(obj)["name"])
			}
		}
		d := container(t, spec, "docker")
		got, _ := json.Marshal(d["securityContext"])
		const want = `{"allowPrivilegeEscalation":true,"appArmorProfile":{"type":"Unconfined"},"procMount":"Unmasked","runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"Unconfined"}}`
		if string(got) != want {
			t.Errorf("the daemon's securityContext:\n got %s\nwant %s", got, want)
		}
		// The socket alone: no TCP flag, port, or DOCKER_HOST the image's
		// entrypoint would turn into a listener.
		if a := strings.Join(toStrings(d["args"]), " "); a != "dockerd --host=unix:///devbot-docker/docker.sock" {
			t.Errorf("the daemon listens on more than the pod's socket: %s", a)
		}
		if b, _ := json.Marshal(d); d["ports"] != nil || d["command"] != nil || strings.Contains(string(b), "tcp://") || strings.Contains(string(b), "2375") || strings.Contains(string(b), "2376") ||
			envValue(d, "DOCKER_HOST") != nil || envValue(d, "DOCKER_TLS_CERTDIR") != nil {
			t.Errorf("the daemon may listen on TCP: %s", b)
		}
		// /dev/net/tun from the device plugin, on the daemon alone: a userns
		// pod can't idmap a hostPath device node.
		if dig(d, "resources", "limits", "devbot.io/tun") != 1 {
			t.Errorf("the daemon has no TUN device: %v", d["resources"])
		}
		for _, c := range append(spec["initContainers"].([]any), spec["containers"].([]any)...) {
			if n := c.(obj)["name"]; n != "docker" && dig(c, "resources", "limits", "devbot.io/tun") != nil {
				t.Errorf("%s claims a TUN device", n)
			}
			for _, m := range c.(obj)["volumeMounts"].([]any) {
				if m.(obj)["mountPath"] == "/dev/net/tun" {
					t.Errorf("%s mounts /dev/net/tun", c.(obj)["name"])
				}
			}
		}
		for _, v := range spec["volumes"].([]any) {
			if v.(obj)["hostPath"] != nil {
				t.Errorf("a hostPath volume: %v", v)
			}
		}
		// dockerd, as uid 1000, makes its data root in the emptyDir, whose
		// own root is root's.
		if m := mount(t, d, "/home/rootless/.local/share"); m["name"] != "docker-data" {
			t.Errorf("the daemon's data: %v", m)
		}
		// Ready once the API answers, not once the socket exists.
		if p := strings.Join(toStrings(dig(d, "startupProbe", "exec", "command")), " "); p != "docker --host=unix:///devbot-docker/docker.sock version" {
			t.Errorf("the daemon's startup probe: %s", p)
		}
		// A bind of a repo file names the agent's path; the daemon has the
		// same volume there, read-only.
		w, aw := mount(t, d, "/work"), mount(t, container(t, spec, "agent"), "/work")
		if w["readOnly"] != true || w["name"] != aw["name"] || w["subPath"] != aw["subPath"] {
			t.Errorf("the daemon's workspace %v, the agent's %v", w, aw)
		}
		if l := dig(d, "resources", "limits", "ephemeral-storage"); l != "20Gi" {
			t.Errorf("the daemon's storage is unbounded: %v", l)
		}
		if envValue(container(t, spec, "agent"), "DOCKER_HOST") != "unix:///devbot-docker/docker.sock" {
			t.Errorf("%s-docker's agent has no DOCKER_HOST", kind)
		}
	}

	// Localhost profiles instead.
	objs = mustRender(t, base+`
docker:
  mode: dind
  dind:
    acceptLooserSandbox: true
    seccompProfile: {type: Localhost, localhostProfile: profiles/rootless-docker.json}
    appArmorProfile: {type: Localhost, localhostProfile: rootless-docker}
`)
	d := container(t, jobSpec(t, objs, "devbot-implement-docker"), "docker")
	if dig(d, "securityContext", "seccompProfile", "type") != "Localhost" || dig(d, "securityContext", "appArmorProfile", "localhostProfile") != "rootless-docker" {
		t.Errorf("the daemon: %v", d["securityContext"])
	}
}

// The chart's TUN device plugin: on the job pods' nodes, pinned, giving out
// /dev/net/tun and nothing else, with no API token and no network. With it
// off, the daemon claims the install's own plugin's resource instead.
func TestChartDockerTunPlugin(t *testing.T) {
	objs := mustRender(t, dind+`
jobs:
  nodeSelector: {kubernetes.io/arch: amd64, devbot.io/jobs: "true"}
  tolerations: [{key: devbot.io/jobs, operator: Exists, effect: NoSchedule}]
  affinity: {nodeAffinity: {requiredDuringSchedulingIgnoredDuringExecution: {nodeSelectorTerms: [{matchExpressions: [{key: zone, operator: In, values: [a]}]}]}}}
`)
	ds := objs["DaemonSet/devbot-tun-plugin"]
	if ds == nil {
		t.Fatal("no TUN device plugin")
	}
	spec := dig(ds, "spec", "template", "spec").(obj)
	job := jobSpec(t, objs, "devbot-implement-docker")
	for _, k := range []string{"nodeSelector", "tolerations", "affinity"} {
		got, _ := json.Marshal(spec[k])
		want, _ := json.Marshal(job[k])
		if string(got) != string(want) {
			t.Errorf("the plugin's %s %s, the job pods' %s", k, got, want)
		}
	}
	if spec["automountServiceAccountToken"] != false || spec["serviceAccountName"] != "devbot-tun-plugin" || objs["ServiceAccount/devbot-tun-plugin"]["automountServiceAccountToken"] != false {
		t.Errorf("the plugin's ServiceAccount: %v, %v", spec, objs["ServiceAccount/devbot-tun-plugin"])
	}
	for name, o := range objs {
		if b, _ := json.Marshal(o); (strings.HasPrefix(name, "RoleBinding/") || strings.HasPrefix(name, "ClusterRoleBinding/")) && strings.Contains(string(b), "tun-plugin") {
			t.Errorf("%s binds the plugin", name)
		}
	}
	cs := spec["containers"].([]any)
	if len(cs) != 1 || spec["initContainers"] != nil {
		t.Fatalf("the plugin's containers: %v", cs)
	}
	c := cs[0].(obj)
	if img := c["image"].(string); !strings.Contains(img, ":0.") || !strings.Contains(img, "@sha256:") || strings.Contains(img, "latest") {
		t.Errorf("the plugin's image isn't pinned: %s", img)
	}
	args := toStrings(c["args"])
	var devices []string
	for _, a := range args {
		if d, ok := strings.CutPrefix(a, "--device="); ok {
			devices = append(devices, d)
		}
	}
	if !slices.Contains(args, "--domain=devbot.io") || len(devices) != 1 {
		t.Fatalf("the plugin's args: %v", args)
	}
	var dev struct {
		Name   string
		Groups []struct {
			Count int
			Paths []struct{ Path, MountPath string }
		}
	}
	if err := json.Unmarshal([]byte(devices[0]), &dev); err != nil {
		t.Fatal(err)
	}
	if dev.Name != "tun" || len(dev.Groups) != 1 || len(dev.Groups[0].Paths) != 1 || dev.Groups[0].Paths[0].Path != "/dev/net/tun" || dev.Groups[0].Paths[0].MountPath != "" || dev.Groups[0].Count < 2 {
		t.Errorf("the plugin gives out more than /dev/net/tun, or one per node: %s", devices[0])
	}
	if dig(c, "resources", "limits", "cpu") == nil || dig(c, "resources", "limits", "memory") == nil {
		t.Errorf("the plugin's resources are unbounded: %v", c["resources"])
	}
	if np := objs["NetworkPolicy/devbot-tun-plugin"]; !slices.Equal(toStrings(dig(np, "spec", "policyTypes")), []string{"Ingress", "Egress"}) || dig(np, "spec", "ingress") != nil || dig(np, "spec", "egress") != nil ||
		dig(np, "spec", "podSelector", "matchLabels", "app.kubernetes.io/component") != "tun-plugin" {
		t.Errorf("the plugin's policy: %v", np)
	}
	// The plugin is root on the node, for the kubelet's root-owned socket,
	// and nothing more: no privilege, no capability, the runtime's seccomp
	// profile, and of the node's /dev only /dev/net, read-only.
	sc := c["securityContext"].(obj)
	if sc["privileged"] != false || sc["allowPrivilegeEscalation"] != false || sc["readOnlyRootFilesystem"] != true || sc["runAsUser"] != 0 ||
		!slices.Equal(toStrings(dig(sc, "capabilities", "drop")), []string{"ALL"}) || dig(sc, "capabilities", "add") != nil ||
		dig(sc, "seccompProfile", "type") != "RuntimeDefault" || dig(sc, "appArmorProfile") != nil || dig(sc, "seLinuxOptions") != nil {
		t.Errorf("the plugin's securityContext: %v", sc)
	}
	var hostPaths []string
	for _, v := range spec["volumes"].([]any) {
		hostPaths = append(hostPaths, dig(v, "hostPath", "path").(string))
	}
	if !slices.Equal(hostPaths, []string{"/var/lib/kubelet/device-plugins", "/dev/net"}) || mount(t, c, "/dev/net")["readOnly"] != true {
		t.Errorf("the plugin's host paths: %v, %v", hostPaths, c["volumeMounts"])
	}

	// No workload is privileged, and job pods mount no host path: the
	// DaemonSet is the only one that does.
	for name, o := range objs {
		if b, _ := json.Marshal(o); strings.Contains(string(b), `"privileged":true`) {
			t.Errorf("%s is privileged", name)
		}
		if !strings.HasPrefix(name, "ScaledJob/") && !strings.HasPrefix(name, "Deployment/") {
			continue
		}
		if b, _ := json.Marshal(o); strings.Contains(string(b), `"hostPath"`) {
			t.Errorf("%s mounts a host path", name)
		}
	}

	// The plugin advertises a resource named in values.
	objs = mustRender(t, base+"docker: {mode: dind, dind: {acceptLooserSandbox: true, tunResource: example.com/net-tun}}\n")
	if a := toStrings(dig(objs["DaemonSet/devbot-tun-plugin"], "spec", "template", "spec", "containers").([]any)[0].(obj)["args"]); !slices.Contains(a, "--domain=example.com") || !strings.Contains(strings.Join(a, " "), `"name":"net-tun"`) {
		t.Errorf("the plugin's args: %v", a)
	}
	if dig(container(t, jobSpec(t, objs, "devbot-review-docker"), "docker"), "resources", "limits", "example.com/net-tun") != 1 {
		t.Error("the daemon doesn't claim the plugin's resource")
	}

	// The install's own plugin.
	objs = mustRender(t, base+"docker: {mode: dind, dind: {acceptLooserSandbox: true, tunPlugin: {enabled: false}, tunResource: squat.ai/tun}}\n")
	for name := range objs {
		if strings.Contains(name, "tun-plugin") {
			t.Errorf("%s rendered with docker.dind.tunPlugin off", name)
		}
	}
	for _, kind := range []string{"implement", "review"} {
		d := container(t, jobSpec(t, objs, "devbot-"+kind+"-docker"), "docker")
		if dig(d, "resources", "limits", "squat.ai/tun") != 1 || dig(d, "resources", "limits", "devbot.io/tun") != nil {
			t.Errorf("%s's daemon: %v", kind, d["resources"])
		}
	}
}

func TestChartDockerRefusesIncompleteModes(t *testing.T) {
	for _, c := range []struct{ name, values, want string }{
		{"an unknown mode", base + "docker: {mode: podman}\n", "docker.mode"},
		{"dind without a user namespace", base + "docker: {mode: dind, dind: {acceptLooserSandbox: true}}\njobs: {userNamespaces: false}\n", "jobs.userNamespaces"},
		{"dind without accepting its sandbox", base + "docker: {mode: dind}\n", "docker.dind.acceptLooserSandbox"},
		{"dind without a TUN device", base + "docker: {mode: dind, dind: {acceptLooserSandbox: true, tunPlugin: {enabled: false}}}\n", "docker.dind.tunPlugin.enabled"},
		{"a TUN resource that isn't one", base + "docker: {mode: dind, dind: {acceptLooserSandbox: true, tunResource: tun}}\n", "docker.dind.tunResource"},
		{"config.docker set by hand", base + "config: {docker: {mode: dind}}\n", "docker.mode"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := render(t, c.values)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error naming %s, got %v", c.want, err)
			}
		})
	}
}

func toStrings(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, s := range l {
		out = append(out, s.(string))
	}
	return out
}

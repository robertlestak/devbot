package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/kube"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// fakeCluster is the part of a namespace serve runs services in: job pods
// the test puts there, and the service pods and NetworkPolicies serve
// creates, as JSON would have them.
type fakeCluster struct {
	pods    map[string]kube.Pod
	created map[string]map[string]any // service pods' specs as sent, by name
	nps     map[string]map[string]any
	creates []string
	deletes []string
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{pods: map[string]kube.Pod{}, created: map[string]map[string]any{}, nps: map[string]map[string]any{}}
}

func (f *fakeCluster) Pod(_ context.Context, name string) (kube.Pod, error) {
	p, ok := f.pods[name]
	if !ok {
		return p, kube.ErrNotFound
	}
	return p, nil
}

func (f *fakeCluster) Pods(_ context.Context, selector string) ([]kube.Pod, error) {
	var out []kube.Pod
	for _, p := range f.pods {
		if matches(p.Metadata.Labels, selector) {
			out = append(out, p)
		}
	}
	return out, nil
}

func matches(labels map[string]string, selector string) bool {
	for _, kv := range strings.Split(selector, ",") {
		k, v, eq := strings.Cut(kv, "=")
		if have, ok := labels[k]; !ok || eq && have != v {
			return false
		}
	}
	return true
}

func asJSON(obj any) map[string]any {
	b, _ := json.Marshal(obj)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func (f *fakeCluster) CreatePod(_ context.Context, obj any) error {
	m := asJSON(obj)
	var p kube.Pod
	b, _ := json.Marshal(m)
	json.Unmarshal(b, &p)
	name := p.Metadata.Name
	if _, ok := f.pods[name]; ok {
		return kube.ErrExists
	}
	p.Metadata.CreationTimestamp = time.Now()
	p.Status = kube.PodStatus{Phase: "Pending"}
	f.pods[name], f.created[name] = p, m
	f.creates = append(f.creates, "pod/"+name)
	return nil
}

func (f *fakeCluster) DeletePod(_ context.Context, name string) error {
	if _, ok := f.pods[name]; !ok {
		return kube.ErrNotFound
	}
	delete(f.pods, name)
	f.deletes = append(f.deletes, "pod/"+name)
	return nil
}

func (f *fakeCluster) NetworkPolicies(_ context.Context, selector string) ([]string, error) {
	var out []string
	for n, np := range f.nps {
		labels := map[string]string{}
		for k, v := range np["metadata"].(map[string]any)["labels"].(map[string]any) {
			labels[k] = v.(string)
		}
		if matches(labels, selector) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (f *fakeCluster) CreateNetworkPolicy(_ context.Context, obj any) error {
	m := asJSON(obj)
	name := m["metadata"].(map[string]any)["name"].(string)
	if _, ok := f.nps[name]; ok {
		return kube.ErrExists
	}
	f.nps[name] = m
	f.creates = append(f.creates, "networkpolicy/"+name)
	return nil
}

func (f *fakeCluster) DeleteNetworkPolicy(_ context.Context, name string) error {
	if _, ok := f.nps[name]; !ok {
		return kube.ErrNotFound
	}
	delete(f.nps, name)
	f.deletes = append(f.deletes, "networkpolicy/"+name)
	return nil
}

// jobPod puts a running job pod of kind in the cluster, owned by a Job with
// its own UID, labelled as the Job controller labels its pods.
func (f *fakeCluster) jobPod(name, kind string) {
	uid := "uid-" + name
	f.pods[name] = kube.Pod{
		Metadata: kube.ObjectMeta{Name: name, Labels: map[string]string{"app.kubernetes.io/instance": "devbot", "app.kubernetes.io/component": kind, jobUIDLabel: uid},
			OwnerReferences: []kube.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "job-" + name, UID: uid}}},
		Status: kube.PodStatus{Phase: "Running"},
	}
}

// phase sets a pod's phase.
func (f *fakeCluster) phase(name, phase string) {
	p := f.pods[name]
	p.Status.Phase = phase
	f.pods[name] = p
}

// ready marks every container of a service pod ready, at ip.
func (f *fakeCluster) ready(name, ip string, svcs ...string) {
	p := f.pods[name]
	p.Status.Phase, p.Status.PodIP, p.Status.ContainerStatuses = "Running", ip, nil
	for _, s := range svcs {
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, kube.ContainerStatus{Name: s, Ready: true})
	}
	f.pods[name] = p
}

var pgService = []config.Service{
	{Name: "openbao", Image: "openbao/openbao:2.4.1", Env: map[string]string{"BAO_DEV_ROOT_TOKEN_ID": "root"}, Ports: []int{18237}, Ready: config.ServiceReady{TCP: 18237}},
	{Name: "pg", Image: "registry.example.com/ci-postgres:17", Env: map[string]string{"PGPORT": "54337"}, Ports: []int{54337},
		Ready: config.ServiceReady{Exec: []string{"pg_isready", "-p", "54337"}}, Resources: config.ServiceResources{CPU: "2", Memory: "2Gi"}},
}

func servicesServer(t *testing.T) (*Server, *fakeCluster) {
	s, _ := newServer(t, "client")
	c := &s.Cfg.Services
	c.Enabled, c.Instance, c.NetworkPolicy, c.UserNamespaces, c.ServiceAccount = true, "devbot", true, true, "devbot-services"
	s.Cfg.Repos.Limits.Services.Images = []string{"registry.example.com/", "docker.io/openbao/"}
	fc := newFakeCluster()
	s.ServicePods = fc
	return s, fc
}

func askServices(t *testing.T, s *Server, kind, ref, pod string, svcs []config.Service) {
	t.Helper()
	q := s.E.Q
	if ok, err := q.Acquire(t.Context(), kind, ref, pod, time.Minute); err != nil || !ok {
		t.Fatal("lease", err)
	}
	q.Describe(t.Context(), kind, ref, pod, queue.LeaseDetail{Pod: pod, Services: svcs})
}

func status(t *testing.T, s *Server, pod string) queue.ServicesStatus {
	t.Helper()
	st, ok, err := s.E.Q.Services(t.Context(), pod)
	if err != nil || !ok {
		t.Fatalf("no status for %s: %v", pod, err)
	}
	return st
}

func dig(m any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			mm, _ := m.(map[string]any)
			m = mm[k]
		case int:
			l, _ := m.([]any)
			if k >= len(l) {
				return nil
			}
			m = l[k]
		}
	}
	return m
}

// Two runs at once, of one repo or two, an implementer and a reviewer: each
// gets a service pod of its own, owned by its own Job, and a NetworkPolicy
// that admits its own job pod alone. Running it again has nothing to do.
func TestEveryRunGetsItsOwnServices(t *testing.T) {
	s, fc := servicesServer(t)
	ctx := t.Context()
	runs := []struct{ kind, ref, pod string }{
		{queue.KindImplement, "fake:acme/app#1", "devbot-implement-a"},
		{queue.KindImplement, "fake:acme/app#2", "devbot-implement-b"},
		{queue.KindReview, "fake:acme/other#3", "devbot-review-c"},
	}
	for _, r := range runs {
		fc.jobPod(r.pod, r.kind)
		askServices(t, s, r.kind, r.ref, r.pod, pgService)
	}
	s.RunServices(ctx)
	if len(fc.created) != 3 || len(fc.nps) != 3 {
		t.Fatalf("created %v", fc.creates)
	}
	for i, r := range runs {
		name := r.pod + "-services"
		p, np := fc.created[name], fc.nps[name]
		uid := "uid-" + r.pod
		if o := dig(p, "metadata", "ownerReferences", 0); dig(o, "kind") != "Job" || dig(o, "uid") != uid || dig(o, "name") != "job-"+r.pod {
			t.Errorf("%s isn't owned by its run's Job: %v", name, o)
		}
		if dig(np, "metadata", "ownerReferences", 0, "uid") != uid {
			t.Errorf("%s's NetworkPolicy isn't owned by its run's Job", name)
		}
		if dig(np, "spec", "podSelector", "matchLabels", runLabel) != uid || dig(p, "metadata", "labels", runLabel) != uid {
			t.Errorf("%s's NetworkPolicy doesn't select its pod alone: %v", name, dig(np, "spec", "podSelector"))
		}
		from := dig(np, "spec", "ingress", 0, "from")
		if len(from.([]any)) != 1 || dig(from, 0, "podSelector", "matchLabels", jobUIDLabel) != uid || dig(np, "spec", "ingress", 1) != nil {
			t.Errorf("%s admits more than its own job pod: %v", name, dig(np, "spec", "ingress"))
		}
		for j, o := range runs {
			if j != i && strings.Contains(fmt.Sprint(np), "uid-"+o.pod) {
				t.Errorf("%s's policy names %s's run", name, o.pod)
			}
		}
		if st := status(t, s, r.pod); st.State != queue.ServicesStarting {
			t.Errorf("%s: %+v", r.pod, st)
		}
	}
	// The run's deadline is its role's.
	if dig(fc.created["devbot-implement-a-services"], "spec", "activeDeadlineSeconds") != float64(70*60) ||
		dig(fc.created["devbot-review-c-services"], "spec", "activeDeadlineSeconds") != float64(30*60) {
		t.Error("deadlines aren't the roles'")
	}

	s.RunServices(ctx)
	if len(fc.creates) != 6 || len(fc.deletes) != 0 {
		t.Fatalf("a second pass did something: %v %v", fc.creates, fc.deletes)
	}

	// Each is ready at its own address, and says so for its own job.
	fc.ready("devbot-implement-a-services", "10.0.0.1", "openbao", "pg")
	fc.ready("devbot-implement-b-services", "10.0.0.2", "openbao", "pg")
	s.RunServices(ctx)
	if a, b := status(t, s, "devbot-implement-a"), status(t, s, "devbot-implement-b"); a.State != queue.ServicesReady || a.IP != "10.0.0.1" || b.IP != "10.0.0.2" {
		t.Fatalf("statuses %+v %+v", a, b)
	}
	if c := status(t, s, "devbot-review-c"); c.State != queue.ServicesStarting {
		t.Fatalf("a pod with no ready containers is ready: %+v", c)
	}
}

// What a service pod is: hardened like a job pod, with nothing to reach the
// API server with and nothing of the node's.
func TestServicePodSpec(t *testing.T) {
	s, fc := servicesServer(t)
	fc.jobPod("devbot-implement-a", queue.KindImplement)
	askServices(t, s, queue.KindImplement, "fake:acme/app#1", "devbot-implement-a", pgService)
	s.RunServices(t.Context())
	p := fc.created["devbot-implement-a-services"]
	spec := dig(p, "spec").(map[string]any)
	for k, want := range map[string]any{"restartPolicy": "Never", "automountServiceAccountToken": false, "enableServiceLinks": false,
		"hostUsers": false, "serviceAccountName": "devbot-services"} {
		if spec[k] != want {
			t.Errorf("spec.%s = %v, want %v", k, spec[k], want)
		}
	}
	if dig(spec, "securityContext", "seccompProfile", "type") != "RuntimeDefault" || spec["volumes"] != nil || spec["hostNetwork"] != nil {
		t.Errorf("spec: %v", spec)
	}
	if l := dig(p, "metadata", "labels").(map[string]any); l["app.kubernetes.io/component"] != "services" || l["app.kubernetes.io/instance"] != "devbot" {
		t.Errorf("labels %v", l)
	}
	bao, pg := dig(spec, "containers", 0), dig(spec, "containers", 1)
	if dig(bao, "image") != "docker.io/openbao/openbao:2.4.1" || dig(bao, "readinessProbe", "tcpSocket", "port") != float64(18237) {
		t.Errorf("openbao: %v", bao)
	}
	if dig(pg, "readinessProbe", "exec", "command", 0) != "pg_isready" || dig(pg, "env", 0, "name") != "PGPORT" || dig(pg, "ports", 0, "containerPort") != float64(54337) {
		t.Errorf("pg: %v", pg)
	}
	if dig(pg, "resources", "limits", "cpu") != "2" || dig(pg, "resources", "limits", "memory") != "2Gi" || dig(bao, "resources", "limits", "memory") != "1Gi" ||
		dig(pg, "resources", "limits", "ephemeral-storage") != "2Gi" || dig(bao, "resources", "limits", "ephemeral-storage") != "2Gi" {
		t.Errorf("resources: %v / %v", dig(pg, "resources"), dig(bao, "resources"))
	}
	if dig(pg, "securityContext", "allowPrivilegeEscalation") != false || dig(pg, "terminationMessagePolicy") != "FallbackToLogsOnError" {
		t.Errorf("pg's securityContext: %v", dig(pg, "securityContext"))
	}
	if spec["imagePullSecrets"] != nil {
		t.Errorf("imagePullSecrets with none configured: %v", spec["imagePullSecrets"])
	}

	// The install's pull secrets, on the pod alone, for the kubelet: no
	// container mounts or reads one.
	s.Cfg.Services.ImagePullSecrets = []string{"regcred", "ci-images"}
	fc.jobPod("devbot-implement-b", queue.KindImplement)
	askServices(t, s, queue.KindImplement, "fake:acme/app#2", "devbot-implement-b", pgService)
	s.RunServices(t.Context())
	spec = dig(fc.created["devbot-implement-b-services"], "spec").(map[string]any)
	if got := fmt.Sprint(spec["imagePullSecrets"]); got != "[map[name:regcred] map[name:ci-images]]" {
		t.Errorf("imagePullSecrets: %s", got)
	}
	for _, c := range spec["containers"].([]any) {
		if dig(c, "envFrom") != nil || dig(c, "volumeMounts") != nil {
			t.Errorf("a service container gets more than literal env: %v", c)
		}
	}
}

// Services go with their job, however it ends: its lease released, its pod
// finished or gone, or after a Redis wipe, when nothing asks for them.
func TestServicesGoWithTheirJob(t *testing.T) {
	s, fc := servicesServer(t)
	ctx := t.Context()
	q := s.E.Q
	for _, p := range []string{"devbot-implement-a", "devbot-implement-b", "devbot-implement-c"} {
		fc.jobPod(p, queue.KindImplement)
	}
	// The chart's own policy for every service pod, which serve never made.
	fc.nps["devbot-services"] = map[string]any{"metadata": map[string]any{"name": "devbot-services",
		"labels": map[string]any{"app.kubernetes.io/instance": "devbot", "app.kubernetes.io/component": "services"}}}
	askServices(t, s, queue.KindImplement, "fake:acme/app#1", "devbot-implement-a", pgService)
	askServices(t, s, queue.KindImplement, "fake:acme/app#2", "devbot-implement-b", pgService)
	askServices(t, s, queue.KindImplement, "fake:acme/app#3", "devbot-implement-c", pgService)
	s.RunServices(ctx)
	if len(fc.created) != 3 {
		t.Fatalf("created %v", fc.creates)
	}

	// a finished; b's pod was killed, its lease not yet expired; c runs on.
	// a's services stay until its pod is done, as after a Redis outage.
	q.Release(ctx, queue.KindImplement, "fake:acme/app#1", "devbot-implement-a")
	s.RunServices(ctx)
	if len(fc.deletes) != 0 {
		t.Fatalf("deleted %v while their job pods still run", fc.deletes)
	}
	fc.phase("devbot-implement-a", "Succeeded")
	fc.phase("devbot-implement-b", "Failed")
	s.RunServices(ctx)
	slices.Sort(fc.deletes)
	want := []string{"networkpolicy/devbot-implement-a-services", "networkpolicy/devbot-implement-b-services", "pod/devbot-implement-a-services", "pod/devbot-implement-b-services"}
	if !slices.Equal(fc.deletes, want) {
		t.Fatalf("deleted %v, want %v", fc.deletes, want)
	}
	if _, ok := fc.pods["devbot-implement-c-services"]; !ok {
		t.Fatal("a running job's services were deleted")
	}
	s.RunServices(ctx)
	if len(fc.deletes) != 4 || len(fc.creates) != 6 {
		t.Fatalf("a second pass did something: %v %v", fc.creates, fc.deletes)
	}

	// Redis wiped: no lease asks for c's services any more, but its job
	// pod runs on, and its next heartbeat says so again.
	q.R.FlushDB(ctx)
	s.RunServices(ctx)
	if len(fc.deletes) != 4 {
		t.Fatalf("a running job's services went with a wipe: %v", fc.deletes)
	}
	fc.phase("devbot-implement-c", "Succeeded")
	s.RunServices(ctx)
	if len(fc.nps) != 1 || fc.nps["devbot-services"] == nil || !slices.Contains(fc.deletes, "pod/devbot-implement-c-services") {
		t.Fatalf("after a wipe: %v, policies %v", fc.deletes, fc.nps)
	}
}

// Redis out of reach for longer than the lease TTL, in the middle of a
// run: serve keeps the run's services, the job's heartbeat re-takes its
// lease with what it described, and the services carry on as they were.
func TestServicesSurviveARedisOutage(t *testing.T) {
	s, fc := servicesServer(t)
	mr := miniredis.RunT(t)
	s.E.Q = queue.New(mr.Addr(), 0)
	ctx := t.Context()
	const pod, ref = "devbot-implement-a", "fake:acme/app#1"
	fc.jobPod(pod, queue.KindImplement)
	askServices(t, s, queue.KindImplement, ref, pod, pgService)
	s.RunServices(ctx)
	fc.ready(pod+"-services", "10.0.0.1", "openbao", "pg")
	s.RunServices(ctx)
	if st := status(t, s, pod); st.State != queue.ServicesReady {
		t.Fatalf("status %+v", st)
	}

	mr.Close()
	s.RunServices(ctx) // can't read the leases: does nothing
	if err := mr.Restart(); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(5 * time.Minute) // the lease and the status lapsed meanwhile
	if run, _ := s.E.Q.Running(ctx); len(run) != 0 {
		t.Fatalf("the lease should have lapsed: %v", run)
	}
	s.RunServices(ctx) // before the job's next heartbeat
	if len(fc.deletes) != 0 {
		t.Fatalf("a running job's services were deleted: %v", fc.deletes)
	}
	if ok, err := s.E.Q.Renew(ctx, queue.KindImplement, ref, pod, time.Minute); err != nil || !ok {
		t.Fatalf("renew: %v %v", ok, err)
	}
	s.RunServices(ctx)
	s.RunServices(ctx)
	if st := status(t, s, pod); st.State != queue.ServicesReady || st.IP != "10.0.0.1" {
		t.Fatalf("status after the outage %+v", st)
	}
	if len(fc.deletes) != 0 || len(fc.creates) != 2 {
		t.Fatalf("the outage changed the services: created %v, deleted %v", fc.creates, fc.deletes)
	}
}

// A service pod that goes away after it started, evicted or deleted by
// hand, fails the run's services rather than starting empty ones at
// another address.
func TestServicePodDeletedMidRun(t *testing.T) {
	for _, gone := range []string{"deleted", "deleting"} {
		t.Run(gone, func(t *testing.T) {
			s, fc := servicesServer(t)
			ctx := t.Context()
			const pod = "devbot-implement-a"
			fc.jobPod(pod, queue.KindImplement)
			askServices(t, s, queue.KindImplement, "fake:acme/app#1", pod, pgService)
			s.RunServices(ctx)
			fc.ready(pod+"-services", "10.0.0.1", "openbao", "pg")
			s.RunServices(ctx)
			if gone == "deleted" {
				delete(fc.pods, pod+"-services")
			} else {
				p := fc.pods[pod+"-services"]
				now := time.Now()
				p.Metadata.DeletionTimestamp = &now
				fc.pods[pod+"-services"] = p
			}
			s.RunServices(ctx)
			s.RunServices(ctx)
			if st := status(t, s, pod); st.State != queue.ServicesFailed || !strings.Contains(st.Reason, "devbot-implement-a-services") {
				t.Fatalf("status %+v", st)
			}
			if len(fc.created) != 1 {
				t.Fatalf("the services were started again: %v", fc.creates)
			}
		})
	}
}

// A service that was ready and misses its readiness probe later in a long
// run, past ready_timeout, is only slow: the run's services stay ready at
// their address, and become failed only if it exits.
func TestServicesNotReadyAfterReady(t *testing.T) {
	s, fc := servicesServer(t)
	ctx := t.Context()
	const pod = "devbot-implement-a"
	fc.jobPod(pod, queue.KindImplement)
	askServices(t, s, queue.KindImplement, "fake:acme/app#1", pod, pgService)
	s.RunServices(ctx)
	fc.ready(pod+"-services", "10.0.0.1", "openbao", "pg")
	s.RunServices(ctx)

	p := fc.pods[pod+"-services"]
	p.Metadata.CreationTimestamp = time.Now().Add(-10 * time.Minute)
	p.Status.ContainerStatuses[1] = kube.ContainerStatus{Name: "pg", State: kube.ContainerState{Running: &struct {
		StartedAt time.Time `json:"startedAt"`
	}{}}}
	fc.pods[pod+"-services"] = p
	s.RunServices(ctx)
	s.RunServices(ctx)
	if st := status(t, s, pod); st.State != queue.ServicesReady || st.IP != "10.0.0.1" {
		t.Fatalf("a missed readiness probe changed the status: %+v", st)
	}
	if len(fc.deletes) != 0 {
		t.Fatalf("a slow service's pod was deleted: %v", fc.deletes)
	}

	p.Status.ContainerStatuses[1] = kube.ContainerStatus{Name: "pg", State: kube.ContainerState{Terminated: &kube.Terminated{ExitCode: 137, Reason: "OOMKilled", Message: ""}}}
	fc.pods[pod+"-services"] = p
	s.RunServices(ctx)
	if st := status(t, s, pod); st.State != queue.ServicesFailed || st.Reason != "service pg exited with code 137 (OOMKilled)" {
		t.Fatalf("status %+v", st)
	}
}

// A service that can't be pulled, exits, or is never ready fails its run's
// services with that reason, once and for good: serve deletes the pod and
// doesn't start it again.
func TestServiceFailures(t *testing.T) {
	cases := []struct {
		name  string
		state func(*kube.Pod)
		want  string
	}{
		{"pull", func(p *kube.Pod) {
			p.Status.ContainerStatuses = []kube.ContainerStatus{{Name: "pg", State: kube.ContainerState{Waiting: &struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			}{"ImagePullBackOff", "manifest unknown"}}}}
		}, "service pg: image registry.example.com/ci-postgres:17 can't be pulled (ImagePullBackOff): manifest unknown"},
		{"exit", func(p *kube.Pod) {
			p.Status.ContainerStatuses = []kube.ContainerStatus{{Name: "pg", State: kube.ContainerState{Terminated: &kube.Terminated{ExitCode: 1, Reason: "Error", Message: "FATAL: pg_tde: no key provider"}}}}
		}, "service pg exited with code 1 (Error):\nFATAL: pg_tde: no key provider"},
		{"timeout", func(p *kube.Pod) {
			p.Metadata.CreationTimestamp = time.Now().Add(-10 * time.Minute)
			p.Status.ContainerStatuses = []kube.ContainerStatus{{Name: "pg", State: kube.ContainerState{Running: &struct {
				StartedAt time.Time `json:"startedAt"`
			}{}}}}
		}, "the services weren't ready after 5m0s (services.ready_timeout): pg: running, not ready yet"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, fc := servicesServer(t)
			ctx := t.Context()
			fc.jobPod("devbot-implement-a", queue.KindImplement)
			askServices(t, s, queue.KindImplement, "fake:acme/app#1", "devbot-implement-a", pgService[1:])
			s.RunServices(ctx)
			p := fc.pods["devbot-implement-a-services"]
			c.state(&p)
			fc.pods["devbot-implement-a-services"] = p
			s.RunServices(ctx)
			if st := status(t, s, "devbot-implement-a"); st.State != queue.ServicesFailed || st.Reason != c.want {
				t.Fatalf("status %+v, want %q", st, c.want)
			}
			if _, ok := fc.pods["devbot-implement-a-services"]; ok {
				t.Fatal("a failed service pod is kept")
			}
			s.RunServices(ctx)
			if len(fc.created) != 1 {
				t.Fatalf("failed services were started again: %v", fc.creates)
			}
			if st := status(t, s, "devbot-implement-a"); st.State != queue.ServicesFailed {
				t.Fatalf("a failure isn't final: %+v", st)
			}
		})
	}
}

// A lease is written by whoever reaches Redis: serve starts services only
// for this install's job pod of the lease's kind, with a Job to own them,
// and only what the install's limits allow.
func TestServicesForgedLease(t *testing.T) {
	s, fc := servicesServer(t)
	ctx := t.Context()
	fc.pods["devbot-serve"] = kube.Pod{Metadata: kube.ObjectMeta{Name: "devbot-serve", Labels: map[string]string{"app.kubernetes.io/instance": "devbot", "app.kubernetes.io/component": "serve"}}}
	fc.jobPod("devbot-review-b", queue.KindReview)
	fc.jobPod("devbot-implement-c", queue.KindImplement)
	orphan := "devbot-implement-d"
	fc.jobPod(orphan, queue.KindImplement)
	p := fc.pods[orphan]
	p.Metadata.OwnerReferences = nil
	fc.pods[orphan] = p

	askServices(t, s, queue.KindImplement, "fake:acme/app#1", "devbot-serve", pgService)
	askServices(t, s, queue.KindImplement, "fake:acme/app#2", "devbot-review-b", pgService) // a review pod, an implement lease
	evil := []config.Service{{Name: "x", Image: "docker.io/evil/miner:1"}}
	askServices(t, s, queue.KindImplement, "fake:acme/app#3", "devbot-implement-c", evil)
	askServices(t, s, queue.KindImplement, "fake:acme/app#4", orphan, pgService)
	askServices(t, s, queue.KindImplement, "fake:acme/app#5", "devbot-implement-gone", pgService)
	s.RunServices(ctx)
	if len(fc.creates) != 0 {
		t.Fatalf("created %v", fc.creates)
	}
	for pod, want := range map[string]string{
		"devbot-serve": "isn't one of this install's implement pods", "devbot-review-b": "isn't one of this install's implement pods",
		"devbot-implement-c": "repos.limits.services.images", orphan: "has no Job",
	} {
		if st := status(t, s, pod); st.State != queue.ServicesFailed || !strings.Contains(st.Reason, want) {
			t.Errorf("%s: %+v, want %q", pod, st, want)
		}
	}
}

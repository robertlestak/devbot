package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/kube"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// ServicePods runs service pods: a kube.Client in the cluster.
type ServicePods interface {
	Pod(ctx context.Context, name string) (kube.Pod, error)
	Pods(ctx context.Context, selector string) ([]kube.Pod, error)
	CreatePod(ctx context.Context, obj any) error
	DeletePod(ctx context.Context, name string) error
	NetworkPolicies(ctx context.Context, selector string) ([]string, error)
	CreateNetworkPolicy(ctx context.Context, obj any) error
	DeleteNetworkPolicy(ctx context.Context, name string) error
}

// Labels and annotations on what serve creates for a run's services.
const (
	// The run's Job's UID, which selects the service pod for its
	// NetworkPolicy. The Job controller labels its own pods with it too
	// (jobUIDLabel), which is how the policy admits the run's job pod.
	runLabel    = "devbot.io/run"
	jobUIDLabel = "batch.kubernetes.io/controller-uid"
	jobPodAnn   = "devbot.io/job-pod"
	taskAnn     = "devbot.io/task"
	servicesTTL = 2 * time.Minute
)

// servicesLoop runs job pods' services every 2s: a run waits on it before
// its model starts.
func (s *Server) servicesLoop(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RunServices(ctx)
		}
	}
}

// RunServices is one pass of servicesLoop. It gives every live lease that
// asks for services a service pod, records how it is doing for the
// harness, and deletes every service pod (and its NetworkPolicy) no live
// job wants: its job ended, failed, was killed, or its services failed. It
// holds no state of its own, so it is safe to repeat, on any replica that
// leads, after a Redis wipe too. A service pod no lease names is kept while
// its job pod runs: the lease lapsed with Redis out of reach, or went with a
// wipe, and the job's next heartbeat puts it back.
func (s *Server) RunServices(ctx context.Context) {
	leases, err := s.E.Q.Running(ctx)
	if err != nil {
		s.Log.Warn("services: reading leases", "err", err)
		return
	}
	keep, leased := map[string]bool{}, map[string]bool{}
	for _, l := range leases {
		if l.Pod == "" || len(l.Services) == 0 {
			continue
		}
		leased[servicePodName(l.Pod)] = true
		if s.ensureServices(ctx, l) {
			keep[servicePodName(l.Pod)] = true
		}
	}
	// Only what serve made: the chart's own policy for service pods has
	// their component too, and no run.
	sel := "app.kubernetes.io/instance=" + s.Cfg.Services.Instance + ",app.kubernetes.io/component=services," + runLabel
	pods, err := s.ServicePods.Pods(ctx, sel)
	if err != nil {
		s.Log.Warn("services: listing service pods", "err", err)
		// Without them, which policies a kept pod needs is unknown too.
		return
	}
	for _, p := range pods {
		name := p.Metadata.Name
		if !leased[name] && s.jobPodRunning(ctx, p.Metadata.Annotations[jobPodAnn]) {
			keep[name] = true
		}
		if !keep[name] && p.Metadata.DeletionTimestamp == nil {
			s.Log.Info("services: deleting a service pod no running job wants", "pod", name, "job_pod", p.Metadata.Annotations[jobPodAnn])
			if err := s.ServicePods.DeletePod(ctx, name); err != nil && !errors.Is(err, kube.ErrNotFound) {
				s.Log.Warn("services: deleting a service pod", "pod", name, "err", err)
			}
		}
	}
	if !s.Cfg.Services.NetworkPolicy {
		return
	}
	nps, err := s.ServicePods.NetworkPolicies(ctx, sel)
	if err != nil {
		s.Log.Warn("services: listing service NetworkPolicies", "err", err)
		return
	}
	for _, n := range nps {
		if !keep[n] {
			if err := s.ServicePods.DeleteNetworkPolicy(ctx, n); err != nil && !errors.Is(err, kube.ErrNotFound) {
				s.Log.Warn("services: deleting a service NetworkPolicy", "name", n, "err", err)
			}
		}
	}
}

func servicePodName(jobPod string) string { return jobPod + "-services" }

// jobPodRunning reports whether a job pod is still running its job, or
// might be: when the API server doesn't answer, it can't tell.
func (s *Server) jobPodRunning(ctx context.Context, name string) bool {
	if name == "" {
		return false
	}
	jp, err := s.ServicePods.Pod(ctx, name)
	if errors.Is(err, kube.ErrNotFound) {
		return false
	} else if err != nil {
		s.Log.Warn("services: reading the job pod", "pod", name, "err", err)
		return true
	}
	return jp.Metadata.DeletionTimestamp == nil && jp.Status.Phase != "Succeeded" && jp.Status.Phase != "Failed"
}

// ensureServices starts l's service pod if it hasn't one, and records its
// status. It reports whether the pod should go on existing; when it can't
// tell (the API server didn't answer), it keeps it.
func (s *Server) ensureServices(ctx context.Context, l queue.Lease) bool {
	log := s.Log.With("task", l.Ref, "pod", l.Pod)
	st, started, err := s.E.Q.Services(ctx, l.Pod)
	if err != nil {
		log.Warn("services: reading their status", "err", err)
		return true
	} else if started && st.State == queue.ServicesFailed {
		// Kept for as long as the job runs, so it reads why.
		s.setServices(ctx, l.Pod, st)
		return false
	}
	fail := func(why string) bool {
		log.Warn("services: failed", "reason", why)
		s.setServices(ctx, l.Pod, queue.ServicesStatus{State: queue.ServicesFailed, Reason: why})
		return false
	}
	jp, err := s.ServicePods.Pod(ctx, l.Pod)
	if errors.Is(err, kube.ErrNotFound) {
		return false
	} else if err != nil {
		log.Warn("services: reading the job pod", "err", err)
		return true
	}
	// A lease is written by whoever can reach Redis, so only this install's
	// job pod of the lease's kind, owned by its Job, gets services.
	if jp.Metadata.Labels["app.kubernetes.io/instance"] != s.Cfg.Services.Instance || jp.Metadata.Labels["app.kubernetes.io/component"] != queue.PodKind(l.Kind) {
		return fail(fmt.Sprintf("pod %s isn't one of this install's %s pods", l.Pod, queue.PodKind(l.Kind)))
	}
	if jp.Metadata.DeletionTimestamp != nil || jp.Status.Phase == "Succeeded" || jp.Status.Phase == "Failed" {
		return false
	}
	i := slices.IndexFunc(jp.Metadata.OwnerReferences, func(o kube.OwnerReference) bool { return o.Kind == "Job" && o.APIVersion == "batch/v1" })
	if i < 0 {
		return fail(fmt.Sprintf("pod %s has no Job to own its services", l.Pod))
	}
	job := jp.Metadata.OwnerReferences[i]
	// Held to the install's limits here too: the lease, not the repo, is
	// what serve reads.
	rc := &config.Repo{Services: l.Services}
	if err := rc.ValidateServices(); err != nil {
		return fail(err.Error())
	}
	if err := s.Cfg.CheckServices(rc); err != nil {
		return fail(err.Error())
	}
	name := servicePodName(l.Pod)
	sp, err := s.ServicePods.Pod(ctx, name)
	if errors.Is(err, kube.ErrNotFound) && started {
		// Starting it again would hand the run empty services at another
		// address, in the middle of its work.
		return fail(fmt.Sprintf("the service pod %s was deleted (evicted, or deleted by hand)", name))
	} else if errors.Is(err, kube.ErrNotFound) {
		// The policy first, so the pod never runs without it.
		if s.Cfg.Services.NetworkPolicy {
			if err := s.ServicePods.CreateNetworkPolicy(ctx, s.serviceNetworkPolicy(name, l, job)); err != nil && !errors.Is(err, kube.ErrExists) {
				log.Warn("services: creating the NetworkPolicy", "err", err)
				return true
			}
		}
		if err := s.ServicePods.CreatePod(ctx, s.servicePod(name, l, job)); kube.Refused(err) {
			// Admission or a quota refusing it won't change by asking again.
			return fail(fmt.Sprintf("Kubernetes refused the service pod: %v", err))
		} else if err != nil && !errors.Is(err, kube.ErrExists) {
			log.Warn("services: creating the service pod", "err", err)
			return true
		}
		log.Info("services: started", "services_pod", name, "services", serviceNames(l.Services))
		s.setServices(ctx, l.Pod, queue.ServicesStatus{State: queue.ServicesStarting, Reason: "the service pod was just created"})
		return true
	} else if err != nil {
		log.Warn("services: reading the service pod", "err", err)
		return true
	}
	if sp.Metadata.Annotations[jobPodAnn] != l.Pod {
		return fail(fmt.Sprintf("pod %s exists and isn't this run's service pod", name))
	}
	if sp.Metadata.DeletionTimestamp != nil {
		return fail(fmt.Sprintf("the service pod %s is being deleted (evicted, or deleted by hand)", name))
	}
	// ready_timeout is for getting ready the first time. Once the services
	// were, a readiness probe missed under load isn't a death: only an exit
	// or a failed pod is. With no status at all (it lapsed while Redis was
	// out of reach) whether they were is unknown, so this pass only records
	// how they are and the next one holds them to it.
	wasReady := started && st.State == queue.ServicesReady
	timeout := s.Cfg.Services.ReadyTimeout
	if wasReady || !started {
		timeout = 0
	}
	cur := serviceStatus(sp, l.Services, timeout, time.Now())
	if cur.State == queue.ServicesFailed {
		return fail(cur.Reason)
	}
	if wasReady && cur.State == queue.ServicesStarting {
		log.Info("services: were ready, aren't for now", "reason", cur.Reason)
		cur = st
	}
	s.setServices(ctx, l.Pod, cur)
	return true
}

func (s *Server) setServices(ctx context.Context, pod string, st queue.ServicesStatus) {
	st.At = time.Now().UTC().Truncate(time.Second)
	if err := s.E.Q.SetServices(ctx, pod, st, servicesTTL); err != nil {
		s.Log.Warn("services: recording their status", "pod", pod, "err", err)
	}
}

func serviceNames(svcs []config.Service) []string {
	var out []string
	for _, v := range svcs {
		out = append(out, v.Name)
	}
	return out
}

// pullFailures and startFailures are the waiting reasons that mean a
// container won't start without someone changing something: a wrong or
// unreachable image, or a spec the runtime rejects.
var (
	pullFailures  = []string{"ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull"}
	startFailures = []string{"CreateContainerConfigError", "CreateContainerError", "RunContainerError", "CrashLoopBackOff"}
)

// serviceStatus reads how a service pod is doing: ready once every
// container is, failed as soon as one can't start or exits (the pod
// restarts nothing, so a crash is final), or when they aren't all ready
// timeout after the pod was created. A zero timeout never runs out.
func serviceStatus(p kube.Pod, svcs []config.Service, timeout time.Duration, now time.Time) queue.ServicesStatus {
	if p.Status.Phase == "Failed" {
		why := "the service pod failed"
		for _, c := range p.Status.Conditions {
			if c.Message != "" {
				why += ": " + c.Message
				break
			}
		}
		if t := terminated(p, svcs); t != "" {
			why = t
		}
		return queue.ServicesStatus{State: queue.ServicesFailed, Reason: why}
	}
	if t := terminated(p, svcs); t != "" {
		return queue.ServicesStatus{State: queue.ServicesFailed, Reason: t}
	}
	byName := map[string]kube.ContainerStatus{}
	for _, c := range p.Status.ContainerStatuses {
		byName[c.Name] = c
	}
	var waiting []string
	for _, v := range svcs {
		c, ok := byName[v.Name]
		switch {
		case ok && c.State.Waiting != nil && slices.Contains(pullFailures, c.State.Waiting.Reason):
			return queue.ServicesStatus{State: queue.ServicesFailed,
				Reason: fmt.Sprintf("service %s: image %s can't be pulled (%s): %s", v.Name, config.NormalizeImage(v.Image), c.State.Waiting.Reason, c.State.Waiting.Message)}
		case ok && c.State.Waiting != nil && slices.Contains(startFailures, c.State.Waiting.Reason):
			return queue.ServicesStatus{State: queue.ServicesFailed,
				Reason: fmt.Sprintf("service %s can't start (%s): %s", v.Name, c.State.Waiting.Reason, c.State.Waiting.Message)}
		case ok && c.Ready:
		case ok && c.State.Waiting != nil:
			waiting = append(waiting, fmt.Sprintf("%s: %s", v.Name, c.State.Waiting.Reason))
		case ok && c.State.Running != nil:
			waiting = append(waiting, v.Name+": running, not ready yet")
		default:
			waiting = append(waiting, v.Name+": not started")
		}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == "PodScheduled" && c.Status == "False" {
			waiting = []string{fmt.Sprintf("not scheduled (%s): %s", c.Reason, c.Message)}
		}
	}
	if len(waiting) == 0 && p.Status.PodIP != "" {
		return queue.ServicesStatus{State: queue.ServicesReady, IP: p.Status.PodIP}
	}
	if len(waiting) == 0 {
		waiting = []string{"no address yet"}
	}
	why := strings.Join(waiting, "; ")
	if timeout > 0 && !p.Metadata.CreationTimestamp.IsZero() && now.Sub(p.Metadata.CreationTimestamp) > timeout {
		return queue.ServicesStatus{State: queue.ServicesFailed, Reason: fmt.Sprintf("the services weren't ready after %s (services.ready_timeout): %s", timeout, why)}
	}
	return queue.ServicesStatus{State: queue.ServicesStarting, Reason: why}
}

// terminated describes the first service that exited, with the end of its
// log the kubelet kept as its termination message.
func terminated(p kube.Pod, svcs []config.Service) string {
	for _, c := range p.Status.ContainerStatuses {
		if !slices.ContainsFunc(svcs, func(v config.Service) bool { return v.Name == c.Name }) {
			continue
		}
		t := c.State.Terminated
		if t == nil {
			t = c.LastState.Terminated
		}
		if t == nil {
			continue
		}
		why := fmt.Sprintf("service %s exited with code %d", c.Name, t.ExitCode)
		if t.Reason != "" {
			why += " (" + t.Reason + ")"
		}
		if m := strings.TrimSpace(t.Message); m != "" {
			why += ":\n" + m
		}
		return why
	}
	return ""
}

func (s *Server) serviceMeta(name string, l queue.Lease, job kube.OwnerReference) map[string]any {
	return map[string]any{
		"name": name,
		"labels": map[string]string{
			"app.kubernetes.io/name": "devbot", "app.kubernetes.io/instance": s.Cfg.Services.Instance,
			"app.kubernetes.io/component": "services", runLabel: job.UID,
		},
		"annotations": map[string]string{jobPodAnn: l.Pod, taskAnn: l.Ref, "sidecar.istio.io/inject": "false"},
		// The Job's, so Kubernetes deletes them with it even when serve
		// never gets to.
		"ownerReferences": []map[string]any{{"apiVersion": "batch/v1", "kind": "Job", "name": job.Name, "uid": job.UID}},
	}
}

// servicePod is the pod a run's services run in: one container each,
// sharing the pod's loopback as a CI job's services do, with no API token,
// no volumes, nothing restarted, and a deadline of its own.
func (s *Server) servicePod(name string, l queue.Lease, job kube.OwnerReference) map[string]any {
	c := s.Cfg.Services
	deadline := c.ActiveDeadline.Implement
	if l.Kind == queue.KindReview {
		deadline = c.ActiveDeadline.Review
	}
	var containers []map[string]any
	for _, v := range l.Services {
		req, lim := s.Cfg.ServiceResources(v)
		ctr := map[string]any{
			"name": v.Name, "image": config.NormalizeImage(v.Image), "imagePullPolicy": "IfNotPresent",
			"resources": map[string]any{
				"requests": map[string]string{"cpu": req.CPU, "memory": req.Memory},
				// The disk limit is the install's alone: its request defaults to it.
				"limits": map[string]string{"cpu": lim.CPU, "memory": lim.Memory, "ephemeral-storage": c.Resources.Limits.EphemeralStorage},
			},
			// Root in the pod's user namespace, as the images expect (a
			// database's entrypoint drops to its own user), like the agent.
			"securityContext": map[string]any{
				"allowPrivilegeEscalation": false,
				"capabilities":             map[string]any{"drop": []string{"NET_RAW", "MKNOD"}},
			},
			// The end of its log, for the reason on the issue when it exits.
			"terminationMessagePolicy": "FallbackToLogsOnError",
		}
		if len(v.Command) > 0 {
			ctr["command"] = v.Command
		}
		if len(v.Args) > 0 {
			ctr["args"] = v.Args
		}
		var env []map[string]string
		for _, k := range slices.Sorted(maps.Keys(v.Env)) {
			env = append(env, map[string]string{"name": k, "value": v.Env[k]})
		}
		if env != nil {
			ctr["env"] = env
		}
		var ports []map[string]any
		for _, p := range v.Ports {
			ports = append(ports, map[string]any{"containerPort": p, "protocol": "TCP"})
		}
		if ports != nil {
			ctr["ports"] = ports
		}
		probe := map[string]any{"periodSeconds": 2, "timeoutSeconds": 5}
		switch {
		case v.Ready.TCP != 0:
			probe["tcpSocket"] = map[string]any{"port": v.Ready.TCP}
			ctr["readinessProbe"] = probe
		case len(v.Ready.Exec) > 0:
			probe["exec"] = map[string]any{"command": v.Ready.Exec}
			ctr["readinessProbe"] = probe
		}
		containers = append(containers, ctr)
	}
	spec := map[string]any{
		"restartPolicy":                 "Never",
		"enableServiceLinks":            false,
		"automountServiceAccountToken":  false,
		"activeDeadlineSeconds":         int(deadline.Seconds()),
		"terminationGracePeriodSeconds": 5,
		"securityContext":               map[string]any{"seccompProfile": map[string]string{"type": "RuntimeDefault"}},
		"containers":                    containers,
	}
	if c.ServiceAccount != "" {
		spec["serviceAccountName"] = c.ServiceAccount
	}
	if c.UserNamespaces {
		spec["hostUsers"] = false
	}
	if c.RuntimeClassName != "" {
		spec["runtimeClassName"] = c.RuntimeClassName
	}
	if len(c.NodeSelector) > 0 {
		spec["nodeSelector"] = c.NodeSelector
	}
	if len(c.Tolerations) > 0 {
		spec["tolerations"] = c.Tolerations
	}
	if len(c.Affinity) > 0 {
		spec["affinity"] = c.Affinity
	}
	var pull []map[string]string
	for _, n := range c.ImagePullSecrets {
		pull = append(pull, map[string]string{"name": n})
	}
	if pull != nil {
		spec["imagePullSecrets"] = pull
	}
	return map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": s.serviceMeta(name, l, job), "spec": spec}
}

// serviceNetworkPolicy admits the run's own job pod to its services' ports,
// and nothing else: the chart's policy for service pods denies all other
// ingress, and policies only add to what is allowed.
func (s *Server) serviceNetworkPolicy(name string, l queue.Lease, job kube.OwnerReference) map[string]any {
	var ports []map[string]any
	for _, v := range l.Services {
		for _, p := range v.Ports {
			ports = append(ports, map[string]any{"port": p, "protocol": "TCP"})
		}
	}
	ingress := []map[string]any{}
	if len(ports) > 0 {
		ingress = append(ingress, map[string]any{
			"from":  []map[string]any{{"podSelector": map[string]any{"matchLabels": map[string]string{jobUIDLabel: job.UID}}}},
			"ports": ports,
		})
	}
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": s.serviceMeta(name, l, job),
		"spec": map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]string{
				"app.kubernetes.io/instance": s.Cfg.Services.Instance, "app.kubernetes.io/component": "services", runLabel: job.UID,
			}},
			"policyTypes": []string{"Ingress"},
			"ingress":     ingress,
		},
	}
}

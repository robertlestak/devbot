package server

import (
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/kube"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// A harness whose agent stopped answering learns how the agent container
// ended from serve, which alone can read the pod: now, or before the kubelet
// restarted it. Only for a lease that asks, and only from this install's job
// pod of the lease's kind.
func TestRecordAgentExits(t *testing.T) {
	s, fc := servicesServer(t)
	s.JobPods = fc
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	jobPod := func(name, component string, st kube.ContainerStatus) {
		fc.pods[name] = kube.Pod{
			Metadata: kube.ObjectMeta{Name: name, Labels: map[string]string{"app.kubernetes.io/instance": "devbot", "app.kubernetes.io/component": component}},
			Status: kube.PodStatus{Phase: "Running", ContainerStatuses: []kube.ContainerStatus{{Name: "harness"}},
				InitContainerStatuses: []kube.ContainerStatus{{Name: "model-proxy"}, st}},
		}
	}
	lease := func(kind, ref, pod string, lost bool) {
		t.Helper()
		if ok, err := s.E.Q.Acquire(t.Context(), kind, ref, pod, time.Minute); err != nil || !ok {
			t.Fatal("lease", err)
		}
		s.E.Q.Describe(t.Context(), kind, ref, pod, queue.LeaseDetail{Pod: pod, AgentLost: lost})
	}
	oom := &kube.Terminated{ExitCode: 137, Reason: "OOMKilled", FinishedAt: at}

	jobPod("pod-ended", "implement", kube.ContainerStatus{Name: "agent", State: kube.ContainerState{Terminated: oom}})
	lease(queue.KindImplement, "gitea:acme/app#1", "pod-ended", true)
	jobPod("pod-restarted", "implement", kube.ContainerStatus{Name: "agent", RestartCount: 1, LastState: kube.ContainerState{Terminated: &kube.Terminated{Reason: "Completed", FinishedAt: at}}})
	lease(queue.KindPlan, "gitea:acme/app#2", "pod-restarted", true)
	jobPod("pod-running", "review", kube.ContainerStatus{Name: "agent"})
	lease(queue.KindReview, "gitea:acme/app#3", "pod-running", true)
	jobPod("pod-unasked", "implement", kube.ContainerStatus{Name: "agent", State: kube.ContainerState{Terminated: oom}})
	lease(queue.KindImplement, "gitea:acme/app#4", "pod-unasked", false)
	jobPod("pod-wrong-kind", "implement", kube.ContainerStatus{Name: "agent", State: kube.ContainerState{Terminated: oom}})
	lease(queue.KindReview, "gitea:acme/app#5", "pod-wrong-kind", true)
	lease(queue.KindImplement, "gitea:acme/app#6", "pod-gone", true)

	for range 2 { // and again: nothing changes
		s.RecordAgentExits(t.Context())
		for pod, want := range map[string]*queue.AgentExit{
			"pod-ended":      {Reason: "OOMKilled", ExitCode: 137, FinishedAt: at},
			"pod-restarted":  {Reason: "Completed", FinishedAt: at},
			"pod-running":    nil,
			"pod-unasked":    nil,
			"pod-wrong-kind": nil,
			"pod-gone":       nil,
		} {
			got, ok, err := s.E.Q.AgentExit(t.Context(), pod)
			if err != nil {
				t.Fatal(err)
			}
			if want == nil && ok {
				t.Errorf("%s: recorded %+v", pod, got)
			} else if want != nil && (!ok || got != *want) {
				t.Errorf("%s: got %+v (%v), want %+v", pod, got, ok, *want)
			}
		}
	}
}

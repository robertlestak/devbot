package server

import (
	"context"
	"errors"
	"time"

	"git.shdw.tech/rob/devbot/internal/kube"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// JobPods reads job pods: a kube.Client in the cluster, when serve has one
// (pod_labels or services).
type JobPods interface {
	Pod(ctx context.Context, name string) (kube.Pod, error)
}

// agentContainer is the job pod's agent container, as the chart names it.
const agentContainer = "agent"

// agentExitTTL keeps a record past the harness's wait for it.
const agentExitTTL = 10 * time.Minute

// agentExitLoop answers harnesses whose agent stopped answering, every 2s.
func (s *Server) agentExitLoop(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RecordAgentExits(ctx)
		}
	}
}

// RecordAgentExits is one pass of agentExitLoop: for each live lease whose
// harness says its agent stopped answering (AgentLost), it reads how the
// pod's agent container ended and records it for the harness's
// attempt-failed comment. A container that hasn't ended yet, as the kubelet
// sees it, is read again on the next pass. It holds no state, so it is safe
// to repeat, on any replica that leads.
func (s *Server) RecordAgentExits(ctx context.Context) {
	leases, err := s.E.Q.Running(ctx)
	if err != nil {
		s.Log.Warn("agent exits: reading leases", "err", err)
		return
	}
	for _, l := range leases {
		if !l.AgentLost || l.Pod == "" {
			continue
		}
		log := s.Log.With("task", l.Ref, "pod", l.Pod)
		p, err := s.JobPods.Pod(ctx, l.Pod)
		if errors.Is(err, kube.ErrNotFound) {
			continue
		} else if err != nil {
			log.Warn("agent exits: reading the job pod", "err", err)
			continue
		}
		// A lease is written by whoever can reach Redis, so only this
		// install's job pod of the lease's kind is read.
		if p.Metadata.Labels["app.kubernetes.io/instance"] != s.jobPodInstance() || p.Metadata.Labels["app.kubernetes.io/component"] != queue.PodKind(l.Kind) {
			continue
		}
		e, ok := agentExit(p)
		if !ok {
			continue
		}
		if have, ok, err := s.E.Q.AgentExit(ctx, l.Pod); err == nil && ok && have.FinishedAt.Equal(e.FinishedAt) && have.Reason == e.Reason && have.ExitCode == e.ExitCode {
			continue
		}
		log.Info("agent exits: the job's agent container ended", "reason", e.Reason, "exit_code", e.ExitCode, "message", e.Message)
		if err := s.E.Q.SetAgentExit(ctx, l.Pod, e, agentExitTTL); err != nil {
			log.Warn("agent exits: recording", "err", err)
		}
	}
}

// agentExit is how a job pod's agent container last ended: now, or before
// the kubelet restarted it. It is a native sidecar (an init container with
// restartPolicy Always), so its status is among the init containers'.
func agentExit(p kube.Pod) (queue.AgentExit, bool) {
	for _, c := range p.Status.InitContainerStatuses {
		if c.Name != agentContainer {
			continue
		}
		t := c.State.Terminated
		if t == nil {
			t = c.LastState.Terminated
		}
		if t == nil {
			return queue.AgentExit{}, false
		}
		return queue.AgentExit{Reason: t.Reason, ExitCode: t.ExitCode, Message: t.Message, FinishedAt: t.FinishedAt}, true
	}
	return queue.AgentExit{}, false
}

// jobPodInstance is the release whose job pods serve reads.
func (s *Server) jobPodInstance() string {
	if s.Cfg.PodLabels.Enabled {
		return s.Cfg.PodLabels.Instance
	}
	return s.Cfg.Services.Instance
}

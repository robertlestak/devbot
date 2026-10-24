package server

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/kube"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// PodLabeler reads and labels pods: a kube.Client in the cluster.
type PodLabeler interface {
	PodMeta(ctx context.Context, name string) (kube.PodMeta, error)
	PatchPodMeta(ctx context.Context, name string, m kube.PodMeta) error
}

// podLabelLoop labels running job pods every 10s. KEDA starts a pod before
// it knows which job the pod will take and can't label it per job, so each
// harness records its attribution on its lease and serve puts it on the pod.
// Job pods so keep no Kubernetes API access of their own.
func (s *Server) podLabelLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.labelPods(ctx)
		}
	}
}

// labelPods puts each live lease's pod labels and annotations on its pod.
// It is safe to repeat: a pod that already carries them isn't patched, and
// labelled remembers which were done only to save the reads.
func (s *Server) labelPods(ctx context.Context) {
	leases, err := s.E.Q.Running(ctx)
	if err != nil {
		s.Log.Warn("labelling job pods: reading leases", "err", err)
		return
	}
	live := map[string]bool{}
	for _, l := range leases {
		if l.Pod == "" || len(l.PodLabels) == 0 {
			continue
		}
		live[l.Pod] = true
		want, dropped := podMetaFromLease(l.PodLabels, l.PodAnnotations)
		fp, _ := json.Marshal(want)
		if s.labelled[l.Pod] == string(fp) {
			continue
		}
		if len(dropped) > 0 {
			s.Log.Warn("labelling job pods: a lease carries metadata devbot doesn't set; leaving it off", "pod", l.Pod, "task", l.Ref, "keys", dropped)
		}
		have, err := s.Pods.PodMeta(ctx, l.Pod)
		if errors.Is(err, kube.ErrNotFound) {
			continue
		} else if err != nil {
			s.Log.Warn("labelling job pods: reading the pod", "pod", l.Pod, "task", l.Ref, "err", err)
			continue
		}
		// Only this install's pod of the lease's kind: a lease is written by
		// whoever can reach Redis, so its pod name alone isn't trusted.
		if have.Labels["app.kubernetes.io/instance"] != s.Cfg.PodLabels.Instance || have.Labels["app.kubernetes.io/component"] != queue.PodKind(l.Kind) {
			s.Log.Warn("labelling job pods: a lease names a pod that isn't one of this install's job pods; leaving it alone", "pod", l.Pod, "task", l.Ref, "kind", l.Kind)
			s.labelled[l.Pod] = string(fp)
			continue
		}
		if !hasAll(have.Labels, want.Labels) || !hasAll(have.Annotations, want.Annotations) {
			if err := s.Pods.PatchPodMeta(ctx, l.Pod, want); err != nil {
				s.Log.Warn("labelling job pods: patching the pod", "pod", l.Pod, "task", l.Ref, "err", err)
				continue
			}
		}
		s.labelled[l.Pod] = string(fp)
	}
	for p := range s.labelled {
		if !live[p] {
			delete(s.labelled, p)
		}
	}
}

// maxPodAnnotation caps an annotation value: the longest devbot sets is an
// origin URL, and the API server limits the total size of a pod's annotations.
const maxPodAnnotation = 1024

// podMetaFromLease keeps only the metadata a harness sets: keys under
// devbot's own prefixes, label values valid as such, annotations of bounded
// size. Anything that reaches Redis can write a lease, the agent in a job pod
// included, so a lease mustn't be able to make serve overwrite a pod's Job
// selector labels or component, or write arbitrary annotations. dropped
// lists the keys left off, sorted.
func podMetaFromLease(labels, annotations map[string]string) (m kube.PodMeta, dropped []string) {
	m = kube.PodMeta{Labels: map[string]string{}, Annotations: map[string]string{}}
	for k, v := range labels {
		if ownPodKey(k) && config.ValidLabelValue(v) {
			m.Labels[k] = v
		} else {
			dropped = append(dropped, "label "+k)
		}
	}
	for k, v := range annotations {
		if ownPodKey(k) && len(v) <= maxPodAnnotation {
			m.Annotations[k] = v
		} else {
			dropped = append(dropped, "annotation "+k)
		}
	}
	slices.Sort(dropped)
	return m, dropped
}

// ownPodKey reports whether k is one of the pod metadata keys devbot sets.
func ownPodKey(k string) bool {
	if name, ok := strings.CutPrefix(k, core.PodStaticLabelPrefix); ok {
		return config.ValidLabelKey(name)
	}
	if name, ok := strings.CutPrefix(k, core.PodLabelPrefix); ok {
		return config.ValidLabelKey(name)
	}
	return false
}

// hasAll reports whether have has every key of want, with its value.
func hasAll(have, want map[string]string) bool {
	for k, v := range want {
		if hv, ok := have[k]; !ok || hv != v {
			return false
		}
	}
	return true
}

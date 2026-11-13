package core

import (
	"context"

	"git.shdw.tech/rob/devbot/internal/audit"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// docker is whether the task's runs get a Docker daemon: as the repo's
// policy and the install settle it, or else as the task's base branch shows
// (config.RepoCache.DetectDocker), with what showed it. When devbot can't
// tell, the runs get none: a guess either reserves a daemon for nothing or
// still leaves the tests without one.
func (t *task) docker(ctx context.Context) (bool, config.DockerSignal, error) {
	if on, ok := t.e.Cfg.DockerSettled(t.rc); ok {
		return on, config.DockerSignal{}, nil
	}
	base, err := t.base(ctx)
	if err != nil {
		return false, config.DockerSignal{}, err
	}
	s, err := t.e.Repos.DetectDocker(ctx, t.it.Ref.Repo, base)
	if err != nil {
		t.e.Log.Warn("can't tell whether the repo's tests need Docker, so its runs get no daemon", "task", t.it.Ref, "base", base, "err", err)
		t.e.Count(ctx, "docker_detection_errors", []string{t.it.Ref.Repo.String()}, 1)
		return false, config.DockerSignal{}, nil
	}
	return s.Path != "", s, nil
}

// enqueue queues j where the task's runs are taken from: the Docker lists
// when they get a daemon. A daemon its base branch turned on is logged and
// audited, naming the file, for each job it is given to.
func (t *task) enqueue(ctx context.Context, j queue.Job) (bool, error) {
	on, s, err := t.docker(ctx)
	if err != nil {
		return false, err
	}
	j.Docker, j.DockerWhy = on, s.String()
	added, err := t.e.Q.Enqueue(ctx, j)
	if added && s.Path != "" {
		t.e.Log.Info("the base branch shows the repo's tests start containers, so the job gets a Docker daemon", "task", t.it.Ref, "kind", j.Kind, "base", t.baseBranch, "file", s.Path)
		t.e.Audit.Log(t.e.TaskContext(ctx, t.it), audit.Record{Event: "docker.detected", ActorKind: "devbot",
			Detail: map[string]any{"kind": j.Kind, "base": t.baseBranch, "file": s.Path, "why": s.Why}})
	}
	return added, err
}

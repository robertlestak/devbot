package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// JobView is one running, queued or delayed job, with its task's title and
// links to the forge.
type JobView struct {
	Kind        string    `json:"kind"`
	Task        string    `json:"task"`
	Repo        string    `json:"repo"`
	Title       string    `json:"title,omitempty"`
	URL         string    `json:"url,omitempty"`
	PR          int       `json:"pr,omitempty"`
	PRURL       string    `json:"pr_url,omitempty"`
	Mode        string    `json:"mode,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	Attempt     int       `json:"attempt,omitempty"`
	MaxAttempts int       `json:"max_attempts,omitempty"`
	Round       int       `json:"round,omitempty"`
	MaxRounds   int       `json:"max_rounds,omitempty"`
	Holder      string    `json:"holder,omitempty"`
	Started     time.Time `json:"started,omitzero"`
	Heartbeat   time.Time `json:"heartbeat,omitzero"`
	EnqueuedAt  time.Time `json:"enqueued_at,omitzero"`
	ReleaseAt   time.Time `json:"release_at,omitzero"`
	// Retries is how many times the job has been put back after a usage
	// limit or a failed review.
	Retries int `json:"retries,omitempty"`
}

// Jobs is what devbot is doing and has waiting.
type Jobs struct {
	Running   []JobView  `json:"running"`
	Queued    []JobView  `json:"queued"`
	Delayed   []JobView  `json:"delayed"`
	HoldUntil *time.Time `json:"ratelimit_hold_until,omitempty"`
}

// jobs lists the jobs c may see: like tasks, a job on a repo c can't read
// isn't there, its repo, title and holder included.
func (s *Server) jobs(ctx context.Context, c core.Caller, repo string) (Jobs, error) {
	out := Jobs{Running: []JobView{}, Queued: []JobView{}, Delayed: []JobView{}}
	running, err := s.E.Q.Running(ctx)
	if err != nil {
		return out, err
	}
	queued, err := s.E.Q.Queued(ctx)
	if err != nil {
		return out, err
	}
	delayed, err := s.E.Q.Delayed(ctx)
	if err != nil {
		return out, err
	}
	for _, l := range running {
		v, ok, err := s.jobView(ctx, c, repo, queue.Job{Kind: l.Kind, Ref: l.Ref, PR: l.PR, Mode: l.Mode, Reason: l.Reason})
		if err != nil {
			return out, err
		}
		if ok {
			v.Attempt, v.MaxAttempts, v.Round, v.MaxRounds = l.Attempt, l.MaxAttempts, l.Round, l.MaxRounds
			v.Holder, v.Started, v.Heartbeat = l.Holder, l.Started, l.Heartbeat
			out.Running = append(out.Running, v)
		}
	}
	for _, j := range queued {
		if v, ok, err := s.jobView(ctx, c, repo, j); err != nil {
			return out, err
		} else if ok {
			out.Queued = append(out.Queued, v)
		}
	}
	for _, d := range delayed {
		if v, ok, err := s.jobView(ctx, c, repo, d.Job); err != nil {
			return out, err
		} else if ok {
			v.ReleaseAt = d.Release
			out.Delayed = append(out.Delayed, v)
		}
	}
	if h, _ := s.E.Q.Hold(ctx); time.Now().Before(h) {
		out.HoldUntil = &h
	}
	return out, nil
}

func (s *Server) jobView(ctx context.Context, c core.Caller, repo string, j queue.Job) (JobView, bool, error) {
	v := JobView{Kind: j.Kind, Task: j.Ref, PR: j.PR, Mode: j.Mode, Reason: firstLine(j.Reason), EnqueuedAt: j.EnqueuedAt, Retries: j.RateLimitN + j.FailN}
	ref, err := forge.ParseItem(j.Ref)
	if err != nil {
		// Without a repo there is nothing to check a signed-in person's
		// access against.
		return v, c.User == nil && repo == "", nil
	}
	v.Repo = ref.Repo.String()
	if repo != "" && v.Repo != repo {
		return v, false, nil
	}
	if ok, err := s.E.CanRead(ctx, c, ref.Repo); err != nil || !ok {
		return v, false, err
	}
	p, err := s.E.Forges.For(ref.Repo)
	if err != nil {
		return v, true, nil
	}
	v.URL = p.IssueURL(ref)
	if v.PR > 0 {
		v.PRURL = p.PRURL(ref.Repo, v.PR)
	}
	v.Title = s.titles.get(ctx, p, ref)
	return v, true, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return s
}

// titleCache remembers issue titles for the jobs list, which the UI polls
// every few seconds, for a minute.
type titleCache struct {
	mu sync.Mutex
	m  map[string]cachedTitle
}

type cachedTitle struct {
	title string
	at    time.Time
}

func (t *titleCache) get(ctx context.Context, p forge.Provider, ref forge.ItemRef) string {
	key := ref.String()
	t.mu.Lock()
	c, ok := t.m[key]
	t.mu.Unlock()
	if ok && time.Since(c.at) < time.Minute {
		return c.title
	}
	it, err := p.GetItem(ctx, ref)
	if err != nil {
		return c.title
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil || len(t.m) >= 10000 {
		t.m = map[string]cachedTitle{}
	}
	t.m[key] = cachedTitle{it.Title, time.Now()}
	return it.Title
}

func (s *Server) apiJobs(w http.ResponseWriter, r *http.Request) {
	js, err := s.jobs(r.Context(), callerOf(r), r.URL.Query().Get("repo"))
	s.writeJSON(w, r, js, err)
}

func (s *Server) apiTimeline(w http.ResponseWriter, r *http.Request) {
	td, err := s.E.TaskDetail(r.Context(), callerOf(r), r.URL.Query().Get("ref"))
	s.writeJSON(w, r, td, err)
}

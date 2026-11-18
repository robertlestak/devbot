package core

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// What the web UI shows beyond what the connectors list: each task's PR,
// latest update and how long it has been in its state, and one task's
// timeline. Like everything else here they are read from the forge, through
// the same visibility rule (CanRead).

// detailTTL bounds how long a task's details are remembered. They are also
// dropped as soon as the issue changes, so this only matters for a PR or
// label event the forge didn't count as a change to the issue.
const detailTTL = time.Minute

const detailMax = 10000

type detailCache struct {
	mu sync.Mutex
	m  map[string]taskDetail
}

type taskDetail struct {
	version    string // the issue's update time and state, when read
	at         time.Time
	lastUpdate string
	pr         int
	since      time.Time
	base       string
}

func (c *detailCache) get(ref, version string) (taskDetail, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.m[ref]
	return d, ok && d.version == version && time.Since(d.at) < detailTTL
}

func (c *detailCache) put(ref string, d taskDetail) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]taskDetail{}
	}
	makeRoom(c.m, detailMax)
	c.m[ref] = d
}

// ListTasksDetailed is ListTasks with each task's PR, latest update and the
// time it entered its state filled in, and, as there, what it could read
// with any error listing them.
func (e *Engine) ListTasksDetailed(ctx context.Context, c Caller, repo, state, originPrefix string) ([]TaskStatus, error) {
	if strings.TrimPrefix(state, "devbot/") == KindParent {
		return e.ListParents(ctx, c, repo, originPrefix)
	}
	items, lerr := e.listItems(ctx, c, repo, state, originPrefix)
	out := make([]TaskStatus, 0, len(items))
	for _, it := range items {
		st := e.statusOf(ctx, it, nil)
		version := it.UpdatedAt.String() + "|" + st.State
		d, ok := e.details.get(st.Task, version)
		if !ok {
			p, err := e.provider(it.Ref.Repo)
			if err != nil {
				return nil, err
			}
			cs, err := p.ListComments(ctx, it.Ref)
			if err != nil {
				return nil, err
			}
			if d, err = e.detail(ctx, p, it, cs); err != nil {
				return nil, err
			}
			d.version = version
			e.details.put(st.Task, d)
		}
		d.apply(e, it, &st)
		out = append(out, st)
	}
	return out, lerr
}

// detail reads what ListTasksDetailed adds from a task's comments and label
// history.
func (e *Engine) detail(ctx context.Context, p forge.Provider, it forge.Item, cs []forge.Comment) (taskDetail, error) {
	d := taskDetail{at: time.Now(), base: e.shownBase(ctx, it, cs)}
	bots := e.bots(p)
	for i := len(cs) - 1; i >= 0; i-- {
		if forge.HasLogin(bots, cs[i].Author) {
			d.lastUpdate = firstLine(stripMarkers(cs[i].Body))
			break
		}
	}
	if s, ok := lastStatus(statuses(cs, bots...), "pr"); ok {
		d.pr = s.PR
	}
	if state := State(it); state != "" {
		evs, err := p.LabelEvents(ctx, it.Ref)
		if err != nil {
			return d, err
		}
		for i := len(evs) - 1; i >= 0; i-- {
			if evs[i].Label == state && evs[i].Added {
				d.since = evs[i].At
				break
			}
		}
	}
	return d, nil
}

func (d taskDetail) apply(e *Engine, it forge.Item, st *TaskStatus) {
	st.LastUpdate = d.lastUpdate
	if d.base != "" {
		st.Base = d.base
	}
	if d.pr > 0 {
		st.PRNumber = d.pr
		if p, err := e.provider(it.Ref.Repo); err == nil {
			st.PR = p.PRURL(it.Ref.Repo, d.pr)
		}
	}
	if !d.since.IsZero() {
		t := d.since
		st.StateSince = &t
	}
}

// TimelineEntry is one thing that happened to a task: an event from the feed
// or a status comment devbot posted on the issue.
type TimelineEntry struct {
	Time    time.Time `json:"time"`
	Source  string    `json:"source"` // "event" | "comment"
	Type    string    `json:"type"`   // the event's type, or the status's kind
	State   string    `json:"state,omitempty"`
	Mode    string    `json:"mode,omitempty"`
	Attempt int       `json:"attempt,omitempty"`
	PR      int       `json:"pr,omitempty"`
	PRURL   string    `json:"pr_url,omitempty"`
	Summary string    `json:"summary,omitempty"`
	// Text is a status comment's body without devbot's markers: the reason
	// for a fix round or a needs-human, a question, a failure's output.
	Text string `json:"text,omitempty"`
}

// TaskDetail is one task with its links and its timeline, oldest first.
type TaskDetail struct {
	TaskStatus
	Branch    string          `json:"branch,omitempty"`
	BranchURL string          `json:"branch_url,omitempty"`
	Timeline  []TimelineEntry `json:"timeline"`
}

// timelineScan bounds how far back the feed is read for one task's events.
const timelineScan = 20

const timelineTextMax = 4000

// TaskDetail reads a task's timeline for c. A task on a repo c can't read is
// not found.
func (e *Engine) TaskDetail(ctx context.Context, c Caller, refStr string) (TaskDetail, error) {
	it, p, _, err := e.load(ctx, c, refStr)
	if err != nil {
		return TaskDetail{}, err
	}
	cs, err := p.ListComments(ctx, it.Ref)
	if err != nil {
		return TaskDetail{}, err
	}
	d, err := e.detail(ctx, p, it, cs)
	if err != nil {
		return TaskDetail{}, err
	}
	td := TaskDetail{TaskStatus: e.statusOf(ctx, it, nil), Timeline: []TimelineEntry{}}
	d.apply(e, it, &td.TaskStatus)
	if isParent(it, e.meta(ctx, it, cs)) {
		// A parent has no PR or branch: its children have.
		if td.TaskStatus, err = e.parentStatus(ctx, c, it, cs); err != nil {
			return TaskDetail{}, err
		}
	} else if pr, err := e.FindPR(ctx, it); err != nil {
		return TaskDetail{}, err
	} else if pr != nil {
		td.PR, td.PRNumber = pr.URL, pr.Number
		td.Note = e.rebaseNote(ctx, it, pr)
		td.Branch, td.BranchURL = pr.HeadBranch, p.BranchURL(pr.Repo, pr.HeadBranch)
	}

	// The feed is read newest first.
	before := ""
	for i := 0; i < timelineScan; i++ {
		evs, next, err := e.Q.EventsBefore(ctx, before, 500)
		if err != nil {
			return TaskDetail{}, err
		}
		for _, ev := range evs {
			if ev.Task == td.Task {
				td.Timeline = append(td.Timeline, eventEntry(ev))
			}
		}
		if len(evs) < 500 {
			break
		}
		before = next
	}
	slices.Reverse(td.Timeline)
	for _, s := range statuses(cs, e.bots(p)...) {
		text := truncate(stripMarkers(s.Body), timelineTextMax)
		te := TimelineEntry{Time: s.CreatedAt, Source: "comment", Type: s.Kind, Mode: s.Mode,
			Attempt: s.Attempt, PR: s.PR, Summary: firstLine(text), Text: text}
		if s.PR > 0 {
			te.PRURL = p.PRURL(it.Ref.Repo, s.PR)
		}
		td.Timeline = append(td.Timeline, te)
	}
	// Forges keep a comment's time to the second, so the timeline is
	// ordered to the second; within one, events come before comments,
	// each in their own order.
	sort.SliceStable(td.Timeline, func(i, j int) bool {
		return td.Timeline[i].Time.Truncate(time.Second).Before(td.Timeline[j].Time.Truncate(time.Second))
	})
	return td, nil
}

func eventEntry(ev queue.Event) TimelineEntry {
	return TimelineEntry{Time: ev.Time, Source: "event", Type: ev.Type, State: ev.State, Summary: ev.Summary, PRURL: ev.PR}
}

// truncate cuts s to at most n bytes, on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "\n..."
}

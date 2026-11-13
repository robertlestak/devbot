package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"git.shdw.tech/rob/devbot/internal/queue"
)

// Feed is the event feed the notifier follows, and the leader's fenced
// writes it keeps its place with: queue.Store's.
type Feed interface {
	Events(ctx context.Context, cursor string, limit int64) ([]queue.Event, string, error)
	EventsExpired(ctx context.Context, cursor string) (bool, error)
	SetFenced(ctx context.Context, key, field, value, token string) (bool, error)
}

const (
	// notifierKey holds the notifier's cursor in the feed.
	notifierKey = prefix + "notifier"
	// postsKey is a hash of task ref to its post: the status message in
	// its thread and the last event posted there.
	postsKey = prefix + "posts"
)

// post is what the notifier has posted in a task's thread.
type post struct {
	// TS is the task's status message, updated in place with each event.
	TS string `json:"ts,omitempty"`
	// Last is the last event posted, so one delivered again after a
	// handover or a retry isn't posted twice.
	Last string `json:"last,omitempty"`
}

// notice is what an event posts in its task's thread: a status, and for
// the events a person should see, a reply too. The reply is the event in
// full, and what pings the thread, so the status then only says the state
// and, for one a person acts on, points at the reply.
type notice struct {
	status string
	reply  func(e queue.Event) string
}

// notices are the events the notifier posts. The rest only move it on.
var notices = map[string]notice{
	"task.started": {status: "working on it"},
	"task.question": {status: "has a question: see the reply below", reply: func(e queue.Event) string {
		return "Question: " + summary(e) + "\nAnswer on " + Link(e.URL, e.Task) + "."
	}},
	"task.answered":          {status: "answered"},
	"task.pr_opened":         {status: "PR opened"},
	"task.changes_requested": {status: "changes requested"},
	"task.needs_approval": {status: "waiting for approval: see the reply below", reply: func(e queue.Event) string {
		return "The change touches files a person has to approve; see " + Link(e.URL, e.Task) + "."
	}},
	"task.approved": {status: "approved"},
	"task.merged": {status: "merged", reply: func(e queue.Event) string {
		return "Merged: " + Link(e.PR, "the PR") + "."
	}},
	"task.needs_human": {status: "stopped, needs a person: see the reply below", reply: func(e queue.Event) string {
		return "devbot stopped: " + summary(e) + ". " + Link(e.URL, e.Task) + " says why."
	}},
	"task.cancelled": {status: "cancelled", reply: func(e queue.Event) string {
		return "Cancelled: " + summary(e) + "."
	}},
	"task.retried": {status: "retried"},
}

// maxSummary is the most of an event's summary a message shows.
const maxSummary = 300

// summary is e's summary as a message shows it: its first line, cut short,
// escaped. A summary is often a model's words.
func summary(e queue.Event) string {
	s, _, _ := strings.Cut(strings.TrimSpace(e.Summary), "\n")
	if utf8.RuneCountInString(s) > maxSummary {
		s = string([]rune(s)[:maxSummary]) + "…"
	}
	return Escape(s)
}

// statusLine is the task's status message after e: the task, what
// happened, the summary unless a reply carries it, and the PR. An event of
// no known type (the state a reconcile read) shows its state.
func statusLine(e queue.Event) string {
	what, replied := "`"+Escape(e.State)+"`", false
	if n, ok := notices[e.Type]; ok {
		what, replied = n.status, n.reply != nil
	}
	line := Link(e.URL, e.Task) + ": " + what
	if s := summary(e); s != "" && !replied {
		line += " (" + s + ")"
	}
	if e.PR != "" {
		line += " · " + Link(e.PR, "PR")
	}
	return line
}

// Notifier posts task events into the Slack thread each task was filed
// from, and nowhere else: a task without a thread (filed over MCP, even
// with a slack: origin) gets nothing. Each task gets one status message in
// its thread, updated in place, and a reply for questions and the states
// a person should see. Only the status message has buttons: it is edited
// with every event, so its buttons always fit the task's state, where a
// reply's would outlive it.
//
// It follows the feed like a subscriber does (Server.deliver): on the
// leader only, in order, at least once, with its cursor and posts moved
// only under the leader's token.
type Notifier struct {
	App  *App
	Feed Feed
	// Current is a task's state now, as an event, for reconciling the
	// threads when the cursor fell out of the feed.
	Current func(ctx context.Context, task string) (queue.Event, error)
	// Held is the commit a task in needs-approval holds now, for its
	// Approve button. Nil, there is no Approve button: without sign-in
	// through the forge nobody can approve from Slack.
	Held func(ctx context.Context, task string) (sha string, err error)
	// Every is how often it looks for new events.
	Every time.Duration
	// Backoff is the wait after a first failure, doubled after each next
	// one, up to five minutes. Default: a second.
	Backoff time.Duration
	// Retention is how long the feed keeps events: event_retention.
	// Default: its default, two weeks.
	Retention time.Duration
}

// errDeposed is a fenced write refused: another replica leads now.
var errDeposed = errors.New("slack notifier: no longer the leader")

// Run posts events until ctx ends or token no longer leads.
func (n *Notifier) Run(ctx context.Context, token string) {
	log := n.App.o.Log
	rc := n.App.o.Redis
	backoff, retention := n.Backoff, n.Retention
	if backoff == 0 {
		backoff = time.Second
	}
	if retention == 0 {
		retention = 14 * 24 * time.Hour
	}
	// hold is how long Slack last asked to be left alone.
	cursor, fail, hold := "", 0, time.Duration(0)
	for {
		wait := n.Every
		if fail > 0 {
			wait = max(min(backoff<<min(fail-1, 8), 5*time.Minute), hold)
		}
		hold = 0
		if cursor == "" && fail == 0 {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if cursor == "" {
			c, err := rc.HGet(ctx, notifierKey, "cursor").Result()
			if err != nil && !errors.Is(err, redis.Nil) {
				fail++
				continue
			}
			if c == "" {
				// The first run starts from now: no thread was bound before.
				// Saved at once, so a handover doesn't move "now" past an event.
				c = n.ago(0)
				if ok, err := n.Feed.SetFenced(ctx, notifierKey, "cursor", c, token); err != nil {
					fail++
					continue
				} else if !ok {
					return
				}
			}
			cursor = c
		}
		// Before the oldest event kept, the cursor may have missed some, but
		// only if it is older than the feed keeps them: a cursor that was
		// the newest event, or now, when the feed was quiet is not.
		if expired, err := n.Feed.EventsExpired(ctx, cursor); err != nil {
			fail++
			continue
		} else if expired && queue.StreamIDLess(cursor, n.ago(retention)) {
			// The events missed are gone: bring each thread's status up to
			// date from the task itself instead, and go on from here.
			mark := n.ago(0)
			log.Warn("slack notifier: its cursor fell out of the feed; reconciling the threads", "cursor", cursor)
			if err := n.reconcile(ctx, token); errors.Is(err, errDeposed) {
				return
			} else if err != nil {
				log.Warn("slack notifier: reconciling", "err", err)
				fail++
				hold = retryAfter(err)
				continue
			}
			if ok, err := n.Feed.SetFenced(ctx, notifierKey, "cursor", mark, token); err != nil {
				fail++
				continue
			} else if !ok {
				return
			}
			cursor = mark
		}
		evs, _, err := n.Feed.Events(ctx, cursor, 100)
		if err != nil {
			fail++
			continue
		}
		if len(evs) == 0 {
			fail = 0
			// A quiet feed moves the cursor on, so it stays within what the
			// feed keeps and doesn't read as having missed events. Behind
			// now, so an event added as it moves isn't skipped.
			if mark := n.ago(retention / 4); queue.StreamIDLess(cursor, n.ago(retention/2)) {
				if ok, err := n.Feed.SetFenced(ctx, notifierKey, "cursor", mark, token); err != nil {
					fail++
				} else if !ok {
					return
				} else {
					cursor = mark
				}
			}
		}
		for _, e := range evs {
			if err := n.handle(ctx, token, e); errors.Is(err, errDeposed) {
				return
			} else if err != nil {
				// Slack or Redis may be back on the next try; nothing after
				// e is posted before it.
				log.Warn("slack notifier: posting an event; trying again", "task", e.Task, "event", e.ID, "type", e.Type, "err", err)
				fail++
				hold = retryAfter(err)
				break
			}
			ok, err := n.Feed.SetFenced(ctx, notifierKey, "cursor", e.ID, token)
			if err != nil {
				fail++
				break
			}
			if !ok {
				return
			}
			cursor, fail = e.ID, 0
		}
	}
}

// ago is a feed ID for d before now.
func (n *Notifier) ago(d time.Duration) string {
	return strconv.FormatInt(n.App.o.Now().Add(-d).UnixMilli(), 10) + "-0"
}

// handle posts e in its task's thread, if it has one. An error is one a
// retry may get past; a post Slack refuses for good is dropped.
func (n *Notifier) handle(ctx context.Context, token string, e queue.Event) error {
	nt, ok := notices[e.Type]
	if !ok {
		return nil
	}
	th, err := n.App.Thread(ctx, e.Task)
	if errors.Is(err, ErrNoThread) {
		return nil
	} else if err != nil {
		return err
	}
	p, err := n.App.post(ctx, e.Task)
	if err != nil {
		return err
	}
	if p.Last != "" && !queue.StreamIDLess(p.Last, e.ID) {
		return nil // posted already
	}
	var reply string
	if nt.reply != nil {
		reply = nt.reply(e)
	}
	if err := n.send(ctx, token, e.Task, th, &p, statusLine(e), reply, n.buttons(ctx, e)); err != nil {
		return err
	}
	p.Last = e.ID
	return n.savePost(ctx, token, e.Task, p)
}

// buttons are the buttons for e's task in its state (taskButtons), with
// the commit it holds for Approve. A hold that can't be read leaves the
// Approve button off rather than hold up the feed.
func (n *Notifier) buttons(ctx context.Context, e queue.Event) []any {
	var sha string
	if n.Held != nil && Applies(ActionApprove, e.State) {
		var err error
		if sha, err = n.Held(ctx, e.Task); err != nil {
			n.App.o.Log.Warn("slack notifier: reading a task's held change for its Approve button", "task", e.Task, "err", err)
		}
	}
	return taskButtons(e, sha)
}

// send updates the task's status message to status, with buttons under
// it, or posts it when there is none yet, then posts reply, if any, in the
// thread, plain. A post Slack refuses for good is dropped: logged, counted, and
// not an error.
func (n *Notifier) send(ctx context.Context, token, task string, th Thread, p *post, status, reply string, buttons []any) error {
	bot, err := n.App.Bot(ctx, th.Workspace)
	if errors.Is(err, ErrNotInstalled) {
		n.drop(task, th, "not_installed", err)
		return nil
	} else if err != nil {
		return err
	}
	m := Message{Channel: th.Channel, ThreadTS: th.TS, Text: status, Blocks: withButtons(status, buttons)}
	if p.TS != "" {
		err = bot.UpdateMessage(ctx, p.TS, m)
		if IsCode(err, "message_not_found") || IsCode(err, "cant_update_message") {
			// Someone deleted it: post a new one.
			p.TS, err = "", nil
		}
	}
	if err == nil && p.TS == "" {
		var ts string
		if ts, err = bot.PostMessage(ctx, m); err == nil {
			p.TS = ts
			// Kept before the reply, so a retry updates this message rather
			// than posting another.
			if err := n.savePost(ctx, token, task, *p); err != nil {
				return err
			}
		}
	}
	if err == nil && reply != "" {
		_, err = bot.PostMessage(ctx, Message{Channel: th.Channel, ThreadTS: th.TS, Text: reply})
	}
	if code, ok := permanent(err); ok {
		n.drop(task, th, code, err)
		return nil
	}
	return err
}

// permanent reports whether err is Slack refusing a call for good (the
// channel gone or devbot not in it, the token revoked, the message
// refused), and its code. Rate limits, Slack's own failures and the
// network are tried again.
func permanent(err error) (string, bool) {
	var e *APIError
	if !errors.As(err, &e) {
		return "", false
	}
	switch {
	case e.Code == "ratelimited", strings.HasPrefix(e.Code, "http_5"), e.Code == "internal_error",
		e.Code == "fatal_error", e.Code == "service_unavailable", e.Code == "request_timeout":
		return "", false
	}
	return e.Code, true
}

// drop gives up a post Slack won't take, so it doesn't hold up the feed.
func (n *Notifier) drop(task string, th Thread, code string, err error) {
	if !codeRE.MatchString(code) {
		code = "other"
	}
	metrics.add("devbot_slack_posts_dropped_total", "error", code)
	n.App.o.Log.Warn("slack notifier: dropping a post to a task's thread", "task", task, "workspace", th.Workspace.String(), "channel", th.Channel, "err", err)
}

// reconcile sets every thread's status message to its task's state now.
// A task it can't read is left as it was.
func (n *Notifier) reconcile(ctx context.Context, token string) error {
	threads, err := n.App.Threads(ctx)
	if err != nil {
		return err
	}
	for task, th := range threads {
		e, err := n.Current(ctx, task)
		if err != nil {
			n.App.o.Log.Warn("slack notifier: reading a task to reconcile its thread", "task", task, "err", err)
			continue
		}
		e.Type, e.Task, e.Summary = "", task, ""
		p, err := n.App.post(ctx, task)
		if err != nil {
			return err
		}
		if err := n.send(ctx, token, task, th, &p, statusLine(e), "", n.buttons(ctx, e)); err != nil {
			return err
		}
	}
	return nil
}

// post is what has been posted in task's thread.
func (a *App) post(ctx context.Context, task string) (post, error) {
	var p post
	b, err := a.o.Redis.HGet(ctx, postsKey, task).Bytes()
	if errors.Is(err, redis.Nil) {
		return p, nil
	} else if err != nil {
		return p, err
	}
	return p, json.Unmarshal(b, &p)
}

// savePost records p as task's post, while token leads.
func (n *Notifier) savePost(ctx context.Context, token, task string, p post) error {
	b, _ := json.Marshal(p)
	ok, err := n.Feed.SetFenced(ctx, postsKey, task, string(b), token)
	if err != nil {
		return fmt.Errorf("slack notifier: recording a post: %w", err)
	} else if !ok {
		return errDeposed
	}
	return nil
}

// retryAfter is how long Slack asked devbot to wait before calling again,
// when err is its rate limit.
func retryAfter(err error) time.Duration {
	var e *APIError
	if errors.As(err, &e) {
		return e.RetryAfter
	}
	return 0
}

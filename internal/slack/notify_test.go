package slack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"git.shdw.tech/rob/devbot/internal/auth"
	"git.shdw.tech/rob/devbot/internal/queue"
	"git.shdw.tech/rob/devbot/internal/slack"
	"git.shdw.tech/rob/devbot/internal/slack/slacktest"
)

var (
	team  = slack.Workspace{Team: "T1"}
	here  = slack.Thread{Workspace: team, Channel: "C1", TS: "1600000000.000001"}
	issue = func(n string) string { return "https://git.example.com/acme/app/issues/" + n }
)

// notifyEnv is a notifier's world: the feed, the app installed in team,
// its Web API the fake, and the leader's token.
type notifyEnv struct {
	t     *testing.T
	q     *queue.Store
	app   *slack.App
	api   *slacktest.Fake
	token string
	// current is what Current answers, by task.
	current map[string]queue.Event
	// retention is the feed's, as the notifier is told it.
	retention time.Duration
	// held is the notifier's Held: nil, no Approve button.
	held func(ctx context.Context, task string) (string, error)
}

func newNotifyEnv(t *testing.T) *notifyEnv {
	t.Helper()
	q := queue.New(miniredis.RunT(t).Addr(), 0)
	sealer, _ := auth.NewSealer([]byte("an-auth-key-for-tests-that-is-long-enough"), nil)
	v := &notifyEnv{t: t, q: q, api: slacktest.New(), current: map[string]queue.Event{}}
	app, err := slack.New(slack.Options{ClientID: "c", ClientSecret: "s", SigningSecret: "x", ResourceURL: "http://devbot",
		AllowedTeams: []string{"T1", "T2"}, Redis: q.R, Sealer: sealer, Log: slog.New(slog.DiscardHandler),
		NewAPI: func(string) slack.API { return v.api }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := app.Save(ctx, slack.Installation{Workspace: team, Token: "xoxb-1"}); err != nil {
		t.Fatal(err)
	}
	v.app = app
	if v.token, err = q.TakeLeader(ctx, "test", time.Hour); err != nil || v.token == "" {
		t.Fatalf("leader: %q %v", v.token, err)
	}
	// The notifier has been running: it posts from here on.
	if ok, err := q.SetFenced(ctx, "devbot:slack:notifier", "cursor", ago(time.Minute), v.token); !ok || err != nil {
		t.Fatalf("cursor: %v", err)
	}
	return v
}

func (v *notifyEnv) bind(task string, th slack.Thread) {
	if err := v.app.Bind(context.Background(), task, th); err != nil {
		v.t.Fatal(err)
	}
}

// emit adds an event for acme/app#n to the feed, and returns its ID.
func (v *notifyEnv) emit(n, typ, summary, pr string) string {
	v.t.Helper()
	id, err := v.q.Emit(context.Background(), queue.Event{Type: typ, Task: "fake:acme/app#" + n, URL: issue(n), PR: pr,
		Summary: summary, Origin: "slack:T1/C1", State: "x"}, time.Hour)
	if err != nil {
		v.t.Fatal(err)
	}
	return id
}

// run runs a notifier until cond holds, then stops it.
func (v *notifyEnv) run(what string, cond func() bool) {
	v.t.Helper()
	n := &slack.Notifier{App: v.app, Feed: v.q, Every: 5 * time.Millisecond, Backoff: time.Millisecond, Retention: v.retention,
		Current: func(ctx context.Context, task string) (queue.Event, error) {
			if e, ok := v.current[task]; ok {
				return e, nil
			}
			return queue.Event{}, errors.New("no such task")
		}, Held: v.held}
	ctx, stop := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { n.Run(ctx, v.token) })
	defer func() { stop(); wg.Wait() }()
	for start := time.Now(); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Since(start) > 5*time.Second {
			v.t.Fatalf("not within 5s: %s; calls:\n%s", what, v.dump())
		}
	}
}

// posted is the calls that took, as "method text".
func (v *notifyEnv) posted() []slacktest.Call {
	var out []slacktest.Call
	for _, c := range v.api.Calls() {
		if c.Err == nil {
			out = append(out, c)
		}
	}
	return out
}

func (v *notifyEnv) dump() string {
	var b strings.Builder
	for _, c := range v.api.Calls() {
		b.WriteString(c.Method + " " + c.Message.Channel + " " + c.TS + " " + c.Message.Text)
		if c.Err != nil {
			b.WriteString(" [" + c.Err.Error() + "]")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ago is a feed ID for d before now.
func ago(d time.Duration) string { return strconv.FormatInt(time.Now().Add(-d).UnixMilli(), 10) + "-0" }

// cursorPast is whether the notifier's cursor is at id or past it.
func cursorPast(v *notifyEnv, id string) func() bool {
	return func() bool {
		c := v.q.R.HGet(context.Background(), "devbot:slack:notifier", "cursor").Val()
		return c != "" && !queue.StreamIDLess(c, id)
	}
}

// A bound task's events go to its thread, in order: one status message
// updated in place, and a reply for each a person should see. A task with
// no thread, though filed with a slack: origin, gets nothing. Run again,
// with the cursor back where it was, nothing is posted twice.
func TestNotifierPostsInTheThreadInOrder(t *testing.T) {
	v := newNotifyEnv(t)
	v.bind("fake:acme/app#1", here)
	pr := "https://git.example.com/acme/app/pulls/7"
	steps := []struct{ typ, summary, pr string }{
		{"task.filed", "a title", ""},
		{"task.started", "attempt 1", ""},
		{"task.question", "which branch?", ""},
		{"task.answered", "main", ""},
		{"task.pr_opened", "PR #7 opened", pr},
		{"task.changes_requested", "review: changes requested on PR #7", pr},
		{"task.needs_approval", "held", pr},
		{"task.approved", "PR #7 approved", pr},
		{"task.retried", "retried by ann", pr},
		{"task.needs_human", "the rebase failed", pr},
		{"task.cancelled", "cancelled by ann", pr},
		{"task.merged", "PR #7 merged", pr},
	}
	var last string
	for _, s := range steps {
		last = v.emit("1", s.typ, s.summary, s.pr)
		v.emit("2", s.typ, s.summary, s.pr)
	}
	v.run("every event posted", cursorPast(v, last))

	want := []struct{ method, text string }{
		{"chat.postMessage", "working on it"},
		{"chat.update", "has a question: see the reply below"},
		{"chat.postMessage", "Question: which branch?"},
		{"chat.update", "answered (main)"},
		{"chat.update", "PR opened"},
		{"chat.update", "changes requested"},
		{"chat.update", "waiting for approval"},
		{"chat.postMessage", "files a person has to approve"},
		{"chat.update", "approved"},
		{"chat.update", "retried"},
		{"chat.update", "stopped, needs a person: see the reply below"},
		{"chat.postMessage", "devbot stopped: the rebase failed"},
		{"chat.update", "cancelled"},
		{"chat.postMessage", "Cancelled: cancelled by ann"},
		{"chat.update", "merged"},
		{"chat.postMessage", "Merged: <" + pr + "|the PR>"},
	}
	got := v.posted()
	if len(got) != len(want) {
		t.Fatalf("posted %d, want %d:\n%s", len(got), len(want), v.dump())
	}
	for i, w := range want {
		c := got[i]
		if c.Method != w.method || !strings.Contains(c.Message.Text, w.text) {
			t.Errorf("call %d: %s %q, want %s with %q", i, c.Method, c.Message.Text, w.method, w.text)
		}
		if c.Message.Channel != "C1" || c.Message.ThreadTS != here.TS {
			t.Errorf("call %d went to %s/%s, not the task's thread", i, c.Message.Channel, c.Message.ThreadTS)
		}
		if c.Method == "chat.update" && c.TS != "1700000000.1" {
			t.Errorf("call %d updated %q, not the status message", i, c.TS)
		}
		if strings.Contains(c.Message.Text, "#2") {
			t.Errorf("call %d is about the task with no thread: %q", i, c.Message.Text)
		}
	}
	if !strings.HasPrefix(got[0].Message.Text, "<"+issue("1")+"|fake:acme/app#1>: ") {
		t.Errorf("the status message doesn't link the issue: %q", got[0].Message.Text)
	}

	// The cursor is back before every event, as after a crash before it
	// was saved: the events are read again, and nothing is posted twice.
	v.q.SetFenced(context.Background(), "devbot:slack:notifier", "cursor", ago(time.Minute), v.token)
	v.run("every event read again", cursorPast(v, last))
	if n := len(v.posted()); n != len(want) {
		t.Errorf("run again, it posted %d more:\n%s", n-len(want), v.dump())
	}
}

// Every model-written field an event carries reaches Slack escaped: no
// mention, broadcast or link of the model's own.
func TestNotifierEscapesModelText(t *testing.T) {
	v := newNotifyEnv(t)
	v.bind("fake:acme/app#1", here)
	hostile := "<!channel> <!here> <!everyone> <@U0BOSS> <!subteam^S1|@oncall> <https://evil.example|click> @here @channel"
	var last string
	for _, typ := range []string{"task.started", "task.question", "task.changes_requested", "task.needs_approval",
		"task.needs_human", "task.cancelled", "task.merged"} {
		last = v.emit("1", typ, hostile+"\nsecond line <!channel>", "https://evil.example/|<!channel>")
	}
	v.run("every event posted", cursorPast(v, last))
	got := v.posted()
	if len(got) == 0 {
		t.Fatal("nothing posted")
	}
	for _, c := range got {
		text := strings.ReplaceAll(c.Message.Text, "<"+issue("1")+"|fake:acme/app#1>", "")
		if strings.ContainsAny(text, "<>") {
			t.Errorf("%s: unescaped markup in %q", c.Method, c.Message.Text)
		}
		for _, b := range []string{"@here", "@channel", "@everyone", "second line"} {
			if strings.Contains(text, b) {
				t.Errorf("%s: %q in %q", c.Method, b, c.Message.Text)
			}
		}
	}
}

// Slack's rate limits and its own failures are tried again, without
// skipping ahead; a post it refuses for good, or one to a workspace that
// uninstalled, is dropped, counted and logged, and the rest go on.
func TestNotifierRetriesAndDrops(t *testing.T) {
	v := newNotifyEnv(t)
	v.bind("fake:acme/app#1", here)
	v.bind("fake:acme/app#3", slack.Thread{Workspace: team, Channel: "CGONE", TS: "1.1"})
	v.bind("fake:acme/app#4", slack.Thread{Workspace: team, Channel: "COUT", TS: "1.2"})
	v.bind("fake:acme/app#5", slack.Thread{Workspace: team, Channel: "CREVOKED", TS: "1.3"})
	v.bind("fake:acme/app#6", slack.Thread{Workspace: slack.Workspace{Team: "T2"}, Channel: "C1", TS: "1.4"})
	var transient atomic.Int32
	v.api.Fail = func(c slacktest.Call) error {
		switch c.Message.Channel {
		case "CGONE":
			return &slack.APIError{Method: c.Method, Code: "channel_not_found"}
		case "COUT":
			return &slack.APIError{Method: c.Method, Code: "not_in_channel"}
		case "CREVOKED":
			return &slack.APIError{Method: c.Method, Code: "token_revoked"}
		}
		// #1's question fails twice, as Slack under load does.
		if strings.HasPrefix(c.Message.Text, "Question") {
			switch transient.Add(1) {
			case 1:
				return &slack.APIError{Method: c.Method, Code: "ratelimited", RetryAfter: 20 * time.Millisecond}
			case 2:
				return &slack.APIError{Method: c.Method, Code: "http_503"}
			}
		}
		return nil
	}
	v.emit("1", "task.started", "", "")
	v.emit("3", "task.started", "", "")
	v.emit("1", "task.question", "which?", "")
	v.emit("4", "task.question", "which?", "")
	v.emit("5", "task.merged", "", "")
	v.emit("6", "task.merged", "", "")
	last := v.emit("1", "task.merged", "", "")
	v.run("every event handled", cursorPast(v, last))

	var c1 []string
	for _, c := range v.posted() {
		c1 = append(c1, c.Method+" "+c.Message.Text)
	}
	// A retried event updates the status again, which changes nothing.
	want := []string{"chat.postMessage", "chat.update", "chat.update", "chat.update", "chat.postMessage Question", "chat.update", "chat.postMessage Merged"}
	if len(c1) != len(want) {
		t.Fatalf("posted:\n%s", v.dump())
	}
	for i := range want {
		if !strings.HasPrefix(c1[i], want[i]) {
			t.Errorf("post %d: %q, want %q", i, c1[i], want[i])
		}
	}
	if transient.Load() != 3 {
		t.Errorf("the question was tried %d times, want 3", transient.Load())
	}
	var b bytes.Buffer
	slack.WriteMetrics(&b)
	for _, code := range []string{"channel_not_found", "not_in_channel", "token_revoked", "not_installed"} {
		if !strings.Contains(b.String(), `devbot_slack_posts_dropped_total{error="`+code+`"}`) {
			t.Errorf("no drop counted for %s:\n%s", code, b.String())
		}
	}
}

// A cursor that fell out of the feed isn't replayed from: each thread's
// status is set from its task as it is now, and events after that are
// posted as usual.
func TestNotifierReconcilesAnExpiredCursor(t *testing.T) {
	v := newNotifyEnv(t)
	ctx := context.Background()
	v.bind("fake:acme/app#1", here)
	v.bind("fake:acme/app#2", slack.Thread{Workspace: team, Channel: "C2", TS: "1.2"})
	v.bind("fake:acme/app#9", slack.Thread{Workspace: team, Channel: "C9", TS: "1.9"})
	pr := "https://git.example.com/acme/app/pulls/7"
	v.current["fake:acme/app#1"] = queue.Event{Task: "fake:acme/app#1", URL: issue("1"), PR: pr, State: "in-review"}
	v.current["fake:acme/app#2"] = queue.Event{Task: "fake:acme/app#2", URL: issue("2"), State: "needs-info"}
	// The feed has moved past where the notifier was.
	old := v.emit("1", "task.started", "", "")
	v.q.SetFenced(ctx, "devbot:slack:notifier", "cursor", "1-0", v.token)
	v.run("both threads reconciled", func() bool { return len(v.posted()) == 2 && cursorPast(v, old)() })
	byChannel := map[string]string{}
	for _, c := range v.posted() {
		if c.Method != "chat.postMessage" {
			t.Errorf("reconciling: %s", c.Method)
		}
		byChannel[c.Message.Channel] = c.Message.Text
	}
	if !strings.Contains(byChannel["C1"], "`in-review`") || !strings.Contains(byChannel["C1"], pr) {
		t.Errorf("#1's status: %q", byChannel["C1"])
	}
	if !strings.Contains(byChannel["C2"], "`needs-info`") {
		t.Errorf("#2's status: %q", byChannel["C2"])
	}
	for _, c := range v.posted() {
		if strings.Contains(c.Message.Text, "working on it") {
			t.Errorf("an event from before the cursor expired was replayed: %q", c.Message.Text)
		}
	}
	if c := v.q.R.HGet(ctx, "devbot:slack:notifier", "cursor").Val(); queue.StreamIDLess(c, old) {
		t.Errorf("the cursor is %s, not past the feed's end %s", c, old)
	}

	// The next event updates the reconciled status message.
	last := v.emit("1", "task.merged", "", pr)
	v.run("the merge posted", cursorPast(v, last))
	got := v.posted()
	status := "1700000000.1" // the fake's ts for its first post
	if got[1].Message.Channel == "C1" {
		status = "1700000000.2"
	}
	if len(got) != 4 || got[2].Method != "chat.update" || got[2].TS != status || got[3].Method != "chat.postMessage" {
		t.Errorf("after reconciling:\n%s", v.dump())
	}
}

// A replica that no longer leads posts nothing more: its first fenced
// write fails and it stops.
func TestNotifierStopsWhenDeposed(t *testing.T) {
	v := newNotifyEnv(t)
	v.bind("fake:acme/app#1", here)
	ctx := context.Background()
	v.q.ResignLeader(ctx, v.token)
	other, _ := v.q.TakeLeader(ctx, "other", time.Hour)
	if other == "" {
		t.Fatal("no new leader")
	}
	v.emit("1", "task.started", "", "")
	v.emit("1", "task.merged", "", "")
	n := &slack.Notifier{App: v.app, Feed: v.q, Every: time.Millisecond, Backoff: time.Millisecond,
		Current: func(context.Context, string) (queue.Event, error) { return queue.Event{}, errors.New("not read") }}
	done := make(chan struct{})
	go func() { n.Run(ctx, v.token); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a deposed notifier kept running")
	}
	// It may have posted the one event it was on when it found out, never
	// more.
	if n := len(v.posted()); n > 1 {
		t.Errorf("a deposed notifier posted %d:\n%s", n, v.dump())
	}
}

// A cursor before the oldest event kept hasn't missed any while it is
// newer than the feed keeps them: after a quiet spell, the next event is
// posted, not reconciled. A quiet feed moves the cursor on, so it stays
// that way.
func TestNotifierQuietFeedIsNotExpired(t *testing.T) {
	v := newNotifyEnv(t)
	ctx := context.Background()
	v.retention = time.Hour
	v.bind("fake:acme/app#1", here)
	v.current["fake:acme/app#1"] = queue.Event{Task: "fake:acme/app#1", URL: issue("1"), State: "in-review"}
	v.q.SetFenced(ctx, "devbot:slack:notifier", "cursor", ago(50*time.Minute), v.token)
	moved := func() bool {
		c := v.q.R.HGet(ctx, "devbot:slack:notifier", "cursor").Val()
		return queue.StreamIDLess(ago(20*time.Minute), c)
	}
	v.run("the cursor moved on", moved)
	if c := v.q.R.HGet(ctx, "devbot:slack:notifier", "cursor").Val(); queue.StreamIDLess(ago(time.Minute), c) {
		t.Errorf("the cursor moved to %s, too close to now", c)
	}
	// The feed now starts after the cursor, with the next event.
	v.q.SetFenced(ctx, "devbot:slack:notifier", "cursor", ago(40*time.Minute), v.token)
	last := v.emit("1", "task.question", "which?", "")
	v.run("the question posted", cursorPast(v, last))
	got := v.posted()
	if len(got) != 2 || !strings.Contains(got[0].Message.Text, "has a question") || !strings.HasPrefix(got[1].Message.Text, "Question: which?") {
		t.Errorf("posted:\n%s", v.dump())
	}
}

// buttons are the action_ids of a message's buttons, and their values.
func buttons(t *testing.T, m slack.Message) ([]string, []slack.Button) {
	t.Helper()
	if m.Blocks == nil {
		return nil, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text struct {
			Text string `json:"text"`
		} `json:"text"`
		Elements []struct {
			ActionID string          `json:"action_id"`
			Value    string          `json:"value"`
			Confirm  json.RawMessage `json:"confirm"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(m.Blocks, &blocks); err != nil || len(blocks) != 2 || blocks[0].Text.Text != m.Text {
		t.Fatalf("blocks %s for %q", m.Blocks, m.Text)
	}
	var ids []string
	var bs []slack.Button
	for _, el := range blocks[1].Elements {
		b, ok := slack.ParseButton(el.Value)
		if !ok || len(el.Value) > 2000 {
			t.Fatalf("button %s: %q", el.ActionID, el.Value)
		}
		if (el.ActionID == slack.ActionCancel) != (el.Confirm != nil) {
			t.Fatalf("only Cancel asks to confirm: %s", el.ActionID)
		}
		ids, bs = append(ids, el.ActionID), append(bs, b)
	}
	return ids, bs
}

// The status message gets the buttons for its task's state, carrying what
// they act on: the question for Answer, the held commit for Approve; a
// reply gets none. Without Held there is no Approve, and a task that ended
// gets none.
func TestNotifierButtons(t *testing.T) {
	for _, approve := range []bool{true, false} {
		v := newNotifyEnv(t)
		v.bind("fake:acme/app#1", here)
		if approve {
			v.held = func(_ context.Context, task string) (string, error) { return "aaaa1111" + task, nil }
		}
		long := strings.Repeat("<&\"é", 600)
		steps := []struct{ typ, state, summary string }{
			{"task.started", "working", "attempt 1"},
			{"task.question", "needs-info", long},
			{"task.pr_opened", "in-review", "PR"},
			{"task.needs_approval", "needs-approval", "held"},
			{"task.needs_human", "needs-human", "stuck"},
			{"task.merged", "approved", "merged"},
		}
		var last string
		for _, s := range steps {
			id, err := v.q.Emit(context.Background(), queue.Event{Type: s.typ, Task: "fake:acme/app#1", URL: issue("1"), Summary: s.summary, State: s.state}, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			last = id
		}
		v.run("every event posted", cursorPast(v, last))
		approveBtn := []string{slack.ActionApprove, slack.ActionCancel}
		if !approve {
			approveBtn = []string{slack.ActionCancel}
		}
		want := [][]string{
			{slack.ActionCancel},
			{slack.ActionAnswer, slack.ActionCancel}, nil,
			{slack.ActionRebase, slack.ActionCancel},
			approveBtn, nil,
			{slack.ActionRetry, slack.ActionCancel}, nil,
			nil, nil,
		}
		got := v.posted()
		if len(got) != len(want) {
			t.Fatalf("posted %d, want %d:\n%s", len(got), len(want), v.dump())
		}
		for i, w := range want {
			ids, bs := buttons(t, got[i].Message)
			if !slices.Equal(ids, w) {
				t.Errorf("approve %v, call %d %q: buttons %v, want %v", approve, i, got[i].Message.Text, ids, w)
			}
			for j, b := range bs {
				if b.Task != "fake:acme/app#1" || b.State == "" {
					t.Errorf("call %d: button %+v", i, b)
				}
				switch ids[j] {
				case slack.ActionApprove:
					if b.SHA != "aaaa1111fake:acme/app#1" {
						t.Errorf("Approve carries %q", b.SHA)
					}
				case slack.ActionAnswer:
					// A long question is cut short, unescaped: the modal escapes it.
					if !strings.HasPrefix(long, strings.TrimSuffix(b.Question, "…")) || b.Question == "" {
						t.Errorf("Answer carries %q", b.Question)
					}
				}
			}
		}
	}
}

// threadMsg is a message in a thread as it shows now: its text and the
// action_ids of its buttons.
type threadMsg struct {
	text    string
	buttons []string
}

// thread is the messages posted in here, in order, each as its last edit
// left it.
func (v *notifyEnv) thread() []threadMsg {
	v.t.Helper()
	var order []string
	byTS := map[string]threadMsg{}
	for i, c := range v.api.Calls() {
		if c.Err != nil || c.Message.Channel != here.Channel || c.Message.ThreadTS != here.TS {
			continue
		}
		ts := c.TS
		if c.Method == "chat.postMessage" {
			ts = "1700000000." + strconv.Itoa(i+1) // the fake's ts
			order = append(order, ts)
		}
		ids, _ := buttons(v.t, c.Message)
		byTS[ts] = threadMsg{c.Message.Text, ids}
	}
	out := make([]threadMsg, len(order))
	for i, ts := range order {
		out[i] = byTS[ts]
	}
	return out
}

// A question and a held change each show once in full, in a reply with no
// buttons, while the status message says the state, points at the reply
// and carries the buttons. Once the task moves on, no message in the
// thread offers Answer or Approve.
func TestNotifierThreadAfterQuestionAndApproval(t *testing.T) {
	v := newNotifyEnv(t)
	v.bind("fake:acme/app#1", here)
	v.held = func(context.Context, string) (string, error) { return "aaaa1111", nil }
	status := "<" + issue("1") + "|fake:acme/app#1>: "
	question := "Question: which branch?\nAnswer on <" + issue("1") + "|fake:acme/app#1>."
	held := "The change touches files a person has to approve; see <" + issue("1") + "|fake:acme/app#1>."
	cancel := []string{slack.ActionCancel}
	steps := []struct {
		typ, state, summary string
		want                []threadMsg
	}{
		{"task.started", "working", "attempt 1", []threadMsg{
			{status + "working on it (attempt 1)", cancel}}},
		{"task.question", "needs-info", "which branch?", []threadMsg{
			{status + "has a question: see the reply below", []string{slack.ActionAnswer, slack.ActionCancel}},
			{question, nil}}},
		{"task.answered", "ready", "main", []threadMsg{
			{status + "answered (main)", cancel},
			{question, nil}}},
		{"task.started", "working", "attempt 2", []threadMsg{
			{status + "working on it (attempt 2)", cancel},
			{question, nil}}},
		{"task.needs_approval", "needs-approval", "holds ci/build.yml", []threadMsg{
			{status + "waiting for approval: see the reply below", []string{slack.ActionApprove, slack.ActionCancel}},
			{question, nil},
			{held, nil}}},
		{"task.approved", "approved", "approved by ann", []threadMsg{
			{status + "approved (approved by ann)", []string{slack.ActionRebase, slack.ActionCancel}},
			{question, nil},
			{held, nil}}},
		{"task.started", "working", "attempt 3", []threadMsg{
			{status + "working on it (attempt 3)", cancel},
			{question, nil},
			{held, nil}}},
	}
	for i, s := range steps {
		id, err := v.q.Emit(context.Background(), queue.Event{Type: s.typ, Task: "fake:acme/app#1", URL: issue("1"), Summary: s.summary, State: s.state}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		v.run(s.typ, cursorPast(v, id))
		want, got := s.want, v.thread()
		if len(got) != len(want) {
			t.Fatalf("step %d %s: thread\n%+v\nwant\n%+v", i, s.typ, got, want)
		}
		for j := range want {
			if got[j].text != want[j].text || !slices.Equal(got[j].buttons, want[j].buttons) {
				t.Errorf("step %d %s, message %d: %q %v, want %q %v", i, s.typ, j, got[j].text, got[j].buttons, want[j].text, want[j].buttons)
			}
		}
	}
}

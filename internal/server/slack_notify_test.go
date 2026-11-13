package server

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"git.shdw.tech/rob/devbot/internal/auth"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/forge/fake"
	"git.shdw.tech/rob/devbot/internal/queue"
	"git.shdw.tech/rob/devbot/internal/slack"
	"git.shdw.tech/rob/devbot/internal/slack/slacktest"
)

// withSlackAPI gives a replica the Slack app, installed in acme, its Web
// API api.
func withSlackAPI(t *testing.T, api *slacktest.Fake) func(*Server) {
	return func(s *Server) {
		s.deliverEvery = 10 * time.Millisecond
		sealer, err := auth.NewSealer([]byte("an-auth-key-for-tests-that-is-long-enough"), nil)
		if err != nil {
			t.Fatal(err)
		}
		a, err := slack.New(slack.Options{ClientID: "1.2", ClientSecret: "s", SigningSecret: "x", ResourceURL: "http://devbot",
			AllowedTeams: []string{"T0ACME"}, Redis: s.E.Q.R, Sealer: sealer, Log: slog.New(slog.DiscardHandler),
			NewAPI: func(string) slack.API { return api }})
		if err != nil {
			t.Fatal(err)
		}
		s.Slack = a
	}
}

// slackCalls is the calls api took.
func slackCalls(api *slacktest.Fake) []string {
	var out []string
	for _, c := range api.Calls() {
		if c.Err == nil {
			out = append(out, c.Method+" "+c.Message.Channel+" "+c.Message.Text)
		}
	}
	return out
}

// Two serve replicas with the Slack app: only the leader posts a bound
// task's events, in order, and across a failover nothing is posted twice.
// A task with no thread gets nothing, whatever its origin says.
func TestSlackThreadsPostedOnceByTheLeader(t *testing.T) {
	mr := miniredis.RunT(t)
	// miniredis only expires keys when told time has passed.
	clock := make(chan struct{})
	ticked := make(chan struct{})
	go func() {
		defer close(ticked)
		last := time.Now()
		for {
			select {
			case <-clock:
				return
			case <-time.After(5 * time.Millisecond):
			}
			now := time.Now()
			mr.FastForward(now.Sub(last))
			last = now
		}
	}()
	defer func() {
		close(clock)
		<-ticked
	}()

	ctx := context.Background()
	q := queue.New(mr.Addr(), 0)
	sealer, _ := auth.NewSealer([]byte("an-auth-key-for-tests-that-is-long-enough"), nil)
	setup, _ := slack.New(slack.Options{ClientID: "1.2", ClientSecret: "s", SigningSecret: "x", ResourceURL: "http://devbot",
		AllowedTeams: []string{"T0ACME"}, Redis: redis.NewClient(&redis.Options{Addr: mr.Addr()}), Sealer: sealer, Log: slog.New(slog.DiscardHandler)})
	if err := setup.Save(ctx, slack.Installation{Workspace: acme, Token: "xoxb-acme"}); err != nil {
		t.Fatal(err)
	}
	if err := setup.Bind(ctx, "fake:acme/app#1", slack.Thread{Workspace: acme, Channel: "C1", TS: "1600000000.000001"}); err != nil {
		t.Fatal(err)
	}
	// The notifier has been running: it posts from here on.
	tok, _ := q.TakeLeader(ctx, "setup", time.Minute)
	q.SetFenced(ctx, "devbot:slack:notifier", "cursor", strconv.FormatInt(time.Now().Add(-time.Minute).UnixMilli(), 10)+"-0", tok)
	q.ResignLeader(ctx, tok)

	emit := func(n, typ, summary string) string {
		id, err := q.Emit(ctx, queue.Event{Type: typ, Task: "fake:acme/app#" + n, URL: "https://git.example.com/acme/app/issues/" + n,
			Summary: summary, Origin: "slack:T0ACME/C1", State: "x"}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	cursorAt := func(id string) func() bool {
		return func() bool { return q.R.HGet(ctx, "devbot:slack:notifier", "cursor").Val() == id }
	}

	f := fake.New()
	f.AddRepo(haRepo, "version: 1\nenabled: true\n")
	cfg, _ := config.Load("")
	cfg.LeaderLease = time.Second
	apis := []*slacktest.Fake{slacktest.New(), slacktest.New()}
	a, b := newReplica(t, mr, f, cfg, withSlackAPI(t, apis[0])), newReplica(t, mr, f, cfg, withSlackAPI(t, apis[1]))
	both := []*replica{a, b}
	defer func() {
		for _, r := range both {
			r.crash()
		}
	}()
	waitFor(t, 2*time.Second, "a leader", func() bool { return a.s.leading.Load() || b.s.leading.Load() })
	leader, follower, lapi, fapi := a, b, apis[0], apis[1]
	if b.s.leading.Load() {
		leader, follower, lapi, fapi = b, a, apis[1], apis[0]
	}

	emit("1", "task.started", "attempt 1")
	emit("2", "task.started", "attempt 1")
	emit("1", "task.question", "which branch?")
	emit("2", "task.question", "which branch?")
	last := emit("1", "task.pr_opened", "PR #7 opened")
	waitFor(t, 5*time.Second, "the leader posts", cursorAt(last))
	if n := len(fapi.Calls()); n != 0 {
		t.Fatalf("the follower called Slack %d times", n)
	}

	leader.crash()
	waitFor(t, 3*time.Second, "failover", follower.s.leading.Load)
	emit("1", "task.changes_requested", "review")
	last = emit("1", "task.merged", "PR #7 merged")
	waitFor(t, 5*time.Second, "the new leader posts", cursorAt(last))

	got := append(slackCalls(lapi), slackCalls(fapi)...)
	want := []string{
		"chat.postMessage C1 working on it",
		"chat.update C1 has a question",
		"chat.postMessage C1 Question: which branch?",
		"chat.update C1 PR opened",
		"chat.update C1 changes requested",
		"chat.update C1 merged",
		"chat.postMessage C1 Merged",
	}
	if len(got) != len(want) {
		t.Fatalf("posted:\n%s", strings.Join(got, "\n"))
	}
	for i, w := range want {
		method, rest, _ := strings.Cut(w, " ")
		channel, text, _ := strings.Cut(rest, " ")
		if !strings.HasPrefix(got[i], method+" "+channel+" ") || !strings.Contains(got[i], text) {
			t.Errorf("post %d: %q, want %q", i, got[i], w)
		}
		if strings.Contains(got[i], "#2") {
			t.Errorf("post %d is about the task with no thread: %q", i, got[i])
		}
	}
}

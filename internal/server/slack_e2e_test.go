package server

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/slack"
	"git.shdw.tech/rob/devbot/internal/slack/slacktest"
)

// The whole of the Slack app, from the outside: signed requests in, a
// recording fake of Slack's Web API out. A workspace installs the app, a
// user links (in auth.mode oauth) or is the anonymous principal (with
// slack.allow_anonymous), files a task from the form, and follows it in
// its thread: a question there, answered from its button; a change held
// for approval, approved from its button where there is a forge sign-in
// and refused where there isn't; and the merge, each as the thread's
// status message and replies.
func TestSlackEndToEnd(t *testing.T) {
	for _, mode := range []struct {
		name      string
		anonymous bool
		// who is the caller as Slack's messages name them.
		who string
	}{
		{"oauth", false, "@alice"},
		{"anonymous", true, "`chat/slack-T0ACME-U1`"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			v := newSlackEnvAs(t, mode.anonymous, false)
			ctx := context.Background()
			v.f.AddRepo(appRepo, approvableApp)
			v.s.deliverEvery = 5 * time.Millisecond

			// Install.
			resp, body := v.get(v.url + "/slack/install")
			if resp.StatusCode != 302 {
				t.Fatalf("install: %d %s", resp.StatusCode, body)
			}
			back, err := v.oidc.Install(resp.Header.Get("Location"), "T0ACME", "xoxb-acme")
			if err != nil {
				t.Fatal(err)
			}
			if resp, body := v.get(back); resp.StatusCode != 200 || !strings.Contains(body, "devbot is installed") {
				t.Fatalf("install callback: %d %s", resp.StatusCode, body)
			}
			if in, err := v.s.Slack.Installation(ctx, acme); err != nil || in.Token != "xoxb-acme" {
				t.Fatalf("the install should be stored: %+v %v", in, err)
			}
			stop := v.notify()
			defer stop()

			// Link, or not.
			if mode.anonymous {
				if r := v.command("U1", ""); !strings.Contains(r.Text, mode.who) {
					t.Fatalf("/devbot: %s", r.Text)
				}
			} else {
				v.link(v.connectURL(v.command("U1", "")), "U1", "alice", "allow")
				if r := v.command("U1", ""); !strings.Contains(r.Text, "@alice on fake") {
					t.Fatalf("/devbot once linked: %s", r.Text)
				}
			}

			// File.
			if out := v.submit("U1", v.openForm("U1", "file"), map[string]any{"repo": picked("fake:acme/app"), "title": text("Add an export"),
				"acceptance_criteria": text("- a CSV download")}); out != nil {
				t.Fatalf("a filed form should close: %v", out)
			}
			calls := v.api.Calls()
			announce := calls[len(calls)-1]
			m := regexp.MustCompile(`^<@U1> filed <[^|]+\|(fake:acme/app#\d+)>: Add an export$`).FindStringSubmatch(announce.Message.Text)
			if announce.Method != "chat.postMessage" || announce.Message.Channel != "C1" || m == nil {
				t.Fatalf("the filing should be announced in the channel: %+v", announce)
			}
			task := m[1]
			ref, _ := forge.ParseItem(task)
			th, err := v.s.Slack.Thread(ctx, task)
			if err != nil {
				t.Fatal(err)
			}
			it, _ := v.f.GetItem(ctx, ref)
			if want := "alice"; !mode.anonymous && it.Author != want {
				t.Fatalf("the issue should be alice's: %s", it.Author)
			}

			// A question, in the thread, answered from its button.
			if err := v.s.E.Transition(ctx, it, core.LNeedsInfo, "task.question", "CSV or <!channel> TSV?"); err != nil {
				t.Fatal(err)
			}
			// The reply is the question, plain; the status message carries Answer.
			v.waitPost(t, th, "Question: CSV or &lt;!channel&gt; TSV?")
			q := v.statusPost(t, th, task)
			b, ok := buttonIn(q, slack.ActionAnswer)
			if !ok || !strings.Contains(q.Message.Text, ": has a question") {
				t.Fatalf("the status message should carry Answer: %q %s", q.Message.Text, q.Message.Blocks)
			}
			view := modal(t, v.click("U1", slack.ActionAnswer, b))
			before := len(v.api.Calls())
			if out := v.submit("U1", view, map[string]any{"answer": text("CSV.")}); out != nil {
				t.Fatalf("an answered modal should close: %v", out)
			}
			v.s.slackWork.Wait()
			if got := told(t, v.api.Calls()[before:], "U1"); !strings.Contains(got, "Answered") || !strings.Contains(got, mode.who) {
				t.Fatalf("the answer: %q", got)
			}
			// The clicker's own comment with a sign-in; devbot's, saying who
			// it is from, without.
			cs, _ := v.f.ListComments(ctx, ref)
			if !slices.ContainsFunc(cs, func(c forge.Comment) bool {
				return strings.Contains(c.Body, "CSV.") && (c.Author == "alice" || mode.anonymous && strings.Contains(c.Body, "chat/slack-T0ACME-U1"))
			}) {
				t.Fatalf("the answer should be on the issue, as the clicker: %v", cs)
			}

			// A change held for approval.
			const sha = "aaaa1111222233334444"
			v.f.AddCommentAt(ref, v.f.Impl, "Holding a CI change.\n\n"+core.RenderStatus(core.Status{Kind: "held", SHA: sha, Branch: "devbot/1-x",
				Files: []core.HeldFile{{Path: "ci/build.yml", Mode: "100644", Blob: strings.Repeat("1", 40)}}}), v.f.Now().Add(-time.Minute))
			it, _ = v.f.GetItem(ctx, ref)
			if err := v.s.E.Transition(ctx, it, core.LNeedsApproval, "task.needs_approval", "holds ci/build.yml"); err != nil {
				t.Fatal(err)
			}
			v.waitPost(t, th, "touches files a person has to approve")
			held := v.statusPost(t, th, task)
			b, ok = buttonIn(held, slack.ActionApprove)
			if mode.anonymous {
				// Nobody can approve without a forge sign-in: no button, and
				// a click on one anyway is refused.
				if ok {
					t.Fatalf("Approve offered without sign-in: %s", held.Message.Blocks)
				}
				got := told(t, v.click("U1", slack.ActionApprove, slack.Button{Task: task, State: "needs-approval", SHA: sha}), "U1")
				if !strings.Contains(got, "signed in through the forge") {
					t.Fatalf("an anonymous approval: %q", got)
				}
				if it, _ := v.f.GetItem(ctx, ref); slices.Contains(it.Labels, core.LApprove) {
					t.Fatal("approved without sign-in")
				}
			} else {
				if !ok || b.SHA != sha {
					t.Fatalf("the hold should carry Approve for %s: %s", sha, held.Message.Blocks)
				}
				view := modal(t, v.click("U1", slack.ActionApprove, b))
				if out := v.submit("U1", view, nil); out != nil {
					t.Fatalf("an approved modal should close: %v", out)
				}
				v.s.slackWork.Wait()
				// approve_task labels the issue as the clicker.
				evs, _ := v.f.LabelEvents(ctx, ref)
				if !slices.ContainsFunc(evs, func(e forge.LabelEvent) bool { return e.Label == core.LApprove && e.Added && e.Actor == "alice" }) {
					t.Fatalf("alice's approval of %s: %+v", sha, evs)
				}
			}

			// The merge.
			it, _ = v.f.GetItem(ctx, ref)
			if err := v.s.E.TransitionWithPR(ctx, it, "", "task.merged", "PR #7 merged", "https://fake/acme/app/pulls/7"); err != nil {
				t.Fatal(err)
			}
			v.waitPost(t, th, "Merged: <https://fake/acme/app/pulls/7|the PR>")

			// One status message, edited in place through each state, and a
			// reply for each a person should see; all in the thread, and
			// nothing else posted publicly.
			var statuses, replies []string
			status := regexp.MustCompile(`^<[^|]+\|` + regexp.QuoteMeta(task) + `>: `)
			for _, c := range v.api.Calls() {
				if c.Err != nil || c.Message.Channel != "C1" {
					continue
				}
				switch {
				case c.Method == "chat.update" || c.Method == "chat.postMessage" && c.Message.ThreadTS == th.TS && status.MatchString(c.Message.Text):
					statuses = append(statuses, c.Message.Text)
				case c.Method == "chat.postMessage" && c.Message.ThreadTS == th.TS:
					replies = append(replies, c.Message.Text)
					if c.Message.Blocks != nil {
						t.Errorf("a reply should carry no buttons: %q %s", c.Message.Text, c.Message.Blocks)
					}
				case c.Method == "chat.postMessage" && c.Message.Text != announce.Message.Text:
					t.Errorf("posted outside the thread: %+v", c)
				}
			}
			for _, want := range []string{"has a question", "waiting for approval", "merged"} {
				if !slices.ContainsFunc(statuses, func(s string) bool { return strings.Contains(s, ": "+want) }) {
					t.Errorf("the status message should have said %q: %q", want, statuses)
				}
			}
			if !strings.Contains(statuses[len(statuses)-1], ": merged") {
				t.Errorf("the status message should end merged: %q", statuses)
			}
			for _, r := range replies {
				if strings.Contains(r, "<!channel>") {
					t.Errorf("a model's words should be escaped: %q", r)
				}
			}
		})
	}
}

// notify runs the leader's Slack notifier until the returned stop.
func (v *slackEnv) notify() (stop func()) {
	v.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	tok, err := v.s.E.Q.TakeLeader(ctx, "e2e", time.Hour)
	if err != nil || tok == "" {
		v.t.Fatalf("leading: %q %v", tok, err)
	}
	var wg sync.WaitGroup
	wg.Go(func() { v.s.slackNotifier().Run(ctx, tok) })
	return func() { cancel(); wg.Wait() }
}

// waitPost waits for the notifier to post text in th, and returns that
// post.
func (v *slackEnv) waitPost(t *testing.T, th slack.Thread, text string) slacktest.Call {
	t.Helper()
	for start := time.Now(); time.Since(start) < 5*time.Second; time.Sleep(5 * time.Millisecond) {
		for _, c := range v.api.Calls() {
			if c.Err == nil && c.Method == "chat.postMessage" && c.Message.Channel == th.Channel && c.Message.ThreadTS == th.TS && strings.Contains(c.Message.Text, text) {
				return c
			}
		}
	}
	t.Fatalf("not posted in the thread within 5s: %q; calls: %+v", text, v.api.Calls())
	return slacktest.Call{}
}

// buttonIn is the action button a post carries, read as Slack hands its
// value back.
func buttonIn(c slacktest.Call, action string) (slack.Button, bool) {
	var blocks []map[string]any
	json.Unmarshal(c.Message.Blocks, &blocks)
	for _, b := range blocks {
		els, _ := b["elements"].([]any)
		for _, el := range els {
			if el, _ := el.(map[string]any); el["action_id"] == action {
				value, _ := el["value"].(string)
				return slack.ParseButton(value)
			}
		}
	}
	return slack.Button{}, false
}

// statusPost is the last post or edit of task's status message in th: the
// one message of a task that carries its buttons.
func (v *slackEnv) statusPost(t *testing.T, th slack.Thread, task string) slacktest.Call {
	t.Helper()
	status := regexp.MustCompile(`^<[^|]+\|` + regexp.QuoteMeta(task) + `>: `)
	calls := v.api.Calls()
	for i := len(calls) - 1; i >= 0; i-- {
		c := calls[i]
		if c.Err == nil && (c.Method == "chat.update" || c.Method == "chat.postMessage" && c.Message.ThreadTS == th.TS) &&
			c.Message.Channel == th.Channel && status.MatchString(c.Message.Text) {
			return c
		}
	}
	t.Fatalf("no status message for %s: %+v", task, calls)
	return slacktest.Call{}
}

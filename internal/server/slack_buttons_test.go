package server

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/fake"
	"git.shdw.tech/rob/devbot/internal/slack"
	"git.shdw.tech/rob/devbot/internal/slack/slacktest"
)

const approvableApp = "version: 1\nenabled: true\nprotected_paths: [ci/**]\napprovable_paths: [ci/**]\n"

// click clicks a task button as user in channel C1, and waits for what it
// set off. It returns the Slack calls it made.
func (v *slackEnv) click(user, action string, b slack.Button) []slacktest.Call {
	v.t.Helper()
	before := len(v.api.Calls())
	value, _ := json.Marshal(b)
	if out := v.interact(user, map[string]any{"type": "block_actions", "channel": map[string]any{"id": "C1"},
		"actions": []any{map[string]any{"action_id": action, "value": string(value)}}}); out != nil {
		v.t.Fatalf("a click is only acknowledged: %v", out)
	}
	v.s.slackWork.Wait()
	return v.api.Calls()[before:]
}

// told is the one ephemeral message calls told user.
func told(t *testing.T, calls []slacktest.Call, user string) string {
	t.Helper()
	var got []string
	for _, c := range calls {
		switch c.Method {
		case "chat.postMessage":
			t.Fatalf("a click must post nothing publicly: %+v", c)
		case "chat.postEphemeral":
			if c.User != user {
				t.Fatalf("told someone else: %+v", c)
			}
			got = append(got, c.Message.Text)
		}
	}
	if len(got) != 1 {
		t.Fatalf("want one ephemeral message to %s, got %q in %+v", user, got, calls)
	}
	return got[0]
}

// modal is the modal calls opened and filled in: its last view.
func modal(t *testing.T, calls []slacktest.Call) map[string]any {
	t.Helper()
	var opened bool
	var view map[string]any
	for _, c := range calls {
		switch c.Method {
		case "views.open":
			opened = true
		case "views.update":
			view = nil
			json.Unmarshal(c.View, &view)
		case "chat.postMessage":
			t.Fatalf("a click must post nothing publicly: %+v", c)
		}
	}
	if !opened || view == nil {
		t.Fatalf("want a modal opened and filled in: %+v", calls)
	}
	return view
}

// modalText is the text of every block of a view.
func modalText(view map[string]any) string {
	var b strings.Builder
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.Encode(view["blocks"])
	return b.String()
}

func (v *slackEnv) held(sha, shown string) forge.ItemRef {
	v.t.Helper()
	v.f.AddRepo(appRepo, approvableApp)
	ref := v.f.NewIssue(appRepo, "alice", "held", "", core.LNeedsApproval)
	v.f.AddCommentAt(ref, v.f.Impl, shown+"\n\n"+core.RenderStatus(core.Status{Kind: "held", SHA: sha, Branch: "devbot/1-x",
		Files: []core.HeldFile{{Path: "ci/build.yml", Mode: "100644", Blob: strings.Repeat("1", 40)}}}), v.f.Now().Add(-2*time.Minute))
	return ref
}

// Retry, Cancel and Rebase run as whoever clicks: someone who may only
// read the repo is refused with the MCP's refusal, and nothing changes;
// the filer's own click works, and is told only to them.
func TestSlackButtonsActAsTheClicker(t *testing.T) {
	v := newSlackEnv(t)
	ctx := context.Background()
	v.f.Grant(appRepo, "dave", forge.PermRead)
	v.linkAs("U1", "alice")
	v.linkAs("U2", "dave")
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	dave := v.login(cid, "http://127.0.0.1/callback", "dave")["access_token"].(string)

	stuck := v.f.NewIssue(appRepo, "alice", "stuck", "", core.LNeedsHuman)
	review := v.f.NewIssue(appRepo, "alice", "in review", "", core.LInReview)
	for _, c := range []struct {
		action, tool, state string
		ref                 forge.ItemRef
	}{
		{slack.ActionRetry, "retry", "needs-human", stuck},
		{slack.ActionCancel, "cancel", "needs-human", stuck},
		{slack.ActionRebase, "rebase", "in-review", review},
	} {
		got := told(t, v.click("U2", c.action, slack.Button{Task: c.ref.String(), State: c.state}), "U2")
		_, mcpText, isErr := v.mcp(dave, c.tool, `{"ref":"`+c.ref.String()+`"}`)
		if !isErr || got != slack.Escape(mcpText) || !strings.Contains(got, "write access") {
			t.Fatalf("dave's %s: Slack %q, MCP %q", c.tool, got, mcpText)
		}
		if st, _ := v.s.E.Status(ctx, core.Caller{}, c.ref.String()); st.State != c.state {
			t.Fatalf("dave's %s changed the task: %s", c.tool, st.State)
		}
	}
	if got := told(t, v.click("U1", slack.ActionRetry, slack.Button{Task: stuck.String(), State: "needs-human"}), "U1"); !strings.Contains(got, "Retried") || !strings.Contains(got, "@alice") {
		t.Fatalf("alice's retry: %q", got)
	}
	if st, _ := v.s.E.Status(ctx, core.Caller{}, stuck.String()); st.State == "needs-human" {
		t.Fatalf("alice's retry should move the task: %s", st.State)
	}
	if got := told(t, v.click("U1", slack.ActionCancel, slack.Button{Task: review.String(), State: "in-review"}), "U1"); !strings.Contains(got, "Cancelled") {
		t.Fatalf("alice's cancel: %q", got)
	}
	// A second click on the same button: the task moved on, so nothing is
	// done, and alice is told why.
	if got := told(t, v.click("U1", slack.ActionCancel, slack.Button{Task: review.String(), State: "in-review"}), "U1"); !strings.Contains(got, "no longer applies") || !strings.Contains(got, "did nothing") {
		t.Fatalf("a stale cancel: %q", got)
	}
}

// A click a task has moved on from does nothing, and says why.
func TestSlackStaleClick(t *testing.T) {
	v := newSlackEnv(t)
	ctx := context.Background()
	v.linkAs("U1", "alice")
	ready := v.f.NewIssue(appRepo, "alice", "ready again", "", core.LReady)
	before, _ := v.f.ListComments(ctx, ready)
	for _, action := range []string{slack.ActionRetry, slack.ActionRebase} {
		got := told(t, v.click("U1", action, slack.Button{Task: ready.String(), State: "needs-human"}), "U1")
		if !strings.Contains(got, "is `ready` now") || !strings.Contains(got, "devbot did nothing") {
			t.Fatalf("a stale %s: %q", action, got)
		}
	}
	view := modal(t, v.click("U1", slack.ActionAnswer, slack.Button{Task: ready.String(), State: "needs-info", Question: "Which?"}))
	if view["submit"] != nil || !strings.Contains(modalText(view), "answering no longer applies") {
		t.Fatalf("a stale answer: %v", view)
	}
	if after, _ := v.f.ListComments(ctx, ready); len(after) != len(before) {
		t.Fatalf("a stale click wrote to the issue: %v", after[len(before):])
	}
	if st, _ := v.s.E.Status(ctx, core.Caller{}, ready.String()); st.State != "ready" {
		t.Fatalf("state: %s", st.State)
	}
}

// Answer opens a modal with the question escaped, and submitting it
// answers as the clicker.
func TestSlackAnswerButton(t *testing.T) {
	v := newSlackEnv(t)
	ctx := context.Background()
	v.linkAs("U1", "alice")
	ref := v.f.NewIssue(appRepo, "alice", "asks", "", core.LNeedsInfo)
	q := "Use <!channel> or <@U9>? & @here"
	view := modal(t, v.click("U1", slack.ActionAnswer, slack.Button{Task: ref.String(), State: "needs-info", Question: q}))
	shown := modalText(view)
	if view["callback_id"] != answerForm || !strings.Contains(shown, slack.Escape(q)) || strings.Contains(shown, "<!channel>") || strings.Contains(shown, "<@U9>") {
		t.Fatalf("the answer modal: %s", shown)
	}
	before := len(v.api.Calls())
	if out := v.submit("U1", view, map[string]any{"answer": text("Use the first.")}); out != nil {
		t.Fatalf("an answered modal should close: %v", out)
	}
	v.s.slackWork.Wait()
	if got := told(t, v.api.Calls()[before:], "U1"); !strings.Contains(got, "Answered") {
		t.Fatalf("told: %q", got)
	}
	cs, _ := v.f.ListComments(ctx, ref)
	last := cs[len(cs)-1]
	if last.Author != "alice" || !strings.Contains(last.Body, "Use the first.") {
		t.Fatalf("the answer should be alice's comment: %+v", last)
	}
}

// Approve shows the held change, escaped, and the commit; it approves
// that commit as the clicker, and only while devbot still holds it.
func TestSlackApproveButton(t *testing.T) {
	v := newSlackEnv(t)
	ctx := context.Background()
	v.f.Grant(appRepo, "dave", forge.PermRead)
	v.linkAs("U1", "alice")
	v.linkAs("U2", "dave")
	const sha = "aaaa1111222233334444"
	ref := v.held(sha, "Diff: <!channel> <https://evil|click> & more")
	b := slack.Button{Task: ref.String(), State: "needs-approval", SHA: sha}

	view := modal(t, v.click("U1", slack.ActionApprove, b))
	shown := modalText(view)
	if view["callback_id"] != approveForm || !strings.Contains(shown, sha) || !strings.Contains(shown, slack.Escape("Diff: <!channel> <https://evil|click> & more")) ||
		strings.Contains(shown, "<!channel>") || strings.Contains(shown, "<https://evil") {
		t.Fatalf("the approve modal: %s", shown)
	}

	// dave may only read: refused as the MCP refuses him, nothing approved.
	daveView := modal(t, v.click("U2", slack.ActionApprove, b))
	out := v.submit("U2", daveView, nil)
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	dave := v.login(cid, "http://127.0.0.1/callback", "dave")["access_token"].(string)
	_, mcpText, isErr := v.mcp(dave, "approve_task", `{"ref":"`+ref.String()+`","sha":"`+sha+`"}`)
	if got := modalText(out["view"].(map[string]any)); !isErr || !strings.Contains(got, slack.Escape(mcpText)) {
		t.Fatalf("dave's approval: Slack %s, MCP %q", got, mcpText)
	}
	if it, _ := v.f.GetItem(ctx, ref); slices.Contains(it.Labels, core.LApprove) {
		t.Fatal("dave approved")
	}

	// devbot holds another commit by the time alice submits: refused, and
	// nothing approved.
	v.f.AddCommentAt(ref, v.f.Impl, "Holding the next.\n\n"+core.RenderStatus(core.Status{Kind: "held", SHA: "bbbb1111", Branch: "devbot/1-x",
		Files: []core.HeldFile{{Path: "ci/build.yml", Mode: "100644", Blob: strings.Repeat("2", 40)}}}), v.f.Now().Add(-time.Minute))
	out = v.submit("U1", view, nil)
	if got := modalText(out["view"].(map[string]any)); !strings.Contains(got, "approved nothing") || !strings.Contains(got, "bbbb1111") {
		t.Fatalf("approving a commit no longer held: %s", got)
	}
	if it, _ := v.f.GetItem(ctx, ref); slices.Contains(it.Labels, core.LApprove) {
		t.Fatal("a moved hold was approved")
	}
	// A click from the message posted for the first hold is refused before
	// any modal can approve it.
	if got := modalText(modal(t, v.click("U1", slack.ActionApprove, b))); !strings.Contains(got, "moved since this message was posted") {
		t.Fatalf("a click on a moved hold: %s", got)
	}

	// The current hold, approved by alice.
	view = modal(t, v.click("U1", slack.ActionApprove, slack.Button{Task: ref.String(), State: "needs-approval", SHA: "bbbb1111"}))
	if out := v.submit("U1", view, nil); out != nil {
		t.Fatalf("an approved modal should close: %v", out)
	}
	v.s.slackWork.Wait()
	cs, _ := v.f.ListComments(ctx, ref)
	if !slices.ContainsFunc(cs, func(c forge.Comment) bool {
		st, ok := core.ParseStatus(c.Body)
		return ok && st.Kind == "approval" && st.SHA == "bbbb1111" && st.Principal == "alice"
	}) {
		t.Fatalf("alice's approval of bbbb1111: %v", cs)
	}
}

// An unlinked clicker gets the connect prompt, only to them, and nothing
// happens: no modal, no change.
func TestSlackButtonUnlinked(t *testing.T) {
	v := newSlackEnv(t)
	ctx := context.Background()
	ref := v.f.NewIssue(appRepo, "alice", "stuck", "", core.LNeedsHuman)
	for _, action := range []string{slack.ActionRetry, slack.ActionCancel, slack.ActionAnswer, slack.ActionApprove} {
		calls := v.click("U9", action, slack.Button{Task: ref.String(), State: "needs-human", SHA: "aaaa111"})
		if got := told(t, calls, "U9"); !strings.Contains(got, "Connect your fake account") {
			t.Fatalf("%s: %q", action, got)
		}
		for _, c := range calls {
			if strings.HasPrefix(c.Method, "views.") {
				t.Fatalf("%s opened a modal: %+v", action, c)
			}
		}
	}
	if st, _ := v.s.E.Status(ctx, core.Caller{}, ref.String()); st.State != "needs-human" {
		t.Fatalf("state: %s", st.State)
	}
}

// Under the anonymous opt-in nobody can approve from Slack: the click is
// refused as approve_task refuses it, and no modal opens. The notifier
// offers no Approve button there either.
func TestSlackApproveRefusedAnonymously(t *testing.T) {
	s, _, err := newSlackServer(t, func(c *config.Config) { c.AnonymousPrincipal = "chat" })
	if err != nil {
		t.Fatal(err)
	}
	api := slacktest.New()
	withSlackAPI(t, api)(s)
	ctx := context.Background()
	if err := s.Slack.Save(ctx, slack.Installation{Workspace: acme, Token: "xoxb"}); err != nil {
		t.Fatal(err)
	}
	f := s.E.Forges["fake"].(*fake.Forge)
	f.AddRepo(appRepo, approvableApp)
	ref := f.NewIssue(appRepo, "alice", "held", "", core.LNeedsApproval)
	f.AddCommentAt(ref, f.Impl, "Holding.\n\n"+core.RenderStatus(core.Status{Kind: "held", SHA: "aaaa1111", Branch: "devbot/1-x",
		Files: []core.HeldFile{{Path: "ci/build.yml", Mode: "100644", Blob: strings.Repeat("1", 40)}}}), f.Now().Add(-time.Minute))
	value, _ := json.Marshal(slack.Button{Task: ref.String(), State: "needs-approval", SHA: "aaaa1111"})
	s.slackButton(ctx, slack.Interaction{Type: "block_actions", TriggerID: "t1", Workspace: acme, User: "U1"},
		slackClick{Action: slack.ActionApprove, Value: string(value), Channel: "C1"})
	s.slackWork.Wait()
	if got := told(t, api.Calls(), "U1"); !strings.Contains(got, "signed in through the forge") {
		t.Fatalf("an anonymous approval: %q", got)
	}
	for _, c := range api.Calls() {
		if strings.HasPrefix(c.Method, "views.") {
			t.Fatalf("a modal opened: %+v", c)
		}
	}
	if it, _ := f.GetItem(ctx, ref); slices.Contains(it.Labels, core.LApprove) {
		t.Fatal("approved")
	}
	if s.slackNotifier().Held != nil {
		t.Fatal("the notifier offers Approve without sign-in")
	}
}

// A long held change is shown in sections Slack takes, each cut at a line
// end or a rune, never inside an entity, and cut short past the most a
// modal shows.
func TestSlackChunks(t *testing.T) {
	text := slack.Escape(strings.Repeat("a <b> & é\n", 30) + strings.Repeat("&<>é", 3000))
	parts := slackChunks(text, 100, 5000)
	if !strings.Contains(parts[len(parts)-1], "cut short") {
		t.Fatalf("not cut short: %q", parts[len(parts)-1])
	}
	joined := strings.Join(parts[:len(parts)-1], "")
	if !strings.HasPrefix(text, joined) || len(joined) > 5000 || len(joined) < 4990 {
		t.Fatalf("joined %d bytes", len(joined))
	}
	for _, p := range parts {
		if len(p) > 100 || !utf8.ValidString(p) || strings.Count(p, "&") != strings.Count(p, ";") {
			t.Fatalf("part %q", p)
		}
	}
	if got := slackChunks("short", 100, 5000); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short: %q", got)
	}
}

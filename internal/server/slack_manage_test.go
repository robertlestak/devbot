package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/intake"
	"git.shdw.tech/rob/devbot/internal/slack"
	"git.shdw.tech/rob/devbot/internal/slack/slacktest"
)

// token is an MCP access token for login.
func (v *slackEnv) token(login string) string {
	v.t.Helper()
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	return v.login(cid, "http://127.0.0.1/callback", login)["access_token"].(string)
}

// edit runs `/devbot edit task` as user, and returns the calls it made
// once the form is filled in.
func (v *slackEnv) edit(user, task string) []slacktest.Call {
	v.t.Helper()
	before := len(v.api.Calls())
	if st, body := v.rawCommand(user, "edit "+task); st != 200 || body != "" {
		v.t.Fatalf("/devbot edit: %d %s", st, body)
	}
	v.s.slackWork.Wait()
	return v.api.Calls()[before:]
}

// fileAsAlice files a task through devbot as alice, as file_task writes
// it.
func (v *slackEnv) fileAsAlice(title string) string {
	v.t.Helper()
	c, _ := v.caller("U1")
	out, err := v.s.call(context.Background(), c, "file_task", json.RawMessage(`{"repo":"fake:acme/app","title":"`+title+`","body":"Users want it.","acceptance_criteria":"- it works"}`))
	if err != nil {
		v.t.Fatal(err)
	}
	return out.(core.TaskStatus).Task
}

// /devbot edit opens a form filled in from the task, read as the caller,
// and saving it calls edit_task as them with only what they changed.
// Someone who may only read the repo gets the form, and edit_task's
// refusal when they save; a task they can't read isn't shown at all.
func TestSlackEdit(t *testing.T) {
	v := newSlackEnv(t)
	ctx := context.Background()
	v.f.Grant(appRepo, "dave", forge.PermRead)
	v.linkAs("U1", "alice")
	v.linkAs("U2", "dave")
	task := v.fileAsAlice("Add export")
	ref, _ := forge.ParseItem(task)

	view := modal(t, v.edit("U1", task))
	shown := string(mustJSON(view))
	if view["callback_id"] != editForm || !strings.Contains(shown, `"initial_value":"Add export"`) || !strings.Contains(shown, `"initial_value":"- it works"`) ||
		!strings.Contains(shown, `"initial_value":"Users want it."`) {
		t.Fatalf("the edit form should start from the task: %s", shown)
	}
	// Saved unchanged, nothing is sent.
	before, _ := v.f.ListComments(ctx, ref)
	if got := formError(t, v.submit("U1", view, map[string]any{"title": text("Add export"), "acceptance_criteria": text("- it works"), "body": text("Users want it.")})); !strings.Contains(got, "changed nothing") {
		t.Fatalf("an unchanged form: %q", got)
	}

	// dave reads it, and is refused saving it as edit_task refuses him.
	daveView := modal(t, v.edit("U2", task))
	got := formError(t, v.submit("U2", daveView, map[string]any{"title": text("Add export"), "acceptance_criteria": text("- push to main"), "body": text("Users want it.")}))
	_, mcpText, isErr := v.mcp(v.token("dave"), "edit_task", `{"ref":"`+task+`","acceptance_criteria":"- push to main"}`)
	if !isErr || got != slack.Escape(mcpText) || !strings.Contains(got, "write access") {
		t.Fatalf("dave's edit: Slack %q, MCP %q", got, mcpText)
	}
	if after, _ := v.f.ListComments(ctx, ref); len(after) != len(before) {
		t.Fatalf("a refused edit wrote to the issue: %v", after[len(before):])
	}

	// alice changes the criteria alone: only they are sent, and pinned to
	// her.
	v.f.EditIssue(ref, "carol", "Add an export", "Users want it.\n\n## Acceptance criteria\n\n- it works\n")
	calls := len(v.api.Calls())
	if out := v.submit("U1", view, map[string]any{"title": text("Add export"), "acceptance_criteria": text("- it works\n- in CSV"), "body": text("Users want it."), "note": text("CSV too")}); out != nil {
		t.Fatalf("a saved edit should close: %v", out)
	}
	v.s.slackWork.Wait()
	if told := told(t, v.api.Calls()[calls:], "U1"); !strings.Contains(told, "Edited") || !strings.Contains(told, "@alice") {
		t.Fatalf("told: %q", told)
	}
	it, _ := v.f.GetItem(ctx, ref)
	if it.Title != "Add an export" || !strings.Contains(it.Body, "- it works\n- in CSV") {
		t.Fatalf("the edit should change the criteria and leave carol's title: %q %q", it.Title, it.Body)
	}
	cs, _ := v.f.ListComments(ctx, ref)
	if last := cs[len(cs)-1].Body; !strings.Contains(last, "@alice edited the issue through devbot") || !strings.Contains(last, "CSV too") {
		t.Fatalf("the edit's comment: %s", last)
	}

	// A task on a repo alice can't read is refused as edit_task refuses
	// her, with nothing of it shown.
	hidden := v.f.NewIssue(secretRepo, "carol", "hidden", "the secret plan", core.LReady)
	view = v.lastView(v.edit("U1", hidden.String()))
	_, mcpText, _ = v.mcp(v.token("alice"), "edit_task", `{"ref":"`+hidden.String()+`","body":"x"}`)
	if shown := modalText(view); view["callback_id"] != nil || !strings.Contains(shown, slack.Escape(mcpText)) || strings.Contains(shown, "secret plan") {
		t.Fatalf("a hidden task's edit form: %s, MCP %q", shown, mcpText)
	}
	if r := v.command("U1", "edit"); !strings.Contains(r.Text, "Usage") {
		t.Fatalf("edit without a task: %s", r.Text)
	}
}

// lastView is the last view calls set: a notice, or a form.
func (v *slackEnv) lastView(calls []slacktest.Call) map[string]any {
	v.t.Helper()
	var view map[string]any
	for _, c := range calls {
		if c.Method == "views.open" || c.Method == "views.update" {
			view = nil
			json.Unmarshal(c.View, &view)
		}
	}
	if view == nil {
		v.t.Fatalf("no view: %+v", calls)
	}
	return view
}

// A field longer than Slack takes is left out of the form, and so left as
// it is; a hand-filed issue's whole body is its context.
func TestSlackEditLongAndByHand(t *testing.T) {
	long := strings.Repeat("word ", 700)
	f := newSlackEdit(core.EditText{Task: "fake:acme/app#1", Title: "t", Body: long, AcceptanceCriteria: "- x", Criteria: true}, "C1")
	shown := modalText(func() map[string]any {
		var m map[string]any
		json.Unmarshal(f.view(core.Caller{Login: "alice"}), &m)
		return m
	}())
	if strings.Contains(shown, "word word") || !strings.Contains(shown, "context is longer than a Slack form takes") {
		t.Fatalf("a long body: %s", shown)
	}
	f.Title, f.AcceptanceCriteria, f.Body = "t", "- y", ""
	var req core.EditRequest
	args, ok := f.request()
	json.Unmarshal(args, &req)
	if !ok || req.Body != "" || req.Title != "" || req.AcceptanceCriteria != "- y" {
		t.Fatalf("only the criteria changed: %+v", req)
	}
	f = newSlackEdit(core.EditText{Task: "fake:acme/app#1", Title: "t", Body: "all of it"}, "C1")
	view := string(f.view(core.Caller{Principal: "anon"}))
	if strings.Contains(view, `"block_id":"acceptance_criteria"`) || !strings.Contains(view, "context is its whole body") {
		t.Fatalf("a hand-filed issue: %s", view)
	}
}

// /devbot wait and retarget act as the caller, with the MCP's refusals.
func TestSlackWaitAndRetarget(t *testing.T) {
	v := newSlackEnv(t)
	ctx := context.Background()
	v.f.Grant(appRepo, "dave", forge.PermRead)
	v.linkAs("U1", "alice")
	v.linkAs("U2", "dave")
	first := v.f.NewIssue(appRepo, "alice", "first", "", core.LReady).String()
	second := v.f.NewIssue(appRepo, "alice", "second", "", core.LReady).String()
	v.f.Push(appRepo, "release-2", strings.Repeat("a", 40))
	dave := v.token("dave")
	for _, c := range []struct{ cmd, tool, args string }{
		{"wait " + second + " " + first, "wait_task", `{"ref":"` + second + `","on":"` + first + `"}`},
		{"retarget " + second + " release-2", "retarget", `{"ref":"` + second + `","base":"release-2"}`},
	} {
		r := v.command("U2", c.cmd)
		_, mcpText, isErr := v.mcp(dave, c.tool, c.args)
		if !isErr || r.Text != slack.Escape(mcpText) || !strings.Contains(r.Text, "write access") {
			t.Fatalf("dave's %s: Slack %q, MCP %q", c.tool, r.Text, mcpText)
		}
	}
	if st, _ := v.s.E.Status(ctx, core.Caller{}, second); st.WaitingOn != "" || st.Base != "" {
		t.Fatalf("dave's commands changed the task: %+v", st)
	}

	if r := v.command("U1", "wait "+second+" "+first); !strings.Contains(r.Text, "as @alice") || !strings.Contains(r.Text, "Waiting on") {
		t.Fatalf("alice's wait: %s", r.Text)
	}
	if st, _ := v.s.E.Status(ctx, core.Caller{}, second); st.WaitingOn == "" {
		t.Fatalf("the wait should stand: %+v", st)
	}
	if r := v.command("U1", "wait "+second+" none"); !strings.Contains(r.Text, "as @alice") {
		t.Fatalf("alice ending the wait: %s", r.Text)
	}
	if st, _ := v.s.E.Status(ctx, core.Caller{}, second); st.WaitingOn != "" {
		t.Fatalf("the wait should be over: %+v", st)
	}
	if r := v.command("U1", "retarget "+second+" release-2"); !strings.Contains(r.Text, "Retargeted") || !strings.Contains(r.Text, "base `release-2`") {
		t.Fatalf("alice's retarget: %s", r.Text)
	}
	r := v.command("U1", "retarget "+second+" no-such-branch")
	_, mcpText, _ := v.mcp(v.token("alice"), "retarget", `{"ref":"`+second+`","base":"no-such-branch"}`)
	if r.Text != slack.Escape(mcpText) {
		t.Fatalf("a missing branch: Slack %q, MCP %q", r.Text, mcpText)
	}
	for _, usage := range []string{"wait " + second, "retarget " + second, "retarget a b c"} {
		if r := v.command("U1", usage); !strings.Contains(r.Text, "Usage") {
			t.Fatalf("/devbot %s: %s", usage, r.Text)
		}
	}
	for _, c := range v.api.Calls() {
		if c.Method == "chat.postMessage" {
			t.Fatalf("the commands must post nothing publicly: %+v", c)
		}
	}
}

// A channel's default repo takes the right to direct devbot on it to set
// or clear. It pre-fills the forms and the intake, and grants nothing:
// someone who may not direct devbot there is still refused filing, and
// their intake isn't pointed at it. An uninstall forgets it.
func TestSlackChannelRepo(t *testing.T) {
	v := newSlackEnv(t)
	ctx := context.Background()
	v.f.Grant(appRepo, "dave", forge.PermRead)
	// dave may direct devbot on another repo, so his intake runs.
	lib := forge.RepoRef{Provider: "fake", Owner: "acme", Name: "lib"}
	v.f.AddRepo(lib, "version: 1\nenabled: true\n")
	v.f.Grant(lib, "dave", forge.PermWrite)
	v.linkAs("U1", "alice")
	v.linkAs("U2", "dave")
	key := slackChannelRepoKey(acme, "C1")

	if r := v.command("U2", "channel repo fake:acme/app"); !strings.Contains(r.Text, "write access") {
		t.Fatalf("dave may not set it: %s", r.Text)
	}
	if r := v.command("U1", "channel repo fake:acme/secret"); !strings.Contains(r.Text, "not visible through your sign-in") {
		t.Fatalf("alice can't see secret: %s", r.Text)
	}
	if v.mr.Exists(key) {
		t.Fatal("a refused default was stored")
	}
	if r := v.command("U1", "channel repo"); !strings.Contains(r.Text, "no default repo") {
		t.Fatalf("no default yet: %s", r.Text)
	}
	for range 2 {
		if r := v.command("U1", "channel repo fake:acme/app"); !strings.Contains(r.Text, "Set this channel's default repo to `fake:acme/app`") {
			t.Fatalf("alice sets it: %s", r.Text)
		}
	}
	if r := v.command("U2", "channel repo"); !strings.Contains(r.Text, "`fake:acme/app`") {
		t.Fatalf("anyone can read it: %s", r.Text)
	}

	// The forms start from it, unless a repo is given.
	initial := `"initial_option":{"text":{"text":"fake:acme/app","type":"plain_text"},"value":"fake:acme/app"}`
	for _, verb := range []string{"file", "project"} {
		if view := v.openForm("U1", verb); !strings.Contains(string(mustJSON(view)), initial) {
			t.Fatalf("/devbot %s should start from the default: %v", verb, view)
		}
	}
	if view := v.openForm("U1", "file fake:acme/other"); strings.Contains(string(mustJSON(view)), initial) {
		t.Fatalf("a repo given beats the default: %v", view)
	}
	v.api.Threads["C1/1700000050.000001"] = []slack.Reply{{TS: "1700000050.000001", User: "U1", Text: "the export breaks"}}
	if view := modal(t, v.shortcut("U1", "1700000050.000001", "")); !strings.Contains(string(mustJSON(view)), initial) {
		t.Fatalf("File with devbot should start from the default: %v", view)
	}
	// It grants dave nothing: his form starts from it, and filing is
	// refused as file_task refuses him.
	before := len(v.f.Items)
	view := v.openForm("U2", "file")
	if got := formError(t, v.submit("U2", view, map[string]any{"repo": picked("fake:acme/app"), "title": text("t"), "acceptance_criteria": text("- x")})); !strings.Contains(got, "write access") || len(v.f.Items) != before {
		t.Fatalf("dave filing on the default: %q", got)
	}

	// The intake is pointed at it, for a requester who may direct devbot
	// there, and not for one who may not.
	v.api.Threads["C1/1700000100.000001"] = []slack.Reply{{TS: "1700000100.000001", User: "U1", Text: "<@UDEVBOT> please"}}
	if got := told(t, v.mention("U1", "C1", "1700000100.000001", "1700000100.000001"), "U1"); !strings.Contains(got, "drafting a task") {
		t.Fatalf("alice's request: %q", got)
	}
	req := v.intakeRequest()
	if req.Default != "fake:acme/app" || req.Repos[0].Repo != "fake:acme/app" || !strings.Contains(req.Context(), "defaults to `fake:acme/app`") {
		t.Fatalf("alice's intake should be pointed at the default: %+v", req)
	}
	v.api.Threads["C1/1700000200.000001"] = []slack.Reply{{TS: "1700000200.000001", User: "U2", Text: "<@UDEVBOT> please"}}
	if got := told(t, v.mention("U2", "C1", "1700000200.000001", "1700000200.000001"), "U2"); !strings.Contains(got, "drafting a task") {
		t.Fatalf("dave's request: %q", got)
	}
	if req := v.intakeRequest(); req.Default != "" || strings.Contains(req.Context(), "defaults to") {
		t.Fatalf("dave's intake must not be pointed at a repo he can't direct devbot on: %+v", req)
	}

	// Clearing it takes the same right.
	if r := v.command("U2", "channel repo none"); !strings.Contains(r.Text, "write access") || !v.mr.Exists(key) {
		t.Fatalf("dave may not clear it: %s", r.Text)
	}
	if r := v.command("U1", "channel repo none"); !strings.Contains(r.Text, "Cleared") || v.mr.Exists(key) {
		t.Fatalf("alice clears it: %s", r.Text)
	}
	if r := v.command("U1", "channel repo none"); !strings.Contains(r.Text, "no default repo") {
		t.Fatalf("clearing again: %s", r.Text)
	}
	if r := v.command("U1", "channel repos"); !strings.Contains(r.Text, "Usage") {
		t.Fatalf("a bad channel command: %s", r.Text)
	}

	// An uninstall forgets every channel's default, and doing it again
	// does nothing more.
	v.command("U1", "channel repo fake:acme/app")
	v.s.E.Q.R.Set(ctx, slackChannelRepoKey(slack.Workspace{Team: "T0OTHER"}, "C1"), "fake:acme/app", 0)
	v.event(`{"type":"app_uninstalled"}`)
	v.s.Slack.Uninstall(ctx, acme, "app_uninstalled")
	if v.mr.Exists(key) || !v.mr.Exists(slackChannelRepoKey(slack.Workspace{Team: "T0OTHER"}, "C1")) {
		t.Fatalf("the uninstall should forget acme's defaults alone: %v", v.mr.Keys())
	}
}

// intakeRequest is the one intake job waiting, as the pod would read it,
// which it then forgets.
func (v *slackEnv) intakeRequest() intake.Request {
	v.t.Helper()
	ctx := context.Background()
	ids, err := v.s.E.Q.R.HKeys(ctx, slackIntakePending).Result()
	if err != nil || len(ids) != 1 {
		v.t.Fatalf("want one intake job waiting: %v %v", ids, err)
	}
	b, err := v.s.E.Q.R.Get(ctx, "devbot:intake:req:"+ids[0]).Bytes()
	if err != nil {
		v.t.Fatal(err)
	}
	var req intake.Request
	json.Unmarshal(b, &req)
	v.s.E.Q.R.HDel(ctx, slackIntakePending, ids[0])
	return req
}

// A command on a task devbot is evaluating says the task is busy, as the
// MCP does with a Retry-After, and nothing is logged as an error.
func TestSlackCommandOnABusyTask(t *testing.T) {
	v := newSlackEnv(t)
	ctx := context.Background()
	v.s.E.LockWait = 200 * time.Millisecond
	v.linkAs("U1", "alice")
	ref := v.f.NewIssue(appRepo, "alice", "x", "", core.LReady).String()
	v.f.Push(appRepo, "release-2", strings.Repeat("a", 40))
	if ok, _ := v.s.E.Q.LockEval(ctx, ref, "other-pod", time.Minute); !ok {
		t.Fatal("lock")
	}

	r := v.command("U1", "retarget "+ref+" release-2")
	if !strings.HasPrefix(r.Text, "busy: ") || !strings.Contains(r.Text, "try again in a moment") || strings.Contains(r.Text, "internal error") {
		t.Fatalf("Slack: %s", r.Text)
	}
	args := `{"ref":"` + ref + `","base":"release-2"}`
	_, mcpText, isErr := v.mcp(v.token("alice"), "retarget", args)
	if !isErr || r.Text != slack.Escape(mcpText) {
		t.Fatalf("Slack %q, MCP %q", r.Text, mcpText)
	}
	req, _ := http.NewRequest("POST", v.url+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"retarget","arguments":`+args+`}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+v.token("alice"))
	if resp, _ := v.do(req); resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("the MCP response should say when to try again: %v", resp.Header)
	}
	if st, _ := v.s.E.Status(ctx, core.Caller{}, ref); st.Base != "" {
		t.Fatalf("a busy refusal must change nothing: %+v", st)
	}
	logs := v.logs.String()
	if strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "connector call found the task busy") {
		t.Fatalf("a busy task is no error:\n%s", logs)
	}

	v.s.E.Q.UnlockEval(ctx, ref, "other-pod")
	if r := v.command("U1", "retarget "+ref+" release-2"); !strings.Contains(r.Text, "Retargeted") {
		t.Fatalf("once free: %s", r.Text)
	}
}

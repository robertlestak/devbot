package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/slack"
)

// rawCommand runs `/devbot text` as user in channel C1 of acme, and returns
// the status and body of its response.
func (v *slackEnv) rawCommand(user, text string) (int, string) {
	v.t.Helper()
	f := url.Values{"team_id": {"T0ACME"}, "user_id": {user}, "channel_id": {"C1"}, "command": {"/devbot"}, "text": {text}, "trigger_id": {"tr" + v.next()}}
	return v.signed("/slack/commands", "application/x-www-form-urlencoded", f.Encode())
}

// interact sends an interactivity payload as user of acme, and returns
// what devbot answered it with.
func (v *slackEnv) interact(user string, payload map[string]any) map[string]any {
	v.t.Helper()
	payload["team"] = map[string]any{"id": "T0ACME"}
	payload["user"] = map[string]any{"id": user}
	if payload["type"] != "block_suggestion" {
		payload["trigger_id"] = "ti" + v.next()
	}
	b, _ := json.Marshal(payload)
	st, body := v.signed("/slack/interactivity", "application/x-www-form-urlencoded", url.Values{"payload": {string(b)}}.Encode())
	if st != 200 {
		v.t.Fatalf("interaction: %d %s", st, body)
	}
	var out map[string]any
	if body != "" {
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			v.t.Fatalf("interaction answered %q", body)
		}
	}
	return out
}

// linkAs links Slack user to the forge login.
func (v *slackEnv) linkAs(user, login string) {
	v.t.Helper()
	if _, ok := v.api.Users[user]; !ok {
		v.api.Users[user] = slack.User{ID: user, Name: login}
	}
	v.link(v.connectURL(v.command(user, "")), user, login, "allow")
}

// openForm runs `/devbot verb` as user and returns the modal it opened.
func (v *slackEnv) openForm(user, verb string) map[string]any {
	v.t.Helper()
	if st, body := v.rawCommand(user, verb); st != 200 || body != "" {
		v.t.Fatalf("/devbot %s: %d %s", verb, st, body)
	}
	calls := v.api.Calls()
	last := calls[len(calls)-1]
	var view map[string]any
	if last.Method != "views.open" || json.Unmarshal(last.View, &view) != nil {
		v.t.Fatalf("/devbot %s should open a form: %+v", verb, last)
	}
	return view
}

// submit submits a form opened with view, filled in with values (block ID
// to the element's state, as Slack sends it), as user.
func (v *slackEnv) submit(user string, view map[string]any, values map[string]any) map[string]any {
	v.t.Helper()
	state := map[string]any{}
	for id, val := range values {
		state[id] = map[string]any{id: val}
	}
	return v.interact(user, map[string]any{"type": "view_submission", "view": map[string]any{
		"id": "V1", "callback_id": view["callback_id"], "private_metadata": view["private_metadata"], "state": map[string]any{"values": state}}})
}

func text(s string) map[string]any { return map[string]any{"type": "plain_text_input", "value": s} }
func picked(s string) map[string]any {
	return map[string]any{"selected_option": map[string]any{"value": s}}
}
func ticked() map[string]any {
	return map[string]any{"selected_options": []any{map[string]any{"value": "yes"}}}
}

// formError is the refusal a form was shown again with.
func formError(t *testing.T, answer map[string]any) string {
	t.Helper()
	view, _ := answer["view"].(map[string]any)
	blocks, _ := view["blocks"].([]any)
	if answer["response_action"] != "update" || len(blocks) == 0 {
		t.Fatalf("the form should come back with the refusal: %v", answer)
	}
	first := blocks[0].(map[string]any)
	return strings.TrimPrefix(first["text"].(map[string]any)["text"].(string), ":warning: ")
}

// metaRE is an issue's ref in devbot's comments, which differs between two
// issues filed the same way.
var metaRE = regexp.MustCompile(`#\d+`)

// Filing through the form makes the same issue, labels and metadata as
// file_task over MCP for the same person, announces it in the channel and
// records that message as its thread. A forged slack: origin over MCP
// records none.
func TestSlackFileTaskLikeMCP(t *testing.T) {
	v := newSlackEnv(t)
	ctx := context.Background()
	v.linkAs("U1", "alice")
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	at := v.login(cid, "http://127.0.0.1/callback", "alice")["access_token"].(string)

	view := v.openForm("U1", "file")
	if view["callback_id"] != fileForm || view["private_metadata"] != `{"channel":"C1"}` {
		t.Fatalf("the form: %v", view)
	}
	before := len(v.f.Items)
	if out := v.submit("U1", view, map[string]any{"repo": picked("fake:acme/app"), "title": text("Add a flag"),
		"acceptance_criteria": text("- the flag works"), "body": text("Because <reasons>."), "base": text(""), "automerge": ticked()}); out != nil {
		t.Fatalf("a filed form should close: %v", out)
	}
	if len(v.f.Items) != before+1 {
		t.Fatalf("one issue should be filed: %d", len(v.f.Items)-before)
	}
	calls := v.api.Calls()
	post := calls[len(calls)-1]
	m := regexp.MustCompile(`^<@U1> filed <[^|]+\|(fake:acme/app#\d+)>: Add a flag$`).FindStringSubmatch(post.Message.Text)
	if post.Method != "chat.postMessage" || post.Message.Channel != "C1" || m == nil {
		t.Fatalf("the filing should be announced in the channel: %+v", post)
	}
	slackRef := m[1]
	th, err := v.s.Slack.Thread(ctx, slackRef)
	if err != nil || th.Channel != "C1" || th.Workspace != acme || !strings.HasPrefix(th.TS, "1700000000.") {
		t.Fatalf("the thread should be the announcement: %+v", th)
	}

	// The same over MCP, with the origin Slack gave it.
	_, out, isErr := v.mcp(at, "file_task", `{"repo":"fake:acme/app","title":"Add a flag","acceptance_criteria":"- the flag works","body":"Because <reasons>.","automerge":true,"origin":"slack:T0ACME/C1"}`)
	var viaMCP core.TaskStatus
	if isErr || json.Unmarshal([]byte(out), &viaMCP) != nil {
		t.Fatalf("MCP: %s", out)
	}
	a, _ := forge.ParseItem(slackRef)
	b, _ := forge.ParseItem(viaMCP.Task)
	ia, _ := v.f.GetItem(ctx, a)
	ib, _ := v.f.GetItem(ctx, b)
	la, lb := slices.Sorted(slices.Values(ia.Labels)), slices.Sorted(slices.Values(ib.Labels))
	if ia.Title != ib.Title || ia.Body != ib.Body || ia.Author != "alice" || ib.Author != "alice" || !slices.Equal(la, lb) || !slices.Contains(la, core.LAutomerge) {
		t.Fatalf("Slack's issue %+v\nMCP's %+v", ia, ib)
	}
	ca, cb := v.f.Comments[a.String()], v.f.Comments[b.String()]
	if len(ca) == 0 || len(ca) != len(cb) {
		t.Fatalf("comments: %v / %v", ca, cb)
	}
	for i := range ca {
		if ca[i].Author != cb[i].Author || metaRE.ReplaceAllString(ca[i].Body, "#N") != metaRE.ReplaceAllString(cb[i].Body, "#N") {
			t.Fatalf("Slack's comment %q\nMCP's %q", ca[i].Body, cb[i].Body)
		}
	}
	sa, _ := v.s.E.Status(ctx, core.Caller{}, slackRef)
	if sa.Origin != "slack:T0ACME/C1" || sa.Requester != "alice" || !sa.Automerge {
		t.Fatalf("the task's status: %+v", sa)
	}
	// The MCP's slack: origin made no thread.
	if _, err := v.s.Slack.Thread(ctx, viaMCP.Task); !errors.Is(err, slack.ErrNoThread) {
		t.Fatalf("a forged origin must have no thread: %v", err)
	}
	if all, _ := v.s.Slack.Threads(ctx); len(all) != 1 {
		t.Fatalf("threads: %v", all)
	}
}

// Someone who may not direct devbot on the repo is refused as the MCP
// refuses them, only to them, and nothing is written.
func TestSlackFileRefused(t *testing.T) {
	v := newSlackEnv(t)
	ctx := context.Background()
	v.f.Grant(appRepo, "dave", forge.PermRead)
	v.linkAs("U1", "alice")
	v.linkAs("U2", "dave")
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	tokens := map[string]string{
		"alice": v.login(cid, "http://127.0.0.1/callback", "alice")["access_token"].(string),
		"dave":  v.login(cid, "http://127.0.0.1/callback", "dave")["access_token"].(string),
	}
	for _, c := range []struct{ user, login, repo, want string }{
		{"U2", "dave", "fake:acme/app", "doesn't have write access"},
		{"U1", "alice", "fake:acme/secret", "not visible through your sign-in"},
	} {
		before, calls := len(v.f.Items), len(v.api.Calls())
		view := v.openForm(c.user, "file")
		got := formError(t, v.submit(c.user, view, map[string]any{"repo": picked(c.repo), "title": text("t"), "acceptance_criteria": text("- x")}))
		_, mcpText, isErr := v.mcp(tokens[c.login], "file_task", `{"repo":"`+c.repo+`","title":"t","acceptance_criteria":"- x","origin":"slack:T0ACME/C1"}`)
		if !isErr || got != slack.Escape(mcpText) || !strings.Contains(got, c.want) {
			t.Fatalf("%s on %s: Slack %q, MCP %q", c.login, c.repo, got, mcpText)
		}
		if len(v.f.Items) != before {
			t.Fatalf("%s on %s: nothing should be filed", c.login, c.repo)
		}
		for _, call := range v.api.Calls()[calls:] {
			if call.Method == "chat.postMessage" {
				t.Fatalf("%s on %s: nothing should be posted: %+v", c.login, c.repo, call)
			}
		}
	}
	if all, _ := v.s.Slack.Threads(ctx); len(all) != 0 {
		t.Fatalf("no thread should be recorded: %v", all)
	}
	// The project form is refused the same way.
	view := v.openForm("U2", "project")
	if view["callback_id"] != projectForm {
		t.Fatalf("the project form: %v", view)
	}
	got := formError(t, v.submit("U2", view, map[string]any{"repo": picked("fake:acme/app"), "title": text("t")}))
	_, mcpText, _ := v.mcp(tokens["dave"], "file_project", `{"repo":"fake:acme/app","title":"t"}`)
	if got != slack.Escape(mcpText) {
		t.Fatalf("project: Slack %q, MCP %q", got, mcpText)
	}
}

func TestSlackFileProject(t *testing.T) {
	v := newSlackEnv(t)
	ctx := context.Background()
	v.linkAs("U1", "alice")
	view := v.openForm("U1", "project fake:acme/app")
	if !strings.Contains(string(mustJSON(view)), `"initial_option":{"text":{"text":"fake:acme/app","type":"plain_text"},"value":"fake:acme/app"}`) {
		t.Fatalf("the repo given should be chosen: %v", view)
	}
	if out := v.submit("U1", view, map[string]any{"repo": picked("fake:acme/app"), "title": text("Across"), "acceptance_criteria": text("- both"),
		"plan": ticked(), "repos": map[string]any{"selected_options": []any{map[string]any{"value": "fake:acme/app"}}}}); out != nil {
		t.Fatalf("a filed form should close: %v", out)
	}
	calls := v.api.Calls()
	post := calls[len(calls)-1]
	m := regexp.MustCompile(`filed the parent task <[^|]+\|(fake:acme/app#\d+)>: Across$`).FindStringSubmatch(post.Message.Text)
	if post.Method != "chat.postMessage" || m == nil {
		t.Fatalf("the parent should be announced: %+v", post)
	}
	st, err := v.s.E.Status(ctx, core.Caller{}, m[1])
	if err != nil || st.Kind != core.KindParent || st.Origin != "slack:T0ACME/C1" {
		t.Fatalf("the parent: %+v %v", st, err)
	}
	if _, err := v.s.Slack.Thread(ctx, m[1]); err != nil {
		t.Fatalf("the parent's thread: %v", err)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// status and list answer only the caller, and never show a task on a repo
// they can't read: two people, each reading one of two repos.
func TestSlackStatusAndListAreTheCallers(t *testing.T) {
	v := newSlackEnv(t)
	v.f.Grant(appRepo, "carol", forge.PermNone)
	v.f.Grant(secretRepo, "carol", forge.PermWrite)
	v.linkAs("U1", "alice")
	v.linkAs("U3", "carol")
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	tokens := map[string]string{
		"U1": v.login(cid, "http://127.0.0.1/callback", "alice")["access_token"].(string),
		"U3": v.login(cid, "http://127.0.0.1/callback", "carol")["access_token"].(string),
	}
	appTask := v.f.NewIssue(appRepo, "alice", "the app task", "", core.LReady).String()
	secretTask := v.f.NewIssue(secretRepo, "carol", "the secret task", "", core.LReady).String()
	sees := map[string]string{"U1": appTask, "U3": secretTask}
	hidden := map[string]string{"U1": secretTask, "U3": appTask}
	for _, user := range []string{"U1", "U3"} {
		for _, args := range []string{"list", "list ready", "list fake:acme/app", "list fake:acme/secret ready"} {
			r := v.command(user, args)
			if strings.Contains(r.Text, hidden[user]) {
				t.Fatalf("%s /devbot %s shows a task they can't read: %s", user, args, r.Text)
			}
		}
		if r := v.command(user, "list"); !strings.Contains(r.Text, sees[user]) {
			t.Fatalf("%s /devbot list should show their task: %s", user, r.Text)
		}
		if r := v.command(user, "status "+sees[user]); !strings.Contains(r.Text, sees[user]) || !strings.Contains(r.Text, "`ready`") {
			t.Fatalf("%s /devbot status: %s", user, r.Text)
		}
		r := v.command(user, "status "+hidden[user])
		_, mcpText, isErr := v.mcp(tokens[user], "task_status", `{"ref":"`+hidden[user]+`"}`)
		if !isErr || r.Text != slack.Escape(mcpText) {
			t.Fatalf("%s /devbot status of a hidden task: Slack %q, MCP %q", user, r.Text, mcpText)
		}
	}
	for _, c := range v.api.Calls() {
		if c.Method == "chat.postMessage" {
			t.Fatalf("status and list must post nothing publicly: %+v", c)
		}
	}
	if r := v.command("U1", "status"); !strings.Contains(r.Text, "Usage") {
		t.Fatalf("status without a task: %s", r.Text)
	}
}

// The repo picker offers only the repos the caller can direct devbot on.
func TestSlackRepoPicker(t *testing.T) {
	v := newSlackEnv(t)
	v.f.AddRepo(forge.RepoRef{Provider: "fake", Owner: "acme", Name: "off"}, "version: 1\nenabled: false\n")
	v.f.Grant(appRepo, "dave", forge.PermRead)
	v.linkAs("U1", "alice")
	v.linkAs("U2", "dave")
	if got := v.choices("U1", ""); !slices.Equal(got, []string{"fake:acme/app"}) {
		t.Fatalf("alice may direct devbot on the app alone: %v", got)
	}
	if got := v.choices("U1", "sec"); len(got) != 0 {
		t.Fatalf("alice searching for secret: %v", got)
	}
	if got := v.choices("U2", ""); len(got) != 0 {
		t.Fatalf("dave may only read: %v", got)
	}
	if got := v.choices("U9", ""); len(got) != 0 {
		t.Fatalf("an unlinked user: %v", got)
	}
	for _, k := range v.mr.Keys() {
		if strings.HasPrefix(k, "devbot:auth:link-start:") {
			t.Fatalf("an unlinked user's search must start no link: %s", k)
		}
	}
}

// A repo the bots work in but don't list, as Gitea's list leaves out a repo
// its site admin bots reach only as admins, is found from the caller's own
// list: alice, authorized there and able to write, gets it in the picker
// and in the intake's repos; dave, who only reads it, and carol, who can
// write but isn't on its authorized_users, get it in neither.
func TestSlackRepoPickerFindsReposTheBotsDontList(t *testing.T) {
	v := newSlackEnv(t)
	ops := forge.RepoRef{Provider: "fake", Owner: "acme", Name: "ops"}
	v.f.AddRepo(ops, "version: 1\nenabled: true\nauthorized_users: [alice, dave]\n")
	v.f.Unlisted = map[string]bool{ops.String(): true}
	v.f.Grant(ops, "dave", forge.PermRead)
	v.f.Grant(ops, "carol", forge.PermWrite)
	v.linkAs("U1", "alice")
	v.linkAs("U2", "dave")
	v.linkAs("U3", "carol")
	if got := v.warmChoices("U1", ""); !slices.Equal(got, []string{"fake:acme/app", "fake:acme/ops"}) {
		t.Fatalf("alice's picker: %v", got)
	}
	for _, u := range []string{"U2", "U3"} {
		if got := v.warmChoices(u, ""); len(got) != 0 {
			t.Fatalf("%s's picker: %v", u, got)
		}
	}

	v.api.Threads["C1/"+threadRoot] = []slack.Reply{{TS: threadRoot, User: "U1", Text: "<@UDEVBOT> fix the deploy"}}
	v.mention("U1", "C1", threadRoot, threadRoot)
	v.pod(&fakeModel{answer: func(c string) string {
		if !strings.Contains(c, "`fake:acme/ops`") {
			t.Errorf("alice's intake should offer acme/ops:\n%s", c)
		}
		return draftJSON
	}})
	v.results()
	if got := told(t, v.mention("U3", "C1", threadRoot, threadRoot), "U3"); !strings.Contains(got, "no repo you can direct devbot on") {
		t.Fatalf("carol's intake: %q", got)
	}
}

func TestSlackHelp(t *testing.T) {
	v := newSlackEnv(t)
	r := v.command("U1", "help")
	for _, verb := range []string{"file", "project", "status", "list", "edit", "wait", "retarget", "channel repo", "disconnect"} {
		if !strings.Contains(r.Text, "`/devbot "+verb) {
			t.Fatalf("help should name %s: %s", verb, r.Text)
		}
	}
	if r := v.command("U1", "frobnicate"); !strings.Contains(r.Text, "Connect your fake account") {
		t.Fatalf("an unlinked user's unknown command: %s", r.Text)
	}
	v.linkAs("U1", "alice")
	if r := v.command("U1", "frobnicate"); !strings.Contains(r.Text, "`/devbot help` lists them") {
		t.Fatalf("an unknown command: %s", r.Text)
	}
}

// Work Slack stopped waiting for still finishes, and its result goes to
// late.
func TestSlackSoon(t *testing.T) {
	s := &Server{slackBudget: 20 * time.Millisecond}
	if r, ok := slackSoon(s, context.Background(), func(context.Context) int { return 1 }, func(context.Context, int) { t.Error("late") }); !ok || r != 1 {
		t.Fatalf("quick work: %d %v", r, ok)
	}
	release, late := make(chan struct{}), make(chan int, 1)
	ctx, cancel := context.WithCancel(context.Background())
	_, ok := slackSoon(s, ctx, func(ctx context.Context) int {
		<-release
		if ctx.Err() != nil {
			return 0
		}
		return 2
	}, func(_ context.Context, r int) { late <- r })
	if ok {
		t.Fatal("slow work should be late")
	}
	// The request ending doesn't end the work.
	cancel()
	close(release)
	if r := <-late; r != 2 {
		t.Fatalf("late got %d", r)
	}
	s.slackWork.Wait()
}

// On a slow forge, /devbot list is acknowledged empty within the budget
// of serve receiving it, and its answer follows through response_url.
func TestSlackCommandAnswersLateOnASlowForge(t *testing.T) {
	v := newSlackEnv(t)
	v.linkAs("U1", "alice")
	task := v.fileAsAlice("Slow")
	v.s.slackBudget = 100 * time.Millisecond
	v.f.ReadDelay = 200 * time.Millisecond
	f := url.Values{"team_id": {"T0ACME"}, "user_id": {"U1"}, "channel_id": {"C1"}, "command": {"/devbot"}, "text": {"list"},
		"trigger_id": {"tr" + v.next()}, "response_url": {v.hooks.URL + "/commands/T0ACME/1/x"}}
	start := time.Now()
	st, body := v.signed("/slack/commands", "application/x-www-form-urlencoded", f.Encode())
	if took := time.Since(start); st != 200 || body != "" || took < v.s.slackBudget || took > v.s.slackBudget+500*time.Millisecond {
		t.Fatalf("acknowledged %d %q after %s, with a budget of %s", st, body, took, v.s.slackBudget)
	}
	select {
	case got := <-v.responses:
		var r slackReply
		if json.Unmarshal([]byte(got), &r) != nil || r.ResponseType != "ephemeral" || !strings.Contains(r.Text, task) {
			t.Fatalf("answered late with %s", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no answer through response_url")
	}
	v.s.slackWork.Wait()
}

// choices is what the repo picker offers user searching for query.
func (v *slackEnv) choices(user, query string) []string {
	v.t.Helper()
	out := v.interact(user, map[string]any{"type": "block_suggestion", "action_id": "repo", "block_id": "repo", "value": query})
	var got []string
	opts, _ := out["options"].([]any)
	for _, o := range opts {
		got = append(got, o.(map[string]any)["value"].(string))
	}
	return got
}

// warmChoices searches as user until the picker has all their repos.
func (v *slackEnv) warmChoices(user, query string) []string {
	v.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if got := v.choices(user, query); !slices.Contains(got, slackStillLoading) {
			return got
		}
	}
	v.t.Fatalf("%s's repos never finished warming", user)
	return nil
}

// On a forge with many repos and slow permission reads, the picker answers
// within its budget with what it has found, says it isn't done, and once
// warm offers the whole filtered list. Each user's list is their own.
func TestSlackRepoPickerOnASlowForge(t *testing.T) {
	v := newSlackEnv(t)
	var all []string
	for i := range 64 {
		r := forge.RepoRef{Provider: "fake", Owner: "acme", Name: fmt.Sprintf("r%02d", i)}
		v.f.AddRepo(r, "version: 1\nenabled: true\n")
		all = append(all, r.String())
	}
	// dave may direct devbot on a few, one of which alice can't read.
	for _, n := range []string{"r01", "r02", "r03", "r10"} {
		v.f.Grant(forge.RepoRef{Provider: "fake", Owner: "acme", Name: n}, "dave", forge.PermWrite)
	}
	v.f.Grant(forge.RepoRef{Provider: "fake", Owner: "acme", Name: "r03"}, "alice", forge.PermNone)
	v.linkAs("U1", "alice")
	v.linkAs("U2", "dave")
	const delay = 20 * time.Millisecond
	v.f.PermDelay = delay
	v.s.slackBudget = 100 * time.Millisecond

	alice := []string{"fake:acme/app"}
	for _, r := range all {
		if r != "fake:acme/r03" {
			alice = append(alice, r)
		}
	}
	slices.Sort(alice)

	// Cold, with no form opened: an answer in time, short of the whole list.
	start := time.Now()
	got := v.choices("U1", "")
	if took := time.Since(start); took > v.s.slackBudget+time.Second {
		t.Fatalf("a cold search took %s", took)
	}
	if len(got) == 0 || got[len(got)-1] != slackStillLoading || len(got)-1 >= len(alice) {
		t.Fatalf("a cold search should offer what it has and say it isn't done: %v", got)
	}
	if got := v.warmChoices("U1", ""); !slices.Equal(got, alice) {
		t.Fatalf("alice's warm list:\n%v\nwant\n%v", got, alice)
	}
	// Each repo is two permission reads: one at a time, that is over 2.6s.
	if took, serial := time.Since(start), time.Duration(2*len(alice))*delay; took > serial/2 {
		t.Fatalf("warming took %s; one at a time it takes %s", took, serial)
	}
	start = time.Now()
	if got := v.choices("U1", "R1"); !slices.Equal(got, alice[slices.Index(alice, "fake:acme/r10"):slices.Index(alice, "fake:acme/r20")]) {
		t.Fatalf("alice searching r1: %v", got)
	}
	if took := time.Since(start); took > v.s.slackBudget {
		t.Fatalf("a warm search took %s", took)
	}

	// Opening the form warms dave's list, and his is his alone.
	v.openForm("U2", "file")
	if got, want := v.warmChoices("U2", ""), []string{"fake:acme/r01", "fake:acme/r02", "fake:acme/r03", "fake:acme/r10"}; !slices.Equal(got, want) {
		t.Fatalf("dave's list: %v", got)
	}
	if got := v.choices("U1", "r03"); len(got) != 0 {
		t.Fatalf("alice must not get dave's repos: %v", got)
	}

	// The "still finding" option is no repo to file on.
	before := len(v.f.Items)
	out := v.submit("U1", v.openForm("U1", "file"), map[string]any{"repo": picked(slackStillLoading), "title": text("t"), "acceptance_criteria": text("ac")})
	if errs, _ := out["errors"].(map[string]any); out["response_action"] != "errors" || errs["repo"] == nil || len(v.f.Items) != before {
		t.Fatalf("submitting the still-finding option: %v", out)
	}
}

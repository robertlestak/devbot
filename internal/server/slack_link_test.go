package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/audit/audittest"
	"git.shdw.tech/rob/devbot/internal/auth"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/slack"
	"git.shdw.tech/rob/devbot/internal/slack/slacktest"
)

var acme = slack.Workspace{Team: "T0ACME"}

// slackEnv is serve with the Slack app on, its Web API the fake: in
// auth.mode oauth and installed in acme (newSlackEnv), or as newSlackEnvAs
// says.
type slackEnv struct {
	*oauthEnv
	api  *slacktest.Fake
	oidc *slacktest.OIDC
	n    int
	// responses are the bodies posted to a command's response_url, which
	// is under hooks.
	hooks     *httptest.Server
	responses chan string
}

func newSlackEnv(t *testing.T) *slackEnv { return newSlackEnvAs(t, false, true) }

// newSlackEnvAs is serve with the Slack app on: in auth.mode none with
// slack.allow_anonymous (the anonymous principal "chat") when anonymous,
// else in mode oauth; and installed in acme already when installed, else
// to install through the fake's install page.
func newSlackEnvAs(t *testing.T, anonymous, installed bool) *slackEnv {
	t.Setenv("TEST_SLACK_SECRET", "client-secret")
	t.Setenv("TEST_SLACK_SIGNING", "signing-secret")
	v := &slackEnv{api: slacktest.New(), oidc: slacktest.NewOIDC(t, "1.2", "client-secret")}
	v.responses = make(chan string, 16)
	v.hooks = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		v.responses <- string(b)
	}))
	t.Cleanup(v.hooks.Close)
	v.api.Users["U1"] = slack.User{ID: "U1", TeamID: "T0ACME", Name: "ann", RealName: "Ann Smith"}
	v.oauthEnv = newOAuthEnvWith(t, func(c *config.Config) {
		c.Slack = config.Slack{Enabled: true, ClientID: "1.2", ClientSecretEnv: "TEST_SLACK_SECRET", SigningSecretEnv: "TEST_SLACK_SIGNING", AllowedTeams: []string{"T0ACME"}}
		if anonymous {
			c.Auth.Mode, c.AnonymousPrincipal, c.Slack.AllowAnonymous = "none", "chat", true
		}
	}, func(o *oauthEnv) {
		if anonymous {
			o.s.Auth = nil
		}
		sealer, err := auth.NewSealer([]byte("an-auth-key-for-tests-that-is-long-enough"), nil)
		if err != nil {
			t.Fatal(err)
		}
		v.oidc.Now = o.clock
		// The app on the test's clock, its Web API the fake, as are its
		// install page and Sign in with Slack.
		a, err := slack.New(slack.Options{ClientID: "1.2", ClientSecret: "client-secret", SigningSecret: "signing-secret", ResourceURL: o.url,
			AllowedTeams: []string{"T0ACME"}, Redis: o.s.E.Q.R, Sealer: sealer, Log: slog.New(slog.DiscardHandler),
			NewAPI: func(string) slack.API { return v.api }, Now: o.clock, APIURL: v.oidc.APIURL(), OIDCURL: v.oidc.AuthorizeURL(), AuthorizeURL: v.oidc.InstallURL(), HooksURL: v.hooks.URL + "/"})
		if err != nil {
			t.Fatal(err)
		}
		if installed {
			if err := a.Save(context.Background(), slack.Installation{Workspace: acme, TeamName: "Acme", Token: "xoxb-acme"}); err != nil {
				t.Fatal(err)
			}
		}
		o.s.Slack = a
		o.s.wireSlack()
	})
	return v
}

// signed posts body to path as Slack does, signed, and returns the
// response.
func (v *slackEnv) signed(path, ctype, body string) (int, string) {
	v.t.Helper()
	req, _ := http.NewRequest("POST", v.url+path, strings.NewReader(body))
	ts := strconv.FormatInt(v.clock().Unix(), 10)
	mac := hmac.New(sha256.New, []byte("signing-secret"))
	fmt.Fprintf(mac, "v0:%s:%s", ts, body)
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	resp, out := v.do(req)
	return resp.StatusCode, out
}

func (v *slackEnv) next() string { v.n++; return strconv.Itoa(v.n) }

// command runs `/devbot text` as user in channel C1 of acme, and returns
// the ephemeral reply.
func (v *slackEnv) command(user, text string) slackReply {
	v.t.Helper()
	f := url.Values{"team_id": {"T0ACME"}, "user_id": {user}, "channel_id": {"C1"}, "command": {"/devbot"}, "text": {text}, "trigger_id": {"tr" + v.next()}}
	st, body := v.signed("/slack/commands", "application/x-www-form-urlencoded", f.Encode())
	var r slackReply
	if st != 200 || json.Unmarshal([]byte(body), &r) != nil || r.ResponseType != "ephemeral" {
		v.t.Fatalf("/devbot %s: %d %s", text, st, body)
	}
	return r
}

// event delivers an Events API event in acme.
func (v *slackEnv) event(inner string) {
	v.t.Helper()
	body := `{"type":"event_callback","team_id":"T0ACME","event_id":"Ev` + v.next() + `","event":` + inner + `}`
	if st, out := v.signed("/slack/events", "application/json", body); st != 200 {
		v.t.Fatalf("event %s: %d %s", inner, st, out)
	}
}

var linkURL = regexp.MustCompile(`"url":"([^"]+)"`)

// connectURL is the connect button's URL in a prompt.
func (v *slackEnv) connectURL(r slackReply) string {
	v.t.Helper()
	b, _ := json.Marshal(r.Blocks)
	m := linkURL.FindStringSubmatch(string(b))
	if m == nil || !strings.Contains(r.Text, "Connect your fake account") {
		v.t.Fatalf("not a connect prompt: %s %s", r.Text, b)
	}
	u, _ := url.Parse(m[1])
	if u.Path != "/oauth/link" || u.Query().Get("state") == "" {
		v.t.Fatalf("the button should start a link: %s", u)
	}
	return v.url + u.RequestURI()
}

// proveSlack follows a connect URL to Sign in with Slack, signs in there as
// user of team, and returns devbot's answer to coming back.
func (v *slackEnv) proveSlack(u, team, user string) (*http.Response, string) {
	v.t.Helper()
	resp, body := v.get(u)
	if resp.StatusCode != 302 || !strings.HasPrefix(resp.Header.Get("Location"), v.oidc.AuthorizeURL()+"?") {
		v.t.Fatalf("the link should start with Sign in with Slack: %d %s %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	back, err := v.oidc.SignIn(resp.Header.Get("Location"), "", team, user)
	if err != nil {
		v.t.Fatal(err)
	}
	return v.get(back)
}

// link follows a connect URL as slackUser of acme in Sign in with Slack,
// then as login through the forge and the consent page, and returns the
// consent page and the final page.
func (v *slackEnv) link(u, slackUser, login, decision string) (consent, done string) {
	v.t.Helper()
	resp, body := v.proveSlack(u, "T0ACME", slackUser)
	if resp.StatusCode != 302 || !strings.HasPrefix(resp.Header.Get("Location"), "https://fake/login/oauth/authorize?") {
		v.t.Fatalf("a proved link should go on to a forge sign-in: %d %s", resp.StatusCode, body)
	}
	cb, err := v.f.SignIn(resp.Header.Get("Location"), login)
	if err != nil {
		v.t.Fatal(err)
	}
	resp, consent = v.get(cb)
	m := consentToken.FindStringSubmatch(consent)
	if resp.StatusCode != 200 || m == nil {
		v.t.Fatalf("the callback should ask for consent: %d %s", resp.StatusCode, consent)
	}
	req, _ := http.NewRequest("POST", v.url+"/oauth/consent", strings.NewReader(url.Values{"consent": {m[1]}, "decision": {decision}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", v.url)
	resp, done = v.do(req)
	if resp.StatusCode != 200 {
		v.t.Fatalf("consent: %d %s", resp.StatusCode, done)
	}
	return consent, done
}

// caller is who a Slack request from user in acme is for, or the prompt.
func (v *slackEnv) caller(user string) (core.Caller, *slackReply) {
	v.t.Helper()
	c, prompt, err := v.s.slackCaller(context.Background(), acme, user, "C1")
	if err != nil {
		v.t.Fatal(err)
	}
	return c, prompt
}

// linkKeys are the stored links.
func (v *slackEnv) linkKeys() []string {
	var out []string
	for _, k := range v.mr.Keys() {
		if strings.HasPrefix(k, "devbot:auth:link:") {
			out = append(out, k)
		}
	}
	return out
}

func TestSlackUnlinkedUserOnlyGetsTheConnectPrompt(t *testing.T) {
	v := newSlackEnv(t)
	items := len(v.f.Items)
	for _, text := range []string{"", "file fake:acme/app"} {
		v.connectURL(v.command("U1", text))
	}
	if len(v.f.Items) != items || len(v.linkKeys()) != 0 {
		t.Fatal("an unlinked user's command must do nothing else")
	}
	// A button: the prompt, posted to them alone.
	payload := `{"type":"block_actions","trigger_id":"t-btn","team":{"id":"T0ACME"},"user":{"id":"U1"},"channel":{"id":"C1"},"actions":[{"action_id":"devbot_approve"}]}`
	if st, out := v.signed("/slack/interactivity", "application/x-www-form-urlencoded", url.Values{"payload": {payload}}.Encode()); st != 200 {
		t.Fatalf("interaction: %d %s", st, out)
	}
	calls := v.api.Calls()
	last := calls[len(calls)-1]
	if last.Method != "chat.postEphemeral" || last.User != "U1" || last.Message.Channel != "C1" || !strings.Contains(string(last.Message.Blocks), "/oauth/link?state=") {
		t.Fatalf("the button should get the prompt, ephemerally: %+v", last)
	}
	// Clicking the connect button itself only opens it.
	n := len(v.api.Calls())
	payload = `{"type":"block_actions","trigger_id":"t-connect","team":{"id":"T0ACME"},"user":{"id":"U1"},"channel":{"id":"C1"},"actions":[{"action_id":"devbot_connect"}]}`
	v.signed("/slack/interactivity", "application/x-www-form-urlencoded", url.Values{"payload": {payload}}.Encode())
	if len(v.api.Calls()) != n {
		t.Fatalf("the connect click should post nothing: %+v", v.api.Calls()[n:])
	}
}

// Linking makes the Slack user the forge user, and a call through Slack is
// authorized exactly as the same person's MCP call: both take s.call.
func TestSlackLinkActsAsTheForgeUser(t *testing.T) {
	v := newSlackEnv(t)
	ctx := context.Background()
	consent, done := v.link(v.connectURL(v.command("U1", "")), "U1", "alice", "allow")
	if !strings.Contains(consent, "Ann Smith @ann (U1)") || !strings.Contains(consent, "Acme (T0ACME)") || !strings.Contains(consent, "@alice") {
		t.Fatalf("the consent page should name the Slack user, the workspace and the forge account: %s", consent)
	}
	if !strings.Contains(done, "@alice on fake") {
		t.Fatalf("done: %s", done)
	}
	calls := v.api.Calls()
	if last := calls[len(calls)-1]; last.Method != "chat.postEphemeral" || last.User != "U1" || last.Message.Channel != "C1" || !strings.Contains(last.Message.Text, "@alice on fake") {
		t.Fatalf("Slack should confirm, to U1 alone, naming the login and forge: %+v", last)
	}
	if r := v.command("U1", ""); !strings.Contains(r.Text, "@alice on fake") || len(r.Blocks) != 0 {
		t.Fatalf("/devbot once linked: %+v", r)
	}

	c, prompt := v.caller("U1")
	if prompt != nil || c.Principal != "alice" || c.Login != "alice" || c.Provider != "fake" || c.User == nil {
		t.Fatalf("the caller should be alice on the forge: %+v %+v", c, prompt)
	}
	// The same person over MCP, and through Slack, call the same tools.
	cid := v.register(`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]}`)
	at := v.login(cid, "http://127.0.0.1/callback", "alice")["access_token"].(string)
	viaSlack := func(c core.Caller, tool, args string) (string, bool) {
		out, err := v.s.call(ctx, c, tool, json.RawMessage(args))
		res := v.s.toolResult(httptest.NewRequest("POST", "/slack/commands", nil), out, err)
		return res["content"].([]map[string]any)[0]["text"].(string), res["isError"] == true
	}
	hidden := v.f.NewIssue(secretRepo, "carol", "hidden", "", core.LReady)
	for _, call := range []struct{ tool, args string }{
		{"file_task", `{"repo":"fake:acme/secret","title":"t","acceptance_criteria":"- x"}`},
		{"task_status", `{"ref":"` + hidden.String() + `"}`},
	} {
		_, mcpText, mcpErr := v.mcp(at, call.tool, call.args)
		text, isErr := viaSlack(c, call.tool, call.args)
		if !isErr || !mcpErr || text != mcpText {
			t.Errorf("%s %s: Slack %q (%v), MCP %q (%v)", call.tool, call.args, text, isErr, mcpText, mcpErr)
		}
	}
	text, isErr := viaSlack(c, "file_task", `{"repo":"fake:acme/app","title":"t","acceptance_criteria":"- x"}`)
	var filed core.TaskStatus
	json.Unmarshal([]byte(text), &filed)
	ref, _ := forge.ParseItem(filed.Task)
	if it, _ := v.f.GetItem(ctx, ref); isErr || it.Author != "alice" || filed.Requester != "alice" {
		t.Fatalf("a task filed from Slack should be alice's own: %s", text)
	}

	// Someone who may read but not direct devbot is refused the same way.
	v.f.Grant(appRepo, "dave", forge.PermRead)
	v.api.Users["U2"] = slack.User{ID: "U2", Name: "dee"}
	v.link(v.connectURL(v.command("U2", "")), "U2", "dave", "allow")
	dc, _ := v.caller("U2")
	dat := v.login(cid, "http://127.0.0.1/callback", "dave")["access_token"].(string)
	args := `{"repo":"fake:acme/app","title":"t","acceptance_criteria":"- x"}`
	_, mcpText, _ := v.mcp(dat, "file_task", args)
	if text, isErr := viaSlack(dc, "file_task", args); !isErr || text != mcpText || !strings.Contains(text, "doesn't have write access") {
		t.Fatalf("dave via Slack %q, via MCP %q", text, mcpText)
	}
	v.assertNoLeak(at, dat)
}

// A link started from a channel the bot isn't in is confirmed in the
// user's messages with the app, since Slack refuses the ephemeral there.
func TestSlackLinkedConfirmedInMessagesWhenNotInChannel(t *testing.T) {
	v := newSlackEnv(t)
	v.api.Fail = func(c slacktest.Call) error {
		if c.Method == "chat.postEphemeral" {
			return &slack.APIError{Method: c.Method, Code: "not_in_channel"}
		}
		return nil
	}
	n := len(v.api.Calls())
	v.link(v.connectURL(v.command("U1", "")), "U1", "alice", "allow")
	var dm []slacktest.Call
	for _, c := range v.api.Calls()[n:] {
		if c.Method == "chat.postMessage" {
			dm = append(dm, c)
		}
	}
	if len(dm) != 1 || dm[0].Message.Channel != "U1" || !strings.Contains(dm[0].Message.Text, "You're connected") || !strings.Contains(dm[0].Message.Text, "@alice on fake") {
		t.Fatalf("the confirmation should go to U1's messages with the app: %+v", v.api.Calls()[n:])
	}
}

// A link is one per Slack user: linking again replaces it, and ends the
// sign-in it replaced. Declining links nothing.
func TestSlackRelinkReplaces(t *testing.T) {
	v := newSlackEnv(t)
	ctx := context.Background()
	v.link(v.connectURL(v.command("U1", "")), "U1", "alice", "allow")
	// Linked, U1 gets no prompt; a link started another way replaces it.
	again, _ := v.s.Auth.LinkURL(ctx, auth.Link{Subject: slackSubject(acme, "U1"), Group: slackGroup(acme), Who: "U1"})
	v.link(again, "U1", "dave", "allow")
	if c, _ := v.caller("U1"); c.Login != "dave" {
		t.Fatalf("the newest link should stand: %+v", c)
	}
	if n := len(v.linkKeys()); n != 1 {
		t.Fatalf("one link per Slack user: %v", v.linkKeys())
	}
	for _, k := range v.mr.Keys() {
		if s, _ := v.mr.Get(k); strings.HasPrefix(k, "devbot:auth:grant:") && strings.Contains(s, `"login":"alice"`) {
			t.Fatalf("the replaced link's sign-in should have ended: %s", k)
		}
	}
	v.api.Users["U4"] = slack.User{ID: "U4", Name: "eve"}
	if _, done := v.link(v.connectURL(v.command("U4", "")), "U4", "erin", "deny"); !strings.Contains(done, "Nothing was linked") {
		t.Fatalf("deny: %s", done)
	}
	if _, prompt := v.caller("U4"); prompt == nil {
		t.Fatal("a declined link must leave U4 unlinked")
	}
}

// The connect state works once, for a short while, and links only the Slack
// user it was issued for.
func TestSlackConnectState(t *testing.T) {
	v := newSlackEnv(t)
	v.api.Users["U2"] = slack.User{ID: "U2", Name: "bo"}

	// Reuse: the second visit is refused, before any sign-in.
	u := v.connectURL(v.command("U1", ""))
	if resp, _ := v.get(u); resp.StatusCode != 302 {
		t.Fatalf("first use: %d", resp.StatusCode)
	}
	if resp, body := v.get(u); resp.StatusCode != 400 || !strings.Contains(body, "already used or has expired") {
		t.Fatalf("a used state must be refused: %d %s", resp.StatusCode, body)
	}

	// Expiry.
	u = v.connectURL(v.command("U1", ""))
	v.advance(11 * time.Minute)
	if resp, body := v.get(u); resp.StatusCode != 400 || !strings.Contains(body, "already used or has expired") {
		t.Fatalf("an expired state must be refused: %d %s", resp.StatusCode, body)
	}

	// Mismatch: U1's state links U1, whoever the request says it is for.
	forU1 := v.connectURL(v.command("U1", ""))
	forU2 := v.connectURL(v.command("U2", ""))
	consent, _ := v.link(forU1+"&user=U2&team=T0ACME&subject=slack::T0ACME:U2", "U1", "alice", "allow")
	if !strings.Contains(consent, "(U1)") || strings.Contains(consent, "(U2)") {
		t.Fatalf("the consent page should name U1 alone: %s", consent)
	}
	if c, _ := v.caller("U1"); c.Login != "alice" {
		t.Fatalf("U1 should be linked: %+v", c)
	}
	if _, prompt := v.caller("U2"); prompt == nil {
		t.Fatal("U1's state must not link U2")
	}
	// U2's own state still links U2, to whoever signs in with it.
	v.link(forU2, "U2", "bob", "allow")
	if c, _ := v.caller("U2"); c.Login != "bob" {
		t.Fatalf("U2: %+v", c)
	}
	// And a state from Slack is no use to anyone else's command: it is
	// stored by its hash only.
	for _, k := range v.mr.Keys() {
		if strings.HasPrefix(k, "devbot:auth:link-start:") && strings.Contains(u, strings.TrimPrefix(k, "devbot:auth:link-start:")) {
			t.Fatalf("a state is stored in the clear: %s", k)
		}
	}
}

// Each way a link ends leaves the Slack user unlinked, and running it again
// does nothing more.
func TestSlackUnlinking(t *testing.T) {
	ctx := context.Background()
	linked := func(t *testing.T) *slackEnv {
		v := newSlackEnv(t)
		v.f.SetClock(v.clock)
		v.f.SetTokenTTL(10 * time.Minute)
		v.link(v.connectURL(v.command("U1", "")), "U1", "alice", "allow")
		if c, _ := v.caller("U1"); c.Login != "alice" {
			t.Fatalf("not linked: %+v", c)
		}
		return v
	}
	unlinked := func(v *slackEnv) {
		v.t.Helper()
		if _, prompt := v.caller("U1"); prompt == nil {
			v.t.Fatal("U1 should be unlinked")
		}
		if len(v.linkKeys()) != 0 {
			v.t.Fatalf("links left: %v", v.linkKeys())
		}
		for _, k := range v.mr.Keys() {
			if s, _ := v.mr.Get(k); strings.HasPrefix(k, "devbot:auth:grant:") && strings.Contains(s, `"login":"alice"`) {
				v.t.Fatalf("the link's sign-in should have ended: %s", k)
			}
			if strings.HasPrefix(k, "devbot:auth:forge:") {
				v.t.Fatalf("alice's forge token should be gone with her last sign-in: %s", k)
			}
		}
	}

	t.Run("disconnect", func(t *testing.T) {
		v := linked(t)
		if r := v.command("U1", "disconnect"); !strings.Contains(r.Text, "no longer acts as @alice on fake") {
			t.Fatalf("disconnect: %s", r.Text)
		}
		unlinked(v)
		if r := v.command("U1", "disconnect"); !strings.Contains(r.Text, "isn't connected") {
			t.Fatalf("disconnect again: %s", r.Text)
		}
	})
	t.Run("forge refuses a refresh", func(t *testing.T) {
		v := linked(t)
		v.advance(9*time.Minute + 30*time.Second)
		v.f.FailRefresh(true)
		unlinked(v)
		v.f.FailRefresh(false)
		unlinked(v)
	})
	t.Run("forge rejects the token mid-call", func(t *testing.T) {
		v := linked(t)
		c, _ := v.caller("U1")
		v.f.RevokeUserTokens("alice")
		if _, err := v.s.call(ctx, c, "file_task", json.RawMessage(`{"repo":"fake:acme/app","title":"t","acceptance_criteria":"- x"}`)); err == nil {
			t.Fatal("a revoked token should fail the call")
		}
		unlinked(v)
	})
	t.Run("deactivated", func(t *testing.T) {
		v := linked(t)
		v.event(`{"type":"user_change","user":{"id":"U1","team_id":"T0ACME","deleted":false}}`)
		if c, _ := v.caller("U1"); c.Login != "alice" {
			t.Fatal("a profile change mustn't unlink")
		}
		v.event(`{"type":"user_change","user":{"id":"U1","team_id":"T0ACME","deleted":true}}`)
		unlinked(v)
		v.event(`{"type":"user_change","user":{"id":"U1","team_id":"T0ACME","deleted":true}}`)
		unlinked(v)
	})
	t.Run("uninstall", func(t *testing.T) {
		v := linked(t)
		v.event(`{"type":"app_uninstalled"}`)
		// Uninstalled, there is no bot left to resolve U1 with a name, but
		// the link is gone all the same.
		unlinked(v)
		if err := v.s.Slack.Uninstall(ctx, acme, "app_uninstalled"); err != nil {
			t.Fatalf("a second uninstall: %v", err)
		}
		unlinked(v)
	})
}

// In auth.mode none with the anonymous opt-in nobody links: each Slack user
// is the anonymous principal, labelled with who they are in Slack.
func TestSlackAnonymousPrincipal(t *testing.T) {
	s, _, err := newSlackServer(t, func(c *config.Config) { c.AnonymousPrincipal = "chat" })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, tc := range []struct {
		ws         slack.Workspace
		user, want string
	}{
		{acme, "U1", "chat/slack-T0ACME-U1"},
		{slack.Workspace{Enterprise: "E0ORG", Team: "T0ACME"}, "W1", "chat/slack-T0ACME-W1"},
		{slack.Workspace{Enterprise: "E0ORG"}, "W1", "chat/slack-E0ORG-W1"},
	} {
		c, prompt, err := s.slackCaller(ctx, tc.ws, tc.user, "C1")
		if err != nil || prompt != nil || c.Principal != tc.want || c.User != nil {
			t.Errorf("%v %s: %+v %+v %v", tc.ws, tc.user, c, prompt, err)
		}
		label := strings.TrimPrefix(c.Principal, "chat/")
		if !config.ValidName(label) {
			t.Errorf("label %q isn't valid", label)
		}
	}
	if _, _, err := s.slackCaller(ctx, acme, strings.Repeat("U", 64), "C1"); err == nil {
		t.Error("a label past the charset's length must be refused")
	}
	if r := s.slackDisconnect(ctx, slack.Command{Workspace: acme, User: "U1"}); !strings.Contains(r.Text, "nothing to disconnect") {
		t.Errorf("disconnect: %s", r.Text)
	}
	// With the connectors off, nobody is anyone.
	s.Cfg.AnonymousPrincipal = ""
	if _, prompt, _ := s.slackCaller(ctx, acme, "U1", "C1"); prompt == nil || !strings.Contains(prompt.Text, "anonymous_principal") {
		t.Errorf("no anonymous principal: %+v", prompt)
	}
}

// A link goes on to the forge only once whoever opened it proved, with
// Sign in with Slack in the same browser, that they are the Slack user it
// was made for: a link sent to someone else links nobody.
func TestSlackLinkProvesTheSlackUser(t *testing.T) {
	v := newSlackEnv(t)
	v.api.Users["U2"] = slack.User{ID: "U2", Name: "bo"}
	nothingLinked := func(t *testing.T) {
		t.Helper()
		if len(v.linkKeys()) != 0 {
			t.Fatalf("links stored: %v", v.linkKeys())
		}
		for _, k := range v.mr.Keys() {
			if strings.HasPrefix(k, "devbot:auth:grant:") || strings.HasPrefix(k, "devbot:auth:pending:") {
				t.Fatalf("nothing should reach the forge: %s", k)
			}
		}
		for _, u := range []string{"U1", "U2"} {
			if _, prompt := v.caller(u); prompt == nil {
				t.Fatalf("%s should be unlinked", u)
			}
		}
	}
	refused := func(t *testing.T, proved string) {
		t.Helper()
		recs := v.audit.Of("auth.link")
		if len(recs) == 0 {
			t.Fatal("a refused link should be audited")
		}
		last := recs[len(recs)-1]
		if d := audittest.Detail(last); last["outcome"] != "refused" || d["link"] != "slack::T0ACME:U1" || d["proved"] != proved {
			t.Fatalf("audit: %v", last)
		}
	}

	t.Run("the forge step needs the Slack step", func(t *testing.T) {
		u := v.connectURL(v.command("U1", ""))
		resp, _ := v.get(u)
		if loc := resp.Header.Get("Location"); resp.StatusCode != 302 || !strings.HasPrefix(loc, v.oidc.AuthorizeURL()+"?") ||
			!strings.Contains(loc, "team=T0ACME") || !strings.Contains(loc, "scope=openid") {
			t.Fatalf("/oauth/link should send the browser to Sign in with Slack: %d %s", resp.StatusCode, loc)
		}
		// Neither the link's own state nor the Slack step's is a forge
		// sign-in.
		state := regexp.MustCompile(`[?&]state=([^&]+)`).FindStringSubmatch(resp.Header.Get("Location"))[1]
		for _, s := range []string{strings.TrimPrefix(u, v.url+"/oauth/link?state="), state} {
			if resp, body := v.get(v.url + "/oauth/callback?code=x&state=" + s); resp.StatusCode != 400 || !strings.Contains(body, "already used or has expired") {
				t.Fatalf("the forge callback with %s: %d %s", s, resp.StatusCode, body)
			}
		}
		nothingLinked(t)
	})

	t.Run("another user in the same workspace", func(t *testing.T) {
		resp, body := v.proveSlack(v.connectURL(v.command("U1", "")), "T0ACME", "U2")
		if resp.StatusCode != 403 || !strings.Contains(body, "made for the Slack user Ann Smith @ann (U1)") || resp.Header.Get("Location") != "" {
			t.Fatalf("U2 opening U1's link: %d %s", resp.StatusCode, body)
		}
		refused(t, "slack::T0ACME:U2")
		nothingLinked(t)
	})

	t.Run("the same user ID in another workspace", func(t *testing.T) {
		resp, body := v.proveSlack(v.connectURL(v.command("U1", "")), "T0OTHER", "U1")
		if resp.StatusCode != 403 || !strings.Contains(body, "for someone else") {
			t.Fatalf("U1 of another workspace: %d %s", resp.StatusCode, body)
		}
		refused(t, "slack::T0OTHER:U1")
		nothingLinked(t)
	})

	t.Run("a replayed or missing state", func(t *testing.T) {
		resp, _ := v.get(v.connectURL(v.command("U1", "")))
		back, err := v.oidc.SignIn(resp.Header.Get("Location"), "", "T0ACME", "U1")
		if err != nil {
			t.Fatal(err)
		}
		if resp, _ := v.get(back); resp.StatusCode != 302 {
			t.Fatalf("first return: %d", resp.StatusCode)
		}
		if resp, body := v.get(back); resp.StatusCode != 400 || !strings.Contains(body, "already used or has expired") {
			t.Fatalf("a replayed return must be refused: %d %s", resp.StatusCode, body)
		}
		u, _ := url.Parse(back)
		u.RawQuery = url.Values{"code": {u.Query().Get("code")}}.Encode()
		if resp, body := v.get(u.String()); resp.StatusCode != 400 || !strings.Contains(body, "already used or has expired") {
			t.Fatalf("a return without a state must be refused: %d %s", resp.StatusCode, body)
		}
		// The forge sign-in that first return started is left to expire.
		v.advance(11 * time.Minute)
		nothingLinked(t)
	})

	t.Run("a wrong or missing nonce", func(t *testing.T) {
		t.Cleanup(func() { v.oidc.Claims = nil })
		for _, nonce := range []any{"someone-elses", nil} {
			v.oidc.Claims = func(c map[string]any) {
				if nonce == nil {
					delete(c, "nonce")
				} else {
					c["nonce"] = nonce
				}
			}
			resp, body := v.proveSlack(v.connectURL(v.command("U1", "")), "T0ACME", "U1")
			if resp.StatusCode != 502 || !strings.Contains(body, "confirm who you are, so nothing was linked") {
				t.Fatalf("nonce %v: %d %s", nonce, resp.StatusCode, body)
			}
			nothingLinked(t)
		}
	})

	t.Run("another browser", func(t *testing.T) {
		resp, _ := v.get(v.connectURL(v.command("U1", "")))
		back, _ := v.oidc.SignIn(resp.Header.Get("Location"), "", "T0ACME", "U1")
		req, _ := http.NewRequest("GET", back, nil)
		other, err := (&http.Client{CheckRedirect: v.hc.CheckRedirect}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		other.Body.Close()
		if other.StatusCode != 400 {
			t.Fatalf("a return to another browser must be refused: %d", other.StatusCode)
		}
		nothingLinked(t)
	})

	t.Run("declined at Slack", func(t *testing.T) {
		resp, _ := v.get(v.connectURL(v.command("U1", "")))
		loc, _ := url.Parse(resp.Header.Get("Location"))
		back := loc.Query().Get("redirect_uri") + "?" + url.Values{"error": {"access_denied"}, "state": {loc.Query().Get("state")}}.Encode()
		if resp, body := v.get(back); resp.StatusCode != 403 || !strings.Contains(body, "nothing was linked") {
			t.Fatalf("declined: %d %s", resp.StatusCode, body)
		}
		nothingLinked(t)
	})

	t.Run("the matching user links", func(t *testing.T) {
		consent, _ := v.link(v.connectURL(v.command("U1", "")), "U1", "alice", "allow")
		if !strings.Contains(consent, "You just confirmed, in this browser, that you are that user") {
			t.Fatalf("consent: %s", consent)
		}
		if c, _ := v.caller("U1"); c.Login != "alice" {
			t.Fatalf("U1 should be alice: %+v", c)
		}
		if recs := v.audit.Of("auth.link"); recs[len(recs)-1]["outcome"] != "ok" {
			t.Fatalf("audit: %v", recs[len(recs)-1])
		}
	})
}

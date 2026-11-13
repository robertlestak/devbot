package github

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// The payloads in testdata/webhooks are deliveries GitHub recorded, from
// octokit/webhooks' payload-examples (MIT). Their App is 29310.
const hookAppID = 29310

func hookClient(t *testing.T) *Client {
	c, err := newClient(Config{
		Name: "github", URL: "https://api.github.com",
		ImplementerAppID: hookAppID, ImplementerKey: pemKey(testKey(0)),
		ReviewerAppID: revAppID, ReviewerKey: pemKey(testKey(1)),
		WebhookSecret: "s3cret",
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// delivery is a recorded payload as the implementer App's webhook sends
// it, signed with the secret. edit, when set, changes the payload first.
func delivery(t *testing.T, event, file string, edit func(p map[string]any)) (*http.Request, []byte) {
	t.Helper()
	body, err := os.ReadFile("testdata/webhooks/" + file + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		var p map[string]any
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatal(err)
		}
		edit(p)
		body, _ = json.Marshal(p)
	}
	r := httptest.NewRequest("POST", "/webhooks/github", bytes.NewReader(body))
	r.Header.Set("X-GitHub-Event", event)
	r.Header.Set("X-GitHub-Delivery", "72d3162e-cc78-11e3-81ab-4c9367dc0958")
	r.Header.Set("X-GitHub-Hook-Installation-Target-Type", "integration")
	r.Header.Set("X-GitHub-Hook-Installation-Target-ID", "29310")
	r.Header.Set("X-Hub-Signature-256", sign("s3cret", body))
	return r, body
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func setAction(a string) func(map[string]any) {
	return func(p map[string]any) { p["action"] = a }
}

func setAppID(id int) func(map[string]any) {
	return func(p map[string]any) { p["installation"].(map[string]any)["app_id"] = id }
}

func TestParseWebhook(t *testing.T) {
	hello := forge.RepoRef{Provider: "github", Owner: "Codertocat", Name: "Hello-World"}
	item := func(n int) forge.Event {
		return forge.Event{Kind: forge.EventItem, Repo: hello, Number: n, Actor: "Codertocat"}
	}
	pr := func(n int) forge.Event {
		return forge.Event{Kind: forge.EventPR, Repo: hello, Number: n, Actor: "Codertocat"}
	}
	status := func(sha string) forge.Event {
		return forge.Event{Kind: forge.EventStatus, Repo: hello, SHA: sha, Actor: "Codertocat"}
	}
	ignored := forge.Event{Kind: forge.EventIgnored}
	repoChanged := func(full string) forge.Event {
		o, n, _ := strings.Cut(full, "/")
		return forge.Event{Kind: forge.EventDefaultBranch, Repo: forge.RepoRef{Provider: "github", Owner: o, Name: n}}
	}
	const head = "ec26c3e57ca3a959ca5aad62de7213c562f8c821"

	for _, tc := range []struct {
		name, event, file string
		edit              func(map[string]any)
		want              forge.Event
	}{
		{"issue opened", "issues", "issues.opened", nil, item(1)},
		{"issue edited", "issues", "issues.edited", nil, item(1)},
		{"issue closed", "issues", "issues.opened", setAction("closed"), item(1)},
		{"issue reopened", "issues", "issues.reopened", nil, item(1)},
		{"issue labeled", "issues", "issues.labeled", nil, item(1)},
		{"issue unlabeled", "issues", "issues.unlabeled", nil, item(1)},
		{"issue assigned", "issues", "issues.assigned", nil, ignored},
		{"issue comment", "issue_comment", "issue_comment.created", nil, item(1)},
		{"issue comment edited", "issue_comment", "issue_comment.created", setAction("edited"), item(1)},
		// The same delivery for a comment on a PR's conversation.
		{"PR comment", "issue_comment", "issue_comment.created", func(p map[string]any) {
			p["issue"].(map[string]any)["pull_request"] = map[string]any{"url": "https://api.github.com/repos/Codertocat/Hello-World/pulls/1", "merged_at": nil}
		}, pr(1)},
		{"PR opened", "pull_request", "pull_request.opened", nil, pr(2)},
		{"PR synchronize", "pull_request", "pull_request.synchronize", nil, pr(2)},
		{"PR edited", "pull_request", "pull_request.opened", setAction("edited"), pr(2)},
		{"PR closed", "pull_request", "pull_request.closed", nil, pr(2)},
		{"PR reopened", "pull_request", "pull_request.reopened", nil, pr(2)},
		{"PR ready for review", "pull_request", "pull_request.ready_for_review", nil, pr(2)},
		// devbot/ready on a PR hands it to devbot.
		{"PR labeled", "pull_request", "pull_request.labeled", nil, pr(2)},
		{"PR unlabeled", "pull_request", "pull_request.labeled", setAction("unlabeled"), ignored},
		{"review submitted", "pull_request_review", "pull_request_review.submitted", nil, pr(2)},
		{"review dismissed", "pull_request_review", "pull_request_review.dismissed", nil, pr(2)},
		{"check run", "check_run", "check_run.completed", nil, pr(2)},
		{"check run created", "check_run", "check_run.created", nil, pr(2)},
		// GitHub leaves pull_requests empty when the head is on a fork.
		{"check run from a fork", "check_run", "check_run.completed", func(p map[string]any) {
			p["check_run"].(map[string]any)["pull_requests"] = []any{}
		}, status(head)},
		{"check run on another repo's PR", "check_run", "check_run.completed", func(p map[string]any) {
			prs := p["check_run"].(map[string]any)["pull_requests"].([]any)
			prs[0].(map[string]any)["base"].(map[string]any)["repo"].(map[string]any)["id"] = 1
		}, status(head)},
		{"check suite", "check_suite", "check_suite.completed", nil, pr(2)},
		{"status", "status", "status", nil, status("6113728f27ae82c7b1a177c8d03f9e96e0adf246")},
		{"push to the default branch", "push", "push.with-new-branch", nil,
			forge.Event{Kind: forge.EventDefaultBranch, Repo: hello, Paths: []string{"README.md"}, Actor: "Codertocat"}},
		{"push of a tag", "push", "push", nil, forge.Event{Kind: forge.EventTag, Repo: hello, Actor: "Codertocat"}},
		{"push to another branch", "push", "push.with-new-branch", func(p map[string]any) { p["ref"] = "refs/heads/feature" }, ignored},
		{"installed", "installation", "installation.created", nil, withActor(repoChanged("Codertocat/Hello-World"), "Codertocat")},
		{"uninstalled", "installation", "installation.deleted", setAppID(hookAppID), withActor(repoChanged("octocat/Hello-World"), "octocat")},
		{"repo added", "installation_repositories", "installation_repositories.added", nil, withActor(repoChanged("Codertocat/Space"), "Codertocat")},
		{"repo removed", "installation_repositories", "installation_repositories.removed", setAppID(hookAppID), withActor(repoChanged("octocat/Hello-World"), "octocat")},
		{"another App's installation", "installation_repositories", "installation_repositories.removed", nil, ignored},
		{"ping", "ping", "ping.with-app_id", nil, ignored},
		{"unknown event", "star", "issues.opened", nil, ignored},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, body := delivery(t, tc.event, tc.file, tc.edit)
			evs, err := hookClient(t).ParseWebhook(r, body)
			if err != nil {
				t.Fatal(err)
			}
			tc.want.DeliveryID = "72d3162e-cc78-11e3-81ab-4c9367dc0958"
			if len(evs) != 1 || !sameEvent(evs[0], tc.want) {
				t.Errorf("got %+v\nwant %+v", evs, tc.want)
			}
		})
	}
}

func withActor(e forge.Event, actor string) forge.Event { e.Actor = actor; return e }

// sameEvent compares events; of an ignored one, only what makes it so.
func sameEvent(a, b forge.Event) bool {
	if a.Kind == forge.EventIgnored || b.Kind == forge.EventIgnored {
		return a.Kind == b.Kind && a.DeliveryID == b.DeliveryID
	}
	return a.DeliveryID == b.DeliveryID && a.Kind == b.Kind && a.Repo == b.Repo && a.Number == b.Number &&
		a.SHA == b.SHA && a.Actor == b.Actor && slices.Equal(a.Paths, b.Paths)
}

func TestParseWebhookRejectsBadSignature(t *testing.T) {
	c := hookClient(t)
	for name, sig := range map[string]string{
		"missing":       "",
		"not hex":       "sha256=zz",
		"no prefix":     sign("s3cret", nil)[len("sha256="):],
		"wrong secret":  "",
		"other payload": "",
	} {
		t.Run(name, func(t *testing.T) {
			r, body := delivery(t, "issues", "issues.opened", nil)
			switch name {
			case "wrong secret":
				sig = sign("guess", body)
			case "other payload":
				sig = sign("s3cret", append(body, ' '))
			}
			r.Header.Set("X-Hub-Signature-256", sig)
			if _, err := c.ParseWebhook(r, body); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	// The SHA-1 signature GitHub also sends isn't enough on its own.
	r, body := delivery(t, "issues", "issues.opened", nil)
	r.Header.Del("X-Hub-Signature-256")
	r.Header.Set("X-Hub-Signature", "sha1=0000")
	if _, err := c.ParseWebhook(r, body); err == nil {
		t.Fatal("accepted with only X-Hub-Signature")
	}
	// No secret configured: nothing is accepted.
	c.cfg.WebhookSecret = ""
	r, body = delivery(t, "issues", "issues.opened", nil)
	if _, err := c.ParseWebhook(r, body); err == nil {
		t.Fatal("accepted without a secret")
	}
}

// The reviewer App's webhook should be off. When it isn't, its deliveries,
// the same events again under other delivery IDs, are ignored.
func TestParseWebhookIgnoresOtherHooks(t *testing.T) {
	c := hookClient(t)
	for name, hdr := range map[string][2]string{
		"reviewer App":    {"integration", "202"},
		"repo webhook":    {"repository", "29310"},
		"another App":     {"integration", "1"},
		"implementer App": {"integration", "29310"},
	} {
		r, body := delivery(t, "issues", "issues.opened", nil)
		r.Header.Set("X-GitHub-Hook-Installation-Target-Type", hdr[0])
		r.Header.Set("X-GitHub-Hook-Installation-Target-ID", hdr[1])
		evs, err := c.ParseWebhook(r, body)
		if err != nil {
			t.Fatal(name, err)
		}
		if want := map[bool]forge.EventKind{true: forge.EventItem, false: forge.EventIgnored}[name == "implementer App"]; len(evs) != 1 || evs[0].Kind != want {
			t.Errorf("%s: %+v, want %s", name, evs, want)
		}
	}
}

// A check suite on a head two PRs share reaches both, each under an ID of
// its own (serve dedupes on it), the same on every redelivery.
func TestParseWebhookCheckOnTwoPRs(t *testing.T) {
	two := func(p map[string]any) {
		cs := p["check_suite"].(map[string]any)
		prs := cs["pull_requests"].([]any)
		other, _ := json.Marshal(prs[0])
		var cp map[string]any
		json.Unmarshal(other, &cp)
		cp["number"] = 7
		cs["pull_requests"] = append(prs, cp)
	}
	parse := func() []forge.Event {
		r, body := delivery(t, "check_suite", "check_suite.completed", two)
		evs, err := hookClient(t).ParseWebhook(r, body)
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}
	evs := parse()
	if len(evs) != 2 || evs[0].Number != 2 || evs[1].Number != 7 || evs[0].Kind != forge.EventPR || evs[0].DeliveryID == evs[1].DeliveryID {
		t.Fatalf("got %+v", evs)
	}
	if again := parse(); again[0].DeliveryID != evs[0].DeliveryID || again[1].DeliveryID != evs[1].DeliveryID {
		t.Errorf("redelivery IDs %q %q, first time %q %q", again[0].DeliveryID, again[1].DeliveryID, evs[0].DeliveryID, evs[1].DeliveryID)
	}
}

// Losing a repo makes the adapter forget its installation, so the next
// call asks GitHub again rather than use a token that no longer reaches it.
func TestParseWebhookForgetsRemovedRepos(t *testing.T) {
	c := hookClient(t)
	c.impl.installs["octocat/Hello-World"] = installation{ID: 2}
	c.impl.installs["octocat/Other"] = installation{ID: 2}
	r, body := delivery(t, "installation_repositories", "installation_repositories.removed", setAppID(hookAppID))
	if _, err := c.ParseWebhook(r, body); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.impl.installs["octocat/Hello-World"]; ok {
		t.Error("still remembers the removed repo's installation")
	}
	if _, ok := c.impl.installs["octocat/Other"]; !ok {
		t.Error("forgot a repo that wasn't removed")
	}
}

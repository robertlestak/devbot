package gitea

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// A burst of deliveries about issues and PRs devbot has nothing to do with
// costs no call to Gitea, not even the repo's config, as on GitHub. Each is
// handled twice, as a redelivery would be. A label of devbot's, a devbot
// branch or a command on the same delivery is read.
func TestUnconcernedDeliveriesCostNoCalls(t *testing.T) {
	v := newTimelineEnv(t)
	parser := New(Config{Name: "gitea", WebhookSecret: "s3cret"})
	handle := func(event, body string) {
		t.Helper()
		r := httptest.NewRequest("POST", "/webhooks/gitea", strings.NewReader(body))
		r.Header.Set("X-Gitea-Event", event)
		r.Header.Set("X-Gitea-Signature", sign(body, "s3cret"))
		evs, err := parser.ParseWebhook(r, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			if ev.Kind == forge.EventIgnored {
				t.Fatalf("%s: ignored", event)
			}
			if err := v.e.HandleEvent(context.Background(), ev); err != nil {
				t.Fatalf("%s: %v", event, err)
			}
		}
	}
	const repo = `"repository":{"full_name":"acme/app","default_branch":"main"},"sender":{"login":"bob"}`
	pull := func(labels, head string) string {
		return `{"action":"opened","number":9,"pull_request":{"number":9,"labels":[` + labels + `],"head":{"ref":"` + head + `"}},` + repo + `}`
	}
	issue := func(labels, comment string) string {
		return `{"action":"created","issue":{"number":1,"labels":[` + labels + `]},"comment":{"body":"` + comment + `"},` + repo + `}`
	}

	before := v.fx.Calls()
	for range 2 {
		handle("status", `{"sha":"abc",`+repo+`}`)
		handle("workflow_run", `{"workflow_run":{"head_sha":"def"},`+repo+`}`)
		handle("workflow_job", `{"workflow_job":{"head_sha":"def"},`+repo+`}`)
		handle("pull_request", pull(`{"name":"bug"}`, "feature"))
		handle("pull_request_approved", pull("", "feature"))
		handle("issues", `{"action":"label_updated","issue":{"number":1,"labels":[{"name":"bug"}]},`+repo+`}`)
		handle("issue_comment", issue(`{"name":"bug"}`, "Thanks"))
		handle("issue_comment", `{"action":"created","issue":{"number":9,"pull_request":{},"labels":[]},"comment":{"body":"LGTM"},`+repo+`}`)
	}
	if n := v.fx.Calls() - before; n != 0 {
		t.Fatalf("%d calls to Gitea, want none", n)
	}
	stats, err := v.e.Q.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := stats["webhook_events_skipped|status"] + stats["webhook_events_skipped|pr"] + stats["webhook_events_skipped|item"]; got != 16 {
		t.Errorf("counted %v skipped, want 16: %v", got, stats)
	}

	for _, tc := range []struct{ name, event, body string }{
		{"devbot/ready on an issue", "issues", `{"action":"label_updated","issue":{"number":1,"labels":[{"name":"devbot/ready"}]},` + repo + `}`},
		{"devbot/ready on a PR", "pull_request", pull(`{"name":"devbot/ready"}`, "feature")},
		{"a PR on a devbot branch", "pull_request", pull("", "devbot/7-fix")},
		{"a command", "issue_comment", issue("", "/rebase")},
		{"a PR named by number alone", "pull_request_comment", `{"number":9,` + repo + `}`},
	} {
		before := v.fx.Calls()
		handle(tc.event, tc.body)
		if v.fx.Calls() == before {
			t.Errorf("%s: not read", tc.name)
		}
	}
}

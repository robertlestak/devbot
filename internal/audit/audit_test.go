package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/audit"
	"git.shdw.tech/rob/devbot/internal/audit/audittest"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/fake"
)

func TestRecordSchema(t *testing.T) {
	r := audittest.New(t, "serve")
	r.Now = func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.FixedZone("x", 3600)) }
	ctx := audit.With(context.Background(), audit.Fields{Request: "req-1", Principal: "alice", Task: "fake:acme/app#1"})
	ctx = audit.With(ctx, audit.Fields{Job: "pod-1", Task: "fake:acme/app#2"})
	r.Log(ctx, audit.Record{Event: "forge.comment.create", Actor: "devbot-impl", ActorKind: "implementer",
		Fields: audit.Fields{PR: 7}, Detail: map[string]any{"bytes": 12}})
	r.Log(ctx, audit.Record{Event: "forge.pr.merge", Err: errors.New("boom")})

	recs := r.Records()
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d:\n%s", len(recs), r.Raw())
	}
	got := recs[0]
	want := map[string]any{
		"time": "2026-10-17T11:00:00Z", "log_type": "audit", "component": "serve", "event": "forge.comment.create",
		"outcome": "ok", "actor": "devbot-impl", "actor_kind": "implementer", "principal": "alice",
		"task": "fake:acme/app#2", "pr": float64(7), "job_id": "pod-1", "request_id": "req-1",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if audittest.Detail(got)["bytes"] != float64(12) {
		t.Errorf("detail = %v", got["detail"])
	}
	if recs[1]["outcome"] != "error" || recs[1]["error"] != "boom" {
		t.Errorf("a record with an error is outcome error: %v", recs[1])
	}
	// The fixed fields are there even when empty, so a schema can rely on them.
	for _, k := range []string{"time", "log_type", "component", "event", "outcome", "actor", "actor_kind"} {
		if _, ok := recs[1][k]; !ok {
			t.Errorf("record without %s: %v", k, recs[1])
		}
	}
	var nilLogger *audit.Logger
	nilLogger.Log(ctx, audit.Record{Event: "x"}) // must not panic
	nilLogger.Secret("whatever-secret")
}

// No credential reaches an audit line: neither a value the process loaded
// nor one of the shapes devbot knows credentials by, wherever in the record
// it turns up.
func TestRecordsNeverHoldSecrets(t *testing.T) {
	r := audittest.New(t, "harness")
	known := []string{
		"0123456789abcdef0123456789abcdef01234567",                                         // a Gitea token
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----", // an App key
		`webhook"secret\with-quotes`,
	}
	r.Secret(known...)
	shaped := []string{
		"ghs_16C7e42F292c6912E7710c838347Ae178B4a", // gitleaks:allow
		"github_pat_11ABCDEFG0123456789_abcdefghijklmnop",
		"sk-ant-oat01-abcdefghijklmnopqrstuvwxyz0123456789",
		"devbot_at_Zm9vYmFyYmF6cXV4cXV1eHF1dXhxdXV4",
		"devbot_rt_Zm9vYmFyYmF6cXV4cXV1eHF1dXhxdXV4",
		"AKIAIOSFODNN7EXAMPLE",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", // gitleaks:allow
		"hunter2hunter2", // as a password=
		"s3cr3tp4ss",     // in a URL
		"dGhpcyBpcyBhIGJhc2ljIGF1dGg=",
	}
	text := strings.Join(known, " ") + " " + strings.Join(shaped[:7], " ") +
		" password=" + shaped[7] + " https://bot:" + shaped[8] + "@gitea.example.com/x.git Authorization: Basic " + shaped[9]
	line, _ := json.Marshal(map[string]string{"type": "assistant", "text": text})
	r.Log(context.Background(), audit.Record{
		Event: "agent.transcript", Actor: text, Err: errors.New(text),
		Fields: audit.Fields{Origin: text, Request: text},
		Detail: map[string]any{"s": text, "list": []string{text}, "nested": map[string]any{"s": text}, "line": json.RawMessage(line)},
	})
	out := r.Raw()
	for _, s := range append(known, shaped...) {
		for _, form := range []string{s, strings.Trim(string(must(json.Marshal(s))), `"`)} {
			if strings.Contains(out, form) {
				t.Errorf("audit output holds %q:\n%s", s, out)
			}
		}
	}
	rec := r.Records()[0]
	if d := audittest.Detail(rec); d["line"] == nil {
		t.Fatalf("no line: %v", rec)
	} else if _, ok := d["line"].(map[string]any); !ok {
		t.Errorf("a transcript line stays JSON after redaction: %T", d["line"])
	}
	if !strings.Contains(out, "[REDACTED]") || !strings.Contains(out, "password=[REDACTED]") {
		t.Errorf("redactions should say so:\n%s", out)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

var repo = forge.RepoRef{Provider: "fake", Owner: "acme", Name: "app"}

// Every write the bots make is one record, as the bot that made it; reads
// aren't audited.
func TestForgeWritesAreAudited(t *testing.T) {
	ctx := context.Background()
	f := fake.New()
	f.AddRepo(repo, "")
	r := audittest.New(t, "serve")
	p := audit.Forge(f, r.Logger)
	impl, rev := f.BotUsers()

	it, err := p.CreateItem(ctx, repo, "a thing", "body", []string{"devbot/ready"})
	if err != nil {
		t.Fatal(err)
	}
	p.GetItem(ctx, it.Ref)
	p.ListComments(ctx, it.Ref)
	p.Comment(ctx, it.Ref, "hello\n\n<!-- devbot:status {\"kind\":\"started\"} -->")
	p.SetLabels(ctx, it.Ref, []string{"devbot/working"}, []string{"devbot/ready"})
	p.EnsureLabels(ctx, repo, []forge.Label{{Name: "devbot/ready"}})
	pr, _ := p.OpenPR(ctx, repo, "devbot/1-a-thing", "main", "A thing", "body")
	p.UpdatePR(ctx, repo, pr.Number, "A thing", "new body")
	p.SetPRBase(ctx, repo, pr.Number, "main")
	p.Review(ctx, repo, pr.Number, pr.HeadSHA, forge.VerdictApprove, "lgtm")
	p.Merge(ctx, repo, pr.Number, "not-the-head", "merge")
	p.Merge(ctx, repo, pr.Number, pr.HeadSHA, "merge")
	p.AttachToPR(ctx, repo, pr.Number, "shot.png", []byte("png"))
	p.ReleaseAttachments(ctx, repo, pr.Number)
	f.Push(repo, "devbot/2-old", "0ld")
	p.DeleteBranch(ctx, repo, "devbot/2-old")
	p.DeleteBranch(ctx, repo, "devbot/2-old")
	p.ListBranches(ctx, repo, "devbot/")
	f.Statuses[pr.HeadSHA] = []forge.CheckDetail{{Context: "build", State: "failure", Cancelled: true, URL: "run/1"}}
	p.RerunChecks(ctx, repo, pr.HeadSHA, f.Statuses[pr.HeadSHA])
	p.SetCommitStatus(ctx, repo, pr.HeadSHA, forge.CommitStatus{Context: "devbot/integration", State: "success"})
	p.CloseItem(ctx, it.Ref)

	want := []struct{ event, actor, outcome string }{
		{"forge.issue.create", impl, "ok"},
		{"forge.comment.create", impl, "ok"},
		{"forge.labels.set", impl, "ok"},
		{"forge.labels.ensure", impl, "ok"},
		{"forge.pr.open", impl, "ok"},
		{"forge.pr.update", impl, "ok"},
		{"forge.pr.retarget", impl, "ok"},
		{"forge.pr.review", rev, "ok"},
		{"forge.pr.merge", rev, "refused"},
		{"forge.pr.merge", rev, "ok"},
		{"forge.attachment.upload", impl, "ok"},
		{"forge.attachment.release", impl, "ok"},
		{"forge.branch.delete", impl, "ok"},
		{"forge.branch.delete", impl, "ok"},
		{"forge.checks.rerun", impl, "ok"},
		{"forge.status.set", impl, "ok"},
		{"forge.issue.close", impl, "ok"},
	}
	recs := r.Records()
	if len(recs) != len(want) {
		t.Fatalf("want %d records, got %v", len(want), r.Events())
	}
	for i, w := range want {
		if recs[i]["event"] != w.event || recs[i]["actor"] != w.actor || recs[i]["outcome"] != w.outcome {
			t.Errorf("record %d = %v %v %v, want %v", i, recs[i]["event"], recs[i]["actor"], recs[i]["outcome"], w)
		}
	}
	if recs[0]["task"] != it.Ref.String() || recs[0]["repo"] != repo.String() {
		t.Errorf("issue create names the issue: %v", recs[0])
	}
	if d := audittest.Detail(recs[1]); d["marker"] != "status" || strings.Contains(r.Raw(), "hello") {
		t.Errorf("a comment is recorded by size and marker, not its text: %v", d)
	}
	if recs[4]["pr"] != float64(pr.Number) {
		t.Errorf("pr open names the PR: %v", recs[4])
	}
	if d := audittest.Detail(recs[12]); d["branch"] != "devbot/2-old" || d["already_gone"] != nil || recs[12]["repo"] != repo.String() {
		t.Errorf("branch delete names the branch: %v", recs[12])
	}
	if d := audittest.Detail(recs[13]); d["already_gone"] != true || recs[13]["error"] != nil {
		t.Errorf("a branch already gone is ok, and says so: %v", recs[13])
	}

	// A person's writes, with their own token, are theirs.
	r.Reset()
	u := audit.User(f.AsUser("carol-token"), r.Logger, "carol")
	u.Comment(ctx, it.Ref, "from carol")
	if recs := r.Records(); len(recs) != 1 || recs[0]["actor"] != "carol" || recs[0]["actor_kind"] != "user" {
		t.Fatalf("a person's comment is audited as theirs: %v", recs)
	}
}

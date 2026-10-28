package gitea

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"git.shdw.tech/rob/devbot/internal/changeset"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/forgetest"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// Gitea's timeline sends each page's length as X-Total-Count: every page is
// still read, and the newest event is among them.
func TestLabelEventsReadEveryTimelinePage(t *testing.T) {
	const n = 120
	for _, maxItems := range []int{50, 20} {
		f := newFakeGitea(t)
		f.maxItems = maxItems
		fx := f.fixture(t)
		issue := fx.Issue("alice", "t", "")
		f.mu.Lock()
		bug, _ := f.label(f.labelID("bug"))
		for i := range n {
			who := "devbot"
			if i == n-1 {
				who = "carol"
			}
			f.issues[issue].timeline = append(f.issues[issue].timeline, labelEvent{Type: "label", Body: "1", User: user{Login: who}, Label: bug, CreatedAt: now()})
		}
		f.mu.Unlock()
		evs, err := fx.P.LabelEvents(context.Background(), forge.ItemRef{Repo: fx.Repo, Number: issue})
		if err != nil || len(evs) != n || evs[n-1].Actor != "carol" {
			t.Fatalf("MAX_RESPONSE_ITEMS %d: got %d events (%v), want %d ending with carol's", maxItems, len(evs), err, n)
		}
	}
}

// Gitea drops code comments from a timeline page after cutting it, so a
// middle page can be short: the pages after it are still read, and the
// newest devbot/ready add on the last one is found.
func TestLabelEventsReadPastAShortMiddlePage(t *testing.T) {
	f := newFakeGitea(t)
	fx := f.fixture(t)
	issue := fx.Issue("alice", "t", "")
	f.mu.Lock()
	bug, _ := f.label(f.labelID("bug"))
	ready, _ := f.label(f.labelID("devbot/ready"))
	is := f.issues[issue]
	for i := range 130 {
		e := labelEvent{Type: "label", Body: "1", User: user{Login: "devbot"}, Label: bug, CreatedAt: now()}
		switch {
		case i >= 10 && i < 20, i >= 60 && i < 100:
			e = labelEvent{Type: "code", User: user{Login: "bob"}, CreatedAt: now()}
		case i == 129:
			e = labelEvent{Type: "label", Body: "1", User: user{Login: "carol"}, Label: ready, CreatedAt: now()}
		}
		is.timeline = append(is.timeline, e)
	}
	f.mu.Unlock()
	evs, err := fx.P.LabelEvents(context.Background(), forge.ItemRef{Repo: fx.Repo, Number: issue})
	if err != nil || len(evs) != 80 || evs[79].Actor != "carol" || evs[79].Label != "devbot/ready" {
		t.Fatalf("got %d events (%v), want 80 ending with carol's devbot/ready add", len(evs), err)
	}
}

// timelineEnv is the engine running against the fake Gitea, for the paths
// that read an issue's timeline.
type timelineEnv struct {
	f   *fakeGitea
	fx  *forgetest.Fixture
	e   *core.Engine
	ref forge.ItemRef
}

func newTimelineEnv(t *testing.T) *timelineEnv {
	f := newFakeGitea(t)
	fx := f.fixture(t)
	fx.File("main", ".devbot.yaml", []byte("version: 1\nenabled: true\nmax_attempts: 3\n"))
	fx.Grant("alice", forge.PermWrite)
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	reg := forge.Registry{"gitea": fx.P}
	e := &core.Engine{Forges: reg, Q: queue.New(miniredis.RunT(t).Addr(), 0), Cfg: cfg, Repos: &config.RepoCache{Forges: reg},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return &timelineEnv{f: f, fx: fx, e: e, ref: forge.ItemRef{Repo: fx.Repo, Number: fx.Issue("alice", "t", "")}}
}

// label records who adding (or removing) name at, and puts the issue's
// labels to match.
func (v *timelineEnv) label(who, name string, add bool, at time.Time) {
	v.f.mu.Lock()
	defer v.f.mu.Unlock()
	is := v.f.issues[v.ref.Number]
	id := v.f.labelID(name)
	l, _ := v.f.label(id)
	body := ""
	if add {
		body = "1"
		is.labels = append(is.labels, id)
	} else {
		is.labels = slices.DeleteFunc(is.labels, func(x int64) bool { return x == id })
	}
	is.timeline = append(is.timeline, labelEvent{Type: "label", Body: body, User: user{Login: who}, Label: l, CreatedAt: at})
}

// churn adds n label events that change nothing devbot reads, from at on.
func (v *timelineEnv) churn(n int, at time.Time) {
	for i := range n {
		v.label("devbot", "bug", i%2 == 0, at.Add(time.Duration(i)*time.Second))
	}
}

func (v *timelineEnv) comment(body string, at time.Time) {
	v.f.mu.Lock()
	defer v.f.mu.Unlock()
	is := v.f.issues[v.ref.Number]
	is.comments = append(is.comments, comment{ID: v.f.id(), Body: body, User: user{Login: "devbot"}, CreatedAt: at})
}

func (v *timelineEnv) eval(t *testing.T) bool {
	t.Helper()
	acted, err := v.e.EvaluateItem(context.Background(), v.ref, core.SrcWebhook)
	if err != nil {
		t.Fatal(err)
	}
	return acted
}

func (v *timelineEnv) comments(t *testing.T) []forge.Comment {
	t.Helper()
	cs, err := v.fx.P.ListComments(context.Background(), v.ref)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// A task that gave up after more than a page of timeline events, retried by
// a person through the connector or by relabelling it in Gitea, starts over
// at attempt 1.
func TestRetryPastTheFirstTimelinePage(t *testing.T) {
	for _, via := range []string{"connector", "relabel"} {
		t.Run(via, func(t *testing.T) {
			v := newTimelineEnv(t)
			start := now().Add(-time.Hour)
			v.label("alice", core.LReady, true, start)
			v.churn(60, start.Add(time.Second))
			v.label("devbot", core.LReady, false, start.Add(2*time.Minute))
			for i := range 3 {
				v.comment("failed "+core.RenderStatus(core.Status{Kind: "failed", Role: "implement", Attempt: i + 1}), start.Add(2*time.Minute))
			}
			v.comment("stopped "+core.RenderStatus(core.Status{Kind: "needs-human"}), start.Add(2*time.Minute))
			v.label("devbot", core.LNeedsHuman, true, start.Add(2*time.Minute))

			if via == "connector" {
				alice := core.Caller{Principal: "alice", User: v.fx.P.(*Client).AsUser("alice-tok"), Provider: "gitea", Login: "alice"}
				if _, err := v.e.Retry(context.Background(), v.ref.String(), alice); err != nil {
					t.Fatal(err)
				}
			} else {
				v.label("alice", core.LNeedsHuman, false, now())
				v.label("alice", core.LReady, true, now())
			}
			v.eval(t)
			cs := v.comments(t)
			if !strings.Contains(cs[len(cs)-1].Body, "Retried by @alice. Attempts and review rounds start over.") {
				t.Fatalf("want the reset comment, last comment is %q", cs[len(cs)-1].Body)
			}
			if a := core.FailedAttempts(core.Statuses(cs, "devbot", "devbot-review")) + 1; a != 1 {
				t.Fatalf("the next implement run is attempt %d, want 1", a)
			}
			if q, _ := v.e.Q.IsQueued(context.Background(), queue.KindImplement, v.ref.String()); !q {
				t.Fatal("the retried task should be queued")
			}
			if v.eval(t) || len(v.comments(t)) != len(cs) {
				t.Fatal("second evaluation must be a no-op")
			}
		})
	}
}

// ready() holds the newest devbot/ready add to account, past the first page
// of the timeline, whoever added it before.
func TestReadyAuthorizesTheNewestAddPastTheFirstPage(t *testing.T) {
	for _, c := range []struct{ first, last string }{{"alice", "mallory"}, {"mallory", "alice"}} {
		t.Run(c.last, func(t *testing.T) {
			v := newTimelineEnv(t)
			start := now().Add(-time.Hour)
			v.label(c.first, core.LReady, true, start)
			v.churn(60, start.Add(time.Second))
			v.label(c.first, core.LReady, false, start.Add(2*time.Minute))
			v.label(c.last, core.LReady, true, start.Add(3*time.Minute))
			v.eval(t)
			cs := v.comments(t)
			q, _ := v.e.Q.IsQueued(context.Background(), queue.KindImplement, v.ref.String())
			if c.last == "alice" {
				if !q || len(cs) != 0 {
					t.Fatalf("alice's newest add should be queued without comment: queued %v, %d comments", q, len(cs))
				}
			} else if q || len(cs) != 1 || !strings.Contains(cs[0].Body, "applied by @mallory") {
				t.Fatalf("mallory's newest add should be ignored: queued %v, comments %+v", q, cs)
			}
			if v.eval(t) || len(v.comments(t)) != len(cs) {
				t.Fatal("second evaluation must be a no-op")
			}
		})
	}
}

// Gitea's issue comments ignore page and limit and serve every comment on
// every page, with the whole count as X-Total-Count: each comment is read
// once, and each status counted once, whether there are fewer comments than
// a page, exactly a page, or more.
func TestCommentsIgnoringPageAreReadOnce(t *testing.T) {
	for _, n := range []int{8, 50, 120} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			v := newTimelineEnv(t)
			at := now().Add(-time.Hour)
			v.comment("failed "+core.RenderStatus(core.Status{Kind: "failed", Role: "implement", Attempt: 1}), at)
			v.comment("opened "+core.RenderStatus(core.Status{Kind: "pr", PR: 2}), at)
			v.comment("fix "+core.RenderStatus(core.Status{Kind: "fix-requested", PR: 2, Mode: "review"}), at)
			for range n - 3 {
				v.comment("hi", at)
			}
			cs := v.comments(t)
			ids := map[int64]bool{}
			for _, c := range cs {
				ids[c.ID] = true
			}
			if len(cs) != n || len(ids) != n {
				t.Fatalf("got %d comments (%d distinct), want %d", len(cs), len(ids), n)
			}
			ss := core.Statuses(cs, "devbot", "devbot-review")
			if a, r := core.FailedAttempts(ss), core.ReviewRound(ss); a != 1 || r != 2 {
				t.Fatalf("got %d failed attempts and review round %d, want 1 and 2", a, r)
			}
		})
	}
}

// An approved PR rebased once (one approved, one rebase
// fix-requested, one pr status) is approved again on its new head, and a
// rebase asked for after that is its second round, not past
// max_review_rounds.
func TestOneRebaseRoundIsCountedOnce(t *testing.T) {
	v := newTimelineEnv(t)
	v.e.Changes = func(context.Context, forge.Provider, forge.PR) ([]changeset.Change, error) {
		return []changeset.Change{{Status: "M", Path: "main.go", OldMode: "100644", NewMode: "100644"}}, nil
	}
	at := now().Add(-time.Hour)
	branch := fmt.Sprintf("devbot/%d-t", v.ref.Number)
	pr := v.fx.PR(forgetest.PRSeed{Author: "devbot", Title: "t", Body: core.RenderPRMeta(core.PRMeta{Issue: v.ref.Number}),
		Head: branch, HeadSHA: "sha2", Base: "main", MergeBase: "ba5e"})
	v.fx.Branch(branch, "sha2")
	v.label("devbot", core.LInReview, true, at)
	v.comment("opened "+core.RenderStatus(core.Status{Kind: "pr", PR: pr, Base: "main"}), at)
	v.comment("approved "+core.RenderStatus(core.Status{Kind: "approved", PR: pr, SHA: "sha1"}), at)
	v.comment("rebase "+core.RenderStatus(core.Status{Kind: "fix-requested", PR: pr, SHA: "sha1", Mode: "rebase"}), at)
	v.comment("pushed "+core.RenderStatus(core.Status{Kind: "pr", PR: pr}), at)
	v.fx.Status("sha2", "ci", "success")
	v.f.mu.Lock()
	v.f.issues[pr].reviews = append(v.f.issues[pr].reviews, fakeReview{user: "devbot-review", state: "APPROVED", sha: "sha2", at: now()})
	v.f.mu.Unlock()

	labels := func() []string {
		t.Helper()
		it, err := v.fx.P.GetItem(context.Background(), v.ref)
		if err != nil {
			t.Fatal(err)
		}
		return it.Labels
	}
	last := func() string { cs := v.comments(t); return cs[len(cs)-1].Body }
	for i := range 2 {
		v.eval(t)
		if l := labels(); !slices.Contains(l, core.LApproved) {
			t.Fatalf("evaluation %d: want approved, labels %v, last comment %q", i+1, l, last())
		}
	}

	// main moves on, and alice asks for a rebase.
	v.fx.Branch("main", "ba5f")
	v.label("alice", core.LRebase, true, now())
	v.eval(t)
	if l := labels(); slices.Contains(l, core.LNeedsHuman) || !slices.Contains(l, core.LChanges) {
		t.Fatalf("want the rebase sent back, labels %v, last comment %q", l, last())
	}
	if c := last(); !strings.Contains(c, "1 of 3 review rounds used") {
		t.Fatalf("the rebase should count one round used: %q", c)
	}
	if n := len(v.comments(t)); v.eval(t) || len(v.comments(t)) != n {
		t.Fatal("second evaluation must be a no-op")
	}
}

// A push to the default branch longer than Gitea lists in its webhook may
// change .devbot.yaml in a commit it leaves out: the cached config is
// dropped. A push listed whole that doesn't touch it keeps the cache.
func TestLongPushDropsTheCachedConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		total   int
		dropped bool
	}{{"listed whole", 5, false}, {"longer than listed", 7, true}} {
		t.Run(tc.name, func(t *testing.T) {
			v := newTimelineEnv(t)
			ctx := context.Background()
			if rc, err := v.e.Repos.Get(ctx, v.fx.Repo); err != nil || rc.Automerge {
				t.Fatalf("before: %+v %v", rc, err)
			}
			v.fx.File("main", ".devbot.yaml", []byte("version: 1\nenabled: true\nautomerge: true\n"))
			body := fmt.Sprintf(`{"ref":"refs/heads/main","total_commits":%d,"commits":[{"modified":["a"]},{"modified":["b"]},{"modified":["c"]},{"modified":["d"]},{"modified":["e"]}],"repository":{"full_name":"acme/app","default_branch":"main"},"sender":{"login":"alice"}}`, tc.total)
			r := httptest.NewRequest("POST", "/webhooks/gitea", strings.NewReader(body))
			r.Header.Set("X-Gitea-Event", "push")
			r.Header.Set("X-Gitea-Signature", sign(body, "s3cret"))
			evs, err := New(Config{Name: "gitea", WebhookSecret: "s3cret"}).ParseWebhook(r, []byte(body))
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				for _, ev := range evs {
					if err := v.e.HandleEvent(ctx, ev); err != nil {
						t.Fatal(err)
					}
				}
				rc, err := v.e.Repos.Get(ctx, v.fx.Repo)
				if err != nil {
					t.Fatal(err)
				}
				if rc.Automerge != tc.dropped {
					t.Fatalf("after the push: automerge %v, want %v", rc.Automerge, tc.dropped)
				}
			}
		})
	}
}

// Anyone with write access can edit a comment of devbot's, and Gitea says
// when one was: an approval or a hold edited into one counts for nothing.
func TestEditedBotCommentsApprovalIsIgnored(t *testing.T) {
	f := newFakeGitea(t)
	fx := f.fixture(t)
	issue := fx.Issue("alice", "t", "")
	files := []core.HeldFile{{Path: ".gitea/workflows/ci.yml", Mode: "100644", Blob: "1111111111111111111111111111111111111111"}}
	hold := "Holding.\n\n" + core.RenderStatus(core.Status{Kind: "held", SHA: "aaaa111", Files: files})
	approval := "Approved.\n\n" + core.RenderStatus(core.Status{Kind: "approval", SHA: "aaaa111", Principal: "alice", Files: files})
	at := now()
	f.mu.Lock()
	f.issues[issue].comments = append(f.issues[issue].comments,
		comment{ID: f.id(), Body: hold, User: user{Login: "devbot"}, CreatedAt: at, UpdatedAt: at},
		comment{ID: f.id(), Body: approval, User: user{Login: "devbot"}, CreatedAt: at, UpdatedAt: at.Add(time.Minute)})
	f.mu.Unlock()
	cs, err := fx.P.ListComments(context.Background(), forge.ItemRef{Repo: fx.Repo, Number: issue})
	if err != nil || len(cs) != 2 || cs[0].Edited() || !cs[1].Edited() {
		t.Fatalf("got %+v, %v", cs, err)
	}
	ss := core.Statuses(cs, "devbot")
	if _, ok := core.LastHold(ss); !ok || core.Approved(ss, files) {
		t.Fatalf("the hold counts and the edited approval doesn't: %+v", ss)
	}
}

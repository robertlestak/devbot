package gitea

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
)

func sign(body, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func TestParseWebhook(t *testing.T) {
	c := New(Config{Name: "gitea", URL: "https://git.example", WebhookSecret: "s3cret"})
	cases := []struct {
		event, body string
		want        forge.Event
	}{
		{"issues", `{"action":"label_updated","issue":{"number":12},"repository":{"full_name":"acme/app","default_branch":"main"},"sender":{"login":"alice"}}`,
			forge.Event{Kind: forge.EventItem, Number: 12, Actor: "alice"}},
		{"issue_comment", `{"issue":{"number":7,"pull_request":{}},"repository":{"full_name":"acme/app"},"sender":{"login":"alice"}}`,
			forge.Event{Kind: forge.EventPR, Number: 7, Actor: "alice"}},
		{"pull_request", `{"number":3,"pull_request":{"number":3},"repository":{"full_name":"acme/app"},"sender":{"login":"devbot"}}`,
			forge.Event{Kind: forge.EventPR, Number: 3, Actor: "devbot"}},
		// devbot/ready on a PR hands it to devbot.
		{"pull_request_label", `{"action":"label_updated","number":5,"pull_request":{"number":5,"labels":[{"name":"devbot/ready"}]},"repository":{"full_name":"acme/app"},"sender":{"login":"alice"}}`,
			forge.Event{Kind: forge.EventPR, Number: 5, Actor: "alice"}},
		{"status", `{"sha":"abc","repository":{"full_name":"acme/app"},"sender":{"login":"ci"}}`,
			forge.Event{Kind: forge.EventStatus, SHA: "abc", Actor: "ci"}},
		{"workflow_run", `{"workflow_run":{"head_sha":"def"},"repository":{"full_name":"acme/app"},"sender":{"login":"ci"}}`,
			forge.Event{Kind: forge.EventStatus, SHA: "def", Actor: "ci"}},
		{"push", `{"ref":"refs/heads/main","commits":[{"modified":[".devbot.yaml"]}],"total_commits":1,"repository":{"full_name":"acme/app","default_branch":"main"},"sender":{"login":"alice"}}`,
			forge.Event{Kind: forge.EventDefaultBranch, Paths: []string{".devbot.yaml"}, Actor: "alice"}},
		// Gitea lists the first 5 commits of 7: the paths aren't known.
		{"push", `{"ref":"refs/heads/main","commits":[{"modified":["a"]},{"modified":["b"]},{"modified":["c"]},{"modified":["d"]},{"added":["e"]}],"total_commits":7,"repository":{"full_name":"acme/app","default_branch":"main"},"sender":{"login":"alice"}}`,
			forge.Event{Kind: forge.EventDefaultBranch, Actor: "alice"}},
		// A tag, pushed or created, may release a wait on it.
		{"push", `{"ref":"refs/tags/v1.4.0","commits":[],"total_commits":0,"repository":{"full_name":"acme/app","default_branch":"main"},"sender":{"login":"alice"}}`,
			forge.Event{Kind: forge.EventTag, Actor: "alice"}},
		{"create", `{"ref":"v1.4.0","ref_type":"tag","repository":{"full_name":"acme/app","default_branch":"main"},"sender":{"login":"alice"}}`,
			forge.Event{Kind: forge.EventTag, Actor: "alice"}},
		{"create", `{"ref":"feature","ref_type":"branch","repository":{"full_name":"acme/app","default_branch":"main"},"sender":{"login":"alice"}}`,
			forge.Event{Kind: forge.EventIgnored, Actor: "alice"}},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("POST", "/webhooks/gitea", strings.NewReader(tc.body))
		r.Header.Set("X-Gitea-Event", tc.event)
		r.Header.Set("X-Gitea-Delivery", "d1")
		r.Header.Set("X-Gitea-Signature", sign(tc.body, "s3cret"))
		evs, err := c.ParseWebhook(r, []byte(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.event, err)
		}
		got := evs[0]
		if got.Kind != tc.want.Kind || got.Number != tc.want.Number || got.SHA != tc.want.SHA || got.Actor != tc.want.Actor ||
			got.Repo.String() != "gitea:acme/app" || got.DeliveryID != "d1" || strings.Join(got.Paths, ",") != strings.Join(tc.want.Paths, ",") {
			t.Errorf("%s: got %+v", tc.event, got)
		}
	}
}

func TestParseWebhookRejectsBadSignature(t *testing.T) {
	c := New(Config{Name: "gitea", WebhookSecret: "s3cret"})
	body := `{"issue":{"number":1},"repository":{"full_name":"acme/app"}}`
	for _, sig := range []string{"", "zz", sign(body, "wrong")} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.Header.Set("X-Gitea-Event", "issues")
		r.Header.Set("X-Gitea-Signature", sig)
		if _, err := c.ParseWebhook(r, []byte(body)); err == nil {
			t.Errorf("signature %q accepted", sig)
		}
	}
}

func TestIssueDecodesShortRepository(t *testing.T) {
	var is issue
	body := `{"number":1,"title":"t","state":"open","labels":[{"id":3,"name":"devbot/ready"}],"user":{"login":"devbot"},"repository":{"id":9,"name":"app","owner":"alice","full_name":"acme/app"}}`
	if err := json.Unmarshal([]byte(body), &is); err != nil {
		t.Fatal(err)
	}
	if is.Repository.FullName != "acme/app" || is.Labels[0].Name != "devbot/ready" {
		t.Fatalf("got %+v", is)
	}
}

func TestAuthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/acme/app/collaborators/"), "/permission")
		switch user {
		case "nobody":
			http.NotFound(w, r)
		case "hidden":
			http.Error(w, `{"message":"forbidden"}`, 403)
		default:
			json.NewEncoder(w).Encode(map[string]string{"permission": user}) // the login is the role
		}
	}))
	defer srv.Close()
	c := New(Config{Name: "gitea", URL: srv.URL, ImplementerUser: "devbot", ImplementerTok: "impl-tok"})
	r := forge.RepoRef{Provider: "gitea", Owner: "acme", Name: "app"}
	cases := []struct {
		actor string
		min   forge.Permission
		want  bool
	}{
		{"write", forge.PermWrite, true},
		{"admin", forge.PermWrite, true},
		{"owner", forge.PermAdmin, true},
		{"read", forge.PermWrite, false},
		{"none", forge.PermWrite, false},
		{"write", forge.PermMaintain, true}, // Gitea has no maintain
		{"write", forge.PermAdmin, false},
		{"admin", forge.PermAdmin, true},
		{"nobody", forge.PermWrite, false},
		{"acme", forge.PermAdmin, true}, // the repo's owner
	}
	for _, tc := range cases {
		got, err := c.Authorized(context.Background(), r, tc.actor, tc.min)
		if err != nil || got != tc.want {
			t.Errorf("%s at %s: got %v, %v; want %v", tc.actor, tc.min, got, err, tc.want)
		}
	}
	ok, err := c.Authorized(context.Background(), r, "hidden", forge.PermWrite)
	var fe *forge.LookupForbiddenError
	if ok || !errors.As(err, &fe) || !strings.Contains(fe.Need, "admin on the repo") {
		t.Fatalf("a 403 must fail closed with a LookupForbiddenError, got %v, %v", ok, err)
	}
}

// labelServer is one issue's labels on a Gitea that records every call and
// its label timeline, and can fail the replace or let someone edit the
// labels just after devbot reads them.
type labelServer struct {
	labels  []label
	calls   []string
	failPut bool
	events  []labelEvent
	// afterRead runs once the issue's labels have been served.
	afterRead func(ls *labelServer)
}

type labelEvent struct {
	ID        int64     `json:"id,omitempty"`
	Type      string    `json:"type"`
	Body      string    `json:"body"`
	User      user      `json:"user"`
	Label     label     `json:"label"`
	CreatedAt time.Time `json:"created_at"`
	OldTitle  string    `json:"old_title,omitempty"`
	NewTitle  string    `json:"new_title,omitempty"`
}

var allLabels = []label{{ID: 1, Name: "devbot/ready"}, {ID: 2, Name: "devbot/working"}, {ID: 3, Name: "devbot/needs-human"}, {ID: 4, Name: "bug"}, {ID: 5, Name: "priority"}}

// set changes the labels as who, recording the timeline as Gitea does.
func (ls *labelServer) set(who string, ids []int64) {
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var next []label
	for _, l := range ls.labels {
		if want[l.ID] {
			next = append(next, l)
			delete(want, l.ID)
		} else {
			ls.events = append(ls.events, labelEvent{Type: "label", User: user{Login: who}, Label: l, CreatedAt: time.Now()})
		}
	}
	for _, l := range allLabels {
		if want[l.ID] {
			next = append(next, l)
			ls.events = append(ls.events, labelEvent{Type: "label", Body: "1", User: user{Login: who}, Label: l, CreatedAt: time.Now()})
		}
	}
	ls.labels = next
}

func (ls *labelServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ls.calls = append(ls.calls, r.Method+" "+r.URL.Path)
	var in struct{ Labels []int64 }
	json.NewDecoder(r.Body).Decode(&in)
	name, byName := strings.CutPrefix(r.URL.Path, "/api/v1/repos/acme/app/labels/")
	switch {
	case r.Method == "GET" && byName:
		i := slices.IndexFunc(allLabels, func(l label) bool { return l.Name == name })
		if i < 0 {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(allLabels[i])
	case r.Method == "GET" && r.URL.Path == "/api/v1/repos/acme/app/issues/5/labels":
		json.NewEncoder(w).Encode(ls.labels)
		if ls.afterRead != nil {
			ls.afterRead(ls)
			ls.afterRead = nil
		}
	case r.Method == "GET" && r.URL.Path == "/api/v1/repos/acme/app/issues/5/timeline":
		since, _ := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
		out := []labelEvent{}
		for _, e := range ls.events {
			if !e.CreatedAt.Truncate(time.Second).Before(since) {
				out = append(out, e)
			}
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == "PUT" && r.URL.Path == "/api/v1/repos/acme/app/issues/5/labels" && !ls.failPut:
		ls.set("devbot", in.Labels)
		json.NewEncoder(w).Encode(ls.labels)
	case r.Method == "POST" && r.URL.Path == "/api/v1/repos/acme/app/issues/5/labels":
		ids := in.Labels
		for _, l := range ls.labels {
			ids = append(ids, l.ID)
		}
		ls.set("devbot", ids)
		json.NewEncoder(w).Encode(ls.labels)
	default:
		http.Error(w, `{"message":"boom"}`, 500)
	}
}

func (ls *labelServer) names() string {
	var n []string
	for _, l := range ls.labels {
		n = append(n, l.Name)
	}
	return strings.Join(n, ",")
}

// A state change is one replace of the whole set: if it fails, the issue
// keeps the labels it had, never a mix of old and new.
func TestSetLabelsIsOneReplace(t *testing.T) {
	ls := &labelServer{labels: []label{{ID: 2, Name: "devbot/working"}, {ID: 4, Name: "bug"}}}
	srv := httptest.NewServer(ls)
	defer srv.Close()
	c := New(Config{Name: "gitea", URL: srv.URL, ImplementerTok: "impl-tok"})
	ref := forge.ItemRef{Repo: forge.RepoRef{Provider: "gitea", Owner: "acme", Name: "app"}, Number: 5}
	ctx := context.Background()

	ls.failPut = true
	if err := c.SetLabels(ctx, ref, []string{"devbot/needs-human"}, []string{"devbot/working"}); err == nil {
		t.Fatal("a failed replace must be an error")
	}
	if got := ls.names(); got != "devbot/working,bug" {
		t.Fatalf("a failed change must leave the labels as they were, got %s", got)
	}
	for _, call := range ls.calls {
		if strings.HasPrefix(call, "POST") || strings.HasPrefix(call, "DELETE") {
			t.Fatalf("labels must change in one PUT, not %s", call)
		}
	}

	ls.failPut, ls.calls = false, nil
	if err := c.SetLabels(ctx, ref, []string{"devbot/needs-human"}, []string{"devbot/working", "devbot/ready"}); err != nil {
		t.Fatal(err)
	}
	if got := ls.names(); got != "bug,devbot/needs-human" {
		t.Fatalf("labels after the change: %s", got)
	}
	puts := 0
	for _, call := range ls.calls {
		if strings.HasPrefix(call, "PUT") {
			puts++
		}
	}
	if puts != 1 {
		t.Fatalf("want one PUT, got %v", ls.calls)
	}

	// Already so: nothing is written.
	ls.calls = nil
	if err := c.SetLabels(ctx, ref, []string{"devbot/needs-human"}, []string{"devbot/working"}); err != nil {
		t.Fatal(err)
	}
	if len(ls.calls) != 1 || !strings.HasPrefix(ls.calls[0], "GET") {
		t.Fatalf("a change already made should only read: %v", ls.calls)
	}
}

// Gitea can't replace labels conditionally, so a label someone adds
// between devbot's read and its replace is dropped by the replace; devbot
// sees that in the timeline and adds it back. What devbot removed before
// its read stays removed.
func TestSetLabelsKeepsAConcurrentAdd(t *testing.T) {
	ls := &labelServer{}
	srv := httptest.NewServer(ls)
	defer srv.Close()
	c := New(Config{Name: "gitea", URL: srv.URL, ImplementerTok: "impl-tok"})
	ref := forge.ItemRef{Repo: forge.RepoRef{Provider: "gitea", Owner: "acme", Name: "app"}, Number: 5}
	ctx := context.Background()
	count := func(method string) int {
		n := 0
		for _, call := range ls.calls {
			if strings.HasPrefix(call, method+" ") {
				n++
			}
		}
		return n
	}

	// An hour ago someone added priority and devbot's replace removed it.
	old := time.Now().Add(-time.Hour)
	ls.labels = []label{allLabels[0], allLabels[3]}
	ls.events = []labelEvent{
		{Type: "label", Body: "1", User: user{Login: "alice"}, Label: allLabels[4], CreatedAt: old},
		{Type: "label", User: user{Login: "devbot"}, Label: allLabels[4], CreatedAt: old},
	}
	ls.afterRead = func(ls *labelServer) { ls.set("alice", []int64{1, 4, 5}) } // alice adds priority
	if err := c.SetLabels(ctx, ref, []string{"devbot/working"}, []string{"devbot/ready"}); err != nil {
		t.Fatal(err)
	}
	if got := ls.names(); got != "bug,devbot/working,priority" {
		t.Fatalf("alice's label must survive the replace, got %s", got)
	}
	if count("PUT") != 1 || count("POST") != 1 {
		t.Fatalf("want one replace and one add back, got %v", ls.calls)
	}

	// Again: nothing to do, so only a read.
	ls.calls = nil
	if err := c.SetLabels(ctx, ref, []string{"devbot/working"}, []string{"devbot/ready"}); err != nil {
		t.Fatal(err)
	}
	if len(ls.calls) != 1 || !strings.HasPrefix(ls.calls[0], "GET") {
		t.Fatalf("a change already made should only read: %v", ls.calls)
	}

	// Only adding: one add, which leaves alice's new label alone.
	ls.calls = nil
	ls.afterRead = func(ls *labelServer) { ls.set("alice", []int64{2, 3, 4, 5}) } // alice adds devbot/needs-human
	if err := c.SetLabels(ctx, ref, []string{"devbot/ready"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := ls.names(); got != "bug,devbot/working,priority,devbot/needs-human,devbot/ready" {
		t.Fatalf("labels after an add: %s", got)
	}
	if count("PUT") != 0 || count("POST") != 1 {
		t.Fatalf("an add alone must be one add, not a replace: %v", ls.calls)
	}
}

// Gitea's issue-or-PR writer check answers the label-edit probe before
// the label lookup: 403 is no, the label's 404 is yes.
func TestCanEditIssues(t *testing.T) {
	for status, want := range map[int]bool{403: false, 404: true, 422: true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "PATCH" || r.URL.Path != "/api/v1/repos/acme/app/labels/0" || r.Header.Get("Authorization") != "token person" {
				t.Errorf("unexpected probe %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(status)
		}))
		c := New(Config{Name: "gitea", URL: srv.URL, ImplementerTok: "impl-tok"})
		got, err := c.AsUser("person").CanEditIssues(context.Background(), forge.RepoRef{Provider: "gitea", Owner: "acme", Name: "app"})
		if err != nil || got != want {
			t.Errorf("HTTP %d: got %v, %v; want %v", status, got, err, want)
		}
		srv.Close()
	}
}

func TestAttachToPRAndGetAttachment(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake image")
	var srvURL string
	var fetched []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/repos/acme/app/issues/7/assets":
			if r.Header.Get("Authorization") != "token impl-tok" || r.URL.Query().Get("name") != "board.png" {
				http.Error(w, "bad request", 400)
				return
			}
			f, h, err := r.FormFile("attachment")
			if err != nil || h.Filename != "board.png" {
				http.Error(w, "no attachment", 400)
				return
			}
			b, _ := io.ReadAll(f)
			if !bytes.Equal(b, png) {
				http.Error(w, "content mangled", 400)
				return
			}
			w.WriteHeader(201)
			// ROOT_URL can differ from the address devbot uses.
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "name": "board.png", "browser_download_url": "https://public.example/attachments/0a1b2c3d-0000-4000-8000-000000000001"})
		case r.Method == "GET" && r.URL.Path == "/api/v1/repos/acme/app/issues/7/assets":
			json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "uuid": "0a1b2c3d-0000-4000-8000-000000000001"}})
		case r.Method == "GET" && r.URL.Path == "/api/v1/repos/acme/app/issues/8/assets":
			w.Write([]byte("[]"))
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/attachments/"):
			fetched = append(fetched, r.URL.Path)
			if r.URL.Path != "/attachments/0a1b2c3d-0000-4000-8000-000000000001" {
				w.Write([]byte("someone else's file"))
				return
			}
			if r.Header.Get("Authorization") != "token rev-tok" {
				http.Error(w, "forbidden", 403)
				return
			}
			w.Write(png)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL
	c := New(Config{Name: "gitea", URL: srvURL, ImplementerTok: "impl-tok", ReviewerTok: "rev-tok"})
	r := forge.RepoRef{Provider: "gitea", Owner: "acme", Name: "app"}
	u, err := c.AttachToPR(context.Background(), r, 7, "board.png", png)
	if err != nil || u != "https://public.example/attachments/0a1b2c3d-0000-4000-8000-000000000001" {
		t.Fatalf("upload: got %q, %v", u, err)
	}
	got, err := c.GetAttachment(context.Background(), r, 7, u, forge.RoleReviewer)
	if err != nil || !bytes.Equal(got, png) {
		t.Fatalf("download: got %q, %v", got, err)
	}
	// Anything that isn't an attachment URL is refused before a request is made.
	for _, bad := range []string{"https://evil.example/steal", srvURL + "/api/v1/repos/acme/app", "https://x/attachments/../../user/settings", "https://x/attachments/"} {
		if _, err := c.GetAttachment(context.Background(), r, 7, bad, forge.RoleReviewer); err == nil {
			t.Errorf("%q was fetched", bad)
		}
	}
	// An attachment URL that isn't in the PR's own assets (another repo's, or
	// another PR's) is refused too, though the token could read it.
	for _, tc := range []struct {
		n   int
		url string
	}{
		{7, "https://public.example/attachments/ffffffff-0000-4000-8000-00000000beef"},
		{8, u},
	} {
		if _, err := c.GetAttachment(context.Background(), r, tc.n, tc.url, forge.RoleReviewer); err == nil || !strings.Contains(err.Error(), "not attached") {
			t.Errorf("#%d %s: want refused, got %v", tc.n, tc.url, err)
		}
	}
	if len(fetched) != 1 {
		t.Fatalf("only the PR's own attachment may be downloaded, got %v", fetched)
	}
}

func TestAttachToPRReportsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"file type is not allowed"}`, 422)
	}))
	defer srv.Close()
	c := New(Config{Name: "gitea", URL: srv.URL, ImplementerTok: "impl-tok"})
	_, err := c.AttachToPR(context.Background(), forge.RepoRef{Provider: "gitea", Owner: "acme", Name: "app"}, 7, "x.png", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("want the API error, got %v", err)
	}
}

func TestWebURLs(t *testing.T) {
	c := New(Config{Name: "gitea", URL: "https://git.example/"})
	r := forge.RepoRef{Provider: "gitea", Owner: "acme", Name: "app"}
	for got, want := range map[string]string{
		c.IssueURL(forge.ItemRef{Repo: r, Number: 12}): "https://git.example/acme/app/issues/12",
		c.PRURL(r, 3):                   "https://git.example/acme/app/pulls/3",
		c.BranchURL(r, "devbot/12-a b"): "https://git.example/acme/app/src/branch/devbot/12-a%20b",
	} {
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

// A review pod holds the reviewer's token alone: it reads and comments as
// the reviewer, and refuses anything asked of the implementer.
func TestReviewerOnly(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := New(Config{Name: "gitea", URL: srv.URL, ImplementerUser: "devbot", ReviewerUser: "devbot-review", ReviewerTok: "rev-tok"})
	r := forge.RepoRef{Provider: "gitea", Owner: "acme", Name: "app"}
	ctx := context.Background()
	if err := c.Comment(ctx, forge.ItemRef{Repo: r, Number: 1}, "hi"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DefaultBranch(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, h, err := c.GitAuth(ctx, r, forge.RoleReviewer); err != nil || h != "Authorization: token rev-tok" {
		t.Fatalf("GitAuth as the reviewer: %q, %v", h, err)
	}
	if _, _, err := c.GitAuth(ctx, r, forge.RoleImplementer); !errors.Is(err, forge.ErrNoCredential) {
		t.Fatalf("GitAuth as the implementer: %v", err)
	}
	if _, err := c.CanWrite(ctx, r, forge.RoleImplementer); !errors.Is(err, forge.ErrNoCredential) {
		t.Fatalf("CanWrite as the implementer: %v", err)
	}
	// Gitea gives workflow files no permission of their own.
	if ok, err := c.CanPushWorkflows(ctx, r, forge.RoleReviewer); !ok || err != nil {
		t.Fatalf("CanPushWorkflows as the reviewer: %v, %v", ok, err)
	}
	if _, err := c.CanPushWorkflows(ctx, r, forge.RoleImplementer); !errors.Is(err, forge.ErrNoCredential) {
		t.Fatalf("CanPushWorkflows as the implementer: %v", err)
	}
	for _, a := range auth {
		if !strings.HasSuffix(a, " token rev-tok") {
			t.Errorf("%s", a)
		}
	}
	if len(auth) != 2 {
		t.Fatalf("requests: %v", auth)
	}
}

// Whether outdated branches are blocked is on the rule, which Gitea shows to
// repo admins only; when it can't be read, the rest still is.
func TestProtectionUpToDate(t *testing.T) {
	for _, c := range []struct {
		status int
		block  bool
		want   bool
	}{{200, true, true}, {200, false, false}, {403, false, false}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/repos/acme/app/branches/main":
				writeJSON(w, 200, map[string]any{"name": "main", "protected": true, "user_can_merge": true,
					"enable_status_check": true, "status_check_contexts": []string{"ci"}, "effective_branch_protection_name": "main"})
			case "/api/v1/repos/acme/app/branch_protections/main":
				writeJSON(w, c.status, map[string]any{"rule_name": "main", "block_on_outdated_branch": c.block})
			default:
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			}
		}))
		cl := New(Config{Name: "gitea", URL: srv.URL, ReviewerUser: "devbot-review", ReviewerTok: "rev-tok"})
		p, err := cl.Protection(context.Background(), forge.RepoRef{Provider: "gitea", Owner: "acme", Name: "app"}, "main")
		if err != nil || p.UpToDate != c.want || len(p.RequiredChecks) != 1 {
			t.Errorf("HTTP %d, block %v: got %+v, %v", c.status, c.block, p, err)
		}
		srv.Close()
	}
}

// A bot that can read the repo but not push to it is told what to grant.
func TestCanWriteNamesMissingAccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"full_name":"acme/app","permissions":{"admin":false,"push":false,"pull":true}}`))
	}))
	defer srv.Close()
	c := New(Config{Name: "gitea", URL: srv.URL, ImplementerUser: "devbot", ImplementerTok: "impl-tok", ReviewerUser: "devbot-review", ReviewerTok: "rev-tok"})
	r := forge.RepoRef{Provider: "gitea", Owner: "acme", Name: "app"}
	ok, err := c.CanWrite(context.Background(), r, forge.RoleReviewer)
	var me *forge.MissingAccessError
	if ok || !errors.As(err, &me) || !strings.HasPrefix(me.Need, "the reviewer bot @devbot-review has read access to acme/app; devbot needs write") {
		t.Fatalf("got %v, %v", ok, err)
	}
}

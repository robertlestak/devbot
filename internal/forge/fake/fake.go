// Package fake is an in-memory forge for tests. It models just enough -
// issues, comments, labels with a timeline, PRs, reviews, statuses, files - to
// drive the engine and the harness end to end.
package fake

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
)

type Forge struct {
	mu sync.Mutex

	ProviderName string
	Impl, Rev    string
	Writers      map[string]bool                        // users with write access
	Perms        map[string]forge.Permission            // overrides Writers for Authorized
	RepoPerms    map[string]map[string]forge.Permission // repo -> user -> permission, overrides both (see Grant)
	ForbidLookup bool                                   // Authorized fails as if the bot may not read permissions
	Actor        string                                 // who label changes are attributed to

	Items       map[string]*forge.Item
	Comments    map[string][]forge.Comment
	Timeline    map[string][]forge.LabelEvent
	LabelsErr   error                          // LabelEvents fails with this
	Edits       map[string][]forge.ContentEdit // see EditIssue
	EditsErr    error                          // ContentEdits fails with this
	Unrecorded  bool                           // EditIssue leaves no record
	PRs         map[string]*forge.PR           // repo#n
	ReviewMap   map[string][]forge.Review
	Lines       map[string][]forge.ReviewComment // repo#n, see AddLineReview
	Statuses    map[string][]forge.CheckDetail   // sha
	Reruns      map[string]int                   // sha -> RerunChecks calls
	RerunErr    error                            // RerunChecks fails with this
	Reports     map[string]forge.CheckReport     // sha + " " + context; a Log is the whole log, which CheckReport tails
	ReportErr   error                            // CheckReport fails with this
	Files       map[string][]byte                // repo:path on the default branch, repo@branch:path on another
	TreeErr     error                            // ListTree fails with this
	Calls       map[string]int                   // method -> calls, for BranchHead, ListTree, GetFile, Authorized, GetItem, GetPR, ListOpenPRs, ListTags and Contains
	Defaults    map[string]string                // repo -> default branch
	CloneURL    map[string]string                // repo -> file:// URL for git
	GitHeader   func(forge.Role) string          // GitAuth's header when set, asked for on every call
	NoWorkflows bool                             // CanPushWorkflows says no, as a GitHub App without workflows write does
	Attachments map[string][]byte                // URL -> content
	CommentErr  error                            // the bot's Comment fails with this
	SetLabelErr error                            // the bot's SetLabels fails with this
	AttachErr   error                            // AttachToPR fails with this
	Released    map[string]int                   // repo#n -> ReleaseAttachments calls
	ReleaseErr  error                            // ReleaseAttachments fails with this
	Branches    map[string]*forge.Branch         // repo:name, besides the default branch (see AddBranch)
	Deleted     []string                         // repo:name, in the order DeleteBranch deleted them
	// Vanish lists branches (repo:name) the forge removes on its own just
	// before DeleteBranch gets to them, as its delete-after-merge would
	// between a list and a delete.
	Vanish map[string]bool
	// Tags are each repo's tags, newest first (see AddTag), and History the
	// commits each commit's history has besides itself (see Descends).
	Tags    map[string][]forge.Tag
	History map[string][]string
	// Protections are the repo's own rules, by "repo:branch". Merge refuses
	// a Blocked PR, as the forge would, unless Bypass is set.
	Protections  map[string]forge.Protection
	Bypass       bool
	ProtectErr   error  // Protection fails with this
	OnProtection func() // runs as Protection is read, between the gate reading the PR and merging it
	// MergeErr makes Merge fail with it without merging; MergeLost makes
	// Merge merge and then fail with it, as a reply lost after the forge
	// acted. BeforeMerge runs first, under the lock, to change the PR
	// between the engine reading it and merging (a push that moves the head).
	MergeErr    error
	MergeLost   error
	BeforeMerge func(*forge.PR)
	attachedTo  map[string]string // URL -> repo#n it was attached to

	oauth oauthState

	next  int
	clock func() time.Time
	seed  []string // branches AddRepo makes (see NewAs)
}

func New() *Forge {
	return &Forge{
		ProviderName: "fake", Impl: "devbot", Rev: "devbot-review", Actor: "alice",
		Writers: map[string]bool{"alice": true, "devbot": true, "devbot-review": true}, Perms: map[string]forge.Permission{}, RepoPerms: map[string]map[string]forge.Permission{}, Items: map[string]*forge.Item{}, Comments: map[string][]forge.Comment{},
		Timeline: map[string][]forge.LabelEvent{}, Edits: map[string][]forge.ContentEdit{}, PRs: map[string]*forge.PR{},
		ReviewMap: map[string][]forge.Review{}, Lines: map[string][]forge.ReviewComment{}, Statuses: map[string][]forge.CheckDetail{}, Reruns: map[string]int{}, Reports: map[string]forge.CheckReport{}, Files: map[string][]byte{}, Calls: map[string]int{},
		Defaults: map[string]string{}, CloneURL: map[string]string{}, Attachments: map[string][]byte{}, Released: map[string]int{}, Branches: map[string]*forge.Branch{}, Tags: map[string][]forge.Tag{}, History: map[string][]string{}, attachedTo: map[string]string{}, Protections: map[string]forge.Protection{}, clock: time.Now, oauth: newOAuthState(),
	}
}

// now is to the second, as real forges keep timestamps: two things done in
// the same second can't be told apart by time.
func (f *Forge) now() time.Time { return f.clock().UTC().Truncate(time.Second) }

// Now is the time on the fake's clock, to the second, as it stamps things.
func (f *Forge) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now()
}

// SetClock puts the fake on the test's clock, for tests that move time.
func (f *Forge) SetClock(c func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = c
}

func (f *Forge) Name() string               { return f.ProviderName }
func (f *Forge) BotUsers() (string, string) { return f.Impl, f.Rev }
func prKey(r forge.RepoRef, n int) string   { return fmt.Sprintf("%s#%d", r, n) }
func (f *Forge) ParseWebhook(*http.Request, []byte) ([]forge.Event, error) {
	return nil, errors.New("fake: no webhooks")
}

func (f *Forge) nextNum() int { f.next++; return f.next }

// AddRepo opts a repo in with the given .devbot.yaml, with the branches
// the profile keeps in every repo (see NewAs).
func (f *Forge) AddRepo(r forge.RepoRef, devbotYAML string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Defaults[r.String()] = "main"
	if devbotYAML != "" {
		f.Files[r.String()+":.devbot.yaml"] = []byte(devbotYAML)
	}
	for _, b := range f.seed {
		f.Branches[r.String()+":"+b] = &forge.Branch{Name: b, SHA: "sha-" + b, CommittedAt: f.now()}
	}
}

func (f *Forge) SearchItems(_ context.Context, label, state string) ([]forge.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []forge.Item
	for _, it := range f.Items {
		if it.HasLabel(label) && (state == "all" || it.State == state) {
			out = append(out, f.copyItem(it))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref.Number < out[j].Ref.Number })
	return out, nil
}

func (f *Forge) ListItems(ctx context.Context, repo forge.RepoRef, label, state string) ([]forge.Item, error) {
	items, err := f.SearchItems(ctx, label, state)
	return slices.DeleteFunc(items, func(it forge.Item) bool { return it.Ref.Repo != repo }), err
}

func (f *Forge) copyItem(it *forge.Item) forge.Item {
	c := *it
	c.Labels = append([]string(nil), it.Labels...)
	return c
}

func copyPR(pr *forge.PR) forge.PR {
	c := *pr
	c.Labels = slices.Clone(pr.Labels)
	return c
}

func (f *Forge) SearchPRs(_ context.Context, label string) ([]forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []forge.PR
	for _, pr := range f.PRs {
		if pr.HasLabel(label) && pr.State == "open" {
			out = append(out, copyPR(pr))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// labelsOf is the labels of issue or PR ref, which share numbers as on a
// real forge, or nil when there is neither.
func (f *Forge) labelsOf(ref forge.ItemRef) *[]string {
	if it, ok := f.Items[ref.String()]; ok {
		return &it.Labels
	}
	if pr, ok := f.PRs[prKey(ref.Repo, ref.Number)]; ok {
		return &pr.Labels
	}
	return nil
}

func (f *Forge) CreateItem(_ context.Context, r forge.RepoRef, title, body string, labels []string) (forge.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createItem(r, f.Impl, title, body, labels), nil
}

func (f *Forge) createItem(r forge.RepoRef, author, title, body string, labels []string) forge.Item {
	ref := forge.ItemRef{Repo: r, Number: f.nextNum()}
	it := &forge.Item{Ref: ref, Title: title, Body: body, State: "open", Labels: append([]string(nil), labels...),
		Author: author, URL: f.IssueURL(ref), CreatedAt: f.now(), UpdatedAt: f.now()}
	f.Items[ref.String()] = it
	for _, l := range labels {
		f.Timeline[ref.String()] = append(f.Timeline[ref.String()], forge.LabelEvent{Label: l, Actor: author, Added: true, At: f.now()})
	}
	return f.copyItem(it)
}

// NewIssue is a human-filed issue.
func (f *Forge) NewIssue(r forge.RepoRef, author, title, body string, labels ...string) forge.ItemRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	ref := forge.ItemRef{Repo: r, Number: f.nextNum()}
	f.Items[ref.String()] = &forge.Item{Ref: ref, Title: title, Body: body, State: "open", Labels: labels,
		Author: author, URL: f.IssueURL(ref), CreatedAt: f.now(), UpdatedAt: f.now()}
	for _, l := range labels {
		f.Timeline[ref.String()] = append(f.Timeline[ref.String()], forge.LabelEvent{Label: l, Actor: author, Added: true, At: f.now()})
	}
	return ref
}

func (f *Forge) GetItem(_ context.Context, ref forge.ItemRef) (forge.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls["GetItem"]++
	it, ok := f.Items[ref.String()]
	if !ok {
		return forge.Item{}, forge.ErrNotFound
	}
	return f.copyItem(it), nil
}

func (f *Forge) CloseItem(_ context.Context, ref forge.ItemRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if it, ok := f.Items[ref.String()]; ok {
		it.State = "closed"
	}
	if pr, ok := f.PRs[prKey(ref.Repo, ref.Number)]; ok && pr.State == "open" {
		pr.State, pr.ClosedAt = "closed", f.now()
	}
	return nil
}

func (f *Forge) ListComments(_ context.Context, ref forge.ItemRef) ([]forge.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Items[ref.String()] == nil && f.PRs[prKey(ref.Repo, ref.Number)] == nil {
		return nil, forge.ErrNotFound
	}
	return append([]forge.Comment(nil), f.Comments[ref.String()]...), nil
}

func (f *Forge) Comment(_ context.Context, ref forge.ItemRef, body string) error {
	if f.CommentErr != nil {
		return f.CommentErr
	}
	f.AddComment(ref, f.Impl, body)
	return nil
}

// AddComment posts as any user. Comments in the same second share a
// timestamp, as on a real forge; their IDs keep them in order.
func (f *Forge) AddComment(ref forge.ItemRef, author, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addComment(ref, author, body)
}

func (f *Forge) addComment(ref forge.ItemRef, author, body string) {
	f.addCommentAt(ref, author, body, f.now())
}

// AddCommentAt posts as any user, at a time of the test's choosing.
func (f *Forge) AddCommentAt(ref forge.ItemRef, author, body string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addCommentAt(ref, author, body, at.UTC().Truncate(time.Second))
}

func (f *Forge) addCommentAt(ref forge.ItemRef, author, body string, at time.Time) {
	cs := f.Comments[ref.String()]
	f.Comments[ref.String()] = append(cs, forge.Comment{ID: int64(len(cs) + 1), Author: author, Body: body, CreatedAt: at, UpdatedAt: at})
}

func (f *Forge) UpdateComment(_ context.Context, ref forge.ItemRef, id int64, body string) error {
	if f.CommentErr != nil {
		return f.CommentErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cs := f.Comments[ref.String()]
	for i := range cs {
		if cs[i].ID == id {
			cs[i].Body, cs[i].UpdatedAt = body, f.now()
			if !cs[i].UpdatedAt.After(cs[i].CreatedAt) {
				cs[i].UpdatedAt = cs[i].CreatedAt.Add(time.Second)
			}
			return nil
		}
	}
	return forge.ErrNotFound
}

// EditComment replaces comment id's body a second after it was posted, as
// anyone with write access can, whoever wrote it.
func (f *Forge) EditComment(ref forge.ItemRef, id int64, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs := f.Comments[ref.String()]
	for i := range cs {
		if cs[i].ID == id {
			cs[i].Body, cs[i].UpdatedAt = body, cs[i].CreatedAt.Add(time.Second)
		}
	}
}

func (f *Forge) SetLabels(_ context.Context, ref forge.ItemRef, add, remove []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.SetLabelErr != nil {
		return f.SetLabelErr
	}
	return f.setLabels(ref, f.Impl, add, remove)
}

func (f *Forge) setLabels(ref forge.ItemRef, actor string, add, remove []string) error {
	labels := f.labelsOf(ref)
	if labels == nil {
		return forge.ErrNotFound
	}
	keep := (*labels)[:0]
	for _, l := range *labels {
		drop := false
		for _, r := range remove {
			if r == l {
				drop = true
				f.Timeline[ref.String()] = append(f.Timeline[ref.String()], forge.LabelEvent{Label: l, Actor: actor, At: f.now()})
			}
		}
		if !drop {
			keep = append(keep, l)
		}
	}
	*labels = keep
	for _, a := range add {
		if !slices.Contains(*labels, a) {
			*labels = append(*labels, a)
			f.Timeline[ref.String()] = append(f.Timeline[ref.String()], forge.LabelEvent{Label: a, Actor: actor, Added: true, At: f.now()})
		}
	}
	return nil
}

// HumanLabel adds a label, to an issue or a PR, as a person.
func (f *Forge) HumanLabel(ref forge.ItemRef, actor, label string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	labels := f.labelsOf(ref)
	if !slices.Contains(*labels, label) {
		*labels = append(*labels, label)
	}
	f.Timeline[ref.String()] = append(f.Timeline[ref.String()], forge.LabelEvent{Label: label, Actor: actor, Added: true, At: f.now()})
}

// HumanUnlabel removes a label, from an issue or a PR, as a person.
func (f *Forge) HumanUnlabel(ref forge.ItemRef, actor, label string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setLabels(ref, actor, nil, []string{label})
}

func (f *Forge) EnsureLabels(context.Context, forge.RepoRef, []forge.Label) error { return nil }

func (f *Forge) LabelEvents(_ context.Context, ref forge.ItemRef) ([]forge.LabelEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.LabelsErr != nil {
		return nil, f.LabelsErr
	}
	return append([]forge.LabelEvent(nil), f.Timeline[ref.String()]...), nil
}

// EditIssue changes an issue's title and body as editor, recording an edit
// for each that changed. An empty editor is an edit the forge records
// without saying who made it. With Unrecorded set, edits leave no record
// at all.
func (f *Forge) EditIssue(ref forge.ItemRef, editor, title, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.editIssue(ref, editor, title, body)
}

// EditItem is the implementer's edit, recorded as a person's would be.
func (f *Forge) EditItem(_ context.Context, ref forge.ItemRef, title, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Items[ref.String()] == nil {
		return forge.ErrNotFound
	}
	f.editIssue(ref, f.Impl, title, body)
	return nil
}

func (f *Forge) editIssue(ref forge.ItemRef, editor, title, body string) {
	it := f.Items[ref.String()]
	for _, e := range []struct{ field, old, new string }{{forge.FieldTitle, it.Title, title}, {forge.FieldBody, it.Body, body}} {
		if e.old != e.new && !f.Unrecorded {
			f.Edits[ref.String()] = append(f.Edits[ref.String()], forge.ContentEdit{Field: e.field, Editor: editor, At: f.now()})
		}
	}
	it.Title, it.Body, it.UpdatedAt = title, body, f.now()
}

func (f *Forge) ContentEdits(_ context.Context, ref forge.ItemRef) ([]forge.ContentEdit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.EditsErr != nil {
		return nil, f.EditsErr
	}
	return append([]forge.ContentEdit(nil), f.Edits[ref.String()]...), nil
}

func (f *Forge) OpenPR(_ context.Context, r forge.RepoRef, head, base, title, body string) (forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openPR(r, f.Impl, head, base, title, body, false), nil
}

// forkOf is author's fork of r, where HumanPR puts a fork PR's head.
func forkOf(r forge.RepoRef, author string) forge.RepoRef {
	return forge.RepoRef{Provider: r.Provider, Owner: author, Name: r.Name}
}

// HumanPR is a PR a person opened, from head in the repo itself or, with
// fork, from a branch of that name in their fork, which the repo doesn't
// have.
func (f *Forge) HumanPR(r forge.RepoRef, author, head, base, title, body string, fork bool) forge.PR {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openPR(r, author, head, base, title, body, fork)
}

func (f *Forge) openPR(r forge.RepoRef, author, head, base, title, body string, fork bool) forge.PR {
	pr := &forge.PR{Repo: r, Number: f.nextNum(), Title: title, Body: body, State: "open", HeadBranch: head, BaseBranch: base,
		HeadSHA: "sha-" + head, Mergeable: true, Author: author, CreatedAt: f.now(), Fork: fork}
	if b, ok := f.Branches[r.String()+":"+head]; ok && !fork {
		pr.HeadSHA = b.SHA
	} else if !fork {
		f.Branches[r.String()+":"+head] = &forge.Branch{Name: head, SHA: pr.HeadSHA, CommittedAt: f.now()}
	}
	pr.HeadRepo = r
	if fork {
		pr.HeadRepo = forkOf(r, author)
	}
	pr.URL = f.PRURL(r, pr.Number)
	f.PRs[prKey(r, pr.Number)] = pr
	return *pr
}

func (f *Forge) UpdatePR(_ context.Context, r forge.RepoRef, n int, title, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, ok := f.PRs[prKey(r, n)]
	if !ok {
		return forge.ErrNotFound
	}
	pr.Title, pr.Body = title, body
	return nil
}

// SetHead moves a PR's head and its branch, as a push would.
func (f *Forge) SetHead(r forge.RepoRef, n int, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr := f.PRs[prKey(r, n)]
	pr.HeadSHA = sha
	f.Branches[r.String()+":"+pr.HeadBranch] = &forge.Branch{Name: pr.HeadBranch, SHA: sha, CommittedAt: f.now()}
}

// Push points a branch at sha, creating it if need be, as a push would.
// Every open PR onto a branch that moved on is behind it now, and every open
// PR from it, in the repo itself, has sha for its head.
func (f *Forge) Push(r forge.RepoRef, name, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := r.String() + ":" + name
	if old, ok := f.Branches[k]; ok && old.SHA != sha {
		for _, pr := range f.PRs {
			if pr.Repo == r && pr.BaseBranch == name && pr.State == "open" {
				pr.Behind = true
			}
			if pr.Repo == r && pr.HeadBranch == name && pr.State == "open" && !pr.Fork {
				pr.HeadSHA = sha
			}
		}
	}
	f.Branches[k] = &forge.Branch{Name: name, SHA: sha, CommittedAt: f.now()}
}

// EditPR changes a PR as edit says, as its author or the forge would.
func (f *Forge) EditPR(r forge.RepoRef, n int, edit func(pr *forge.PR)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edit(f.PRs[prKey(r, n)])
}

// ClosePR closes a PR without merging it, as a person would.
func (f *Forge) ClosePR(r forge.RepoRef, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr := f.PRs[prKey(r, n)]
	pr.State, pr.ClosedAt = "closed", f.now()
}

func (f *Forge) GetPR(_ context.Context, r forge.RepoRef, n int) (forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls["GetPR"]++
	pr, ok := f.PRs[prKey(r, n)]
	if !ok {
		return forge.PR{}, forge.ErrNotFound
	}
	return copyPR(pr), nil
}

func (f *Forge) ListOpenPRs(_ context.Context, r forge.RepoRef) ([]forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls["ListOpenPRs"]++
	var out []forge.PR
	for _, pr := range f.PRs {
		if pr.Repo == r && pr.State == "open" {
			out = append(out, copyPR(pr))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

func (f *Forge) SetStatus(sha, context, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, d := range f.Statuses[sha] {
		if d.Context == context {
			f.Statuses[sha][i].State, f.Statuses[sha][i].Cancelled = state, false
			return
		}
	}
	f.Statuses[sha] = append(f.Statuses[sha], forge.CheckDetail{Context: context, State: state})
}

// SetCancelled reports context on sha as cancelled, as the forge's own CI
// does for a run cancelled before it finished.
func (f *Forge) SetCancelled(sha, context string) {
	f.SetStatus(sha, context, "failure")
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, d := range f.Statuses[sha] {
		if d.Context == context {
			f.Statuses[sha][i].Cancelled, f.Statuses[sha][i].URL = true, "fake/runs/"+context
		}
	}
}

// SetFailedJob reports context on sha as a failed job of the forge's own
// CI, whose log is log.
func (f *Forge) SetFailedJob(sha, context, log string) {
	f.SetStatus(sha, context, "failure")
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, d := range f.Statuses[sha] {
		if d.Context == context {
			f.Statuses[sha][i].URL = "fake/runs/" + context
		}
	}
	f.Reports[sha+" "+context] = forge.CheckReport{Log: log, HasLog: true}
}

// SetCheckRun reports context on sha as a failed check run with the output
// and annotations in r, and a log if r has one.
func (f *Forge) SetCheckRun(sha, context string, r forge.CheckReport) {
	f.SetStatus(sha, context, "failure")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Reports[sha+" "+context] = r
}

// CheckReport is what SetFailedJob or SetCheckRun gave the check, its log
// tailed to logLines; a check they didn't set is a plain commit status.
func (f *Forge) CheckReport(_ context.Context, _ forge.RepoRef, sha string, check forge.CheckDetail, logLines int) (forge.CheckReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ReportErr != nil {
		return forge.CheckReport{}, f.ReportErr
	}
	r := f.Reports[sha+" "+check.Context]
	r.Annotations = slices.Clone(r.Annotations)
	if len(r.Annotations) > forge.MaxAnnotations {
		r.Annotations, r.MoreAnnotations = r.Annotations[:forge.MaxAnnotations], true
	}
	if r.HasLog {
		var err error
		if r.Log, r.LogCut, err = forge.TailLines(strings.NewReader(r.Log), logLines); err != nil {
			return forge.CheckReport{}, err
		}
	}
	return r, nil
}

// RerunChecks starts the checks again, pending, as the forge does once a
// re-run is queued, and counts the call in Reruns. A check with no URL isn't
// the forge's own CI.
func (f *Forge) RerunChecks(_ context.Context, _ forge.RepoRef, sha string, checks []forge.CheckDetail) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RerunErr != nil {
		return f.RerunErr
	}
	for _, c := range checks {
		if c.URL == "" {
			return fmt.Errorf("check %q: %w", c.Context, forge.ErrNotRerunnable)
		}
	}
	for _, c := range checks {
		for i, d := range f.Statuses[sha] {
			if d.Context == c.Context {
				f.Statuses[sha][i].State, f.Statuses[sha][i].Cancelled = "pending", false
			}
		}
	}
	f.Reruns[sha]++
	return nil
}

func (f *Forge) CombinedChecks(_ context.Context, _ forge.RepoRef, sha string) (forge.CheckState, []forge.CheckDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ds := slices.Clone(f.Statuses[sha])
	if len(ds) == 0 {
		return forge.ChecksNone, nil, nil
	}
	st, pending := forge.ChecksSuccess, false
	for i, d := range ds {
		switch d.State {
		case "pending":
			pending, ds[i].Pending = true, true
		case "failure":
			st, ds[i].Failed = forge.ChecksFailure, true
		}
	}
	if pending {
		return forge.ChecksPending, ds, nil
	}
	return st, ds, nil
}

func (f *Forge) Review(_ context.Context, r forge.RepoRef, n int, sha string, v forge.Verdict, body string) error {
	f.AddReview(r, n, f.Rev, sha, v, body)
	return nil
}

func (f *Forge) AddReview(r forge.RepoRef, n int, author, sha string, v forge.Verdict, body string) {
	f.AddLineReview(r, n, author, sha, v, body)
}

// AddLineReview submits a review as any user, with comments on lines of the
// diff, each given its Path, Line and Body.
func (f *Forge) AddLineReview(r forge.RepoRef, n int, author, sha string, v forge.Verdict, body string, lines ...forge.ReviewComment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := prKey(r, n)
	id := int64(len(f.ReviewMap[k]) + 1)
	f.ReviewMap[k] = append(f.ReviewMap[k], forge.Review{ID: id, Author: author, Verdict: v, CommitSHA: sha, Body: body, SubmittedAt: f.now()})
	for _, c := range lines {
		c.ID, c.ReviewID, c.Author, c.CommitSHA, c.CreatedAt = int64(len(f.Lines[k])+1), id, author, sha, f.now()
		f.Lines[k] = append(f.Lines[k], c)
	}
}

func (f *Forge) Reviews(_ context.Context, r forge.RepoRef, n int) ([]forge.Review, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forge.Review(nil), f.ReviewMap[prKey(r, n)]...), nil
}

func (f *Forge) ReviewComments(_ context.Context, r forge.RepoRef, n int) ([]forge.ReviewComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forge.ReviewComment(nil), f.Lines[prKey(r, n)]...), nil
}

func (f *Forge) Merge(_ context.Context, r forge.RepoRef, n int, sha, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, ok := f.PRs[prKey(r, n)]
	if !ok {
		return forge.ErrNotFound
	}
	if f.BeforeMerge != nil {
		f.BeforeMerge(pr)
	}
	if f.MergeErr != nil {
		return f.MergeErr
	}
	switch {
	case pr.HeadSHA != sha:
		return forge.ErrHeadMoved
	case pr.MergeableUnknown:
		return fmt.Errorf("%w: still checking", forge.ErrMergePending)
	case !pr.Mergeable:
		return fmt.Errorf("%w: it conflicts", forge.ErrNotMergeable)
	case pr.Blocked && !f.Bypass:
		return fmt.Errorf("%w: at least 1 approving review is required", forge.ErrNotMergeable)
	}
	pr.Merged, pr.State, pr.ClosedAt = true, "closed", f.now()
	// The merge commit has the head in its history.
	pr.MergeSHA = "merge-" + sha
	f.History[pr.MergeSHA] = append(f.History[pr.MergeSHA], sha)
	// The base moved: every other open PR onto it is now behind.
	for _, o := range f.PRs {
		if o != pr && o.Repo == r && o.BaseBranch == pr.BaseBranch && o.State == "open" {
			o.Behind = true
		}
	}
	return f.MergeLost
}

func (f *Forge) Protection(_ context.Context, r forge.RepoRef, branch string) (forge.Protection, error) {
	if f.OnProtection != nil {
		f.OnProtection()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ProtectErr != nil {
		return forge.Protection{}, f.ProtectErr
	}
	return f.Protections[r.String()+":"+branch], nil
}

func (f *Forge) DefaultBranch(_ context.Context, r forge.RepoRef) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.Defaults[r.String()]
	if !ok {
		return "", forge.ErrNotFound
	}
	return b, nil
}

// head is the commit branch points at: where Push left it, or
// "sha-<branch>" for a default branch nobody pushed to.
func (f *Forge) head(r forge.RepoRef, branch string) (string, bool) {
	if b, ok := f.Branches[r.String()+":"+branch]; ok {
		return b.SHA, true
	}
	if f.Defaults[r.String()] == branch {
		return "sha-" + branch, true
	}
	return "", false
}

// branchAt is the branch ref names, a branch or the SHA of one's head.
func (f *Forge) branchAt(r forge.RepoRef, ref string) string {
	if _, ok := f.head(r, ref); ok {
		return ref
	}
	for k, b := range f.Branches {
		if strings.HasPrefix(k, r.String()+":") && b.SHA == ref {
			return b.Name
		}
	}
	if d := f.Defaults[r.String()]; ref == "sha-"+d {
		return d
	}
	return ref
}

func (f *Forge) BranchHead(_ context.Context, r forge.RepoRef, branch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls["BranchHead"]++
	sha, ok := f.head(r, branch)
	if !ok {
		return "", forge.ErrNotFound
	}
	return sha, nil
}

// ListTree lists the default branch's files, and on another branch those
// with its own files over them, as GetFile reads them.
func (f *Forge) ListTree(_ context.Context, r forge.RepoRef, ref string) ([]forge.TreeFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls["ListTree"]++
	if f.TreeErr != nil {
		return nil, f.TreeErr
	}
	ref = f.branchAt(r, ref)
	if _, ok := f.head(r, ref); !ok {
		return nil, forge.ErrNotFound
	}
	files := map[string]int64{}
	for k, b := range f.Files {
		if p, ok := strings.CutPrefix(k, r.String()+":"); ok {
			files[p] = int64(len(b))
		}
	}
	if ref != f.Defaults[r.String()] {
		for k, b := range f.Files {
			if p, ok := strings.CutPrefix(k, r.String()+"@"+ref+":"); ok {
				files[p] = int64(len(b))
			}
		}
	}
	var out []forge.TreeFile
	for _, p := range slices.Sorted(maps.Keys(files)) {
		out = append(out, forge.TreeFile{Path: p, Size: files[p]})
	}
	return out, nil
}

func (f *Forge) GetFile(_ context.Context, r forge.RepoRef, ref, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls["GetFile"]++
	path = strings.TrimPrefix(path, "/")
	ref = f.branchAt(r, ref)
	if ref != "" && ref != f.Defaults[r.String()] {
		if b, ok := f.Files[r.String()+"@"+ref+":"+path]; ok {
			return b, nil
		}
	}
	b, ok := f.Files[r.String()+":"+path]
	if !ok {
		return nil, forge.ErrNotFound
	}
	return b, nil
}

func (f *Forge) GitAuth(_ context.Context, r forge.RepoRef, role forge.Role) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.GitHeader != nil {
		return f.CloneURL[r.String()], f.GitHeader(role), nil
	}
	return f.CloneURL[r.String()], "Authorization: Basic secret-token-" + string(role), nil
}

// CanPushWorkflows is false with NoWorkflows set.
func (f *Forge) CanPushWorkflows(_ context.Context, _ forge.RepoRef, _ forge.Role) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.NoWorkflows, nil
}

// CanWrite is the bot's own permission on r, as Authorized reads it.
func (f *Forge) CanWrite(_ context.Context, r forge.RepoRef, role forge.Role) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if role == forge.RoleReviewer {
		return f.perm(r, f.Rev) >= forge.PermWrite, nil
	}
	return f.perm(r, f.Impl) >= forge.PermWrite, nil
}

// Authorized reads RepoPerms, then Perms, falling back to write for Writers. ForbidLookup
// makes every lookup fail the way a bot without access to permissions does.
func (f *Forge) Authorized(_ context.Context, r forge.RepoRef, actor string, min forge.Permission) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ForbidLookup {
		return false, &forge.LookupForbiddenError{Need: "the fake bot needs to read permissions", Err: errors.New("fake: 403")}
	}
	f.Calls["Authorized"]++
	return f.perm(r, actor) >= min, nil
}

// Grant sets user's permission on one repo, over Perms and Writers.
func (f *Forge) Grant(r forge.RepoRef, user string, p forge.Permission) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RepoPerms[r.String()] == nil {
		f.RepoPerms[r.String()] = map[string]forge.Permission{}
	}
	f.RepoPerms[r.String()][user] = p
}

func (f *Forge) perm(r forge.RepoRef, user string) forge.Permission {
	if p, ok := f.RepoPerms[r.String()][user]; ok {
		return p
	}
	p, ok := f.Perms[user]
	if !ok && f.Writers[user] {
		p = forge.PermWrite
	}
	return p
}

// AttachToPR stores the file in Attachments under a fake URL, or fails with
// AttachErr.
func (f *Forge) AttachToPR(_ context.Context, r forge.RepoRef, n int, name string, data []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.AttachErr != nil {
		return "", f.AttachErr
	}
	if _, ok := f.PRs[prKey(r, n)]; !ok {
		return "", forge.ErrNotFound
	}
	u := fmt.Sprintf("https://fake/attachments/%d/%s", len(f.Attachments)+1, name)
	f.Attachments[u] = append([]byte(nil), data...)
	f.attachedTo[u] = prKey(r, n)
	return u, nil
}

// GetAttachment refuses a URL AttachToPR didn't return for this PR, as a real
// adapter must.
func (f *Forge) GetAttachment(_ context.Context, r forge.RepoRef, n int, url string, _ forge.Role) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.Attachments[url]
	if !ok || f.attachedTo[url] != prKey(r, n) {
		return nil, forge.ErrNotFound
	}
	return b, nil
}

// ReleaseAttachments forgets the PR's attachments and counts the call in
// Released, or fails with ReleaseErr.
func (f *Forge) ReleaseAttachments(_ context.Context, r forge.RepoRef, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Released[prKey(r, n)]++
	if f.ReleaseErr != nil {
		return f.ReleaseErr
	}
	for u, to := range f.attachedTo {
		if to == prKey(r, n) {
			delete(f.Attachments, u)
			delete(f.attachedTo, u)
		}
	}
	return nil
}

func (f *Forge) IssueURL(ref forge.ItemRef) string {
	return fmt.Sprintf("https://fake/%s/issues/%d", ref.Repo.FullName(), ref.Number)
}
func (f *Forge) PRURL(r forge.RepoRef, n int) string {
	return fmt.Sprintf("https://fake/%s/pulls/%d", r.FullName(), n)
}
func (f *Forge) BranchURL(r forge.RepoRef, branch string) string {
	return fmt.Sprintf("https://fake/%s/src/branch/%s", r.FullName(), (&url.URL{Path: branch}).EscapedPath())
}

// As is the forge as a job pod of role sees it, holding that bot's
// credential alone: it comments and labels as that bot, and refuses what
// only the other may do with forge.ErrNoCredential, as a real adapter
// built with one bot's credential does.
func (f *Forge) As(role forge.Role) forge.Provider { return &asRole{Forge: f, role: role} }

type asRole struct {
	*Forge
	role forge.Role
}

func (a *asRole) login() string {
	if a.role == forge.RoleReviewer {
		return a.Rev
	}
	return a.Impl
}

func (a *asRole) need(role forge.Role) error {
	if role != a.role {
		return fmt.Errorf("fake: the %s bot: %w", role, forge.ErrNoCredential)
	}
	return nil
}

func (a *asRole) Comment(_ context.Context, ref forge.ItemRef, body string) error {
	a.AddComment(ref, a.login(), body)
	return nil
}

func (a *asRole) SetLabels(_ context.Context, ref forge.ItemRef, add, remove []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.setLabels(ref, a.login(), add, remove)
}

func (a *asRole) Review(ctx context.Context, r forge.RepoRef, n int, sha string, v forge.Verdict, body string) error {
	if err := a.need(forge.RoleReviewer); err != nil {
		return err
	}
	return a.Forge.Review(ctx, r, n, sha, v, body)
}

func (a *asRole) Merge(ctx context.Context, r forge.RepoRef, n int, sha, method string) error {
	if err := a.need(forge.RoleReviewer); err != nil {
		return err
	}
	return a.Forge.Merge(ctx, r, n, sha, method)
}

func (a *asRole) GitAuth(ctx context.Context, r forge.RepoRef, role forge.Role) (string, string, error) {
	if err := a.need(role); err != nil {
		return "", "", err
	}
	return a.Forge.GitAuth(ctx, r, role)
}

func (a *asRole) CanPushWorkflows(ctx context.Context, r forge.RepoRef, role forge.Role) (bool, error) {
	if err := a.need(role); err != nil {
		return false, err
	}
	return a.Forge.CanPushWorkflows(ctx, r, role)
}

func (a *asRole) CanWrite(ctx context.Context, r forge.RepoRef, role forge.Role) (bool, error) {
	if err := a.need(role); err != nil {
		return false, err
	}
	return a.Forge.CanWrite(ctx, r, role)
}

func (a *asRole) GetAttachment(ctx context.Context, r forge.RepoRef, n int, url string, role forge.Role) ([]byte, error) {
	if err := a.need(role); err != nil {
		return nil, err
	}
	return a.Forge.GetAttachment(ctx, r, n, url, role)
}

// SetPRBase moves a PR onto another base, which must exist (see Branches).
func (f *Forge) SetPRBase(_ context.Context, r forge.RepoRef, n int, base string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, ok := f.PRs[prKey(r, n)]
	if !ok {
		return forge.ErrNotFound
	}
	if !f.branchExists(r, base) {
		return fmt.Errorf("fake: no branch %q", base)
	}
	pr.BaseBranch = base
	return nil
}

// ChangeBase moves a PR onto another base, as a person in the forge UI would.
func (f *Forge) ChangeBase(r forge.RepoRef, n int, base string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PRs[prKey(r, n)].BaseBranch = base
}

// AddBranch creates a branch on r besides its default one.
func (f *Forge) AddBranch(r forge.RepoRef, branch string) {
	f.Push(r, branch, "sha-"+branch)
}

func (f *Forge) BranchExists(_ context.Context, r forge.RepoRef, branch string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.branchExists(r, branch), nil
}

func (f *Forge) branchExists(r forge.RepoRef, branch string) bool {
	_, ok := f.Branches[r.String()+":"+branch]
	return f.Defaults[r.String()] == branch || ok
}

// ListRepos is every repo AddRepo added.
func (f *Forge) ListRepos(context.Context) ([]forge.RepoRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []forge.RepoRef
	for r := range f.Defaults {
		ref, err := forge.ParseRepo(r)
		if err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

func (f *Forge) ListBranches(_ context.Context, r forge.RepoRef, prefix string) ([]forge.Branch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []forge.Branch
	for k, b := range f.Branches {
		if strings.HasPrefix(k, r.String()+":") && strings.HasPrefix(b.Name, prefix) {
			out = append(out, *b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteBranch deletes the branch and records it in Deleted. A branch
// that is gone, or in Vanish, is ErrNotFound and not recorded.
func (f *Forge) DeleteBranch(_ context.Context, r forge.RepoRef, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := r.String() + ":" + name
	if f.Vanish[k] {
		delete(f.Branches, k)
	}
	if _, ok := f.Branches[k]; !ok {
		return forge.ErrNotFound
	}
	delete(f.Branches, k)
	f.Deleted = append(f.Deleted, k)
	return nil
}

// AddTag tags sha in r, as the newest of its tags.
func (f *Forge) AddTag(r forge.RepoRef, name, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Tags[r.String()] = slices.Insert(f.Tags[r.String()], 0, forge.Tag{Name: name, SHA: sha})
}

// Descends records that commit sha has ancestors in its history, as a
// commit on top of them does.
func (f *Forge) Descends(sha string, ancestors ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.History[sha] = append(f.History[sha], ancestors...)
}

func (f *Forge) ListTags(_ context.Context, r forge.RepoRef, n int) ([]forge.Tag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls["ListTags"]++
	tags := f.Tags[r.String()]
	return slices.Clone(tags[:min(n, len(tags))]), nil
}

// Contains follows History from ref, a tag of r or a commit, which any
// SHA is to the fake.
func (f *Forge) Contains(_ context.Context, r forge.RepoRef, ref, ancestor string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls["Contains"]++
	for _, t := range f.Tags[r.String()] {
		if t.Name == ref {
			ref = t.SHA
		}
	}
	seen := map[string]bool{}
	todo := []string{ref}
	for len(todo) > 0 {
		c := todo[0]
		todo = todo[1:]
		if c == ancestor {
			return true, nil
		}
		if !seen[c] {
			seen[c] = true
			todo = append(todo, f.History[c]...)
		}
	}
	return false, nil
}

func (f *Forge) SetCommitStatus(_ context.Context, _ forge.RepoRef, sha string, s forge.CommitStatus) error {
	f.SetStatus(sha, s.Context, s.State)
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, d := range f.Statuses[sha] {
		if d.Context == s.Context {
			f.Statuses[sha][i].Description, f.Statuses[sha][i].URL = s.Description, s.URL
		}
	}
	return nil
}

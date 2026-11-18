package github

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
)

type user struct {
	Login string `json:"login"`
}

type label struct {
	Name string `json:"name"`
}

type issue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	Labels    []label   `json:"labels"`
	User      user      `json:"user"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// GitHub serves every PR as an issue too, with this set.
	PullRequest *struct{} `json:"pull_request"`
}

func (c *Client) item(r forge.RepoRef, is issue) forge.Item {
	it := forge.Item{
		Ref: forge.ItemRef{Repo: r, Number: is.Number}, Title: is.Title, Body: is.Body,
		State: is.State, Author: is.User.Login, URL: is.HTMLURL,
		CreatedAt: is.CreatedAt, UpdatedAt: is.UpdatedAt,
	}
	for _, l := range is.Labels {
		it.Labels = append(it.Labels, l.Name)
	}
	return it
}

func (c *Client) GetItem(ctx context.Context, ref forge.ItemRef) (forge.Item, error) {
	cr, err := c.repoCred(ctx, c.acting(), ref.Repo)
	if err != nil {
		return forge.Item{}, err
	}
	var is issue
	if _, err := c.do(ctx, cr, "GET", fmt.Sprintf("%s/issues/%d", repoPath(ref.Repo), ref.Number), nil, &is); err != nil {
		return forge.Item{}, err
	}
	// Issues and PRs share numbers, so a webhook about a PR's conversation
	// names a PR here. There is no issue by that number.
	if is.PullRequest != nil {
		return forge.Item{}, fmt.Errorf("%s is a pull request, not an issue: %w", ref, forge.ErrNotFound)
	}
	return c.item(ref.Repo, is), nil
}

// ListComments reads the conversation, oldest first: on a PR, the comments
// on the PR as a whole, not on lines of its diff.
func (c *Client) ListComments(ctx context.Context, ref forge.ItemRef) ([]forge.Comment, error) {
	cr, err := c.repoCred(ctx, c.acting(), ref.Repo)
	if err != nil {
		return nil, err
	}
	cs, err := list[struct {
		ID        int64     `json:"id"`
		Body      string    `json:"body"`
		User      user      `json:"user"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}](ctx, c, cr, fmt.Sprintf("%s/issues/%d/comments", repoPath(ref.Repo), ref.Number))
	if err != nil {
		return nil, err
	}
	out := make([]forge.Comment, 0, len(cs))
	for _, cm := range cs {
		out = append(out, forge.Comment{ID: cm.ID, Author: cm.User.Login, Body: cm.Body, CreatedAt: cm.CreatedAt, UpdatedAt: cm.UpdatedAt})
	}
	return out, nil
}

// SearchItems finds the issues carrying lbl in state with the search API
// (GET /search/issues): one query per installation, in the account it is
// installed on (see search).
func (c *Client) SearchItems(ctx context.Context, lbl, state string) ([]forge.Item, error) {
	q := "is:issue label:" + strconv.Quote(lbl)
	if state != "all" {
		q += " state:" + state
	}
	var all []forge.Item
	err := c.search(ctx, q, func(r forge.RepoRef, is issue) {
		if is.PullRequest == nil {
			all = append(all, c.item(r, is))
		}
	})
	return all, err
}

// ListItems lists r's issues carrying lbl (docs: "List repository issues"),
// which GitHub serves with its PRs.
func (c *Client) ListItems(ctx context.Context, r forge.RepoRef, lbl, state string) ([]forge.Item, error) {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return nil, err
	}
	q := url.Values{"labels": {lbl}, "state": {state}}
	iss, err := list[issue](ctx, c, cr, repoPath(r)+"/issues?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var out []forge.Item
	for _, is := range iss {
		if is.PullRequest == nil {
			out = append(out, c.item(r, is))
		}
	}
	return out, nil
}

// SearchPRs is SearchItems's search for open PRs. A repo without issues is
// skipped, as there: devbot couldn't open a tracking issue there.
func (c *Client) SearchPRs(ctx context.Context, lbl string) ([]forge.PR, error) {
	var all []forge.PR
	err := c.search(ctx, "is:pr is:open label:"+strconv.Quote(lbl), func(r forge.RepoRef, is issue) {
		if is.PullRequest != nil {
			it := c.item(r, is)
			all = append(all, forge.PR{Repo: r, Number: is.Number, Title: is.Title, State: is.State, Labels: it.Labels,
				URL: c.PRURL(r, is.Number), Author: is.User.Login, CreatedAt: is.CreatedAt})
		}
	})
	return all, err
}

// ListRepos is every repo the implementer App is installed on, from the
// inventory.
func (c *Client) ListRepos(ctx context.Context) ([]forge.RepoRef, error) {
	installs, err := c.inventory(ctx, c.app(c.acting()))
	var out []forge.RepoRef
	for _, in := range installs {
		for _, full := range slices.Sorted(maps.Keys(in.repos)) {
			owner, name, _ := strings.Cut(full, "/")
			out = append(out, c.ref(owner, name))
		}
	}
	return out, err
}

// search runs q, a search API query, in the account of each installation
// in the inventory, and hands fn each hit in a repo the inventory has with
// issues on: a query by account also finds the account's public repos the
// App isn't installed on, and repos.allow/deny may turn some down. The
// requests it makes grow with the installations and the hits, never with
// the repos. GitHub's search index lags behind by up to a minute or so,
// which the next reconcile pass makes up for: webhooks are what devbot
// acts on at once. An installation that fails is skipped, and its error
// joined into the one returned, with an inventory refresh's.
func (c *Client) search(ctx context.Context, q string, fn func(r forge.RepoRef, is issue)) error {
	a := c.app(c.acting())
	installs, err := c.inventory(ctx, a)
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	for _, in := range installs {
		if !in.hasIssues() {
			continue
		}
		tok, _, err := c.installToken(ctx, a, in.installation, searchScope)
		if errors.Is(err, forge.ErrNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("installation %d: %w", in.ID, err))
			continue
		}
		cr := cred{bucket: "installation " + strconv.FormatInt(in.ID, 10), token: tok}
		account := "user:"
		if in.org {
			account = "org:"
		}
		hits, err := c.searchAll(ctx, cr, q+" "+account+in.account, time.Time{}, time.Time{})
		if err != nil {
			errs = append(errs, fmt.Errorf("installation %d: %w", in.ID, err))
		}
		for _, h := range hits {
			_, full, _ := strings.Cut(h.RepositoryURL, "/repos/")
			if rp, ok := in.repos[full]; ok && rp.HasIssues {
				owner, name, _ := strings.Cut(full, "/")
				fn(c.ref(owner, name), h.issue)
			}
		}
	}
	return errors.Join(errs...)
}

// searchHit is an issue or PR as the search API returns it, with its repo.
type searchHit struct {
	issue
	RepositoryURL string `json:"repository_url"`
}

// searchCap is the most results the search API returns of one query.
const searchCap = 1000

// searchEpoch is before anything on GitHub was created: the start of the
// range a query with more than searchCap results is split over.
var searchEpoch = time.Date(2007, 10, 1, 0, 0, 0, 0, time.UTC)

// searchAll is every hit of q among the items created from from to to, or
// with both zero of q itself. A query with more than searchCap results,
// when first read or by the time its last page is, is split in two by when
// its items were created, and each half again until every part fits. More
// than that created in one second can't be split: its first searchCap are
// returned with an error, as is all a search GitHub gave up on part way
// (incomplete_results) found.
func (c *Client) searchAll(ctx context.Context, cr cred, q string, from, to time.Time) ([]searchHit, error) {
	full := q
	if !from.IsZero() {
		full += " created:" + from.Format(time.RFC3339) + ".." + to.Format(time.RFC3339)
	}
	// split searches the two halves of the range instead, unless it is a
	// single second.
	split := func() ([]searchHit, error, bool) {
		if from.IsZero() {
			// A day past now, so nothing is left out for GitHub's clock
			// running ahead of this one.
			from, to = searchEpoch, c.now().UTC().Add(24*time.Hour).Truncate(time.Second)
		}
		if !to.After(from) {
			return nil, nil, false
		}
		mid := from.Add(to.Sub(from) / 2).Truncate(time.Second)
		early, err1 := c.searchAll(ctx, cr, q, from, mid)
		late, err2 := c.searchAll(ctx, cr, q, mid.Add(time.Second), to)
		return append(early, late...), errors.Join(err1, err2), true
	}
	// Oldest first, so what is created while it pages comes last.
	next := "/search/issues?" + url.Values{"q": {full}, "sort": {"created"}, "order": {"asc"}, "per_page": {"100"}}.Encode()
	var hits []searchHit
	incomplete := false
	over := 0 // results in one second, past what can be read
	for page := 0; next != ""; page++ {
		if page == searchCap/100 {
			if hits, err, ok := split(); ok {
				return hits, err
			}
			over = max(over, searchCap+1)
			break
		}
		var res struct {
			TotalCount int         `json:"total_count"`
			Incomplete bool        `json:"incomplete_results"`
			Items      []searchHit `json:"items"`
		}
		h, err := c.do(ctx, cr, "GET", next, nil, &res)
		if err != nil {
			return hits, err
		}
		if res.TotalCount > searchCap && over == 0 {
			if hits, err, ok := split(); ok {
				return hits, err
			}
			over = res.TotalCount
		}
		incomplete = incomplete || res.Incomplete
		hits = append(hits, res.Items...)
		next = nextLink(h)
	}
	if over > 0 {
		return hits, fmt.Errorf("github: search %q has %d results, more than the %d the search API returns, all created in one second", full, over, searchCap)
	}
	if incomplete {
		return hits, fmt.Errorf("github: search %q timed out on GitHub's side, so some of its results may be missing", full)
	}
	return hits, nil
}

// CreateItem files an issue (docs: "Create an issue"). Labels that don't
// exist on the repo yet are created by GitHub with its default colour.
func (c *Client) CreateItem(ctx context.Context, r forge.RepoRef, title, body string, labels []string) (forge.Item, error) {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return forge.Item{}, err
	}
	return c.createItem(ctx, cr, r, title, body, labels)
}

func (c *Client) CloseItem(ctx context.Context, ref forge.ItemRef) error {
	cr, err := c.repoCred(ctx, c.acting(), ref.Repo)
	if err != nil {
		return err
	}
	return c.closeItem(ctx, cr, ref)
}

func (c *Client) EditItem(ctx context.Context, ref forge.ItemRef, title, body string) error {
	cr, err := c.repoCred(ctx, c.acting(), ref.Repo)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, cr, "PATCH", fmt.Sprintf("%s/issues/%d", repoPath(ref.Repo), ref.Number), map[string]any{"title": title, "body": body}, nil)
	return err
}

func (c *Client) Comment(ctx context.Context, ref forge.ItemRef, body string) error {
	cr, err := c.repoCred(ctx, c.acting(), ref.Repo)
	if err != nil {
		return err
	}
	return c.comment(ctx, cr, ref, body)
}

func (c *Client) UpdateComment(ctx context.Context, ref forge.ItemRef, id int64, body string) error {
	cr, err := c.repoCred(ctx, c.acting(), ref.Repo)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, cr, "PATCH", fmt.Sprintf("%s/issues/comments/%d", repoPath(ref.Repo), id), map[string]any{"body": body}, nil)
	return err
}

func (c *Client) SetLabels(ctx context.Context, ref forge.ItemRef, add, remove []string) error {
	cr, err := c.repoCred(ctx, c.acting(), ref.Repo)
	if err != nil {
		return err
	}
	return c.setLabels(ctx, cr, c.app(c.acting()).login, ref, add, remove)
}

// createItem, closeItem, comment and setLabels act as cr: a bot's
// installation or a signed-in person.

func (c *Client) createItem(ctx context.Context, cr cred, r forge.RepoRef, title, body string, labels []string) (forge.Item, error) {
	in := map[string]any{"title": title, "body": body}
	if len(labels) > 0 {
		in["labels"] = labels
	}
	var is issue
	if _, err := c.do(ctx, cr, "POST", repoPath(r)+"/issues", in, &is); err != nil {
		return forge.Item{}, err
	}
	return c.item(r, is), nil
}

func (c *Client) closeItem(ctx context.Context, cr cred, ref forge.ItemRef) error {
	_, err := c.do(ctx, cr, "PATCH", fmt.Sprintf("%s/issues/%d", repoPath(ref.Repo), ref.Number), map[string]any{"state": "closed"}, nil)
	return err
}

func (c *Client) comment(ctx context.Context, cr cred, ref forge.ItemRef, body string) error {
	_, err := c.do(ctx, cr, "POST", fmt.Sprintf("%s/issues/%d/comments", repoPath(ref.Repo), ref.Number), map[string]any{"body": body}, nil)
	return err
}

// setLabels changes the issue's labels, as us, in one call, so a failure
// part way can't leave two state labels on it (docs: "Labels"). Adding
// alone is "Add labels", which leaves the others alone. Anything that
// removes is "Set labels", a replace of the whole set: GitHub has no
// conditional replace, so it drops any label someone added between the
// read and the write, and restoreLost puts those back.
func (c *Client) setLabels(ctx context.Context, cr cred, us string, ref forge.ItemRef, add, remove []string) error {
	base := fmt.Sprintf("%s/issues/%d/labels", repoPath(ref.Repo), ref.Number)
	read := func() (keep []string, have map[string]bool, drop, grow bool, err error) {
		cur, err := list[label](ctx, c, cr, base)
		if err != nil {
			return nil, nil, false, false, err
		}
		keep, have = []string{}, map[string]bool{}
		for _, l := range cur {
			if !slices.Contains(remove, l.Name) && !slices.Contains(add, l.Name) {
				keep = append(keep, l.Name)
			}
			have[l.Name] = true
		}
		for _, r := range remove {
			drop = drop || have[r]
		}
		for _, a := range add {
			grow = grow || !have[a]
		}
		return keep, have, drop, grow, nil
	}
	_, _, drop, grow, err := read()
	if err != nil || (!drop && !grow) {
		return err
	}
	if !drop {
		_, err := c.do(ctx, cr, "POST", base, map[string]any{"labels": add}, nil)
		return err
	}
	// A replace is placed among the issue's events by the last one before
	// a fresh read: Date and created_at are to the second, too coarse to
	// tell a label added after the read from one devbot itself removed
	// in the same second.
	evs, err := c.labelEvents(ctx, cr, ref)
	if err != nil {
		return err
	}
	var mark int64
	for _, e := range evs {
		mark = max(mark, e.id)
	}
	keep, have, drop, grow, err := read()
	if err != nil || (!drop && !grow) {
		return err
	}
	if !drop {
		_, err := c.do(ctx, cr, "POST", base, map[string]any{"labels": add}, nil)
		return err
	}
	if _, err := c.do(ctx, cr, "PUT", base, map[string]any{"labels": append(keep, add...)}, nil); err != nil {
		return err
	}
	return c.restoreLost(ctx, cr, us, ref, mark, have, add, remove)
}

// restoreLost adds back the labels a replace just dropped that weren't on
// the issue when it was read: since mark, an add by anyone, and a removal
// by us that is the label's last event. Only the replace removes as us in
// that time, so the label was added after the read. The add can't drop
// anything, so one is enough.
func (c *Client) restoreLost(ctx context.Context, cr cred, us string, ref forge.ItemRef, mark int64, had map[string]bool, add, remove []string) error {
	evs, err := c.labelEvents(ctx, cr, ref)
	if err != nil {
		return err
	}
	added := map[string]bool{}
	last := map[string]labelEvent{}
	for _, e := range evs {
		if e.id <= mark || had[e.Label] || slices.Contains(add, e.Label) || slices.Contains(remove, e.Label) {
			continue
		}
		added[e.Label] = added[e.Label] || e.Added
		last[e.Label] = e
	}
	var lost []string
	for n, e := range last {
		if added[n] && !e.Added && e.Actor == us {
			lost = append(lost, n)
		}
	}
	if len(lost) == 0 {
		return nil
	}
	slices.Sort(lost)
	_, err = c.do(ctx, cr, "POST", fmt.Sprintf("%s/issues/%d/labels", repoPath(ref.Repo), ref.Number), map[string]any{"labels": lost}, nil)
	return err
}

// EnsureLabels creates each of labels that's missing and repairs the
// colour and description of one that has drifted, leaving every other
// label on the repo alone (docs: "Labels"). Each is read by its name, so
// the requests grow with labels, never with the repo's own.
func (c *Client) EnsureLabels(ctx context.Context, r forge.RepoRef, labels []forge.Label) error {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return err
	}
	type repoLabel struct {
		Name        string `json:"name"`
		Color       string `json:"color"`
		Description string `json:"description"`
	}
	for _, l := range labels {
		color := strings.ToLower(strings.TrimPrefix(l.Color, "#"))
		in := map[string]any{"color": color, "description": l.Description}
		at := repoPath(r) + "/labels/" + url.PathEscape(l.Name)
		var old repoLabel
		_, err := c.do(ctx, cr, "GET", at, nil, &old)
		if errors.Is(err, forge.ErrNotFound) {
			in["name"] = l.Name
			_, err = c.do(ctx, cr, "POST", repoPath(r)+"/labels", in, nil)
			if err == nil {
				continue
			}
			if ae := (*apiError)(nil); !errors.As(err, &ae) || ae.Status != http.StatusUnprocessableEntity {
				return fmt.Errorf("create label %s on %s: %w", l.Name, r, err)
			}
			// Created by someone else since the read (already_exists):
			// read it again, to repair it if need be.
			delete(in, "name")
			_, err = c.do(ctx, cr, "GET", at, nil, &old)
		}
		if err != nil {
			return fmt.Errorf("read label %s on %s: %w", l.Name, r, err)
		}
		if strings.ToLower(old.Color) != color || old.Description != l.Description {
			if _, err := c.do(ctx, cr, "PATCH", at, in, nil); err != nil {
				return fmt.Errorf("repair label %s on %s: %w", l.Name, r, err)
			}
		}
	}
	return nil
}

func (c *Client) LabelEvents(ctx context.Context, ref forge.ItemRef) ([]forge.LabelEvent, error) {
	cr, err := c.repoCred(ctx, c.acting(), ref.Repo)
	if err != nil {
		return nil, err
	}
	evs, err := c.labelEvents(ctx, cr, ref)
	if err != nil {
		return nil, err
	}
	out := make([]forge.LabelEvent, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.LabelEvent)
	}
	return out, nil
}

// labelEvent is a forge.LabelEvent with GitHub's ID for the event.
type labelEvent struct {
	forge.LabelEvent
	id int64
}

// labelEvents reads the issue's labeled and unlabeled events (docs: "List
// issue events"). An App's own are by "<slug>[bot]". The docs don't give
// an order, so they are put in the order of their IDs, which GitHub hands
// out as it records them.
func (c *Client) labelEvents(ctx context.Context, cr cred, ref forge.ItemRef) ([]labelEvent, error) {
	evs, err := c.issueEvents(ctx, cr, ref)
	if err != nil {
		return nil, err
	}
	var out []labelEvent
	for _, e := range evs {
		if (e.Event != "labeled" && e.Event != "unlabeled") || e.Label == nil {
			continue
		}
		out = append(out, labelEvent{forge.LabelEvent{Label: e.Label.Name, Actor: e.Actor.Login, Added: e.Event == "labeled", At: e.CreatedAt}, e.ID})
	}
	return out, nil
}

type issueEvent struct {
	ID        int64     `json:"id"`
	Event     string    `json:"event"`
	Actor     user      `json:"actor"`
	Label     *label    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
}

// issueEvents reads the issue's events (docs: "List issue events"), in the
// order of their IDs.
func (c *Client) issueEvents(ctx context.Context, cr cred, ref forge.ItemRef) ([]issueEvent, error) {
	evs, err := list[issueEvent](ctx, c, cr, fmt.Sprintf("%s/issues/%d/events", repoPath(ref.Repo), ref.Number))
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(evs, func(a, b issueEvent) int { return cmp.Compare(a.ID, b.ID) })
	return evs, nil
}

// ContentEdits are the issue's renamed events, for the title, and its
// userContentEdits over GraphQL, for the body, which REST doesn't have.
func (c *Client) ContentEdits(ctx context.Context, ref forge.ItemRef) ([]forge.ContentEdit, error) {
	cr, err := c.repoCred(ctx, c.acting(), ref.Repo)
	if err != nil {
		return nil, err
	}
	evs, err := c.issueEvents(ctx, cr, ref)
	if err != nil {
		return nil, err
	}
	var out []forge.ContentEdit
	for _, e := range evs {
		if e.Event == "renamed" {
			out = append(out, forge.ContentEdit{Field: forge.FieldTitle, Editor: e.Actor.Login, At: e.CreatedAt})
		}
	}
	body, err := c.bodyEdits(ctx, cr, ref)
	if err != nil {
		return nil, err
	}
	out = append(out, body...)
	slices.SortStableFunc(out, func(a, b forge.ContentEdit) int { return a.At.Compare(b.At) })
	return out, nil
}

const bodyEditsQuery = `query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
      userContentEdits(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes { editedAt editor { __typename login } }
      }
    }
  }
}`

// bodyEdits reads the issue body's edit history (docs: GraphQL
// "UserContentEdit"). GitHub lists it newest first, and its oldest entry,
// the last on the final page, is the body as filed, which isn't an edit. It
// is told by its place, not by a time: GitHub doesn't promise the original
// is stamped with the issue's creation, and an original taken for an edit
// would make the issue's author an editor. A deleted account is no editor,
// so its edits are by someone unknown.
func (c *Client) bodyEdits(ctx context.Context, cr cred, ref forge.ItemRef) ([]forge.ContentEdit, error) {
	type node struct {
		EditedAt time.Time `json:"editedAt"`
		Editor   *struct {
			Typename string `json:"__typename"`
			Login    string `json:"login"`
		} `json:"editor"`
	}
	var nodes []node
	var after any
	complete := false
	for !complete {
		var data struct {
			Repository *struct {
				Issue *struct {
					UserContentEdits struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []node `json:"nodes"`
					} `json:"userContentEdits"`
				} `json:"issue"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": ref.Repo.Owner, "name": ref.Repo.Name, "number": ref.Number, "after": after}
		if err := c.graphql(ctx, cr, bodyEditsQuery, vars, &data); err != nil {
			return nil, err
		}
		if data.Repository == nil || data.Repository.Issue == nil {
			return nil, forge.ErrNotFound
		}
		edits := data.Repository.Issue.UserContentEdits
		nodes = append(nodes, edits.Nodes...)
		complete, after = !edits.PageInfo.HasNextPage, edits.PageInfo.EndCursor
	}
	if len(nodes) > 0 {
		nodes = nodes[:len(nodes)-1]
	}
	var out []forge.ContentEdit
	for _, n := range nodes {
		e := forge.ContentEdit{Field: forge.FieldBody, At: n.EditedAt}
		if n.Editor != nil {
			e.Editor = n.Editor.Login
			// GraphQL names an App by its slug; REST, and BotUsers,
			// by "<slug>[bot]".
			if n.Editor.Typename == "Bot" && !strings.HasSuffix(e.Editor, "[bot]") {
				e.Editor += "[bot]"
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// Package gitea is the forge adapter for Gitea (and Forgejo), over its REST
// API with plain net/http - no SDK, so an SDK's version skew against the
// server can't break it.
package gitea

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
)

type Config struct {
	Name            string // provider name in RepoRefs, e.g. "gitea"
	URL             string // https://gitea.example.com
	ImplementerUser string
	ImplementerTok  string
	ReviewerUser    string
	ReviewerTok     string
	WebhookSecret   string

	// The install's OAuth app, for people signing in to devbot (auth.mode
	// oauth). Empty when this forge isn't the install's identity provider.
	OAuthClientID     string
	OAuthClientSecret string
}

type Client struct {
	cfg  Config
	http *http.Client

	mu     sync.Mutex
	labels map[string]map[string]int64 // repo full name -> label name -> id
	// recheck is how long GetPR waits before reading a PR Gitea called
	// unmergeable again; tests shorten it.
	recheck time.Duration
}

func New(cfg Config) *Client {
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	return &Client{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}, labels: map[string]map[string]int64{}, recheck: 2 * time.Second}
}

func (c *Client) Name() string               { return c.cfg.Name }
func (c *Client) BotUsers() (string, string) { return c.cfg.ImplementerUser, c.cfg.ReviewerUser }
func (c *Client) login(role forge.Role) string {
	if role == forge.RoleReviewer {
		return c.cfg.ReviewerUser
	}
	return c.cfg.ImplementerUser
}
func (c *Client) ref(o, n string) forge.RepoRef {
	return forge.RepoRef{Provider: c.cfg.Name, Owner: o, Name: n}
}
func repoPath(r forge.RepoRef) string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

// token is role's token, or "" in a process that doesn't hold it (a job
// pod of the other role), which send refuses with forge.ErrNoCredential.
func (c *Client) token(role forge.Role) string {
	if role == forge.RoleReviewer {
		return c.cfg.ReviewerTok
	}
	return c.cfg.ImplementerTok
}

// acting is the role of the calls that don't take one: the implementer,
// or the reviewer in a process that holds only the reviewer's token.
func (c *Client) acting() forge.Role {
	if c.cfg.ImplementerTok == "" {
		return forge.RoleReviewer
	}
	return forge.RoleImplementer
}

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("gitea: HTTP %d: %s", e.Status, e.Body) }

// Is lets callers test for the forge's refusals without knowing Gitea's shapes.
func (e *apiError) Is(target error) bool {
	return (target == forge.ErrForbidden && e.Status == http.StatusForbidden) ||
		(target == forge.ErrUnauthorized && e.Status == http.StatusUnauthorized)
}

func (c *Client) do(ctx context.Context, role forge.Role, method, path string, in, out any) error {
	return c.doToken(ctx, c.token(role), method, path, in, out)
}

// doToken calls the API as whoever tok belongs to: a bot, or a signed-in person.
func (c *Client) doToken(ctx context.Context, tok, method, path string, in, out any) error {
	_, err := c.send(ctx, tok, method, path, in, out)
	return err
}

// send is doToken that also gives back the response headers.
func (c *Client) send(ctx context.Context, tok, method, path string, in, out any) (http.Header, error) {
	if tok == "" {
		return nil, fmt.Errorf("gitea: %w", forge.ErrNoCredential)
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.URL+"/api/v1"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+tok)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode == http.StatusNotFound {
		return resp.Header, forge.ErrNotFound
	}
	if resp.StatusCode >= 300 {
		return resp.Header, &apiError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.Header, fmt.Errorf("gitea: decode %s %s: %w", method, path, err)
		}
	}
	return resp.Header, nil
}

// download GETs path as whoever tok belongs to and hands the body to read
// as it streams, for a download too big to hold (a job's log).
func (c *Client) download(ctx context.Context, tok, path string, read func(io.Reader) error) error {
	if tok == "" {
		return fmt.Errorf("gitea: %w", forge.ErrNoCredential)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.cfg.URL+"/api/v1"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+tok)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return forge.ErrNotFound
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &apiError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	return read(resp.Body)
}

// pageLimit is the page size devbot asks for. The server caps it at its
// MAX_RESPONSE_ITEMS, which may be lower, so a page shorter than this isn't
// the end.
const pageLimit = 50

// list reads every page of a Gitea list at path, handing each item to fn,
// however many there are: a partial list could leave out the very event,
// label or review a decision turns on. It stops once it has seen the
// X-Total-Count Gitea sends, or at a page whose Link header has no
// rel="next". A missing Link isn't the end: some lists send only the total,
// and a single page sends no Link at all.
func list[T any](ctx context.Context, c *Client, tok, path string, fn func(T)) error {
	return pages(ctx, c, tok, path, true, fn)
}

// listTimeline reads every page of an issue's timeline. Gitea sends each
// timeline page's own length as X-Total-Count, so only an empty page ends
// it: Gitea cuts the timeline into pages before dropping what the token
// can't see (code comments, cross-references from unreadable repos), so a
// short page can come before ones that still hold events.
func listTimeline[T any](ctx context.Context, c *Client, tok, path string, fn func(T)) error {
	return pages(ctx, c, tok, path, false, fn)
}

func pages[T any](ctx context.Context, c *Client, tok, path string, trustTotal bool, fn func(T)) error {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	seen := 0
	var prev []json.RawMessage
	for page := 1; ; page++ {
		var batch []json.RawMessage
		h, err := c.send(ctx, tok, "GET", fmt.Sprintf("%s%slimit=%d&page=%d", path, sep, pageLimit, page), nil, &batch)
		if err != nil {
			return err
		}
		// An endpoint that ignores page (as issue comments do) serves the
		// whole list every time: a page that repeats the last one is the
		// end, not more items.
		if len(batch) == 0 || slices.EqualFunc(batch, prev, func(a, b json.RawMessage) bool { return bytes.Equal(a, b) }) {
			return nil
		}
		for _, raw := range batch {
			var x T
			if err := json.Unmarshal(raw, &x); err != nil {
				return fmt.Errorf("gitea: decode GET %s: %w", path, err)
			}
			fn(x)
		}
		seen += len(batch)
		prev = batch
		if !trustTotal {
			continue
		}
		if total, terr := strconv.Atoi(h.Get("X-Total-Count")); terr == nil && seen >= total {
			return nil
		}
		if link := h.Get("Link"); link != "" && !hasNext(link) {
			return nil
		}
	}
}

// hasNext reports whether a Link header has a rel="next" entry.
func hasNext(link string) bool {
	for part := range strings.SplitSeq(link, ",") {
		if strings.Contains(part, `rel="next"`) {
			return true
		}
	}
	return false
}

// ---- API shapes (only the fields devbot reads) ----

type user struct {
	Login string `json:"login"`
}

type repository struct {
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Owner         user   `json:"owner"`
	DefaultBranch string `json:"default_branch"`
	CloneURL      string `json:"clone_url"`
}

type label struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type issue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	Labels      []label   `json:"labels"`
	User        user      `json:"user"`
	HTMLURL     string    `json:"html_url"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	PullRequest *struct{} `json:"pull_request"`
	// An issue embeds a short repository object whose owner is a plain
	// string, unlike the full repository in webhooks.
	Repository *struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
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

type comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      user      `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type branchRef struct {
	Ref  string    `json:"ref"`
	SHA  string    `json:"sha"`
	Repo *repoName `json:"repo"`
}

type repoName struct {
	FullName string `json:"full_name"`
}

// fork reports whether a PR's head is outside r.
func (b branchRef) fork(r forge.RepoRef) bool {
	return b.Repo == nil || !strings.EqualFold(b.Repo.FullName, r.Owner+"/"+r.Name)
}

// repo is the repo the branch is in, on r's forge, or zero for a deleted
// fork. The forge's clone URL for it is left alone: it may not be the
// address devbot is configured to reach the forge at.
func (b branchRef) repo(r forge.RepoRef) forge.RepoRef {
	if b.Repo == nil {
		return forge.RepoRef{}
	}
	owner, name, ok := strings.Cut(b.Repo.FullName, "/")
	if !ok {
		return forge.RepoRef{}
	}
	return forge.RepoRef{Provider: r.Provider, Owner: owner, Name: name}
}

type pull struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"`
	Merged    bool       `json:"merged"`
	Mergeable bool       `json:"mergeable"`
	MergeBase string     `json:"merge_base"`
	Head      branchRef  `json:"head"`
	Base      branchRef  `json:"base"`
	HTMLURL   string     `json:"html_url"`
	User      user       `json:"user"`
	CreatedAt time.Time  `json:"created_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	Labels    []label    `json:"labels"`

	// AllowMaintainerEdit is a fork PR's "Allow edits from maintainers".
	AllowMaintainerEdit bool `json:"allow_maintainer_edit"`
	// MergeCommitID is the commit a merged PR left on its base.
	MergeCommitID string `json:"merge_commit_sha"`
}

func (c *Client) pr(r forge.RepoRef, p pull) forge.PR {
	out := forge.PR{
		Repo: r, Number: p.Number, Title: p.Title, Body: p.Body, State: p.State, Merged: p.Merged,
		HeadBranch: p.Head.Ref, HeadSHA: p.Head.SHA, BaseBranch: p.Base.Ref, Fork: p.Head.fork(r), Mergeable: true,
		HeadRepo: p.Head.repo(r), MaintainerCanModify: p.AllowMaintainerEdit,
		URL: p.HTMLURL, Author: p.User.Login, CreatedAt: p.CreatedAt,
	}
	if p.Merged {
		out.MergeSHA = p.MergeCommitID
	}
	if p.ClosedAt != nil {
		out.ClosedAt = *p.ClosedAt
	}
	for _, l := range p.Labels {
		out.Labels = append(out.Labels, l.Name)
	}
	// Gitea's mergeable is false for a conflict, but also while it is still
	// checking the PR, when the check failed, and whenever the title marks
	// it a work in progress. A WIP title says nothing about conflicts (Gitea
	// refuses the merge itself, with its own reason); any other false is not
	// known to be a conflict until GetPR reads it twice.
	if p.State == "open" && !p.Mergeable && !wip(p.Title) {
		out.MergeableUnknown = true
	}
	return out
}

// wip reports whether a PR's title starts with one of Gitea's default
// work-in-progress prefixes, which it compares ignoring case.
func wip(title string) bool {
	for _, prefix := range []string{"WIP:", "[WIP]"} {
		if len(title) >= len(prefix) && strings.EqualFold(title[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

// ---- items ----

func (c *Client) SearchItems(ctx context.Context, lbl, state string) ([]forge.Item, error) {
	var all []forge.Item
	q := url.Values{"type": {"issues"}, "state": {state}, "labels": {lbl}}
	err := list(ctx, c, c.token(c.acting()), "/repos/issues/search?"+q.Encode(), func(is issue) {
		if is.Repository == nil || is.PullRequest != nil {
			return
		}
		owner, name, _ := strings.Cut(is.Repository.FullName, "/")
		all = append(all, c.item(c.ref(owner, name), is))
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

// ListItems lists r's issues carrying lbl.
func (c *Client) ListItems(ctx context.Context, r forge.RepoRef, lbl, state string) ([]forge.Item, error) {
	var all []forge.Item
	q := url.Values{"type": {"issues"}, "state": {state}, "labels": {lbl}}
	err := list(ctx, c, c.token(c.acting()), repoPath(r)+"/issues?"+q.Encode(), func(is issue) {
		if is.PullRequest == nil {
			all = append(all, c.item(r, is))
		}
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

// SearchPRs is SearchItems's search for open pull requests, which Gitea
// returns as issues with pull_request set.
func (c *Client) SearchPRs(ctx context.Context, lbl string) ([]forge.PR, error) {
	var all []forge.PR
	q := url.Values{"type": {"pulls"}, "state": {"open"}, "labels": {lbl}}
	err := list(ctx, c, c.token(c.acting()), "/repos/issues/search?"+q.Encode(), func(is issue) {
		if is.Repository == nil || is.PullRequest == nil {
			return
		}
		owner, name, _ := strings.Cut(is.Repository.FullName, "/")
		it := c.item(c.ref(owner, name), is)
		all = append(all, forge.PR{Repo: it.Ref.Repo, Number: is.Number, Title: is.Title, State: is.State, Labels: it.Labels,
			URL: c.PRURL(it.Ref.Repo, is.Number), Author: is.User.Login, CreatedAt: is.CreatedAt})
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

func (c *Client) CreateItem(ctx context.Context, r forge.RepoRef, title, body string, labels []string) (forge.Item, error) {
	return c.createItem(ctx, c.token(c.acting()), r, title, body, labels)
}

func (c *Client) createItem(ctx context.Context, tok string, r forge.RepoRef, title, body string, labels []string) (forge.Item, error) {
	// An issue filed with a stale label ID is still filed: the retry puts
	// the labels on it rather than filing it again.
	var is issue
	err := c.withLabelIDs(ctx, r, labels, func(ids []int64) ([]label, error) {
		if is.Number != 0 {
			var got []label
			err := c.doToken(ctx, tok, "POST", fmt.Sprintf("%s/issues/%d/labels", repoPath(r), is.Number), map[string]any{"labels": ids}, &got)
			is.Labels = got
			return got, err
		}
		err := c.doToken(ctx, tok, "POST", repoPath(r)+"/issues",
			map[string]any{"title": title, "body": body, "labels": ids}, &is)
		return is.Labels, err
	})
	if err != nil && is.Number == 0 {
		return forge.Item{}, err
	}
	// Filed, whatever became of its labels: the caller sees which it has.
	return c.item(r, is), nil
}

func (c *Client) GetItem(ctx context.Context, ref forge.ItemRef) (forge.Item, error) {
	var is issue
	if err := c.do(ctx, c.acting(), "GET", fmt.Sprintf("%s/issues/%d", repoPath(ref.Repo), ref.Number), nil, &is); err != nil {
		return forge.Item{}, err
	}
	if is.PullRequest != nil {
		return forge.Item{}, fmt.Errorf("%s is a pull request, not an issue: %w", ref, forge.ErrNotFound)
	}
	return c.item(ref.Repo, is), nil
}

func (c *Client) CloseItem(ctx context.Context, ref forge.ItemRef) error {
	return c.closeItem(ctx, c.token(c.acting()), ref)
}

func (c *Client) closeItem(ctx context.Context, tok string, ref forge.ItemRef) error {
	return c.doToken(ctx, tok, "PATCH", fmt.Sprintf("%s/issues/%d", repoPath(ref.Repo), ref.Number),
		map[string]any{"state": "closed"}, nil)
}

func (c *Client) EditItem(ctx context.Context, ref forge.ItemRef, title, body string) error {
	return c.do(ctx, c.acting(), "PATCH", fmt.Sprintf("%s/issues/%d", repoPath(ref.Repo), ref.Number),
		map[string]any{"title": title, "body": body}, nil)
}

func (c *Client) ListComments(ctx context.Context, ref forge.ItemRef) ([]forge.Comment, error) {
	// Gitea's issue comments take only since and before: page and limit are
	// ignored and every comment comes in one response, so there's no pager.
	var cms []comment
	if _, err := c.send(ctx, c.token(c.acting()), "GET", fmt.Sprintf("%s/issues/%d/comments", repoPath(ref.Repo), ref.Number), nil, &cms); err != nil {
		return nil, err
	}
	var out []forge.Comment
	for _, cm := range cms {
		out = append(out, forge.Comment{ID: cm.ID, Author: cm.User.Login, Body: cm.Body, CreatedAt: cm.CreatedAt, UpdatedAt: cm.UpdatedAt})
	}
	return out, nil
}

func (c *Client) Comment(ctx context.Context, ref forge.ItemRef, body string) error {
	return c.comment(ctx, c.token(c.acting()), ref, body)
}

func (c *Client) UpdateComment(ctx context.Context, ref forge.ItemRef, id int64, body string) error {
	return c.doToken(ctx, c.token(c.acting()), "PATCH", fmt.Sprintf("%s/issues/comments/%d", repoPath(ref.Repo), id),
		map[string]any{"body": body}, nil)
}

func (c *Client) comment(ctx context.Context, tok string, ref forge.ItemRef, body string) error {
	return c.doToken(ctx, tok, "POST", fmt.Sprintf("%s/issues/%d/comments", repoPath(ref.Repo), ref.Number),
		map[string]any{"body": body}, nil)
}

// withLabelIDs calls fn with the IDs of names, and fn returns the labels
// the issue then has. A label deleted and recreated since it was cached
// has a new ID, which the forge refuses (404 or 422) or quietly leaves
// off; either way names are read again and fn tried once more.
func (c *Client) withLabelIDs(ctx context.Context, r forge.RepoRef, names []string, fn func(ids []int64) ([]label, error)) error {
	apply := func(ids []int64) error {
		got, err := fn(ids)
		if err != nil {
			return err
		}
		for _, n := range names {
			if !slices.ContainsFunc(got, func(l label) bool { return l.Name == n }) {
				return fmt.Errorf("gitea: label %s on %s wasn't applied: %w", n, r, forge.ErrNotFound)
			}
		}
		return nil
	}
	ids, err := c.labelIDs(ctx, r, names)
	if err != nil {
		return err
	}
	err = apply(ids)
	var ae *apiError
	if len(names) == 0 || !(errors.Is(err, forge.ErrNotFound) || errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity) {
		return err
	}
	c.forgetLabels(r, names)
	if ids, err = c.labelIDs(ctx, r, names); err != nil {
		return err
	}
	return apply(ids)
}

// labelIDs maps names to IDs, reading each the cache doesn't have by its
// name, so what it costs grows with names, never with the repo's labels.
// Gitea's issue-label endpoints take IDs (names only from 1.22 on).
func (c *Client) labelIDs(ctx context.Context, r forge.RepoRef, names []string) ([]int64, error) {
	if len(names) == 0 {
		return nil, nil
	}
	ids, missing := c.cachedIDs(r, names)
	if len(missing) == 0 {
		return ids, nil
	}
	var gone []string
	for _, n := range missing {
		l, err := c.readLabel(ctx, r, n)
		if errors.Is(err, forge.ErrNotFound) {
			gone = append(gone, n)
			continue
		}
		if err != nil {
			return nil, err
		}
		c.cacheLabel(r, l)
	}
	if len(gone) > 0 {
		return nil, fmt.Errorf("gitea: labels %v don't exist on %s: %w", gone, r, forge.ErrNotFound)
	}
	ids, _ = c.cachedIDs(r, names)
	return ids, nil
}

// readLabel reads r's label called name. Gitea takes a number in its place
// for an ID, so a name that is one is looked for in the repo's whole list
// instead: only someone else's label is named so, never devbot's.
func (c *Client) readLabel(ctx context.Context, r forge.RepoRef, name string) (label, error) {
	if _, err := strconv.ParseInt(name, 10, 64); err == nil {
		var found []label
		if err := list(ctx, c, c.token(c.acting()), repoPath(r)+"/labels", func(l label) {
			if l.Name == name {
				found = append(found, l)
			}
		}); err != nil {
			return label{}, err
		}
		if len(found) == 0 {
			return label{}, forge.ErrNotFound
		}
		return found[0], nil
	}
	var l label
	if err := c.do(ctx, c.acting(), "GET", repoPath(r)+"/labels/"+url.PathEscape(name), nil, &l); err != nil {
		return label{}, err
	}
	// A database that compares names ignoring case finds a label by
	// another name, which the issue-label endpoints would then report.
	if l.Name != name {
		return label{}, forge.ErrNotFound
	}
	return l, nil
}

func (c *Client) cachedIDs(r forge.RepoRef, names []string) (ids []int64, missing []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.labels[r.FullName()]
	for _, n := range names {
		if id, ok := m[n]; ok {
			ids = append(ids, id)
		} else {
			missing = append(missing, n)
		}
	}
	return ids, missing
}

func (c *Client) cacheLabel(r forge.RepoRef, l label) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.labels[r.FullName()] == nil {
		c.labels[r.FullName()] = map[string]int64{}
	}
	c.labels[r.FullName()][l.Name] = l.ID
}

func (c *Client) forgetLabels(r forge.RepoRef, names []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range names {
		delete(c.labels[r.FullName()], n)
	}
}

func (c *Client) SetLabels(ctx context.Context, ref forge.ItemRef, add, remove []string) error {
	return c.setLabels(ctx, c.token(c.acting()), ref, add, remove)
}

// setLabels changes the issue's labels in one call, so a failure part way
// can't leave two state labels on it. Gitea has no conditional replace, so
// a replace drops any label someone added between the read and the write;
// restoreLost puts those back.
func (c *Client) setLabels(ctx context.Context, tok string, ref forge.ItemRef, add, remove []string) error {
	base := fmt.Sprintf("%s/issues/%d/labels", repoPath(ref.Repo), ref.Number)
	var cur []label
	h, err := c.send(ctx, tok, "GET", base, nil, &cur)
	if err != nil {
		return err
	}
	var ids []int64
	have := map[string]bool{}
	for _, l := range cur {
		if !slices.Contains(remove, l.Name) && !slices.Contains(add, l.Name) {
			ids = append(ids, l.ID)
		}
		have[l.Name] = true
	}
	drop, grow := false, false
	for _, r := range remove {
		drop = drop || have[r]
	}
	for _, a := range add {
		grow = grow || !have[a]
	}
	if !drop && !grow {
		return nil
	}
	err = c.withLabelIDs(ctx, ref.Repo, add, func(addIDs []int64) ([]label, error) {
		// Gitea's add leaves the other labels alone, so nothing can be lost.
		method, want := "POST", addIDs
		if drop {
			method, want = "PUT", append(slices.Clip(ids), addIDs...)
		}
		var got []label
		err := c.doToken(ctx, tok, method, base, map[string]any{"labels": want}, &got)
		return got, err
	})
	if err != nil || !drop {
		return err
	}
	read, err := http.ParseTime(h.Get("Date"))
	if err != nil {
		// Without the server's time of the read there is no telling a
		// label added since from one removed long ago.
		return nil
	}
	return c.restoreLost(ctx, tok, ref, read, have, add, remove)
}

// restoreLost adds back the labels a replace just dropped that weren't on
// the issue when it was read: in the timeline since the read, an add by
// anyone followed by a removal by whoever made the replace. That is known
// from the removal of a label it was asked to remove. The add can't drop
// anything, so one is enough.
func (c *Client) restoreLost(ctx context.Context, tok string, ref forge.ItemRef, read time.Time, had map[string]bool, add, remove []string) error {
	evs, err := c.labelEvents(ctx, tok, ref, read)
	if err != nil {
		return err
	}
	us := ""
	for _, e := range evs {
		if !e.Added && had[e.Label] && slices.Contains(remove, e.Label) {
			us = e.Actor
		}
	}
	added := map[string]bool{}
	last := map[string]forge.LabelEvent{}
	for _, e := range evs {
		if had[e.Label] || slices.Contains(add, e.Label) || slices.Contains(remove, e.Label) {
			continue
		}
		added[e.Label] = added[e.Label] || e.Added
		last[e.Label] = e
	}
	var lost []string
	for n, e := range last {
		if us != "" && added[n] && !e.Added && e.Actor == us {
			lost = append(lost, n)
		}
	}
	if len(lost) == 0 {
		return nil
	}
	slices.Sort(lost)
	base := fmt.Sprintf("%s/issues/%d/labels", repoPath(ref.Repo), ref.Number)
	return c.withLabelIDs(ctx, ref.Repo, lost, func(ids []int64) ([]label, error) {
		var got []label
		err := c.doToken(ctx, tok, "POST", base, map[string]any{"labels": ids}, &got)
		return got, err
	})
}

// EnsureLabels creates each of labels that's missing and repairs the
// colour and description of one that has drifted, leaving every other
// label on the repo alone. Each is read by its name, so the requests grow
// with labels, never with the repo's own.
func (c *Client) EnsureLabels(ctx context.Context, r forge.RepoRef, labels []forge.Label) error {
	for _, l := range labels {
		color := strings.ToLower(strings.TrimPrefix(l.Color, "#"))
		in := map[string]any{"color": "#" + color, "description": l.Description}
		old, err := c.readLabel(ctx, r, l.Name)
		switch {
		case errors.Is(err, forge.ErrNotFound):
			in["name"] = l.Name
			if err := c.do(ctx, c.acting(), "POST", repoPath(r)+"/labels", in, &old); err != nil {
				return fmt.Errorf("create label %s on %s: %w", l.Name, r, err)
			}
		case err != nil:
			return fmt.Errorf("read label %s on %s: %w", l.Name, r, err)
		case strings.ToLower(strings.TrimPrefix(old.Color, "#")) != color || old.Description != l.Description:
			if err := c.do(ctx, c.acting(), "PATCH", fmt.Sprintf("%s/labels/%d", repoPath(r), old.ID), in, &old); err != nil {
				return fmt.Errorf("repair label %s on %s: %w", l.Name, r, err)
			}
		}
		c.cacheLabel(r, old)
	}
	return nil
}

func (c *Client) LabelEvents(ctx context.Context, ref forge.ItemRef) ([]forge.LabelEvent, error) {
	return c.labelEvents(ctx, c.token(c.acting()), ref, time.Time{})
}

// ContentEdits are the title changes in the issue's timeline. Gitea's API
// doesn't say who edited a body (only its web UI shows that history), so
// body edits are missing, which devbot takes as made by someone unknown.
func (c *Client) ContentEdits(ctx context.Context, ref forge.ItemRef) ([]forge.ContentEdit, error) {
	evs, err := c.timeline(ctx, c.token(c.acting()), ref, time.Time{})
	if err != nil {
		return nil, err
	}
	var out []forge.ContentEdit
	for _, ev := range evs {
		if ev.Type == "change_title" {
			out = append(out, forge.ContentEdit{Field: forge.FieldTitle, Editor: ev.User.Login, At: ev.CreatedAt})
		}
	}
	return out, nil
}

// labelEvents reads the issue's label events, oldest first, from since on
// (to the second, as Gitea keeps them) or from the start when it is zero.
func (c *Client) labelEvents(ctx context.Context, tok string, ref forge.ItemRef, since time.Time) ([]forge.LabelEvent, error) {
	evs, err := c.timeline(ctx, tok, ref, since)
	if err != nil {
		return nil, err
	}
	var out []forge.LabelEvent
	for _, ev := range evs {
		if ev.Type != "label" || ev.Label == nil {
			continue
		}
		// Gitea stores a label comment's content as "1" for an add and
		// empty for a removal.
		out = append(out, forge.LabelEvent{Label: ev.Label.Name, Actor: ev.User.Login, Added: ev.Body == "1", At: ev.CreatedAt})
	}
	return out, nil
}

type timelineEvent struct {
	Type      string    `json:"type"`
	Body      string    `json:"body"`
	User      user      `json:"user"`
	Label     *label    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
}

// timeline reads the issue's timeline, oldest first, from since on or from
// the start when it is zero.
func (c *Client) timeline(ctx context.Context, tok string, ref forge.ItemRef, since time.Time) ([]timelineEvent, error) {
	q := ""
	if !since.IsZero() {
		q = "?since=" + since.UTC().Format(time.RFC3339)
	}
	var out []timelineEvent
	p := fmt.Sprintf("%s/issues/%d/timeline%s", repoPath(ref.Repo), ref.Number, q)
	err := listTimeline(ctx, c, tok, p, func(ev timelineEvent) { out = append(out, ev) })
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- pull requests ----

func (c *Client) OpenPR(ctx context.Context, r forge.RepoRef, head, base, title, body string) (forge.PR, error) {
	var p pull
	err := c.do(ctx, c.acting(), "POST", repoPath(r)+"/pulls",
		map[string]any{"head": head, "base": base, "title": title, "body": body}, &p)
	if err != nil {
		return forge.PR{}, err
	}
	return c.pr(r, p), nil
}

func (c *Client) UpdatePR(ctx context.Context, r forge.RepoRef, number int, title, body string) error {
	return c.do(ctx, c.acting(), "PATCH", fmt.Sprintf("%s/pulls/%d", repoPath(r), number),
		map[string]any{"title": title, "body": body}, nil)
}

func (c *Client) SetPRBase(ctx context.Context, r forge.RepoRef, number int, base string) error {
	return c.do(ctx, forge.RoleImplementer, "PATCH", fmt.Sprintf("%s/pulls/%d", repoPath(r), number),
		map[string]any{"base": base}, nil)
}

func (c *Client) GetPR(ctx context.Context, r forge.RepoRef, number int) (forge.PR, error) {
	path := fmt.Sprintf("%s/pulls/%d", repoPath(r), number)
	var p pull
	if err := c.do(ctx, c.acting(), "GET", path, nil, &p); err != nil {
		return forge.PR{}, err
	}
	out := c.pr(r, p)
	if p.State == "open" && out.MergeableUnknown {
		// A false read just after a push to the head or the base is most
		// likely Gitea still checking the PR: a moment later it has the
		// answer, and a false then is a conflict (or a check that keeps
		// failing, which a rebase is as likely as anything to clear). A
		// second read that fails, or that finds a new head, whose check has
		// only started, leaves it unknown for a later evaluation.
		if err := sleep(ctx, c.recheck); err != nil {
			return forge.PR{}, err
		}
		var again pull
		if err := c.do(ctx, c.acting(), "GET", path, nil, &again); err == nil {
			moved := again.Head.SHA != p.Head.SHA
			p, out = again, c.pr(r, again)
			if out.MergeableUnknown && !moved {
				out.Mergeable, out.MergeableUnknown = false, false
			}
		}
	}
	if p.State == "open" {
		var b struct {
			Commit struct {
				ID string `json:"id"`
			} `json:"commit"`
		}
		// No merge base onto a base branch that is there is a head that
		// shares no commit with it: a base force-pushed to a history of its
		// own. Gitea still calls it mergeable, and merging it would bring
		// the old history back as a second root.
		if err := c.do(ctx, c.acting(), "GET", repoPath(r)+"/branches/"+url.PathEscape(p.Base.Ref), nil, &b); err == nil && b.Commit.ID != "" {
			out.Unrelated = p.MergeBase == ""
			out.Behind = b.Commit.ID != p.MergeBase
		}
	}
	return out, nil
}

func (c *Client) ListOpenPRs(ctx context.Context, r forge.RepoRef) ([]forge.PR, error) {
	var out []forge.PR
	if err := list(ctx, c, c.token(c.acting()), repoPath(r)+"/pulls?state=open", func(p pull) { out = append(out, c.pr(r, p)) }); err != nil {
		return nil, err
	}
	return out, nil
}

// SetCommitStatus posts a status on sha. Docs: "Create a commit status"
// (POST /repos/{owner}/{repo}/statuses/{sha}).
func (c *Client) SetCommitStatus(ctx context.Context, r forge.RepoRef, sha string, s forge.CommitStatus) error {
	return c.doToken(ctx, c.token(c.acting()), "POST", repoPath(r)+"/statuses/"+url.PathEscape(sha),
		statusBody(s), nil)
}

func (c *Client) CombinedChecks(ctx context.Context, r forge.RepoRef, sha string) (forge.CheckState, []forge.CheckDetail, error) {
	// The combined status is paged like a list (30 contexts by default), and
	// its own state and total_count cover only the page, so every page is
	// read, however many, as list does: a check left out could be the one
	// that failed. The state is worked out here.
	var details []forge.CheckDetail
	for page := 1; ; page++ {
		var st struct {
			Statuses []struct {
				Context     string `json:"context"`
				Status      string `json:"status"`
				Description string `json:"description"`
				TargetURL   string `json:"target_url"`
			} `json:"statuses"`
		}
		p := fmt.Sprintf("%s/commits/%s/status?limit=%d&page=%d", repoPath(r), url.PathEscape(sha), pageLimit, page)
		h, err := c.send(ctx, c.token(c.acting()), "GET", p, nil, &st)
		if err != nil {
			return "", nil, err
		}
		for _, s := range st.Statuses {
			details = append(details, forge.CheckDetail{Context: s.Context, State: s.Status, Description: s.Description, URL: s.TargetURL,
				Cancelled: cancelled(s.Status, s.Description, s.TargetURL, r)})
		}
		total, terr := strconv.Atoi(h.Get("X-Total-Count"))
		if len(st.Statuses) == 0 || (terr == nil && len(details) >= total) {
			break
		}
	}
	pending, failed := false, false
	for i, s := range details {
		switch s.State {
		case "pending", "running", "waiting", "queued":
			pending, details[i].Pending = true, true
		case "failure", "error", "cancelled", "skipped_failure":
			failed, details[i].Failed = true, true
		}
	}
	switch {
	case len(details) == 0:
		return forge.ChecksNone, nil, nil
	case pending:
		// A failure that's already in is still reported only once every check
		// has finished, so the implementer gets the whole picture in one go.
		return forge.ChecksPending, details, nil
	case failed:
		return forge.ChecksFailure, details, nil
	}
	return forge.ChecksSuccess, details, nil
}

// actionsCancelled matches the description Gitea Actions gives a job's
// status once the job is cancelled: "Canceled after 0s", or "Has been
// cancelled" in older versions.
var actionsCancelled = regexp.MustCompile(`(?i)^(cancell?ed\b|has been cancell?ed)`)

// cancelled reports whether a status is an Actions job of r that was
// cancelled. Gitea reports those as failures; other CI's descriptions are
// its own, so only a status linking to one of r's runs is read this way.
func cancelled(state, description, target string, r forge.RepoRef) bool {
	if state != "failure" && state != "error" {
		return false
	}
	_, ok := actionsRun(target, r)
	return ok && actionsCancelled.MatchString(description)
}

// RerunChecks re-runs the failed and cancelled jobs of each Actions run
// behind checks, as the implementer. A job's status links to the job under
// its run, by the run's ID or, in older versions, its number in the repo, so
// the run is found among those on sha by the same link.
func (c *Client) RerunChecks(ctx context.Context, r forge.RepoRef, sha string, checks []forge.CheckDetail) error {
	var want []string
	for _, d := range checks {
		n, ok := actionsRun(d.URL, r)
		if !ok {
			return fmt.Errorf("gitea: check %q: %w", d.Context, forge.ErrNotRerunnable)
		}
		if !slices.Contains(want, n) {
			want = append(want, n)
		}
	}
	ids, err := c.runIDs(ctx, forge.RoleImplementer, r, sha, want)
	if errors.Is(err, forge.ErrNotFound) {
		return fmt.Errorf("gitea: this Gitea has no API to re-run Actions runs: %w", forge.ErrNotRerunnable)
	} else if err != nil {
		return err
	}
	for _, n := range want {
		id, ok := ids[n]
		if !ok {
			return fmt.Errorf("gitea: Actions run %s isn't among those on %s: %w", n, sha, forge.ErrNotRerunnable)
		}
		if err := c.do(ctx, forge.RoleImplementer, "POST", fmt.Sprintf("%s/actions/runs/%d/rerun-failed-jobs", repoPath(r), id), nil, nil); err != nil {
			return fmt.Errorf("gitea: re-running Actions run %d: %w", id, err)
		}
	}
	return nil
}

// runIDs finds the IDs of the Actions runs on sha that want names by the
// run in their links (see actionsRun), as role. A Gitea without the API to
// list runs is ErrNotFound.
func (c *Client) runIDs(ctx context.Context, role forge.Role, r forge.RepoRef, sha string, want []string) (map[string]int64, error) {
	ids := map[string]int64{}
	for page, seen := 1, 0; len(ids) < len(want); page++ {
		var out struct {
			Total int64 `json:"total_count"`
			Runs  []struct {
				ID      int64  `json:"id"`
				HTMLURL string `json:"html_url"`
			} `json:"workflow_runs"`
		}
		p := fmt.Sprintf("%s/actions/runs?head_sha=%s&limit=%d&page=%d", repoPath(r), url.QueryEscape(sha), pageLimit, page)
		if err := c.do(ctx, role, "GET", p, nil, &out); err != nil {
			return nil, err
		}
		for _, run := range out.Runs {
			if n, ok := actionsRun(run.HTMLURL, r); ok && slices.Contains(want, n) {
				ids[n] = run.ID
			}
		}
		seen += len(out.Runs)
		if len(out.Runs) == 0 || int64(seen) >= out.Total {
			break
		}
	}
	return ids, nil
}

// CheckReport reads the end of an Actions job's log. Gitea links a job's
// status to the job by its place in its run, so the job is found among the
// run's jobs by that link, and its log read by the job's ID (Gitea 1.24's
// /actions/runs/{run}/jobs and /actions/jobs/{job_id}/logs; an older Gitea
// has neither, which is ErrNotFound). Statuses are all Gitea has, so
// another CI's has nothing more to read here.
func (c *Client) CheckReport(ctx context.Context, r forge.RepoRef, sha string, d forge.CheckDetail, logLines int) (forge.CheckReport, error) {
	n, ok := actionsRun(d.URL, r)
	if !ok {
		return forge.CheckReport{}, nil
	}
	out := forge.CheckReport{HasLog: true}
	role := c.acting()
	ids, err := c.runIDs(ctx, role, r, sha, []string{n})
	if errors.Is(err, forge.ErrNotFound) {
		return out, fmt.Errorf("gitea: this Gitea has no API to read Actions logs (it came in 1.24): %w", err)
	} else if err != nil {
		return out, err
	}
	run, ok := ids[n]
	if !ok {
		return out, fmt.Errorf("gitea: Actions run %s isn't among those on %s: %w", n, sha, forge.ErrNotFound)
	}
	want := jobLink(d.URL)
	var job int64
	for page, seen := 1, 0; job == 0; page++ {
		var jobs struct {
			Total int64 `json:"total_count"`
			Jobs  []struct {
				ID      int64  `json:"id"`
				HTMLURL string `json:"html_url"`
			} `json:"jobs"`
		}
		p := fmt.Sprintf("%s/actions/runs/%d/jobs?limit=%d&page=%d", repoPath(r), run, pageLimit, page)
		if err := c.do(ctx, role, "GET", p, nil, &jobs); errors.Is(err, forge.ErrNotFound) {
			return out, fmt.Errorf("gitea: this Gitea has no API to read Actions logs (it came in 1.24): %w", err)
		} else if err != nil {
			return out, err
		}
		for _, j := range jobs.Jobs {
			if want != "" && strings.EqualFold(jobLink(j.HTMLURL), want) {
				job = j.ID
			}
		}
		seen += len(jobs.Jobs)
		if len(jobs.Jobs) == 0 || int64(seen) >= jobs.Total {
			break
		}
	}
	if job == 0 {
		return out, fmt.Errorf("gitea: no job of Actions run %d links to %s: %w", run, d.URL, forge.ErrNotFound)
	}
	err = c.download(ctx, c.token(role), fmt.Sprintf("%s/actions/jobs/%d/logs", repoPath(r), job), func(body io.Reader) error {
		var err error
		out.Log, out.LogCut, err = forge.TailLines(body, logLines)
		return err
	})
	if err != nil {
		return out, fmt.Errorf("gitea: reading the log of job %q: %w", d.Context, err)
	}
	return out, nil
}

// jobLink is the part of a link to an Actions job that names it within the
// repo, "{run}/jobs/{job}", or "" for a link to anything else.
func jobLink(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	_, after, ok := strings.Cut(p.Path, "/actions/runs/")
	if !ok || !strings.Contains(after, "/jobs/") {
		return ""
	}
	return strings.TrimSuffix(after, "/")
}

// actionsRun is the run in the link to an Actions run of r, or to a job
// under it: {ROOT_URL}/{owner}/{repo}/actions/runs/{run}. Status links may
// leave out the host.
func actionsRun(u string, r forge.RepoRef) (string, bool) {
	p, err := url.Parse(u)
	if err != nil {
		return "", false
	}
	before, after, ok := strings.Cut(p.Path, "/actions/runs/")
	if !ok || !strings.HasSuffix(strings.ToLower(before), strings.ToLower("/"+r.Owner+"/"+r.Name)) {
		return "", false
	}
	n, _, _ := strings.Cut(after, "/")
	if _, err := strconv.ParseUint(n, 10, 64); err != nil {
		return "", false
	}
	return n, true
}

// Protection reads the branch as the reviewer, which merges, so
// user_can_merge is its own. Gitea matches status check contexts as glob
// patterns; only those without a wildcard name a check that must report.
func (c *Client) Protection(ctx context.Context, r forge.RepoRef, branch string) (forge.Protection, error) {
	var b struct {
		Protected           bool     `json:"protected"`
		RequiredApprovals   int      `json:"required_approvals"`
		EnableStatusCheck   bool     `json:"enable_status_check"`
		StatusCheckContexts []string `json:"status_check_contexts"`
		UserCanMerge        bool     `json:"user_can_merge"`
		RuleName            string   `json:"effective_branch_protection_name"`
	}
	err := c.do(ctx, forge.RoleReviewer, "GET", repoPath(r)+"/branches/"+url.PathEscape(branch), nil, &b)
	if errors.Is(err, forge.ErrNotFound) {
		return forge.Protection{}, nil
	}
	if err != nil || !b.Protected {
		return forge.Protection{}, err
	}
	var out forge.Protection
	switch {
	case b.RequiredApprovals == 1:
		out.Rules = append(out.Rules, "an approving review")
	case b.RequiredApprovals > 1:
		out.Rules = append(out.Rules, fmt.Sprintf("%d approving reviews", b.RequiredApprovals))
	}
	if b.EnableStatusCheck && len(b.StatusCheckContexts) > 0 {
		out.Rules = append(out.Rules, "checks matching `"+strings.Join(b.StatusCheckContexts, "`, `")+"` must pass")
		for _, s := range b.StatusCheckContexts {
			if !strings.ContainsAny(s, `*?[\`) {
				out.RequiredChecks = append(out.RequiredChecks, s)
			}
		}
	}
	if !b.UserCanMerge {
		out.Rules = append(out.Rules, "devbot's reviewer isn't allowed to merge into it")
	}
	// Whether the rule blocks outdated branches is only on the rule itself,
	// which Gitea shows to repo admins alone: without that, it isn't known.
	var rule struct {
		BlockOnOutdatedBranch bool `json:"block_on_outdated_branch"`
	}
	if b.RuleName != "" {
		err := c.do(ctx, forge.RoleReviewer, "GET", repoPath(r)+"/branch_protections/"+url.PathEscape(b.RuleName), nil, &rule)
		if err != nil && !errors.Is(err, forge.ErrNotFound) && !errors.Is(err, forge.ErrForbidden) {
			return forge.Protection{}, err
		}
		if rule.BlockOnOutdatedBranch {
			out.UpToDate = true
			out.Rules = append(out.Rules, "the branch must be up to date with the base")
		}
	}
	if len(out.Rules) > 0 {
		out.Exempt = fmt.Sprintf("In the repo's branch protection rule `%s` (Settings, Branches), add the reviewer, @%s, which merges, to the merge whitelist, and to the approvals whitelist if that is on.",
			b.RuleName, c.cfg.ReviewerUser)
	}
	return out, nil
}

func (c *Client) Review(ctx context.Context, r forge.RepoRef, number int, sha string, v forge.Verdict, body string) error {
	ev := map[forge.Verdict]string{
		forge.VerdictApprove: "APPROVED", forge.VerdictRequestChanges: "REQUEST_CHANGES", forge.VerdictComment: "COMMENT",
	}[v]
	if ev == "" {
		return fmt.Errorf("unknown verdict %q", v)
	}
	return c.do(ctx, forge.RoleReviewer, "POST", fmt.Sprintf("%s/pulls/%d/reviews", repoPath(r), number),
		map[string]any{"event": ev, "body": body, "commit_id": sha}, nil)
}

func (c *Client) Reviews(ctx context.Context, r forge.RepoRef, number int) ([]forge.Review, error) {
	var out []forge.Review
	type review struct {
		ID          int64     `json:"id"`
		User        user      `json:"user"`
		State       string    `json:"state"`
		CommitID    string    `json:"commit_id"`
		Body        string    `json:"body"`
		SubmittedAt time.Time `json:"submitted_at"`
		Dismissed   bool      `json:"dismissed"`
	}
	err := list(ctx, c, c.token(c.acting()), fmt.Sprintf("%s/pulls/%d/reviews", repoPath(r), number), func(rv review) {
		v := forge.VerdictComment
		switch rv.State {
		case "APPROVED":
			v = forge.VerdictApprove
		case "REQUEST_CHANGES":
			v = forge.VerdictRequestChanges
		}
		out = append(out, forge.Review{ID: rv.ID, Author: rv.User.Login, Verdict: v, CommitSHA: rv.CommitID, Body: rv.Body, SubmittedAt: rv.SubmittedAt, Dismissed: rv.Dismissed})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ReviewComments reads each review's line comments, only for reviews that
// have any. Gitea serves a review's comments as one list, without pages,
// while the reviews themselves are paged.
func (c *Client) ReviewComments(ctx context.Context, r forge.RepoRef, number int) ([]forge.ReviewComment, error) {
	tok := c.token(c.acting())
	var ids []int64
	err := list(ctx, c, tok, fmt.Sprintf("%s/pulls/%d/reviews", repoPath(r), number), func(rv struct {
		ID       int64 `json:"id"`
		Comments int   `json:"comments_count"`
	}) {
		if rv.Comments > 0 {
			ids = append(ids, rv.ID)
		}
	})
	if err != nil {
		return nil, err
	}
	var out []forge.ReviewComment
	for _, id := range ids {
		var cs []struct {
			ID               int64     `json:"id"`
			ReviewID         int64     `json:"pull_request_review_id"`
			User             user      `json:"user"`
			Body             string    `json:"body"`
			Path             string    `json:"path"`
			Position         int       `json:"position"`
			OriginalPosition int       `json:"original_position"`
			CommitID         string    `json:"commit_id"`
			CreatedAt        time.Time `json:"created_at"`
		}
		// A review deleted since the list is no longer anyone's feedback.
		err := c.do(ctx, c.acting(), "GET", fmt.Sprintf("%s/pulls/%d/reviews/%d/comments", repoPath(r), number, id), nil, &cs)
		if errors.Is(err, forge.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, rc := range cs {
			// Gitea puts a comment on the new side of the diff in position,
			// and one on the old side in original_position.
			out = append(out, forge.ReviewComment{ID: rc.ID, ReviewID: cmp.Or(rc.ReviewID, id), Author: rc.User.Login, Body: rc.Body,
				Path: rc.Path, Line: cmp.Or(rc.Position, rc.OriginalPosition), CommitSHA: rc.CommitID, CreatedAt: rc.CreatedAt})
		}
	}
	slices.SortStableFunc(out, func(a, b forge.ReviewComment) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

func (c *Client) Merge(ctx context.Context, r forge.RepoRef, number int, sha, method string) error {
	if method == "" {
		method = "squash"
	}
	err := c.do(ctx, forge.RoleReviewer, "POST", fmt.Sprintf("%s/pulls/%d/merge", repoPath(r), number),
		map[string]any{"Do": method, "head_commit_id": sha}, nil)
	var ae *apiError
	if !errors.As(err, &ae) {
		return err
	}
	msg := ae.Body
	var b struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(ae.Body), &b) == nil && b.Message != "" {
		msg = b.Message
	}
	switch ae.Status {
	// 409 is the head no longer being head_commit_id ("head out of date"),
	// but also a conflict or unrelated histories found only while merging,
	// or a push a hook rejected. Core tells a real head move from the rest
	// by reading the PR's head again, and counts the rest as failures.
	case http.StatusConflict:
		return fmt.Errorf("%w: gitea: %s", forge.ErrHeadMoved, msg)
	// 405 is Gitea refusing the merge as the PR stands: a conflict, or the
	// branch protection ("Does not have enough approvals"). "Please try
	// again later" is the exception: Gitea is still checking the PR, and
	// will be done soon.
	case http.StatusMethodNotAllowed:
		if strings.Contains(strings.ToLower(msg), "try again later") {
			return fmt.Errorf("%w: gitea: %s", forge.ErrMergePending, msg)
		}
		return fmt.Errorf("%w: gitea: %s", forge.ErrNotMergeable, msg)
	}
	return err
}

// AttachToPR uploads to the PR's issue assets; Gitea stores a PR as an issue
// with the same index.
func (c *Client) AttachToPR(ctx context.Context, r forge.RepoRef, number int, name string, data []byte) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("attachment", name)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	u := fmt.Sprintf("%s/api/v1%s/issues/%d/assets?name=%s", c.cfg.URL, repoPath(r), number, url.QueryEscape(name))
	req, err := http.NewRequestWithContext(ctx, "POST", u, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "token "+c.token(c.acting()))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", &apiError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	var a struct {
		BrowserDownloadURL string `json:"browser_download_url"`
	}
	if err := json.Unmarshal(b, &a); err != nil || a.BrowserDownloadURL == "" {
		return "", fmt.Errorf("gitea: upload of %s returned no download URL", name)
	}
	return a.BrowserDownloadURL, nil
}

var attachmentUUID = regexp.MustCompile(`^[0-9a-fA-F-]{8,64}$`)

// GetAttachment fetches /attachments/<uuid>, where Gitea accepts an API token
// for downloads, but only a UUID listed in the PR's own issue assets: the
// token can read attachments on every repo the bot sees, private ones too.
// Only the UUID is taken from the URL, and the request goes to the
// configured server: the URL's own host may be ROOT_URL rather than the
// address devbot reaches Gitea at, and must never receive the token anyway.
func (c *Client) GetAttachment(ctx context.Context, r forge.RepoRef, number int, raw string, role forge.Role) ([]byte, error) {
	pu, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("gitea: attachment URL %q: %w", raw, err)
	}
	dir, id := path.Split(pu.Path)
	if !strings.HasSuffix(dir, "/attachments/") || !attachmentUUID.MatchString(id) {
		return nil, fmt.Errorf("gitea: %q is not a Gitea attachment URL", raw)
	}
	var assets []struct {
		UUID string `json:"uuid"`
	}
	if err := c.do(ctx, role, "GET", fmt.Sprintf("%s/issues/%d/assets", repoPath(r), number), nil, &assets); err != nil {
		return nil, err
	}
	found := false
	for _, a := range assets {
		found = found || strings.EqualFold(a.UUID, id)
	}
	if !found {
		return nil, fmt.Errorf("gitea: %q is not attached to %s#%d", raw, r, number)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.cfg.URL+"/attachments/"+id, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+c.token(role))
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, forge.ErrNotFound
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, &apiError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	return b, nil
}

// ReleaseAttachments does nothing: Gitea keeps them as the PR's own issue
// assets, where the merged PR's body still shows them.
func (c *Client) ReleaseAttachments(context.Context, forge.RepoRef, int) error { return nil }

// ---- repo ----

func (c *Client) DefaultBranch(ctx context.Context, r forge.RepoRef) (string, error) {
	var rp repository
	if err := c.do(ctx, c.acting(), "GET", repoPath(r), nil, &rp); err != nil {
		return "", err
	}
	return rp.DefaultBranch, nil
}

func (c *Client) BranchExists(ctx context.Context, r forge.RepoRef, branch string) (bool, error) {
	err := c.do(ctx, forge.RoleImplementer, "GET", repoPath(r)+"/branches/"+url.PathEscape(branch), nil, nil)
	if errors.Is(err, forge.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// ListRepos reads /user/repos, which has the repos the implementer owns,
// collaborates on or reaches through an org team.
func (c *Client) ListRepos(ctx context.Context) ([]forge.RepoRef, error) {
	var out []forge.RepoRef
	err := list(ctx, c, c.token(forge.RoleImplementer), "/user/repos", func(rp struct {
		repository
		Permissions struct {
			Admin bool `json:"admin"`
			Push  bool `json:"push"`
		} `json:"permissions"`
	}) {
		if rp.Permissions.Push || rp.Permissions.Admin {
			out = append(out, c.ref(rp.Owner.Login, rp.Name))
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListBranches pages through every branch: Gitea's list has no name filter.
func (c *Client) ListBranches(ctx context.Context, r forge.RepoRef, prefix string) ([]forge.Branch, error) {
	var out []forge.Branch
	err := list(ctx, c, c.token(forge.RoleImplementer), repoPath(r)+"/branches", func(b struct {
		Name   string `json:"name"`
		Commit struct {
			ID        string    `json:"id"`
			Timestamp time.Time `json:"timestamp"`
		} `json:"commit"`
	}) {
		if strings.HasPrefix(b.Name, prefix) {
			out = append(out, forge.Branch{Name: b.Name, SHA: b.Commit.ID, CommittedAt: b.Commit.Timestamp})
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) BranchHead(ctx context.Context, r forge.RepoRef, branch string) (string, error) {
	var b struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := c.do(ctx, c.acting(), "GET", repoPath(r)+"/branches/"+url.PathEscape(branch), nil, &b); err != nil {
		return "", err
	}
	return b.Commit.ID, nil
}

// ListTags reads pages of the tag list, which Gitea sorts newest first,
// until it has n.
func (c *Client) ListTags(ctx context.Context, r forge.RepoRef, n int) ([]forge.Tag, error) {
	var out []forge.Tag
	for page := 1; len(out) < n; page++ {
		var batch []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		p := fmt.Sprintf("%s/tags?limit=%d&page=%d", repoPath(r), min(n, pageLimit), page)
		if err := c.do(ctx, c.acting(), "GET", p, nil, &batch); err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		for _, t := range batch {
			out = append(out, forge.Tag{Name: t.Name, SHA: t.Commit.SHA})
		}
	}
	return out[:min(n, len(out))], nil
}

// Contains compares ref with ancestor as the base: the commits ancestor
// has that ref doesn't are none exactly when ref's history has it. Each
// commit's verification and files are left out, which are the costly part.
func (c *Client) Contains(ctx context.Context, r forge.RepoRef, ref, ancestor string) (bool, error) {
	var cmp struct {
		TotalCommits int `json:"total_commits"`
	}
	p := repoPath(r) + "/compare/" + escapePath(ref) + "..." + url.PathEscape(ancestor) + "?verification=false&files=false"
	if err := c.do(ctx, c.acting(), "GET", p, nil, &cmp); err != nil {
		return false, err
	}
	return cmp.TotalCommits == 0, nil
}

// ListTree pages through the recursive tree, which Gitea marks truncated
// while there are pages to come (DEFAULT_GIT_TREES_PER_PAGE, 1000 unless
// the instance says otherwise), to its end.
func (c *Client) ListTree(ctx context.Context, r forge.RepoRef, ref string) ([]forge.TreeFile, error) {
	var out []forge.TreeFile
	for page := 1; ; page++ {
		var t struct {
			Tree []struct {
				Path string `json:"path"`
				Type string `json:"type"`
				Size int64  `json:"size"`
			} `json:"tree"`
			Truncated bool `json:"truncated"`
		}
		p := fmt.Sprintf("%s/git/trees/%s?recursive=true&per_page=1000&page=%d", repoPath(r), url.PathEscape(ref), page)
		if err := c.do(ctx, c.acting(), "GET", p, nil, &t); err != nil {
			return nil, err
		}
		for _, e := range t.Tree {
			if e.Type == "blob" {
				out = append(out, forge.TreeFile{Path: e.Path, Size: e.Size})
			}
		}
		if !t.Truncated || len(t.Tree) == 0 {
			return out, nil
		}
	}
}

func (c *Client) DeleteBranch(ctx context.Context, r forge.RepoRef, name string) error {
	return c.do(ctx, forge.RoleImplementer, "DELETE", repoPath(r)+"/branches/"+escapePath(name), nil, nil)
}

func (c *Client) GetFile(ctx context.Context, r forge.RepoRef, ref, path string) ([]byte, error) {
	u := c.cfg.URL + "/api/v1" + repoPath(r) + "/raw/" + escapePath(path)
	if ref != "" {
		u += "?ref=" + url.QueryEscape(ref)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+c.token(c.acting()))
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, forge.ErrNotFound
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, &apiError{Status: resp.StatusCode, Body: string(b)}
	}
	return b, nil
}

func escapePath(p string) string {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func (c *Client) GitAuth(ctx context.Context, r forge.RepoRef, role forge.Role) (string, string, error) {
	if c.token(role) == "" {
		return "", "", fmt.Errorf("gitea: the %s bot: %w", role, forge.ErrNoCredential)
	}
	return c.cfg.URL + "/" + r.Owner + "/" + r.Name + ".git", "Authorization: token " + c.token(role), nil
}

// CanPushWorkflows is true for a bot devbot holds the token of: Gitea gives
// workflow files no permission of their own.
func (c *Client) CanPushWorkflows(_ context.Context, _ forge.RepoRef, role forge.Role) (bool, error) {
	if c.token(role) == "" {
		return false, fmt.Errorf("gitea: the %s bot: %w", role, forge.ErrNoCredential)
	}
	return true, nil
}

func (c *Client) CanWrite(ctx context.Context, r forge.RepoRef, role forge.Role) (bool, error) {
	var rp struct {
		Permissions struct {
			Admin bool `json:"admin"`
			Push  bool `json:"push"`
		} `json:"permissions"`
	}
	if err := c.do(ctx, role, "GET", repoPath(r), nil, &rp); err != nil {
		if errors.Is(err, forge.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if !rp.Permissions.Push && !rp.Permissions.Admin {
		return false, &forge.MissingAccessError{Need: fmt.Sprintf("the %s bot @%s has read access to %s; devbot needs write, so add it as a collaborator with write or to a team that has it", role, c.login(role), r.FullName())}
	}
	return true, nil
}

// Authorized asks Gitea for actor's permission, which includes what org
// teams grant. Gitea answers only a site admin or an admin of the repo
// (directly or through a team), so a refusal means the bot asking lacks that
// access, not that actor does.
func (c *Client) Authorized(ctx context.Context, r forge.RepoRef, actor string, min forge.Permission) (bool, error) {
	if actor == "" {
		return false, nil
	}
	if min == forge.PermMaintain {
		min = forge.PermWrite // Gitea has no maintain role
	}
	if actor == r.Owner {
		return true, nil
	}
	var p struct {
		Permission string `json:"permission"`
	}
	err := c.do(ctx, c.acting(), "GET", repoPath(r)+"/collaborators/"+url.PathEscape(actor)+"/permission", nil, &p)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == 403 {
		return false, &forge.LookupForbiddenError{
			Need: fmt.Sprintf("the %s bot @%s needs admin on the repo, or on the org team that holds it, to read collaborators' permissions", c.acting(), c.login(c.acting())),
			Err:  err,
		}
	}
	if errors.Is(err, forge.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return giteaPermission(p.Permission) >= min, nil
}

func giteaPermission(s string) forge.Permission {
	switch s {
	case "owner", "admin":
		return forge.PermAdmin
	case "write":
		return forge.PermWrite
	case "read":
		return forge.PermRead
	}
	return forge.PermNone
}

// ---- webhooks ----

// ParseWebhook verifies X-Gitea-Signature (hex HMAC-SHA256 of the body) and
// maps the delivery onto the object it is about. Past identifiers, it reads
// only what lets the engine skip a delivery that can't concern devbot
// (forge.Event.Facts): the engine re-reads everything else, because
// deliveries are retried and arrive out of order.
func (c *Client) ParseWebhook(r *http.Request, body []byte) ([]forge.Event, error) {
	if c.cfg.WebhookSecret == "" {
		return nil, errors.New("gitea: no webhook secret configured")
	}
	sig, err := hex.DecodeString(r.Header.Get("X-Gitea-Signature"))
	if err != nil || len(sig) == 0 {
		return nil, errors.New("gitea: missing or malformed signature")
	}
	mac := hmac.New(sha256.New, []byte(c.cfg.WebhookSecret))
	mac.Write(body)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, errors.New("gitea: bad signature")
	}

	var p struct {
		Repository  *repository `json:"repository"`
		Sender      user        `json:"sender"`
		Number      int         `json:"number"`
		Issue       *issue      `json:"issue"`
		PullRequest *pull       `json:"pull_request"`
		Comment     *struct {
			Body string `json:"body"`
		} `json:"comment"`
		SHA         string `json:"sha"`
		Ref         string `json:"ref"`
		RefType     string `json:"ref_type"`
		WorkflowRun *struct {
			HeadSHA string `json:"head_sha"`
		} `json:"workflow_run"`
		WorkflowJob *struct {
			HeadSHA string `json:"head_sha"`
		} `json:"workflow_job"`
		Commits []struct {
			Added    []string `json:"added"`
			Removed  []string `json:"removed"`
			Modified []string `json:"modified"`
		} `json:"commits"`
		TotalCommits int `json:"total_commits"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("gitea: decode webhook: %w", err)
	}
	ev := forge.Event{DeliveryID: r.Header.Get("X-Gitea-Delivery"), Actor: p.Sender.Login, Kind: forge.EventIgnored}
	if p.Repository == nil {
		return []forge.Event{ev}, nil
	}
	owner, name, _ := strings.Cut(p.Repository.FullName, "/")
	ev.Repo = c.ref(owner, name)

	switch typ := r.Header.Get("X-Gitea-Event"); {
	// A tag comes as a push of refs/tags/, and as a create to a hook that
	// asked for those.
	case typ == "push" && strings.HasPrefix(p.Ref, "refs/tags/"), typ == "create" && p.RefType == "tag":
		ev.Kind = forge.EventTag
	case typ == "push":
		if p.Ref == "refs/heads/"+p.Repository.DefaultBranch {
			ev.Kind = forge.EventDefaultBranch
			// Gitea lists only the first few commits of a longer push (5
			// by default): with some left out, the paths aren't known.
			if p.TotalCommits > len(p.Commits) {
				break
			}
			for _, cm := range p.Commits {
				ev.Paths = append(ev.Paths, cm.Added...)
				ev.Paths = append(ev.Paths, cm.Removed...)
				ev.Paths = append(ev.Paths, cm.Modified...)
			}
		}
	case typ == "status":
		ev.Kind, ev.SHA = forge.EventStatus, p.SHA
	case p.WorkflowJob != nil && p.WorkflowJob.HeadSHA != "":
		ev.Kind, ev.SHA = forge.EventStatus, p.WorkflowJob.HeadSHA
	case p.WorkflowRun != nil && p.WorkflowRun.HeadSHA != "":
		ev.Kind, ev.SHA = forge.EventStatus, p.WorkflowRun.HeadSHA
	case p.PullRequest != nil:
		ev.Kind, ev.Number = forge.EventPR, p.PullRequest.Number
		ev.Facts, ev.Labels, ev.Head = true, labelNames(p.PullRequest.Labels), p.PullRequest.Head.Ref
	case p.Issue != nil && p.Issue.PullRequest != nil:
		ev.Kind, ev.Number = forge.EventPR, p.Issue.Number
		ev.Facts, ev.Labels = true, labelNames(p.Issue.Labels)
	case p.Issue != nil:
		ev.Kind, ev.Number = forge.EventItem, p.Issue.Number
		ev.Facts, ev.Labels = true, labelNames(p.Issue.Labels)
	// A delivery that names a PR only by number says nothing else of it.
	case strings.HasPrefix(typ, "pull_request") && p.Number > 0:
		ev.Kind, ev.Number = forge.EventPR, p.Number
	}
	if ev.Facts && p.Comment != nil {
		ev.Comment = p.Comment.Body
	}
	return []forge.Event{ev}, nil
}

func labelNames(ls []label) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}

func (c *Client) repoWeb(r forge.RepoRef) string {
	return c.cfg.URL + "/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

func (c *Client) IssueURL(ref forge.ItemRef) string {
	return fmt.Sprintf("%s/issues/%d", c.repoWeb(ref.Repo), ref.Number)
}

func (c *Client) PRURL(r forge.RepoRef, number int) string {
	return fmt.Sprintf("%s/pulls/%d", c.repoWeb(r), number)
}

func (c *Client) BranchURL(r forge.RepoRef, branch string) string {
	return c.repoWeb(r) + "/src/branch/" + escapePath(branch)
}

// sleep waits d, or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// statusBody is a commit status as the API takes it, with no empty link.
func statusBody(s forge.CommitStatus) map[string]string {
	b := map[string]string{"state": s.State, "context": s.Context, "description": s.Description}
	if s.URL != "" {
		b["target_url"] = s.URL
	}
	return b
}

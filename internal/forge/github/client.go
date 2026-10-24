// Package github is the forge adapter for GitHub.com and GitHub Enterprise
// Server, over the REST API with plain net/http. Each bot is a GitHub App:
// it acts with installation tokens minted from the App's private key for
// an hour at a time, so no long-lived token is ever stored.
//
// API behaviour follows GitHub's REST docs (docs.github.com/en/rest), API
// version 2022-11-28.
package github

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
)

const apiVersion = "2022-11-28"

var _ forge.Provider = (*Client)(nil)

type Config struct {
	Name string // provider name in RepoRefs, e.g. "github"
	// URL is the API base: https://api.github.com, or
	// https://ghe.example.com/api/v3 for GitHub Enterprise Server. Web
	// URLs are derived from it.
	URL string
	// Each bot is an App: its ID and PEM private key. A process that acts as
	// one bot only (a job pod) has only that App's key, and is given the
	// other's login instead, which can't be read without its key. It then
	// acts as its own App for everything, and a call made as the other
	// role fails with forge.ErrNoCredential.
	ImplementerAppID int64
	ImplementerKey   []byte
	ImplementerLogin string
	ReviewerAppID    int64
	ReviewerKey      []byte
	ReviewerLogin    string
	WebhookSecret    string
	// Allowed is the install's repos.allow/deny: SearchItems returns
	// nothing from a repo it turns down. nil allows every repo.
	Allowed func(forge.RepoRef) bool
	// InventoryGeneration and BumpInventory, when set, share the repo
	// inventory's invalidation between processes (queue.InventoryGeneration):
	// an installation webhook bumps it on whichever serve replica it is
	// delivered to, and the inventory listed under another generation is
	// listed again. Without them a webhook invalidates only this process's.
	InventoryGeneration func(ctx context.Context) (string, error)
	BumpInventory       func(ctx context.Context) error
	// LoadQuota and NoteQuota, when set, share what GitHub's answers say of
	// each rate limit between processes (queue.Quotas, queue.NoteQuota), so
	// a replica or job pod holds back a credential bucket another found
	// spent. Without them each process knows only its own answers; when
	// they fail, it goes on with what it knows.
	LoadQuota func(ctx context.Context, bucket string, resources ...string) ([]forge.Quota, error)
	NoteQuota func(ctx context.Context, bucket, resource string, q forge.Quota) error
	// LowQuotaPercent is the share of a bucket's core quota below which
	// calls made with forge.LowPriority are deferred to its reset. 0
	// defers none.
	LowQuotaPercent int
	// The sign-in App's client ID and secret, for people signing in to
	// devbot (auth.mode oauth): the implementer App's, or a separate App's.
	OAuthClientID     string
	OAuthClientSecret string
}

type Client struct {
	cfg  Config
	api  *url.URL
	web  string
	http *http.Client
	impl *app
	rev  *app

	// The clock and sleep, which tests replace.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
	// maxWait is the longest a call waits out a rate limit. A longer wait
	// fails the call instead: the reconciler comes back to it.
	maxWait time.Duration
	// secondaryBase is the first wait on a secondary rate limit that
	// doesn't say how long to wait, doubling on each retry.
	secondaryBase time.Duration

	mu sync.Mutex
	// quota is what GitHub last said of each primary rate limit, by
	// credential bucket and resource ("core", "graphql", "search").
	quota map[string]forge.Quota
	// committed is each commit date ListBranches has read, by SHA.
	committed map[string]time.Time
}

// New builds the adapter and reads each App's login (GET /app), so a bad
// key or App ID fails at startup.
func New(ctx context.Context, cfg Config) (*Client, error) {
	c, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	for _, a := range []*app{c.impl, c.rev} {
		if a.key == nil {
			continue
		}
		if err := c.identify(ctx, a); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func newClient(cfg Config) (*Client, error) {
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	api, err := url.Parse(cfg.URL)
	if err != nil || (api.Scheme != "https" && api.Scheme != "http") || api.Host == "" {
		return nil, fmt.Errorf("github: url %q: want the API base, like https://api.github.com", cfg.URL)
	}
	c := &Client{
		cfg: cfg, api: api, web: webBase(api), http: &http.Client{Timeout: 30 * time.Second},
		now: time.Now, sleep: sleep, maxWait: 2 * time.Minute, secondaryBase: time.Minute,
		quota: map[string]forge.Quota{}, committed: map[string]time.Time{},
	}
	if c.impl, err = newApp(forge.RoleImplementer, cfg.ImplementerAppID, cfg.ImplementerKey, cfg.ImplementerLogin); err != nil {
		return nil, err
	}
	if c.rev, err = newApp(forge.RoleReviewer, cfg.ReviewerAppID, cfg.ReviewerKey, cfg.ReviewerLogin); err != nil {
		return nil, err
	}
	if c.impl.key == nil && c.rev.key == nil {
		return nil, errors.New("github: neither App has a private key")
	}
	return c, nil
}

// webBase is the web host for an API base: github.com for api.github.com,
// the host itself for GHES's /api/v3, and the host without "api." for
// GitHub Enterprise Cloud with data residency (api.acme.ghe.com).
func webBase(api *url.URL) string {
	if p, ok := strings.CutSuffix(api.Path, "/api/v3"); ok {
		return api.Scheme + "://" + api.Host + p
	}
	if h, ok := strings.CutPrefix(api.Host, "api."); ok {
		return api.Scheme + "://" + h
	}
	return api.Scheme + "://" + api.Host
}

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

func (c *Client) Name() string { return c.cfg.Name }

// BotUsers are the Apps' logins, "<slug>[bot]".
func (c *Client) BotUsers() (string, string) { return c.impl.login, c.rev.login }

func (c *Client) ref(o, n string) forge.RepoRef {
	return forge.RepoRef{Provider: c.cfg.Name, Owner: o, Name: n}
}

func (c *Client) app(role forge.Role) *app {
	if role == forge.RoleReviewer {
		return c.rev
	}
	return c.impl
}

// acting is the role of the calls that don't take one: the implementer,
// or the reviewer in a process that only holds the reviewer's key.
func (c *Client) acting() forge.Role {
	if c.impl.key == nil {
		return forge.RoleReviewer
	}
	return forge.RoleImplementer
}

func repoPath(r forge.RepoRef) string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

func escapePath(p string) string {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// secret is a JWT or token. It prints redacted, so one that ends up in a
// formatted error or a log line doesn't leak.
type secret string

func (secret) String() string   { return "[redacted]" }
func (secret) GoString() string { return "[redacted]" }

// cred is who a call is made as.
type cred struct {
	// bucket names the rate limit the call counts against: an App's JWT
	// or one installation. Never secret.
	bucket string
	token  secret
	// perms is what an installation token may do; nil for an App's JWT.
	perms map[string]string
}

// fmt can't reach an unexported field's String, so cred redacts itself.
func (c cred) String() string   { return c.bucket + " [redacted]" }
func (c cred) GoString() string { return c.String() }

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("github: HTTP %d: %s", e.Status, e.Body) }

// message is GitHub's own explanation, for a person: the body's "message",
// or the whole error when there isn't one.
func (e *apiError) message() string {
	var b struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(e.Body), &b) == nil && b.Message != "" {
		return "github: " + b.Message
	}
	return e.Error()
}

// Is lets callers test for the forge's refusals without knowing GitHub's shapes.
func (e *apiError) Is(target error) bool {
	return (target == forge.ErrForbidden && e.Status == http.StatusForbidden) ||
		(target == forge.ErrUnauthorized && e.Status == http.StatusUnauthorized)
}

// rateLimit is a call given up for a rate limit until until, counted in
// forge.RateLimitErrors.
func (c *Client) rateLimit(kind string, until time.Time) *forge.RateLimitError {
	forge.RateLimitErrors.Add(c.cfg.Name, kind)
	return &forge.RateLimitError{Provider: "github", Kind: kind, Until: until}
}

// do calls the API as cr and decodes a JSON answer into out.
func (c *Client) do(ctx context.Context, cr cred, method, path string, in, out any) (http.Header, error) {
	b, h, err := c.send(ctx, cr, method, path, "", in)
	if err != nil {
		return h, err
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return h, fmt.Errorf("github: decode %s %s: %w", method, path, err)
		}
	}
	return h, nil
}

const maxRetries = 4

// send makes one call, waiting out rate limits on the way: it holds back
// a call while the credential's primary limit for its resource is spent,
// and retries one that hits a limit after the wait GitHub asks for (docs:
// "Rate limits for the REST API", "Best practices for using the REST API"),
// within maxWait and what is left of ctx's forge.WaitBudget.
// path is relative to the API base, or a full URL there (a Link header's).
func (c *Client) send(ctx context.Context, cr cred, method, path, accept string, in any) ([]byte, http.Header, error) {
	u, err := c.resolve(path)
	if err != nil {
		return nil, nil, err
	}
	var body []byte
	if in != nil {
		if body, err = json.Marshal(in); err != nil {
			return nil, nil, err
		}
	}
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	res := c.resource(u)
	for attempt := 0; ; attempt++ {
		if err := c.waitPrimary(ctx, cr.bucket, res); err != nil {
			return nil, nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+string(cr.token))
		req.Header.Set("Accept", accept)
		req.Header.Set("X-GitHub-Api-Version", apiVersion)
		req.Header.Set("User-Agent", "devbot")
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		c.count(cr.bucket, method, u, resp, err)
		if err != nil {
			return nil, nil, err
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			return nil, resp.Header, err
		}
		c.notePrimary(ctx, cr.bucket, res, resp.Header)
		if kind, wait, limited := c.rateLimited(resp, b, attempt, res == "graphql"); limited {
			if attempt >= maxRetries || wait > c.maxWait || !forge.SpendWait(ctx, wait) {
				return nil, resp.Header, c.rateLimit(kind, c.now().Add(wait))
			}
			forge.RateLimitWaits.Add(c.cfg.Name, kind, wait)
			if err := c.sleep(ctx, wait); err != nil {
				return nil, resp.Header, err
			}
			continue
		}
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return nil, resp.Header, forge.ErrNotFound
		case resp.StatusCode >= 300:
			return nil, resp.Header, &apiError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
		}
		return b, resp.Header, nil
	}
}

// download GETs path as cr and hands the body to read as it streams, for a
// download too big to hold (a job's log). GitHub answers some with a
// redirect to storage elsewhere, which the HTTP client follows without the
// Authorization header, as it does for any other host. It doesn't retry on
// a rate limit: what it reads is extra, and a caller does without.
func (c *Client) download(ctx context.Context, cr cred, path string, read func(io.Reader) error) error {
	u, err := c.resolve(path)
	if err != nil {
		return err
	}
	res := c.resource(u)
	if err := c.waitPrimary(ctx, cr.bucket, res); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+string(cr.token))
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "devbot")
	resp, err := c.http.Do(req)
	c.count(cr.bucket, "GET", u, resp, err)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	c.notePrimary(ctx, cr.bucket, res, resp.Header)
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return forge.ErrNotFound
	case resp.StatusCode >= 300:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &apiError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	return read(resp.Body)
}

// resolve turns path into a URL on the API base. A full URL must be on the
// API base too: a Link header is followed with the credential, which must
// never go anywhere else.
func (c *Client) resolve(path string) (string, error) {
	if path == c.graphqlURL() {
		return path, nil
	}
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		return c.cfg.URL + path, nil
	}
	u, err := url.Parse(path)
	if err != nil || u.Scheme != c.api.Scheme || u.Host != c.api.Host || !strings.HasPrefix(u.Path, c.api.Path+"/") {
		return "", fmt.Errorf("github: refusing to call %q, which isn't under %s", path, c.cfg.URL)
	}
	return path, nil
}

// graphqlURL is the GraphQL endpoint beside the REST API base: /graphql on
// api.github.com, /api/graphql on GHES.
func (c *Client) graphqlURL() string {
	if base, ok := strings.CutSuffix(c.cfg.URL, "/api/v3"); ok {
		return base + "/api/graphql"
	}
	return c.cfg.URL + "/graphql"
}

// graphql runs one GraphQL query as cr and decodes its data into out.
// GraphQL reports most failures in the answer rather than its status.
func (c *Client) graphql(ctx context.Context, cr cred, query string, vars map[string]any, out any) error {
	b, _, err := c.send(ctx, cr, "POST", c.graphqlURL(), "", map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return fmt.Errorf("github: decode graphql: %w", err)
	}
	if len(resp.Errors) > 0 {
		if resp.Errors[0].Type == "NOT_FOUND" {
			return forge.ErrNotFound
		}
		return fmt.Errorf("github: graphql: %s", resp.Errors[0].Message)
	}
	return json.Unmarshal(resp.Data, out)
}

// resource is the rate limit a call to u counts against, as GitHub names
// it in X-RateLimit-Resource: GraphQL and search each have a budget of
// their own beside the REST API's core one.
func (c *Client) resource(u string) string {
	if u == c.graphqlURL() {
		return "graphql"
	}
	if strings.HasPrefix(u, c.cfg.URL+"/search/") {
		return "search"
	}
	return "core"
}

// waitPrimary holds a call back until the primary limit of bucket's
// resource res resets, when GitHub has said it's spent, here or in another
// process. A low-priority call (forge.LowPriority) isn't held back but
// deferred, with a forge.LimitLowQuota error: while that limit is spent, or
// while less than LowQuotaPercent of the bucket's core quota is left. A
// wait past maxWait or ctx's forge.WaitBudget fails the call instead.
func (c *Client) waitPrimary(ctx context.Context, bucket, res string) error {
	q, core := c.quotas(ctx, bucket, res)
	now := c.now()
	low := forge.IsLowPriority(ctx)
	if q.Remaining == 0 && !q.Reset.IsZero() {
		// A second past the reset, as GitHub's clock and ours may disagree.
		until := q.Reset.Add(time.Second)
		if wait := until.Sub(now); wait > 0 {
			switch {
			case low:
				return c.rateLimit(forge.LimitLowQuota, until)
			case wait > c.maxWait, !forge.SpendWait(ctx, wait):
				return c.rateLimit(forge.LimitPrimary, until)
			}
			forge.RateLimitWaits.Add(c.cfg.Name, forge.LimitPrimary, wait)
			return c.sleep(ctx, wait)
		}
	}
	if low && c.cfg.LowQuotaPercent > 0 && core.Limit > 0 && now.Before(core.Reset) &&
		core.Remaining*100 < core.Limit*c.cfg.LowQuotaPercent {
		return c.rateLimit(forge.LimitLowQuota, core.Reset.Add(time.Second))
	}
	return nil
}

// quotas is what is known of bucket's limit for res and of its core one:
// this process's own, or what another process noted since, if newer.
func (c *Client) quotas(ctx context.Context, bucket, res string) (q, core forge.Quota) {
	resources := []string{res}
	if res != "core" {
		resources = append(resources, "core")
	}
	var shared []forge.Quota
	if c.cfg.LoadQuota != nil {
		shared, _ = c.cfg.LoadQuota(ctx, bucket, resources...)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	got := make([]forge.Quota, len(resources))
	for i, r := range resources {
		k := bucket + " " + r
		if i < len(shared) && newer(shared[i], c.quota[k]) {
			c.quota[k] = shared[i]
		}
		got[i] = c.quota[k]
	}
	return got[0], got[len(got)-1]
}

// newer reports whether a says more recently than b what is left: a later
// window, or less left in the same one. Answers that cross on the way
// can't make a spent limit look unspent.
func newer(a, b forge.Quota) bool {
	if !a.Reset.Equal(b.Reset) {
		return a.Reset.After(b.Reset)
	}
	return a.Remaining < b.Remaining
}

// notePrimary records what a response says of a primary limit of bucket:
// the one X-RateLimit-Resource names, else res, the one the call was
// expected to count against. It is shared with the other processes too.
func (c *Client) notePrimary(ctx context.Context, bucket, res string, h http.Header) {
	remaining, err := strconv.Atoi(h.Get("X-Ratelimit-Remaining"))
	if err != nil {
		return
	}
	reset, err := strconv.ParseInt(h.Get("X-Ratelimit-Reset"), 10, 64)
	if err != nil {
		return
	}
	limit, _ := strconv.Atoi(h.Get("X-Ratelimit-Limit"))
	q := forge.Quota{Remaining: remaining, Limit: limit, Reset: time.Unix(reset, 0)}
	res = cmp.Or(h.Get("X-Ratelimit-Resource"), res)
	c.mu.Lock()
	if k := bucket + " " + res; newer(q, c.quota[k]) {
		c.quota[k] = q
	}
	c.mu.Unlock()
	if c.cfg.NoteQuota != nil {
		c.cfg.NoteQuota(ctx, bucket, res, q)
	}
}

// count records a call in forge.Requests. People's buckets are one series:
// there is one for every person signed in.
func (c *Client) count(bucket, method, u string, resp *http.Response, err error) {
	if strings.HasPrefix(bucket, "user ") {
		bucket = "user"
	}
	status := "error"
	if err == nil {
		status = forge.StatusClass(resp.StatusCode)
	}
	forge.Requests.Add(forge.Request{Provider: c.cfg.Name, Bucket: bucket, Method: method, Endpoint: c.endpoint(u), Status: status})
}

// endpoint is u's path as a template, for a metric's label: the API base
// and query left out, and every segment that isn't a word of GitHub's API
// a placeholder, a run of them one, so a branch name or file path with
// slashes in it is one too. The repo's owner and name are {owner}/{repo}.
func (c *Client) endpoint(u string) string {
	if u == c.graphqlURL() {
		return "/graphql"
	}
	p := strings.TrimPrefix(u, c.cfg.URL)
	p, _, _ = strings.Cut(p, "?")
	var out []string
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i := 0; i < len(segs); i++ {
		s := segs[i]
		switch {
		case s == "repos" && i+2 < len(segs) && len(out) == 0:
			out = append(out, "repos", "{owner}", "{repo}")
			i += 2
		case apiWords[s]:
			out = append(out, s)
		case len(out) == 0 || out[len(out)-1] != "{}":
			out = append(out, "{}")
		}
	}
	return "/" + strings.Join(out, "/")
}

// apiWords are the literal path segments of the endpoints the adapter
// calls; anything else in a path is a name, a number or a SHA.
var apiWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`app installations installation access_tokens repositories repos user users
		orgs search issues pulls comments labels events reviews merge files commits status statuses
		check-runs check-suites annotations actions runs jobs logs rerun-failed-jobs branches protection
		rules rulesets git ref refs heads matching-refs trees blobs contents compare collaborators
		permission rate_limit timeline graphql tags`) {
		apiWords[w] = true
	}
}

// rateLimited reports whether resp is a rate limit and how long to wait
// before trying again. GitHub answers 403 or 429, or for GraphQL 200 with
// an error of type RATE_LIMITED (docs: "Rate limits and query limits for
// the GraphQL API"). Retry-After says how long; a spent primary limit says
// when it resets; a secondary limit that says neither gets at least a
// minute, doubling on each retry.
func (c *Client) rateLimited(resp *http.Response, body []byte, attempt int, graphql bool) (kind string, wait time.Duration, ok bool) {
	limited := resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests
	if !limited && graphql && resp.StatusCode == http.StatusOK {
		limited = graphqlRateLimited(body)
	}
	if !limited {
		return "", 0, false
	}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
		return "secondary", time.Duration(s) * time.Second, true
	}
	if resp.Header.Get("X-Ratelimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
			return "primary", max(time.Unix(reset+1, 0).Sub(c.now()), 0), true
		}
	}
	if resp.StatusCode != http.StatusForbidden || bytes.Contains(bytes.ToLower(body), []byte("secondary rate limit")) {
		return "secondary", c.secondaryBase << attempt, true
	}
	return "", 0, false
}

// graphqlRateLimited reports whether a GraphQL answer is a rate limit.
func graphqlRateLimited(body []byte) bool {
	if !bytes.Contains(body, []byte("RATE_LIMITED")) {
		return false
	}
	var resp struct {
		Errors []struct {
			Type string `json:"type"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return false
	}
	for _, e := range resp.Errors {
		if e.Type == "RATE_LIMITED" {
			return true
		}
	}
	return false
}

// list reads every page of a list endpoint, following the Link header's
// next page to the end, however many there are: a partial list could leave
// out the very event, label or check a decision turns on. Lists that can be
// narrowed (by name, head or prefix) are, so their length is devbot's.
func list[T any](ctx context.Context, c *Client, cr cred, path string) ([]T, error) {
	return listIn(ctx, c, cr, path, func(page []T) []T { return page })
}

// listIn is list for an endpoint whose pages wrap the items in an object
// of type W, which items unwraps.
func listIn[W, T any](ctx context.Context, c *Client, cr cred, path string, items func(W) []T) ([]T, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	next := path + sep + "per_page=100"
	var all []T
	for next != "" {
		var batch W
		h, err := c.do(ctx, cr, "GET", next, nil, &batch)
		if err != nil {
			return nil, err
		}
		all = append(all, items(batch)...)
		next = nextLink(h)
	}
	return all, nil
}

// nextLink is the rel="next" URL of a Link header, or "".
func nextLink(h http.Header) string {
	for _, part := range strings.Split(strings.Join(h.Values("Link"), ","), ",") {
		target, params, ok := strings.Cut(part, ";")
		if !ok {
			continue
		}
		for _, p := range strings.Split(params, ";") {
			if strings.TrimSpace(p) == `rel="next"` {
				return strings.Trim(strings.TrimSpace(target), "<>")
			}
		}
	}
	return ""
}

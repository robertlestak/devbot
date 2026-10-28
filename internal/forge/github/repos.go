package github

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
)

func (c *Client) DefaultBranch(ctx context.Context, r forge.RepoRef) (string, error) {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return "", err
	}
	var rp struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := c.do(ctx, cr, "GET", repoPath(r), nil, &rp); err != nil {
		return "", err
	}
	return rp.DefaultBranch, nil
}

// GetFile reads a file through the contents API, raw (docs: "Repository
// contents"). An empty ref is the default branch.
func (c *Client) GetFile(ctx context.Context, r forge.RepoRef, ref, path string) ([]byte, error) {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return nil, err
	}
	p := repoPath(r) + "/contents/" + escapePath(path)
	if ref != "" {
		p += "?ref=" + url.QueryEscape(ref)
	}
	b, _, err := c.send(ctx, cr, "GET", p, "application/vnd.github.raw+json", nil)
	return b, err
}

func (c *Client) BranchHead(ctx context.Context, r forge.RepoRef, branch string) (string, error) {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return "", err
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if _, err := c.do(ctx, cr, "GET", repoPath(r)+"/git/ref/heads/"+escapePath(branch), nil, &ref); err != nil {
		return "", err
	}
	return ref.Object.SHA, nil
}

// ListTags reads pages of the tag list (docs: "List repository tags"),
// whose commit is the tag's peeled, until it has n. GitHub lists the
// highest version first, which is the newest for tags that are versions.
func (c *Client) ListTags(ctx context.Context, r forge.RepoRef, n int) ([]forge.Tag, error) {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return nil, err
	}
	var out []forge.Tag
	next := fmt.Sprintf("%s/tags?per_page=%d", repoPath(r), min(max(n, 1), 100))
	for next != "" && len(out) < n {
		var batch []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		h, err := c.do(ctx, cr, "GET", next, nil, &batch)
		if err != nil {
			return nil, err
		}
		for _, t := range batch {
			out = append(out, forge.Tag{Name: t.Name, SHA: t.Commit.SHA})
		}
		next = nextLink(h)
	}
	return out[:min(n, len(out))], nil
}

// Contains compares ancestor, as the base, with ref (docs: "Compare two
// commits"): ref's history has ancestor exactly when ancestor has nothing
// ref doesn't, behind_by 0. One commit to a page keeps the answer small.
func (c *Client) Contains(ctx context.Context, r forge.RepoRef, ref, ancestor string) (bool, error) {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return false, err
	}
	var cmp struct {
		BehindBy int `json:"behind_by"`
	}
	basehead := url.PathEscape(ancestor) + "..." + escapePath(ref)
	if _, err := c.do(ctx, cr, "GET", repoPath(r)+"/compare/"+basehead+"?per_page=1", nil, &cmp); err != nil {
		return false, err
	}
	return cmp.BehindBy == 0, nil
}

// ListTree reads the recursive tree (docs: "Get a tree"), in one call when
// GitHub lists it whole. Past 100,000 entries or 7 MB GitHub cuts it short
// and marks it truncated; then it is read a level at a time, each
// subdirectory again in one call where GitHub lists that whole, so only a
// single directory too big for one response is ErrTreeTooBig.
func (c *Client) ListTree(ctx context.Context, r forge.RepoRef, ref string) ([]forge.TreeFile, error) {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return nil, err
	}
	return c.listTree(ctx, cr, r, ref, "")
}

// listTree lists the files of the tree at ref, a branch, commit or tree
// SHA, with prefix before each path.
func (c *Client) listTree(ctx context.Context, cr cred, r forge.RepoRef, ref, prefix string) ([]forge.TreeFile, error) {
	type tree struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
			Size int64  `json:"size"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	var t tree
	if _, err := c.do(ctx, cr, "GET", repoPath(r)+"/git/trees/"+escapePath(ref)+"?recursive=1", nil, &t); err != nil {
		return nil, err
	}
	var out []forge.TreeFile
	if !t.Truncated {
		for _, e := range t.Tree {
			if e.Type == "blob" {
				out = append(out, forge.TreeFile{Path: prefix + e.Path, Size: e.Size})
			}
		}
		return out, nil
	}
	t = tree{}
	if _, err := c.do(ctx, cr, "GET", repoPath(r)+"/git/trees/"+escapePath(ref), nil, &t); err != nil {
		return nil, err
	}
	if t.Truncated {
		return nil, fmt.Errorf("github: %s at %s: %s holds more entries than GitHub lists: %w", r, ref, cmp.Or(prefix, "/"), forge.ErrTreeTooBig)
	}
	for _, e := range t.Tree {
		switch e.Type {
		case "blob":
			out = append(out, forge.TreeFile{Path: prefix + e.Path, Size: e.Size})
		case "tree":
			sub, err := c.listTree(ctx, cr, r, e.SHA, prefix+e.Path+"/")
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
		}
	}
	return out, nil
}

// GitAuth authenticates git with the role's installation token, which git
// takes as the password of user x-access-token (docs: "Authenticating as a
// GitHub App installation"). The token lasts an hour.
func (c *Client) GitAuth(ctx context.Context, r forge.RepoRef, role forge.Role) (string, string, error) {
	cr, err := c.repoCred(ctx, role, r)
	if err != nil {
		return "", "", err
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + string(cr.token)))
	return c.repoWeb(r) + ".git", "Authorization: Basic " + basic, nil
}

// CanPushWorkflows reads the token GitAuth hands out for r: GitHub refuses
// a push that changes .github/workflows from an App token without
// workflows write, which the token has only when the installation grants it.
func (c *Client) CanPushWorkflows(ctx context.Context, r forge.RepoRef, role forge.Role) (bool, error) {
	cr, err := c.repoCred(ctx, role, r)
	if err != nil {
		return false, err
	}
	return cr.perms["workflows"] == "write", nil
}

// CanWrite asks GitHub, as the role's App, for its installation on r: it
// can write when the App is installed there with contents (push), pull
// requests and issues write. Each role's job pod acts on the issue as its
// own App. An installation without them is a *forge.MissingAccessError
// naming what to grant.
func (c *Client) CanWrite(ctx context.Context, r forge.RepoRef, role forge.Role) (bool, error) {
	a := c.app(role)
	in, err := c.installation(ctx, a, r)
	if errors.Is(err, forge.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if need := missingPermissions(a, in.Permissions); need != "" {
		return false, &forge.MissingAccessError{Need: need}
	}
	return true, nil
}

// writePermissions are what CanWrite requires of each App's installation.
var writePermissions = []string{"contents", "pull_requests", "issues"}

// missingPermissions says, for a person, which of writePermissions an
// installation granted perms lacks and how to fix it, or "" when none.
// A role's job pod acts as that App alone, so both need all three.
func missingPermissions(a *app, perms map[string]string) string {
	var has, need []string
	for _, p := range writePermissions {
		if perms[p] == "write" || perms[p] == "admin" {
			continue
		}
		has = append(has, p+": "+cmp.Or(perms[p], "none"))
		need = append(need, p+": write")
	}
	if len(need) == 0 {
		return ""
	}
	return fmt.Sprintf("the %s App %s is installed with %s; devbot needs %s, so grant it in the App's settings and accept the change on the installation",
		a.role, strings.TrimSuffix(a.login, "[bot]"), strings.Join(has, ", "), strings.Join(need, ", "))
}

// CheckInstallations reads every installation of each App this process
// holds the key of, and says what each lacks of what CanWrite requires, so
// a misconfigured App shows at startup rather than as repos devbot ignores.
func (c *Client) CheckInstallations(ctx context.Context) ([]string, error) {
	var out []string
	for _, a := range []*app{c.impl, c.rev} {
		if a.key == nil {
			continue
		}
		jwt, err := c.appCred(a)
		if err != nil {
			return out, err
		}
		installs, err := list[struct {
			installation
			Account struct {
				Login string `json:"login"`
			} `json:"account"`
			SuspendedAt *time.Time `json:"suspended_at"`
		}](ctx, c, jwt, "/app/installations")
		if err != nil {
			return out, fmt.Errorf("github: the %s App's installations: %w", a.role, err)
		}
		for _, in := range installs {
			if need := missingPermissions(a, in.Permissions); need != "" && in.SuspendedAt == nil {
				out = append(out, fmt.Sprintf("on %s, %s", in.Account.Login, need))
			}
		}
	}
	return out, nil
}

// Authorized asks GitHub for actor's role on r (docs: "Get repository
// permissions for a user"), which includes what org base permissions and
// teams grant. role_name is the real role; a custom role's name means
// nothing here, so it falls back to permission, the base role it builds on.
// GitHub answers 404 both for a login that isn't a user and for a repo the
// App can't see, so a 404 is told apart by whether the App can read the
// repo.
func (c *Client) Authorized(ctx context.Context, r forge.RepoRef, actor string, min forge.Permission) (bool, error) {
	if actor == "" {
		return false, nil
	}
	a := c.app(c.acting())
	forbidden := func(err error) error {
		return &forge.LookupForbiddenError{
			Need: fmt.Sprintf("the %s App %s needs to be installed on %s with Metadata: read to look up collaborators' permissions", a.role, a.login, r.FullName()),
			Err:  err,
		}
	}
	cr, err := c.repoCred(ctx, c.acting(), r)
	if errors.Is(err, forge.ErrNotFound) {
		return false, forbidden(err)
	}
	if err != nil {
		return false, err
	}
	var p struct {
		Permission string `json:"permission"`
		RoleName   string `json:"role_name"`
	}
	_, err = c.do(ctx, cr, "GET", repoPath(r)+"/collaborators/"+url.PathEscape(actor)+"/permission", nil, &p)
	if errors.Is(err, forge.ErrNotFound) {
		if _, rerr := c.do(ctx, cr, "GET", repoPath(r), nil, nil); rerr != nil {
			if errors.Is(rerr, forge.ErrNotFound) || errors.Is(rerr, forge.ErrForbidden) {
				return false, forbidden(err)
			}
			return false, rerr
		}
		return false, nil
	}
	if errors.Is(err, forge.ErrForbidden) {
		return false, forbidden(err)
	}
	if err != nil {
		return false, err
	}
	perm, ok := githubRole(p.RoleName)
	if !ok {
		perm, _ = githubRole(p.Permission)
	}
	return perm >= min, nil
}

// githubRole maps GitHub's repository roles onto devbot's. Triage, which
// can label and close issues but not push, is read here.
func githubRole(s string) (forge.Permission, bool) {
	switch s {
	case "admin":
		return forge.PermAdmin, true
	case "maintain":
		return forge.PermMaintain, true
	case "write":
		return forge.PermWrite, true
	case "triage", "read":
		return forge.PermRead, true
	case "none":
		return forge.PermNone, true
	}
	return forge.PermNone, false
}

func (c *Client) repoWeb(r forge.RepoRef) string {
	return c.web + "/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

func (c *Client) IssueURL(ref forge.ItemRef) string {
	return fmt.Sprintf("%s/issues/%d", c.repoWeb(ref.Repo), ref.Number)
}

func (c *Client) PRURL(r forge.RepoRef, number int) string {
	return fmt.Sprintf("%s/pull/%d", c.repoWeb(r), number)
}

func (c *Client) BranchURL(r forge.RepoRef, branch string) string {
	return c.repoWeb(r) + "/tree/" + escapePath(branch)
}

func (c *Client) BranchExists(ctx context.Context, r forge.RepoRef, branch string) (bool, error) {
	cr, err := c.repoCred(ctx, forge.RoleImplementer, r)
	if err != nil {
		return false, err
	}
	_, err = c.do(ctx, cr, "GET", repoPath(r)+"/branches/"+escapePath(branch), nil, nil)
	if errors.Is(err, forge.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// ListBranches reads the branch refs under prefix (docs: "List matching
// references"), and each head's commit date from the git data API, which a
// SHA's date never changes, so it is only asked once per SHA.
func (c *Client) ListBranches(ctx context.Context, r forge.RepoRef, prefix string) ([]forge.Branch, error) {
	cr, err := c.repoCred(ctx, forge.RoleImplementer, r)
	if err != nil {
		return nil, err
	}
	refs, err := list[struct {
		Ref    string `json:"ref"`
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}](ctx, c, cr, repoPath(r)+"/git/matching-refs/heads/"+escapePath(prefix))
	if err != nil {
		return nil, err
	}
	var out []forge.Branch
	for _, ref := range refs {
		name, ok := strings.CutPrefix(ref.Ref, "refs/heads/")
		if !ok || !strings.HasPrefix(name, prefix) || ref.Object.Type != "commit" {
			continue
		}
		at, err := c.committedAt(ctx, cr, r, ref.Object.SHA)
		if err != nil {
			return nil, err
		}
		out = append(out, forge.Branch{Name: name, SHA: ref.Object.SHA, CommittedAt: at})
	}
	return out, nil
}

func (c *Client) committedAt(ctx context.Context, cr cred, r forge.RepoRef, sha string) (time.Time, error) {
	c.mu.Lock()
	at, ok := c.committed[sha]
	c.mu.Unlock()
	if ok {
		return at, nil
	}
	var cm struct {
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	}
	if _, err := c.do(ctx, cr, "GET", repoPath(r)+"/git/commits/"+url.PathEscape(sha), nil, &cm); err != nil {
		return time.Time{}, err
	}
	c.mu.Lock()
	// Bounded by forgetting all of them now and then: each is one call to
	// read again.
	if len(c.committed) >= 10000 {
		clear(c.committed)
	}
	c.committed[sha] = cm.Committer.Date
	c.mu.Unlock()
	return cm.Committer.Date, nil
}

// DeleteBranch deletes the branch's ref (docs: "Delete a reference"). It
// refuses the attachments branch, which holds every open PR's files.
func (c *Client) DeleteBranch(ctx context.Context, r forge.RepoRef, name string) error {
	if name == attachBranch {
		return fmt.Errorf("github: %s holds PR attachments and is never deleted", attachBranch)
	}
	cr, err := c.repoCred(ctx, forge.RoleImplementer, r)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, cr, "DELETE", repoPath(r)+"/git/refs/heads/"+escapePath(name), nil, nil)
	// GitHub answers 422 "Reference does not exist" for a branch that's gone.
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity && strings.Contains(ae.Body, "Reference does not exist") {
		return forge.ErrNotFound
	}
	return err
}

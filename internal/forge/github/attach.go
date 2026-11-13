package github

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// GitHub's REST API can't upload a file to an issue or PR (the web UI's
// drag and drop isn't an API), so attachments live in the repo itself, on
// a branch that is never merged: pr-<n>/<sha256 prefix>-<name>. A blob URL
// with ?raw=true renders in markdown, inline images included, for anyone
// who can read the repo, so a private repo's files stay private. Gists
// (reachable by anyone with the link) and release assets (public on a
// public repo, and cluttering its releases) were rejected for that reason.
const attachBranch = "devbot/attachments"

const attachReadme = `# devbot/attachments

devbot keeps the files it attaches to its pull requests here (screenshots,
logs), one directory per PR, because GitHub has no API to upload a file to a
PR. A PR's directory is deleted when the PR is closed or merged. This branch
is never merged; don't base work on it.
`

func attachDir(number int) string { return fmt.Sprintf("pr-%d", number) }

// attachName is the file name a PR's attachment is stored under. The
// content's hash makes a repeat upload the same path, and the name is cut
// down to characters that need no escaping in a URL or markdown.
var attachUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

var attachFile = regexp.MustCompile(`^[0-9a-f]{12}-[A-Za-z0-9._-]{1,100}$`)

func attachName(name string, data []byte) string {
	clean := strings.TrimLeft(attachUnsafe.ReplaceAllString(path.Base(strings.ReplaceAll(name, `\`, "/")), "-"), ".-")
	if len(clean) > 100 {
		clean = clean[len(clean)-100:]
	}
	if clean == "" {
		clean = "file"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12] + "-" + clean
}

func (c *Client) attachURL(r forge.RepoRef, number int, file string) string {
	return c.repoWeb(r) + "/blob/" + attachBranch + "/" + attachDir(number) + "/" + file + "?raw=true"
}

// ProtectedBranchError is a write to the attachments branch that branch
// protection or a ruleset refused.
type ProtectedBranchError struct{ Err error }

func (e *ProtectedBranchError) Error() string {
	return "github: branch protection or a ruleset refused the write to " + attachBranch +
		"; attaching files needs the implementer App exempted (a bypass for " + attachBranch + "): " + e.Err.Error()
}

func (e *ProtectedBranchError) Unwrap() error { return e.Err }

// protected names the exemption a write needs when GitHub refused it for
// the branch's protection or a ruleset. GitHub's docs don't give the shape
// of that refusal, only the statuses (409, 422); its message names a
// protected branch or a rule violation.
func protected(err error) error {
	var ae *apiError
	if !errors.As(err, &ae) || (ae.Status != http.StatusConflict && ae.Status != http.StatusUnprocessableEntity && ae.Status != http.StatusForbidden) {
		return err
	}
	msg := strings.ToLower(ae.Body)
	if strings.Contains(msg, "protected branch") || strings.Contains(msg, "rule violation") {
		return &ProtectedBranchError{Err: err}
	}
	return err
}

// AttachToPR commits the file to pr-<n>/ on the attachments branch with
// the contents API (docs: "Repository contents", create or update file
// contents), creating the branch first if it's missing. Uploading the same
// file again returns the same URL without a new commit.
func (c *Client) AttachToPR(ctx context.Context, r forge.RepoRef, number int, name string, data []byte) (string, error) {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return "", err
	}
	if _, err := c.attachHead(ctx, cr, r, true); err != nil {
		return "", err
	}
	file := attachName(name, data)
	p := repoPath(r) + "/contents/" + attachDir(number) + "/" + file
	_, err = c.do(ctx, cr, "PUT", p, map[string]any{
		"message": fmt.Sprintf("Attach %s to #%d", file, number),
		"content": base64.StdEncoding.EncodeToString(data),
		"branch":  attachBranch,
	}, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity {
		// The path is already there (GitHub wants the old blob's sha to
		// replace a file). It's named for its content, so it's this file.
		have, _, gerr := c.send(ctx, cr, "GET", p+"?ref="+url.QueryEscape(attachBranch), "application/vnd.github.raw+json", nil)
		if gerr == nil && bytes.Equal(have, data) {
			err = nil
		}
	}
	if err != nil {
		return "", fmt.Errorf("github: attach %s to %s#%d: %w", file, r, number, protected(err))
	}
	return c.attachURL(r, number, file), nil
}

// attachHead is the attachments branch's head commit. When create is set,
// a missing branch is created as an orphan with one README commit, with the
// git data API (docs: "Git trees", "Git commits", "Git references"), so
// none of the repo's history is copied into it. Otherwise a missing branch
// is "".
func (c *Client) attachHead(ctx context.Context, cr cred, r forge.RepoRef, create bool) (string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	_, err := c.do(ctx, cr, "GET", repoPath(r)+"/git/ref/heads/"+escapePath(attachBranch), nil, &ref)
	switch {
	case err == nil:
		return ref.Object.SHA, nil
	case !errors.Is(err, forge.ErrNotFound):
		return "", err
	case !create:
		return "", nil
	}
	var tree, commit struct {
		SHA string `json:"sha"`
	}
	if _, err := c.do(ctx, cr, "POST", repoPath(r)+"/git/trees", map[string]any{
		"tree": []map[string]string{{"path": "README.md", "mode": "100644", "type": "blob", "content": attachReadme}},
	}, &tree); err != nil {
		return "", fmt.Errorf("github: create %s: %w", attachBranch, err)
	}
	if _, err := c.do(ctx, cr, "POST", repoPath(r)+"/git/commits", map[string]any{
		"message": "Start " + attachBranch, "tree": tree.SHA, "parents": []string{},
	}, &commit); err != nil {
		return "", fmt.Errorf("github: create %s: %w", attachBranch, err)
	}
	_, err = c.do(ctx, cr, "POST", repoPath(r)+"/git/refs", map[string]string{"ref": "refs/heads/" + attachBranch, "sha": commit.SHA}, nil)
	if err != nil {
		// Another upload may have created it first: then use theirs.
		if head, gerr := c.attachHead(ctx, cr, r, false); gerr == nil && head != "" {
			return head, nil
		}
		return "", fmt.Errorf("github: create %s: %w", attachBranch, protected(err))
	}
	return commit.SHA, nil
}

// attachedPath is the path on the attachments branch that raw, a URL
// AttachToPR returned for PR number, points at. It accepts nothing else:
// the URL is read from a PR body anyone with write access can edit.
func (c *Client) attachedPath(r forge.RepoRef, number int, raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	want, err := url.Parse(c.repoWeb(r) + "/blob/" + attachBranch + "/" + attachDir(number) + "/")
	if err != nil || u.Scheme != want.Scheme || u.Host != want.Host || u.User != nil || u.Fragment != "" ||
		(u.RawQuery != "" && u.RawQuery != "raw=true") {
		return "", false
	}
	file, ok := strings.CutPrefix(u.Path, want.Path)
	if !ok || !attachFile.MatchString(file) {
		return "", false
	}
	return attachDir(number) + "/" + file, true
}

// GetAttachment reads a file of this PR's from the attachments branch,
// through the contents API as role. Only the path is taken from the URL,
// and only once it's known to be in this PR's directory.
func (c *Client) GetAttachment(ctx context.Context, r forge.RepoRef, number int, raw string, role forge.Role) ([]byte, error) {
	p, ok := c.attachedPath(r, number, raw)
	if !ok {
		return nil, fmt.Errorf("github: %q is not a file attached to %s#%d", raw, r, number)
	}
	cr, err := c.repoCred(ctx, role, r)
	if err != nil {
		return nil, err
	}
	b, _, err := c.send(ctx, cr, "GET", repoPath(r)+"/contents/"+p+"?ref="+url.QueryEscape(attachBranch), "application/vnd.github.raw+json", nil)
	return b, err
}

type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// ReleaseAttachments deletes the PR's directory from the attachments
// branch in one commit: a root tree without it, committed on the head and
// moved to as a fast-forward. If an upload moved the branch meanwhile, it
// starts again from the new head.
func (c *Client) ReleaseAttachments(ctx context.Context, r forge.RepoRef, number int) error {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return err
	}
	dir := attachDir(number)
	for attempt := 0; ; attempt++ {
		head, err := c.attachHead(ctx, cr, r, false)
		if err != nil || head == "" {
			return err
		}
		var commit struct {
			Tree struct {
				SHA string `json:"sha"`
			} `json:"tree"`
		}
		if _, err := c.do(ctx, cr, "GET", repoPath(r)+"/git/commits/"+url.PathEscape(head), nil, &commit); err != nil {
			return err
		}
		var tree struct {
			Tree      []treeEntry `json:"tree"`
			Truncated bool        `json:"truncated"`
		}
		if _, err := c.do(ctx, cr, "GET", repoPath(r)+"/git/trees/"+url.PathEscape(commit.Tree.SHA), nil, &tree); err != nil {
			return err
		}
		if tree.Truncated {
			// A tree built from part of the list would drop the rest.
			return fmt.Errorf("github: %s's root tree is too large to list", attachBranch)
		}
		keep := make([]treeEntry, 0, len(tree.Tree))
		for _, e := range tree.Tree {
			if e.Path != dir {
				keep = append(keep, e)
			}
		}
		if len(keep) == len(tree.Tree) {
			return nil
		}
		var nt, nc struct {
			SHA string `json:"sha"`
		}
		if _, err := c.do(ctx, cr, "POST", repoPath(r)+"/git/trees", map[string]any{"tree": keep}, &nt); err != nil {
			return err
		}
		if _, err := c.do(ctx, cr, "POST", repoPath(r)+"/git/commits", map[string]any{
			"message": fmt.Sprintf("Remove #%d's attachments", number), "tree": nt.SHA, "parents": []string{head},
		}, &nc); err != nil {
			return err
		}
		_, err = c.do(ctx, cr, "PATCH", repoPath(r)+"/git/refs/heads/"+escapePath(attachBranch), map[string]any{"sha": nc.SHA, "force": false}, nil)
		if err == nil {
			return nil
		}
		err = protected(err)
		var ae *apiError
		if attempt >= 2 || errors.As(err, new(*ProtectedBranchError)) || !errors.As(err, &ae) || ae.Status != http.StatusUnprocessableEntity {
			return fmt.Errorf("github: remove %s from %s: %w", dir, attachBranch, err)
		}
	}
}

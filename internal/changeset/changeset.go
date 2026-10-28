// Package changeset lists what a branch changes, from git itself: every path,
// with its old and new mode, and nothing capped or paged the way a forge's
// file list is. The harness checks it before pushing, and the merge gate
// before merging.
package changeset

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Change is one path a branch touches. Renames and copies are never
// detected, so a move is a delete of the old path and an add of the new one.
type Change struct {
	Status  string // A, D, M or T, as git diff --raw reports it
	Path    string
	OldMode string // "000000" for an added path
	NewMode string // "000000" for a deleted path
	// OldBlob and NewBlob are the object ids on each side, all zeros where
	// the path doesn't exist: what the path holds, which an approval of a
	// protected change is bound to.
	OldBlob, NewBlob string
}

// Modes git uses for links rather than files.
const (
	ModeSymlink = "120000"
	ModeGitlink = "160000" // a submodule pointer
)

// Compute lists what head changes against its merge base with base, in the
// git repo at dir. With more than one merge base it is the union of the diff
// against each, so nothing a branch changes can hide behind the choice.
func Compute(ctx context.Context, dir, base, head string) ([]Change, error) {
	bases, err := git(ctx, dir, "", "merge-base", "--all", base, head)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(bases) == "" {
		return nil, fmt.Errorf("changeset: %s and %s have no merge base", base, head)
	}
	var all []Change
	seen := map[Change]bool{}
	for _, mb := range strings.Fields(bases) {
		// --ignore-submodules=none, so a repo's diff.ignoreSubmodules can't
		// hide a gitlink change.
		out, err := git(ctx, dir, "", "diff", "--raw", "-z", "--no-renames", "--no-abbrev", "--no-ext-diff",
			"--ignore-submodules=none", mb, head, "--")
		if err != nil {
			return nil, err
		}
		cs, err := Parse(out)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if !seen[c] {
				seen[c] = true
				all = append(all, c)
			}
		}
	}
	return all, nil
}

// Fetch fetches the base branch and head, a ref (a fork PR's head is
// refs/pull/<n>/head, as the forges keep it, not a branch of url's), from url
// into a throwaway repo under tmp and computes head's change set there. It
// fails unless the head it fetched is wantHead, so the answer is about the
// commit being judged.
// Only trees are fetched: modes and paths are all a change set needs.
func Fetch(ctx context.Context, tmp, url, header, base, head, wantHead string) ([]Change, error) {
	dir, err := os.MkdirTemp(tmp, "devbot-changeset-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if _, err := git(ctx, dir, "", "init", "--quiet", "--bare"); err != nil {
		return nil, err
	}
	if _, err := git(ctx, dir, header, "fetch", "--quiet", "--no-tags", "--filter=blob:none", url,
		"+refs/heads/"+base+":refs/changeset/base", "+"+head+":refs/changeset/head"); err != nil {
		return nil, err
	}
	got, err := git(ctx, dir, "", "rev-parse", "--verify", "refs/changeset/head^{commit}")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(got) != wantHead {
		return nil, fmt.Errorf("changeset: %s is at %s, not %s", head, strings.TrimSpace(got), wantHead)
	}
	return Compute(ctx, dir, "refs/changeset/base", "refs/changeset/head")
}

// Parse reads git diff --raw -z output. Anything it doesn't recognise is an
// error, not skipped: a change set that can't be read fully isn't one.
func Parse(raw string) ([]Change, error) {
	var cs []Change
	f := strings.Split(raw, "\x00")
	if len(f) > 0 && f[len(f)-1] == "" {
		f = f[:len(f)-1]
	}
	for i := 0; i < len(f); {
		meta := strings.Fields(strings.TrimPrefix(f[i], ":"))
		if !strings.HasPrefix(f[i], ":") || len(meta) != 5 || meta[4] == "" {
			return nil, fmt.Errorf("changeset: unexpected git diff output %q", f[i])
		}
		c := Change{OldMode: meta[0], NewMode: meta[1], OldBlob: meta[2], NewBlob: meta[3], Status: meta[4][:1]}
		paths := 1
		if c.Status == "R" || c.Status == "C" {
			paths = 2
		}
		if i+paths >= len(f) {
			return nil, errors.New("changeset: git diff output ends mid-entry")
		}
		for _, p := range f[i+1 : i+1+paths] {
			if p == "" {
				return nil, errors.New("changeset: git diff output has an empty path")
			}
			c.Path = p
			cs = append(cs, c)
		}
		i += 1 + paths
	}
	return cs, nil
}

// git runs git in dir with repo config that could execute anything
// disabled, returning stdout alone so warnings can't corrupt what is parsed.
// header, when set, authenticates the command and is redacted from errors.
func git(ctx context.Context, dir, header string, args ...string) (string, error) {
	pre := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "protocol.file.allow=always"}
	if header != "" {
		pre = append(pre, "-c", "http.extraHeader="+header)
	}
	cmd := exec.CommandContext(ctx, "git", append(pre, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		s := strings.TrimSpace(errb.String())
		if header != "" {
			s = strings.ReplaceAll(s, header, "[redacted]")
		}
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, s)
	}
	return out.String(), nil
}

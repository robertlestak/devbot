package harness

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// A task whose wait builds against another task's unmerged work (see
// core.BuildAgainst) gets that work's PR head checked out under
// core.DepsRoot in the model's repo. The harness fetches it into a private
// repo of its own, with the implementer's credential for that task's repo,
// and hands the model plain files: no .git, no remote, no credential. devbot
// wires nothing into the build, knowing no ecosystem: the model does, the way
// its project uses a local copy of a dependency, and keeps that wiring out of
// its commits, which push refuses otherwise (depsWiring).

// maxDepBytes caps the files a checkout hands the model.
const maxDepBytes = 1 << 30

// checkoutDep checks out o's PR head at o.Dir in w's shared repo, replacing
// whatever an earlier run left there, and returns the commit it is.
func (r *Runner) checkoutDep(ctx context.Context, w *workspace, o *core.Override) (string, error) {
	p, err := r.E.Forges.For(o.On.Repo)
	if err != nil {
		return "", err
	}
	url, header, err := p.GitAuth(ctx, o.On.Repo, forge.RoleImplementer)
	if err != nil {
		return "", err
	}
	w.audit.Secret(header, header[strings.LastIndex(header, " ")+1:])
	dir := filepath.Join(filepath.Dir(w.priv), "dep-"+randomID())
	if _, err := runGit(ctx, filepath.Dir(w.priv), "", false, "init", "--quiet", "--bare", dir); err != nil {
		return "", err
	}
	// The PR's ref, not its branch: the forge keeps it, merged or not, and
	// for a fork's PR too.
	head := fmt.Sprintf("refs/pull/%d/head", o.PR.Number)
	if _, err := runGit(ctx, dir, header, true, "fetch", "--quiet", "--no-tags", "--depth=1", url, "+"+head+":refs/devbot/dep"); err != nil {
		return "", fmt.Errorf("fetching %s of %s: %w", head, o.On.Repo, err)
	}
	sha, err := runGit(ctx, dir, "", false, "rev-parse", "--verify", "refs/devbot/dep^{commit}")
	if err != nil {
		return "", err
	}
	cmd := gitCmd(ctx, dir, "", false, "archive", "--format=tar", sha)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	err = w.untarShared(o.Dir, out)
	io.Copy(io.Discard, out)
	if werr := cmd.Wait(); err == nil && werr != nil {
		err = fmt.Errorf("git archive: %v: %s", werr, strings.TrimSpace(stderr.String()))
	}
	if err != nil {
		return "", fmt.Errorf("checking out %s at %s: %w", o.On.Repo, o.Dir, err)
	}
	return sha, nil
}

// untarShared writes the tar stream r into dir of the model's repo, through
// os.Root, so neither a symlink committed to the repo nor one in the
// archive can send a write outside it. Everything is left writable from
// the agent side, whatever uid it runs as: a project's tools may write into
// a local copy of a dependency.
func (w *workspace) untarShared(dir string, r io.Reader) error {
	root, err := os.OpenRoot(w.repo)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.RemoveAll(dir); err != nil {
		return err
	}
	mkdir := func(p string) error {
		if err := root.MkdirAll(p, 0o777); err != nil {
			return err
		}
		return root.Chmod(p, 0o777)
	}
	if err := mkdir(dir); err != nil {
		return err
	}
	var total int64
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(h.Name)
		if h.Typeflag == tar.TypeXGlobalHeader || name == "." {
			continue
		}
		if !filepath.IsLocal(name) {
			return fmt.Errorf("%q is outside the checkout", h.Name)
		}
		p := path.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			err = mkdir(p)
		case tar.TypeReg:
			if total += h.Size; total > maxDepBytes {
				return fmt.Errorf("its files are over the %d MB limit", maxDepBytes>>20)
			}
			err = w.writeDepFile(root, p, h, tr, mkdir)
		case tar.TypeSymlink:
			if err = mkdir(path.Dir(p)); err == nil {
				err = root.Symlink(h.Linkname, p)
			}
		}
		if err != nil {
			return err
		}
	}
}

func (w *workspace) writeDepFile(root *os.Root, p string, h *tar.Header, r io.Reader, mkdir func(string) error) error {
	if err := mkdir(path.Dir(p)); err != nil {
		return err
	}
	mode := os.FileMode(0o666)
	if h.Mode&0o111 != 0 {
		mode = 0o777
	}
	f, err := root.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return root.Chmod(p, mode)
}

// depsContext is what context.md tells a run about the work it builds
// against, checked out at commit sha. The prompt (Vars.Deps) says how to
// use it; this says what it is.
func depsContext(own forge.RepoRef, o *core.Override, sha string) string {
	state := "still open"
	if o.PR.Merged {
		state = "merged, but no release has it yet"
	}
	return fmt.Sprintf("## Building against an unmerged dependency\n\n"+
		"This task builds against the work of %s in %s, which isn't released yet: its PR #%d (%s) is %s. devbot has checked out that PR's head, `%s`, at `%s/` in this workspace, as plain files without git history.\n\n"+
		"Use it the way this project uses a local checkout of a dependency, and write this task's change against the interface that PR has. **Keep that local wiring out of your commits**: the dependency's version is set once %s releases the change, when devbot sends this task's PR back for a round that depends on the release. devbot refuses to push work whose diff mentions `%s`, and fails the attempt. "+
		"Until the release, this task's PR isn't reviewed, merged or sent back for its CI, which builds against the published %s, without the change.\n",
		core.RefText(own, o.On), o.On.Repo, o.PR.Number, o.PR.URL, state, sha, o.Dir, o.On.Repo, core.DepsRoot, o.On.Repo)
}

// depsError is work whose diff mentions core.DepsRoot: local wiring of a
// dependency checkout, which push never sends.
type depsError struct{ Files []string }

func (e *depsError) Error() string {
	return fmt.Sprintf("the work wasn't pushed: its diff mentions `%s`, where devbot checks out the unmerged work this task builds against. That local wiring stays out of the commits (in `.devbot/setup.sh` or files listed in `.git/info/exclude`): the dependency's version is set once its work is released. In:\n- %s",
		core.DepsRoot, strings.Join(e.Files, "\n- "))
}

// maxDepsFiles bounds the files a depsError names.
const maxDepsFiles = 10

// depsWiring lists the files whose changes, against the base, name
// core.DepsRoot: a path under it, or an added line that mentions it, in any
// ecosystem's form. The diff is read as a stream, so a large one costs no
// memory.
func (w *workspace) depsWiring(ctx context.Context, paths []string) ([]string, error) {
	var hit []string
	add := func(f string) bool {
		if !contains(hit, f) {
			hit = append(hit, f)
		}
		return len(hit) == maxDepsFiles
	}
	for _, p := range paths {
		if strings.Contains(p, core.DepsRoot) && add(p) {
			return hit, nil
		}
	}
	cmd := gitCmd(ctx, w.priv, "", false, "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "-U0",
		"refs/remotes/origin/"+w.base+"...refs/devbot/work")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	err = addedMentions(out, []byte(core.DepsRoot), add)
	io.Copy(io.Discard, out)
	if werr := cmd.Wait(); err == nil && werr != nil {
		err = fmt.Errorf("git diff: %v: %s", werr, strings.TrimSpace(stderr.String()))
	}
	return hit, err
}

// addedMentions reads a unified diff from r and calls found with each file
// that has an added line containing needle, until it returns true. Lines
// of any length are read in pieces, a match across two of them included.
func addedMentions(r io.Reader, needle []byte, found func(string) bool) error {
	br := bufio.NewReaderSize(r, 64<<10)
	file, start, added := "", true, false
	var tail []byte
	for {
		frag, more, err := br.ReadLine()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if start {
			if f, ok := bytes.CutPrefix(frag, []byte("+++ ")); ok {
				file = strings.TrimPrefix(strings.Trim(string(f), `"`), "b/")
			}
			added = bytes.HasPrefix(frag, []byte("+")) && !bytes.HasPrefix(frag, []byte("+++ "))
			tail = tail[:0]
		}
		if added {
			buf := append(tail, frag...)
			if bytes.Contains(buf, needle) {
				if found(file) {
					return nil
				}
				added = false
			}
			tail = append(tail[:0], buf[max(0, len(buf)-len(needle)+1):]...)
		}
		start = !more
	}
}

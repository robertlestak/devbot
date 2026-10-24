package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/audit"
	"git.shdw.tech/rob/devbot/internal/changeset"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// workspace is one job's two git checkouts.
//
//   - priv: the harness's own clone, in this container's filesystem. It holds
//     the forge credential (per command, never in config) and is the only
//     repo anything is pushed from. The model can't see or touch it.
//   - repo: the model's working copy, in the volume shared with the agent
//     container. The harness never runs git there after handing it over,
//     because the model controls its .git/config and hooks.
//
// Work comes back from repo to priv as a git bundle: a file of objects and
// refs, which fetching cannot execute anything from.
type workspace struct {
	priv   string // private clone
	shared string // shared job dir (agent sees it)
	repo   string // shared/repo
	base   string
	branch string

	// The forge and repo git authenticates against, as role. The header is
	// asked for again before every authenticated command: a GitHub
	// installation token lasts an hour, and a job can outlast it.
	provider forge.Provider
	remote   forge.RepoRef

	// Pushes are audited as actor, the bot whose credential header is.
	audit *audit.Logger
	actor string
	role  forge.Role

	// carried is branch being a PR devbot carries for someone, not its own
	// (see push). rewrote is set when a push to it had to rewrite its
	// history.
	carried, rewrote bool
	// name and email are devbot's git identity, which the agent commits
	// as, and harness the agent CLI that ran (see attribute).
	name, email, harness string
	// head is the ref branch's head was fetched to, which a push that
	// rewrites a carried branch leases, and pushTo the remote a push goes
	// to: origin, or the fork a carried PR's branch is in.
	head, pushTo string
	// approved reports whether an approval on the task covers what a
	// change leaves in protected files a person may approve; without one,
	// push holds the work instead (heldError).
	approved func([]core.HeldFile) bool
	// held is work fetched back from its held ref (releaseHeld), whose
	// commits were attributed before they were held: push sends them as
	// they are, the very commit a person approved.
	held bool
	// depsGuard is set for a task that has built against another task's
	// unmerged work: push refuses work whose diff mentions core.DepsRoot.
	depsGuard bool
}

func randomID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// git runs git in the private clone with repo config that could execute
// anything disabled, and with the auth header only when asked.
func (w *workspace) git(ctx context.Context, auth bool, args ...string) (string, error) {
	header := ""
	if auth {
		var err error
		if _, header, err = w.auth(ctx); err != nil {
			return "", err
		}
	}
	return runGit(ctx, w.priv, header, auth, args...)
}

// auth is the clone URL and a header that is current now, from the forge.
func (w *workspace) auth(ctx context.Context) (string, string, error) {
	url, header, err := w.provider.GitAuth(ctx, w.remote, w.role)
	if err != nil {
		return "", "", err
	}
	// The credential must never reach an audit record, whatever git says.
	w.audit.Secret(header, header[strings.LastIndex(header, " ")+1:])
	return url, header, nil
}

func runGit(ctx context.Context, dir, header string, auth bool, args ...string) (string, error) {
	return runGitIO(ctx, dir, header, auth, nil, "", args...)
}

// runGitIO is runGit with env added to git's environment and stdin as its
// input.
func runGitIO(ctx context.Context, dir, header string, auth bool, env []string, stdin string, args ...string) (string, error) {
	cmd := gitCmd(ctx, dir, header, auth, args...)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	s := strings.TrimSpace(out.String())
	if err != nil {
		// Never echo the header back into logs or comments.
		s = strings.ReplaceAll(s, header, "[redacted]")
		return s, fmt.Errorf("git %s: %v: %s", args[0], err, s)
	}
	return s, nil
}

// gitCmd is git in dir with repo config that could execute anything
// disabled, and the auth header only when asked.
func gitCmd(ctx context.Context, dir, header string, auth bool, args ...string) *exec.Cmd {
	pre := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "protocol.file.allow=always"}
	if auth && header != "" {
		pre = append(pre, "-c", "http.extraHeader="+header)
	}
	cmd := exec.CommandContext(ctx, "git", append(pre, args...)...)
	cmd.Dir = dir
	// No user's ignore or attributes files either: the harness runs
	// unprivileged, with a $HOME it can't read, and git's warning about one
	// would land in output that is parsed.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "XDG_CONFIG_HOME=/dev/null")
	return cmd
}

// headSource is where prepare finds a branch that isn't origin's own.
type headSource struct {
	// Fork is the repo, on origin's forge, that branch is in, which, with
	// Push, the work is pushed to. Its head is never read from there, but
	// from origin's Head, so a fork devbot can't read still starts. Push
	// without a Fork and a Head is an error, never origin's branch of that
	// name: that is often the base.
	Fork forge.RepoRef
	Push bool
	// Start is a ref on origin that a branch origin doesn't have yet starts
	// from: the head of a fork's PR (refs/pull/<n>/head) that devbot carries
	// on a branch of its own.
	Start string
	// Head is a ref on origin read in place of origin's branch of that
	// name: a fork's PR's refs/pull/<n>/head, which the forge keeps even
	// once the fork is deleted.
	Head string
}

// prepare clones privately, checks out branch (existing on the remote, or new
// from base, or as from says), and hands a copy to the agent.
func (r *Runner) prepare(ctx context.Context, p forge.Provider, repo forge.RepoRef, role forge.Role, base, branch, name, email string, from headSource) (*workspace, error) {
	id := randomID()
	privRoot := r.PrivateDir
	if privRoot == "" {
		privRoot = os.TempDir()
	}
	w := &workspace{
		priv:   filepath.Join(privRoot, "devbot-"+id, "src"),
		shared: filepath.Join(r.Workdir, id),
		base:   base, branch: branch,
		name: name, email: email,
		provider: p, remote: repo,
		head: "refs/remotes/origin/" + branch, pushTo: "origin",
	}
	w.repo = filepath.Join(w.shared, "repo")
	impl, rev := p.BotUsers()
	w.audit, w.actor, w.role = r.E.Audit, impl, role
	if role == forge.RoleReviewer {
		w.actor = rev
	}
	url, header, err := w.auth(ctx)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(w.priv), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(w.shared, 0o777); err != nil {
		return nil, err
	}
	os.Chmod(w.shared, 0o777)
	if _, err := runGit(ctx, filepath.Dir(w.priv), header, true, "clone", "--quiet", "--no-tags", url, w.priv); err != nil {
		return nil, err
	}
	start := "refs/remotes/origin/" + base
	if from.Push && (from.Fork == (forge.RepoRef{}) || from.Head == "") {
		return nil, fmt.Errorf("no fork to push %s to", branch)
	}
	if from.Head != "" {
		// To a ref of its own: the fork's branch may share its name with
		// one of origin's, the base even.
		w.head = "refs/devbot/head"
		if _, err := w.git(ctx, true, "fetch", "--quiet", "--no-tags", "origin", "+"+from.Head+":"+w.head); err != nil {
			return nil, fmt.Errorf("fetching %s: %w", from.Head, err)
		}
		start = w.head
		if from.Push {
			fork, err := core.ForkURL(url, repo, from.Fork)
			if err != nil {
				return nil, err
			}
			w.pushTo = fork
			// A dry run is let in only where devbot's credential may push,
			// and sends nothing.
			if out, err := w.git(ctx, true, "push", "--dry-run", "--quiet", "--no-force", fork, w.head+":refs/heads/"+branch); err != nil && core.ForkRefused(out) {
				w.cleanup()
				return nil, &forkRefusedError{Branch: branch, Reason: out}
			}
		}
	} else if _, err := w.git(ctx, false, "rev-parse", "--verify", "--quiet", w.head); err == nil {
		start = w.head
	} else if from.Start != "" {
		if _, err := w.git(ctx, true, "fetch", "--quiet", "--no-tags", "origin", "+"+from.Start+":refs/devbot/head"); err != nil {
			return nil, fmt.Errorf("fetching %s: %w", from.Start, err)
		}
		start = "refs/devbot/head"
	}
	if _, err := w.git(ctx, false, "checkout", "--quiet", "-B", branch, start); err != nil {
		return nil, err
	}

	// The model's copy. Everything the harness does here happens before the
	// agent is asked to do anything, so the repo is still ours to configure.
	if _, err := runGit(ctx, w.shared, "", false, "clone", "--quiet", "--no-hardlinks", w.priv, w.repo); err != nil {
		return nil, err
	}
	steps := [][]string{
		{"fetch", "--quiet", w.priv, "+refs/remotes/origin/*:refs/remotes/origin/*"},
		{"remote", "set-url", "origin", url},
		{"config", "user.name", name},
		{"config", "user.email", email},
	}
	for _, s := range steps {
		if _, err := runGit(ctx, w.repo, "", false, s...); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Join(w.repo, ".devbot"), 0o777); err != nil {
		return nil, err
	}
	excl := filepath.Join(w.repo, ".git", "info", "exclude")
	os.MkdirAll(filepath.Dir(excl), 0o755)
	f, err := os.OpenFile(excl, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(f, ".devbot/")
	f.Close()
	// Everything in the shared volume must be writable from the agent side,
	// whatever uid each container runs as.
	exec.CommandContext(ctx, "chmod", "-R", "a+rwX", w.shared).Run()
	return w, nil
}

// collect has the agent commit anything left uncommitted and bundle its
// commits, then fetches the bundle into the private clone as refs/devbot/work.
// It returns how many commits the branch is ahead of the base.
func (w *workspace) collect(ctx context.Context, ag agent.Runner, name, email string) (int, error) {
	script := fmt.Sprintf(`set -e
cd repo
git add -A
if ! git diff --cached --quiet; then git -c user.name=%[1]s -c user.email=%[2]s commit -q -m "devbot: uncommitted changes at the end of the run"; fi
rm -f ../out.bundle
if [ "$(git rev-list --count %[3]s)" -gt 0 ]; then
  git bundle create -q ../out.bundle %[3]s
fi
chmod a+r ../out.bundle 2>/dev/null || true`, shq(name), shq(email), shq("refs/remotes/origin/"+w.base+"..HEAD"))
	res, err := ag.Exec(ctx, agent.ExecRequest{Dir: w.shared, Script: script})
	if err != nil {
		return 0, err
	}
	if res.ExitCode != 0 {
		return 0, fmt.Errorf("collecting the agent's commits failed: %s", res.Output)
	}
	bundle := filepath.Join(w.shared, "out.bundle")
	// Copy the bundle into private storage before git reads it: opened
	// without following symlinks, so it can't be swapped for a link to one of
	// the harness's own files between a check and the read.
	src, err := os.OpenFile(bundle, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("out.bundle: %w", err)
	}
	defer src.Close()
	if st, err := src.Stat(); err != nil || !st.Mode().IsRegular() {
		return 0, errors.New("out.bundle is not a regular file")
	}
	local := filepath.Join(filepath.Dir(w.priv), "work.bundle")
	dst, err := os.Create(local)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(dst, io.LimitReader(src, 500<<20+1))
	dst.Close()
	if err != nil {
		return 0, err
	}
	if n > 500<<20 {
		return 0, errors.New("the agent's commits are over the 500 MB limit")
	}
	if _, err := w.git(ctx, false, "fetch", "--quiet", local, "+HEAD:refs/devbot/work"); err != nil {
		return 0, err
	}
	ahead, err := w.git(ctx, false, "rev-list", "--count", "refs/remotes/origin/"+w.base+"..refs/devbot/work")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(ahead)
}

// push force-pushes the collected work to the branch. The branch is devbot's,
// and a fix round may have rebased it. Work that touches what rc protects is
// never pushed: CI runs on push, with the repo's secrets, before anyone has
// looked at it. Work that changes protected files a person may approve is
// held for them (heldError) unless an approval already covers it. Work that
// mentions core.DepsRoot, of a task that has built against another's
// unmerged work, is never pushed either (depsError): it wires in a local copy
// of a dependency.
//
// A carried PR's branch is someone else's, so the work goes on top of it: a
// plain push, unless the work no longer contains the head the run started
// from (a rebase onto the base, to resolve conflicts). Then it is forced
// with a lease on that head, so nothing pushed meanwhile is lost, and
// rewrote says so for the comment on the PR.
func (w *workspace) push(ctx context.Context, rc *config.Repo) error {
	if !w.held {
		if err := w.attribute(ctx, rc.Commits); err != nil {
			return fmt.Errorf("applying commits.co_authored_by and commits.trailers: %w", err)
		}
	}
	cs, err := changeset.Compute(ctx, w.priv, "refs/remotes/origin/"+w.base, "refs/devbot/work")
	if err != nil {
		return fmt.Errorf("listing what the work changes: %w", err)
	}
	if w.depsGuard {
		paths := make([]string, len(cs))
		for i, c := range cs {
			paths[i] = c.Path
		}
		if hit, err := w.depsWiring(ctx, paths); err != nil {
			return fmt.Errorf("checking the work for a dependency's local wiring: %w", err)
		} else if len(hit) > 0 {
			return &depsError{Files: hit}
		}
	}
	held, hit := rc.HeldChanges(cs)
	if len(hit) > 0 {
		return &protectedError{Branch: w.branch, Paths: hit}
	}
	if len(held) > 0 && (w.approved == nil || !w.approved(core.HeldFiles(held))) {
		return &heldError{Branch: w.branch, Changes: held}
	}
	mode, force := "--force", true
	if w.carried {
		started := w.head
		if _, err := w.git(ctx, false, "merge-base", "--is-ancestor", started, "refs/devbot/work"); err == nil {
			mode, force = "--no-force", false
		} else {
			old, err := w.git(ctx, false, "rev-parse", "--verify", started)
			if err != nil {
				return err
			}
			mode, w.rewrote = "--force-with-lease=refs/heads/"+w.branch+":"+old, true
		}
	}
	out, err := w.git(ctx, true, "push", "--quiet", mode, w.pushTo, "refs/devbot/work:refs/heads/"+w.branch)
	sha, _ := w.git(ctx, false, "rev-parse", "refs/devbot/work")
	w.audit.Log(ctx, audit.Record{Event: "git.push", Outcome: pushOutcome(out, err), Actor: w.actor, ActorKind: string(w.role), Err: err,
		Detail: map[string]any{"branch": w.branch, "sha": sha, "force": force, "fork": w.pushTo != "origin"}})
	if err != nil && w.pushTo != "origin" && core.ForkRefused(out) {
		return &forkRefusedError{Branch: w.branch, Reason: out}
	}
	if err != nil {
		if e := w.refusal(ctx, w.branch, out); e != nil {
			return e
		}
	}
	return err
}

// refusal is the forge's refusal of a push to ref, read from git push's
// output, or nil when the push failed some other way.
func (w *workspace) refusal(ctx context.Context, ref, out string) error {
	if !strings.Contains(out, "! [remote rejected]") {
		return nil
	}
	e := &pushRefusedError{Branch: ref, Reason: strings.Join(rejectedRE.FindAllString(out, -1), "\n")}
	if workflowsRefusedRE.MatchString(out) {
		e.Workflows, e.WorkflowsErr = w.workflowsGrant(ctx)
	}
	return e
}

// pushPartial pushes work a run stopped part way through (a question, a
// wait, a usage limit), for the next run to carry on from. Not onto a
// carried PR: its CI and its people see every push to it, so half-done work
// stays off it, and the next run starts from the PR again.
func (w *workspace) pushPartial(ctx context.Context, rc *config.Repo) error {
	if w.carried {
		return nil
	}
	return w.push(ctx, rc)
}

// pushOutcome is how a push reads in its audit record.
func pushOutcome(out string, err error) string {
	switch {
	case err == nil:
		return audit.OK
	case strings.Contains(out, "! [remote rejected]"):
		return audit.Refused
	}
	return audit.Error
}

// rejectedRE picks out the remote's own refusal from git push's output: its
// "! [remote rejected] ... (reason)" line and any "remote:" messages.
var rejectedRE = regexp.MustCompile(`(?m)^(?:remote: .*\S.*|\s*! \[remote rejected\] .*)$`)

// workflowsRefusedRE matches GitHub refusing a push that changes workflow
// files from a token without the workflows permission.
var workflowsRefusedRE = regexp.MustCompile("without .?workflows.? permission")

// workflowsGrant asks the forge whether the credential the push went with
// may change workflow files, for the comment on a refused push.
func (w *workspace) workflowsGrant(ctx context.Context) (workflowsGrant, error) {
	ok, err := w.provider.CanPushWorkflows(ctx, w.remote, w.role)
	switch {
	case err != nil:
		return workflowsUnknown, err
	case ok:
		return workflowsGranted, nil
	}
	return workflowsMissing, nil
}

// workflowsGrant is what devbot knows of a push's permission to change
// workflow files, when the forge refused it for changing them.
type workflowsGrant int

const (
	workflowsNotAsked workflowsGrant = iota // the refusal wasn't about workflows
	workflowsMissing                        // the credential lacked it
	workflowsGranted                        // it had it, and the forge refused anyway
	workflowsUnknown                        // the forge wouldn't say
)

// pushRefusedError is the forge refusing the push itself: a rule the
// implementer's identity can't get past, such as changing CI workflow files
// without the permission to, which pushing again won't change.
type pushRefusedError struct {
	Branch, Reason string
	// Workflows is, for a refusal for changing workflow files, whether the
	// credential could; WorkflowsErr why the forge wouldn't say.
	Workflows    workflowsGrant
	WorkflowsErr error
}

func (e *pushRefusedError) Error() string {
	return "the forge refused the push to " + e.Branch + ":\n" + e.Reason
}

// forkRefusedError is a fork turning devbot's credential away, on a push or
// the dry run of one: the PR allows edits by maintainers, but devbot can't
// push to the fork, so the work goes onto a branch of devbot's own instead
// (core.SupersedeCarry).
type forkRefusedError struct {
	Branch, Reason string
}

func (e *forkRefusedError) Error() string {
	return "the fork refused devbot's push to " + e.Branch + ":\n" + e.Reason
}

// protectedError is work that touches protected paths, which devbot doesn't
// push. Paths are ready to list in a comment.
type protectedError struct {
	Branch string
	Paths  []string
}

func (e *protectedError) Error() string {
	return "the work touches protected paths, so it wasn't pushed to " + e.Branch + ":\n" + strings.Join(e.Paths, "\n")
}

func (w *workspace) cleanup() {
	os.RemoveAll(filepath.Dir(w.priv))
	os.RemoveAll(w.shared)
}

// readShared reads a file the agent left, capped at max bytes. It is data.
func (w *workspace) readShared(rel string, max int64) ([]byte, error) {
	// os.Root refuses symlinks that leave the repo, so a planted
	// .devbot/result.json -> /proc/self/environ can't make the harness read
	// its own secrets back out.
	root, err := os.OpenRoot(w.repo)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}

// resetShared replaces a directory of the model's repo with an empty one,
// through os.Root, so a committed symlink can't point the removal elsewhere.
func (w *workspace) resetShared(rel string) error {
	root, err := os.OpenRoot(w.repo)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.RemoveAll(rel); err != nil {
		return err
	}
	if err := root.Mkdir(rel, 0o777); err != nil {
		return err
	}
	// Writable from the agent side, whatever uid it runs as.
	return root.Chmod(rel, 0o777)
}

// writeShared writes into the model's repo through os.Root, so a symlink
// committed to the repo can't redirect the write into the harness's files.
func (w *workspace) writeShared(rel string, data []byte) error {
	root, err := os.OpenRoot(w.repo)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.Dir(rel), 0o777); err != nil {
		return err
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// shq single-quotes s for bash.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var shaRE = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// sameDiff reports whether the collected work makes the same changes as
// commit before did, each against its merge base with the base branch: a
// rebase that needed no conflict resolution, and nothing else. Context lines,
// line numbers and blob ids are left out, because those move whenever the base
// does. Anything it can't tell (before is gone, say) counts as changed.
func (w *workspace) sameDiff(ctx context.Context, before string) bool {
	if !shaRE.MatchString(before) {
		return false
	}
	diff := func(head string) (string, error) {
		out, err := w.git(ctx, false, "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "-U0",
			"refs/remotes/origin/"+w.base+"..."+head)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, l := range strings.Split(out, "\n") {
			if !strings.HasPrefix(l, "index ") && !strings.HasPrefix(l, "@@") {
				b.WriteString(l + "\n")
			}
		}
		return b.String(), nil
	}
	old, err := diff(before)
	if err != nil {
		return false
	}
	cur, err := diff("refs/devbot/work")
	return err == nil && old != "" && old == cur
}

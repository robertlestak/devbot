package core

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// A fork's PR whose author lets maintainers push to it is carried on its own
// branch only while devbot can push there. The forge's word on maintainer
// edits isn't enough: a private fork of a private repo, or a fork whose owner
// keeps the implementer's App out, refuses devbot's credential all the same.
// So adoption reads the fork's branch with that credential first, each run
// probes a push before the agent starts, and a push the fork refuses at the
// end of a run moves the work too: each time to devbot/pr-<N>, as for a fork
// that doesn't allow edits (SupersedeCarry). The PR's code is always read
// from the repo's refs/pull/<N>/head, never the fork, so none of that stops
// the work from starting.

// ForkURL is fork's git URL, built from origin, repo's URL as the forge's
// config gives it, the way the forge builds every repo's: the same server
// and path prefix, with fork's owner and name in place of repo's. git sends
// origin's credential there, so it can go nowhere but the configured forge,
// and a forge that advertises other clone URLs than the address devbot
// reaches it at (Gitea's ROOT_URL, say) changes nothing.
func ForkURL(origin string, repo, fork forge.RepoRef) (string, error) {
	tail := "/" + repo.Owner + "/" + repo.Name + ".git"
	prefix, ok := strings.CutSuffix(origin, tail)
	if !ok {
		return "", fmt.Errorf("%s doesn't end in %s, so devbot can't tell where %s's fork is", origin, tail, repo.FullName())
	}
	if fork.Provider != repo.Provider || !pathName(fork.Owner) || !pathName(fork.Name) {
		return "", fmt.Errorf("the PR's head is in %q, not a repo on %s's forge", fork.String(), repo.FullName())
	}
	return prefix + "/" + fork.Owner + "/" + fork.Name + ".git", nil
}

// pathName reports whether s is an owner or repo name that is one path
// segment as it stands: nothing a URL would read as more, or less.
func pathName(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// forkRefusedRE matches git's words for a remote that won't have devbot's
// credential: GitHub and Gitea answer a repo the token can't see with "not
// found", and one it can see but not push to with 403 or "denied".
var forkRefusedRE = regexp.MustCompile(`(?i)not found|does not appear to be a git repository|denied|forbidden|returned error: 40[134]|authentication failed|could not read username|! \[remote rejected\]`)

// ForkRefused reports whether git's output says the fork turned devbot's
// credential away, as opposed to failing for a reason another try could
// get past (the network, a lease that went stale).
func ForkRefused(out string) bool { return forkRefusedRE.MatchString(out) }

// forkReachable reads pr's head branch from its fork with the implementer's
// git credential, as a push there would go.
func (e *Engine) forkReachable(ctx context.Context, p forge.Provider, pr forge.PR) (bool, error) {
	if e.ForkReachable != nil {
		return e.ForkReachable(ctx, p, pr)
	}
	url, header, err := p.GitAuth(ctx, pr.Repo, forge.RoleImplementer)
	if err != nil {
		return false, err
	}
	e.Audit.Secret(header, header[strings.LastIndex(header, " ")+1:])
	fork, err := ForkURL(url, pr.Repo, pr.HeadRepo)
	if err != nil {
		return false, err
	}
	cmd := exec.CommandContext(ctx, "git", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "protocol.file.allow=always", "-c", "http.extraHeader="+header,
		"ls-remote", "--heads", fork, "refs/heads/"+pr.HeadBranch)
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		s := strings.ReplaceAll(strings.TrimSpace(out.String()), header, "[redacted]")
		if ForkRefused(s) {
			return false, nil
		}
		return false, fmt.Errorf("git ls-remote %s: %v: %s", fork, err, s)
	}
	// A branch gone from a fork devbot can read is the fork's own business:
	// the PR's head is still the repo's refs/pull/<N>/head.
	return true, nil
}

// ForkUnreachableText is why a fork's PR that allows maintainer edits is
// carried on a branch of devbot's own all the same, for the comments that
// say so.
func ForkUnreachableText(pr forge.PR) string {
	return fmt.Sprintf("This PR allows edits by maintainers, but devbot can't push to the fork, `%s`: the forge refuses the implementer's credential there (a private fork, or one whose owner keeps devbot's App out).",
		pr.HeadRepo.FullName())
}

// SupersedeCarry moves a task carrying pr, a fork's PR, on the PR's own
// branch onto devbot/pr-<N> instead, because devbot can't push to the fork
// after all (why says how that showed). The PR is told why and where the
// work goes, then the task's routing records it, superseding pr rather than
// carrying it: every reader after that one, the harness's next run and
// serve's evaluations, takes the superseding path. Done already, it does
// nothing.
func (e *Engine) SupersedeCarry(ctx context.Context, p forge.Provider, it forge.Item, pr forge.PR, why string) error {
	cs, err := p.ListComments(ctx, it.Ref)
	if err != nil {
		return err
	}
	m, _ := metaOf(cs, e.bots(p)...)
	if m.Supersedes == pr.Number {
		return nil
	}
	branch := SupersedeBranch(pr.Number)
	link := fmt.Sprintf("%s So devbot carries it on `%s` in this repo instead, starting from this PR's commits, which keep their authorship, and opens a new PR from there that links back here. This PR stays open until that one merges; then devbot closes it as superseded. The task's state, its updates, and the way to cancel it are still on #%d (%s).",
		why, branch, it.Ref.Number, it.URL)
	if err := p.Comment(ctx, forge.ItemRef{Repo: pr.Repo, Number: pr.Number}, link+"\n\n"+RenderCarry(CarryLink{Issue: it.Ref.Number})); err != nil {
		return err
	}
	m.Carry, m.Supersedes = 0, pr.Number
	if m.Base == "" {
		m.Base = pr.BaseBranch
	}
	note := fmt.Sprintf("devbot can't push to PR #%d's fork, so it carries the PR on `%s` in this repo from here on and opens a PR of its own from there, which supersedes #%d.",
		pr.Number, branch, pr.Number)
	if err := p.Comment(ctx, it.Ref, note+"\n\n"+RenderMeta(m)); err != nil {
		return err
	}
	e.metas.put(it.Ref.String(), m)
	return nil
}

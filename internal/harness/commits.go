package harness

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"git.shdw.tech/rob/devbot/internal/agent/claude"
	"git.shdw.tech/rob/devbot/internal/agent/codex"
	"git.shdw.tech/rob/devbot/internal/config"
)

var (
	// aiCoAuthorRE is a co-author trailer an agent CLI adds, which names its
	// vendor's no-reply address. A person's never does.
	aiCoAuthorRE = regexp.MustCompile(`(?i)^\s*co-authored-by:.*<noreply@(anthropic|openai)\.com>\s*$`)
	// coAuthorRE is any co-author trailer, with the address it names.
	coAuthorRE = regexp.MustCompile(`(?i)^\s*co-authored-by:.*<([^>]*)>\s*$`)
	// generatedRE is the line an agent CLI adds to a commit message or a PR
	// body: "🤖 Generated with [Claude Code](https://claude.com/claude-code)".
	generatedRE = regexp.MustCompile(`(?i)^\W*generated (with|by) \[?(claude|codex|openai|chatgpt)\b`)
)

// coAuthors is each agent CLI's own co-author trailer, in its own words, and
// what finds one it already added.
var coAuthors = map[string]struct {
	trailer string
	has     *regexp.Regexp
}{
	claude.Name: {"Co-Authored-By: Claude <noreply@anthropic.com>", regexp.MustCompile(`(?im)^co-authored-by:.*<noreply@anthropic\.com>\s*$`)},
	codex.Name:  {"Co-authored-by: Codex <noreply@openai.com>", regexp.MustCompile(`(?im)^co-authored-by:.*<noreply@openai\.com>\s*$`)},
}

// dropAIAttribution takes the agent CLIs' co-author trailers and "Generated
// with" lines out of s, a commit message or PR text, leaving every other
// line, a person's Co-authored-by among them. s comes back as it was when it
// has none.
func dropAIAttribution(s string) string {
	return dropLines(s, func(l string) bool { return aiCoAuthorRE.MatchString(l) || generatedRE.MatchString(l) })
}

// dropSelfCoAuthor takes the co-author trailers naming email, the commit's
// own author, out of msg: an agent whose CLI's attribution is off may credit
// the git identity it commits as instead, which says nothing.
func dropSelfCoAuthor(msg, email string) string {
	return dropLines(msg, func(l string) bool {
		m := coAuthorRE.FindStringSubmatch(l)
		return m != nil && strings.EqualFold(strings.TrimSpace(m[1]), email)
	})
}

// dropLines takes the lines drop matches out of s. s comes back as it was
// when there are none.
func dropLines(s string, drop func(string) bool) string {
	var kept []string
	dropped := false
	for _, l := range strings.Split(s, "\n") {
		if drop(l) {
			dropped = true
			continue
		}
		// The blank line that set a dropped line off goes with it.
		if strings.TrimSpace(l) == "" && len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
			continue
		}
		kept = append(kept, l)
	}
	if !dropped {
		return s
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// commitMessage is msg, a commit by author, as c has it: never crediting
// author as its own co-author, without the agent's attribution, or with
// harness's co-author trailer, and with c's trailers, each once.
func (w *workspace) commitMessage(ctx context.Context, msg, author string, c config.Commits) (string, error) {
	msg = dropSelfCoAuthor(msg, author)
	trailers := c.Trailers
	if c.CoAuthoredBy != nil && !*c.CoAuthoredBy {
		msg = dropAIAttribution(msg)
	}
	if co, ok := coAuthors[w.harness]; ok && c.CoAuthoredBy != nil && *c.CoAuthoredBy && !co.has.MatchString(msg) {
		trailers = append([]string{co.trailer}, trailers...)
	}
	if len(trailers) == 0 {
		return msg, nil
	}
	// git's own rules for where the trailer block is and what counts as
	// one already there, so the trailers stay put however often a commit
	// passes through here.
	args := []string{"interpret-trailers", "--if-exists", "addIfDifferent"}
	for _, t := range trailers {
		args = append(args, "--trailer", t)
	}
	return runGitIO(ctx, w.priv, "", false, nil, msg+"\n", args...)
}

// attribute rewrites the collected work's commits as c asks, before they are
// pushed: the agent CLI's own setting (agent.RunRequest.CoAuthoredBy) is a
// request it may not honour. Only the commits devbot made, those whose
// committer is its own identity, are changed; a person's commits, on a PR
// devbot carries or a fork's it supersedes, keep their messages, and on a
// carried PR's branch nothing already there is touched. A commit whose
// message comes out the same is kept as it was, and a rewritten one keeps
// its author, committer and dates, so a commit that has been through here
// before comes out the very same commit: a fix round or a rebase leaves
// earlier work alone, and the trailers are on every commit once. It runs
// with c unset too: a co-author trailer naming the commit's own author goes
// whatever c says.
func (w *workspace) attribute(ctx context.Context, c config.Commits) error {
	args := []string{"rev-list", "--reverse", "--topo-order", "--parents", "refs/devbot/work", "^refs/remotes/origin/" + w.base}
	if w.carried {
		args = append(args, "^"+w.head)
	}
	out, err := w.git(ctx, false, args...)
	if err != nil || out == "" {
		return err
	}
	now := map[string]string{} // commit -> what it is after the rewrite
	last := ""
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		sha, parents, moved := f[0], f[1:], false
		for i, p := range parents {
			if n, ok := now[p]; ok && n != p {
				parents[i], moved = n, true
			}
		}
		last = sha
		info, err := w.git(ctx, false, "show", "-s", "--date=raw", "--format=%T%x00%an%x00%ae%x00%ad%x00%cn%x00%ce%x00%cd%x00%B", sha)
		if err != nil {
			return err
		}
		v := strings.SplitN(info, "\x00", 8)
		if len(v) != 8 {
			return fmt.Errorf("reading commit %s: %q", sha, info)
		}
		msg := v[7]
		if strings.EqualFold(v[5], w.email) {
			if msg, err = w.commitMessage(ctx, msg, v[2], c); err != nil {
				return err
			}
		}
		if !moved && strings.TrimSpace(msg) == strings.TrimSpace(v[7]) {
			now[sha] = sha
			continue
		}
		ct := []string{"commit-tree", v[0]}
		for _, p := range parents {
			ct = append(ct, "-p", p)
		}
		env := []string{"GIT_AUTHOR_NAME=" + v[1], "GIT_AUTHOR_EMAIL=" + v[2], "GIT_AUTHOR_DATE=" + v[3],
			"GIT_COMMITTER_NAME=" + v[4], "GIT_COMMITTER_EMAIL=" + v[5], "GIT_COMMITTER_DATE=" + v[6]}
		if now[sha], err = runGitIO(ctx, w.priv, "", false, env, strings.TrimSpace(msg)+"\n", ct...); err != nil {
			return err
		}
	}
	if now[last] == last {
		return nil
	}
	_, err = w.git(ctx, false, "update-ref", "refs/devbot/work", now[last])
	return err
}

package harness

import (
	"context"
	"fmt"
	"strings"

	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// carriedContext is what the implementer of a PR devbot carries needs beyond
// its tracking issue: which PR it is, and what was said on it, which the
// issue's own discussion doesn't have (prFeedback).
func (r *Runner) carriedContext(ctx context.Context, p forge.Provider, pr *forge.PR, reason string) (string, error) {
	fb, err := r.fixFeedback(ctx, p, pr, "### Reviews and comments on the PR")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(fmt.Sprintf("## The PR you are carrying\n\nPR #%d (%s), opened by @%s, from `%s` onto `%s`.\n\n%s\n\n%s",
		pr.Number, pr.URL, pr.Author, pr.HeadBranch, pr.BaseBranch, reason, fb)), nil
}

// carriedPushText is devbot's comment on a carried PR after it pushed to
// it: what it pushed, for which task, and the implementer's summary, which
// is where it answers the review comments it didn't act on.
func carriedPushText(it forge.Item, pr *forge.PR, head string, rewrote bool, summary string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "devbot pushed `%s` to this PR for #%d.", head[:min(12, len(head))], it.Ref.Number)
	if rewrote {
		fmt.Fprintf(&b, " The branch conflicted with `%s`, so it was rebased onto it: its history was rewritten and force-pushed. Anyone with a local copy of `%s` should reset it to the new head.",
			pr.BaseBranch, pr.HeadBranch)
	}
	b.WriteString("\n\n" + core.Defang(summary))
	return b.String()
}

// supersedeReason is what the run that opens a PR to supersede a fork's PR
// is told about where its work goes, as a carried PR's first run is told by
// core's carryReason.
func supersedeReason(orig *forge.PR, branch string) string {
	return fmt.Sprintf("devbot is carrying this PR, opened by @%s from a fork that doesn't let maintainers push to it, so the work goes onto `%s` in this repo: it starts at the PR's head, with its author's commits, and your commits go on top. devbot opens a PR from it that supersedes #%d. "+
		"Bring it to its acceptance criteria in the issue: fix what fails its checks, rebase onto the base if it conflicts, and address or answer its open review comments.", orig.Author, branch, orig.Number)
}

// supersedesText credits, in the description of devbot's PR, the fork's PR
// it carries on.
func supersedesText(orig *forge.PR) string {
	return fmt.Sprintf("Supersedes #%d (%s) by @%s, which devbot carried on from its fork: this PR keeps its commits, and their authorship, with devbot's on top.", orig.Number, orig.URL, orig.Author)
}

// supersedingText is devbot's comment on a fork's PR when it opened pr from
// branch to carry it on.
func supersedingText(pr *forge.PR, branch string) string {
	return fmt.Sprintf("devbot opened #%d (%s) from `%s` to carry this PR on, with your commits and their authorship. This PR stays open until #%d merges; then devbot closes it as superseded.",
		pr.Number, pr.URL, branch, pr.Number)
}

// forkPushRefusedText is why a fork's PR that allows maintainer edits moved
// onto a branch of devbot's own when the fork refused the push itself.
func forkPushRefusedText(pr *forge.PR) string {
	return fmt.Sprintf("This PR allows edits by maintainers, but the fork, `%s`, refused devbot's push to `%s`.", pr.HeadRepo.FullName(), pr.HeadBranch)
}

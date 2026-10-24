package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// rule is one active ruleset rule on a branch (docs: "REST API endpoints for
// rules", "Get rules for a branch"). Only the parameters devbot describes
// are read.
type rule struct {
	Type       string `json:"type"`
	Parameters struct {
		// pull_request
		RequiredApprovingReviewCount   int  `json:"required_approving_review_count"`
		RequireCodeOwnerReview         bool `json:"require_code_owner_review"`
		RequireLastPushApproval        bool `json:"require_last_push_approval"`
		RequiredReviewThreadResolution bool `json:"required_review_thread_resolution"`
		// required_status_checks
		RequiredStatusChecks             []checkContext `json:"required_status_checks"`
		StrictRequiredStatusChecksPolicy bool           `json:"strict_required_status_checks_policy"`
		// required_deployments
		RequiredDeploymentEnvironments []string `json:"required_deployment_environments"`
	} `json:"parameters"`
	RulesetSource string `json:"ruleset_source"`
}

// classic is the classic branch protection on a branch (docs: "Get branch
// protection"). Reading it takes the Administration read permission.
type classic struct {
	RequiredPullRequestReviews *struct {
		RequiredApprovingReviewCount int  `json:"required_approving_review_count"`
		RequireCodeOwnerReviews      bool `json:"require_code_owner_reviews"`
		RequireLastPushApproval      bool `json:"require_last_push_approval"`
	} `json:"required_pull_request_reviews"`
	Restrictions *struct {
		Apps []struct {
			Slug string `json:"slug"`
		} `json:"apps"`
	} `json:"restrictions"`
	RequiredLinearHistory          enabled `json:"required_linear_history"`
	RequiredSignatures             enabled `json:"required_signatures"`
	RequiredConversationResolution enabled `json:"required_conversation_resolution"`
	LockBranch                     enabled `json:"lock_branch"`
}

type enabled struct {
	Enabled bool `json:"enabled"`
}

// Protection combines GitHub's two systems: rulesets, which anyone who can
// read the repo can list for a branch, and classic branch protection. A
// branch says whether it is protected and which checks that requires; the
// rest of classic protection is only readable with Administration read, so
// without it the rule is named but not spelled out.
func (c *Client) Protection(ctx context.Context, r forge.RepoRef, branch string) (forge.Protection, error) {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return forge.Protection{}, err
	}
	var out forge.Protection
	addChecks := func(names ...string) {
		for _, n := range names {
			if n != "" && !slices.Contains(out.RequiredChecks, n) {
				out.RequiredChecks = append(out.RequiredChecks, n)
			}
		}
	}

	rules, err := list[rule](ctx, c, cr, repoPath(r)+"/rules/branches/"+escapePath(branch))
	if planUnavailable(err) {
		return forge.Protection{}, nil
	}
	if err != nil && !errors.Is(err, forge.ErrNotFound) {
		return forge.Protection{}, err
	}
	for _, ru := range rules {
		p := ru.Parameters
		var d string
		switch ru.Type {
		case "pull_request":
			d = reviewRule(p.RequiredApprovingReviewCount, p.RequireCodeOwnerReview, p.RequireLastPushApproval)
			if p.RequiredReviewThreadResolution {
				d += ", with every review conversation resolved"
			}
		case "required_status_checks":
			names := contexts(p.RequiredStatusChecks)
			addChecks(names...)
			d = "checks " + codeList(names) + " must pass"
			if p.StrictRequiredStatusChecksPolicy {
				d += ", on a branch that is up to date with the base"
				out.UpToDate = true
			}
		case "required_deployments":
			d = "deployments to " + codeList(p.RequiredDeploymentEnvironments) + " must succeed"
		case "required_signatures":
			d = "commits must have verified signatures"
		case "required_linear_history":
			d = "linear history: only squash or rebase merges"
		case "merge_queue":
			d = "merges must go through the merge queue"
		case "update":
			d = "only bypass actors may update the branch"
		case "creation", "deletion", "non_fast_forward":
			continue // about pushes to the branch itself, not merging into it
		default:
			d = "rule `" + ru.Type + "`"
		}
		if ru.RulesetSource != "" {
			d += " (ruleset on " + ru.RulesetSource + ")"
		}
		out.Rules = append(out.Rules, d)
	}

	// Get a branch: its required checks come with it, so they are known
	// even when the rest of classic protection can't be read.
	var b struct {
		Protected  bool `json:"protected"`
		Protection struct {
			RequiredStatusChecks struct {
				EnforcementLevel string         `json:"enforcement_level"`
				Contexts         []string       `json:"contexts"`
				Checks           []checkContext `json:"checks"`
				Strict           bool           `json:"strict"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	if _, err := c.do(ctx, cr, "GET", repoPath(r)+"/branches/"+escapePath(branch), nil, &b); err != nil {
		if errors.Is(err, forge.ErrNotFound) {
			return c.finish(out), nil
		}
		return forge.Protection{}, err
	}
	// A ruleset alone may mark a branch protected, so a 404 from classic
	// protection's own endpoint means there is none.
	if b.Protected {
		var names []string
		if rsc := b.Protection.RequiredStatusChecks; rsc.EnforcementLevel != "off" {
			for _, n := range append(rsc.Contexts, contexts(rsc.Checks)...) {
				if !slices.Contains(names, n) {
					names = append(names, n)
				}
			}
		}
		if len(names) > 0 {
			addChecks(names...)
			d := "checks " + codeList(names) + " must pass"
			if b.Protection.RequiredStatusChecks.Strict {
				d += ", on a branch that is up to date with the base"
				out.UpToDate = true
			}
			out.Rules = append(out.Rules, d+" (branch protection)")
		}
		var cl classic
		_, err := c.do(ctx, cr, "GET", repoPath(r)+"/branches/"+escapePath(branch)+"/protection", nil, &cl)
		switch {
		case planUnavailable(err):
			// Not on this plan: only rulesets apply.
		case errors.Is(err, forge.ErrForbidden):
			// Only named when it's surely there: rulesets alone also make
			// the branch protected.
			if len(rules) == 0 || len(names) > 0 {
				out.Rules = append(out.Rules, "branch protection, whose other settings devbot can't read (that takes the implementer App's Administration read permission)")
			}
		case errors.Is(err, forge.ErrNotFound):
			// Not protected classically: only rulesets apply.
		case err != nil:
			return forge.Protection{}, err
		default:
			out.Rules = append(out.Rules, classicRules(cl)...)
		}
	}
	return c.finish(out), nil
}

// planUnavailable is GitHub's 403 for a feature the repo's plan doesn't
// have, as rulesets and branch protection on a free plan's private repo: no
// such rule can exist there, so it means no protection, not an error. Any
// other 403 is still one.
func planUnavailable(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
		return false
	}
	m := ae.message()
	return strings.Contains(m, "Upgrade to GitHub Pro") || strings.Contains(m, "make this repository public")
}

type checkContext struct {
	Context string `json:"context"`
}

func contexts(cs []checkContext) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Context)
	}
	return out
}

func (c *Client) finish(p forge.Protection) forge.Protection {
	if len(p.Rules) > 0 {
		p.Exempt = fmt.Sprintf("Let the reviewer App, @%s, which merges, past the rules: in a ruleset (Settings, Rules, Rulesets) add it to the bypass list; "+
			"in classic branch protection add it under \"Allow specified actors to bypass required pull requests\", and under \"Restrict who can push to matching branches\" if that is on.", c.rev.login)
	}
	return p
}

func classicRules(cl classic) []string {
	var out []string
	if rv := cl.RequiredPullRequestReviews; rv != nil {
		out = append(out, reviewRule(rv.RequiredApprovingReviewCount, rv.RequireCodeOwnerReviews, rv.RequireLastPushApproval)+" (branch protection)")
	}
	if rs := cl.Restrictions; rs != nil {
		var apps []string
		for _, a := range rs.Apps {
			apps = append(apps, a.Slug)
		}
		d := "only the people, teams and Apps it lists may push or merge (branch protection)"
		if len(apps) > 0 {
			d = "only the people, teams and Apps it lists may push or merge, Apps " + codeList(apps) + " (branch protection)"
		}
		out = append(out, d)
	}
	for _, e := range []struct {
		on   bool
		what string
	}{
		{cl.RequiredLinearHistory.Enabled, "linear history: only squash or rebase merges"},
		{cl.RequiredSignatures.Enabled, "commits must have verified signatures"},
		{cl.RequiredConversationResolution.Enabled, "every review conversation must be resolved"},
		{cl.LockBranch.Enabled, "the branch is locked"},
	} {
		if e.on {
			out = append(out, e.what+" (branch protection)")
		}
	}
	return out
}

func reviewRule(n int, codeOwners, lastPush bool) string {
	d := "a pull request"
	switch {
	case n == 1:
		d += " with an approving review"
	case n > 1:
		d += fmt.Sprintf(" with %d approving reviews", n)
	}
	if codeOwners {
		d += ", including the code owners'"
	}
	if lastPush {
		d += ", the last push approved by someone else"
	}
	return d
}

func codeList(names []string) string {
	if len(names) == 0 {
		return "(none named)"
	}
	return "`" + strings.Join(names, "`, `") + "`"
}

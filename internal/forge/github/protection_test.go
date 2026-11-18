package github

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// Shapes from GitHub's docs: "Get rules for a branch" and "Get branch
// protection".
var (
	rulesetRules = []map[string]any{
		{"type": "deletion", "ruleset_source_type": "Repository", "ruleset_source": "acme/app", "ruleset_id": 7},
		{"type": "pull_request", "ruleset_source_type": "Repository", "ruleset_source": "acme/app", "ruleset_id": 7,
			"parameters": map[string]any{"required_approving_review_count": 1, "dismiss_stale_reviews_on_push": true,
				"require_code_owner_review": false, "require_last_push_approval": true, "required_review_thread_resolution": false}},
		{"type": "required_status_checks", "ruleset_source_type": "Organization", "ruleset_source": "acme", "ruleset_id": 9,
			"parameters": map[string]any{"strict_required_status_checks_policy": true,
				"required_status_checks": []map[string]any{{"context": "ci", "integration_id": 15368}, {"context": "e2e"}}}},
		{"type": "merge_queue", "ruleset_source_type": "Repository", "ruleset_source": "acme/app", "ruleset_id": 7, "parameters": map[string]any{}},
	}
	classicProtection = map[string]any{
		"url":                           "https://api.github.com/repos/acme/app/branches/main/protection",
		"required_status_checks":        map[string]any{"strict": false, "enforcement_level": "non_admins", "contexts": []string{"ci", "lint"}, "checks": []map[string]any{{"context": "ci", "app_id": nil}, {"context": "lint", "app_id": 15368}}},
		"required_pull_request_reviews": map[string]any{"required_approving_review_count": 2, "require_code_owner_reviews": true, "dismiss_stale_reviews": false},
		"restrictions":                  map[string]any{"users": []any{}, "teams": []any{}, "apps": []map[string]any{{"slug": "deploy-bot"}}},
		"required_linear_history":       map[string]any{"enabled": true},
		"enforce_admins":                map[string]any{"enabled": false},
		"lock_branch":                   map[string]any{"enabled": false},
	}
)

func (f *fakeGitHub) protect(rules []map[string]any, classic map[string]any, adminRead bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos["acme/app"]
	if rules != nil {
		r.rules["main"] = rules
	}
	if classic != nil {
		r.classic["main"] = classic
	}
	if adminRead {
		f.installs[11].perms["administration"] = "read"
	}
}

func hasRule(p forge.Protection, sub string) bool {
	return slices.ContainsFunc(p.Rules, func(r string) bool { return strings.Contains(r, sub) })
}

func TestProtectionReadsRulesets(t *testing.T) {
	f := newFakeGitHub(t, "")
	f.protect(rulesetRules, nil, false)
	p, err := f.client().Protection(ctx, repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.RequiredChecks, []string{"ci", "e2e"}) {
		t.Errorf("required checks %v", p.RequiredChecks)
	}
	for _, want := range []string{
		"a pull request with an approving review, the last push approved by someone else (ruleset on acme/app)",
		"checks `ci`, `e2e` must pass, on a branch that is up to date with the base (ruleset on acme)",
		"merge queue",
	} {
		if !hasRule(p, want) {
			t.Errorf("no rule %q in %q", want, p.Rules)
		}
	}
	if !p.UpToDate {
		t.Error("a strict required-checks rule asks for an up-to-date branch")
	}
	if len(p.Rules) != 3 {
		t.Errorf("deletion is about the branch itself, not merging: %q", p.Rules)
	}
	if !strings.Contains(p.Exempt, "@devbot-review[bot]") || !strings.Contains(p.Exempt, "bypass list") {
		t.Errorf("exempt: %q", p.Exempt)
	}
}

func TestProtectionReadsClassicProtection(t *testing.T) {
	f := newFakeGitHub(t, "")
	f.protect(nil, classicProtection, true)
	p, err := f.client().Protection(ctx, repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.RequiredChecks, []string{"ci", "lint"}) {
		t.Errorf("required checks %v", p.RequiredChecks)
	}
	for _, want := range []string{
		"checks `ci`, `lint` must pass (branch protection)",
		"a pull request with 2 approving reviews, including the code owners' (branch protection)",
		"Apps `deploy-bot`",
		"linear history",
	} {
		if !hasRule(p, want) {
			t.Errorf("no rule %q in %q", want, p.Rules)
		}
	}
	if p.UpToDate {
		t.Error("checks that aren't strict don't ask for an up-to-date branch")
	}
	if !strings.Contains(p.Exempt, "Restrict who can push") {
		t.Errorf("exempt: %q", p.Exempt)
	}
}

// Strict required checks on classic protection come with the branch too.
func TestProtectionClassicStrictChecks(t *testing.T) {
	f := newFakeGitHub(t, "")
	strict := maps.Clone(classicProtection)
	strict["required_status_checks"] = map[string]any{"strict": true, "enforcement_level": "non_admins", "contexts": []string{"ci"}, "checks": []map[string]any{}}
	f.protect(nil, strict, false)
	p, err := f.client().Protection(ctx, repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !p.UpToDate || !hasRule(p, "checks `ci` must pass, on a branch that is up to date with the base (branch protection)") {
		t.Errorf("got %+v", p)
	}
}

// Without Administration read, classic protection's required checks still
// come with the branch, and the rest is named as unreadable.
func TestProtectionClassicWithoutAdministrationRead(t *testing.T) {
	f := newFakeGitHub(t, "")
	f.protect(nil, classicProtection, false)
	p, err := f.client().Protection(ctx, repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.RequiredChecks, []string{"ci", "lint"}) || !hasRule(p, "Administration read") || hasRule(p, "approving reviews") {
		t.Errorf("got %+v", p)
	}
}

// Both at once: a check either requires is listed once.
func TestProtectionCombinesRulesetsAndClassic(t *testing.T) {
	f := newFakeGitHub(t, "")
	f.protect(rulesetRules, classicProtection, true)
	p, err := f.client().Protection(ctx, repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.RequiredChecks, []string{"ci", "e2e", "lint"}) || !hasRule(p, "checks `ci`, `lint` must pass (branch protection)") {
		t.Errorf("got %q, %q", p.RequiredChecks, p.Rules)
	}
}

func TestProtectionOfAnUnprotectedBranch(t *testing.T) {
	f := newFakeGitHub(t, "")
	for _, b := range []string{"main", "no-such-branch"} {
		p, err := f.client().Protection(ctx, repo, b)
		if err != nil || len(p.RequiredChecks)+len(p.Rules) > 0 || p.Exempt != "" {
			t.Errorf("%s: got %+v, %v", b, p, err)
		}
	}
}

// planMessage is GitHub's answer to a rules or protection read on a private
// repo whose plan has neither, as the live test saw it.
const planMessage = "Upgrade to GitHub Pro or make this repository public to enable this feature."

func TestPlanUnavailable(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   bool
	}{
		{403, `{"message":"` + planMessage + `","documentation_url":"https://docs.github.com/rest/repos/rules#get-rules-for-a-branch","status":"403"}`, true},
		{403, `{"message":"Upgrade to GitHub Pro to enable this feature."}`, true},
		{403, `{"message":"Resource not accessible by integration"}`, false},
		{403, `{"message":"Must have admin rights to Repository."}`, false},
		{404, `{"message":"` + planMessage + `"}`, false},
		{422, `{"message":"` + planMessage + `"}`, false},
	} {
		if got := planUnavailable(&apiError{Status: c.status, Body: c.body}); got != c.want {
			t.Errorf("%d %s: got %v, want %v", c.status, c.body, got, c.want)
		}
	}
	if planUnavailable(nil) || planUnavailable(forge.ErrForbidden) {
		t.Error("only GitHub's own answer counts")
	}
}

// A private repo on GitHub Free can't have rulesets or branch protection,
// so it has none, and its PRs merge.
func TestProtectionOnAFreePlanPrivateRepo(t *testing.T) {
	f := newFakeGitHub(t, "")
	f.protect(nil, nil, true)
	f.mu.Lock()
	f.repos["acme/app"].freePlan = true
	f.mu.Unlock()
	p, err := f.client().Protection(ctx, repo, "main")
	if err != nil || len(p.RequiredChecks)+len(p.Rules) > 0 || p.Exempt != "" {
		t.Fatalf("got %+v, %v", p, err)
	}
	n := f.seedPR(t, nil)
	if err := f.client().Merge(ctx, repo, n, "f00d", "squash"); err != nil {
		t.Fatalf("merge: %v", err)
	}
}

// Any other 403 is still an error: devbot can't tell there are no rules.
func TestProtectionOtherForbiddenStillErrors(t *testing.T) {
	f := newFakeGitHub(t, "")
	f.mu.Lock()
	f.repos["acme/app"].rulesForbidden = true
	f.mu.Unlock()
	if _, err := f.client().Protection(ctx, repo, "main"); !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

// A merge GitHub refuses for the repo's rules says which rule, in GitHub's
// words, rather than a raw response.
func TestMergeRefusalSaysWhy(t *testing.T) {
	f := newFakeGitHub(t, "")
	n := f.seedPR(t, func(is *fakeIssue) { is.mergeState = "blocked" })
	err := f.client().Merge(ctx, repo, n, "f00d", "squash")
	if !errors.Is(err, forge.ErrNotMergeable) || !strings.Contains(err.Error(), "At least 1 approving review is required") || strings.Contains(err.Error(), "documentation_url") {
		t.Errorf("got %v", err)
	}
}

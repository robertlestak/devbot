package config

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"

	"git.shdw.tech/rob/devbot/internal/changeset"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// RepoFile is the opt-in file on a repo's default branch.
const RepoFile = ".devbot.yaml"

// Repo is one repo's .devbot.yaml.
type Repo struct {
	Version         int    `yaml:"version"`
	Enabled         *bool  `yaml:"enabled"`
	Automerge       bool   `yaml:"automerge"`
	TaskAutomerge   *bool  `yaml:"task_automerge"`
	MergeMethod     string `yaml:"merge_method"`
	RequireChecks   *bool  `yaml:"require_checks"`
	MaxReviewRounds int    `yaml:"max_review_rounds"`
	MaxRebaseRounds int    `yaml:"max_rebase_rounds"`
	Rebase          string `yaml:"rebase"`
	MaxAttempts     int    `yaml:"max_attempts"`
	// MaxReplans bounds how often a parent task filed with plan, in this
	// repo, plans again because a child asked to change its contract.
	MaxReplans     *int     `yaml:"max_replans"`
	CheckRetries   *int     `yaml:"check_retries"`
	Setup          []string `yaml:"setup"`
	Checks         []string `yaml:"checks"`
	ProtectedPaths []string `yaml:"protected_paths"`
	// ApprovablePaths are the protected paths a person may approve a change
	// to: devbot holds such work unpushed until someone who can direct it
	// approves exactly what it shows them, instead of stopping at
	// needs-human. Only a path that is also protected is affected.
	ApprovablePaths []string `yaml:"approvable_paths"`
	// Approval is who approves such a change: a person (ApprovalPerson),
	// or, with ApprovalAutoClean, devbot itself when the risk read of its
	// protected diff finds nothing, so a person is asked only when a
	// workflow could run what a model wrote with the repo's secrets.
	Approval        string   `yaml:"approval"`
	AuthorizedUsers []string `yaml:"authorized_users"`
	// RequiredReviewers must each approve a PR's current head, on top of
	// devbot's reviewer, before it merges. Their reviews count in the gate
	// and nowhere else: they direct devbot in nothing.
	RequiredReviewers []string `yaml:"required_reviewers"`
	// HumanChecks are checks (globs over their names) only a person can
	// make pass. One that isn't green holds the merge without a fix round.
	HumanChecks []string `yaml:"human_checks"`
	// TrustedReviewers are logins whose change requests on a PR count as an
	// authorized person's: they send it back and hold the merge. Nothing else.
	TrustedReviewers []string `yaml:"trusted_reviewers"`
	// GeneratedPaths are globs of files the reviewer gets listed but not
	// shown, as with linguist-generated in the base's .gitattributes: it
	// reviews the source behind them instead.
	GeneratedPaths []string `yaml:"generated_paths"`
	// ReviewBudgetKiB bounds the patch content of files that aren't
	// generated that a reviewer is given. Past it, the task goes to a person.
	ReviewBudgetKiB int `yaml:"review_budget_kib"`
	Agents          struct {
		Implement RepoAgent `yaml:"implement"`
		Review    RepoAgent `yaml:"review"`
	} `yaml:"agents"`
	Timeouts struct {
		Implement time.Duration `yaml:"implement"`
		Review    time.Duration `yaml:"review"`
	} `yaml:"timeouts"`
	// Labels attribute the repo's jobs (team, cost center) on their pods,
	// logs and usage lines, and on metrics where the install asks.
	Labels map[string]string `yaml:"labels"`
	// Docker gives the repo's implement and review runs a Docker daemon each,
	// their own, at DOCKER_HOST, for tests that start containers
	// (testcontainers, docker compose). The install decides which kind
	// (docker.mode); one without any gives none. Unset, the task's base
	// branch decides (DetectDocker), where the install has docker.detect on.
	Docker *bool `yaml:"docker"`

	// BaseBranch is the base of every task in the repo that doesn't name
	// its own; unset, the default branch is. The file itself is still read
	// from the default branch only.
	BaseBranch string `yaml:"base_branch"`

	// PullRequests is whether devbot takes on a PR someone else opened when
	// it is labelled devbot/ready (AdoptsPRs).
	PullRequests PullRequests `yaml:"pull_requests"`

	// Services are started for each implement and review run, and Env is
	// exported to the agent's commands and the checks: where the tests find
	// the services, say.
	Services []Service         `yaml:"services"`
	Env      map[string]string `yaml:"env"`

	// BranchCleanup deletes devbot's branches once their PR is closed or
	// merged. Reconcile sweeps a closed PR's branch BranchCleanupGrace after
	// the PR closed, and a branch that never got a PR OrphanBranchGrace after
	// its last commit.
	BranchCleanup      *bool         `yaml:"branch_cleanup"`
	BranchCleanupGrace time.Duration `yaml:"branch_cleanup_grace"`
	OrphanBranchGrace  time.Duration `yaml:"orphan_branch_grace"`

	// Commits is what devbot does to the attribution of the commits it
	// pushes. A repo's keys override repos.defaults.commits one by one
	// (LayerCommits), even for a repo with its own file.
	Commits Commits `yaml:"commits"`

	// ChecksPendingTimeout is how long a PR head may have checks still
	// pending, from when devbot first saw them pending, before the task
	// stops: a runner that died, or a status nothing will ever finish, would
	// hold it in review.
	ChecksPendingTimeout time.Duration `yaml:"checks_pending_timeout"`

	// Integration, on a parent task's repo, is the check run over its
	// children's PR heads together before any of them merges. Without a
	// command, a parent's children merge as any task does.
	Integration Integration `yaml:"integration"`

	// Source is "file" for a repo's own .devbot.yaml, "defaults" for the
	// install's policy applied to a repo without one.
	Source string `yaml:"-"`
	// DefaultBranch is the branch the file was read from.
	DefaultBranch string `yaml:"-"`
}

// Integration is a repo's integration block: a command run from a
// workspace holding each child of a parent task at its PR head, and Repos
// besides, at their default branches.
type Integration struct {
	Command string        `yaml:"command"`
	Timeout time.Duration `yaml:"timeout"`
	// Repos are full refs ("gitea:acme/schema") of repos no child changes
	// that the command needs checked out too.
	Repos []string `yaml:"repos"`
	// Retries is how many times a run that couldn't finish (a clone that
	// failed, a pod that died) is tried again for the same heads. A command
	// that fails isn't retried: that is the result.
	Retries *int `yaml:"retries"`
}

// On reports whether the repo defines an integration check.
func (i *Integration) On() bool { return i.Command != "" }

// PullRequests is a repo's pull_requests block: whether devbot carries a PR
// it didn't open, handed to it with devbot/ready, to merge, and whether one
// from a fork too.
type PullRequests struct {
	Enabled *bool `yaml:"enabled"`
	Forks   *bool `yaml:"forks"`
}

// Commits is a repo's commits block. Both keys unset leaves the agent
// CLI's attribution as it is.
type Commits struct {
	// CoAuthoredBy false drops the AI co-author trailer and the "Generated
	// with" line from every commit devbot pushes, true makes sure the
	// trailer is there, and unset is the agent CLI's own default.
	CoAuthoredBy *bool `yaml:"co_authored_by"`
	// Trailers ("Signed-off-by: Bot <bot@example.com>") are added to every
	// commit devbot pushes, each once.
	Trailers []string `yaml:"trailers"`
}

// IsZero reports whether c leaves the agent CLI's attribution as it is and
// adds no trailers.
func (c Commits) IsZero() bool { return c.CoAuthoredBy == nil && len(c.Trailers) == 0 }

// LayerCommits lays r's commits block over d, the install's
// repos.defaults.commits, key by key: an unset key takes d's, and an
// explicit trailers: [] still means none.
func (r *Repo) LayerCommits(d Commits) {
	if r.Commits.CoAuthoredBy == nil {
		r.Commits.CoAuthoredBy = d.CoAuthoredBy
	}
	if r.Commits.Trailers == nil {
		r.Commits.Trailers = d.Trailers
	}
}

// trailerRE is a trailer as git interpret-trailers reads one: a token of
// letters, digits and dashes, a colon, and a value on the same line.
var trailerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*: *\S[^\r\n]*$`)

// AdoptsPRs reports whether devbot carries a PR handed to it in this repo.
// repos.limits.pull_requests.enabled false is already in it (Cap).
func (r *Repo) AdoptsPRs() bool {
	return r.PullRequests.Enabled == nil || *r.PullRequests.Enabled
}

// AdoptsForks reports whether devbot carries a PR from a fork handed to it
// in this repo: only where it adopts PRs at all. repos.limits is in it too.
func (r *Repo) AdoptsForks() bool {
	return r.AdoptsPRs() && (r.PullRequests.Forks == nil || *r.PullRequests.Forks)
}

// ParseRepo parses a .devbot.yaml and fills defaults.
func ParseRepo(b []byte) (*Repo, error) {
	r := &Repo{}
	if err := yaml.Unmarshal(b, r); err != nil {
		return nil, fmt.Errorf("%s: %w", RepoFile, err)
	}
	r.Source = "file"
	if err := r.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", RepoFile, err)
	}
	return r.fill(), nil
}

// defaultRepoYAML is the policy for a repo without a .devbot.yaml: devbot
// works like any outside contributor would. People merge; CI and release
// configuration are off-limits; the checks are whatever the project itself
// uses, which the implementer works out and the harness then replays.
const defaultRepoYAML = `
version: 1
enabled: true
automerge: false
require_checks: false
`

// DefaultProtectedPaths is protected_paths when a policy doesn't set it: what
// runs as CI or with its secrets, and what decides how git checks a tree out
// or who must review it. AlwaysProtected is added either way.
var DefaultProtectedPaths = append(slices.Clone(DefaultApprovablePaths),
	".pre-commit-config.yaml",
	// git and review
	".gitmodules", ".gitattributes", "CODEOWNERS",
)

// DefaultApprovablePaths is approvable_paths when a policy doesn't set it:
// the CI/CD configuration among DefaultProtectedPaths, which a person can
// read in a diff and approve.
var DefaultApprovablePaths = []string{
	".github/**", ".gitea/**", ".forgejo/**", ".gitlab-ci.yml", ".gitlab/**", ".circleci/**", ".buildkite/**", "Jenkinsfile",
}

// DefaultRepo is the policy for a repo with no .devbot.yaml: the built-in
// values above, with the install's repos.defaults laid over them.
func DefaultRepo(overrides map[string]any) (*Repo, error) {
	r := &Repo{}
	if err := yaml.Unmarshal([]byte(defaultRepoYAML), r); err != nil {
		return nil, err
	}
	if len(overrides) > 0 {
		b, err := yaml.Marshal(overrides)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(b, r); err != nil {
			return nil, err
		}
	}
	r.Enabled, r.Source = nil, "defaults"
	if err := r.validate(); err != nil {
		return nil, err
	}
	return r.fill(), nil
}

// A label key must be valid both as a Kubernetes label name and, behind
// label_, as a Prometheus label name; a value as a Kubernetes label value.
var (
	labelKeyRE   = regexp.MustCompile(`^[a-z]([a-z0-9_]{0,61}[a-z0-9])?$`)
	labelValueRE = regexp.MustCompile(`^(([A-Za-z0-9][-A-Za-z0-9_.]{0,61})?[A-Za-z0-9])?$`)
)

// ValidLabelValue reports whether v is a valid Kubernetes label value.
func ValidLabelValue(v string) bool { return labelValueRE.MatchString(v) }

// ValidLabelKey reports whether k is a valid static label key.
func ValidLabelKey(k string) bool { return labelKeyRE.MatchString(k) }

func (r *Repo) validate() error {
	// 0 or unset is the default, which fill applies; a negative is a typo.
	for _, n := range []struct {
		key string
		v   int
	}{{"max_attempts", r.MaxAttempts}, {"max_review_rounds", r.MaxReviewRounds}, {"max_rebase_rounds", r.MaxRebaseRounds}, {"review_budget_kib", r.ReviewBudgetKiB}} {
		if n.v < 0 {
			return fmt.Errorf("%s: %d is negative (0 or unset is the default)", n.key, n.v)
		}
	}
	if r.CheckRetries != nil && *r.CheckRetries < 0 {
		return fmt.Errorf("check_retries: %d is negative", *r.CheckRetries)
	}
	if r.MaxReplans != nil && *r.MaxReplans < 0 {
		return fmt.Errorf("max_replans: %d is negative", *r.MaxReplans)
	}
	if r.ChecksPendingTimeout < 0 {
		return fmt.Errorf("checks_pending_timeout: %s is negative (0 or unset is the default)", r.ChecksPendingTimeout)
	}
	if r.BaseBranch != "" && !ValidBranch(r.BaseBranch) {
		return fmt.Errorf("base_branch: %q: want %s", r.BaseBranch, BranchRule)
	}
	if r.Rebase != "" && r.Rebase != RebaseLazy && r.Rebase != RebaseEager {
		return fmt.Errorf("rebase: %q is neither %s nor %s", r.Rebase, RebaseLazy, RebaseEager)
	}
	if r.Approval != "" && r.Approval != ApprovalPerson && r.Approval != ApprovalAutoClean {
		return fmt.Errorf("approval: %q is neither %s nor %s", r.Approval, ApprovalPerson, ApprovalAutoClean)
	}
	for _, k := range slices.Sorted(maps.Keys(r.Labels)) {
		if !ValidLabelKey(k) {
			return fmt.Errorf("labels: key %q: want lowercase letters, digits and _, starting with a letter, at most 63", k)
		}
		if !ValidLabelValue(r.Labels[k]) {
			return fmt.Errorf("labels.%s: %q isn't a Kubernetes label value (at most 63 of A-Z a-z 0-9 - _ ., alphanumeric at each end)", k, r.Labels[k])
		}
	}
	for _, pat := range r.ApprovablePaths {
		if p, ok := alwaysProtectedBy(pat); ok {
			return fmt.Errorf("approvable_paths: %q reaches %s, which is always protected: a change to it always needs a person", pat, p)
		}
	}
	if err := r.validateIntegration(); err != nil {
		return err
	}
	for _, t := range r.Commits.Trailers {
		if !trailerRE.MatchString(t) {
			return fmt.Errorf("commits.trailers: %q isn't a \"Key: value\" trailer (a key of letters, digits and -, a colon, and a value on one line)", t)
		}
	}
	return r.ValidateServices()
}

func (r *Repo) validateIntegration() error {
	in := r.Integration
	if !in.On() && in.Timeout == 0 && in.Retries == nil && in.Repos == nil {
		return nil
	}
	if strings.TrimSpace(in.Command) == "" {
		return errors.New("integration.command: required: the command the check runs from the workspace root")
	}
	if in.Timeout < 0 {
		return fmt.Errorf("integration.timeout: %s is negative (0 or unset is the default)", in.Timeout)
	}
	if in.Retries != nil && (*in.Retries < 0 || *in.Retries > 10) {
		return fmt.Errorf("integration.retries: %d is outside 0 to 10", *in.Retries)
	}
	seen := map[forge.RepoRef]bool{}
	for _, s := range in.Repos {
		ref, err := forge.ParseRepo(s)
		if err != nil {
			return fmt.Errorf("integration.repos: %w", err)
		}
		if !ValidPathPart(ref.Provider) || !ValidPathPart(ref.Owner) || !ValidPathPart(ref.Name) {
			return fmt.Errorf("integration.repos: %q: its provider, owner and name are each a directory of the workspace, so each must be a plain name", s)
		}
		if seen[ref] {
			return fmt.Errorf("integration.repos: %q is listed twice", s)
		}
		seen[ref] = true
	}
	return nil
}

// pathPartRE is a provider, owner or repo name that is safe as one
// directory of a path: the integration workspace lays repos out by them.
var pathPartRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// ValidPathPart reports whether s is a single directory name, and not "."
// or "..".
func ValidPathPart(s string) bool { return pathPartRE.MatchString(s) && s != "." && s != ".." }

// branchRE is a conservative branch name. git takes far more (`;`, `$(...)`,
// backquotes), and a base ends up in commands and the agent's shell, so a
// name a shell would read as more than a word is refused up front.
var branchRE = regexp.MustCompile(`^[A-Za-z0-9_+@][A-Za-z0-9._/+@-]*$`)

// BranchRule says what ValidBranch takes, for messages.
const BranchRule = "a branch name of letters, digits and . _ / + @ -"

// ValidBranch reports whether b is a name devbot takes as a base branch.
func ValidBranch(b string) bool { return branchRE.MatchString(b) }

// modelRE is a model name as the harnesses' CLIs take one: Anthropic's and
// OpenAI's, Bedrock's IDs and ARNs, and suffixes like "[1m]". A task's model
// comes from a comment or a connector call and ends up on the agent's
// command line, so anything else is refused up front.
var modelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@\[\]-]{0,199}$`)

// ModelRule says what ValidModel takes, for messages.
const ModelRule = "a model name of up to 200 letters, digits and . _ : / @ [ ] -"

// ValidModel reports whether m is a name devbot takes as a task's model.
func ValidModel(m string) bool { return modelRE.MatchString(m) }

// IsEnabled: a .devbot.yaml is on unless it says enabled: false.
func (r *Repo) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

// AlwaysProtected are added to every repo's protected_paths, whether the
// policy came from the repo or from the install: .devbot.yaml, so devbot can
// never widen its own permissions, and the agent-config files the agent CLIs
// load, so a change can't plant instructions, hooks or MCP servers for the
// next run, or a result.json for the reviewer. Each matches at any depth,
// since the CLIs also load them from subdirectories.
var AlwaysProtected = []string{RepoFile, "**/.devbot/**", "**/.claude/**", "**/.codex/**", ".mcp.json", "CLAUDE.md", "CLAUDE.local.md", "AGENTS.md"}

// NeverApprovable are paths that, when protected, a change to always needs
// a person, wherever approvable_paths reaches (".github/**" covers
// ".github/CODEOWNERS"): who must review, and how git reads the tree,
// including whether a diff shows a file at all.
var NeverApprovable = []string{"CODEOWNERS", ".gitattributes"}

// alwaysSamples are paths each AlwaysProtected pattern matches, which no
// approvable_paths pattern may: one that covers any of them is refused,
// whatever glob it is written as ("**" as much as ".devbot.yaml").
var alwaysSamples = []string{RepoFile, "sub/" + RepoFile, ".devbot/x", "sub/.devbot/x", ".claude/x", "sub/.claude/x", ".codex/x", "sub/.codex/x", ".mcp.json", "CLAUDE.md", "CLAUDE.local.md", "AGENTS.md"}

// alwaysProtectedBy reports whether pat names an always-protected path, or
// one inside one (".claude/settings.json", "sub/.devbot/a"), or matches one
// of alwaysSamples, and which. A pattern that only reaches one through a
// wildcard spanning directories (".github/**" and ".github/.claude/x") is
// let through: approvable never approves an always-protected path anyway.
func alwaysProtectedBy(pat string) (string, bool) {
	for _, a := range AlwaysProtected {
		if a == pat || Glob(a, pat) {
			return "`" + a + "`", true
		}
	}
	for _, s := range alwaysSamples {
		if Glob(pat, s) {
			return "`" + s + "`", true
		}
	}
	return "", false
}

// fill applies the key defaults. An explicit protected_paths, even an empty
// one, replaces DefaultProtectedPaths; AlwaysProtected is added either way.
// approvable_paths is the same: [] approves nothing.
func (r *Repo) fill() *Repo {
	if r.MergeMethod == "" {
		r.MergeMethod = "squash"
	}
	if r.TaskAutomerge == nil {
		t := true
		r.TaskAutomerge = &t
	}
	if r.RequireChecks == nil {
		t := true
		r.RequireChecks = &t
	}
	if r.ChecksPendingTimeout == 0 {
		r.ChecksPendingTimeout = 2 * time.Hour
	}
	if r.MaxReviewRounds == 0 {
		r.MaxReviewRounds = 3
	}
	if r.MaxRebaseRounds == 0 {
		r.MaxRebaseRounds = 10
	}
	if r.ReviewBudgetKiB == 0 {
		r.ReviewBudgetKiB = 512
	}
	if r.Rebase == "" {
		r.Rebase = RebaseLazy
	}
	if r.Approval == "" {
		r.Approval = ApprovalPerson
	}
	if r.MaxAttempts == 0 {
		r.MaxAttempts = 3
	}
	if r.CheckRetries == nil {
		n := 2
		r.CheckRetries = &n
	}
	if r.MaxReplans == nil {
		n := 2
		r.MaxReplans = &n
	}
	if r.PullRequests.Enabled == nil {
		t := true
		r.PullRequests.Enabled = &t
	}
	if r.PullRequests.Forks == nil {
		t := true
		r.PullRequests.Forks = &t
	}
	if r.BranchCleanup == nil {
		t := true
		r.BranchCleanup = &t
	}
	if r.BranchCleanupGrace == 0 {
		r.BranchCleanupGrace = time.Hour
	}
	if r.OrphanBranchGrace == 0 {
		r.OrphanBranchGrace = 24 * time.Hour
	}
	if r.Timeouts.Implement == 0 {
		r.Timeouts.Implement = 60 * time.Minute
	}
	if r.Timeouts.Review == 0 {
		r.Timeouts.Review = 20 * time.Minute
	}
	if r.Integration.Timeout == 0 {
		r.Integration.Timeout = 30 * time.Minute
	}
	if r.Integration.Retries == nil {
		n := 2
		r.Integration.Retries = &n
	}
	if r.ProtectedPaths == nil {
		r.ProtectedPaths = append([]string(nil), DefaultProtectedPaths...)
	}
	for _, p := range AlwaysProtected {
		if !slices.Contains(r.ProtectedPaths, p) {
			r.ProtectedPaths = append(r.ProtectedPaths, p)
		}
	}
	if r.ApprovablePaths == nil {
		r.ApprovablePaths = append([]string(nil), DefaultApprovablePaths...)
	}
	return r
}

// capAt is v, or c when v is over it. A zero c is no ceiling.
func capAt[T int | time.Duration](v, c T) T {
	if c > 0 && v > c {
		return c
	}
	return v
}

// Cap brings r within l: a value over its ceiling becomes the ceiling, and
// a model the install doesn't allow gives way to the install's own.
func (r *Repo) Cap(l RepoLimits) {
	r.MaxAttempts = capAt(r.MaxAttempts, l.MaxAttempts)
	if l.CheckRetries != nil && *r.CheckRetries > *l.CheckRetries {
		n := *l.CheckRetries
		r.CheckRetries = &n
	}
	r.MaxReviewRounds = capAt(r.MaxReviewRounds, l.MaxReviewRounds)
	r.MaxRebaseRounds = capAt(r.MaxRebaseRounds, l.MaxRebaseRounds)
	r.Timeouts.Implement = capAt(r.Timeouts.Implement, l.Timeouts.Implement)
	r.Timeouts.Review = capAt(r.Timeouts.Review, l.Timeouts.Review)
	// It runs in an implement job pod, under that role's deadline.
	r.Integration.Timeout = capAt(r.Integration.Timeout, l.Timeouts.Implement)
	if l.Docker != nil && !*l.Docker {
		off := false
		r.Docker = &off
	}
	if l.PullRequests.Enabled != nil && !*l.PullRequests.Enabled {
		off := false
		r.PullRequests.Enabled = &off
	}
	if l.PullRequests.Forks != nil && !*l.PullRequests.Forks {
		off := false
		r.PullRequests.Forks = &off
	}
	for _, a := range []*RepoAgent{&r.Agents.Implement, &r.Agents.Review} {
		if !l.AllowsModel(a.Model) {
			a.Model = ""
		}
	}
}

// AllowsModel reports whether a repo or a task may pick model m: any, when
// models is empty.
func (l RepoLimits) AllowsModel(m string) bool {
	return len(l.Models) == 0 || m == "" || slices.Contains(l.Models, m)
}

// When an approved PR that is behind its base, or conflicts with it, is
// rebased: lazily, when it is about to merge or someone asks, or eagerly, as
// soon as the base moves.
const (
	RebaseLazy  = "lazy"
	RebaseEager = "eager"
)

// Who approves a change to approvable_paths: always a person, or devbot
// itself when the change's risk read finds nothing.
const (
	ApprovalPerson    = "person"
	ApprovalAutoClean = "auto_clean"
)

// RepoAgent is what a repo may change about a role's agent: the model, and
// which of the install's MCP servers for the role it gets.
type RepoAgent struct {
	Model string `yaml:"model"`
	// MCPServers, when set, is a subset of the role's servers in the
	// install's config; [] turns them off. Unset keeps the install's.
	MCPServers *[]string `yaml:"mcp_servers"`
}

// Agent resolves a role's agent: the install's, with the repo's model when
// it sets one, and the repo's choice among the install's MCP servers for the
// role. A repo can narrow those, never add one, so naming any other is an
// error.
func (r *Repo) Agent(role string, d Agents) (Agent, error) {
	a, o := d.Implement, r.Agents.Implement
	if role == "review" {
		a, o = d.Review, r.Agents.Review
	}
	if o.Model != "" {
		a.Model = o.Model
	}
	if o.MCPServers != nil {
		for _, n := range *o.MCPServers {
			if !slices.Contains(a.MCPServers, n) {
				have := "none"
				if len(a.MCPServers) > 0 {
					have = strings.Join(a.MCPServers, ", ")
				}
				return Agent{}, fmt.Errorf("agents.%s.mcp_servers: %q isn't one of the MCP servers this install gives the %s role (%s)", role, n, role, have)
			}
		}
		var keep []string
		for _, n := range a.MCPServers {
			if slices.Contains(*o.MCPServers, n) {
				keep = append(keep, n)
			}
		}
		a.MCPServers = keep
	}
	return a, nil
}

// HumanCheck reports whether the check named name is one of human_checks.
func (r *Repo) HumanCheck(name string) bool {
	return slices.ContainsFunc(r.HumanChecks, func(pat string) bool { return Glob(pat, name) })
}

// Generated reports whether path matches one of generated_paths.
func (r *Repo) Generated(path string) bool {
	return slices.ContainsFunc(r.GeneratedPaths, func(pat string) bool { return Glob(pat, path) })
}

// Protected returns the files among paths that match a protected pattern.
func (r *Repo) Protected(paths []string) []string {
	var hit []string
	for _, p := range paths {
		for _, pat := range r.ProtectedPaths {
			if Glob(pat, p) {
				hit = append(hit, p)
				break
			}
		}
	}
	return hit
}

// ProtectedChanges returns what in a change set needs a person, one line per
// path: a path matching a protected pattern (with renames undetected, both a
// move's old and new path are in the set), a new or retargeted symlink, which
// could point the next reader anywhere, and any change to a submodule pointer,
// which brings in code that no diff shows.
func (r *Repo) ProtectedChanges(cs []changeset.Change) []string {
	var hit []string
	seen := map[string]bool{}
	for _, c := range cs {
		var why string
		switch {
		case len(r.Protected([]string{c.Path})) > 0:
		case c.OldMode == changeset.ModeGitlink || c.NewMode == changeset.ModeGitlink:
			why = " (submodule)"
		case c.NewMode == changeset.ModeSymlink && c.OldMode != changeset.ModeSymlink:
			why = " (new symlink)"
		case c.NewMode == changeset.ModeSymlink:
			why = " (symlink changed)"
		default:
			continue
		}
		if !seen[c.Path] {
			seen[c.Path] = true
			hit = append(hit, ShowPath(c.Path)+why)
		}
	}
	return hit
}

// approvable reports whether a protected path is one a person may approve a
// change to: on approvable_paths, and neither always protected nor in
// NeverApprovable, whatever approvable_paths says.
func (r *Repo) approvable(path string) bool {
	for _, pat := range append(slices.Clone(AlwaysProtected), NeverApprovable...) {
		if Glob(pat, path) {
			return false
		}
	}
	return slices.ContainsFunc(r.ApprovablePaths, func(pat string) bool { return Glob(pat, path) })
}

// HeldChanges splits ProtectedChanges in two: what a person may approve
// (held, changes to protected files on approvable_paths), and the rest,
// which needs a person to make the change (hard, one line per path as
// ProtectedChanges words it). A symlink or submodule change is hard
// wherever it is.
func (r *Repo) HeldChanges(cs []changeset.Change) (held []changeset.Change, hard []string) {
	var keep []changeset.Change
	for _, c := range cs {
		link := c.OldMode == changeset.ModeGitlink || c.NewMode == changeset.ModeGitlink || c.NewMode == changeset.ModeSymlink
		if !link && len(r.Protected([]string{c.Path})) > 0 && r.approvable(c.Path) {
			held = append(held, c)
			continue
		}
		keep = append(keep, c)
	}
	return held, r.ProtectedChanges(keep)
}

// PathRules splits protected_paths as the implementer is told them: the
// patterns it must leave alone, and the approvable ones, which it may change
// when the task needs it, for a person to approve before anything is
// pushed. A pattern on approvable_paths that nothing protected covers
// pushes straight through, so it isn't listed; a protected pattern inside
// an approvable one is listed as approvable in its place.
func (r *Repo) PathRules() (hard, approvable []string) {
	for _, a := range r.ApprovablePaths {
		if slices.ContainsFunc(r.ProtectedPaths, func(p string) bool { return Glob(p, a) }) && !slices.Contains(approvable, a) {
			approvable = append(approvable, a)
		}
	}
	for _, p := range r.ProtectedPaths {
		switch {
		case slices.Contains(approvable, p):
		case r.approvable(p):
			approvable = append(approvable, p)
		default:
			hard = append(hard, p)
		}
	}
	return hard, approvable
}

// ShowPath puts a path in a markdown code span for a comment. The path is the
// model's choice, so one that could break out of the span or open a devbot
// marker is shown escaped.
func ShowPath(p string) string {
	if strings.ContainsAny(p, "`<") || strings.IndexFunc(p, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		p = strings.NewReplacer("`", `\x60`, "<", `\x3c`).Replace(strconv.QuoteToASCII(p))
	}
	return "`" + p + "`"
}

// RepoCache caches each repo's .devbot.yaml. A push to the default branch
// that touches the file invalidates it; the TTL bounds staleness when that
// webhook is lost.
type RepoCache struct {
	Forges forge.Registry
	Policy RepoPolicy
	TTL    time.Duration
	// Generation, when set, reads a per-repo counter every process shares
	// (queue.RepoConfigGeneration), which whoever gets the push bumps: an
	// entry cached under another generation is stale, whichever serve
	// replica the push was delivered to. Unreadable, the TTL alone applies.
	Generation func(ctx context.Context, repo string) (string, error)

	mu      sync.Mutex
	entries map[string]repoEntry
	signals map[string]DockerSignal // repo@commit -> what DetectDocker found there
}

type repoEntry struct {
	cfg *Repo // nil = not opted in
	err error
	at  time.Time
	gen string
}

// ErrNotOptedIn means devbot doesn't work in the repo. The wrapped message
// says why: opted out, outside the install's allow/deny, no .devbot.yaml
// where the install requires one, or the bots can't write to it.
var ErrNotOptedIn = errors.New("devbot isn't enabled for this repo")

func notOptedIn(why string) error { return fmt.Errorf("%w: %s", ErrNotOptedIn, why) }

// Get returns the repo's config, or ErrNotOptedIn.
func (c *RepoCache) Get(ctx context.Context, repo forge.RepoRef) (*Repo, error) {
	ttl := c.TTL
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	gen, genErr := "", error(nil)
	if c.Generation != nil {
		gen, genErr = c.Generation(ctx, repo.String())
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]repoEntry{}
	}
	e, ok := c.entries[repo.String()]
	c.mu.Unlock()
	if ok && time.Since(e.at) < ttl && (genErr != nil || e.gen == gen) {
		return e.cfg, e.err
	}
	cfg, err := c.load(ctx, repo)
	if err != nil && !errors.Is(err, ErrNotOptedIn) {
		return nil, err // transient: don't cache
	}
	c.mu.Lock()
	// Read before loading, so a bump during the load makes the next Get
	// load again rather than keep what may predate it.
	c.entries[repo.String()] = repoEntry{cfg: cfg, err: err, at: time.Now(), gen: gen}
	c.mu.Unlock()
	return cfg, err
}

func (c *RepoCache) load(ctx context.Context, repo forge.RepoRef) (*Repo, error) {
	if !c.Policy.Allowed(repo.String()) {
		return nil, notOptedIn("outside the install's repos.allow / repos.deny")
	}
	p, err := c.Forges.For(repo)
	if err != nil {
		return nil, err
	}
	branch, err := p.DefaultBranch(ctx, repo)
	if errors.Is(err, forge.ErrNotFound) {
		return nil, notOptedIn("the bots can't see it")
	}
	if err != nil {
		return nil, err
	}
	var cfg *Repo
	b, err := p.GetFile(ctx, repo, branch, RepoFile)
	switch {
	case errors.Is(err, forge.ErrNotFound):
		if c.Policy.OptIn == "file" {
			return nil, notOptedIn("no " + RepoFile + " on " + branch + ", and this install requires one")
		}
		if cfg, err = DefaultRepo(c.Policy.Defaults); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		if cfg, err = ParseRepo(b); err != nil {
			return nil, err
		}
		// The install's commits block is the base a file's overrides key by
		// key, unlike the rest of repos.defaults.
		d, err := DefaultRepo(c.Policy.Defaults)
		if err != nil {
			return nil, err
		}
		cfg.LayerCommits(d.Commits)
		if !cfg.IsEnabled() {
			return nil, notOptedIn(RepoFile + " sets enabled: false")
		}
	}
	// Whoever can push writes .devbot.yaml, so the install has the last word.
	cfg.Cap(c.Policy.Limits)
	// Both bots must be able to write: the implementer pushes branches and
	// moves labels, the reviewer reviews and merges. Granting them write
	// access is what puts a repo in reach. A job pod can only ask as its own
	// bot; serve asked as both before it queued the job.
	impl, rev := p.BotUsers()
	for role, u := range map[forge.Role]string{forge.RoleImplementer: impl, forge.RoleReviewer: rev} {
		ok, err := p.CanWrite(ctx, repo, role)
		if errors.Is(err, forge.ErrNoCredential) {
			continue
		}
		var me *forge.MissingAccessError
		if errors.As(err, &me) {
			return nil, notOptedIn(me.Need)
		}
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, notOptedIn(u + " has no write access")
		}
	}
	cfg.DefaultBranch = branch
	return cfg, nil
}

// Invalidate drops a repo's cached config.
func (c *RepoCache) Invalidate(repo forge.RepoRef) {
	c.mu.Lock()
	delete(c.entries, repo.String())
	c.mu.Unlock()
}

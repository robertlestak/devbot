// Package forge is the boundary between devbot and a code forge (Gitea,
// GitHub, ...). Everything above it speaks these types; everything
// forge-specific - API shapes, auth, webhook signatures, label IDs - stays in
// an adapter.
//
// The interface is deliberately the lowest common denominator: issues,
// comments, labels, pull requests, reviews, a combined commit status and a
// re-run of the forge's own CI behind it, what a branch's own protection asks
// of a merge, a merge, files attached to a PR, listing and deleting
// branches, listing tags, and whether one commit's history has another
// (every forge's compare). Features only one forge has (GitHub Projects, Gitea time
// tracking, native dependencies, native auto-merge) are not modelled, so
// nothing in the core can come to depend on them.
package forge

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned for a missing issue, PR, file or user.
var ErrNotFound = errors.New("not found")

// ErrNotMergeable is Merge refused because the PR can't be merged as it
// stands: it conflicts, or the repo's rules block it. ErrHeadMoved is Merge
// refused because the PR's head is no longer the SHA asked for.
// ErrMergePending is Merge refused for now: the forge is still working out
// whether the PR can merge, and asks to be tried again later. Adapters whose
// forge tells these apart wrap them, with the forge's own reason.
var (
	ErrNotMergeable = errors.New("pull request not mergeable")
	ErrHeadMoved    = errors.New("pull request head moved")
	ErrMergePending = errors.New("pull request mergeability still being checked")
)

// ErrForbidden is the forge refusing an authenticated call: the identity
// lacks the permission. ErrUnauthorized is the forge rejecting the token
// itself (expired, revoked, or never valid).
var (
	ErrForbidden    = errors.New("forbidden by the forge")
	ErrUnauthorized = errors.New("token rejected by the forge")
)

// ErrNoCredential is a call made as a role whose credential this process
// doesn't hold: a job pod has only its own role's.
var ErrNoCredential = errors.New("no credential for that bot in this process")

// ErrNotRerunnable is RerunChecks asked to re-run a check that isn't a run of
// the forge's own CI (Actions), which is all it can re-run.
var ErrNotRerunnable = errors.New("not a run of the forge's own CI, so it can't be re-run through the forge")

// Permission is an actor's role on a repo, lowest first. Each adapter maps its
// forge's roles onto these, and a role its forge lacks onto the nearest one
// below (Gitea has no maintain, so maintain means write there).
type Permission int

const (
	PermNone Permission = iota
	PermRead
	PermWrite
	PermMaintain
	PermAdmin
)

var permNames = []string{"none", "read", "write", "maintain", "admin"}

func (p Permission) String() string {
	if p < 0 || int(p) >= len(permNames) {
		return "unknown"
	}
	return permNames[p]
}

// ParsePermission parses "none", "read", "write", "maintain" or "admin".
func ParsePermission(s string) (Permission, error) {
	for i, n := range permNames {
		if n == s {
			return Permission(i), nil
		}
	}
	return PermNone, fmt.Errorf("permission %q: want one of %s", s, strings.Join(permNames, ", "))
}

// LookupForbiddenError is Authorized's error when the forge won't tell the
// bot an actor's permission. That is a setup problem, not a "no": Need says
// what access the bot lacks, for whoever runs the install.
type LookupForbiddenError struct {
	Need string
	Err  error
}

func (e *LookupForbiddenError) Error() string {
	return "permission lookup forbidden (" + e.Need + "): " + e.Err.Error()
}

func (e *LookupForbiddenError) Unwrap() error { return e.Err }

// MissingAccessError is CanWrite's error when a bot reaches the repo but
// without the access devbot needs. Need names the bot, what it has and what
// it needs, for whoever runs the install.
type MissingAccessError struct {
	Need string
}

func (e *MissingAccessError) Error() string { return e.Need }

// RepoRef names a repository on a configured provider: "gitea:acme/webapp".
type RepoRef struct {
	Provider string
	Owner    string
	Name     string
}

func (r RepoRef) String() string { return r.Provider + ":" + r.Owner + "/" + r.Name }

// FullName is the forge's own "owner/name".
func (r RepoRef) FullName() string { return r.Owner + "/" + r.Name }

// SameLogin reports whether a and b name the same user. Every forge devbot
// supports ignores case in logins, while config and API responses may each
// spell one differently.
func SameLogin(a, b string) bool { return strings.EqualFold(a, b) }

// HasLogin reports whether list holds login, in any case.
func HasLogin(list []string, login string) bool {
	for _, x := range list {
		if SameLogin(x, login) {
			return true
		}
	}
	return false
}

// ParseRepo parses "provider:owner/name".
func ParseRepo(s string) (RepoRef, error) {
	p, rest, ok := strings.Cut(s, ":")
	if !ok || p == "" {
		return RepoRef{}, fmt.Errorf("repo %q: want provider:owner/name", s)
	}
	o, n, ok := strings.Cut(rest, "/")
	if !ok || o == "" || n == "" || strings.Contains(n, "/") {
		return RepoRef{}, fmt.Errorf("repo %q: want provider:owner/name", s)
	}
	return RepoRef{Provider: p, Owner: o, Name: n}, nil
}

// ItemRef names an issue: "gitea:acme/webapp#12".
type ItemRef struct {
	Repo   RepoRef
	Number int
}

func (r ItemRef) String() string { return r.Repo.String() + "#" + strconv.Itoa(r.Number) }

// ParseItem parses "provider:owner/name#number".
func ParseItem(s string) (ItemRef, error) {
	repo, num, ok := strings.Cut(s, "#")
	if !ok {
		return ItemRef{}, fmt.Errorf("item %q: want provider:owner/name#number", s)
	}
	r, err := ParseRepo(repo)
	if err != nil {
		return ItemRef{}, err
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return ItemRef{}, fmt.Errorf("item %q: bad number", s)
	}
	return ItemRef{Repo: r, Number: n}, nil
}

// Item is an issue. PRs are separate (PR), even on forges that store them as
// issues, because devbot's state lives on the issue and the PR is its output.
type Item struct {
	Ref       ItemRef
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []string
	Author    string
	URL       string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// HasLabel reports whether the item carries label.
func (i Item) HasLabel(label string) bool {
	for _, l := range i.Labels {
		if l == label {
			return true
		}
	}
	return false
}

type Comment struct {
	ID        int64
	Author    string
	Body      string
	CreatedAt time.Time
	// UpdatedAt is when the body was last edited, CreatedAt if never.
	// Anyone with write access can edit any comment, devbot's included.
	UpdatedAt time.Time
}

// Edited reports whether the comment's body changed after it was posted.
func (c Comment) Edited() bool { return c.UpdatedAt.After(c.CreatedAt) }

type PR struct {
	Repo       RepoRef
	Number     int
	Title      string
	Body       string
	State      string // "open" | "closed"
	Merged     bool
	HeadBranch string
	HeadSHA    string
	BaseBranch string
	// Fork is true when the head branch is in another repo than Repo, or
	// the forge no longer knows which (a deleted fork).
	Fork bool
	// HeadRepo is the repo the head branch is in: Repo, or the fork. Zero
	// once a fork is deleted. It is a name on Repo's forge, never a URL the
	// forge advertises: git reaches it through the forge's configured URL.
	HeadRepo RepoRef
	// MaintainerCanModify is a fork PR's author letting those who can push
	// to Repo push to its head branch too: GitHub's "Allow edits by
	// maintainers", Gitea's "Allow edits from maintainers".
	MaintainerCanModify bool
	// Mergeable is false when the forge knows the PR conflicts with its base.
	Mergeable bool
	// MergeableUnknown is true while the forge is still working out whether
	// the PR conflicts. Mergeable is true meanwhile: nothing is known to
	// conflict, but nothing is known to merge cleanly either.
	MergeableUnknown bool
	// Blocked is true when the repo's own rules (branch protection: required
	// reviews or checks) refuse the merge as things stand, although the PR
	// doesn't conflict.
	Blocked bool
	// Behind is true when the base branch has moved past the PR's merge base,
	// so merging would land a combination nobody tested.
	Behind    bool
	URL       string
	Author    string
	CreatedAt time.Time
	// ClosedAt is when the PR was closed or merged; zero while it is open.
	ClosedAt time.Time
	// MergeSHA is the commit a merged PR left on its base: the merge
	// commit, or the squash's or the rebase's last commit. "" until it
	// merges.
	MergeSHA string
	Labels   []string
}

// HasLabel reports whether the PR carries label.
func (p PR) HasLabel(label string) bool { return slices.Contains(p.Labels, label) }

// TreeFile is one file in a tree ListTree listed.
type TreeFile struct {
	Path string
	Size int64
}

// ErrTreeTooBig is ListTree refusing a tree the forge won't list in full.
var ErrTreeTooBig = errors.New("tree too big to list in full")

// Branch is a branch and its head commit.
type Branch struct {
	Name string
	SHA  string
	// CommittedAt is when the head commit was committed.
	CommittedAt time.Time
}

// Tag is a tag and the commit it points at (an annotated tag's, peeled).
type Tag struct {
	Name string
	SHA  string
}

// CheckState is the combined status of every check on one commit.
type CheckState string

const (
	ChecksNone    CheckState = "none" // nothing has reported
	ChecksPending CheckState = "pending"
	ChecksSuccess CheckState = "success"
	ChecksFailure CheckState = "failure"
)

type CheckDetail struct {
	Context     string
	State       string
	Description string
	URL         string
	// Failed is true for a check that finished without passing: State is
	// the forge's own word, and which of its words pass is the adapter's to
	// know.
	Failed bool
	// Cancelled is true for a failed check whose run was cancelled before
	// it finished, which says nothing about the code.
	Cancelled bool
	// Pending is true for a check that hasn't finished, in the adapter's
	// reading of the forge's words, as Failed.
	Pending bool
	// ID is the forge's own ID for a check that has one apart from its
	// commit status (a GitHub check run), for CheckReport; 0 otherwise.
	ID int64
}

// CommitStatus is a status devbot reports on a commit (SetCommitStatus).
// State is "pending", "success" or "failure".
type CommitStatus struct {
	Context, State, Description, URL string
}

// CheckReport is what the forge says about why one check failed, beyond its
// CheckDetail line. All of it is CI's output from running the PR's code, so
// it is untrusted, and its forge's secret masking is left as it is.
type CheckReport struct {
	// Title, Summary and Text are a check run's output, on forges whose
	// checks have one (GitHub's check runs, from any App).
	Title, Summary, Text string
	// Annotations are a check run's, every page of them up to
	// MaxAnnotations; MoreAnnotations is true when it had more.
	Annotations     []Annotation
	MoreAnnotations bool
	// Log is the end of the log of a job of the forge's own CI (Actions):
	// its last lines, as many as CheckReport was asked for. LogCut is true
	// when the log had more.
	Log    string
	LogCut bool
	// HasLog is true for a check that is such a job, even when its log was
	// empty.
	HasLog bool
}

// Annotation is one finding a check run attached to a line range of a file.
type Annotation struct {
	Path               string
	StartLine, EndLine int
	Level              string // notice, warning or failure
	Title, Message     string
}

// MaxAnnotations is the most annotations CheckReport reads of one check.
const MaxAnnotations = 500

// MaxLogLine is the most bytes TailLines keeps of one line: a minified
// bundle or a progress bar can be megabytes without a newline.
const MaxLogLine = 4 << 10

// TailLines reads r to the end and returns its last n lines, each cut to
// MaxLogLine bytes, and whether there were more. Only those lines are held
// in memory, however long the log.
func TailLines(r io.Reader, n int) (string, bool, error) {
	if n <= 0 {
		return "", false, nil
	}
	br := bufio.NewReaderSize(r, 64<<10)
	ring := make([][]byte, n)
	count := 0
	var cur []byte
	long := false
	push := func() {
		ring[count%n] = cur
		count++
		cur, long = nil, false
	}
	for {
		frag, err := br.ReadSlice('\n')
		if !long {
			room := MaxLogLine - len(cur)
			if len(frag) > room {
				frag, long = frag[:room], true
			}
			cur = append(cur, frag...)
		}
		switch {
		case err == nil:
			cur = bytes.TrimRight(cur, "\n")
			push()
		case errors.Is(err, bufio.ErrBufferFull):
		case errors.Is(err, io.EOF):
			if len(cur) > 0 {
				push()
			}
			var b strings.Builder
			for i := max(0, count-n); i < count; i++ {
				b.Write(ring[i%n])
				b.WriteByte('\n')
			}
			return strings.TrimSuffix(b.String(), "\n"), count > n, nil
		default:
			return "", false, err
		}
	}
}

// Protection is what a repo's own rules (branch protection, rulesets) ask of
// a merge into one branch, as far as the bots can see them. devbot's merge
// gate is its own; the forge enforces these on top of it.
type Protection struct {
	// RequiredChecks are the checks that must pass before the forge allows
	// the merge, by the name CombinedChecks reports each under.
	RequiredChecks []string
	// UpToDate is true when the forge only merges a PR that is up to date
	// with the branch, so one that is behind needs a rebase first.
	UpToDate bool
	// Rules describe, for a person, each rule that can hold the merge back.
	Rules []string
	// Exempt says, for a person, how to let the reviewer identity, which
	// merges, past the rules on this forge.
	Exempt string
}

type Verdict string

const (
	VerdictApprove        Verdict = "approve"
	VerdictRequestChanges Verdict = "request_changes"
	VerdictComment        Verdict = "comment"
)

type Review struct {
	// ID is the forge's for the review, which its ReviewComments carry.
	ID          int64
	Author      string
	Verdict     Verdict
	CommitSHA   string
	Body        string
	SubmittedAt time.Time
	Dismissed   bool
}

// ReviewComment is one comment a review left on a line of a PR's diff.
type ReviewComment struct {
	ID int64
	// ReviewID is the Review it was left in.
	ReviewID int64
	Author   string
	Body     string
	Path     string
	// Line is the line of Path it is on, in the commit it was left on; 0
	// when the forge no longer places it on a line.
	Line      int
	CommitSHA string
	CreatedAt time.Time
}

// LabelEvent is one add or remove of a label on an issue or a PR, with who
// did it. It is how devbot checks that devbot/ready was applied by someone
// allowed to start work, whichever path (webhook or reconciler) noticed the
// label.
type LabelEvent struct {
	Label string
	Actor string
	Added bool
	At    time.Time
}

// ContentEdit is one change to an issue's title or body after it was filed,
// with who made it. It is how devbot tells whether an issue that changed
// after it was approved was changed by someone allowed to direct the work.
type ContentEdit struct {
	Field  string // "title" or "body"
	Editor string // "" when the forge records the edit but not who made it
	At     time.Time
}

const (
	FieldTitle = "title"
	FieldBody  = "body"
)

type Label struct {
	Name        string
	Color       string // hex without '#'
	Description string
}

// Role picks which bot identity an action is taken as.
type Role string

const (
	RoleImplementer Role = "implementer"
	RoleReviewer    Role = "reviewer"
)

// EventKind is what a webhook delivery was about. Handlers treat it as a hint
// for which object to re-read, never as the state itself.
type EventKind string

const (
	EventItem          EventKind = "item"           // issue opened/edited/labelled/commented/closed
	EventPR            EventKind = "pr"             // PR opened/synced/labelled/reviewed/closed
	EventStatus        EventKind = "status"         // a commit status changed
	EventDefaultBranch EventKind = "default_branch" // push to the default branch
	EventTag           EventKind = "tag"            // a tag pushed or moved
	EventIgnored       EventKind = "ignored"
)

type Event struct {
	DeliveryID string
	Kind       EventKind
	Repo       RepoRef
	Number     int      // issue or PR number, when there is one
	SHA        string   // for status events
	Paths      []string // a default-branch push's changed paths; empty when the forge can't list them all
	Actor      string

	// What the delivery itself says about the issue or PR, for the engine
	// to skip one that can't concern devbot before it asks the forge
	// anything. Facts is set when the adapter filled them in from the
	// payload; an event without it is never skipped on them.
	Facts   bool
	Labels  []string // the item's labels after the change; none on a check event, which names only the PR
	Head    string   // a PR's head branch, when the payload names it
	Comment string   // the body of the comment the delivery is about
}

// Provider is one forge. Every method that writes acts as the implementer
// identity unless it takes a Role or says otherwise.
//
// A provider built with one bot's credential alone (a job pod's) acts as
// that bot in every method that takes no Role, and fails one asked to act
// as the other with ErrNoCredential. BotUsers still names both.
type Provider interface {
	Name() string
	// BotUsers returns the implementer's and the reviewer's logins.
	BotUsers() (implementer, reviewer string)

	// ParseWebhook verifies the delivery's signature and normalises it.
	ParseWebhook(r *http.Request, body []byte) ([]Event, error)
	// SearchItems lists issues carrying label across every repo the bot can
	// see. state is "open", "closed" or "all". A repo it can't read doesn't
	// hide the others: the error comes back with the items it did read.
	SearchItems(ctx context.Context, label, state string) ([]Item, error)
	// ListItems lists repo's issues carrying label, as SearchItems does
	// across every repo: the requests it makes are to repo alone, however
	// many repos the bot can see.
	ListItems(ctx context.Context, repo RepoRef, label, state string) ([]Item, error)
	// SearchPRs lists the open PRs carrying label across every repo the bot
	// can see, with what a listing has: Repo, Number, Title, Labels and URL
	// at least, and GetPR reads the rest. A repo it can't read doesn't hide
	// the others, as in SearchItems.
	SearchPRs(ctx context.Context, label string) ([]PR, error)

	CreateItem(ctx context.Context, repo RepoRef, title, body string, labels []string) (Item, error)
	GetItem(ctx context.Context, ref ItemRef) (Item, error)
	CloseItem(ctx context.Context, ref ItemRef) error
	// EditItem replaces an issue's title and body.
	EditItem(ctx context.Context, ref ItemRef, title, body string) error
	// ListComments, Comment, SetLabels, LabelEvents and CloseItem take a
	// PR's number as well as an issue's: on every forge devbot supports the
	// two share one sequence, and a PR's conversation, labels and state are
	// its issue's.
	ListComments(ctx context.Context, ref ItemRef) ([]Comment, error)
	Comment(ctx context.Context, ref ItemRef, body string) error
	// UpdateComment replaces the body of comment id, one the acting bot
	// posted on issue or PR ref.
	UpdateComment(ctx context.Context, ref ItemRef, id int64, body string) error
	SetLabels(ctx context.Context, ref ItemRef, add, remove []string) error
	EnsureLabels(ctx context.Context, repo RepoRef, labels []Label) error
	LabelEvents(ctx context.Context, ref ItemRef) ([]LabelEvent, error)
	// ContentEdits lists the edits to an issue's title and body, oldest
	// first, as far as the forge's API records them. An edit it doesn't
	// record is missing, so a change with no edit to show for it was made
	// by someone unknown.
	ContentEdits(ctx context.Context, ref ItemRef) ([]ContentEdit, error)

	OpenPR(ctx context.Context, repo RepoRef, head, base, title, body string) (PR, error)
	UpdatePR(ctx context.Context, repo RepoRef, number int, title, body string) error
	// SetPRBase changes the branch an open PR merges into.
	SetPRBase(ctx context.Context, repo RepoRef, number int, base string) error
	GetPR(ctx context.Context, repo RepoRef, number int) (PR, error)
	ListOpenPRs(ctx context.Context, repo RepoRef) ([]PR, error)
	CombinedChecks(ctx context.Context, repo RepoRef, sha string) (CheckState, []CheckDetail, error)
	// SetCommitStatus reports s on sha as the acting bot: a check of
	// devbot's own, which CombinedChecks then lists with the rest. Another
	// with the same context replaces it.
	SetCommitStatus(ctx context.Context, repo RepoRef, sha string, s CommitStatus) error
	// RerunChecks re-runs the forge's own CI runs (Actions) behind checks
	// that CombinedChecks reported on sha, on the same commit: each run once,
	// however many of the checks are its jobs, and in each only the jobs that
	// were cancelled or failed. A check from elsewhere fails it with
	// ErrNotRerunnable.
	RerunChecks(ctx context.Context, repo RepoRef, sha string, checks []CheckDetail) error
	// CheckReport reads what the forge has on why check, one CombinedChecks
	// reported failed on sha, failed: a check run's output and annotations,
	// and the last logLines lines of the log of a job of the forge's own CI.
	// A plain commit status has nothing past its description and URL, which
	// is the zero CheckReport. Logs that are gone, or a forge without the API
	// for them, are an error, as for any other call.
	CheckReport(ctx context.Context, repo RepoRef, sha string, check CheckDetail, logLines int) (CheckReport, error)
	// Protection reads the repo's own rules for merging into branch. A
	// branch with none has the zero Protection.
	Protection(ctx context.Context, repo RepoRef, branch string) (Protection, error)
	// Review posts a review as the reviewer identity, pinned to sha.
	Review(ctx context.Context, repo RepoRef, number int, sha string, v Verdict, body string) error
	Reviews(ctx context.Context, repo RepoRef, number int) ([]Review, error)
	// ReviewComments lists the comments every review on PR number left on
	// lines of its diff, oldest first, however many pages that takes.
	ReviewComments(ctx context.Context, repo RepoRef, number int) ([]ReviewComment, error)
	// Merge merges as the reviewer identity, and only if the head is still sha.
	Merge(ctx context.Context, repo RepoRef, number int, sha, method string) error
	// AttachToPR uploads a file to a PR as the implementer and returns a URL
	// that markdown in the PR body can link or embed.
	AttachToPR(ctx context.Context, repo RepoRef, number int, name string, data []byte) (url string, err error)
	// GetAttachment downloads, as role, a file whose URL AttachToPR returned
	// for PR number. The URL is read back from a PR body anyone with write
	// access can edit, so an adapter refuses one that isn't attached to that
	// PR rather than fetch whatever the role's token can read.
	GetAttachment(ctx context.Context, repo RepoRef, number int, url string, role Role) ([]byte, error)
	// ReleaseAttachments frees what AttachToPR stored for PR number outside
	// the PR itself, once the PR is closed or merged. An adapter whose forge
	// keeps attachments with the PR has nothing to do. Releasing twice, or a
	// PR with no attachments, is not an error.
	ReleaseAttachments(ctx context.Context, repo RepoRef, number int) error

	// The forge's web pages for an issue, a PR and a branch, for people to
	// follow from devbot's web UI. Built without a call to the forge.
	IssueURL(ref ItemRef) string
	PRURL(repo RepoRef, number int) string
	BranchURL(repo RepoRef, branch string) string

	DefaultBranch(ctx context.Context, repo RepoRef) (string, error)
	// BranchExists reports whether repo has branch.
	BranchExists(ctx context.Context, repo RepoRef, branch string) (bool, error)
	// ListRepos lists the repos the implementer has access to.
	ListRepos(ctx context.Context) ([]RepoRef, error)
	// ListBranches lists repo's branches whose names start with prefix.
	ListBranches(ctx context.Context, repo RepoRef, prefix string) ([]Branch, error)
	// DeleteBranch deletes a branch as the implementer. It returns
	// ErrNotFound for a branch that is already gone, which callers treat as
	// success: the forge's own delete-after-merge often gets there first.
	DeleteBranch(ctx context.Context, repo RepoRef, name string) error
	// BranchHead is the commit branch points at, or ErrNotFound when repo
	// has no such branch.
	BranchHead(ctx context.Context, repo RepoRef, branch string) (string, error)
	// ListTags lists repo's tags, newest first as far as the forge orders
	// them, at most n of them.
	ListTags(ctx context.Context, repo RepoRef, n int) ([]Tag, error)
	// Contains reports whether commit ancestor is in the history of ref (a
	// commit SHA or a tag), ref's own commit included, as the forge's
	// compare of the two says. A commit or tag the forge doesn't know is
	// ErrNotFound.
	Contains(ctx context.Context, repo RepoRef, ref, ancestor string) (bool, error)
	// GetFile returns ErrNotFound for a missing path.
	GetFile(ctx context.Context, repo RepoRef, ref, path string) ([]byte, error)
	// ListTree lists every file in the tree at ref (a branch or a commit),
	// by its path from the root. A tree too big for the forge to list in
	// full is ErrTreeTooBig, never part of one.
	ListTree(ctx context.Context, repo RepoRef, ref string) ([]TreeFile, error)
	// GitAuth returns a clone URL with no credentials in it, and the HTTP
	// header that authenticates git as role. The header is passed to git per
	// command and never written into .git/config, where the model could read
	// it.
	GitAuth(ctx context.Context, repo RepoRef, role Role) (cloneURL, header string, err error)
	// Authorized reports whether the forge gives actor at least min on repo,
	// directly or through an org team. When the forge won't say, it returns
	// false and a *LookupForbiddenError.
	Authorized(ctx context.Context, repo RepoRef, actor string, min Permission) (bool, error)
	// CanWrite reports whether a bot identity can push to repo, asked as that
	// identity (forges may not let one user read another's permissions).
	// A bot that reaches repo without enough access gets false and a
	// *MissingAccessError; one that can't reach it at all, false and nil.
	CanWrite(ctx context.Context, repo RepoRef, role Role) (bool, error)
	// CanPushWorkflows reports whether git, authenticated by GitAuth as
	// role, may push changes to the forge's own CI workflow files. A forge
	// that lets anyone who can push change them answers true. It only
	// explains a refused push: what the work may touch is protected_paths'.
	CanPushWorkflows(ctx context.Context, repo RepoRef, role Role) (bool, error)

	// The install's one OAuth app on the forge, through which people sign
	// in to devbot (auth.mode oauth). The forge is the identity provider.
	//
	// OAuthURL is the forge's login and consent page for the app, asking for
	// only the scopes User needs. challenge is a PKCE S256 code challenge.
	// It fails when the install has no OAuth app configured for this forge.
	OAuthURL(redirectURI, state, challenge string) (string, error)
	// OAuthExchange trades the code the forge sent back for the person's token.
	OAuthExchange(ctx context.Context, redirectURI, code, verifier string) (UserToken, error)
	// OAuthRefresh renews a person's token. Forges that rotate refresh
	// tokens return the new one; an error means the person must sign in again.
	OAuthRefresh(ctx context.Context, refresh string) (UserToken, error)
	// CurrentUser is the login a person's token belongs to.
	CurrentUser(ctx context.Context, token string) (string, error)
	// AsUser is the forge as the person token belongs to.
	AsUser(token string) User
}

// UserToken is a person's OAuth token for the install's app on a forge.
type UserToken struct {
	Access  string    `json:"access"`
	Refresh string    `json:"refresh,omitempty"`
	Expiry  time.Time `json:"expiry,omitzero"` // zero: it doesn't expire
}

// User does what a person starts through a connector, with their own token,
// so the forge enforces their permissions and records them as the actor.
// The bots never do these on a signed-in person's behalf. Calls the forge
// refuses fail with ErrForbidden (or ErrNotFound, for a repo the person
// can't see), and with ErrUnauthorized when the token is no longer good.
type User interface {
	// Permission is the person's own access to repo, asked as them:
	// PermNone when they can't see it.
	Permission(ctx context.Context, repo RepoRef) (Permission, error)
	// CanEditIssues reports whether the person may label issues on repo.
	// A forge can grant that apart from push (Gitea's per-unit team
	// permissions, GitHub's triage role), and may drop labels it won't let
	// the person set at create instead of refusing.
	CanEditIssues(ctx context.Context, repo RepoRef) (bool, error)
	CreateItem(ctx context.Context, repo RepoRef, title, body string, labels []string) (Item, error)
	// CloseItem closes an issue the person filed.
	CloseItem(ctx context.Context, ref ItemRef) error
	Comment(ctx context.Context, ref ItemRef, body string) error
	SetLabels(ctx context.Context, ref ItemRef, add, remove []string) error
}

// Registry resolves a provider by the name used in RepoRefs.
type Registry map[string]Provider

func (r Registry) For(repo RepoRef) (Provider, error) {
	p, ok := r[repo.Provider]
	if !ok {
		return nil, fmt.Errorf("no provider %q configured; this install has: %s", repo.Provider, strings.Join(r.Names(), ", "))
	}
	return p, nil
}

// Names is the providers' names, sorted.
func (r Registry) Names() []string { return slices.Sorted(maps.Keys(r)) }

// LogValue makes refs log as "gitea:acme/app" and "gitea:acme/app#12".
func (r RepoRef) LogValue() slog.Value { return slog.StringValue(r.String()) }
func (r ItemRef) LogValue() slog.Value { return slog.StringValue(r.String()) }

// RateLimitWaits counts, in this process, the times an adapter held a call
// back for its forge's rate limit and for how long, by provider and kind
// ("primary" or "secondary"), for serve's /metrics.
var RateLimitWaits = &WaitCounter{}

type WaitCounter struct {
	mu sync.Mutex
	m  map[[2]string]WaitStat
}

type WaitStat struct {
	Count   int
	Seconds float64
}

func (w *WaitCounter) Add(provider, kind string, d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.m == nil {
		w.m = map[[2]string]WaitStat{}
	}
	s := w.m[[2]string{provider, kind}]
	s.Count++
	s.Seconds += d.Seconds()
	w.m[[2]string{provider, kind}] = s
}

// Snapshot is the counts so far, keyed by provider and kind.
func (w *WaitCounter) Snapshot() map[[2]string]WaitStat {
	w.mu.Lock()
	defer w.mu.Unlock()
	return maps.Clone(w.m)
}

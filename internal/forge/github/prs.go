package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
)

type branchRef struct {
	Ref  string    `json:"ref"`
	SHA  string    `json:"sha"`
	Repo *repoName `json:"repo"` // null once a fork is deleted
}

type repoName struct {
	FullName string `json:"full_name"`
}

// fork reports whether a PR's head is outside r.
func (b branchRef) fork(r forge.RepoRef) bool {
	return b.Repo == nil || !strings.EqualFold(b.Repo.FullName, r.Owner+"/"+r.Name)
}

// repo is the repo the branch is in, on r's forge, or zero for a deleted
// fork. The forge's clone URL for it is left alone: it may not be the
// address devbot is configured to reach the forge at.
func (b branchRef) repo(r forge.RepoRef) forge.RepoRef {
	if b.Repo == nil {
		return forge.RepoRef{}
	}
	owner, name, ok := strings.Cut(b.Repo.FullName, "/")
	if !ok {
		return forge.RepoRef{}
	}
	return forge.RepoRef{Provider: r.Provider, Owner: owner, Name: name}
}

type pull struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	// Merged is only in a single PR; a list has MergedAt.
	Merged   bool       `json:"merged"`
	MergedAt *time.Time `json:"merged_at"`
	// Mergeable is null while GitHub works it out, and missing from lists,
	// as is MergeableState.
	Mergeable      *bool      `json:"mergeable"`
	MergeableState string     `json:"mergeable_state"`
	Head           branchRef  `json:"head"`
	Base           branchRef  `json:"base"`
	HTMLURL        string     `json:"html_url"`
	User           user       `json:"user"`
	CreatedAt      time.Time  `json:"created_at"`
	ClosedAt       *time.Time `json:"closed_at"`
	Labels         []label    `json:"labels"`

	// MaintainerCanModify is a fork PR's "Allow edits by maintainers".
	MaintainerCanModify bool `json:"maintainer_can_modify"`
	// MergeCommitSHA is, once the PR has merged, the commit it left on its
	// base. Before that GitHub fills it with a test merge of its own.
	MergeCommitSHA string `json:"merge_commit_sha"`
}

func (c *Client) pr(r forge.RepoRef, p pull) forge.PR {
	out := forge.PR{
		Repo: r, Number: p.Number, Title: p.Title, Body: p.Body, State: p.State,
		Merged:     p.Merged || p.MergedAt != nil,
		HeadBranch: p.Head.Ref, HeadSHA: p.Head.SHA, BaseBranch: p.Base.Ref, Fork: p.Head.fork(r),
		HeadRepo: p.Head.repo(r), MaintainerCanModify: p.MaintainerCanModify,
		// Not known to conflict until GitHub says so.
		Mergeable: p.Mergeable == nil || *p.Mergeable,
		URL:       p.HTMLURL, Author: p.User.Login, CreatedAt: p.CreatedAt,
	}
	if out.Merged {
		out.MergeSHA = p.MergeCommitSHA
	}
	if p.ClosedAt != nil {
		out.ClosedAt = *p.ClosedAt
	}
	for _, l := range p.Labels {
		out.Labels = append(out.Labels, l.Name)
	}
	if p.Mergeable == nil {
		out.MergeableUnknown = true
		return out
	}
	// mergeable_state isn't in GitHub's reference docs beyond its type; the
	// values are those the GraphQL MergeStateStatus enum documents. clean,
	// has_hooks, unstable (failing checks that aren't required) and draft
	// are mergeable; CombinedChecks reports the checks themselves.
	switch p.MergeableState {
	case "dirty":
		out.Mergeable = false
	case "behind":
		out.Behind = true
	case "blocked":
		out.Blocked = true
	}
	return out
}

// noCommonAncestor reports whether a compare's 404 is GitHub saying the two
// commits share no history ("No common ancestor between main and x."),
// rather than that one of them is missing.
func noCommonAncestor(body string) bool {
	var b struct {
		Message string `json:"message"`
	}
	return json.Unmarshal([]byte(body), &b) == nil && strings.Contains(strings.ToLower(b.Message), "no common ancestor")
}

// mergeableTries is how many times GetPR reads an open PR whose
// mergeability GitHub is still computing, a second longer apart each time,
// before reporting it unknown.
const mergeableTries = 4

func (c *Client) GetPR(ctx context.Context, r forge.RepoRef, number int) (forge.PR, error) {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return forge.PR{}, err
	}
	var p pull
	for try := 1; ; try++ {
		if _, err := c.do(ctx, cr, "GET", fmt.Sprintf("%s/pulls/%d", repoPath(r), number), nil, &p); err != nil {
			return forge.PR{}, err
		}
		// A null mergeable means GitHub has started computing it in the
		// background: ask again after a moment (docs: "Get a pull request").
		if p.State != "open" || p.Mergeable != nil || try == mergeableTries {
			break
		}
		if err := c.sleep(ctx, time.Duration(try)*time.Second); err != nil {
			return forge.PR{}, err
		}
	}
	out := c.pr(r, p)
	if p.State == "open" {
		// Behind: the base branch has commits the head doesn't. GitHub
		// itself only says "behind" when the base requires branches to be
		// up to date. A failed compare leaves it as it is, as Gitea's
		// adapter does, except the 404 GitHub answers for two commits that
		// share none: the base was force-pushed to a history of its own.
		var cmp struct {
			BehindBy int `json:"behind_by"`
		}
		basehead := escapePath(p.Base.Ref) + "..." + url.PathEscape(p.Head.SHA)
		_, err := c.do(ctx, cr, "GET", repoPath(r)+"/compare/"+basehead+"?per_page=1", nil, &cmp)
		var nf *notFoundError
		switch {
		case err == nil:
			out.Behind = out.Behind || cmp.BehindBy > 0
		case errors.As(err, &nf) && noCommonAncestor(nf.Body):
			out.Behind, out.Unrelated = true, true
		}
	}
	return out, nil
}

func (c *Client) ListOpenPRs(ctx context.Context, r forge.RepoRef) ([]forge.PR, error) {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return nil, err
	}
	ps, err := list[pull](ctx, c, cr, repoPath(r)+"/pulls?state=open")
	if err != nil {
		return nil, err
	}
	out := make([]forge.PR, 0, len(ps))
	for _, p := range ps {
		out = append(out, c.pr(r, p))
	}
	return out, nil
}

// OpenPR opens head, a branch on r itself, against base. GitHub hasn't
// worked out mergeability yet, so the PR comes back with it unknown.
func (c *Client) OpenPR(ctx context.Context, r forge.RepoRef, head, base, title, body string) (forge.PR, error) {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return forge.PR{}, err
	}
	var p pull
	in := map[string]string{"title": title, "head": head, "base": base, "body": body}
	if _, err := c.do(ctx, cr, "POST", repoPath(r)+"/pulls", in, &p); err != nil {
		return forge.PR{}, err
	}
	return c.pr(r, p), nil
}

func (c *Client) UpdatePR(ctx context.Context, r forge.RepoRef, number int, title, body string) error {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, cr, "PATCH", fmt.Sprintf("%s/pulls/%d", repoPath(r), number), map[string]string{"title": title, "body": body}, nil)
	return err
}

type commitStatus struct {
	Context     string `json:"context"`
	State       string `json:"state"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
}

type checkRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	DetailsURL string `json:"details_url"`
	Output     struct {
		Title string `json:"title"`
	} `json:"output"`
}

// CombinedChecks combines GitHub's two systems: commit statuses (external
// CI) and check runs (GitHub Actions and other Apps), every suite of them.
// Docs: "REST API endpoints for commit statuses" (combined status, the
// latest per context) and "REST API endpoints for check runs" (list check
// runs for a Git reference).
func (c *Client) CombinedChecks(ctx context.Context, r forge.RepoRef, sha string) (forge.CheckState, []forge.CheckDetail, error) {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return "", nil, err
	}
	commit := repoPath(r) + "/commits/" + url.PathEscape(sha)
	statuses, err := listIn(ctx, c, cr, commit+"/status", func(w struct {
		Statuses []commitStatus `json:"statuses"`
	}) []commitStatus {
		return w.Statuses
	})
	if err != nil {
		return "", nil, err
	}
	runs, err := listIn(ctx, c, cr, commit+"/check-runs?filter=latest", func(w struct {
		CheckRuns []checkRun `json:"check_runs"`
	}) []checkRun {
		return w.CheckRuns
	})
	if err != nil {
		return "", nil, err
	}

	var details []forge.CheckDetail
	pending, failed := false, false
	add := func(d forge.CheckDetail, outcome string) {
		d.Failed, d.Pending = outcome == "failure", outcome == "pending"
		details = append(details, d)
		pending = pending || outcome == "pending"
		failed = failed || d.Failed
	}
	for _, s := range statuses {
		add(forge.CheckDetail{Context: s.Context, State: s.State, Description: s.Description, URL: s.TargetURL}, statusOutcome(s.State))
	}
	// A re-run of a check is a new run with the same name, and supersedes
	// the old one; the newest has the highest ID.
	latest := map[string]checkRun{}
	var names []string
	for _, run := range runs {
		old, seen := latest[run.Name]
		if !seen {
			names = append(names, run.Name)
		}
		if !seen || run.ID > old.ID {
			latest[run.Name] = run
		}
	}
	for _, n := range names {
		run := latest[n]
		state := run.Status
		if run.Status == "completed" {
			state = run.Conclusion
		}
		// The link is how RerunChecks finds the workflow run, so a details_url
		// that names one wins over html_url, which may be the check run's own
		// page.
		u := run.HTMLURL
		if _, ok := actionsRun(run.DetailsURL, r); ok || u == "" {
			u = run.DetailsURL
		}
		add(forge.CheckDetail{Context: run.Name, State: state, Description: run.Output.Title, URL: u, ID: run.ID,
			Cancelled: run.Status == "completed" && run.Conclusion == "cancelled"}, runOutcome(run.Status, run.Conclusion))
	}
	switch {
	case len(details) == 0:
		return forge.ChecksNone, nil, nil
	case pending:
		// As on Gitea: a failure is reported once every check has finished,
		// so the implementer gets the whole picture in one go.
		return forge.ChecksPending, details, nil
	case failed:
		return forge.ChecksFailure, details, nil
	}
	return forge.ChecksSuccess, details, nil
}

// SetCommitStatus posts a commit status on sha, which takes the App's
// Commit statuses write (docs: "Create a commit status").
func (c *Client) SetCommitStatus(ctx context.Context, r forge.RepoRef, sha string, s forge.CommitStatus) error {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, cr, "POST", repoPath(r)+"/statuses/"+url.PathEscape(sha),
		statusBody(s), nil)
	return err
}

// RerunChecks re-runs the failed and cancelled jobs of each Actions workflow
// run behind checks, as the implementer App, which needs Actions write for
// it (docs: "Re-run failed jobs from a workflow run"). A check run from
// Actions links to its job under the run, which is how its run is found;
// one from any other App, or a commit status, has no run to re-run here.
func (c *Client) RerunChecks(ctx context.Context, r forge.RepoRef, sha string, checks []forge.CheckDetail) error {
	var runs []string
	for _, d := range checks {
		id, ok := actionsRun(d.URL, r)
		if !ok {
			return fmt.Errorf("github: check %q: %w", d.Context, forge.ErrNotRerunnable)
		}
		if !slices.Contains(runs, id) {
			runs = append(runs, id)
		}
	}
	cr, err := c.repoCred(ctx, forge.RoleImplementer, r)
	if err != nil {
		return err
	}
	for _, id := range runs {
		if _, err := c.do(ctx, cr, "POST", repoPath(r)+"/actions/runs/"+id+"/rerun-failed-jobs", nil, nil); err != nil {
			return fmt.Errorf("github: re-running workflow run %s: %w", id, err)
		}
	}
	return nil
}

// CheckReport reads a check run's output and every page of its annotations
// (docs: "Get a check run", "List check run annotations"), from any App. A
// check run of an Actions job has the job's ID, so for one that links to an
// Actions run the end of the job's log is read too ("Download job logs for
// a workflow run", which needs Actions read). A commit status has nothing
// more to read. An error comes back with what was read before it.
func (c *Client) CheckReport(ctx context.Context, r forge.RepoRef, sha string, d forge.CheckDetail, logLines int) (forge.CheckReport, error) {
	var out forge.CheckReport
	if d.ID == 0 {
		return out, nil
	}
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return out, err
	}
	run := repoPath(r) + "/check-runs/" + strconv.FormatInt(d.ID, 10)
	var got struct {
		Output struct {
			Title       string `json:"title"`
			Summary     string `json:"summary"`
			Text        string `json:"text"`
			Annotations int    `json:"annotations_count"`
		} `json:"output"`
	}
	if _, err := c.do(ctx, cr, "GET", run, nil, &got); err != nil {
		return out, fmt.Errorf("github: reading check run %q: %w", d.Context, err)
	}
	out.Title, out.Summary, out.Text = got.Output.Title, got.Output.Summary, got.Output.Text
	if got.Output.Annotations > 0 {
		next := run + "/annotations?per_page=100"
		for next != "" && len(out.Annotations) < forge.MaxAnnotations {
			var page []struct {
				Path      string `json:"path"`
				StartLine int    `json:"start_line"`
				EndLine   int    `json:"end_line"`
				Level     string `json:"annotation_level"`
				Title     string `json:"title"`
				Message   string `json:"message"`
			}
			h, err := c.do(ctx, cr, "GET", next, nil, &page)
			if err != nil {
				return out, fmt.Errorf("github: reading check run %q's annotations: %w", d.Context, err)
			}
			for _, a := range page {
				out.Annotations = append(out.Annotations, forge.Annotation{Path: a.Path, StartLine: a.StartLine, EndLine: a.EndLine, Level: a.Level, Title: a.Title, Message: a.Message})
			}
			next = nextLink(h)
		}
		if len(out.Annotations) > forge.MaxAnnotations || next != "" {
			out.Annotations, out.MoreAnnotations = out.Annotations[:min(len(out.Annotations), forge.MaxAnnotations)], true
		}
	}
	if _, ok := actionsRun(d.URL, r); ok {
		out.HasLog = true
		err := c.download(ctx, cr, repoPath(r)+"/actions/jobs/"+strconv.FormatInt(d.ID, 10)+"/logs", func(body io.Reader) error {
			var err error
			out.Log, out.LogCut, err = forge.TailLines(body, logLines)
			return err
		})
		if err != nil {
			return out, fmt.Errorf("github: reading the log of job %q: %w", d.Context, err)
		}
	}
	return out, nil
}

// actionsRun is the ID of the Actions workflow run whose page, or a job's
// page under it, u is: https://github.com/{owner}/{repo}/actions/runs/{id}.
func actionsRun(u string, r forge.RepoRef) (string, bool) {
	p, err := url.Parse(u)
	if err != nil {
		return "", false
	}
	before, after, ok := strings.Cut(p.Path, "/actions/runs/")
	if !ok || !strings.EqualFold(before, "/"+r.Owner+"/"+r.Name) {
		return "", false
	}
	id, _, _ := strings.Cut(after, "/")
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return "", false
	}
	return id, true
}

// statusOutcome maps a commit status's state: error, failure, pending or
// success.
func statusOutcome(state string) string {
	switch state {
	case "success":
		return "success"
	case "pending":
		return "pending"
	}
	return "failure"
}

// runOutcome maps a check run's status and conclusion. A run that hasn't
// completed (queued, in_progress, waiting, requested, pending) is pending.
// A conclusion that isn't a pass fails, stale (GitHub gave up on a run left
// incomplete for two weeks) and any GitHub adds later included.
func runOutcome(status, conclusion string) string {
	if status != "completed" {
		return "pending"
	}
	switch conclusion {
	case "success", "neutral", "skipped":
		return "success"
	}
	return "failure"
}

// Review submits a review as the reviewer App, on sha rather than whatever
// the head is by the time it lands (docs: "Create a review for a pull
// request").
func (c *Client) Review(ctx context.Context, r forge.RepoRef, number int, sha string, v forge.Verdict, body string) error {
	ev := map[forge.Verdict]string{
		forge.VerdictApprove: "APPROVE", forge.VerdictRequestChanges: "REQUEST_CHANGES", forge.VerdictComment: "COMMENT",
	}[v]
	if ev == "" {
		return fmt.Errorf("github: unknown verdict %q", v)
	}
	cr, err := c.repoCred(ctx, forge.RoleReviewer, r)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, cr, "POST", fmt.Sprintf("%s/pulls/%d/reviews", repoPath(r), number),
		map[string]string{"commit_id": sha, "body": body, "event": ev}, nil)
	return err
}

// Reviews lists the PR's submitted reviews, oldest first. GitHub replaces a
// dismissed review's state with DISMISSED, so what it said is lost; it
// reads as a dismissed comment.
func (c *Client) Reviews(ctx context.Context, r forge.RepoRef, number int) ([]forge.Review, error) {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return nil, err
	}
	rs, err := list[struct {
		ID          int64     `json:"id"`
		User        user      `json:"user"`
		State       string    `json:"state"`
		CommitID    string    `json:"commit_id"`
		Body        string    `json:"body"`
		SubmittedAt time.Time `json:"submitted_at"`
	}](ctx, c, cr, fmt.Sprintf("%s/pulls/%d/reviews", repoPath(r), number))
	if err != nil {
		return nil, err
	}
	var out []forge.Review
	for _, rv := range rs {
		v := forge.VerdictComment
		switch rv.State {
		case "PENDING": // a draft, not submitted
			continue
		case "APPROVED":
			v = forge.VerdictApprove
		case "CHANGES_REQUESTED":
			v = forge.VerdictRequestChanges
		}
		out = append(out, forge.Review{ID: rv.ID, Author: rv.User.Login, Verdict: v, CommitSHA: rv.CommitID, Body: rv.Body,
			SubmittedAt: rv.SubmittedAt, Dismissed: rv.State == "DISMISSED"})
	}
	return out, nil
}

// ReviewComments lists the PR's review comments, every review's in one list
// (docs: "List review comments on a pull request"), oldest first. line is
// null on a comment whose line the PR's head no longer has; original_line is
// still the one it was left on.
func (c *Client) ReviewComments(ctx context.Context, r forge.RepoRef, number int) ([]forge.ReviewComment, error) {
	cr, err := c.repoCred(ctx, c.acting(), r)
	if err != nil {
		return nil, err
	}
	cs, err := list[struct {
		ID           int64     `json:"id"`
		ReviewID     int64     `json:"pull_request_review_id"`
		User         user      `json:"user"`
		Body         string    `json:"body"`
		Path         string    `json:"path"`
		Line         int       `json:"line"`
		OriginalLine int       `json:"original_line"`
		CommitID     string    `json:"commit_id"`
		CreatedAt    time.Time `json:"created_at"`
	}](ctx, c, cr, fmt.Sprintf("%s/pulls/%d/comments?sort=created&direction=asc", repoPath(r), number))
	if err != nil {
		return nil, err
	}
	out := make([]forge.ReviewComment, 0, len(cs))
	for _, rc := range cs {
		line := rc.Line
		if line == 0 {
			line = rc.OriginalLine
		}
		out = append(out, forge.ReviewComment{ID: rc.ID, ReviewID: rc.ReviewID, Author: rc.User.Login, Body: rc.Body,
			Path: rc.Path, Line: line, CommitSHA: rc.CommitID, CreatedAt: rc.CreatedAt})
	}
	return out, nil
}

// Merge merges as the reviewer App, only while the head is sha (docs:
// "Merge a pull request"). GitHub answers 405 when the PR can't be merged
// (a conflict, or branch protection), and 409 when the head has moved. Its
// message says which rule is unmet ("At least 1 approving review is
// required by reviewers with write access."), so that is what the error
// carries. Two 405s are only for now: "Base branch was modified. Review and
// try the merge again." when another merge landed on the base in between,
// and any 405 while GitHub is still computing whether the PR can merge,
// which only a read of the PR tells apart.
func (c *Client) Merge(ctx context.Context, r forge.RepoRef, number int, sha, method string) error {
	switch method {
	case "":
		method = "squash"
	case "merge", "squash", "rebase":
	default:
		return fmt.Errorf("github: merge method %q: want merge, squash or rebase", method)
	}
	cr, err := c.repoCred(ctx, forge.RoleReviewer, r)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, cr, "PUT", fmt.Sprintf("%s/pulls/%d/merge", repoPath(r), number),
		map[string]string{"sha": sha, "merge_method": method}, nil)
	var ae *apiError
	if errors.As(err, &ae) {
		switch ae.Status {
		case http.StatusMethodNotAllowed:
			msg := strings.ToLower(ae.message())
			if strings.Contains(msg, "base branch was modified") || strings.Contains(msg, "try the merge again") {
				return fmt.Errorf("%w: %s", forge.ErrMergePending, ae.message())
			}
			// One read, not GetPR's wait: the engine's backoff is the wait.
			var p pull
			if _, gerr := c.do(ctx, cr, "GET", fmt.Sprintf("%s/pulls/%d", repoPath(r), number), nil, &p); gerr == nil && p.State == "open" && p.Mergeable == nil {
				return fmt.Errorf("%w: %s (GitHub is still checking whether it can merge)", forge.ErrMergePending, ae.message())
			}
			return fmt.Errorf("%w: %s", forge.ErrNotMergeable, ae.message())
		case http.StatusConflict:
			return fmt.Errorf("%w: %s", forge.ErrHeadMoved, ae.message())
		}
	}
	return err
}

func (c *Client) SetPRBase(ctx context.Context, r forge.RepoRef, number int, base string) error {
	cr, err := c.repoCred(ctx, forge.RoleImplementer, r)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, cr, "PATCH", fmt.Sprintf("%s/pulls/%d", repoPath(r), number), map[string]string{"base": base}, nil)
	return err
}

// statusBody is a commit status as the API takes it, with no empty link.
func statusBody(s forge.CommitStatus) map[string]string {
	b := map[string]string{"state": s.State, "context": s.Context, "description": s.Description}
	if s.URL != "" {
		b["target_url"] = s.URL
	}
	return b
}

package github

import (
	"fmt"
	"slices"
	"testing"

	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/forgetest"
)

func (f *fakeGitHub) fixture(t *testing.T) *forgetest.Fixture {
	repo := func() *fakeRepo { return f.repos["acme/app"] }
	locked := func(fn func()) {
		f.mu.Lock()
		defer f.mu.Unlock()
		fn()
	}
	return &forgetest.Fixture{
		P:        f.client(),
		Repo:     forge.RepoRef{Provider: "github", Owner: "acme", Name: "app"},
		NoAccess: forge.RepoRef{Provider: "github", Owner: "acme", Name: "private"},
		Issue: func(author, title, body string, labels ...string) (n int) {
			locked(func() {
				r := repo()
				r.next++
				n = r.next
				now := f.now()
				is := &fakeIssue{number: n, title: title, body: body, author: author, state: "open", created: now, updated: now}
				r.issues[n] = is
				f.setLabels(r, is, author, labels)
			})
			return n
		},
		Comment: func(n int, author, body string) {
			locked(func() {
				is := repo().issues[n]
				is.comments = append(is.comments, fakeComment{id: int64(1000*n + len(is.comments) + 1), author: author, body: body, created: f.now()})
			})
		},
		PR: func(p forgetest.PRSeed) (n int) {
			locked(func() {
				r := repo()
				r.next++
				n = r.next
				is := &fakeIssue{number: n, title: p.Title, body: p.Body, author: p.Author, state: "open", created: f.now(), updated: f.now(),
					isPR: true, headRef: p.Head, headSHA: p.HeadSHA, baseRef: p.Base, mergeBase: p.MergeBase, unrelated: p.Unrelated, conflicted: p.Conflicted, fork: p.Fork, maintainerEdit: p.MaintainerCanModify}
				if p.Checking {
					is.unknownFor = 100
				}
				r.issues[n] = is
			})
			return n
		},
		Edit: func(n int, editor, title, body string) {
			locked(func() { f.edit(repo().issues[n], editor, title, body) })
		},
		Close: func(n int) {
			locked(func() {
				is := repo().issues[n]
				is.state, is.updated = "closed", f.now()
			})
		},
		// A push to an open PR's head branch moves the PR's head.
		Branch: func(name, sha string) {
			locked(func() {
				r := repo()
				r.branches[name], r.commitAt[sha] = sha, f.now()
				for _, is := range r.issues {
					if is.isPR && is.headRef == name && is.state == "open" && !is.fork {
						is.headSHA = sha
					}
				}
			})
		},
		File: func(ref, path string, data []byte) { locked(func() { repo().files[ref+":"+path] = data }) },
		// Every other line comment has moved out of the head's diff, so
		// GitHub reports its line as null and only its original_line.
		LineReview: func(n int, author, sha string, v forge.Verdict, body string, lines ...forge.ReviewComment) {
			locked(func() {
				r := repo()
				is := r.issues[n]
				state := map[forge.Verdict]string{forge.VerdictApprove: "APPROVED", forge.VerdictRequestChanges: "CHANGES_REQUESTED", forge.VerdictComment: "COMMENTED"}[v]
				r.nextID++
				rv := fakeReview{ID: r.nextID, User: fakeUser{author}, State: state, CommitID: sha, Body: body, SubmittedAt: f.stamp()}
				is.reviews = append(is.reviews, rv)
				for i, c := range lines {
					r.nextID++
					l := fakeLine{ID: r.nextID, ReviewID: rv.ID, User: rv.User, Body: c.Body, Path: c.Path, OrigLine: c.Line, CommitID: sha, Created: f.stamp()}
					if i%2 == 0 {
						l.Line = &c.Line
					}
					is.lines = append(is.lines, l)
				}
			})
		},
		Status: func(sha, ctx, state string) {
			locked(func() {
				r := repo()
				r.statuses[sha] = slices.DeleteFunc(r.statuses[sha], func(s fakeStatus) bool { return s.Context == ctx })
				r.statuses[sha] = append(r.statuses[sha], fakeStatus{Context: ctx, State: state})
			})
		},
		// A failed job's check run has the job's ID and links to it under
		// its workflow run; reading its log takes Actions read.
		FailedJob: func(sha, ctx, log string) {
			locked(func() {
				r := repo()
				r.nextID++
				id := r.nextID
				failure := "failure"
				r.runs[sha] = append(r.runs[sha], fakeRun{ID: id, Name: ctx, Status: "completed", Conclusion: &failure,
					HTMLURL: fmt.Sprintf("https://github.example/acme/app/actions/runs/%d/job/%d", 900+id, id)})
				r.outputs[id], r.logs[id] = forge.CheckReport{}, log
				for _, in := range f.installs {
					if in.perms["actions"] == "" {
						in.perms["actions"] = "read"
					}
				}
			})
		},
		CheckRun: func(sha, ctx string, out forge.CheckReport) {
			locked(func() {
				r := repo()
				r.nextID++
				failure := "failure"
				r.runs[sha] = append(r.runs[sha], fakeRun{ID: r.nextID, Name: ctx, Status: "completed", Conclusion: &failure,
					HTMLURL: fmt.Sprintf("https://github.example/acme/app/runs/%d", r.nextID), DetailsURL: "https://sast.example/scan/1"})
				r.outputs[r.nextID] = out
			})
		},
		Grant: func(login string, p forge.Permission) { f.grant(login, "acme/app", role(p)) },
		SignIn: func(login string, p forge.Permission) string {
			f.grant(login, "acme/app", role(p))
			return f.signIn(login)
		},
		RequireChecks: func(branch string, checks ...string) {
			locked(func() {
				repo().classic[branch] = map[string]any{"required_status_checks": map[string]any{"enforcement_level": "everyone", "contexts": checks, "checks": []any{}}}
			})
		},
		Tag: func(name, sha string) {
			locked(func() { repo().tags = append([][2]string{{name, sha}}, repo().tags...) })
		},
		Descends: func(sha string, parents ...string) {
			locked(func() {
				r := repo()
				r.history[sha] = append(r.history[sha], parents...)
				// A parent is a commit the forge knows, history or not.
				for _, p := range parents {
					if _, ok := r.history[p]; !ok {
						r.history[p] = nil
					}
				}
			})
		},
		Calls: func() (n int) {
			locked(func() { n = f.calls })
			return n
		},
	}
}

func TestConformance(t *testing.T) {
	forgetest.Run(t, func(t *testing.T) *forgetest.Fixture { return newFakeGitHub(t, "").fixture(t) })
}

// The same contract on GitHub Enterprise Server, whose API is under /api/v3
// and whose web pages are on the host itself.
func TestConformanceGHES(t *testing.T) {
	forgetest.Run(t, func(t *testing.T) *forgetest.Fixture { return newFakeGitHub(t, "/api/v3").fixture(t) })
}

// role is GitHub's name for p.
func role(p forge.Permission) string {
	return map[forge.Permission]string{forge.PermRead: "read", forge.PermWrite: "write", forge.PermMaintain: "maintain", forge.PermAdmin: "admin"}[p]
}

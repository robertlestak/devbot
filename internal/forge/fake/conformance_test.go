package fake

import (
	"testing"

	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/forgetest"
)

// fixture wires the generic fake, named as p names things, to the
// conformance suite, so the forge every core and harness test runs against
// keeps to the contract the real adapters do.
func fixture(t *testing.T, p Profile) *forgetest.Fixture {
	// The suite starts from a fresh repo, which has no branch but main.
	p.Branches = nil
	f := NewAs(p)
	provider, impl, rev := p.Provider, p.Impl, p.Rev
	repo := forge.RepoRef{Provider: provider, Owner: "acme", Name: "app"}
	private := forge.RepoRef{Provider: provider, Owner: "acme", Name: "private"}
	f.AddRepo(repo, "")
	f.CloneURL[repo.String()] = "https://fake/acme/app.git"
	for _, u := range []string{impl, rev, "alice"} {
		f.Grant(private, u, forge.PermNone)
	}
	f.Push(repo, "main", "ba5e")
	return &forgetest.Fixture{
		P:        f,
		Repo:     repo,
		NoAccess: private,
		Issue: func(author, title, body string, labels ...string) int {
			return f.NewIssue(repo, author, title, body, labels...).Number
		},
		Comment: func(n int, author, body string) { f.AddComment(forge.ItemRef{Repo: repo, Number: n}, author, body) },
		PR: func(p forgetest.PRSeed) int {
			f.mu.Lock()
			defer f.mu.Unlock()
			n := f.nextNum()
			base := f.Branches[repo.String()+":"+p.Base]
			f.PRs[prKey(repo, n)] = &forge.PR{Repo: repo, Number: n, Title: p.Title, Body: p.Body, State: "open",
				HeadBranch: p.Head, HeadSHA: p.HeadSHA, BaseBranch: p.Base, Fork: p.Fork, Mergeable: !p.Conflicted, MergeableUnknown: p.Checking,
				Behind: base != nil && (p.Unrelated || base.SHA != p.MergeBase), Unrelated: base != nil && p.Unrelated, URL: f.PRURL(repo, n), Author: p.Author, CreatedAt: f.now(),
				HeadRepo: repo, MaintainerCanModify: p.MaintainerCanModify}
			if p.Fork {
				f.PRs[prKey(repo, n)].HeadRepo = forkOf(repo, p.Author)
			}
			return n
		},
		Edit: func(n int, editor, title, body string) {
			f.EditIssue(forge.ItemRef{Repo: repo, Number: n}, editor, title, body)
		},
		Close: func(n int) {
			if _, ok := f.PRs[prKey(repo, n)]; ok {
				f.ClosePR(repo, n)
				return
			}
			f.CloseItem(t.Context(), forge.ItemRef{Repo: repo, Number: n})
		},
		Branch: func(name, sha string) { f.Push(repo, name, sha) },
		File: func(ref, path string, data []byte) {
			k := repo.String() + "@" + ref + ":" + path
			if ref == "main" {
				k = repo.String() + ":" + path
			}
			f.Files[k] = data
		},
		LineReview: func(n int, author, sha string, v forge.Verdict, body string, lines ...forge.ReviewComment) {
			f.AddLineReview(repo, n, author, sha, v, body, lines...)
		},
		Status:    func(sha, ctx, state string) { f.SetStatus(sha, ctx, state) },
		FailedJob: func(sha, ctx, log string) { f.SetFailedJob(sha, ctx, log) },
		CheckRun:  func(sha, ctx string, r forge.CheckReport) { f.SetCheckRun(sha, ctx, r) },
		Grant:     func(user string, p forge.Permission) { f.Grant(repo, user, p) },
		RequireChecks: func(branch string, checks ...string) {
			f.Protections[repo.String()+":"+branch] = forge.Protection{RequiredChecks: checks,
				Rules: []string{"required checks"}, Exempt: "let " + rev + " bypass the branch's rules"}
		},
		SignIn: func(login string, p forge.Permission) string {
			f.Grant(repo, login, p)
			return f.TokenFor(login).Access
		},
		Tag:      func(name, sha string) { f.AddTag(repo, name, sha) },
		Descends: f.Descends,
		// The fake answers from memory: nothing it does is a call.
		Calls: func() int { return 0 },
	}
}

func TestConformance(t *testing.T) {
	for _, p := range Profiles {
		t.Run(p.Name, func(t *testing.T) {
			forgetest.Run(t, func(t *testing.T) *forgetest.Fixture { return fixture(t, p) })
		})
	}
}

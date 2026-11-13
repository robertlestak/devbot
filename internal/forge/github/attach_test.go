package github

import (
	"bytes"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/forgetest"
)

func seedPR(f *fakeGitHub) int {
	return f.fixture(f.t).PR(forgetest.PRSeed{Author: "alice", Title: "t", Head: "h", HeadSHA: "hh", Base: "main", MergeBase: "ba5e"})
}

func TestAttachCreatesTheOrphanBranchOnce(t *testing.T) {
	for _, prefix := range []string{"", "/api/v3"} {
		f := newFakeGitHub(t, prefix)
		c := f.client()
		n := seedPR(f)
		png := []byte("\x89PNG\r\n\x1a\nboard")
		u, err := c.AttachToPR(ctx, repo, n, "board.png", png)
		if err != nil {
			t.Fatal(err)
		}
		want := f.srv.URL + "/acme/app/blob/devbot/attachments/pr-1/" + attachName("board.png", png) + "?raw=true"
		if u != want {
			t.Errorf("%q: url %q, want %q", prefix, u, want)
		}
		if got := f.filesOn("acme/app", attachBranch); !slices.Equal(got, []string{"README.md", "pr-1/" + attachName("board.png", png)}) {
			t.Errorf("files on the branch: %v", got)
		}
		// The README commit has no parent: nothing of main's is on it.
		if n := f.commitsOn("acme/app", attachBranch); n != 2 {
			t.Errorf("want the README commit and the file's, got %d", n)
		}

		// The same file again is the same URL and no new commit; another
		// file is one more commit on the same branch.
		again, err := c.AttachToPR(ctx, repo, n, "board.png", png)
		if err != nil || again != u {
			t.Fatalf("repeat upload: %q, %v", again, err)
		}
		if _, err := c.AttachToPR(ctx, repo, n, "run.log", []byte("ok\n")); err != nil {
			t.Fatal(err)
		}
		if n := f.commitsOn("acme/app", attachBranch); n != 3 {
			t.Errorf("want 3 commits after two files, got %d", n)
		}
		got, err := c.GetAttachment(ctx, repo, n, u, forge.RoleReviewer)
		if err != nil || !bytes.Equal(got, png) {
			t.Fatalf("read back: %q, %v", got, err)
		}
	}
}

func TestAttachNameIsContentAddressedAndSafe(t *testing.T) {
	data := []byte("x")
	for name, want := range map[string]string{
		"board.png":          "board.png",
		"../../etc/passwd":   "passwd",
		`dir\shot.png`:       "shot.png",
		"my shot (1).png":    "my-shot-1-.png",
		".hidden":            "hidden",
		"":                   "file",
		"<img src=x>.png":    "img-src-x-.png",
		"ünïcode screen.jpg": "n-code-screen.jpg",
	} {
		got := attachName(name, data)
		if !attachFile.MatchString(got) || !strings.HasSuffix(got, "-"+want) {
			t.Errorf("%q: %q, want <hash>-%s", name, got, want)
		}
	}
	if attachName("a.png", []byte("1")) == attachName("a.png", []byte("2")) {
		t.Error("different content, same name")
	}
}

func TestGetAttachmentRefusesAnythingElse(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	n := seedPR(f)
	other := seedPR(f)
	data := []byte("secret")
	u, err := c.AttachToPR(ctx, repo, n, "a.txt", data)
	if err != nil {
		t.Fatal(err)
	}
	file := attachName("a.txt", data)
	base := f.srv.URL + "/acme/app/blob/devbot/attachments/"
	f.mu.Lock()
	f.repos["acme/app"].files["main:pr-1/"+file] = data
	f.mu.Unlock()
	calls := f.fixture(t).Calls
	before := calls()
	for _, bad := range []string{
		base + "pr-2/" + file + "?raw=true",                          // another PR's directory
		f.srv.URL + "/acme/app/blob/main/pr-1/" + file + "?raw=true", // another branch
		f.srv.URL + "/acme/private/blob/devbot/attachments/pr-1/" + file,
		strings.Replace(u, "http://", "https://", 1),
		strings.Replace(u, "127.0.0.1", "localhost", 1),
		"https://evil.example/acme/app/blob/devbot/attachments/pr-1/" + file,
		base + "pr-1/sub/" + file,
		base + "pr-1/../pr-2/" + file,
		base + "pr-1/%2e%2e%2fREADME.md",
		base + "pr-1/README.md",
		base + "pr-1/" + file + "?raw=true&ref=main",
		base + "pr-1/" + file + "#x",
		strings.Replace(u, "http://", "http://user@", 1),
		"",
		"::",
	} {
		if _, err := c.GetAttachment(ctx, repo, n, bad, forge.RoleReviewer); err == nil {
			t.Errorf("fetched %q", bad)
		}
	}
	if _, err := c.GetAttachment(ctx, repo, other, u, forge.RoleReviewer); err == nil {
		t.Error("fetched PR 1's file as PR 2's")
	}
	if calls() != before {
		t.Errorf("a refused URL made %d calls to GitHub", calls()-before)
	}
	// Without ?raw=true too, as the blob page.
	if got, err := c.GetAttachment(ctx, repo, n, strings.TrimSuffix(u, "?raw=true"), forge.RoleImplementer); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back: %q, %v", got, err)
	}
}

func TestReleaseAttachmentsDeletesThePRsDirectoryInOneCommit(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	if err := c.ReleaseAttachments(ctx, repo, 1); err != nil {
		t.Fatalf("no branch yet: %v", err)
	}
	n, keep := seedPR(f), seedPR(f)
	for _, name := range []string{"a.png", "b.png"} {
		if _, err := c.AttachToPR(ctx, repo, n, name, []byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.AttachToPR(ctx, repo, keep, "c.png", []byte("c")); err != nil {
		t.Fatal(err)
	}
	commits := f.commitsOn("acme/app", attachBranch)
	if err := c.ReleaseAttachments(ctx, repo, n); err != nil {
		t.Fatal(err)
	}
	if got := f.filesOn("acme/app", attachBranch); !slices.Equal(got, []string{"README.md", "pr-2/" + attachName("c.png", []byte("c"))}) {
		t.Errorf("files left: %v", got)
	}
	if got := f.commitsOn("acme/app", attachBranch); got != commits+1 {
		t.Errorf("want one more commit, got %d more", got-commits)
	}
	// Again: nothing to do, and no commit.
	if err := c.ReleaseAttachments(ctx, repo, n); err != nil || f.commitsOn("acme/app", attachBranch) != commits+1 {
		t.Errorf("second release: %v, %d commits", err, f.commitsOn("acme/app", attachBranch))
	}
}

// An upload that lands between reading the head and moving the branch
// makes the move a non-fast-forward; the release starts again on top of it.
func TestReleaseAttachmentsRetriesWhenTheBranchMoved(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	n := seedPR(f)
	seedPR(f)
	if _, err := c.AttachToPR(ctx, repo, n, "a.png", []byte("a")); err != nil {
		t.Fatal(err)
	}
	raced := false
	f.mu.Lock()
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "PATCH" && !raced {
			raced = true
			f.mu.Lock()
			rp := f.repos["acme/app"]
			head := rp.objects[rp.branches[attachBranch]]
			tree := rp.withBlob(head.tree, "pr-2/late.png", rp.put(&fakeObject{kind: "blob", data: []byte("late")}))
			rp.branches[attachBranch] = rp.put(&fakeObject{kind: "commit", tree: tree, parents: []string{rp.branches[attachBranch]}})
			f.mu.Unlock()
		}
		return false
	}
	f.mu.Unlock()
	if err := c.ReleaseAttachments(ctx, repo, n); err != nil {
		t.Fatal(err)
	}
	if !raced {
		t.Fatal("the release never moved the branch")
	}
	if got := f.filesOn("acme/app", attachBranch); !slices.Equal(got, []string{"README.md", "pr-2/late.png"}) {
		t.Errorf("files left: %v", got)
	}
}

func TestProtectedAttachmentsBranchNamesTheExemption(t *testing.T) {
	check := func(t *testing.T, err error) {
		t.Helper()
		var pe *ProtectedBranchError
		if !errors.As(err, &pe) || !strings.Contains(err.Error(), "bypass for devbot/attachments") {
			t.Fatalf("want a ProtectedBranchError naming the bypass, got %v", err)
		}
	}
	t.Run("creating the branch", func(t *testing.T) {
		f := newFakeGitHub(t, "")
		c := f.client()
		n := seedPR(f)
		f.repos["acme/app"].protected[attachBranch] = true
		_, err := c.AttachToPR(ctx, repo, n, "a.png", []byte("a"))
		check(t, err)
	})
	t.Run("committing to it", func(t *testing.T) {
		f := newFakeGitHub(t, "")
		c := f.client()
		n := seedPR(f)
		if _, err := c.AttachToPR(ctx, repo, n, "a.png", []byte("a")); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		f.repos["acme/app"].protected[attachBranch] = true
		f.mu.Unlock()
		_, err := c.AttachToPR(ctx, repo, n, "b.png", []byte("b"))
		check(t, err)
		check(t, c.ReleaseAttachments(ctx, repo, n))
	})
}

// The attachments branch is under devbot/, where branch cleanup works, but
// holds every open PR's files: the adapter never deletes it.
func TestDeleteBranchRefusesTheAttachmentsBranch(t *testing.T) {
	f := newFakeGitHub(t, "")
	c := f.client()
	if _, err := c.AttachToPR(ctx, repo, seedPR(f), "run.log", []byte("ok\n")); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteBranch(ctx, repo, attachBranch); err == nil {
		t.Fatal("deleted the attachments branch")
	}
	if f.commitsOn("acme/app", attachBranch) == 0 {
		t.Fatal("the attachments branch is gone")
	}
}

// A commit's date never changes, so a second listing doesn't ask again.
func TestListBranchesReadsEachCommitDateOnce(t *testing.T) {
	f := newFakeGitHub(t, "")
	fx := f.fixture(t)
	for _, b := range []string{"devbot/1-a", "devbot/2-b", "devbot/3-c"} {
		fx.Branch(b, "5ha-"+b)
	}
	if _, err := fx.P.ListBranches(ctx, repo, "devbot/"); err != nil {
		t.Fatal(err)
	}
	before := fx.Calls()
	bs, err := fx.P.ListBranches(ctx, repo, "devbot/")
	if err != nil || len(bs) != 3 {
		t.Fatalf("got %v, %v", bs, err)
	}
	if n := fx.Calls() - before; n != 1 {
		t.Fatalf("second listing made %d calls, want only the list", n)
	}
}

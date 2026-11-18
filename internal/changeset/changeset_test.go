package changeset_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.shdw.tech/rob/devbot/internal/changeset"
	"git.shdw.tech/rob/devbot/internal/config"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, rel, data string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo is a repo whose main has a workflow and some code, checked out on
// a work branch from it.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, ".github/workflows/ci.yml", "on: push\n")
	write(t, dir, "main.go", "package main\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-qm", "init")
	git(t, dir, "checkout", "-q", "-b", "work")
	return dir
}

func commit(t *testing.T, dir string) {
	t.Helper()
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "work")
}

func protectedByDefault(t *testing.T, dir string) []string {
	t.Helper()
	cs, err := changeset.Compute(context.Background(), dir, "main", "work")
	if err != nil {
		t.Fatal(err)
	}
	rc, err := config.DefaultRepo(nil)
	if err != nil {
		t.Fatal(err)
	}
	return rc.ProtectedChanges(cs)
}

func TestDefaultsRefuse(t *testing.T) {
	for _, c := range []struct {
		name string
		do   func(t *testing.T, dir string)
		want string
	}{
		{"a workflow", func(t *testing.T, dir string) { write(t, dir, ".gitea/workflows/x.yaml", "on: push\n") }, "`.gitea/workflows/x.yaml`"},
		{"a rename out of a protected path", func(t *testing.T, dir string) { git(t, dir, "mv", ".github/workflows/ci.yml", "ci.yml") }, "`.github/workflows/ci.yml`"},
		{"a new symlink", func(t *testing.T, dir string) { os.Symlink("/etc/passwd", filepath.Join(dir, "docs.txt")) }, "`docs.txt` (new symlink)"},
		{"a file turned into a symlink", func(t *testing.T, dir string) {
			os.Remove(filepath.Join(dir, "main.go"))
			os.Symlink("/etc/passwd", filepath.Join(dir, "main.go"))
		}, "`main.go` (new symlink)"},
		{"a gitlink", func(t *testing.T, dir string) {
			// An empty directory is an unpopulated submodule, not a deletion.
			os.MkdirAll(filepath.Join(dir, "vendor", "lib"), 0o755)
			git(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+git(t, dir, "rev-parse", "HEAD")+",vendor/lib")
		}, "`vendor/lib` (submodule)"},
		{".gitmodules", func(t *testing.T, dir string) { write(t, dir, ".gitmodules", "[submodule \"lib\"]\n") }, "`.gitmodules`"},
		{".gitattributes", func(t *testing.T, dir string) { write(t, dir, ".gitattributes", "* filter=x\n") }, "`.gitattributes`"},
		{"CODEOWNERS", func(t *testing.T, dir string) { write(t, dir, "docs/CODEOWNERS", "* @x\n") }, "`docs/CODEOWNERS`"},
		{"agent config", func(t *testing.T, dir string) { write(t, dir, ".claude/settings.json", "{}\n") }, "`.claude/settings.json`"},
		{"nested agent config", func(t *testing.T, dir string) { write(t, dir, "tools/.claude/settings.json", "{}\n") }, "`tools/.claude/settings.json`"},
		{"CLAUDE.local.md", func(t *testing.T, dir string) { write(t, dir, "CLAUDE.local.md", "x\n") }, "`CLAUDE.local.md`"},
		{"a path that would break out of the comment", func(t *testing.T, dir string) { write(t, dir, ".gitea/`<!-- x", "") }, "`\".gitea/\\x60\\x3c!-- x\"`"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := newRepo(t)
			c.do(t, dir)
			commit(t, dir)
			hit := protectedByDefault(t, dir)
			if !strings.Contains(strings.Join(hit, "\n"), c.want) {
				t.Fatalf("got %q, want %s among them", hit, c.want)
			}
		})
	}
}

// A symlink the base already has is no safer pointed somewhere new.
func TestRetargetedSymlinkRefused(t *testing.T) {
	dir := newRepo(t)
	git(t, dir, "checkout", "-q", "main")
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	os.Symlink("v2", filepath.Join(dir, "docs", "latest"))
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "link")
	git(t, dir, "checkout", "-q", "-B", "work")
	os.Remove(filepath.Join(dir, "docs", "latest"))
	os.Symlink("/proc/self/environ", filepath.Join(dir, "docs", "latest"))
	commit(t, dir)
	hit := protectedByDefault(t, dir)
	if want := []string{"`docs/latest` (symlink changed)"}; strings.Join(hit, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q, want %q", hit, want)
	}
	// Removing it is fine.
	git(t, dir, "rm", "-q", "docs/latest")
	commit(t, dir)
	if hit := protectedByDefault(t, dir); len(hit) > 0 {
		t.Fatalf("got %q, want nothing protected", hit)
	}
}

func TestOrdinaryChangesPass(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "main.go", "package main\n\nfunc main() {}\n")
	write(t, dir, "docs/a.md", "a\n")
	commit(t, dir)
	// The base moving on with a workflow change of its own isn't the
	// branch's change, merged in or not.
	git(t, dir, "checkout", "-q", "main")
	write(t, dir, ".github/workflows/other.yml", "on: push\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "ci")
	git(t, dir, "checkout", "-q", "work")
	git(t, dir, "merge", "-q", "--no-edit", "main")
	if hit := protectedByDefault(t, dir); len(hit) > 0 {
		t.Fatalf("got %q, want nothing protected", hit)
	}
}

// A forge's file list stops at a few thousand files; git's doesn't.
func TestLargeChangeSetIsComplete(t *testing.T) {
	dir := newRepo(t)
	for i := range 3001 {
		write(t, dir, fmt.Sprintf("gen/f%04d.txt", i), "x\n")
	}
	// Sorts after everything else.
	write(t, dir, "zz/.gitattributes", "* filter=x\n")
	commit(t, dir)
	cs, err := changeset.Compute(context.Background(), dir, "main", "work")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3002 {
		t.Fatalf("got %d changes, want 3002", len(cs))
	}
	if hit := protectedByDefault(t, dir); len(hit) != 1 || hit[0] != "`zz/.gitattributes`" {
		t.Fatalf("got %q", hit)
	}
}

func TestUnreadableFailsClosed(t *testing.T) {
	dir := newRepo(t)
	if _, err := changeset.Compute(context.Background(), dir, "main", "no-such-branch"); err == nil {
		t.Fatal("a head that isn't there must be an error")
	}
	for _, raw := range []string{
		"garbage\x00",
		":100644 100644 aaa bbb M\x00",
		":100644 100644 aaa bbb\x00a.go\x00",
		":100644 100644 aaa bbb M\x00\x00",
	} {
		if cs, err := changeset.Parse(raw); err == nil {
			t.Errorf("Parse(%q) = %v, want an error", raw, cs)
		}
	}
	cs, err := changeset.Parse(":100644 100644 aaa bbb R100\x00old.go\x00new.go\x00")
	if err != nil || len(cs) != 2 || cs[0].Path != "old.go" || cs[1].Path != "new.go" {
		t.Fatalf("a rename lists both paths: %v %v", cs, err)
	}
}

func TestFetchJudgesTheHeadItWasAskedAbout(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, ".gitea/workflows/x.yaml", "on: push\n")
	commit(t, dir)
	head := git(t, dir, "rev-parse", "HEAD")
	ctx := context.Background()
	cs, err := changeset.Fetch(ctx, t.TempDir(), "file://"+dir, "", "main", "refs/heads/work", head)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Path != ".gitea/workflows/x.yaml" || cs[0].Status != "A" {
		t.Fatalf("got %+v", cs)
	}
	if _, err := changeset.Fetch(ctx, t.TempDir(), "file://"+dir, "", "main", "refs/heads/work", strings.Repeat("0", 40)); err == nil {
		t.Fatal("a branch that has moved on from the head being judged must be an error")
	}
}

// A change to a workflow, which the default approvable_paths let a person
// approve, is held with what it leaves there, its blob; agent config, a
// symlink, a submodule or a protected file off approvable_paths isn't, and
// with approvable_paths: [] nothing is.
func TestHeldChanges(t *testing.T) {
	held := func(t *testing.T, dir, yaml string) ([]changeset.Change, []string) {
		t.Helper()
		cs, err := changeset.Compute(context.Background(), dir, "main", "work")
		if err != nil {
			t.Fatal(err)
		}
		rc, err := config.ParseRepo([]byte(yaml))
		if err != nil {
			t.Fatal(err)
		}
		return rc.HeldChanges(cs)
	}
	dir := newRepo(t)
	write(t, dir, ".github/workflows/ci.yml", "on: [push]\n")
	write(t, dir, ".gitea/workflows/new.yaml", "on: push\n")
	write(t, dir, "main.go", "package main\n\nfunc main() {}\n")
	commit(t, dir)
	h, hard := held(t, dir, "version: 1\n")
	if len(hard) > 0 || len(h) != 2 || h[0].Path != ".gitea/workflows/new.yaml" || h[1].Path != ".github/workflows/ci.yml" {
		t.Fatalf("want both workflows held, got %+v, %q", h, hard)
	}
	if blob := git(t, dir, "rev-parse", "work:.github/workflows/ci.yml"); h[1].NewBlob != blob || h[1].OldBlob != git(t, dir, "rev-parse", "main:.github/workflows/ci.yml") {
		t.Fatalf("a change carries the blobs on each side: %+v", h[1])
	}
	if h, hard := held(t, dir, "version: 1\napprovable_paths: []\n"); len(h) > 0 || len(hard) != 2 {
		t.Fatalf("approvable_paths: [] holds nothing: %+v, %q", h, hard)
	}

	for _, c := range []struct {
		name string
		do   func(t *testing.T, dir string)
		want string
	}{
		{"agent config", func(t *testing.T, dir string) { write(t, dir, "AGENTS.md", "be brief\n") }, "`AGENTS.md`"},
		{"agent config under an approvable path", func(t *testing.T, dir string) { write(t, dir, ".github/.claude/settings.json", "{}\n") }, "`.github/.claude/settings.json`"},
		{"a symlink there", func(t *testing.T, dir string) {
			os.Symlink("../../main.go", filepath.Join(dir, ".github", "workflows", "link.yml"))
		}, "`.github/workflows/link.yml`"},
		{"a protected file off approvable_paths", func(t *testing.T, dir string) { write(t, dir, "CODEOWNERS", "* @x\n") }, "`CODEOWNERS`"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := newRepo(t)
			write(t, dir, ".github/workflows/ci.yml", "on: [push]\n")
			c.do(t, dir)
			commit(t, dir)
			h, hard := held(t, dir, "version: 1\n")
			if len(h) != 1 || len(hard) != 1 || hard[0] != c.want {
				t.Fatalf("want the workflow held and %s hard, got %+v, %q", c.want, h, hard)
			}
		})
	}
}

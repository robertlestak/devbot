package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.shdw.tech/rob/devbot/internal/config"
)

// Each kind of change git can report gets its own index entry and, unless
// it is generated, a whole patch: a type change, which git prints as two
// patches, and a rename are each one file.
func TestDiffForReviewSplitsEveryKindOfChange(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write := func(name, content string) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
	}
	write(".gitattributes", "gen/** linguist-generated\n")
	write("link", "was a file\n")
	write("old.go", strings.Repeat("package moved\n", 20))
	write("gone.txt", "bye\n")
	write("logo.png", "\x89PNG\x00\x01")
	write("gen/out.js", "old\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "base")
	git(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	git(t, dir, "checkout", "-qb", "pr")
	os.Remove(filepath.Join(dir, "link"))
	os.Symlink("old.go", filepath.Join(dir, "link"))
	git(t, dir, "mv", "old.go", "new.go")
	write("new.go", strings.Repeat("package moved\n", 20)+"func Added() {}\n")
	os.Remove(filepath.Join(dir, "gone.txt"))
	write("logo.png", "\x89PNG\x00\x02")
	write("gen/out.js", "new\n")
	write("vendor/lib.js", "lib\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "pr")

	w := &workspace{priv: dir}
	rc := &config.Repo{GeneratedPaths: []string{"vendor/**"}, ReviewBudgetKiB: 1}
	d, err := w.diffForReview(context.Background(), rc, "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		status    byte
		oldPath   string
		generated string
		patch     string
	}
	wants := map[string]want{
		"gen/out.js":    {'M', "", "linguist-generated", ""},
		"gone.txt":      {'D', "", "", "-bye"},
		"link":          {'T', "", "", "+old.go"},
		"logo.png":      {'M', "", "binary", ""},
		"new.go":        {'R', "old.go", "", "+func Added() {}"},
		"vendor/lib.js": {'A', "", "generated_paths", ""},
	}
	if len(d.files) != len(wants) {
		t.Fatalf("want %d files, got %d", len(wants), len(d.files))
	}
	for _, f := range d.files {
		w, ok := wants[f.path]
		if !ok || f.status != w.status || f.oldPath != w.oldPath || f.generated != w.generated || f.bytes == 0 || f.size == 0 {
			t.Errorf("%s: got %c %q %q %d bytes, size %d", f.path, f.status, f.oldPath, f.generated, f.bytes, f.size)
		}
		if w.generated != "" && f.patch != nil {
			t.Errorf("%s: a generated file's patch was kept", f.path)
		}
		if w.generated == "" && !strings.Contains(string(f.patch), w.patch) {
			t.Errorf("%s: no %q in its patch:\n%s", f.path, w.patch, f.patch)
		}
	}
	// The type change's patch is both halves: the file's removal and the
	// link's addition.
	for _, f := range d.files {
		if f.path == "link" && (!strings.Contains(string(f.patch), "-was a file") || strings.Count(string(f.patch), "diff --git ") != 2) {
			t.Errorf("link's patch:\n%s", f.patch)
		}
	}
	if d.overBudget() {
		t.Fatalf("%d bytes of source is over a 1 KiB budget", d.source)
	}

	rc.ReviewBudgetKiB = 0
	d, err = w.diffForReview(context.Background(), rc, "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if !d.overBudget() {
		t.Fatalf("%d bytes of source fits a budget of none", d.source)
	}
}

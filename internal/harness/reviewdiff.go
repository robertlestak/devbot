package harness

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"git.shdw.tech/rob/devbot/internal/config"
)

// Where the reviewer finds the PR's changes: an index of every changed
// file, and a patch for each one that isn't generated.
const (
	reviewIndex   = ".devbot/review/index.md"
	reviewPatches = ".devbot/review/patches"
)

// changedFile is one entry of a PR's diff against its base.
type changedFile struct {
	status           byte // A, C, D, M, R or T, as git diff --raw gives it
	path, oldPath    string
	oldBlob, newBlob string
	added, deleted   int    // -1 for a binary file
	bytes            int    // of its patch
	size             int    // of the blob at the head, or at the base for a deletion
	generated        string // why it counts as generated; "" for source
	patch            []byte // nil for a generated file
	patchPath        string
}

// reviewDiff is a PR's changes, laid out for the reviewer.
type reviewDiff struct {
	files []*changedFile
	// source is the bytes of patch content of the files that aren't
	// generated, and budget what the repo lets a reviewer be given.
	source, budget int
}

func (d *reviewDiff) overBudget() bool { return d.source > d.budget }

func (d *reviewDiff) generatedCount() int {
	n := 0
	for _, f := range d.files {
		if f.generated != "" {
			n++
		}
	}
	return n
}

// diffForReview reads the PR's changes against its merge base with base.
// Whether a file is generated comes from the base's .gitattributes and the
// repo's generated_paths, never from anything at the head: the implementer
// could otherwise mark its own changes generated and so out of the
// reviewer's sight. --attr-source keeps the head's .gitattributes out of the
// diff itself too, where -diff or binary would turn source into "Binary
// files differ".
func (w *workspace) diffForReview(ctx context.Context, rc *config.Repo, base string) (*reviewDiff, error) {
	attrs := "--attr-source=" + base
	rng := base + "...HEAD"
	opts := []string{"--no-color", "--no-ext-diff", "--no-textconv", "-M"}
	raw, err := w.git(ctx, false, append(append([]string{attrs, "diff", "--raw", "-z", "--no-abbrev"}, opts...), rng)...)
	if err != nil {
		return nil, err
	}
	files, err := parseRaw(raw)
	if err != nil {
		return nil, err
	}
	num, err := w.git(ctx, false, append(append([]string{attrs, "diff", "--numstat", "-z"}, opts...), rng)...)
	if err != nil {
		return nil, err
	}
	if err := parseNumstat(num, files); err != nil {
		return nil, err
	}
	if err := w.markGenerated(ctx, rc, attrs, files); err != nil {
		return nil, err
	}
	if err := w.blobSizes(ctx, files); err != nil {
		return nil, err
	}
	d := &reviewDiff{files: files, budget: rc.ReviewBudgetKiB << 10}
	if err := w.readPatches(ctx, append(append([]string{attrs, "diff"}, opts...), rng), d); err != nil {
		return nil, err
	}
	return d, nil
}

// parseRaw reads git diff --raw -z: ":oldmode newmode old new STATUS\0path\0",
// with a second path after a rename's or copy's.
func parseRaw(s string) ([]*changedFile, error) {
	var files []*changedFile
	parts := strings.Split(strings.TrimSuffix(s, "\x00"), "\x00")
	for i := 0; i < len(parts); i++ {
		if parts[i] == "" {
			continue
		}
		meta := strings.Fields(strings.TrimPrefix(parts[i], ":"))
		if len(meta) != 5 || meta[4] == "" || i+1 >= len(parts) {
			return nil, fmt.Errorf("unexpected git diff --raw output %q", parts[i])
		}
		f := &changedFile{status: meta[4][0], oldBlob: meta[2], newBlob: meta[3], path: parts[i+1]}
		i++
		if f.status == 'R' || f.status == 'C' {
			if i+1 >= len(parts) {
				return nil, fmt.Errorf("unexpected git diff --raw output for %q", f.path)
			}
			f.oldPath, f.path = f.path, parts[i+1]
			i++
		}
		files = append(files, f)
	}
	return files, nil
}

// parseNumstat reads git diff --numstat -z into files, which are in the same
// order: "added\tdeleted\tpath\0", or "added\tdeleted\t\0old\0new\0".
func parseNumstat(s string, files []*changedFile) error {
	parts := strings.Split(strings.TrimSuffix(s, "\x00"), "\x00")
	n := 0
	for i := 0; i < len(parts); i++ {
		if parts[i] == "" && n == len(files) {
			continue
		}
		cols := strings.SplitN(parts[i], "\t", 3)
		if len(cols) != 3 || n >= len(files) {
			return fmt.Errorf("unexpected git diff --numstat output %q", parts[i])
		}
		path := cols[2]
		if path == "" {
			if i+2 >= len(parts) {
				return fmt.Errorf("unexpected git diff --numstat output %q", parts[i])
			}
			path = parts[i+2]
			i += 2
		}
		f := files[n]
		if path != f.path {
			return fmt.Errorf("git diff --numstat and --raw disagree: %q, %q", path, f.path)
		}
		f.added, f.deleted = -1, -1
		if cols[0] != "-" {
			f.added, _ = strconv.Atoi(cols[0])
			f.deleted, _ = strconv.Atoi(cols[1])
		}
		n++
	}
	if n != len(files) {
		return fmt.Errorf("git diff --numstat listed %d files, --raw %d", n, len(files))
	}
	return nil
}

// markGenerated says which files are generated and why. A rename or copy is
// generated only when both its paths are.
func (w *workspace) markGenerated(ctx context.Context, rc *config.Repo, attrs string, files []*changedFile) error {
	var in strings.Builder
	for _, f := range files {
		in.WriteString(f.path + "\x00")
		if f.oldPath != "" {
			in.WriteString(f.oldPath + "\x00")
		}
	}
	out, err := runGitIO(ctx, w.priv, "", false, nil, in.String(), attrs, "check-attr", "-z", "--stdin", "linguist-generated")
	if err != nil {
		return err
	}
	linguist := map[string]bool{}
	parts := strings.Split(out, "\x00")
	for i := 0; i+2 < len(parts); i += 3 {
		if v := parts[i+2]; v == "set" || v == "true" {
			linguist[parts[i]] = true
		}
	}
	for _, f := range files {
		paths := []string{f.path}
		if f.oldPath != "" {
			paths = append(paths, f.oldPath)
		}
		all := func(gen func(string) bool) bool {
			for _, p := range paths {
				if !gen(p) {
					return false
				}
			}
			return true
		}
		switch {
		case f.added < 0:
			f.generated = "binary"
		case all(func(p string) bool { return linguist[p] }):
			f.generated = "linguist-generated"
		case all(rc.Generated):
			f.generated = "generated_paths"
		}
	}
	return nil
}

// blobSizes fills in each file's size, from its blob at the head, or at the
// base for a deletion.
func (w *workspace) blobSizes(ctx context.Context, files []*changedFile) error {
	var in strings.Builder
	for _, f := range files {
		in.WriteString(f.blob() + "\n")
	}
	out, err := runGitIO(ctx, w.priv, "", false, nil, in.String(), "cat-file", "--batch-check")
	if err != nil {
		return err
	}
	lines := strings.Split(out, "\n")
	for i, f := range files {
		// "<id> blob <size>", or "<id> missing" for a submodule's commit.
		if i < len(lines) {
			if fs := strings.Fields(lines[i]); len(fs) == 3 && fs[1] == "blob" {
				f.size, _ = strconv.Atoi(fs[2])
			}
		}
	}
	return nil
}

func (f *changedFile) blob() string {
	if f.status == 'D' {
		return f.oldBlob
	}
	return f.newBlob
}

// readPatches streams the whole diff and cuts it into one patch per file,
// in the order --raw listed them. Generated files' patches are counted but
// not kept, and nothing more is kept once the source is over budget: none
// of it goes to a reviewer then.
func (w *workspace) readPatches(ctx context.Context, args []string, d *reviewDiff) error {
	cmd := gitCmd(ctx, w.priv, "", false, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// A type change is two patches, the old file's deletion and the new
	// one's addition, under one --raw entry.
	var owner []*changedFile
	for _, f := range d.files {
		owner = append(owner, f)
		if f.status == 'T' {
			owner = append(owner, f)
		}
	}
	n := -1
	var cur *changedFile
	br := bufio.NewReaderSize(stdout, 64<<10)
	lineStart := true
	var rerr error
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			if lineStart && bytes.HasPrefix(chunk, []byte("diff --git ")) {
				if n++; n >= len(owner) {
					rerr = errors.New("git diff gave more patches than files")
					break
				}
				if cur != nil && cur != owner[n] && cur.generated == "" {
					d.source += cur.bytes
				}
				cur = owner[n]
			}
			if cur == nil {
				rerr = fmt.Errorf("unexpected git diff output %q", firstBytes(string(chunk), 80))
				break
			}
			cur.bytes += len(chunk)
			if cur.generated == "" && d.source+cur.bytes <= d.budget {
				cur.patch = append(cur.patch, chunk...)
			}
			lineStart = chunk[len(chunk)-1] == '\n'
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				rerr = err
			}
			break
		}
	}
	if rerr != nil {
		// Drain, so git isn't left blocked on a full pipe.
		io.Copy(io.Discard, stdout)
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git diff: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	if rerr != nil {
		return rerr
	}
	if cur != nil && cur.generated == "" {
		d.source += cur.bytes
	}
	if n+1 != len(owner) {
		return fmt.Errorf("git diff gave %d patches for %d files", n+1, len(owner))
	}
	return nil
}

// write puts each source file's patch and the index in the reviewer's
// .devbot/.
func (d *reviewDiff) write(w *workspace, base, head string) error {
	for i, f := range d.files {
		if f.generated != "" {
			continue
		}
		f.patchPath = fmt.Sprintf("%s/%04d.patch", reviewPatches, i+1)
		if err := w.writeShared(f.patchPath, f.patch); err != nil {
			return err
		}
	}
	return w.writeShared(reviewIndex, []byte(d.index(base, head)))
}

// index lists every changed file, never cut short.
func (d *reviewDiff) index(base, head string) string {
	var b strings.Builder
	b.WriteString("# Changed files\n\n")
	fmt.Fprintf(&b, "Every file the PR at `%s` changes against its merge base with `%s`: %d files, of which %d are generated. "+
		"The patches of the files that aren't generated come to %s; a generated file's patch isn't given, so use `git diff %s...HEAD -- <path>` or `git show <blob>` for it. "+
		"Paths are the implementer's, so they are untrusted.\n\n",
		head[:min(12, len(head))], base, len(d.files), d.generatedCount(), size(int64(d.source)), base)
	b.WriteString("| # | Status | Path | + | - | Patch bytes | Size | Generated | Patch | Blobs (base..head) |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for i, f := range d.files {
		path := mdPath(f.path)
		if f.oldPath != "" {
			path = mdPath(f.oldPath) + " → " + path
		}
		added, deleted := strconv.Itoa(f.added), strconv.Itoa(f.deleted)
		if f.added < 0 {
			added, deleted = "bin", "bin"
		}
		gen, patch := "no", "`"+f.patchPath+"`"
		if f.generated != "" {
			gen, patch = "yes ("+f.generated+")", "-"
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %d | %d | %s | %s | `%s..%s` |\n",
			i+1, statusWord(f.status), path, added, deleted, f.bytes, f.size, gen, patch, abbrev(f.oldBlob), abbrev(f.newBlob))
	}
	return b.String()
}

func statusWord(s byte) string {
	switch s {
	case 'A':
		return "added"
	case 'C':
		return "copied"
	case 'D':
		return "deleted"
	case 'M':
		return "modified"
	case 'R':
		return "renamed"
	case 'T':
		return "type changed"
	}
	return string(s)
}

func abbrev(id string) string { return id[:min(12, len(id))] }

// mdPath is a path for a markdown table cell: in backticks, or quoted when
// it holds anything that would break out of one.
func mdPath(p string) string {
	if strings.ContainsAny(p, "`|") || strings.ContainsFunc(p, unicode.IsControl) {
		return "`" + strings.NewReplacer("`", `\x60`, "|", `\x7c`).Replace(strconv.Quote(p)) + "`"
	}
	return "`" + p + "`"
}

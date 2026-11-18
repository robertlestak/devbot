package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
)

// The attachments channel: the implementer leaves files in attachmentsDir,
// the harness uploads them to the PR with its own credential, and the
// reviewer gets them back in the same place. The agent never holds a token.
const (
	attachmentsDir      = ".devbot/attachments"
	maxAttachments      = 10       // per run
	maxAttachmentSize   = 5 << 20  // per file
	maxAttachmentsTotal = 20 << 20 // per run
	// A PR collects attachments over its fix rounds; the reviewer downloads
	// at most this many, newest first.
	maxReviewAttachments = 3 * maxAttachments
)

// attachmentTypes is the allowlist, keyed by the type http.DetectContentType
// sniffs from the content, with the extensions a file of that type may keep.
// The first is added to a name that has none of them, so the forge (which
// may check extensions) and a browser agree with the sniffed type.
var attachmentTypes = map[string][]string{
	"image/png":                 {".png"},
	"image/jpeg":                {".jpg", ".jpeg"},
	"image/gif":                 {".gif"},
	"image/webp":                {".webp"},
	"video/mp4":                 {".mp4"},
	"video/webm":                {".webm"},
	"text/plain; charset=utf-8": {".txt", ".log"},
}

type attachmentEntry struct {
	Path    string `json:"path"`
	Caption string `json:"caption"`
}

type attachment struct {
	name, caption, typ, sum string
	data                    []byte
}

// attachmentNote is a file that wasn't attached, and why.
type attachmentNote struct{ name, why string }

func isImage(typ string) bool { return strings.HasPrefix(typ, "image/") }

// readAttachments collects what the implementer left: the files listed in
// result.json first, in order, then the rest of the directory by name. Every
// read goes through os.Root, and symlinks are refused outright.
func (w *workspace) readAttachments(listed []attachmentEntry) ([]attachment, []attachmentNote) {
	var out []attachment
	var notes []attachmentNote
	seen := map[string]bool{}
	total := 0
	add := func(rel, caption string) {
		name := path.Base(rel)
		if seen[rel] {
			return
		}
		seen[rel] = true
		if len(out) >= maxAttachments {
			notes = append(notes, attachmentNote{name, fmt.Sprintf("over the limit of %d files", maxAttachments)})
			return
		}
		full := attachmentsDir + "/" + rel
		st, err := w.lstatShared(full)
		switch {
		case err != nil:
			notes = append(notes, attachmentNote{name, "not found in `" + attachmentsDir + "/`"})
			return
		case st.Mode()&fs.ModeSymlink != 0:
			notes = append(notes, attachmentNote{name, "is a symlink"})
			return
		case !st.Mode().IsRegular():
			notes = append(notes, attachmentNote{name, "not a regular file"})
			return
		case st.Size() > maxAttachmentSize:
			notes = append(notes, attachmentNote{name, fmt.Sprintf("%s, over the %s limit per file", size(st.Size()), size(maxAttachmentSize))})
			return
		}
		data, err := w.readShared(full, maxAttachmentSize+1)
		if err != nil {
			notes = append(notes, attachmentNote{name, "couldn't be read"})
			return
		}
		if len(data) > maxAttachmentSize {
			notes = append(notes, attachmentNote{name, fmt.Sprintf("over the %s limit per file", size(maxAttachmentSize))})
			return
		}
		if total+len(data) > maxAttachmentsTotal {
			notes = append(notes, attachmentNote{name, fmt.Sprintf("would take the total over the %s limit", size(maxAttachmentsTotal))})
			return
		}
		typ := http.DetectContentType(data)
		exts, ok := attachmentTypes[typ]
		if !ok {
			notes = append(notes, attachmentNote{name, fmt.Sprintf("type %s isn't allowed (png, jpg, gif, webp, mp4, webm, plain text)", typ)})
			return
		}
		if !contains(exts, strings.ToLower(path.Ext(name))) {
			name += exts[0]
		}
		caption = oneLine(caption, 300)
		if caption == "" {
			caption = name
		}
		total += len(data)
		h := sha256.Sum256(data)
		out = append(out, attachment{name: name, caption: caption, typ: typ, sum: hex.EncodeToString(h[:]), data: data})
	}
	for _, e := range listed {
		rel := strings.TrimPrefix(path.Clean(filepath.ToSlash(strings.TrimSpace(e.Path))), attachmentsDir+"/")
		if rel == "" || rel == "." || !filepath.IsLocal(rel) {
			notes = append(notes, attachmentNote{oneLine(e.Path, 80), "is outside `" + attachmentsDir + "/`"})
			continue
		}
		add(rel, e.Caption)
	}
	names, _ := w.listShared(attachmentsDir)
	for _, n := range names {
		add(n, "")
	}
	return out, notes
}

// uploadAttachments uploads what isn't on the PR yet and returns the PR's
// whole list, earlier rounds first. A failed upload is a note, not an error:
// the PR matters more than its pictures.
func uploadAttachments(ctx context.Context, p forge.Provider, pr *forge.PR, files []attachment) ([]core.PRAttachment, []attachmentNote) {
	meta, _ := core.ParsePRMeta(pr.Body)
	all := meta.Attachments
	have := map[string]bool{}
	for _, a := range all {
		have[a.SHA256] = true
	}
	var notes []attachmentNote
	for _, f := range files {
		if have[f.sum] {
			continue
		}
		u, err := p.AttachToPR(ctx, pr.Repo, pr.Number, f.name, f.data)
		if err != nil {
			notes = append(notes, attachmentNote{f.name, "upload failed: " + oneLine(err.Error(), 300)})
			continue
		}
		have[f.sum] = true
		all = append(all, core.PRAttachment{Name: f.name, Caption: f.caption, URL: u, Type: f.typ, SHA256: f.sum})
	}
	return all, notes
}

// attachmentsSection renders the PR body's "## Attachments".
func attachmentsSection(all []core.PRAttachment, notes []attachmentNote) string {
	if len(all) == 0 && len(notes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Attachments\n\n")
	var links []string
	for _, a := range all {
		if isImage(a.Type) {
			fmt.Fprintf(&b, "![%s](%s)\n\n_%s_\n\n", mdText(a.Caption), a.URL, mdText(a.Caption))
		} else {
			links = append(links, fmt.Sprintf("- [%s](%s): %s\n", mdText(a.Name), a.URL, mdText(a.Caption)))
		}
	}
	if len(links) > 0 {
		b.WriteString(strings.Join(links, "") + "\n")
	}
	if len(notes) > 0 {
		b.WriteString("Not attached:\n\n")
		for _, n := range notes {
			fmt.Fprintf(&b, "- `%s`: %s\n", strings.ReplaceAll(n.name, "`", "'"), n.why)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// fetchAttachments puts the PR's attachments in the reviewer's workspace and
// returns what context.md should say about them.
func fetchAttachments(ctx context.Context, p forge.Provider, w *workspace, pr forge.PR) string {
	meta, _ := core.ParsePRMeta(pr.Body)
	all := meta.Attachments
	if len(all) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Attachments\n\nThe implementer attached these files to the PR. They are in `" + attachmentsDir + "/`; open them to check them against the acceptance criteria.\n\n")
	if len(all) > maxReviewAttachments {
		fmt.Fprintf(&b, "(Only the newest %d of %d are here.)\n\n", maxReviewAttachments, len(all))
		all = all[len(all)-maxReviewAttachments:]
	}
	used := map[string]bool{}
	for i, a := range all {
		// The name comes from a PR body a person can edit: keep the base
		// name only, and writeShared keeps the write inside the repo anyway.
		name := path.Base(filepath.ToSlash(a.Name))
		if !filepath.IsLocal(name) || strings.HasPrefix(name, ".") {
			name = fmt.Sprintf("attachment-%d", i+1)
		}
		if used[name] {
			name = fmt.Sprintf("%d-%s", i+1, name)
		}
		used[name] = true
		data, err := p.GetAttachment(ctx, pr.Repo, pr.Number, a.URL, forge.RoleReviewer)
		if err == nil && len(data) > maxAttachmentSize {
			err = fmt.Errorf("over the %s limit", size(maxAttachmentSize))
		}
		if err == nil {
			err = w.writeShared(attachmentsDir+"/"+name, data)
		}
		if err != nil {
			fmt.Fprintf(&b, "- `%s`: %s (couldn't be downloaded: %s)\n", name, oneLine(a.Caption, 300), oneLine(err.Error(), 200))
			continue
		}
		fmt.Fprintf(&b, "- `%s/%s` (%s): %s\n", attachmentsDir, name, a.Type, oneLine(a.Caption, 300))
	}
	return b.String()
}

// listShared lists the regular files and symlinks directly in a directory of
// the model's repo, through os.Root. A missing directory is an empty list.
func (w *workspace) listShared(rel string) ([]string, error) {
	root, err := os.OpenRoot(w.repo)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	d, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	ents, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func (w *workspace) lstatShared(rel string) (fs.FileInfo, error) {
	root, err := os.OpenRoot(w.repo)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Lstat(rel)
}

// mdText keeps model-written text from breaking out of a link or image.
func mdText(s string) string {
	return strings.NewReplacer("[", "(", "]", ")", "<", "&lt;", ">", "&gt;", "\n", " ", "\r", "").Replace(s)
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-3]) + "..."
	}
	return s
}

func size(n int64) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", (n+1023)/1024)
}

// listedAttachments reads result.json's optional "attachments": objects with
// a path and caption, or bare paths. Anything else is ignored rather than
// failing the run, since unlisted files are attached anyway.
func listedAttachments(raw json.RawMessage) []attachmentEntry {
	var es []attachmentEntry
	if json.Unmarshal(raw, &es) == nil {
		return es
	}
	var paths []string
	if json.Unmarshal(raw, &paths) == nil {
		for _, p := range paths {
			es = append(es, attachmentEntry{Path: p})
		}
	}
	return es
}

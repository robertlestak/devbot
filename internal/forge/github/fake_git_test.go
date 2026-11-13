package github

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"path"
	"slices"
	"strings"
)

// fakeObject is a git blob, tree or commit in a fakeRepo.
type fakeObject struct {
	kind    string // "blob" | "tree" | "commit"
	data    []byte
	entries []treeEntry // a tree's, by name
	tree    string      // a commit's
	parents []string
	message string
}

// put stores o and returns its SHA, derived from its content as git's is.
func (r *fakeRepo) put(o *fakeObject) string {
	b, _ := json.Marshal([]any{o.kind, o.data, o.entries, o.tree, o.parents, o.message})
	sum := sha1.Sum(b)
	sha := hex.EncodeToString(sum[:])
	r.objects[sha] = o
	return sha
}

func (r *fakeRepo) blobAt(tree, path string) ([]byte, bool) {
	t := r.objects[tree]
	name, rest, nested := strings.Cut(path, "/")
	for _, e := range t.entries {
		if e.Path != name {
			continue
		}
		if nested {
			if e.Type != "tree" {
				return nil, false
			}
			return r.blobAt(e.SHA, rest)
		}
		if e.Type != "blob" {
			return nil, false
		}
		return r.objects[e.SHA].data, true
	}
	return nil, false
}

// withBlob is tree with path set to blob, as a new tree's SHA.
func (r *fakeRepo) withBlob(tree, path, blob string) string {
	var entries []treeEntry
	if t := r.objects[tree]; t != nil {
		entries = append(entries, t.entries...)
	}
	name, rest, nested := strings.Cut(path, "/")
	e := treeEntry{Path: name, Mode: "100644", Type: "blob", SHA: blob}
	i := 0
	for i < len(entries) && entries[i].Path != name {
		i++
	}
	if nested {
		sub := ""
		if i < len(entries) {
			sub = entries[i].SHA
		}
		e = treeEntry{Path: name, Mode: "040000", Type: "tree", SHA: r.withBlob(sub, rest, blob)}
	}
	if i < len(entries) {
		entries[i] = e
	} else {
		entries = append(entries, e)
	}
	return r.put(&fakeObject{kind: "tree", entries: entries})
}

func (r *fakeRepo) isAncestor(old, sha string) bool {
	if sha == old {
		return true
	}
	c := r.objects[sha]
	if c == nil || c.kind != "commit" {
		return false
	}
	for _, p := range c.parents {
		if r.isAncestor(old, p) {
			return true
		}
	}
	return false
}

// ruleViolation is how GitHub refuses a write that a ruleset blocks. The
// docs don't show it; this is the shape seen from github.com.
func ruleViolation(w http.ResponseWriter, status int) {
	writeJSON(w, status, map[string]any{"message": "Repository rule violations found\n\nCannot update this protected ref.\n\n", "status": fmt.Sprint(status)})
}

// serveGit is the git data API (docs: "Git database") and writes through
// the contents API, over a fakeRepo's objects.
func (f *fakeGitHub) serveGit(w http.ResponseWriter, r *http.Request, repo *fakeRepo, sub string) {
	var in struct {
		Ref, SHA, Message, Content, Branch, Tree string
		Parents                                  *[]string
		Force                                    bool
		BaseTree                                 *string `json:"base_tree"`
		Entries                                  []struct {
			Path, Mode, Type string
			SHA              *string
			Content          *string
		} `json:"tree"`
	}
	if r.Method != "GET" && r.Method != "DELETE" {
		// Tree is a commit's tree SHA, or a new tree's entries.
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			writeJSON(w, 400, map[string]string{"message": "Problems parsing JSON"})
			return
		}
		if t, ok := raw["tree"]; ok && len(t) > 0 && t[0] == '"' {
			json.Unmarshal(t, &in.Tree)
			delete(raw, "tree")
		}
		b, _ := json.Marshal(raw)
		json.Unmarshal(b, &in)
	}
	invalid := func(msg string) { writeJSON(w, 422, map[string]string{"message": msg}) }
	switch {
	case r.Method == "GET" && strings.HasPrefix(sub, "git/ref/heads/"):
		name := strings.TrimPrefix(sub, "git/ref/heads/")
		sha, ok := repo.branches[name]
		if !ok {
			notFound(w)
			return
		}
		writeJSON(w, 200, map[string]any{"ref": "refs/heads/" + name, "object": map[string]any{"type": "commit", "sha": sha}})
	case r.Method == "POST" && sub == "git/refs":
		name, ok := strings.CutPrefix(in.Ref, "refs/heads/")
		switch {
		case !ok || in.SHA == "":
			invalid("Reference name or sha invalid")
		case repo.branches[name] != "":
			invalid("Reference already exists")
		case repo.protected[name]:
			ruleViolation(w, 422)
		default:
			repo.branches[name] = in.SHA
			writeJSON(w, 201, map[string]any{"ref": in.Ref, "object": map[string]any{"sha": in.SHA}})
		}
	case r.Method == "PATCH" && strings.HasPrefix(sub, "git/refs/heads/"):
		name := strings.TrimPrefix(sub, "git/refs/heads/")
		old, ok := repo.branches[name]
		switch {
		case !ok:
			invalid("Reference does not exist")
		case repo.protected[name]:
			ruleViolation(w, 422)
		case !in.Force && !repo.isAncestor(old, in.SHA):
			invalid("Update is not a fast forward")
		default:
			repo.branches[name] = in.SHA
			writeJSON(w, 200, map[string]any{"ref": "refs/heads/" + name, "object": map[string]any{"sha": in.SHA}})
		}
	case r.Method == "POST" && sub == "git/trees":
		if in.BaseTree != nil {
			f.t.Errorf("the fake doesn't model base_tree")
		}
		var entries []treeEntry
		for _, e := range in.Entries {
			if strings.Contains(e.Path, "/") || (e.SHA == nil) == (e.Content == nil) {
				f.t.Errorf("the fake only models top-level entries with a sha or content: %+v", e)
				invalid("tree entry")
				return
			}
			sha := ""
			if e.Content != nil {
				sha = repo.put(&fakeObject{kind: "blob", data: []byte(*e.Content)})
			} else if o := repo.objects[*e.SHA]; o == nil || o.kind != e.Type {
				invalid("tree.sha " + *e.SHA + " is not a valid " + e.Type)
				return
			} else {
				sha = *e.SHA
			}
			entries = append(entries, treeEntry{Path: e.Path, Mode: e.Mode, Type: e.Type, SHA: sha})
		}
		writeJSON(w, 201, map[string]any{"sha": repo.put(&fakeObject{kind: "tree", entries: entries})})
	case r.Method == "GET" && strings.HasPrefix(sub, "git/trees/") && f.filesTree(w, r, repo, strings.TrimPrefix(sub, "git/trees/")):
	case r.Method == "GET" && strings.HasPrefix(sub, "git/trees/"):
		sha := strings.TrimPrefix(sub, "git/trees/")
		t := repo.objects[sha]
		if t == nil || t.kind != "tree" {
			notFound(w)
			return
		}
		writeJSON(w, 200, map[string]any{"sha": sha, "tree": t.entries, "truncated": false})
	case r.Method == "POST" && sub == "git/commits":
		if t := repo.objects[in.Tree]; t == nil || t.kind != "tree" || in.Parents == nil {
			invalid("Invalid tree or parents")
			return
		}
		writeJSON(w, 201, map[string]any{"sha": repo.put(&fakeObject{kind: "commit", tree: in.Tree, parents: *in.Parents, message: in.Message})})
	case r.Method == "GET" && strings.HasPrefix(sub, "git/commits/"):
		sha := strings.TrimPrefix(sub, "git/commits/")
		c := repo.objects[sha]
		if at, ok := repo.commitAt[sha]; ok && c == nil {
			writeJSON(w, 200, map[string]any{"sha": sha, "committer": map[string]any{"date": at}})
			return
		}
		if c == nil || c.kind != "commit" {
			notFound(w)
			return
		}
		writeJSON(w, 200, map[string]any{"sha": sha, "message": c.message, "tree": map[string]any{"sha": c.tree},
			"committer": map[string]any{"date": f.now()}})
	case r.Method == "GET" && strings.HasPrefix(sub, "git/matching-refs/heads/"):
		prefix := strings.TrimPrefix(sub, "git/matching-refs/heads/")
		out := []map[string]any{}
		for _, name := range slices.Sorted(maps.Keys(repo.branches)) {
			if strings.HasPrefix(name, prefix) {
				out = append(out, map[string]any{"ref": "refs/heads/" + name, "object": map[string]any{"type": "commit", "sha": repo.branches[name]}})
			}
		}
		writeJSON(w, 200, out)
	case r.Method == "DELETE" && strings.HasPrefix(sub, "git/refs/heads/"):
		name := strings.TrimPrefix(sub, "git/refs/heads/")
		switch {
		case repo.branches[name] == "":
			invalid("Reference does not exist")
		case repo.protected[name]:
			ruleViolation(w, 422)
		default:
			delete(repo.branches, name)
			w.WriteHeader(204)
		}
	case r.Method == "PUT" && strings.HasPrefix(sub, "contents/"):
		p := strings.TrimPrefix(sub, "contents/")
		head := repo.objects[repo.branches[in.Branch]]
		data, err := base64.StdEncoding.DecodeString(in.Content)
		switch {
		case head == nil || head.kind != "commit":
			notFound(w)
		case err != nil:
			invalid("content is not valid Base64")
		case repo.protected[in.Branch]:
			ruleViolation(w, 409)
		default:
			if _, exists := repo.blobAt(head.tree, p); exists && in.SHA == "" {
				invalid("Invalid request.\n\n\"sha\" wasn't supplied.")
				return
			}
			tree := repo.withBlob(head.tree, p, repo.put(&fakeObject{kind: "blob", data: data}))
			sha := repo.put(&fakeObject{kind: "commit", tree: tree, parents: []string{repo.branches[in.Branch]}, message: in.Message})
			repo.branches[in.Branch] = sha
			writeJSON(w, 201, map[string]any{"content": map[string]any{"path": p}, "commit": map[string]any{"sha": sha}})
		}
	default:
		notFound(w)
	}
}

// commitsOn counts the commits on branch, following first parents.
func (f *fakeGitHub) commitsOn(full, branch string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	repo := f.repos[full]
	n := 0
	for c := repo.objects[repo.branches[branch]]; c != nil && c.kind == "commit"; {
		n++
		if len(c.parents) == 0 {
			break
		}
		c = repo.objects[c.parents[0]]
	}
	return n
}

// filesOn lists the paths of the blobs on branch.
func (f *fakeGitHub) filesOn(full, branch string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	repo := f.repos[full]
	c := repo.objects[repo.branches[branch]]
	if c == nil {
		return nil
	}
	var out []string
	var walk func(tree, prefix string)
	walk = func(tree, prefix string) {
		for _, e := range repo.objects[tree].entries {
			if e.Type == "tree" {
				walk(e.SHA, prefix+e.Path+"/")
			} else {
				out = append(out, prefix+e.Path)
			}
		}
	}
	walk(c.tree, "")
	return out
}

// filesTree answers a tree read of a branch, or its SHA, whose files are in
// files, and of the directories in it, which it names "~<branch>~<dir>"
// with dir's slashes as "|". Recursive, it is every entry under it at once,
// directories too; past repo.treeMax entries it is cut short and marked
// truncated, as GitHub cuts its own past 100,000. It reports whether id
// was such a tree.
func (f *fakeGitHub) filesTree(w http.ResponseWriter, r *http.Request, repo *fakeRepo, id string) bool {
	ref, dir := id, ""
	if rest, ok := strings.CutPrefix(id, "~"); ok {
		ref, dir, _ = strings.Cut(rest, "~")
		dir = strings.ReplaceAll(dir, "|", "/")
	} else {
		for name, sha := range repo.branches {
			if sha == id {
				ref = name
			}
		}
		if _, ok := repo.branches[ref]; !ok || (repo.objects[id] != nil && r.URL.Query().Get("recursive") == "") {
			return false
		}
	}
	recursive := r.URL.Query().Get("recursive") != ""
	var all []map[string]any
	dirs := map[string]bool{}
	for _, k := range slices.Sorted(maps.Keys(repo.files)) {
		p, ok := strings.CutPrefix(k, ref+":")
		if !ok {
			continue
		}
		if dir != "" {
			if p, ok = strings.CutPrefix(p, dir+"/"); !ok {
				continue
			}
		}
		for d := path.Dir(p); d != "." && !dirs[d]; d = path.Dir(d) {
			dirs[d] = true
			if recursive || !strings.Contains(d, "/") {
				all = append(all, map[string]any{"path": d, "type": "tree", "mode": "040000",
					"sha": "~" + ref + "~" + strings.ReplaceAll(path.Join(dir, d), "/", "|")})
			}
		}
		if recursive || !strings.Contains(p, "/") {
			all = append(all, map[string]any{"path": p, "type": "blob", "mode": "100644", "size": len(repo.files[k])})
		}
	}
	truncated := repo.treeMax > 0 && len(all) > repo.treeMax
	if truncated {
		all = all[:repo.treeMax]
	}
	writeJSON(w, 200, map[string]any{"sha": id, "tree": all, "truncated": truncated})
	return true
}

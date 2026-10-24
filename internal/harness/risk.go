package harness

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"git.shdw.tech/rob/devbot/internal/config"
)

// The risk read of a held change: what in its protected files could let CI
// run code nobody reviewed with the repo's secrets, or reach devbot's own
// branches, said plainly above the diff for the person approving it. It is
// a reading aid, not a gate: it can miss things, and the comment says so.

// riskFile is one protected file of a held change: its content on the base
// and in the change ("" where it doesn't exist), and its patch.
type riskFile struct {
	Path, Old, New, Patch string
}

// sampleBranch is a branch of devbot's, for reading which of a workflow's
// triggers reach devbot/**.
const sampleBranch = "devbot/1-task"

var (
	secretRE     = regexp.MustCompile(`\bsecrets\b`)
	secretNameRE = regexp.MustCompile(`\bsecrets\s*\.\s*([A-Za-z_][A-Za-z0-9_-]*)`)
	exprRE       = regexp.MustCompile(`\$\{\{.*?\}\}`)
	usesRE       = regexp.MustCompile(`^\s*(?:-\s*)?uses\s*:\s*['"]?([^'"#\s]+)`)
	ifRE         = regexp.MustCompile(`^\s*(?:-\s*)?if\s*:`)
	ignoreRE     = regexp.MustCompile(`^\s*(?:-\s*)?branches-ignore\s*:`)
	shaRefRE     = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// risk is what riskRead found: flags are the things a person should look
// at, one markdown line each; warn the workflows that both run on devbot's
// branches and use the repo's secrets; exposed those that do either, which
// run the rest of the held commit, unreviewed, as soon as it is pushed.
type risk struct {
	flags, warn, exposed []string
}

// riskRead reads the held change's protected files. others are the paths
// the held commit changes outside them, which a workflow may run.
func riskRead(files []riskFile, others []string) risk {
	var r risk
	for _, f := range files {
		added, removed := patchLines(f.Patch)
		p := config.ShowPath(f.Path)

		var refs []string
		old := secretRefs(strings.Split(f.Old, "\n"))
		for _, ref := range secretRefs(added) {
			if !slices.Contains(old, ref) {
				refs = append(refs, ref)
			}
		}
		if len(refs) > 0 {
			r.flags = append(r.flags, fmt.Sprintf("%s adds uses of the repo's secrets: %s.", p, codes(refs)))
		}

		if workflow(f.Path) && f.New != "" {
			now, err := reach(f.New)
			if err != nil {
				r.flags = append(r.flags, fmt.Sprintf("%s doesn't parse as YAML (%s), so devbot couldn't read its triggers or steps: check them by hand.", p, codeSpan(err.Error())))
			} else {
				before, _ := reach(f.Old)
				for _, t := range now {
					if slices.Contains(before, t) {
						continue
					}
					switch t {
					case "pull_request_target", "workflow_run":
						r.flags = append(r.flags, fmt.Sprintf("%s adds a `%s` trigger, which runs with the repo's secrets whoever opened the change behind it.", p, t))
					default:
						r.flags = append(r.flags, fmt.Sprintf("%s now runs on `%s` for devbot's branches (`devbot/**`), with whatever secrets it uses.", p, t))
					}
				}
				secrets := secretRE.MatchString(f.New)
				if len(now) > 0 && secrets {
					r.warn = append(r.warn, p)
				}
				if len(now) > 0 || secrets {
					r.exposed = append(r.exposed, p)
				}
			}
		}
		// Workflows, and the actions they use, alike.
		if ext := path.Ext(f.Path); (ext == ".yml" || ext == ".yaml") && f.New != "" {
			if called := calls(runSteps(f.New), others); len(called) > 0 {
				r.flags = append(r.flags, fmt.Sprintf("%s has `run:` steps that call files this commit changes outside the protected paths, which this comment only lists: %s.", p, showPaths(called)))
			}
		}

		kept := map[string]bool{}
		for _, l := range added {
			kept[strings.TrimSpace(l)] = true
		}
		for _, l := range removed {
			t := strings.TrimSpace(l)
			if t == "" || kept[t] {
				continue
			}
			if ignoreRE.MatchString(l) || strings.Contains(l, "devbot") || (ifRE.MatchString(l) && strings.Contains(l, "ref")) {
				r.flags = append(r.flags, fmt.Sprintf("%s removes %s, which may have kept jobs off devbot's branches.", p, codes([]string{t})))
			}
		}

		var unpinned, third []string
		for _, l := range added {
			m := usesRE.FindStringSubmatch(l)
			if m == nil || strings.HasPrefix(m[1], "./") {
				continue
			}
			u := m[1]
			if docker, ok := strings.CutPrefix(u, "docker://"); ok {
				if !strings.Contains(docker, "@sha256:") && !slices.Contains(unpinned, u) {
					unpinned = append(unpinned, u)
				}
				continue
			}
			action, ref, _ := strings.Cut(u, "@")
			if !shaRefRE.MatchString(ref) && !slices.Contains(unpinned, u) {
				unpinned = append(unpinned, u)
			}
			owner, rest, _ := strings.Cut(action, "/")
			repo, _, _ := strings.Cut(rest, "/")
			name := owner + "/" + repo
			if !strings.EqualFold(owner, "actions") && !strings.EqualFold(owner, "github") && !strings.Contains(f.Old, name) && !slices.Contains(third, name) {
				third = append(third, name)
			}
		}
		if len(third) > 0 {
			r.flags = append(r.flags, fmt.Sprintf("%s brings in third-party actions it didn't use before: %s.", p, codes(third)))
		}
		if len(unpinned) > 0 {
			r.flags = append(r.flags, fmt.Sprintf("%s uses actions not pinned to a commit SHA, which their owners can change under it: %s.", p, codes(unpinned)))
		}
	}
	return r
}

// workflow reports whether path is an Actions workflow of a forge devbot
// knows, whose triggers reach can read.
func workflow(p string) bool {
	dir, ext := path.Dir(p), path.Ext(p)
	return (ext == ".yml" || ext == ".yaml") && (dir == ".github/workflows" || dir == ".gitea/workflows" || dir == ".forgejo/workflows")
}

// reach lists a workflow's triggers that run it for devbot's own work: on
// a push to, or the creation of, a devbot/** branch, or on its PR (whose
// branches filter is about the base, so any), and the two that run with
// the secrets whatever started them, pull_request_target and workflow_run.
func reach(content string) ([]string, error) {
	if strings.TrimSpace(content) == "" {
		return nil, nil
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, err
	}
	on, ok := doc["on"]
	if !ok {
		on = doc["true"] // "on" as YAML 1.1 reads it
	}
	events := map[string]any{}
	switch v := on.(type) {
	case string:
		events[v] = nil
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok {
				events[s] = nil
			}
		}
	case map[string]any:
		events = v
	}
	var out []string
	for _, e := range []string{"push", "create", "pull_request", "pull_request_target", "workflow_run"} {
		filters, ok := events[e]
		if !ok || (e == "push" && !pushReaches(filters)) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// pushReaches reads a push trigger's filters for sampleBranch: no branch
// filter at all runs it for every branch, unless only tags are filtered.
func pushReaches(filters any) bool {
	f, _ := filters.(map[string]any)
	branches, hasB := f["branches"]
	ignore, hasI := f["branches-ignore"]
	_, hasT := f["tags"]
	_, hasTI := f["tags-ignore"]
	switch {
	case hasB:
		in := false
		for _, pat := range strs(branches) {
			if neg, ok := strings.CutPrefix(pat, "!"); ok {
				if branchGlob(neg, sampleBranch) {
					in = false
				}
			} else if branchGlob(pat, sampleBranch) {
				in = true
			}
		}
		return in
	case hasI:
		for _, pat := range strs(ignore) {
			if branchGlob(pat, sampleBranch) {
				return false
			}
		}
		return true
	}
	return !hasT && !hasTI
}

func strs(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// branchGlob matches a branch against an Actions filter pattern: ** spans
// slashes, * doesn't, ? and + repeat the character before them, and []
// is a class.
func branchGlob(pat, name string) bool {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pat); i++ {
		switch c := pat[i]; {
		case c == '*' && i+1 < len(pat) && pat[i+1] == '*':
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?' || c == '+':
			b.WriteByte(c)
		case c == '[':
			if j := strings.IndexByte(pat[i:], ']'); j > 0 {
				b.WriteString(pat[i : i+j+1])
				i += j
			} else {
				b.WriteString(`\[`)
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return err == nil && re.MatchString(name)
}

// patchLines splits a unified diff into the lines it adds and removes.
func patchLines(patch string) (added, removed []string) {
	for _, l := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---"):
		case strings.HasPrefix(l, "+"):
			added = append(added, l[1:])
		case strings.HasPrefix(l, "-"):
			removed = append(removed, l[1:])
		}
	}
	return added, removed
}

// codes is each of list in a code span, for a comment.
func codes(list []string) string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = codeSpan(s)
	}
	return strings.Join(out, ", ")
}

// codeSpan puts s, text from a held change, in a code span it can't close,
// with no HTML comment opener left that could pass for a devbot marker.
func codeSpan(s string) string {
	s = strings.ReplaceAll(oneLine(s, 200), "<!--", "&lt;!--")
	if !strings.Contains(s, "`") {
		return "`" + s + "`"
	}
	ticks := "``"
	for strings.Contains(s, ticks) {
		ticks += "`"
	}
	return ticks + " " + s + " " + ticks
}

// codeBlock puts s in a fenced block of lang it can't close early, every
// line as it is but for HTML comment openers: devbot reads markers from its
// own comments, so none can be left in, and stripping one would hide what
// it says from the person reading the diff.
func codeBlock(lang, s string) string {
	fence := "```"
	for strings.Contains(s, fence) {
		fence += "`"
	}
	return fence + lang + "\n" + strings.ReplaceAll(s, "<!--", "&lt;!--") + "\n" + fence
}

// secretRefs lists the uses of the repo's secrets in lines: secrets.NAME
// for each named one, and, for one read any other way (secrets['X'],
// toJSON(secrets), "secrets: inherit"), the expression it is in, or the
// line when it is in none.
func secretRefs(lines []string) []string {
	var out []string
	add := func(s string) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	// More uses of secrets than named ones: some read it whole.
	whole := func(s string) bool {
		return len(secretRE.FindAllStringIndex(s, -1)) > len(secretNameRE.FindAllStringIndex(s, -1))
	}
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#") || !secretRE.MatchString(t) {
			continue
		}
		for _, m := range secretNameRE.FindAllStringSubmatch(t, -1) {
			add("secrets." + m[1])
		}
		rest := t
		for _, e := range exprRE.FindAllString(t, -1) {
			if whole(e) {
				add(e)
			}
			rest = strings.Replace(rest, e, "", 1)
		}
		if whole(rest) {
			add(t)
		}
	}
	return out
}

// runSteps is every `run:` script in a workflow or action, wherever it is.
func runSteps(content string) []string {
	var doc any
	if yaml.Unmarshal([]byte(content), &doc) != nil {
		return nil
	}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, e := range v {
				if s, ok := e.(string); ok && k == "run" {
					out = append(out, s)
				} else {
					walk(e)
				}
			}
		case []any:
			for _, e := range v {
				walk(e)
			}
		}
	}
	walk(doc)
	return out
}

// calls lists the paths in others that the scripts name, by path, or, for
// a Makefile or a package.json, by the tool that reads it.
func calls(scripts, others []string) []string {
	var out []string
	for _, o := range others {
		tool := map[string]string{"Makefile": "make", "makefile": "make", "GNUmakefile": "make", "package.json": "npm"}[path.Base(o)]
		for _, s := range scripts {
			if mentions(s, o) || (tool != "" && mentions(s, tool)) || (tool == "npm" && (mentions(s, "yarn") || mentions(s, "pnpm"))) {
				out = append(out, o)
				break
			}
		}
	}
	return out
}

// mentions reports whether s names name as a word, or a path, of its own:
// "./scripts/x.sh" names "scripts/x.sh", "lib/scripts/x.sh" doesn't, and
// a word ends at a sentence's full stop.
func mentions(s, name string) bool {
	part := func(c byte) bool {
		return c == '_' || c == '-' || c == '.' || c == '/' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
	}
	for i := 0; ; {
		j := strings.Index(s[i:], name)
		if j < 0 {
			return false
		}
		j += i
		start := strings.TrimSuffix(s[:j], "./")
		end := strings.TrimPrefix(s[j+len(name):], ".")
		if (start == "" || !part(start[len(start)-1])) && (end == "" || !part(end[0])) {
			return true
		}
		i = j + 1
	}
}

// showPaths is each of paths as config.ShowPath shows it.
func showPaths(paths []string) string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = config.ShowPath(p)
	}
	return strings.Join(out, ", ")
}

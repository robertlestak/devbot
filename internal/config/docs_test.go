package config

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// keys lists every YAML key of t as a dotted path: "timeouts.implement",
// "providers[].url". Nested structs recurse; durations and scalars are leaves.
func keys(t reflect.Type, prefix string) []string {
	var out []string
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		path := prefix + tag
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch {
		case ft.Kind() == reflect.Struct && ft != reflect.TypeOf(time.Duration(0)):
			out = append(out, keys(ft, path+".")...)
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
			out = append(out, path)
			out = append(out, keys(ft.Elem(), path+"[].")...)
		default:
			out = append(out, path)
		}
	}
	return out
}

// The reference docs must name every key the code reads, so adding a knob
// without documenting it fails the build.
func TestDocsCoverEveryKey(t *testing.T) {
	cases := []struct {
		doc string
		typ reflect.Type
	}{
		{"../../docs/devbot-yaml.md", reflect.TypeOf(Repo{})},
		{"../../docs/operating.md", reflect.TypeOf(Config{})},
	}
	for _, c := range cases {
		b, err := os.ReadFile(c.doc)
		if err != nil {
			t.Fatal(err)
		}
		doc := string(b)
		for _, k := range keys(c.typ, "") {
			if !strings.Contains(doc, "`"+k+"`") {
				t.Errorf("%s doesn't document `%s`", c.doc, k)
			}
		}
	}
}

func TestSchemaCoversRepoKeys(t *testing.T) {
	b, err := os.ReadFile("../../docs/devbot.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	type node struct {
		Properties map[string]*node `json:"properties"`
		Items      *node            `json:"items"`
	}
	var s node
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("schema isn't valid JSON: %v", err)
	}
	for _, k := range keys(reflect.TypeOf(Repo{}), "") {
		n := &s
		for _, part := range strings.Split(k, ".") {
			// "services[]" is the items of the array "services".
			name, list := strings.CutSuffix(part, "[]")
			if n = n.Properties[name]; n != nil && list {
				n = n.Items
			}
			if n == nil {
				t.Errorf("schema has no property %q", k)
				break
			}
		}
	}
}

// The default protected paths are listed in the docs and the schema too.
func TestDocsListDefaultProtectedPaths(t *testing.T) {
	b, err := os.ReadFile("../../docs/devbot.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties struct {
			ProtectedPaths struct {
				Default []string `json:"default"`
			} `json:"protected_paths"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Properties.ProtectedPaths.Default, DefaultProtectedPaths) {
		t.Errorf("schema default %v, want %v", s.Properties.ProtectedPaths.Default, DefaultProtectedPaths)
	}
	doc, err := os.ReadFile("../../docs/devbot-yaml.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range DefaultProtectedPaths {
		if !strings.Contains(string(doc), "\n  - "+p+"\n") && !strings.Contains(string(doc), "\n  - "+p+" ") {
			t.Errorf("docs/devbot-yaml.md doesn't list %s", p)
		}
	}
}

// schemaNode is the part of a JSON Schema the tests below read.
type schemaNode struct {
	Properties map[string]*schemaNode `json:"properties"`
	Items      *schemaNode            `json:"items"`
	Default    json.RawMessage        `json:"default"`
}

// field is the value at a dotted YAML key path in v, a struct.
func field(v reflect.Value, path string) reflect.Value {
	for _, part := range strings.Split(path, ".") {
		for v.Kind() == reflect.Pointer && !v.IsNil() {
			v = v.Elem()
		}
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			if strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0] == part {
				v = v.Field(i)
				break
			}
		}
	}
	return v
}

// The defaults the schema and the reference table give are the ones fill
// applies, and every key fill gives a value has one in both.
func TestSchemaAndDocsDefaultsMatchTheCode(t *testing.T) {
	b, err := os.ReadFile("../../docs/devbot.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s schemaNode
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile("../../docs/devbot-yaml.md")
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseRepo([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys(reflect.TypeOf(Repo{}), "") {
		if strings.Contains(k, "[]") || k == "protected_paths" {
			// List entries have no defaults; protected_paths has its own
			// test, since fill adds AlwaysProtected to it.
			continue
		}
		n := &s
		for _, part := range strings.Split(k, ".") {
			if n = n.Properties[part]; n == nil {
				break
			}
		}
		v := field(reflect.ValueOf(r).Elem(), k)
		var got any
		switch {
		case k == "enabled":
			got = r.IsEnabled()
		case v.Type() == reflect.TypeOf(time.Duration(0)):
			got = v.Interface()
		case v.Kind() == reflect.Pointer && v.IsNil():
			got = nil
		default:
			j, _ := json.Marshal(v.Interface())
			json.Unmarshal(j, &got)
		}
		if n == nil || n.Default == nil {
			if got != nil && !v.IsZero() {
				t.Errorf("%s defaults to %v, and the schema gives no default", k, got)
			}
			continue
		}
		var want any
		json.Unmarshal(n.Default, &want)
		if d, ok := got.(time.Duration); ok {
			ws, _ := want.(string)
			if wd, err := time.ParseDuration(ws); err != nil || wd != d {
				t.Errorf("%s: schema default %q, code %v", k, ws, d)
			}
			got = ws
		} else if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: schema default %v, code %v", k, want, got)
		}
		_, row, ok := strings.Cut(string(doc), "\n| `"+k+"` | ")
		if !ok {
			t.Errorf("devbot-yaml.md has no row for %s", k)
			continue
		}
		if cols := strings.SplitN(row, " | ", 3); len(cols) < 3 || cols[1] != fmt.Sprintf("`%v`", got) {
			t.Errorf("devbot-yaml.md's default for %s isn't `%v`", k, got)
		}
	}
}

// envReads are the environment variables the non-test Go code under root
// reads by name: os.Getenv, os.LookupEnv, and the envOr and getenv helpers
// with a constant name. A name built from a constant prefix ends in "<".
func envReads(t *testing.T, root string) []string {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string][]*ast.File{} // by package directory
	consts := map[string]string{}     // "pkg.Name" and "dir:Name"
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		dir := filepath.Dir(p)
		files[dir] = append(files[dir], f)
		for _, decl := range f.Decls {
			g, ok := decl.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							v, _ := strconv.Unquote(lit.Value)
							consts[f.Name.Name+"."+name.Name] = v
							consts[dir+":"+name.Name] = v
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for dir, fs := range files {
		var name func(ast.Expr) (string, bool)
		name = func(x ast.Expr) (string, bool) {
			switch x := x.(type) {
			case *ast.BasicLit:
				s, err := strconv.Unquote(x.Value)
				return s, err == nil
			case *ast.Ident:
				s, ok := consts[dir+":"+x.Name]
				return s, ok
			case *ast.SelectorExpr:
				if pkg, ok := x.X.(*ast.Ident); ok {
					s, ok := consts[pkg.Name+"."+x.Sel.Name]
					return s, ok
				}
			case *ast.BinaryExpr:
				if s, ok := name(x.X); ok && x.Op == token.ADD {
					return s + "<", true
				}
			}
			return "", false
		}
		for _, f := range fs {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				var fn string
				switch x := call.Fun.(type) {
				case *ast.SelectorExpr:
					if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "os" {
						fn = x.Sel.Name
					}
				case *ast.Ident:
					fn = x.Name
				}
				if fn != "Getenv" && fn != "LookupEnv" && fn != "envOr" && fn != "getenv" {
					return true
				}
				if s, ok := name(call.Args[0]); ok {
					seen[s] = true
				}
				return true
			})
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// operating.md's table of environment variables names every variable the
// code reads.
func TestDocsListEveryEnvVar(t *testing.T) {
	b, err := os.ReadFile("../../docs/operating.md")
	if err != nil {
		t.Fatal(err)
	}
	_, table, _ := strings.Cut(string(b), "\n## Environment variables\n")
	table, _, _ = strings.Cut(table, "\n## ")
	reads := envReads(t, "../..")
	if !slices.Contains(reads, "DEVBOT_CONFIG") {
		t.Fatalf("found %v, which misses DEVBOT_CONFIG: the scan is broken", reads)
	}
	for _, v := range reads {
		want := "`" + v
		if !strings.HasSuffix(v, "<") {
			want += "`"
		}
		if !strings.Contains(table, want) {
			t.Errorf("operating.md's environment variables don't list %s", v)
		}
	}
}

// eventTypes is core.EventTypes, read from its source: core imports this
// package, so this test can't import core.
func eventTypes(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "../core/engine.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "EventTypes" {
			return true
		}
		for _, e := range vs.Values[0].(*ast.CompositeLit).Elts {
			s, _ := strconv.Unquote(e.(*ast.BasicLit).Value)
			out = append(out, s)
		}
		return false
	})
	if len(out) == 0 {
		t.Fatal("no EventTypes in internal/core/engine.go")
	}
	return out
}

// connectors.md's table of event types is core.EventTypes, and every event
// type the code names is one of them.
func TestDocsListEveryEventType(t *testing.T) {
	want := eventTypes(t)
	b, err := os.ReadFile("../../docs/connectors.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, _ := strings.Cut(string(b), "\n## Events\n")
	section, _, _ = strings.Cut(section, "\n## ")
	var doc []string
	for _, line := range strings.Split(section, "\n") {
		if first, _, ok := strings.Cut(strings.TrimPrefix(line, "| "), " | "); ok && (strings.HasPrefix(line, "| `task.") || strings.HasPrefix(line, "| `parent.")) {
			for _, typ := range strings.Split(first, ", ") {
				doc = append(doc, strings.Trim(typ, "`"))
			}
		}
	}
	if !reflect.DeepEqual(doc, want) {
		t.Errorf("connectors.md lists event types %v, want core.EventTypes %v", doc, want)
	}
	// Every "task.*" and "parent.*" literal in the code is an event type,
	// but for the audit record of a label change.
	litRE := regexp.MustCompile(`"((?:task|parent)\.[a-z_]+)"`)
	filepath.WalkDir("../..", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		src, _ := os.ReadFile(p)
		for _, m := range litRE.FindAllStringSubmatch(string(src), -1) {
			if m[1] != "task.transition" && !slices.Contains(want, m[1]) {
				t.Errorf("%s names event type %s, which isn't in core.EventTypes", p, m[1])
			}
		}
		return nil
	})
}

// The quickstart's Gitea webhook events, named as Gitea's form names them,
// are operating.md's, and include the PR label event PR adoption needs.
func TestDocsGiteaWebhookEventsMatch(t *testing.T) {
	ui := map[string]string{
		"Issues": "issues", "Issue Label": "issue_label", "Issue Comment": "issue_comment",
		"Pull Request": "pull_request", "Pull Request Synchronized": "pull_request_sync",
		"Pull Request Label": "pull_request_label", "Pull Request Reviewed": "pull_request_review_*",
		"Pull Request Comment": "pull_request_comment", "Status": "status",
		"Workflow Run": "workflow_run", "Workflow Job": "workflow_job", "Push": "push",
	}
	section := func(path, from, to string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_, s, ok := strings.Cut(string(b), from)
		s, _, ok2 := strings.Cut(s, to)
		if !ok || !ok2 {
			t.Fatalf("%s: no %q ... %q", path, from, to)
		}
		return s
	}
	var quick []string
	for _, m := range regexp.MustCompile(`\*\*([A-Z][A-Za-z ]+?):?\*\*`).FindAllStringSubmatch(section("../../docs/quickstart.md", "- **Trigger on:**", "\nA Gitea outside"), -1) {
		id, ok := ui[m[1]]
		if !ok {
			t.Errorf("quickstart.md: Gitea event %q has no webhook name here", m[1])
		}
		quick = append(quick, id)
	}
	var ops []string
	for _, m := range regexp.MustCompile("`([a-z_*]+)`").FindAllStringSubmatch(section("../../docs/operating.md", "\n- **Events:** all of these", "\nTwo Gitea traps:"), -1) {
		ops = append(ops, m[1])
	}
	slices.Sort(quick)
	slices.Sort(ops)
	if !slices.Equal(quick, ops) {
		t.Errorf("quickstart.md's Gitea webhook events %v, operating.md's %v", quick, ops)
	}
	if !slices.Contains(ops, "pull_request_label") {
		t.Error("the Gitea webhook events leave out pull_request_label")
	}
}

// anchors are the heading anchors a forge renders for doc: lower case, with
// punctuation dropped and spaces turned into dashes, numbered when repeated.
func anchors(doc string) map[string]bool {
	out, seen := map[string]bool{}, map[string]int{}
	drop := regexp.MustCompile(`[^\p{L}\p{N} _-]`)
	fence := false
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "```") {
			fence = !fence
		}
		h := strings.TrimLeft(line, "#")
		if fence || h == line || !strings.HasPrefix(h, " ") {
			continue
		}
		a := strings.ReplaceAll(drop.ReplaceAllString(strings.ToLower(strings.TrimSpace(h)), ""), " ", "-")
		if n := seen[a]; n > 0 {
			out[fmt.Sprintf("%s-%d", a, n)] = true
		} else {
			out[a] = true
		}
		seen[a]++
	}
	return out
}

// Every relative link in the docs and the README goes to a file that
// exists, and to a heading it has.
func TestDocsLinksResolve(t *testing.T) {
	docs, _ := filepath.Glob("../../docs/*.md")
	docs = append(docs, "../../README.md")
	linkRE := regexp.MustCompile(`\]\(([^)\s]+)\)`)
	cache := map[string]map[string]bool{}
	read := func(p string) (map[string]bool, bool) {
		if a, ok := cache[p]; ok {
			return a, true
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, false
		}
		cache[p] = anchors(string(b))
		return cache[p], true
	}
	for _, d := range docs {
		b, _ := os.ReadFile(d)
		for _, m := range linkRE.FindAllStringSubmatch(string(b), -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			file, anchor, _ := strings.Cut(target, "#")
			p := d
			if file != "" {
				p = filepath.Join(filepath.Dir(d), file)
			}
			if !strings.HasSuffix(p, ".md") {
				if _, err := os.Stat(p); err != nil {
					t.Errorf("%s links to %s, which doesn't exist", d, target)
				}
				continue
			}
			a, ok := read(p)
			if !ok {
				t.Errorf("%s links to %s, which doesn't exist", d, target)
			} else if anchor != "" && !a[anchor] {
				t.Errorf("%s links to %s, which has no such heading", d, target)
			}
		}
	}
}

// A blank line inside a table ends it: what follows renders as text. Each
// table in operating.md's config reference is one block of rows.
func TestConfigTableIsOneTable(t *testing.T) {
	b, err := os.ReadFile("../../docs/operating.md")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	for i := 1; i+1 < len(lines); i++ {
		if lines[i] == "" && strings.HasPrefix(lines[i-1], "| `") && strings.HasPrefix(lines[i+1], "| `") {
			t.Errorf("operating.md:%d: a blank line splits a table", i+1)
		}
	}
}

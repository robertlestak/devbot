package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// usageText names every command main dispatches and every flag a client
// command defines, and docs/connectors.md's CLI reference quotes it as it is.
func TestUsageListsEveryCommandAndFlag(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var cmds, flags []string
	lit := func(e ast.Expr) string {
		if b, ok := e.(*ast.BasicLit); ok && b.Kind == token.STRING {
			s, _ := strconv.Unquote(b.Value)
			return s
		}
		return ""
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			if n.Name.Name == "main" {
				ast.Inspect(n.Body, func(n ast.Node) bool {
					if cc, ok := n.(*ast.CaseClause); ok {
						for _, e := range cc.List {
							if s := lit(e); s != "" {
								cmds = append(cmds, s)
							}
						}
					}
					return true
				})
			}
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, _ := sel.X.(*ast.Ident); x == nil || x.Name != "fs" {
				return true
			}
			i := 0
			if strings.HasSuffix(sel.Sel.Name, "Var") {
				i = 1
			}
			if i < len(n.Args) {
				flags = append(flags, lit(n.Args[i]))
			}
		}
		return true
	})
	if !slices.Contains(cmds, "serve") || !slices.Contains(flags, "repo") {
		t.Fatalf("found commands %v and flags %v: the scan is broken", cmds, flags)
	}
	for _, c := range cmds {
		if !regexp.MustCompile(`(devbot | \| )` + regexp.QuoteMeta(c) + `( |$|\n)`).MatchString(usageText) {
			t.Errorf("usage doesn't list the command %s", c)
		}
	}
	for _, fl := range flags {
		if !regexp.MustCompile(`\[?-` + regexp.QuoteMeta(fl) + `[ \]]`).MatchString(usageText) {
			t.Errorf("usage doesn't list the flag -%s", fl)
		}
	}
	// One placeholder means one thing: -base's isn't -body's.
	by := map[string]string{}
	for _, m := range regexp.MustCompile(`-([a-z]+) ([A-Z]+)`).FindAllStringSubmatch(usageText, -1) {
		if fl, ok := by[m[2]]; ok && fl != m[1] {
			t.Errorf("-%s and -%s share the placeholder %s", fl, m[1], m[2])
		}
		by[m[2]] = m[1]
	}
	doc, err := os.ReadFile("../../docs/connectors.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "```text\n"+usageText+"\n```") {
		t.Error("docs/connectors.md's CLI reference doesn't quote usageText as it is")
	}
}

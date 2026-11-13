// Package agenttest helps test agent harness adapters against a fake of
// their CLI, so an adapter's flags, parsing and error handling are tested
// without the real binary or a model.
package agenttest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// CLI is a fake agent CLI: an executable that records how it was called and
// then runs a bash script standing in for the real thing.
type CLI struct {
	// Path is the executable to give the adapter as its command.
	Path string
	dir  string
}

// Call is one recorded invocation.
type Call struct {
	Args  []string
	Stdin string
	Env   []string // KEY=value, as the adapter set it
	Dir   string
}

// Getenv returns the value of key in the call's environment.
func (c Call) Getenv(key string) (string, bool) {
	for _, kv := range c.Env {
		if k, v, _ := strings.Cut(kv, "="); k == key {
			return v, true
		}
	}
	return "", false
}

// NewCLI writes a fake CLI named name. script is bash: it gets the
// adapter's arguments as "$@", its stdin, environment and working directory,
// and what it prints and its exit status are the CLI's. The recording
// doesn't touch the environment, so a script can't tell it apart from the
// real thing.
func NewCLI(t testing.TB, name, script string) *CLI {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "calls"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "script"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	d := shq(dir)
	wrapper := fmt.Sprintf(`#!/usr/bin/env bash
c=%[1]s/calls/$(date +%%s%%N)-$$
mkdir -p "$c"
printf '%%s\0' "$@" > "$c/args"
cat > "$c/stdin"
env -0 > "$c/env"
pwd > "$c/dir"
exec bash %[1]s/script "$@" < "$c/stdin"
`, d)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	return &CLI{Path: path, dir: dir}
}

// Calls returns every invocation so far, oldest first.
func (c *CLI) Calls(t testing.TB) []Call {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(c.dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var out []Call
	for _, n := range names {
		read := func(f string) []byte {
			b, err := os.ReadFile(filepath.Join(c.dir, "calls", n, f))
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		out = append(out, Call{
			Args:  split0(read("args")),
			Stdin: string(read("stdin")),
			Env:   split0(read("env")),
			Dir:   strings.TrimSpace(string(read("dir"))),
		})
	}
	return out
}

// Last returns the latest invocation, failing the test if there was none.
func (c *CLI) Last(t testing.TB) Call {
	t.Helper()
	calls := c.Calls(t)
	if len(calls) == 0 {
		t.Fatal("the fake CLI was never called")
	}
	return calls[len(calls)-1]
}

func split0(b []byte) []string {
	b = bytes.TrimSuffix(b, []byte{0})
	if len(b) == 0 {
		return nil
	}
	return strings.Split(string(b), "\x00")
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

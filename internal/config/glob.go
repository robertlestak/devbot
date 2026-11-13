package config

import (
	"path"
	"strings"
)

// Glob matches a slash-separated path against a pattern where "**" spans any
// number of directories and every other segment uses path.Match. A pattern
// without a slash also matches by base name anywhere in the tree, the way
// .gitignore does ("*.sql" matches "db/migrations/001.sql").
func Glob(pattern, name string) bool {
	pattern = strings.TrimPrefix(pattern, "/")
	name = strings.TrimPrefix(name, "/")
	if !strings.Contains(pattern, "/") && pattern != "**" {
		ok, _ := path.Match(pattern, path.Base(name))
		if ok {
			return true
		}
	}
	return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegs(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			if len(rest) == 0 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegs(rest, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

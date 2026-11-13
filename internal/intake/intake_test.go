package intake

import (
	"errors"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	repos := []string{"gitea:acme/app", "gitea:acme/lib"}
	long := strings.Repeat("x", MaxText+1)
	for name, c := range map[string]struct {
		raw   string
		ok    bool
		error string
	}{
		"draft":           {`{"draft":{"repo":"gitea:acme/app","title":"Fix it","acceptance_criteria":"- fixed","body":""}}`, true, ""},
		"project":         {`{"draft":{"repo":"gitea:acme/app","title":"Fix it","acceptance_criteria":"- fixed","body":"","project":{"plan":true,"repos":["gitea:acme/app","gitea:acme/lib"]}}}`, true, ""},
		"questions":       {`{"questions":["Which page?"]}`, true, ""},
		"another repo":    {`{"draft":{"repo":"gitea:acme/secret","title":"Fix it","acceptance_criteria":"- fixed","body":""}}`, false, "isn't one of the repos"},
		"plan elsewhere":  {`{"draft":{"repo":"gitea:acme/app","title":"t","acceptance_criteria":"- a","body":"","project":{"plan":true,"repos":["gitea:acme/secret"]}}}`, false, "isn't one of the repos"},
		"plan twice":      {`{"draft":{"repo":"gitea:acme/app","title":"t","acceptance_criteria":"- a","body":"","project":{"plan":true,"repos":["gitea:acme/app","gitea:acme/app"]}}}`, false, "twice"},
		"plan no repos":   {`{"draft":{"repo":"gitea:acme/app","title":"t","acceptance_criteria":"- a","body":"","project":{"plan":true}}}`, false, "1 to 10 repos"},
		"unknown key":     {`{"draft":{"repo":"gitea:acme/app","title":"t","acceptance_criteria":"- a","body":"","base":"main"}}`, false, "schema"},
		"two objects":     {`{"questions":["a"]} {"questions":["b"]}`, false, "more after"},
		"both":            {`{"draft":{"repo":"gitea:acme/app","title":"t","acceptance_criteria":"- a","body":""},"questions":["q"]}`, false, "both"},
		"neither":         {`{}`, false, "neither"},
		"title two lines": {`{"draft":{"repo":"gitea:acme/app","title":"a\nb","acceptance_criteria":"- a","body":""}}`, false, "title"},
		"long body":       {`{"draft":{"repo":"gitea:acme/app","title":"t","acceptance_criteria":"- a","body":"` + long + `"}}`, false, "longer than"},
		"many questions":  {`{"questions":["1","2","3","4","5","6"]}`, false, "more than 5"},
		"empty question":  {`{"questions":[" "]}`, false, "empty"},
	} {
		o, err := Parse([]byte(c.raw), repos)
		if c.ok != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.error)) {
			t.Errorf("%s: %+v %v", name, o, err)
		}
		if err != nil && strings.Contains(c.error, "schema") && !errors.Is(err, ErrSchema) {
			t.Errorf("%s: not ErrSchema: %v", name, err)
		}
	}
}

// A channel's default repo is named only when it is one of the requester's.
func TestContextDefault(t *testing.T) {
	r := Request{Requester: "ann", Request: "fix it", Repos: []Repo{{Repo: "gitea:acme/app"}, {Repo: "gitea:acme/lib"}}, Default: "gitea:acme/lib"}
	if c := r.Context(); !strings.Contains(c, "defaults to `gitea:acme/lib`") {
		t.Fatalf("the default should be named: %s", c)
	}
	r.Default = "gitea:acme/secret"
	if c := r.Context(); strings.Contains(c, "defaults to") || strings.Contains(c, "secret") {
		t.Fatalf("a default the requester can't direct devbot on must not be named: %s", c)
	}
}

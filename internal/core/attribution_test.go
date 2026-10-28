package core

import (
	"strings"
	"testing"

	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/forge"
)

func TestLabelValue(t *testing.T) {
	long := strings.Repeat("a", 70)
	cases := map[string]string{
		"alice":                    "alice",
		"anonymous/claude-desktop": "anonymous-claude-desktop",
		"devbot[bot]":              "devbot-bot",
		"-_.x y_.-":                "x-y",
		"":                         "",
		"日本":                       "",
	}
	for in, want := range cases {
		if got := LabelValue(in); got != want {
			t.Errorf("LabelValue(%q) = %q, want %q", in, got, want)
		}
	}
	a, b := LabelValue(long), LabelValue(long+"b")
	if len(a) > 63 || a == b || !strings.HasPrefix(a, strings.Repeat("a", 54)+"-") {
		t.Errorf("long values: %q, %q", a, b)
	}
	for _, v := range []string{a, b, LabelValue(strings.Repeat("a", 53) + "/" + long)} {
		if !config.ValidLabelValue(v) {
			t.Errorf("%q isn't a valid label value", v)
		}
	}
}

func TestAttributionRequester(t *testing.T) {
	repo := forge.RepoRef{Provider: "gitea", Owner: "acme", Name: "app"}
	it := forge.Item{Ref: forge.ItemRef{Repo: repo, Number: 7}, Author: "devbot"}
	for _, c := range []struct {
		meta Meta
		want string
	}{
		{Meta{}, "devbot"},
		{Meta{RequesterHandle: "bob"}, "bob"},
		{Meta{Requester: "alice", RequesterHandle: "bob", Origin: "chat:1"}, "alice"},
	} {
		if a := NewAttribution(it, c.meta, "implement", 0, nil); a.Requester != c.want {
			t.Errorf("meta %+v: requester %q, want %q", c.meta, a.Requester, c.want)
		}
	}
	a := Attribution{Repo: repo, Task: 7, Role: "review", Requester: "anonymous/x|y", Labels: map[string]string{"team": "payments"}}
	got := a.MetricValues([]string{"repo", "owner", "requester", "label_team", "label_missing"})
	if strings.Join(got, ",") != "gitea:acme/app,acme,anonymous/x_y,payments," {
		t.Errorf("metric values %q", got)
	}
}

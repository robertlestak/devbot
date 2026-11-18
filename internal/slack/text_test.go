package slack

import (
	"strings"
	"testing"
)

// hostile are model-written strings that try to reach people or disguise a
// link.
var hostile = []string{
	"<!channel> look",
	"<!here>",
	"<!everyone|everyone>",
	"ping <@U0123ABC> and <@W0456|bob>",
	"<!subteam^S0123|@oncall> wake up",
	"<https://evil.example/|https://git.example.com/acme/app/pulls/1>",
	"<mailto:a@b.c|click>",
	"@channel @here @Everyone",
	"&lt;!channel&gt;",
	"<<!channel>>",
}

// TestEscapeNeutralisesMentionsAndLinks: none of Slack's <...> forms
// survive, and no bare broadcast either.
func TestEscapeNeutralisesMentionsAndLinks(t *testing.T) {
	for _, s := range hostile {
		got := Escape(s)
		if strings.ContainsAny(got, "<>") {
			t.Errorf("Escape(%q) = %q: a < or > got through", s, got)
		}
		for _, b := range []string{"@channel", "@here", "@everyone"} {
			if strings.Contains(strings.ToLower(got), b) {
				t.Errorf("Escape(%q) = %q: %s got through", s, got, b)
			}
		}
	}
	// An entity in the text stays text, rather than becoming a < once
	// Slack decodes it.
	if got := Escape("&lt;!channel&gt;"); got != "&amp;lt;!channel&amp;gt;" {
		t.Errorf("an entity: %q", got)
	}
	// Plain text and email addresses read the same.
	if got := Escape("fix a@b.com's *bug* in main.go"); got != "fix a@b.com's *bug* in main.go" {
		t.Errorf("plain text: %q", got)
	}
}

func TestLink(t *testing.T) {
	cases := []struct{ url, label, want string }{
		{"https://git.example.com/acme/app/issues/1", "acme/app#1", "<https://git.example.com/acme/app/issues/1|acme/app#1>"},
		{"https://x.example/?a=1&b=2", "<!channel>", "<https://x.example/?a=1&amp;b=2|&lt;!channel&gt;>"},
		{"https://x.example/|<!channel>", "t", "t"},
		{"https://x.example/>", "t", "t"},
		{"javascript:alert(1)", "t", "t"},
		{"", "<@U1>", "&lt;@U1&gt;"},
	}
	for _, c := range cases {
		if got := Link(c.url, c.label); got != c.want {
			t.Errorf("Link(%q, %q) = %q, want %q", c.url, c.label, got, c.want)
		}
	}
}

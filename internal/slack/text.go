package slack

import (
	"regexp"
	"strings"
)

// escaper turns Slack's control characters into the entities Slack
// documents for them, so nothing in the text is read as Slack's <...>
// syntax: no <!channel>, <!here> or <!everyone> broadcast, no <@U...> or
// <!subteam^...> mention, no <url|label> link with a label of its own.
var escaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// broadcastRE is a bare @channel, @here or @everyone, which Slack also
// takes as a broadcast in a message posted with link_names. A word joiner after
// the @ keeps it reading the same.
var broadcastRE = regexp.MustCompile(`(?i)@(channel|here|everyone)\b`)

// Escape makes s plain text in a Slack message. Everything devbot posts
// that it didn't write itself (a model's question, summary or note, a
// title, a refusal) goes through it.
func Escape(s string) string {
	return broadcastRE.ReplaceAllString(escaper.Replace(s), "@\u2060$1")
}

// Link is a link to url labelled label, the label escaped. A url that
// isn't http(s), or that would break out of the link, is left out: only
// the label is shown.
func Link(url, label string) string {
	if (!strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://")) || strings.ContainsAny(url, "<>| \t\r\n") {
		return Escape(label)
	}
	return "<" + strings.ReplaceAll(url, "&", "&amp;") + "|" + Escape(label) + ">"
}

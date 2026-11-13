package slack

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"git.shdw.tech/rob/devbot/internal/queue"
)

// The buttons on a task's thread messages, by action_id. Each runs as the
// Slack user who clicks it, never as whoever filed the task: serve
// (server.slackButton) checks the task still is in a state the action
// applies to, then calls the same tool the clicker's MCP client would.
const (
	ActionAnswer  = "devbot_answer"
	ActionApprove = "devbot_approve"
	ActionRetry   = "devbot_retry"
	ActionCancel  = "devbot_cancel"
	ActionRebase  = "devbot_rebase"
)

// actions are the buttons a task in each state gets, in order. A state
// not listed (a closed task's, none) gets none.
var actions = map[string][]string{
	"ready":             {ActionCancel},
	"working":           {ActionCancel},
	"changes-requested": {ActionCancel},
	"needs-info":        {ActionAnswer, ActionCancel},
	"needs-approval":    {ActionApprove, ActionCancel},
	"needs-human":       {ActionRetry, ActionCancel},
	"in-review":         {ActionRebase, ActionCancel},
	"approved":          {ActionRebase, ActionCancel},
}

// IsAction reports whether id is one of the task buttons.
func IsAction(id string) bool {
	switch id {
	case ActionAnswer, ActionApprove, ActionRetry, ActionCancel, ActionRebase:
		return true
	}
	return false
}

// Applies reports whether action applies to a task in state: what decides
// both which buttons a message gets and whether a click is stale.
func Applies(action, state string) bool { return slices.Contains(actions[state], action) }

// Button is what a task button carries, as its value. Slack hands it back
// on a click from devbot's own message, signed; serve trusts none of it
// for permission, which is the clicker's.
type Button struct {
	Task string `json:"task"`
	// State is the task's state when the message was posted.
	State string `json:"state"`
	// SHA is, on Approve, the held commit when the message was posted: what
	// is approved, and only while devbot still holds it.
	SHA string `json:"sha,omitempty"`
	// Question is, on Answer, the question as the event summarised it: a
	// model's words, escaped wherever it is shown.
	Question string `json:"question,omitempty"`
}

// maxValue is the longest value Slack takes on a button.
const maxValue = 2000

// ParseButton reads a button's value.
func ParseButton(value string) (Button, bool) {
	var b Button
	if json.Unmarshal([]byte(value), &b) != nil || b.Task == "" {
		return Button{}, false
	}
	return b, true
}

func (b Button) value() string {
	enc := func(b Button) string {
		var buf bytes.Buffer
		e := json.NewEncoder(&buf)
		e.SetEscapeHTML(false)
		e.Encode(b)
		return strings.TrimSpace(buf.String())
	}
	v := enc(b)
	if len(v) > maxValue {
		// The modal then sends the person to the issue for the question.
		b.Question = ""
		v = enc(b)
	}
	return v
}

// labels are the buttons' texts.
var labels = map[string]string{
	ActionAnswer: "Answer", ActionApprove: "Approve…", ActionRetry: "Retry", ActionCancel: "Cancel", ActionRebase: "Rebase",
}

// taskButtons are the buttons for e's task in e's state: none for a task
// that ended. sha is the held commit, for Approve; without one, there is
// no Approve button.
func taskButtons(e queue.Event, sha string) []any {
	if e.Type == "task.merged" || e.Type == "task.cancelled" {
		return nil
	}
	var els []any
	for _, a := range actions[e.State] {
		b := Button{Task: e.Task, State: e.State}
		switch a {
		case ActionApprove:
			if sha == "" {
				continue
			}
			b.SHA = sha
		case ActionAnswer:
			b.Question = question(e)
		}
		el := map[string]any{"type": "button", "action_id": a, "value": b.value(),
			"text": map[string]any{"type": "plain_text", "text": labels[a]}}
		switch a {
		case ActionAnswer, ActionApprove, ActionRetry:
			el["style"] = "primary"
		case ActionCancel:
			el["style"] = "danger"
			el["confirm"] = map[string]any{
				"title":   map[string]any{"type": "plain_text", "text": "Cancel this task?"},
				"text":    map[string]any{"type": "mrkdwn", "text": "devbot stops working on " + Escape(e.Task) + ", as you. Its branch and any PR are left as they are."},
				"confirm": map[string]any{"type": "plain_text", "text": "Cancel the task"},
				"deny":    map[string]any{"type": "plain_text", "text": "Keep it"},
				"style":   "danger",
			}
		}
		els = append(els, el)
	}
	return els
}

// maxQuestion is the most of a question a button carries.
const maxQuestion = 280

// question is e's question as a button carries it: unescaped, cut short.
func question(e queue.Event) string {
	if e.Type != "task.question" {
		return ""
	}
	s := strings.TrimSpace(e.Summary)
	if utf8.RuneCountInString(s) > maxQuestion {
		s = string([]rune(s)[:maxQuestion]) + "…"
	}
	return s
}

// withButtons is text as blocks, with buttons under it: nil without any,
// for a plain message.
func withButtons(text string, buttons []any) json.RawMessage {
	if len(buttons) == 0 {
		return nil
	}
	b, _ := json.Marshal([]any{
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}},
		map[string]any{"type": "actions", "block_id": "devbot_task", "elements": buttons},
	})
	return b
}

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/slack"
)

// Redirecting a task from Slack (/devbot edit, wait and retarget), and a
// channel's default repo. Like the other commands, each acts as the caller
// through s.call, so it is allowed and refused exactly as their MCP call.
//
// A channel's default repo only saves picking one: it pre-fills the file
// and project forms and points the intake at it. Filing still checks the
// filer, and the intake only ever offers a requester their own repos, so
// it lets nobody do anything they couldn't do without it. Setting one
// takes the right to direct devbot on that repo all the same, so nobody
// can point a channel at a repo they have nothing to do with.

const (
	// editForm is the edit modal's callback_id.
	editForm = "devbot_edit"
	// slackMaxInput is the most characters Slack takes in a text input.
	slackMaxInput = 3000
)

// slackManageCommand handles the commands that redirect a task and set a
// channel's default repo, for c; handled is false for any other verb.
func (s *Server) slackManageCommand(ctx context.Context, cmd slack.Command, c core.Caller, verb, rest string) (reply any, handled bool) {
	ref := slackRef{Workspace: cmd.Workspace, User: cmd.User, Channel: cmd.Channel}
	words := strings.Fields(rest)
	switch verb {
	case "edit":
		if len(words) != 1 {
			return ephemeral("Usage: `/devbot edit <task>`, e.g. `/devbot edit " + s.exampleRef() + "`: a form to change its title, acceptance criteria or context."), true
		}
		return s.slackOpenEdit(ctx, cmd, c, words[0]), true
	case "wait":
		if len(words) < 2 {
			return ephemeral("Usage: `/devbot wait <task> <other task> [until tagged [pattern]]`, e.g. `/devbot wait " + s.exampleRef() + " #13`; `none` as the other task ends a wait."), true
		}
		// The rest is the wait as a /wait comment says it, which wait()
		// reads: the task, then any "until" and ", build against".
		args, _ := json.Marshal(refText{Ref: words[0], On: strings.Join(words[1:], " ")})
		return s.slackAnswer(ctx, cmd, func(ctx context.Context) *slackReply {
			return s.slackRedirect(ctx, c, ref, "wait_task", args)
		}), true
	case "retarget":
		if len(words) != 2 {
			return ephemeral("Usage: `/devbot retarget <task> <branch>`, e.g. `/devbot retarget " + s.exampleRef() + " release-2`."), true
		}
		args, _ := json.Marshal(refText{Ref: words[0], Base: words[1]})
		return s.slackAnswer(ctx, cmd, func(ctx context.Context) *slackReply {
			return s.slackRedirect(ctx, c, ref, "retarget", args)
		}), true
	case "channel":
		if len(words) == 0 || words[0] != "repo" || len(words) > 2 {
			return ephemeral("Usage: `/devbot channel repo [<repo>|none]`: this channel's default repo, set to a repo, or cleared."), true
		}
		return s.slackAnswer(ctx, cmd, func(ctx context.Context) *slackReply {
			return s.slackChannelRepoCommand(ctx, c, ref, words[1:])
		}), true
	}
	return nil, false
}

// slackRedirect calls tool as c and says what came of it, only to c.
func (s *Server) slackRedirect(ctx context.Context, c core.Caller, ref slackRef, tool string, args json.RawMessage) *slackReply {
	out, err := s.call(ctx, c, tool, args)
	if err != nil {
		return s.slackRefusal(ctx, ref, slackCommandsPath, err)
	}
	st, _ := out.(core.TaskStatus)
	what := "Retargeted"
	if tool == "wait_task" {
		what = "Set the wait of"
	}
	return ephemeral(fmt.Sprintf("%s %s as %s.\n%s", what, slackLink(st), slackAs(c), slackStatusText(st)))
}

// ---- the channel's default repo ----

func slackChannelRepoKey(ws slack.Workspace, channel string) string {
	return "devbot:slack:channel-repo:" + wsKey(ws) + ":" + channel
}

// slackChannelRepo is channel's default repo, or "" when it has none or it
// can't be read: a default is only a convenience.
func (s *Server) slackChannelRepo(ctx context.Context, ws slack.Workspace, channel string) string {
	if channel == "" {
		return ""
	}
	repo, err := s.E.Q.R.Get(ctx, slackChannelRepoKey(ws, channel)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		s.Log.Warn("slack: reading a channel's default repo", "workspace", ws.String(), "channel", channel, "err", err)
	}
	return repo
}

// slackChannelRepoCommand shows, sets or clears ref's channel's default
// repo. Setting one, or clearing one, takes the right to direct devbot on
// that repo, checked as filing there checks it.
func (s *Server) slackChannelRepoCommand(ctx context.Context, c core.Caller, ref slackRef, args []string) *slackReply {
	key := slackChannelRepoKey(ref.Workspace, ref.Channel)
	cur := s.slackChannelRepo(ctx, ref.Workspace, ref.Channel)
	failed := func(err error) *slackReply {
		s.Log.Warn("slack: a channel's default repo", "workspace", ref.Workspace.String(), "channel", ref.Channel, "err", err)
		return ephemeral("devbot couldn't change this channel's default repo. Try again.")
	}
	if len(args) == 0 {
		if cur == "" {
			return ephemeral("This channel has no default repo. `/devbot channel repo <repo>` sets one, which the file form and `@devbot` drafts here start from.")
		}
		return ephemeral("This channel's default repo is `" + slack.Escape(cur) + "`: the file form and `@devbot` drafts here start from it. `/devbot channel repo none` clears it.")
	}
	if args[0] == "none" {
		if cur == "" {
			return ephemeral("This channel has no default repo to clear.")
		}
		if _, err := s.E.MayDirect(callerContext(ctx, c), c, cur, "slack_channel_repo"); err != nil {
			return s.slackRefusal(ctx, ref, slackCommandsPath, err)
		}
		if err := s.E.Q.R.Del(ctx, key).Err(); err != nil {
			return failed(err)
		}
		return ephemeral("Cleared this channel's default repo, `" + slack.Escape(cur) + "`, as " + slackAs(c) + ".")
	}
	repo, err := s.E.MayDirect(callerContext(ctx, c), c, args[0], "slack_channel_repo")
	if err != nil {
		return s.slackRefusal(ctx, ref, slackCommandsPath, err)
	}
	if err := s.E.Q.R.Set(ctx, key, repo.String(), 0).Err(); err != nil {
		return failed(err)
	}
	return ephemeral("Set this channel's default repo to `" + slack.Escape(repo.String()) + "`, as " + slackAs(c) + ": the file form and `@devbot` drafts here start from it. " +
		"It is only a default: it lets nobody file or draft anywhere they couldn't without it.")
}

// slackForgetChannels drops the default repos of ws's channels.
func (s *Server) slackForgetChannels(ctx context.Context, ws slack.Workspace) error {
	iter := s.E.Q.R.Scan(ctx, 0, slackChannelRepoKey(ws, "*"), 200).Iterator()
	for iter.Next(ctx) {
		if err := s.E.Q.R.Del(ctx, iter.Val()).Err(); err != nil {
			return err
		}
	}
	return iter.Err()
}

// ---- the edit form ----

// slackEdit is the edit modal: what it opens with, and what was
// submitted.
type slackEdit struct {
	slackEditMeta
	Title, Body, AcceptanceCriteria, Note string
	// Error is why devbot refused the last submit, as mrkdwn.
	Error string
}

// slackEditMeta is the edit modal's private_metadata. Held is the hash of
// each field as the form opened with it: only a field submitted different
// is sent, so the form's copy of a field doesn't overwrite an edit made
// meanwhile by someone else. A field missing from it isn't in the form.
type slackEditMeta struct {
	Task    string            `json:"task"`
	URL     string            `json:"url,omitempty"`
	Channel string            `json:"channel,omitempty"`
	Held    map[string]string `json:"held,omitempty"`
	// Long are the fields left out for being longer than Slack takes, and
	// ByHand says the issue has no criteria of devbot's to edit.
	Long   []string `json:"long,omitempty"`
	ByHand bool     `json:"by_hand,omitempty"`
}

// editHash is a field's hash, line endings aside.
func editHash(s string) string {
	h := sha256.Sum256([]byte(strings.ReplaceAll(s, "\r\n", "\n")))
	return hex.EncodeToString(h[:8])
}

// newSlackEdit is the edit form for t, opened from channel. A field longer
// than Slack takes is left out of it, and so left as it is.
func newSlackEdit(t core.EditText, channel string) slackEdit {
	f := slackEdit{slackEditMeta: slackEditMeta{Task: t.Task, URL: t.URL, Channel: channel, Held: map[string]string{}, ByHand: !t.Criteria}}
	set := func(id, v string, to *string) {
		if utf8.RuneCountInString(v) > slackMaxInput {
			f.Long = append(f.Long, id)
			return
		}
		f.Held[id], *to = editHash(v), v
	}
	set("title", t.Title, &f.Title)
	if t.Criteria {
		set("acceptance_criteria", t.AcceptanceCriteria, &f.AcceptanceCriteria)
	}
	set("body", t.Body, &f.Body)
	return f
}

// slackEditFields are the edit form's fields, in its order.
var slackEditFields = []struct{ id, label, hint string }{
	{"title", "Title", ""},
	{"acceptance_criteria", "Acceptance criteria", "What done looks like, as checkable statements."},
	{"body", "Context", "What and why. Emptied, it is left as it is."},
}

func slackContext(text string) map[string]any {
	return map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn", "text": text}}}
}

// view is the edit form as a modal, as c fills it in.
func (f slackEdit) view(c core.Caller) json.RawMessage {
	meta, _ := json.Marshal(f.slackEditMeta)
	var blocks []any
	if f.Error != "" {
		blocks = append(blocks, mrkdwnSection(":warning: "+f.Error))
	}
	blocks = append(blocks, mrkdwnSection("*Edit "+slack.Link(f.URL, f.Task)+"*"))
	values := map[string]string{"title": f.Title, "acceptance_criteria": f.AcceptanceCriteria, "body": f.Body}
	var long []string
	for _, field := range slackEditFields {
		if slices.Contains(f.Long, field.id) {
			long = append(long, strings.ToLower(field.label))
		}
		if _, ok := f.Held[field.id]; ok {
			blocks = append(blocks, input(field.id, field.label, field.hint, field.id == "body", textInput(field.id != "title", values[field.id])))
		}
	}
	blocks = append(blocks, input("note", "Why", "One line, shown in devbot's comment on the issue.", true, textInput(false, f.Note)))
	if f.ByHand {
		blocks = append(blocks, slackContext("This issue has no acceptance criteria section of devbot's (it wasn't filed through devbot), so the context is its whole body."))
	}
	if len(long) > 0 {
		blocks = append(blocks, slackContext("The "+strings.Join(long, " and ")+" is longer than a Slack form takes, so it isn't here and stays as it is: edit it with `devbot edit` or the MCP's `edit_task`."))
	}
	blocks = append(blocks, slackContext("devbot edits the issue and works from the new text as approved by "+slackAs(c)+
		". A task with an open PR goes back to the implementer with what changed. Only the fields you change are sent."))
	v, _ := json.Marshal(map[string]any{"type": "modal", "callback_id": editForm, "private_metadata": string(meta),
		"title": plain("Edit a task"), "submit": plain("Save"), "close": plain("Cancel"), "blocks": blocks})
	return v
}

// editForm is what was submitted in v.
func (v slackView) editForm() slackEdit {
	var f slackEdit
	json.Unmarshal([]byte(v.PrivateMetadata), &f.slackEditMeta)
	value := func(id string) string { return v.State.Values[id][id].Value }
	f.Title, f.Body, f.AcceptanceCriteria, f.Note = value("title"), value("body"), value("acceptance_criteria"), value("note")
	return f
}

// request is the edit_task call f makes: the fields changed from what the
// form opened with, and the note. ok is false when nothing changed.
func (f slackEdit) request() (args json.RawMessage, ok bool) {
	req := core.EditRequest{Ref: f.Task, Note: strings.TrimSpace(f.Note)}
	changed := func(id, v string) string {
		if h, in := f.Held[id]; in && strings.TrimSpace(v) != "" && editHash(v) != h {
			return v
		}
		return ""
	}
	req.Title = changed("title", f.Title)
	req.Body = changed("body", f.Body)
	req.AcceptanceCriteria = changed("acceptance_criteria", f.AcceptanceCriteria)
	args, _ = json.Marshal(req)
	return args, req.Title != "" || req.Body != "" || req.AcceptanceCriteria != "" || req.Note != ""
}

// slackOpenEdit opens the edit form for task, at once, while Slack's
// trigger is fresh, and fills it in once devbot has read the task as c.
// A task c can't read, or that can't be edited, is refused as edit_task
// refuses it, in the modal.
func (s *Server) slackOpenEdit(ctx context.Context, cmd slack.Command, c core.Caller, task string) any {
	ref := slackRef{Workspace: cmd.Workspace, User: cmd.User, Channel: cmd.Channel}
	bot, err := s.Slack.Bot(ctx, cmd.Workspace)
	var viewID string
	if err == nil {
		viewID, err = bot.OpenView(ctx, cmd.TriggerID, slackNotice("Edit a task", "Reading "+slack.Escape(task)+"…"))
	}
	if err != nil {
		s.Log.Warn("slack: opening a form", "workspace", cmd.Workspace.String(), "err", err)
		return ephemeral("devbot couldn't open the form. Try again.")
	}
	s.slackGo(ctx, func(ctx context.Context) {
		var view json.RawMessage
		t, err := s.E.EditableText(callerContext(ctx, c), c, task)
		if err != nil {
			r := s.slackRefusal(ctx, ref, slackCommandsPath, err)
			if len(r.Blocks) > 0 {
				// The connect button only works as a message.
				s.slackTell(ctx, ref, r.Text, r.Blocks)
			}
			view = slackNotice("Edit a task", r.Text)
		} else {
			view = newSlackEdit(t, cmd.Channel).view(c)
		}
		if err := bot.UpdateView(ctx, viewID, "", view); err != nil {
			s.Log.Warn("slack: filling in a modal", "workspace", cmd.Workspace.String(), "err", err)
		}
	})
	return nil
}

// slackSubmitEdit edits the task as the user who submitted the form. A
// refusal in time shows the form again, as it was, under the refusal; one
// after Slack stopped waiting comes as an ephemeral message, as does what
// was done.
func (s *Server) slackSubmitEdit(ctx context.Context, in slack.Interaction, v slackView) (any, error) {
	f := v.editForm()
	ref := slackRef{Workspace: in.Workspace, User: in.User, Channel: f.Channel}
	c, prompt, err := s.slackCaller(ctx, in.Workspace, in.User, f.Channel)
	if err != nil {
		prompt = s.slackFailed(in.Workspace, err)
	}
	if prompt != nil {
		if len(prompt.Blocks) > 0 {
			s.slackTell(ctx, ref, prompt.Text, prompt.Blocks)
		}
		return map[string]any{"response_action": "update", "view": slackNotice("Edit a task", prompt.Text)}, nil
	}
	args, ok := f.request()
	if !ok {
		f.Error = "You changed nothing: change a field, or cancel."
		return map[string]any{"response_action": "update", "view": f.view(c)}, nil
	}
	type outcome struct {
		st  core.TaskStatus
		err error
	}
	done := func(ctx context.Context, o outcome) {
		if o.err != nil {
			r := s.slackRefusal(ctx, ref, slackInteractionPath, o.err)
			s.slackTell(ctx, ref, r.Text, r.Blocks)
			return
		}
		text := fmt.Sprintf("Edited %s as %s.", slackLink(o.st), slackAs(c))
		if o.st.Note != "" {
			text += " " + slack.Escape(o.st.Note)
		}
		s.slackTell(ctx, ref, text, nil)
	}
	o, ok := slackSoon(s, ctx, func(ctx context.Context) outcome {
		out, err := s.call(ctx, c, "edit_task", args)
		st, _ := out.(core.TaskStatus)
		return outcome{st: st, err: err}
	}, done)
	if !ok {
		return nil, nil
	}
	if o.err == nil {
		done(ctx, o)
		return nil, nil
	}
	r := s.slackRefusal(ctx, ref, slackInteractionPath, o.err)
	if len(r.Blocks) > 0 {
		s.slackTell(ctx, ref, r.Text, r.Blocks)
	}
	f.Error = r.Text
	return map[string]any{"response_action": "update", "view": f.view(c)}, nil
}

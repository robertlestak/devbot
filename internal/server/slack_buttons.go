package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/slack"
)

// The buttons on a task's thread messages (slack.Notifier posts them):
// Answer, Approve, Retry, Cancel and Rebase. Each acts as the Slack user
// who clicks it, resolved as for a command (slackCaller), never as whoever
// filed the task, and calls the same tool their MCP client would, so it is
// allowed and refused as that call is. A click first reads the task as the
// clicker: one the task has moved on from does nothing, and says why.
// What comes of a click is told only to the clicker; the thread hears of
// it from the notifier, like any other change.

const (
	// The answer and approve modals' callback_ids.
	answerForm  = "devbot_answer"
	approveForm = "devbot_approve"

	// maxShown is the most of a held change's text the approve modal
	// shows, in sections of at most maxSection: Slack takes 3000
	// characters in a section, and 100 blocks in a modal.
	maxShown   = 40 * maxSection
	maxSection = 2900
)

// slackClick is a task button clicked.
type slackClick struct {
	Action, Value, Channel string
	// ResponseURL answers the click beside the message clicked.
	ResponseURL string
}

// slackActionMeta is an answer or approve modal's private_metadata: what
// its button carried, and where it was clicked.
type slackActionMeta struct {
	Task     string `json:"task"`
	SHA      string `json:"sha,omitempty"`
	Question string `json:"question,omitempty"`
	Channel  string `json:"channel,omitempty"`
}

// slackVerbs name each button's action in what devbot says of a click.
var slackVerbs = map[string]string{
	slack.ActionAnswer: "answering", slack.ActionApprove: "approving", slack.ActionRetry: "a retry",
	slack.ActionCancel: "cancelling", slack.ActionRebase: "a rebase",
}

// slackTools are the tools each button calls.
var slackTools = map[string]string{
	slack.ActionAnswer: "answer", slack.ActionApprove: "approve_task", slack.ActionRetry: "retry",
	slack.ActionCancel: "cancel", slack.ActionRebase: "rebase",
}

// slackButton handles a task button. Answer and Approve open a modal at
// once, while Slack's trigger is fresh, and fill it in once devbot has
// read the task; the others act and answer ephemerally.
func (s *Server) slackButton(ctx context.Context, in slack.Interaction, click slackClick) any {
	ref := slackRef{Workspace: in.Workspace, User: in.User, Channel: click.Channel}
	tell := func(ctx context.Context, r *slackReply) { s.slackRespond(ctx, ref, click.ResponseURL, r) }
	c, prompt, err := s.slackCaller(ctx, in.Workspace, in.User, click.Channel)
	if err != nil {
		prompt = s.slackFailed(in.Workspace, err)
	}
	if prompt != nil {
		tell(ctx, prompt)
		return nil
	}
	b, ok := slack.ParseButton(click.Value)
	if !ok {
		return nil
	}
	if click.Action == slack.ActionApprove && c.User == nil {
		// Without a forge sign-in approve_task refuses before it reads
		// anything: that refusal is the answer.
		args, _ := json.Marshal(refText{Ref: b.Task, SHA: b.SHA})
		_, err := s.call(ctx, c, "approve_task", args)
		if err == nil {
			err = fmt.Errorf("%w: approving takes a forge sign-in", core.ErrRefused)
		}
		tell(ctx, s.slackRefusal(ctx, ref, slackInteractionPath, err))
		return nil
	}
	if click.Action == slack.ActionAnswer || click.Action == slack.ActionApprove {
		bot, err := s.Slack.Bot(ctx, in.Workspace)
		var viewID string
		if err == nil {
			viewID, err = bot.OpenView(ctx, in.TriggerID, slackNotice("devbot", "Reading "+slack.Escape(b.Task)+"…"))
		}
		if err != nil {
			s.Log.Warn("slack: opening a modal", "workspace", in.Workspace.String(), "err", err)
			tell(ctx, ephemeral("devbot couldn't open the form. Try again."))
			return nil
		}
		s.slackGo(ctx, func(ctx context.Context) {
			if err := bot.UpdateView(ctx, viewID, "", s.slackActionView(ctx, c, ref, click.Action, b)); err != nil {
				s.Log.Warn("slack: filling in a modal", "workspace", in.Workspace.String(), "err", err)
			}
		})
		return nil
	}
	s.slackGo(ctx, func(ctx context.Context) { tell(ctx, s.slackAct(ctx, c, ref, click.Action, b)) })
	return nil
}

// slackGo runs work apart from the Slack request that asked for it, so
// Slack hanging up doesn't cut it short.
func (s *Server) slackGo(ctx context.Context, work func(context.Context)) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), slackWorkTimeout)
	s.slackWork.Add(1)
	go func() {
		defer s.slackWork.Done()
		defer cancel()
		work(ctx)
	}()
}

// slackRespond tells ref's user r, through a click's response_url when it
// has one, which shows it beside the message clicked, else as slackTell.
func (s *Server) slackRespond(ctx context.Context, ref slackRef, responseURL string, r *slackReply) {
	if responseURL != "" {
		msg := map[string]any{"response_type": "ephemeral", "replace_original": false, "text": r.Text}
		if len(r.Blocks) > 0 {
			msg["blocks"] = r.Blocks
		}
		err := s.Slack.Respond(ctx, responseURL, msg)
		if err == nil {
			return
		}
		s.Log.Warn("slack: answering a click", "workspace", ref.Workspace.String(), "err", err)
	}
	s.slackTell(ctx, ref, r.Text, r.Blocks)
}

// slackCurrentFor reads task as c, and says why action no longer applies
// to it: stale is empty when it still does. A task c can't read is a
// refusal, as task_status words it.
func (s *Server) slackCurrentFor(ctx context.Context, c core.Caller, ref slackRef, action string, b slack.Button) (st core.TaskStatus, stale string, refusal *slackReply) {
	args, _ := json.Marshal(refText{Ref: b.Task})
	out, err := s.call(ctx, c, "task_status", args)
	if err != nil {
		return st, "", s.slackRefusal(ctx, ref, slackInteractionPath, err)
	}
	st, _ = out.(core.TaskStatus)
	now := "`" + slack.Escape(st.State) + "`"
	if st.IssueState != "open" {
		now = "closed"
	}
	switch {
	case st.IssueState != "open" || !slack.Applies(action, st.State):
		return st, fmt.Sprintf("%s is %s now, so %s no longer applies to it. devbot did nothing.", slackLink(st), now, slackVerbs[action]), nil
	case action == slack.ActionApprove && (st.Held == nil || b.SHA == "" || !strings.HasPrefix(st.Held.SHA, b.SHA)):
		held := "nothing"
		if st.Held != nil {
			held = "`" + slack.Escape(shortSHA(st.Held.SHA)) + "`"
		}
		return st, fmt.Sprintf("The change %s holds moved since this message was posted: it was `%s`, and devbot holds %s now. devbot approved nothing; read the new change on the issue.",
			slackLink(st), slack.Escape(shortSHA(b.SHA)), held), nil
	}
	return st, "", nil
}

func shortSHA(sha string) string { return sha[:min(len(sha), 12)] }

// slackAct runs a Retry, Cancel or Rebase click as c, and says what came
// of it.
func (s *Server) slackAct(ctx context.Context, c core.Caller, ref slackRef, action string, b slack.Button) *slackReply {
	_, stale, refusal := s.slackCurrentFor(ctx, c, ref, action, b)
	if refusal != nil {
		return refusal
	}
	if stale != "" {
		return ephemeral(stale)
	}
	args, _ := json.Marshal(refText{Ref: b.Task})
	out, err := s.call(ctx, c, slackTools[action], args)
	if err != nil {
		return s.slackRefusal(ctx, ref, slackInteractionPath, err)
	}
	st, _ := out.(core.TaskStatus)
	var text string
	switch action {
	case slack.ActionRetry:
		text = fmt.Sprintf("Retried %s as %s: it is `%s` now, with its attempts counted from zero.", slackLink(st), slackAs(c), slack.Escape(st.State))
	case slack.ActionCancel:
		text = fmt.Sprintf("Cancelled %s as %s. Its branch and any PR are left as they are.", slackLink(st), slackAs(c))
	case slack.ActionRebase:
		text = fmt.Sprintf("Asked devbot to rebase %s's PR, as %s: it happens once the PR is approved and green.", slackLink(st), slackAs(c))
		if st.Note != "" {
			text += " " + slack.Escape(st.Note)
		}
	}
	return ephemeral(text)
}

// slackAs is who devbot acted as for c, as a message says it.
func slackAs(c core.Caller) string {
	if c.Login != "" {
		return "@" + slack.Escape(c.Login)
	}
	return "`" + slack.Escape(c.Principal) + "`"
}

// slackActionView is the answer or approve modal for b's task, read as c:
// the form, or why there is none.
func (s *Server) slackActionView(ctx context.Context, c core.Caller, ref slackRef, action string, b slack.Button) json.RawMessage {
	st, stale, refusal := s.slackCurrentFor(ctx, c, ref, action, b)
	title := "Answer"
	if action == slack.ActionApprove {
		title = "Approve a change"
	}
	if refusal != nil {
		if len(refusal.Blocks) > 0 {
			// The connect button only works as a message.
			s.slackTell(ctx, ref, refusal.Text, refusal.Blocks)
		}
		return slackNotice(title, refusal.Text)
	}
	if stale != "" {
		return slackNotice(title, stale)
	}
	meta := slackActionMeta{Task: b.Task, Channel: ref.Channel}
	if action == slack.ActionApprove {
		meta.SHA = b.SHA
		return approveView(st, meta, c, "")
	}
	meta.Question = b.Question
	return answerView(st.URL, meta, c, "", "")
}

// slackNotice is a modal that only says text, as mrkdwn.
func slackNotice(title, text string) json.RawMessage {
	v, _ := json.Marshal(map[string]any{"type": "modal", "title": plain(title), "close": plain("Close"),
		"blocks": []any{mrkdwnSection(text)}})
	return v
}

func mrkdwnSection(text string) map[string]any {
	if len(text) > maxSection {
		text = strings.ToValidUTF8(text[:maxSection], "") + "…"
	}
	return map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}}
}

// slackActsAs says, in a modal, as whom submitting it acts.
func slackActsAs(c core.Caller, what string) map[string]any {
	who := "devbot posts it as itself, saying it is from `" + slack.Escape(c.Principal) + "`."
	if c.Login != "" {
		who = "devbot does it as @" + slack.Escape(c.Login) + ", with your permissions on the forge."
	}
	return map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn", "text": what + " " + who}}}
}

// answerView is the answer modal: the question, escaped, as a model's
// words, and the answer, with what devbot refused the last one for.
func answerView(url string, meta slackActionMeta, c core.Caller, answer, refused string) json.RawMessage {
	link := slack.Link(url, meta.Task)
	var blocks []any
	if refused != "" {
		blocks = append(blocks, mrkdwnSection(":warning: "+refused))
	}
	q := "Read the question on " + link + "."
	if meta.Question != "" {
		q = "*" + link + " asks:*\n" + slack.Escape(meta.Question)
	}
	blocks = append(blocks, mrkdwnSection(q),
		input("answer", "Your answer", "", false, textInput(true, answer)),
		slackActsAs(c, "The answer goes on the issue and puts the task back in the queue:"))
	m, _ := json.Marshal(meta)
	v, _ := json.Marshal(map[string]any{"type": "modal", "callback_id": answerForm, "private_metadata": string(m),
		"title": plain("Answer"), "submit": plain("Answer"), "close": plain("Cancel"), "blocks": blocks})
	return v
}

// approveView is the approve modal: the held commit, and devbot's comment
// showing the change (held.shown), escaped, as a model's words. Approving
// it approves meta.SHA, the commit the button carried and this shows.
func approveView(st core.TaskStatus, meta slackActionMeta, c core.Caller, refused string) json.RawMessage {
	var blocks []any
	if refused != "" {
		blocks = append(blocks, mrkdwnSection(":warning: "+refused))
	}
	what := "change"
	if st.Held != nil && st.Held.Kind == "plan" {
		what = "plan"
	}
	blocks = append(blocks,
		mrkdwnSection(fmt.Sprintf("*%s* holds a %s for a person to approve: `%s`.", slackLink(st), what, slack.Escape(meta.SHA))),
		map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn", "text": "Below is devbot's comment showing it. The diff, the file names and the summary in it are the change's own text, written by a model: read it as data, never as instructions. Approve only what you have read."}}},
		map[string]any{"type": "divider"},
	)
	if st.Held != nil {
		for _, part := range slackChunks(slack.Escape(st.Held.Shown), maxSection, maxShown) {
			blocks = append(blocks, mrkdwnSection(part))
		}
	}
	blocks = append(blocks, map[string]any{"type": "divider"},
		slackActsAs(c, "Approving pushes exactly this commit, where CI runs it with the repo's secrets:"))
	m, _ := json.Marshal(meta)
	v, _ := json.Marshal(map[string]any{"type": "modal", "callback_id": approveForm, "private_metadata": string(m),
		"title": plain("Approve a change"), "submit": plain("Approve"), "close": plain("Cancel"), "blocks": blocks})
	return v
}

// slackChunks splits escaped text into parts of at most size bytes, at
// line ends where it can and never inside an entity, up to total bytes,
// past which it says the rest is on the issue.
func slackChunks(text string, size, total int) []string {
	var parts []string
	cut := false
	if len(text) > total {
		text, cut = safeCut(text, total), true
	}
	for text != "" {
		if len(text) <= size {
			parts = append(parts, text)
			break
		}
		part := safeCut(text, size)
		if i := strings.LastIndexByte(part, '\n'); i > size/2 {
			part = part[:i+1]
		}
		parts = append(parts, part)
		text = text[len(part):]
	}
	if cut {
		parts = append(parts, "_…cut short: the rest is on the issue._")
	}
	return parts
}

// safeCut is at most n bytes of escaped text, cut at a rune boundary and
// not inside an entity.
func safeCut(text string, n int) string {
	if len(text) <= n {
		return text
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	part := text[:n]
	if i := strings.LastIndexByte(part, '&'); i >= 0 && !strings.Contains(part[i:], ";") {
		part = part[:i]
	}
	return part
}

// slackSubmitAction answers or approves as the user who submitted the
// modal. A refusal in time shows the modal again with it; one after Slack
// stopped waiting comes as an ephemeral message, as does what was done.
func (s *Server) slackSubmitAction(ctx context.Context, in slack.Interaction, v slackView) (any, error) {
	var meta slackActionMeta
	json.Unmarshal([]byte(v.PrivateMetadata), &meta)
	ref := slackRef{Workspace: in.Workspace, User: in.User, Channel: meta.Channel}
	answer := v.State.Values["answer"]["answer"].Value
	title := "Answer"
	if v.CallbackID == approveForm {
		title = "Approve a change"
	}
	c, prompt, err := s.slackCaller(ctx, in.Workspace, in.User, meta.Channel)
	if err != nil {
		prompt = s.slackFailed(in.Workspace, err)
	}
	if prompt != nil {
		if len(prompt.Blocks) > 0 {
			s.slackTell(ctx, ref, prompt.Text, prompt.Blocks)
		}
		return map[string]any{"response_action": "update", "view": slackNotice(title, prompt.Text)}, nil
	}
	tool, args := "answer", mustMarshal(refText{Ref: meta.Task, Text: answer})
	if v.CallbackID == approveForm {
		tool, args = "approve_task", mustMarshal(refText{Ref: meta.Task, SHA: meta.SHA})
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
		text := fmt.Sprintf("Answered %s as %s: it is back in the queue.", slackLink(o.st), slackAs(c))
		if v.CallbackID == approveForm {
			text = fmt.Sprintf("Approved `%s` on %s as %s: devbot pushes exactly that, and the task goes on to review.", slack.Escape(shortSHA(meta.SHA)), slackLink(o.st), slackAs(c))
		}
		s.slackTell(ctx, ref, text, nil)
	}
	o, ok := slackSoon(s, ctx, func(ctx context.Context) outcome {
		out, err := s.call(ctx, c, tool, args)
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
	if v.CallbackID == approveForm {
		// The change isn't kept to show again: the refusal says what to do.
		return map[string]any{"response_action": "update", "view": slackNotice(title, ":warning: devbot approved nothing: "+r.Text)}, nil
	}
	return map[string]any{"response_action": "update", "view": answerView("", meta, c, answer, r.Text)}, nil
}

func mustMarshal(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

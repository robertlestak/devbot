package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"git.shdw.tech/rob/devbot/internal/auth"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/queue"
	"git.shdw.tech/rob/devbot/internal/slack"
)

// The Slack app in serve: who a Slack request is for, and the command,
// interactions and events that link Slack users to their forge sign-ins
// and unlink them.
//
// In auth.mode oauth a Slack user is linked to a devbot sign-in of their
// forge account (auth.Link), and what they do in Slack is done as that
// forge user, through the same call path as their MCP calls. Until they
// link, every action of theirs only gets them a button to. In mode none,
// with slack.allow_anonymous, nobody links: each Slack user is the
// anonymous principal, labelled with who they are in Slack.

// connectAction is the connect button's action_id.
const connectAction = "devbot_connect"

// wireSlack registers serve's handlers with the Slack app, and with the
// OAuth broker for links made.
func (s *Server) wireSlack() {
	s.Slack.OnCommand(s.slackCommand)
	s.Slack.OnInteraction(s.slackInteraction)
	s.Slack.OnEvent("user_change", s.slackUserChange)
	// @devbot in a channel, and a message to the app: the intake.
	s.Slack.OnEvent("app_mention", s.slackMention)
	s.Slack.OnEvent("message", s.slackMention)
	s.Slack.OnUninstall(func(ctx context.Context, ws slack.Workspace) error {
		if err := s.slackForgetIntakes(ctx, ws); err != nil {
			return err
		}
		if err := s.slackForgetChannels(ctx, ws); err != nil {
			return err
		}
		if s.Auth == nil {
			return nil
		}
		return s.Auth.UnlinkGroup(ctx, slackGroup(ws))
	})
	if s.Auth != nil {
		s.Auth.OnLink(s.slackLinked)
		// A Slack user proves who they are with Sign in with Slack before a
		// link to them reaches the forge.
		s.Auth.ProveLinks("slack", s.Slack)
	}
}

// slackSubject names a Slack user's link: one per user and workspace.
func slackSubject(ws slack.Workspace, user string) string { return slack.Subject(ws, user) }

// slackGroup names a workspace's links, which its uninstall removes.
func slackGroup(ws slack.Workspace) string { return "slack:" + ws.Enterprise + ":" + ws.Team }

// slackLabel is a Slack user's label on the anonymous principal.
func slackLabel(ws slack.Workspace, user string) string {
	team := ws.Team
	if team == "" {
		team = ws.Enterprise
	}
	return "slack-" + team + "-" + user
}

// slackRef is where a link was asked for, so its confirmation goes there.
type slackRef struct {
	Workspace slack.Workspace `json:"ws"`
	User      string          `json:"user"`
	Channel   string          `json:"channel,omitempty"`
	// Thread is the thread in Channel to tell the user in, empty for the
	// channel itself.
	Thread string `json:"thread,omitempty"`
}

// slackReply is a message only the user it answers sees.
type slackReply struct {
	ResponseType string `json:"response_type"`
	Text         string `json:"text"`
	Blocks       []any  `json:"blocks,omitempty"`
}

func ephemeral(text string) *slackReply { return &slackReply{ResponseType: "ephemeral", Text: text} }

// slackCaller resolves who a Slack request from user, in ws's channel, is
// for. Every Slack handler asks it before acting. With a non-nil prompt
// there is nobody to act as: the handler answers with the prompt, only to
// that user, and does nothing else.
//
// In auth.mode oauth the caller is the forge user the Slack user linked,
// exactly as signedIn makes it for their MCP calls. Unlinked, or no longer
// (the forge refused their sign-in, which ends the link), the prompt is a
// button to link. In mode none the caller is the anonymous principal
// labelled with the Slack user.
func (s *Server) slackCaller(ctx context.Context, ws slack.Workspace, user, channel string) (c core.Caller, prompt *slackReply, err error) {
	if s.Auth == nil {
		if s.Cfg.AnonymousPrincipal == "" {
			return core.Caller{}, ephemeral("devbot doesn't take requests: its connectors are off (`anonymous_principal` is empty)."), nil
		}
		label := slackLabel(ws, user)
		if !config.ValidName(label) {
			return core.Caller{}, nil, fmt.Errorf("%w: Slack label %q doesn't match %s", errBadName, label, config.NameRule)
		}
		return core.Caller{Principal: s.Cfg.AnonymousPrincipal + "/" + label}, nil, nil
	}
	id, err := s.Auth.Linked(ctx, slackSubject(ws, user))
	if err == nil {
		c, err = s.signedIn(ctx, id)
		if err == nil {
			return c, nil, nil
		}
	}
	if !errors.Is(err, auth.ErrNotLinked) && !errors.Is(err, auth.ErrInvalidToken) {
		return core.Caller{}, nil, err
	}
	prompt, err = s.connectPrompt(ctx, ws, user, channel)
	return core.Caller{}, prompt, err
}

// connectPrompt is the button that links user to their forge account, with
// a URL that works once, for that user only.
func (s *Server) connectPrompt(ctx context.Context, ws slack.Workspace, user, channel string) (*slackReply, error) {
	reply, _ := json.Marshal(slackRef{Workspace: ws, User: user, Channel: channel})
	u, err := s.Auth.LinkURL(ctx, auth.Link{Subject: slackSubject(ws, user), Group: slackGroup(ws), Who: s.slackWho(ctx, ws, user), Reply: string(reply)})
	if err != nil {
		return nil, err
	}
	forgeName := s.Cfg.Auth.Provider
	text := "Connect your " + forgeName + " account to use devbot from Slack. devbot then acts as you on " + forgeName + ", with your permissions there."
	return &slackReply{ResponseType: "ephemeral", Text: text, Blocks: []any{
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}},
		map[string]any{"type": "actions", "elements": []any{map[string]any{"type": "button", "action_id": connectAction, "style": "primary", "url": u,
			"text": map[string]any{"type": "plain_text", "text": "Connect your forge account"}}}},
		map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn", "text": "The button works once, within 10 minutes, and only for you: you sign in with Slack first, to show it is you."}}},
	}}, nil
}

// slackWho names a Slack user, and their workspace, on the consent page:
// what Slack calls them when it can say, and always their IDs.
func (s *Server) slackWho(ctx context.Context, ws slack.Workspace, user string) string {
	who, where := user, ws.String()
	if in, err := s.Slack.Installation(ctx, ws); err == nil && in.TeamName != "" {
		where = in.TeamName + " (" + ws.String() + ")"
	}
	if bot, err := s.Slack.Bot(ctx, ws); err == nil {
		if u, err := bot.UserInfo(ctx, user); err == nil && (u.RealName != "" || u.Name != "") {
			name := u.RealName
			if u.Name != "" {
				name = strings.TrimSpace(name + " @" + u.Name)
			}
			who = name + " (" + user + ")"
		}
	}
	return "the Slack user " + who + " in the workspace " + where
}

// slackLinked confirms a link, only to the Slack user it was made for, in
// the channel they asked from, or in their messages with the app when the
// bot isn't in that channel (slackTell).
func (s *Server) slackLinked(ctx context.Context, l auth.Link, login string) {
	var ref slackRef
	if !strings.HasPrefix(l.Subject, "slack:") || json.Unmarshal([]byte(l.Reply), &ref) != nil || ref.User == "" {
		return
	}
	text := "You're connected: devbot acts as @" + slack.Escape(login) + " on " + s.Cfg.Auth.Provider + " for what you do with it here. `/devbot disconnect` undoes it."
	s.slackTell(ctx, ref, text, nil)
}

// slackCommand handles /devbot. Without arguments it says who the caller
// is to devbot; disconnect unlinks them, help lists the commands, and the
// rest (slackTasksCommand, slackManageCommand) act as the caller.
func (s *Server) slackCommand(ctx context.Context, cmd slack.Command) (any, error) {
	verb, rest, _ := strings.Cut(cmd.Text, " ")
	switch verb {
	case "disconnect":
		return s.slackDisconnect(ctx, cmd), nil
	case "help":
		return ephemeral(slackHelp), nil
	}
	c, prompt, err := s.slackCaller(ctx, cmd.Workspace, cmd.User, cmd.Channel)
	if err != nil {
		return s.slackFailed(cmd.Workspace, err), nil
	}
	if prompt != nil {
		return prompt, nil
	}
	if reply, ok := s.slackTasksCommand(ctx, cmd, c, verb, rest); ok {
		return reply, nil
	}
	if reply, ok := s.slackManageCommand(ctx, cmd, c, verb, rest); ok {
		return reply, nil
	}
	who := "You use devbot as `" + c.Principal + "`."
	if c.Login != "" {
		who = "You use devbot as @" + slack.Escape(c.Login) + " on " + c.Provider + ". `/devbot disconnect` unlinks your Slack account from it."
	}
	if verb != "" {
		who = "devbot has no `/devbot " + slack.Escape(verb) + "` command; `/devbot help` lists them. " + who
	}
	return ephemeral(who), nil
}

// slackFailed is the answer to a request devbot couldn't resolve a caller
// for: the name devbot won't record, or only that something failed.
func (s *Server) slackFailed(ws slack.Workspace, err error) *slackReply {
	if errors.Is(err, errBadName) {
		return ephemeral("devbot can't act for you: " + slack.Escape(err.Error()))
	}
	s.Log.Warn("slack: resolving the caller", "workspace", ws.String(), "err", err)
	return ephemeral("devbot couldn't check who you are. Try again.")
}

// slackDisconnect ends the caller's link and the sign-in behind it.
func (s *Server) slackDisconnect(ctx context.Context, cmd slack.Command) *slackReply {
	if s.Auth == nil {
		return ephemeral("devbot runs without sign-in here, so there is nothing to disconnect.")
	}
	login, err := s.Auth.Unlink(ctx, slackSubject(cmd.Workspace, cmd.User))
	switch {
	case errors.Is(err, auth.ErrNotLinked):
		return ephemeral("Your Slack account isn't connected to devbot.")
	case err != nil:
		s.Log.Warn("slack: unlinking", "workspace", cmd.Workspace.String(), "err", err)
		return ephemeral("devbot couldn't disconnect you. Try again.")
	}
	return ephemeral("Disconnected: devbot no longer acts as @" + slack.Escape(login) + " on " + s.Cfg.Auth.Provider + " for you. `/devbot` connects you again.")
}

// slackInteraction handles a button, shortcut or modal: a repo picker's
// search, the file shortcut on a message, the file, project and edit forms
// submitted, a task's buttons and the answer and approve modals they
// open, and an intake draft's buttons. Otherwise only the caller's
// resolution happens yet: an unlinked user is sent the connect prompt.
func (s *Server) slackInteraction(ctx context.Context, in slack.Interaction) (any, error) {
	var p struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
		Actions []struct {
			ActionID string `json:"action_id"`
			Value    string `json:"value"`
		} `json:"actions"`
		ResponseURL string `json:"response_url"`
		// A message shortcut's.
		CallbackID string `json:"callback_id"`
		Message    struct {
			TS       string `json:"ts"`
			ThreadTS string `json:"thread_ts"`
		} `json:"message"`
		// A block_suggestion's search.
		Value string    `json:"value"`
		View  slackView `json:"view"`
	}
	json.Unmarshal(in.Raw, &p)
	switch {
	case in.Type == "block_suggestion":
		return s.slackRepoChoices(ctx, in, p.Value), nil
	case in.Type == "message_action" && p.CallbackID == fileShortcut:
		return s.slackFileShortcut(ctx, in, slackShortcut{Channel: p.Channel.ID, TS: p.Message.TS, ThreadTS: p.Message.ThreadTS, ResponseURL: p.ResponseURL}), nil
	case in.Type == "view_submission" && (p.View.CallbackID == fileForm || p.View.CallbackID == projectForm):
		return s.slackSubmit(ctx, in, p.View)
	case in.Type == "view_submission" && (p.View.CallbackID == answerForm || p.View.CallbackID == approveForm):
		return s.slackSubmitAction(ctx, in, p.View)
	case in.Type == "view_submission" && p.View.CallbackID == editForm:
		return s.slackSubmitEdit(ctx, in, p.View)
	case in.Type == "block_actions" && len(p.Actions) == 1 && isDraftAction(p.Actions[0].ActionID):
		return s.slackDraftButton(ctx, in, slackClick{Action: p.Actions[0].ActionID, Value: p.Actions[0].Value, Channel: p.Channel.ID, ResponseURL: p.ResponseURL}), nil
	case in.Type == "block_actions" && len(p.Actions) == 1 && slack.IsAction(p.Actions[0].ActionID):
		return s.slackButton(ctx, in, slackClick{Action: p.Actions[0].ActionID, Value: p.Actions[0].Value, Channel: p.Channel.ID, ResponseURL: p.ResponseURL}), nil
	}
	// The connect button opens its URL in the browser. Slack tells devbot
	// it was clicked too, which only needs an acknowledgement.
	if in.Type == "block_actions" && len(p.Actions) == 1 && p.Actions[0].ActionID == connectAction {
		return nil, nil
	}
	ref := slackRef{Workspace: in.Workspace, User: in.User, Channel: p.Channel.ID}
	_, prompt, err := s.slackCaller(ctx, in.Workspace, in.User, ref.Channel)
	if err != nil {
		prompt = s.slackFailed(in.Workspace, err)
	}
	if prompt != nil && ref.Channel != "" {
		s.slackTell(ctx, ref, prompt.Text, prompt.Blocks)
	}
	return nil, nil
}

// slackUserChange unlinks a Slack user who was deactivated.
func (s *Server) slackUserChange(ctx context.Context, ev slack.Event) error {
	var e struct {
		User struct {
			ID      string `json:"id"`
			Deleted bool   `json:"deleted"`
		} `json:"user"`
	}
	if s.Auth == nil || json.Unmarshal(ev.Raw, &e) != nil || !e.User.Deleted || e.User.ID == "" {
		return nil
	}
	if _, err := s.Auth.Unlink(ctx, slackSubject(ev.Workspace, e.User.ID)); err != nil && !errors.Is(err, auth.ErrNotLinked) {
		return err
	}
	return nil
}

// slackNotifier posts tasks' events in the Slack threads they were filed
// from. The leader runs it (singletons).
func (s *Server) slackNotifier() *slack.Notifier {
	n := &slack.Notifier{App: s.Slack, Feed: s.E.Q, Every: s.deliverEvery, Retention: s.Cfg.EventRetention, Current: s.slackCurrent}
	// Only a person signed in through the forge can approve.
	if s.Auth != nil {
		n.Held = s.slackHeld
	}
	return n
}

// slackHeld is the commit task holds for approval now, read as devbot.
func (s *Server) slackHeld(ctx context.Context, task string) (string, error) {
	st, err := s.E.Status(ctx, core.Caller{Principal: "devbot"}, task)
	if err != nil || st.Held == nil {
		return "", err
	}
	return st.Held.SHA, nil
}

// slackCurrent is task's state now, as the notifier reconciles a thread
// with it: read as devbot, which shows it only in the thread the task was
// filed from.
func (s *Server) slackCurrent(ctx context.Context, task string) (queue.Event, error) {
	st, err := s.E.Status(ctx, core.Caller{Principal: "devbot"}, task)
	return queue.Event{Task: st.Task, URL: st.URL, PR: st.PR, State: st.State}, err
}

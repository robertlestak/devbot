package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"git.shdw.tech/rob/devbot/internal/auth"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/slack"
)

// Filing and following tasks from Slack: /devbot file and project open a
// form, status and list answer only the caller. Each calls the same tool,
// through s.call, that the caller's MCP client would, as the caller, so it
// is allowed, refused and filtered exactly as that call is.
//
// A task filed from a channel is announced there, publicly, and devbot
// records that message as the task's thread (slack.App.Bind). The task's
// origin says Slack too, but it is the filer's to choose over MCP, so
// nothing reads a thread from it.

const (
	// The forms' callback_ids.
	fileForm    = "devbot_file"
	projectForm = "devbot_project"

	// slackRepoChoices is the most repos a picker offers, Slack's limit.
	slackRepoChoices = 100
	// slackListMax is the most tasks /devbot list shows.
	slackListMax = 50
	// slackWorkTimeout bounds work that carries on after Slack's request.
	slackWorkTimeout = time.Minute

	slackCommandsPath    = "/slack/commands"
	slackInteractionPath = "/slack/interactivity"
)

// slackHelp is /devbot help.
const slackHelp = "*devbot in Slack*\n" +
	"• `/devbot file [repo]`: file a task, in a form. devbot posts that you filed it in the channel, and follows the task in that message's thread.\n" +
	"• `/devbot project [repo]`: file a parent task, one change across several repos, in a form; devbot can plan its child tasks for you to approve.\n" +
	"• `@devbot <what you need>` in a channel or a thread, or a message to devbot: devbot drafts a task from it, shown only to you, to file, edit or discard; or asks what it needs to know in the thread.\n" +
	"• *File with devbot*, in a message's *More actions* menu: file a task from its thread, which devbot copies into the form for you to edit, and follows the task in that thread.\n" +
	"• `/devbot status <task>`: a task's status, shown only to you.\n" +
	"• `/devbot list [repo] [state]`: the open tasks you can see, shown only to you.\n" +
	"• `/devbot edit <task>`: change a task's title, acceptance criteria or context, in a form.\n" +
	"• `/devbot wait <task> <other task> [until tagged [pattern]]`: have a task wait for another to merge first; `none` ends a wait.\n" +
	"• `/devbot retarget <task> <branch>`: point a task's work at another base branch.\n" +
	"• `/devbot channel repo [<repo>|none]`: this channel's default repo, which the file form and `@devbot` drafts here start from; it permits nothing.\n" +
	"• `/devbot`: who devbot acts as for you. `/devbot disconnect` unlinks your forge account.\n" +
	"• `/devbot help`: this."

// slackAckBudget is how long after serve received a Slack request it
// answers with what it has: an empty acknowledgement, a picker short of
// repos. Slack gives up 3 seconds after it sent the request, and those
// include the trip from Slack through any edge and gateway to serve and
// back: 2.5s of serve's own time was seen to time out. 1.5s leaves the
// other 1.5s to the network.
const slackAckBudget = 1500 * time.Millisecond

// slackDeadline is when the Slack request ctx is for is to be answered:
// slackBudget after serve received it, or from now outside one.
func (s *Server) slackDeadline(ctx context.Context) time.Time {
	t := slack.Received(ctx)
	if t.IsZero() {
		t = time.Now()
	}
	return t.Add(s.slackBudget)
}

// slackSoon runs work apart from the Slack request that asked for it, so
// Slack hanging up doesn't cut it short, and returns its result when it
// is done by s.slackDeadline. Otherwise ok is false, and late gets the
// result once there is one.
func slackSoon[T any](s *Server, ctx context.Context, work func(context.Context) T, late func(context.Context, T)) (res T, ok bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), slackWorkTimeout)
	var mu sync.Mutex
	gaveUp := false
	done := make(chan T, 1)
	s.slackWork.Add(1)
	go func() {
		defer s.slackWork.Done()
		defer cancel()
		r := work(ctx)
		mu.Lock()
		if gaveUp {
			mu.Unlock()
			late(ctx, r)
			return
		}
		done <- r
		mu.Unlock()
	}()
	t := time.NewTimer(time.Until(s.slackDeadline(ctx)))
	defer t.Stop()
	select {
	case r := <-done:
		return r, true
	case <-t.C:
	}
	mu.Lock()
	defer mu.Unlock()
	select {
	case r := <-done:
		return r, true
	default:
		gaveUp = true
		return res, false
	}
}

// slackTasksCommand handles the commands that file and read tasks, for c;
// handled is false for any other verb.
func (s *Server) slackTasksCommand(ctx context.Context, cmd slack.Command, c core.Caller, verb, rest string) (reply any, handled bool) {
	ref := slackRef{Workspace: cmd.Workspace, User: cmd.User, Channel: cmd.Channel}
	switch verb {
	case "file", "project":
		f := slackForm{Kind: fileForm, Channel: cmd.Channel, Repo: strings.TrimSpace(rest)}
		if verb == "project" {
			f.Kind = projectForm
		}
		if f.Repo == "" {
			f.Repo = s.slackChannelRepo(ctx, cmd.Workspace, cmd.Channel)
		}
		// The picker will want them, before the person gets to it.
		s.slackWarmRepos(ctx, cmd.Workspace, cmd.User, c)
		return s.slackOpenForm(ctx, cmd, f), true
	case "status":
		task := strings.TrimSpace(rest)
		if task == "" || strings.ContainsAny(task, " \t") {
			return ephemeral("Usage: `/devbot status <task>`, e.g. `/devbot status " + s.exampleRef() + "`."), true
		}
		args, _ := json.Marshal(refText{Ref: task})
		return s.slackAnswer(ctx, cmd, func(ctx context.Context) *slackReply {
			out, err := s.call(ctx, c, "task_status", args)
			if err != nil {
				return s.slackRefusal(ctx, ref, slackCommandsPath, err)
			}
			st, _ := out.(core.TaskStatus)
			return ephemeral(slackStatusText(st))
		}), true
	case "list":
		var q struct {
			Repo  string `json:"repo,omitempty"`
			State string `json:"state,omitempty"`
		}
		for _, w := range strings.Fields(rest) {
			if strings.Contains(w, ":") && q.Repo == "" {
				q.Repo = w
			} else if q.State == "" {
				q.State = w
			} else {
				return ephemeral("Usage: `/devbot list [repo] [state]`."), true
			}
		}
		args, _ := json.Marshal(q)
		return s.slackAnswer(ctx, cmd, func(ctx context.Context) *slackReply {
			out, err := s.call(ctx, c, "list_tasks", args)
			if err != nil {
				return s.slackRefusal(ctx, ref, slackCommandsPath, err)
			}
			return ephemeral(s.slackListText(ctx, out))
		}), true
	}
	return nil, false
}

// exampleRef is a task ref on this install, for a usage message.
func (s *Server) exampleRef() string {
	_, ref := s.examples()
	ref, _, _ = strings.Cut(ref, " or ")
	return ref
}

// slackAnswer answers cmd with what work replies, in the response to the
// command when it is quick, else later through the command's response_url.
// Either way only the caller sees it.
func (s *Server) slackAnswer(ctx context.Context, cmd slack.Command, work func(context.Context) *slackReply) any {
	r, ok := slackSoon(s, ctx, work, func(ctx context.Context, r *slackReply) {
		if cmd.ResponseURL != "" {
			err := s.Slack.Respond(ctx, cmd.ResponseURL, r)
			if err == nil {
				return
			}
			s.Log.Warn("slack: answering a command late", "workspace", cmd.Workspace.String(), "err", err)
		}
		s.slackTell(ctx, slackRef{Workspace: cmd.Workspace, User: cmd.User, Channel: cmd.Channel}, r.Text, r.Blocks)
	})
	if !ok {
		return nil
	}
	return r
}

// slackRefusal is what the caller is told of a call that failed: what
// their MCP client would be told, or the connect button when the forge no
// longer takes their sign-in.
func (s *Server) slackRefusal(ctx context.Context, ref slackRef, path string, err error) *slackReply {
	if s.Auth != nil && errors.Is(err, auth.ErrInvalidToken) {
		if prompt, perr := s.connectPrompt(ctx, ref.Workspace, ref.User, ref.Channel); perr == nil {
			return prompt
		}
	}
	_, msg := s.publicErrorAt(ctx, path, err)
	return ephemeral(slack.Escape(msg))
}

// slackTell tells ref's user text, ephemerally in ref's channel (in its
// thread, when it has one), or in their messages with the app when the bot
// can't post there.
func (s *Server) slackTell(ctx context.Context, ref slackRef, text string, blocks []any) {
	bot, err := s.Slack.Bot(ctx, ref.Workspace)
	if err != nil {
		s.Log.Warn("slack: telling a user", "workspace", ref.Workspace.String(), "err", err)
		return
	}
	m := slack.Message{Channel: ref.Channel, ThreadTS: ref.Thread, Text: text}
	if blocks != nil {
		m.Blocks, _ = json.Marshal(blocks)
	}
	if ref.Channel != "" {
		if err = bot.PostEphemeral(ctx, ref.User, m); err == nil {
			return
		}
	}
	m.Channel, m.ThreadTS = ref.User, ""
	if _, derr := bot.PostMessage(ctx, m); derr != nil {
		s.Log.Warn("slack: telling a user", "workspace", ref.Workspace.String(), "channel", ref.Channel, "err", errors.Join(err, derr))
	}
}

// ---- status and list ----

// slackLink is a task's link, named by its ref.
func slackLink(st core.TaskStatus) string { return slack.Link(st.URL, st.Task) }

func slackTaskLine(st core.TaskStatus) string {
	return slackLink(st) + " `" + slack.Escape(st.State) + "` " + slack.Escape(st.Title)
}

// slackStatusText is task_status's answer as a message. A held change is
// a model's words, so it is only pointed to, never quoted.
func slackStatusText(st core.TaskStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*%s* %s\nState: `%s`", slackLink(st), slack.Escape(st.Title), slack.Escape(st.State))
	if st.PR != "" {
		fmt.Fprintf(&b, " · PR: <%s>", st.PR)
	}
	if st.Base != "" {
		fmt.Fprintf(&b, " · base `%s`", slack.Escape(st.Base))
	}
	if st.Automerge {
		b.WriteString(" · automerge")
	}
	if st.Parent != "" {
		fmt.Fprintf(&b, "\nPart of the parent task %s.", slack.Escape(st.Parent))
	}
	if st.WaitingOn != "" {
		fmt.Fprintf(&b, "\nWaiting on %s (for its %s).", slack.Escape(st.WaitingOn), slack.Escape(st.WaitingFor))
	}
	if st.Held != nil {
		b.WriteString("\nIt holds a change for a person to approve: see the issue.")
	}
	if st.Note != "" {
		fmt.Fprintf(&b, "\n%s", slack.Escape(st.Note))
	}
	if len(st.Children) > 0 {
		b.WriteString("\nChild tasks:")
		for _, ch := range st.Children {
			b.WriteString("\n• " + slackTaskLine(ch))
		}
	}
	return b.String()
}

// slackListText is list_tasks' answer as a message, with what couldn't be
// read said as the MCP says it.
func (s *Server) slackListText(ctx context.Context, out any) string {
	var missing string
	if p, ok := out.(incomplete); ok {
		_, msg := s.publicErrorAt(ctx, slackCommandsPath, p.err)
		out, missing = p.list, "\nincomplete: "+slack.Escape(msg)
	}
	list, _ := out.([]core.TaskStatus)
	if len(list) == 0 {
		return "No open tasks that you can see." + missing
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d open tasks you can see:", len(list))
	for i, st := range list {
		if i == slackListMax {
			fmt.Fprintf(&b, "\n…and %d more: narrow it with a repo or a state.", len(list)-i)
			break
		}
		b.WriteString("\n• " + slackTaskLine(st))
	}
	return b.String() + missing
}

// ---- the file and project forms ----

// slackForm is a file or project form: what it opens with, and what was
// submitted.
type slackForm struct {
	Kind    string // fileForm or projectForm
	Channel string // where it was opened, kept in its private_metadata
	// Thread, kept there too, is the ts of the thread it was opened from
	// with the message shortcut: the task's thread is that one.
	Thread string
	// Draft, kept in its private_metadata, is the intake draft whose Edit
	// opened it, which filing it uses up.
	Draft string

	Repo, Title, Body, AcceptanceCriteria, Base string
	Automerge, Plan                             bool
	Repos                                       []string
	// Error is why devbot refused the last submit, as mrkdwn, shown above
	// the form.
	Error string
}

type slackFormMeta struct {
	Channel string `json:"channel,omitempty"`
	Thread  string `json:"thread,omitempty"`
	Draft   string `json:"draft,omitempty"`
}

func plain(s string) map[string]any { return map[string]any{"type": "plain_text", "text": s} }

// slackOption is a repo as a select's option. Slack shows at most 75
// characters of it.
func slackOption(repo string) map[string]any {
	text := repo
	if len(text) > 75 {
		text = strings.ToValidUTF8(text[:74], "") + "…"
	}
	return map[string]any{"text": plain(text), "value": repo}
}

func input(id, label, hint string, optional bool, el map[string]any) map[string]any {
	el["action_id"] = id
	b := map[string]any{"type": "input", "block_id": id, "optional": optional, "label": plain(label), "element": el}
	if hint != "" {
		b["hint"] = plain(hint)
	}
	return b
}

func textInput(multiline bool, initial string) map[string]any {
	el := map[string]any{"type": "plain_text_input", "multiline": multiline}
	if initial != "" {
		el["initial_value"] = initial
	}
	return el
}

func checkbox(text, desc string, on bool) map[string]any {
	opt := map[string]any{"text": plain(text), "value": "yes", "description": plain(desc)}
	el := map[string]any{"type": "checkboxes", "options": []any{opt}}
	if on {
		el["initial_options"] = []any{opt}
	}
	return el
}

// view is the form as a modal.
func (f slackForm) view() json.RawMessage {
	meta, _ := json.Marshal(slackFormMeta{Channel: f.Channel, Thread: f.Thread, Draft: f.Draft})
	var blocks []any
	if f.Error != "" {
		text := f.Error
		if len(text) > 2900 {
			text = strings.ToValidUTF8(text[:2900], "") + "…"
		}
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": ":warning: " + text}})
	}
	switch {
	case f.Draft != "":
		blocks = append(blocks, map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn", "text": slackDraftNote}}})
	case f.Thread != "":
		blocks = append(blocks, map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn", "text": slackThreadNote}}})
	}
	repo := map[string]any{"type": "external_select", "min_query_length": 0, "placeholder": plain("provider:owner/name")}
	if f.Repo != "" {
		repo["initial_option"] = slackOption(f.Repo)
	}
	title, submit := "File a task", "File"
	repoHint := "The repos you can direct devbot on."
	acHint := "What done looks like, as checkable statements."
	if f.Kind == projectForm {
		title, submit = "File a parent task", "File"
		repoHint = "Where the parent issue goes: a repo you can direct devbot on."
		acHint = "What done looks like for the whole change. Required to have devbot plan it."
	}
	blocks = append(blocks,
		input("repo", "Repo", repoHint, false, repo),
		input("title", "Title", "", false, textInput(false, f.Title)),
		input("acceptance_criteria", "Acceptance criteria", acHint, f.Kind == projectForm, textInput(true, f.AcceptanceCriteria)),
		input("body", "Context", "What and why: links, reproduction steps for a bug.", true, textInput(true, f.Body)),
	)
	if f.Kind == projectForm {
		repos := map[string]any{"type": "multi_external_select", "min_query_length": 0, "max_selected_items": core.MaxPlanRepos,
			"placeholder": plain("provider:owner/name")}
		if len(f.Repos) > 0 {
			var opts []any
			for _, r := range f.Repos {
				opts = append(opts, slackOption(r))
			}
			repos["initial_options"] = opts
		}
		blocks = append(blocks,
			input("plan", "Plan", "", true, checkbox("Have devbot plan the child tasks",
				"A model reads the repos below and proposes the child tasks, for a person to approve before any is filed.", f.Plan)),
			input("repos", "Repos to plan across", fmt.Sprintf("With plan: the repos the change may touch, at most %d.", core.MaxPlanRepos), true, repos),
			input("automerge", "Automerge", "", true, checkbox("Merge child tasks without waiting for a person",
				"Each child's PR merges once its full merge gate passes, never sooner.", f.Automerge)),
		)
	} else {
		blocks = append(blocks,
			input("base", "Base branch", "The branch to work from and open the PR against. Empty: the repo's.", true, textInput(false, f.Base)),
			input("automerge", "Automerge", "", true, checkbox("Merge without waiting for a person",
				"Once the full merge gate passes, never sooner.", f.Automerge)),
		)
	}
	v, _ := json.Marshal(map[string]any{"type": "modal", "callback_id": f.Kind, "private_metadata": string(meta),
		"title": plain(title), "submit": plain(submit), "close": plain("Cancel"), "blocks": blocks})
	return v
}

// slackView is the part of a submitted modal devbot reads.
type slackView struct {
	CallbackID      string `json:"callback_id"`
	PrivateMetadata string `json:"private_metadata"`
	State           struct {
		Values map[string]map[string]struct {
			Value          string `json:"value"`
			SelectedOption *struct {
				Value string `json:"value"`
			} `json:"selected_option"`
			SelectedOptions []struct {
				Value string `json:"value"`
			} `json:"selected_options"`
		} `json:"values"`
	} `json:"state"`
}

// form is what was submitted in v.
func (v slackView) form() slackForm {
	var meta slackFormMeta
	json.Unmarshal([]byte(v.PrivateMetadata), &meta)
	f := slackForm{Kind: v.CallbackID, Channel: meta.Channel, Thread: meta.Thread, Draft: meta.Draft}
	value := func(id string) string { return v.State.Values[id][id].Value }
	f.Title, f.Body, f.AcceptanceCriteria, f.Base = value("title"), value("body"), value("acceptance_criteria"), value("base")
	if o := v.State.Values["repo"]["repo"].SelectedOption; o != nil {
		f.Repo = o.Value
	}
	for _, o := range v.State.Values["repos"]["repos"].SelectedOptions {
		f.Repos = append(f.Repos, o.Value)
	}
	f.Automerge = len(v.State.Values["automerge"]["automerge"].SelectedOptions) > 0
	f.Plan = len(v.State.Values["plan"]["plan"].SelectedOptions) > 0
	return f
}

// stillLoading is the picker's "still finding" option where it was
// picked, as a submission's errors by block: nil when it wasn't.
func (f slackForm) stillLoading() map[string]string {
	const again = "devbot was still finding your repos: search again, and pick one."
	errs := map[string]string{}
	if f.Repo == slackStillLoading {
		errs["repo"] = again
	}
	if slices.Contains(f.Repos, slackStillLoading) {
		errs["repos"] = again
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// request is the tool call f makes, with origin.
func (f slackForm) request(origin string) (tool string, args json.RawMessage) {
	if f.Kind == projectForm {
		args, _ = json.Marshal(core.ProjectRequest{Repo: f.Repo, Title: f.Title, Body: f.Body, AcceptanceCriteria: f.AcceptanceCriteria,
			Origin: origin, Plan: f.Plan, Repos: f.Repos, Automerge: f.Automerge})
		return "file_project", args
	}
	args, _ = json.Marshal(core.FileRequest{Repo: f.Repo, Title: f.Title, Body: f.Body, AcceptanceCriteria: f.AcceptanceCriteria,
		Origin: origin, Base: f.Base, Automerge: f.Automerge})
	return "file_task", args
}

// slackOrigin is the origin of a task filed from channel in ws. It is only
// a label, like any origin: the thread is the binding's.
func slackOrigin(ws slack.Workspace, channel string) string {
	return "slack:" + ws.String() + "/" + channel
}

// slackOpenForm opens f for cmd's user.
func (s *Server) slackOpenForm(ctx context.Context, cmd slack.Command, f slackForm) any {
	bot, err := s.Slack.Bot(ctx, cmd.Workspace)
	if err == nil {
		_, err = bot.OpenView(ctx, cmd.TriggerID, f.view())
	}
	if err != nil {
		s.Log.Warn("slack: opening a form", "workspace", cmd.Workspace.String(), "err", err)
		return ephemeral("devbot couldn't open the form. Try again.")
	}
	return nil
}

// slackRepoChoices answers a repo picker's search with the repos the
// user may direct devbot on: none until they are linked. Slack gives up
// after 3 seconds, and checking every repo can take longer, so it answers
// from the user's list, warming it if the form didn't, with what it has
// by s.slackDeadline.
func (s *Server) slackRepoChoices(ctx context.Context, in slack.Interaction, query string) any {
	deadline := s.slackDeadline(ctx)
	opts := []any{}
	// Not slackCaller first: an unlinked user's every keystroke would make
	// a connect state.
	if s.Auth != nil {
		if _, err := s.Auth.Linked(ctx, slackSubject(in.Workspace, in.User)); err != nil {
			return map[string]any{"options": opts}
		}
	}
	c, prompt, err := s.slackCaller(ctx, in.Workspace, in.User, "")
	if err != nil || prompt != nil {
		return map[string]any{"options": opts}
	}
	l := s.slackWarmRepos(ctx, in.Workspace, in.User, c)
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case <-l.done:
	case <-t.C:
	case <-ctx.Done():
	}
	repos, warming := l.search(query)
	limit := slackRepoChoices
	if warming {
		limit--
	}
	for _, r := range repos[:min(len(repos), limit)] {
		opts = append(opts, slackOption(r))
	}
	if warming {
		opts = append(opts, map[string]any{"text": plain("Still finding your repos: type again…"), "value": slackStillLoading})
	}
	return map[string]any{"options": opts}
}

// slackStillLoading is the picker's option that says it isn't done: no
// repo, as no ref has this form.
const slackStillLoading = "devbot:loading"

// slackRepoTTL is how long a user's repos for the picker are kept. Filing
// checks the repo again, so a stale list only offers a repo that is then
// refused, or misses one for this long.
const slackRepoTTL = 5 * time.Minute

// repoLists are the picker's repo lists, one per Slack user.
type repoLists struct {
	mu sync.Mutex
	m  map[string]*repoList
}

// repoList is the repos one user may direct devbot on, as they are found.
type repoList struct {
	done chan struct{} // closed once all are checked
	// expires is when the list is to be read again: zero while warming.
	// Guarded by repoLists.mu.
	expires time.Time

	mu    sync.Mutex
	repos []string
}

// search is l's repos that contain query, sorted, and whether more may
// come.
func (l *repoList) search(query string) (repos []string, warming bool) {
	select {
	case <-l.done:
	default:
		warming = true
	}
	query = strings.ToLower(strings.TrimSpace(query))
	l.mu.Lock()
	for _, r := range l.repos {
		if strings.Contains(strings.ToLower(r), query) {
			repos = append(repos, r)
		}
	}
	l.mu.Unlock()
	slices.Sort(repos)
	return repos, warming
}

// slackWarmRepos is the list of repos user of ws, as c, may direct devbot
// on, starting to find them unless a list for them is warming or fresh.
// The key is the Slack user and the person they act as, so a list is never
// another user's, nor theirs from before they linked another account.
func (s *Server) slackWarmRepos(ctx context.Context, ws slack.Workspace, user string, c core.Caller) *repoList {
	key := slackSubject(ws, user) + "\x00" + c.Principal + "\x00" + c.Provider + ":" + strings.ToLower(c.Login)
	now := s.now()
	ls := &s.slackRepos
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if l := ls.m[key]; l != nil && (l.expires.IsZero() || now.Before(l.expires)) {
		return l
	}
	if ls.m == nil {
		ls.m = map[string]*repoList{}
	}
	for k, l := range ls.m {
		if !l.expires.IsZero() && !now.Before(l.expires) {
			delete(ls.m, k)
		}
	}
	l := &repoList{done: make(chan struct{})}
	ls.m[key] = l
	s.slackWork.Add(1)
	go func() {
		defer s.slackWork.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), slackWorkTimeout)
		defer cancel()
		err := s.E.DirectableRepos(callerContext(ctx, c), c, func(r string) {
			l.mu.Lock()
			l.repos = append(l.repos, r)
			l.mu.Unlock()
		})
		ttl := slackRepoTTL
		if err != nil {
			// What was found stands, and the next search looks again.
			s.Log.Warn("slack: listing repos for a picker", "workspace", ws.String(), "err", err)
			ttl = 0
		}
		ls.mu.Lock()
		l.expires = s.now().Add(ttl)
		ls.mu.Unlock()
		close(l.done)
	}()
	return l
}

// slackSubmit files what a form was submitted with, as the user who
// submitted it. A refusal in time shows the form again, as it was, under
// the refusal; one after Slack stopped waiting comes as an ephemeral
// message.
func (s *Server) slackSubmit(ctx context.Context, in slack.Interaction, v slackView) (any, error) {
	f := v.form()
	if errs := f.stillLoading(); errs != nil {
		return map[string]any{"response_action": "errors", "errors": errs}, nil
	}
	ref := slackRef{Workspace: in.Workspace, User: in.User, Channel: f.Channel}
	again := func(r *slackReply) (any, error) {
		if len(r.Blocks) > 0 {
			// The connect button only works as a message.
			s.slackTell(ctx, ref, r.Text, r.Blocks)
		}
		f.Error = r.Text
		return map[string]any{"response_action": "update", "view": f.view()}, nil
	}
	c, prompt, err := s.slackCaller(ctx, in.Workspace, in.User, f.Channel)
	if err != nil {
		prompt = s.slackFailed(in.Workspace, err)
	}
	if prompt != nil {
		return again(prompt)
	}
	tool, args := f.request(slackOrigin(in.Workspace, f.Channel))
	type outcome struct {
		st  core.TaskStatus
		err error
	}
	o, ok := slackSoon(s, ctx, func(ctx context.Context) outcome {
		out, err := s.call(ctx, c, tool, args)
		if err != nil {
			return outcome{err: err}
		}
		st, _ := out.(core.TaskStatus)
		s.slackAnnounce(ctx, ref, f.Thread, st, f.Kind)
		if f.Draft != "" {
			s.E.Q.R.Del(ctx, slackDraftKey(in.Workspace, f.Draft))
		}
		return outcome{st: st}
	}, func(ctx context.Context, o outcome) {
		if o.err != nil {
			r := s.slackRefusal(ctx, ref, slackInteractionPath, o.err)
			s.slackTell(ctx, ref, r.Text, r.Blocks)
		}
	})
	if !ok || o.err == nil {
		return nil, nil
	}
	return again(s.slackRefusal(ctx, ref, slackInteractionPath, o.err))
}

// slackAnnounce says in ref's channel, publicly, that ref's user filed st,
// and records that message as the task's thread. A task filed from a
// thread (the message shortcut) is announced in that thread instead, which
// is its thread. Where the bot can't post, the user is told, and the task
// has no thread. told is whether the user was told so, which says it was
// filed.
func (s *Server) slackAnnounce(ctx context.Context, ref slackRef, thread string, st core.TaskStatus, kind string) (told bool) {
	what := "filed"
	if kind == projectForm {
		what = "filed the parent task"
	}
	text := fmt.Sprintf("<@%s> %s %s: %s", ref.User, what, slackLink(st), slack.Escape(st.Title))
	bot, err := s.Slack.Bot(ctx, ref.Workspace)
	var ts string
	if err == nil {
		if ref.Channel == "" {
			err = errors.New("no channel")
		} else {
			ts, err = bot.PostMessage(ctx, slack.Message{Channel: ref.Channel, ThreadTS: thread, Text: text})
		}
	}
	if thread != "" {
		ts = thread
	}
	if err != nil {
		s.Log.Warn("slack: announcing a filed task", "workspace", ref.Workspace.String(), "channel", ref.Channel, "task", st.Task, "err", err)
		s.slackTell(ctx, ref, fmt.Sprintf("Filed %s: %s. devbot couldn't post that in this channel, so it won't follow the task in a thread here; invite devbot to the channel for the next one.",
			slackLink(st), slack.Escape(st.Title)), nil)
		return true
	}
	if err := s.Slack.Bind(ctx, st.Task, slack.Thread{Workspace: ref.Workspace, Channel: ref.Channel, TS: ts}); err != nil {
		s.Log.Warn("slack: recording a task's thread", "workspace", ref.Workspace.String(), "task", st.Task, "err", err)
		s.slackTell(ctx, ref, fmt.Sprintf("Filed %s, but devbot couldn't record its thread, so it won't follow the task there.", slackLink(st)), nil)
		return true
	}
	return false
}

// waitSlackWork waits, until ctx ends, for the Slack work still under way.
func (s *Server) waitSlackWork(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		s.slackWork.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.Log.Warn("shutdown: Slack requests still being handled at the deadline")
	}
}

package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/intake"
	"git.shdw.tech/rob/devbot/internal/slack"
)

// The Slack intake: `@devbot <free text>` in a channel or a thread, or a
// message to the app, has a model draft a task from the request and its
// thread, or ask what it needs to know first.
//
// serve holds no model credential and makes no model call. It queues an
// intake job (queue.EnqueueIntake), which an intake job pod runs behind the
// model proxy, with no forge credential, and checks what comes back
// against the schema and the requester's repos (intake.Parse). A draft is
// shown only to the requester, with File, Edit and Discard, and nothing is
// filed until they click File: then it is filed as them, exactly as from
// /devbot file. Questions go in the thread, escaped; the requester's next
// @devbot there runs the intake again with the conversation so far.
//
// What only the requester sees (that devbot is drafting, the draft, why
// there is none, and what came of its buttons) goes where they asked: in
// the channel for a request at its top level, in the thread for one in a
// thread. That is where they look, and an ephemeral reply in a thread they
// never opened shows nowhere else.

const (
	// The draft's buttons' action_ids.
	draftFile    = "devbot_intake_file"
	draftEdit    = "devbot_intake_edit"
	draftDiscard = "devbot_intake_discard"

	// slackIntakeBudget is the most characters of the thread an intake
	// reads, besides the request itself.
	slackIntakeBudget = 12000
	// slackIntakeRepos is the most repos an intake chooses from.
	slackIntakeRepos = 100
	// slackIntakeRepoWait is how long an intake waits for the requester's
	// repos to be found.
	slackIntakeRepoWait = 30 * time.Second
	// slackDraftTTL is how long a draft waits for its requester's click,
	// and slackAskedTTL how long devbot remembers what it asked in a thread.
	slackDraftTTL = 24 * time.Hour
	slackAskedTTL = 7 * 24 * time.Hour
	// slackAskedRounds is the most rounds of questions remembered.
	slackAskedRounds = 5
	// slackDescTTL is how long a repo's description is kept.
	slackDescTTL = 10 * time.Minute

	slackIntakePending = "devbot:slack:intake-pending"
)

// slackDraftNote is shown on the form a draft's Edit opens.
const slackDraftNote = "devbot's model drafted this from the thread. Everything in this form becomes part of a forge issue, filed as you: " +
	"check it, and remove what shouldn't be there, before you file it."

// slackIntakeNotInChannel is the answer where the bot can't read the thread.
const slackIntakeNotInChannel = "devbot isn't in this channel, so it can't read the thread, and drafted nothing. " +
	"Add it with `/invite @devbot` in the channel (or the channel's details, *Integrations*, *Add apps*), then ask again."

// slackIntakeAsk is a request to the intake: who, where, and in what
// thread.
type slackIntakeAsk struct {
	Workspace slack.Workspace `json:"ws"`
	User      string          `json:"user"`
	Channel   string          `json:"channel"`
	// Thread is the thread's root, where devbot answers.
	Thread string `json:"thread"`
	// DM is a request in the requester's messages with the app.
	DM bool `json:"dm,omitempty"`
	// InThread is a request made in Thread, not at the channel's top
	// level.
	InThread bool `json:"in_thread,omitempty"`
}

// ref is where a's requester is told what only they see: where they asked.
func (a slackIntakeAsk) ref() slackRef {
	r := slackRef{Workspace: a.Workspace, User: a.User, Channel: a.Channel}
	if a.InThread {
		r.Thread = a.Thread
	}
	return r
}

// slackIntakeWaiting is an intake job serve waits on: the request, the
// repos its draft may name, and until when.
type slackIntakeWaiting struct {
	slackIntakeAsk
	Repos    []string  `json:"repos"`
	Deadline time.Time `json:"deadline"`
}

// slackDraft is a draft waiting for its requester.
type slackDraft struct {
	slackIntakeAsk
	Draft intake.Draft `json:"draft"`
}

func wsKey(ws slack.Workspace) string { return ws.Enterprise + ":" + ws.Team }

func slackDraftKey(ws slack.Workspace, id string) string {
	return "devbot:slack:intake-draft:" + wsKey(ws) + ":" + id
}

func slackAskedKey(ws slack.Workspace, channel, thread string) string {
	return "devbot:slack:intake-asked:" + wsKey(ws) + ":" + channel + ":" + thread
}

func newIntakeID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// slackMention takes a request to the intake: an app_mention outside the
// app's messages, or a person's message in them. An unlinked requester
// only gets the connect prompt, before anything runs.
func (s *Server) slackMention(ctx context.Context, ev slack.Event) error {
	var e struct {
		User        string `json:"user"`
		TS          string `json:"ts"`
		ThreadTS    string `json:"thread_ts"`
		Channel     string `json:"channel"`
		ChannelType string `json:"channel_type"`
		BotID       string `json:"bot_id"`
		Subtype     string `json:"subtype"`
	}
	if json.Unmarshal(ev.Raw, &e) != nil || e.User == "" || e.Channel == "" || e.TS == "" || e.BotID != "" || e.Subtype != "" {
		return nil
	}
	dm := e.ChannelType == "im" || strings.HasPrefix(e.Channel, "D")
	// A message to the app is its own request; a mention elsewhere comes as
	// app_mention, and the message event of a channel isn't subscribed to.
	if (ev.Type == "message") != dm {
		return nil
	}
	a := slackIntakeAsk{Workspace: ev.Workspace, User: e.User, Channel: e.Channel, Thread: e.ThreadTS, DM: dm, InThread: e.ThreadTS != ""}
	if a.Thread == "" {
		a.Thread = e.TS
	}
	c, prompt, err := s.slackCaller(ctx, ev.Workspace, e.User, e.Channel)
	if err != nil {
		prompt = s.slackFailed(ev.Workspace, err)
	}
	if prompt != nil {
		s.slackGo(ctx, func(ctx context.Context) { s.slackTell(ctx, a.ref(), prompt.Text, prompt.Blocks) })
		return nil
	}
	s.slackGo(ctx, func(ctx context.Context) { s.slackIntake(ctx, c, a, e.TS) })
	return nil
}

// slackIntake queues an intake job for the request ts in a's thread, for c,
// within the rate limits.
func (s *Server) slackIntake(ctx context.Context, c core.Caller, a slackIntakeAsk, ts string) {
	ref := a.ref()
	tell := func(text string) { s.slackTell(ctx, ref, text, nil) }
	lim := s.Cfg.Slack.Intake.WithDefaults()
	if over, err := s.slackIntakeAllowed(ctx, a); err != nil {
		s.Log.Warn("slack: counting an intake request", "workspace", a.Workspace.String(), "err", err)
		tell("devbot couldn't take your request. Try again.")
		return
	} else if over != "" {
		tell(over)
		return
	}
	bot, err := s.Slack.Bot(ctx, a.Workspace)
	if err != nil {
		s.Log.Warn("slack: an intake request", "workspace", a.Workspace.String(), "err", err)
		return
	}
	msgs, more, err := bot.Replies(ctx, a.Channel, a.Thread, slackThreadMessages)
	switch {
	case slack.IsCode(err, "not_in_channel") || slack.IsCode(err, "channel_not_found"):
		tell(slackIntakeNotInChannel)
		return
	case slack.IsCode(err, "missing_scope"):
		tell("devbot can't read message history in this workspace, so it drafted nothing: the app was installed before it asked to. " +
			"Ask whoever installed it to install it again, from devbot's `/slack/install`.")
		return
	case err != nil:
		s.Log.Warn("slack: reading a thread", "workspace", a.Workspace.String(), "channel", a.Channel, "err", err)
		tell("devbot couldn't read the thread, so it drafted nothing. Try again.")
		return
	}
	link, err := bot.Permalink(ctx, a.Channel, a.Thread)
	if err != nil {
		s.Log.Warn("slack: a thread's link", "workspace", a.Workspace.String(), "channel", a.Channel, "err", err)
	}
	names := slackNames{bot: bot, names: map[string]string{}}
	req := intake.Request{ID: newIntakeID(), Requester: names.user(ctx, a.User)}
	for _, m := range msgs {
		if m.TS == ts {
			req.Request = names.plain(ctx, m.Text)
		}
	}
	req.Thread = slackThreadText(ctx, link, msgs, more, &names, slackIntakeBudget)
	l := s.slackWarmRepos(ctx, a.Workspace, a.User, c)
	// What is found in half the work's minute; the rest of it queues the
	// job.
	t := time.NewTimer(slackIntakeRepoWait)
	select {
	case <-l.done:
	case <-t.C:
	}
	t.Stop()
	repos, _ := l.search("")
	if len(repos) == 0 {
		tell("devbot can't draft a task for you: there is no repo you can direct devbot on.")
		return
	}
	// The channel's default goes first, so the cap never drops it, but
	// only if it is one of the requester's: it permits nothing.
	if d := s.slackChannelRepo(ctx, a.Workspace, a.Channel); d != "" {
		if i := slices.Index(repos, d); i >= 0 {
			repos = append([]string{d}, slices.Delete(repos, i, i+1)...)
			req.Default = d
		}
	}
	repos = repos[:min(len(repos), slackIntakeRepos)]
	req.Repos = s.repoDescriptions(ctx, repos)
	req.Asked = s.slackAsked(ctx, a)
	w, _ := json.Marshal(slackIntakeWaiting{slackIntakeAsk: a, Repos: repos, Deadline: s.now().Add(lim.Timeout)})
	err = s.E.Q.R.HSet(ctx, slackIntakePending, req.ID, w).Err()
	if err == nil {
		err = s.E.Q.EnqueueIntake(ctx, req, lim.Timeout+time.Minute)
	}
	if err != nil {
		s.E.Q.R.HDel(context.WithoutCancel(ctx), slackIntakePending, req.ID)
		s.Log.Warn("slack: queueing an intake job", "workspace", a.Workspace.String(), "err", err)
		tell("devbot couldn't take your request. Try again.")
		return
	}
	tell("devbot is drafting a task from this. The draft will appear here, where only you see it, and nothing is filed until you click *File*.")
}

// intakeRateScript counts a request against the user's and the workspace's
// hourly limits, unless it is over either: 1 for the user's, 2 for the
// workspace's.
var intakeRateScript = redis.NewScript(`
if tonumber(redis.call('GET', KEYS[1]) or '0') >= tonumber(ARGV[1]) then return 1 end
if tonumber(redis.call('GET', KEYS[2]) or '0') >= tonumber(ARGV[2]) then return 2 end
for i = 1, 2 do
  redis.call('INCR', KEYS[i])
  redis.call('EXPIRE', KEYS[i], ARGV[3])
end
return 0`)

// slackIntakeAllowed counts one intake request of a's against its limits,
// and says why not when it is over one.
func (s *Server) slackIntakeAllowed(ctx context.Context, a slackIntakeAsk) (string, error) {
	lim := s.Cfg.Slack.Intake.WithDefaults()
	hour := fmt.Sprint(s.now().Unix() / 3600)
	key := "devbot:slack:intake-rate:" + wsKey(a.Workspace) + ":"
	n, err := intakeRateScript.Run(ctx, s.E.Q.R, []string{key + a.User + ":" + hour, key + "*:" + hour},
		lim.PerUserPerHour, lim.PerWorkspacePerHour, 3600).Int()
	switch {
	case err != nil:
		return "", err
	case n == 1:
		return fmt.Sprintf("You've asked devbot for %d drafts this hour, as many as it takes from one person, so it drafted nothing. "+
			"Try again later, or file it yourself with `/devbot file`.", lim.PerUserPerHour), nil
	case n == 2:
		return fmt.Sprintf("devbot has taken %d requests from this workspace this hour, as many as it takes, so it drafted nothing. "+
			"Try again later, or file it yourself with `/devbot file`.", lim.PerWorkspacePerHour), nil
	}
	return "", nil
}

// slackAsked is what devbot asked in a's thread before.
func (s *Server) slackAsked(ctx context.Context, a slackIntakeAsk) [][]string {
	var asked [][]string
	if b, err := s.E.Q.R.Get(ctx, slackAskedKey(a.Workspace, a.Channel, a.Thread)).Bytes(); err == nil {
		json.Unmarshal(b, &asked)
	}
	return asked
}

// repoDescs keeps repos' descriptions for a while: each intake lists up to
// slackIntakeRepos of them.
type repoDescs struct {
	mu sync.Mutex
	m  map[string]cachedTitle
}

// repoDescriptions is repos with what their forges say they are, each read
// as devbot, 8 at a time. One that can't be read has none.
func (s *Server) repoDescriptions(ctx context.Context, repos []string) []intake.Repo {
	out := make([]intake.Repo, len(repos))
	now := s.now()
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, r := range repos {
		out[i].Repo = r
		s.descs.mu.Lock()
		c, ok := s.descs.m[r]
		s.descs.mu.Unlock()
		if ok && now.Sub(c.at) < slackDescTTL {
			out[i].Description = c.title
			continue
		}
		ref, err := forge.ParseRepo(r)
		if err != nil {
			continue
		}
		p, err := s.E.Forges.For(ref)
		if err != nil {
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			d, err := p.RepoDescription(ctx, ref)
			if err != nil {
				return
			}
			out[i].Description = d
			s.descs.mu.Lock()
			defer s.descs.mu.Unlock()
			if s.descs.m == nil || len(s.descs.m) >= 10000 {
				s.descs.m = map[string]cachedTitle{}
			}
			s.descs.m[r] = cachedTitle{d, now}
		})
	}
	wg.Wait()
	return out
}

// slackIntakeLoop acts on each intake job's result as it comes, or on its
// not coming in time. The leader runs it.
func (s *Server) slackIntakeLoop(ctx context.Context) {
	t := time.NewTicker(s.intakeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.slackIntakeResults(ctx); err != nil && ctx.Err() == nil {
			s.Log.Warn("slack: reading intake results", "err", err)
		}
	}
}

// slackIntakeResults acts on every intake job that has finished or run out
// of time, then forgets it. A failover between acting and forgetting acts
// again: at least once, never lost.
func (s *Server) slackIntakeResults(ctx context.Context) error {
	waiting, err := s.E.Q.R.HGetAll(ctx, slackIntakePending).Result()
	if err != nil {
		return err
	}
	for id, raw := range waiting {
		var w slackIntakeWaiting
		if err := json.Unmarshal([]byte(raw), &w); err == nil {
			res, ok, err := s.E.Q.IntakeResult(ctx, id)
			if err != nil {
				return err
			}
			if !ok && s.now().Before(w.Deadline) {
				continue
			}
			if !ok {
				res.Error = "it didn't finish in time"
			}
			s.slackIntakeDone(ctx, id, w, res)
		}
		if err := s.E.Q.R.HDel(ctx, slackIntakePending, id).Err(); err != nil {
			return err
		}
		s.E.Q.DropIntake(ctx, id)
	}
	return nil
}

// slackIntakeDone tells w's requester what came of their request: a draft,
// questions in the thread, or why there is neither. What the model said is
// checked first, and shown only escaped.
func (s *Server) slackIntakeDone(ctx context.Context, id string, w slackIntakeWaiting, res intake.Result) {
	ref := w.ref()
	if res.Error != "" {
		s.slackTell(ctx, ref, "devbot couldn't draft a task from your request: "+slack.Escape(res.Error)+". "+
			"Try again, or file it yourself with `/devbot file`.", nil)
		return
	}
	out, err := intake.Parse(res.Output, w.Repos)
	if err != nil {
		s.Log.Warn("slack: an intake answer refused", "workspace", w.Workspace.String(), "intake", id, "err", err)
		s.slackTell(ctx, ref, "devbot's draft didn't pass its checks, so it was dropped: "+slack.Escape(err.Error())+". "+
			"Try again, or file it yourself with `/devbot file`.", nil)
		return
	}
	if out.Draft != nil {
		s.slackOfferDraft(ctx, id, w.slackIntakeAsk, *out.Draft)
		return
	}
	s.slackAsk(ctx, w.slackIntakeAsk, out.Questions)
}

// slackAsk posts questions in a's thread, for everyone there, and
// remembers them for the next round.
func (s *Server) slackAsk(ctx context.Context, a slackIntakeAsk, questions []string) {
	var b strings.Builder
	fmt.Fprintf(&b, "<@%s> before devbot drafts this, it needs to know:", a.User)
	for _, q := range questions {
		b.WriteString("\n• " + slack.Escape(strings.Join(strings.Fields(q), " ")))
	}
	if a.DM {
		b.WriteString("\nAnswer here, in this thread.")
	} else {
		b.WriteString("\nAnswer here, in this thread, mentioning @devbot.")
	}
	asked := append(s.slackAsked(ctx, a), questions)
	// The last few rounds are what the next one needs.
	asked = asked[max(0, len(asked)-slackAskedRounds):]
	if j, err := json.Marshal(asked); err == nil {
		if err := s.E.Q.R.Set(ctx, slackAskedKey(a.Workspace, a.Channel, a.Thread), j, slackAskedTTL).Err(); err != nil {
			s.Log.Warn("slack: remembering an intake's questions", "workspace", a.Workspace.String(), "err", err)
		}
	}
	bot, err := s.Slack.Bot(ctx, a.Workspace)
	if err == nil {
		_, err = bot.PostMessage(ctx, slack.Message{Channel: a.Channel, ThreadTS: a.Thread, Text: b.String()})
	}
	if err != nil {
		s.Log.Warn("slack: asking in a thread", "workspace", a.Workspace.String(), "channel", a.Channel, "err", err)
		s.slackTell(ctx, a.ref(), b.String(), nil)
	}
}

// slackOfferDraft keeps d for a's requester and shows it to them alone,
// where they asked: ephemerally in a channel, in their messages with the
// app otherwise.
func (s *Server) slackOfferDraft(ctx context.Context, id string, a slackIntakeAsk, d intake.Draft) {
	j, _ := json.Marshal(slackDraft{slackIntakeAsk: a, Draft: d})
	if err := s.E.Q.R.Set(ctx, slackDraftKey(a.Workspace, id), j, slackDraftTTL).Err(); err != nil {
		s.Log.Warn("slack: keeping a draft", "workspace", a.Workspace.String(), "err", err)
		s.slackTell(ctx, a.ref(), "devbot drafted a task, but couldn't keep it for you to file. Try again.", nil)
		return
	}
	text, blocks := draftMessage(id, d)
	m := slack.Message{Channel: a.Channel, ThreadTS: a.ref().Thread, Text: text}
	m.Blocks, _ = json.Marshal(blocks)
	bot, err := s.Slack.Bot(ctx, a.Workspace)
	if err == nil {
		if a.DM {
			_, err = bot.PostMessage(ctx, m)
		} else {
			err = bot.PostEphemeral(ctx, a.User, m)
		}
	}
	if err != nil {
		s.Log.Warn("slack: showing a draft", "workspace", a.Workspace.String(), "channel", a.Channel, "err", err)
		s.slackTell(ctx, slackRef{Workspace: a.Workspace, User: a.User}, text, blocks)
	}
}

// maxBlocks is the most blocks Slack takes in a message.
const maxBlocks = 50

// draftMessage is a draft as its requester sees it, every word of the
// model's escaped, with its buttons. File files exactly what this shows,
// so the criteria and the body are shown whole, in as many sections as
// they take; a draft too long for one message is offered only to Edit,
// whose form holds all of it, and Discard.
func draftMessage(id string, d intake.Draft) (string, []any) {
	kind := "a task"
	if d.Project != nil {
		kind = "a parent task"
	}
	text := fmt.Sprintf("devbot drafted %s in %s: %s", kind, slack.Escape(d.Repo), slack.Escape(d.Title))
	shown, whole := draftSections(d)
	until := "you click *File*"
	if !whole {
		until = "you submit it from *Edit*"
	}
	blocks := append([]any{
		mrkdwnSection(fmt.Sprintf("*devbot drafted %s* in `%s`. Only you see this, and nothing is filed until %s.", kind, slack.Escape(d.Repo), until)),
		mrkdwnSection("*Title:* " + slack.Escape(d.Title)),
	}, shown...)
	if !whole {
		blocks = append(blocks, mrkdwnSection(":warning: "+draftTooLong))
	}
	if p := d.Project; p != nil && p.Plan {
		blocks = append(blocks, mrkdwnSection("devbot plans its child tasks across `"+slack.Escape(strings.Join(p.Repos, "`, `"))+"`, for a person to approve."))
	}
	button := func(label, action, style string) map[string]any {
		b := map[string]any{"type": "button", "action_id": action, "value": id, "text": plain(label)}
		if style != "" {
			b["style"] = style
		}
		return b
	}
	buttons := []any{button("Edit", draftEdit, ""), button("Discard", draftDiscard, "danger")}
	if whole {
		buttons = append([]any{button("File", draftFile, "primary")}, buttons...)
	}
	blocks = append(blocks,
		map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn",
			"text": "A model wrote this from the thread. Filing it makes a forge issue of it, as you, readable by whoever can read the repo's issues."}}},
		map[string]any{"type": "actions", "elements": buttons},
	)
	return text, blocks
}

const draftTooLong = "This draft is too long to show here in full, so it can't be filed as it is: click *Edit* to read all of it in the form, and file it from there."

// draftSections are d's acceptance criteria and body, escaped, in sections
// Slack takes, and whether that is all of them: past the blocks the rest
// of the message leaves, only the first are returned, and false.
func draftSections(d intake.Draft) ([]any, bool) {
	var parts []string
	add := func(heading, text string) {
		text = slack.Escape(text)
		for i, part := range slackChunks(text, maxSection-len(heading), len(text)) {
			if i == 0 {
				part = heading + part
			}
			parts = append(parts, part)
		}
	}
	add("*Acceptance criteria:*\n", d.AcceptanceCriteria)
	if strings.TrimSpace(d.Body) != "" {
		add("*Context:*\n", d.Body)
	}
	// Left for the intro, the title, the warning, the plan, the context
	// line and the buttons.
	room := maxBlocks - 6
	whole := len(parts) <= room
	if !whole {
		parts = parts[:room]
	}
	sections := make([]any, len(parts))
	for i, part := range parts {
		sections[i] = mrkdwnSection(part)
	}
	return sections, whole
}

func isDraftAction(id string) bool { return id == draftFile || id == draftEdit || id == draftDiscard }

// slackDraftButton handles a draft's File, Edit and Discard. Only the
// requester may: anyone else is refused, and nothing happens. File files
// the draft as them, through the same call as /devbot file; Edit opens that
// form filled in with it; Discard forgets it. What comes of the requester's
// click is told once, where the draft is; a filed or discarded draft is
// removed.
func (s *Server) slackDraftButton(ctx context.Context, in slack.Interaction, click slackClick) any {
	ref := slackRef{Workspace: in.Workspace, User: in.User, Channel: click.Channel}
	tell := func(ctx context.Context, r *slackReply) { s.slackRespond(ctx, ref, click.ResponseURL, r) }
	key := slackDraftKey(in.Workspace, click.Value)
	var d slackDraft
	b, err := s.E.Q.R.Get(ctx, key).Bytes()
	if err == nil {
		err = json.Unmarshal(b, &d)
	}
	switch {
	case errors.Is(err, redis.Nil):
		tell(ctx, ephemeral("This draft is gone: it was filed or discarded, or a day went by."))
		return nil
	case err != nil:
		s.Log.Warn("slack: reading a draft", "workspace", in.Workspace.String(), "err", err)
		tell(ctx, ephemeral("devbot couldn't read the draft. Try again."))
		return nil
	case d.User != in.User:
		tell(ctx, ephemeral(fmt.Sprintf("Only <@%s>, who asked for this draft, can file, edit or discard it.", d.User)))
		return nil
	}
	at := d.ref()
	tell = func(ctx context.Context, r *slackReply) { s.slackTell(ctx, at, r.Text, r.Blocks) }
	// done removes the draft and says what came of it.
	done := func(ctx context.Context, text string) {
		if click.ResponseURL != "" {
			if err := s.Slack.Respond(ctx, click.ResponseURL, map[string]any{"delete_original": true}); err != nil {
				s.Log.Warn("slack: removing a draft", "workspace", in.Workspace.String(), "err", err)
			}
		}
		if text != "" {
			s.slackTell(ctx, at, text, nil)
		}
	}
	c, prompt, err := s.slackCaller(ctx, in.Workspace, in.User, click.Channel)
	if err != nil {
		prompt = s.slackFailed(in.Workspace, err)
	}
	if prompt != nil {
		tell(ctx, prompt)
		return nil
	}
	f := slackForm{Kind: fileForm, Channel: d.Channel, Thread: d.Thread, Repo: d.Draft.Repo, Title: d.Draft.Title,
		AcceptanceCriteria: d.Draft.AcceptanceCriteria, Body: d.Draft.Body}
	if p := d.Draft.Project; p != nil {
		f.Kind, f.Plan, f.Repos = projectForm, p.Plan, p.Repos
	}
	switch click.Action {
	case draftDiscard:
		s.E.Q.R.Del(ctx, key)
		s.slackGo(ctx, func(ctx context.Context) { done(ctx, "Discarded the draft.") })
	case draftEdit:
		f.Draft = click.Value
		s.slackWarmRepos(ctx, in.Workspace, in.User, c)
		bot, err := s.Slack.Bot(ctx, in.Workspace)
		if err == nil {
			_, err = bot.OpenView(ctx, in.TriggerID, f.view())
		}
		if err != nil {
			s.Log.Warn("slack: opening a form", "workspace", in.Workspace.String(), "err", err)
			tell(ctx, ephemeral("devbot couldn't open the form. Try again."))
		}
	case draftFile:
		// Filed as it is only when it was shown whole, whatever the click.
		if _, whole := draftSections(d.Draft); !whole {
			tell(ctx, ephemeral(draftTooLong))
			return nil
		}
		// Taken at once, so a second click doesn't file it twice.
		if n, err := s.E.Q.R.Del(ctx, key).Result(); err != nil || n == 0 {
			tell(ctx, ephemeral("This draft is gone: it was filed or discarded, or a day went by."))
			return nil
		}
		s.slackGo(ctx, func(ctx context.Context) {
			tool, args := f.request(slackOrigin(in.Workspace, d.Channel))
			out, err := s.call(ctx, c, tool, args)
			if err != nil {
				// Kept, so it can be edited and filed again.
				s.E.Q.R.Set(ctx, key, b, slackDraftTTL)
				tell(ctx, s.slackRefusal(ctx, at, slackInteractionPath, err))
				return
			}
			st, _ := out.(core.TaskStatus)
			// What the announcement told the requester says it was filed.
			if s.slackAnnounce(ctx, at, d.Thread, st, f.Kind) {
				done(ctx, "")
			} else {
				done(ctx, fmt.Sprintf("Filed %s: %s.", slackLink(st), slack.Escape(st.Title)))
			}
		})
	}
	return nil
}

// slackForgetIntakes drops what the intake keeps for a workspace that
// uninstalled the app: its requests waiting, drafts and questions asked.
func (s *Server) slackForgetIntakes(ctx context.Context, ws slack.Workspace) error {
	waiting, err := s.E.Q.R.HGetAll(ctx, slackIntakePending).Result()
	if err != nil {
		return err
	}
	for id, raw := range waiting {
		var w slackIntakeWaiting
		if json.Unmarshal([]byte(raw), &w) == nil && w.Workspace != ws {
			continue
		}
		s.E.Q.R.HDel(ctx, slackIntakePending, id)
		s.E.Q.DropIntake(ctx, id)
	}
	for _, pattern := range []string{"devbot:slack:intake-draft:" + wsKey(ws) + ":*", "devbot:slack:intake-asked:" + wsKey(ws) + ":*"} {
		iter := s.E.Q.R.Scan(ctx, 0, pattern, 200).Iterator()
		for iter.Next(ctx) {
			if err := s.E.Q.R.Del(ctx, iter.Val()).Err(); err != nil {
				return err
			}
		}
		if err := iter.Err(); err != nil {
			return err
		}
	}
	return nil
}

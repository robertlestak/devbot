package core

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"git.shdw.tech/rob/devbot/internal/forge"
)

// State labels. Exactly one is on an issue devbot is tracking; none means
// devbot isn't involved.
const (
	LReady      = "devbot/ready"
	LWorking    = "devbot/working"
	LNeedsInfo  = "devbot/needs-info"
	LNeedsHuman = "devbot/needs-human"
	LInReview   = "devbot/in-review"
	LChanges    = "devbot/changes-requested"
	LApproved   = "devbot/approved"
	// LNeedsApproval is work that changes protected files a person may
	// approve (approvable_paths), held unpushed until one does.
	LNeedsApproval = "devbot/needs-approval"
)

var StateLabels = []string{LReady, LWorking, LNeedsInfo, LNeedsHuman, LInReview, LChanges, LApproved, LNeedsApproval}

// LAutomerge asks for one task to be merged once the gate passes, on a repo
// whose `automerge` is off. It is not a state label: it sits beside them and
// transitions leave it alone, so removing it is how a person cancels.
const LAutomerge = "devbot/automerge"

// LRebase asks devbot to rebase a task's approved PR onto its base now. Like
// a `/rebase` comment, it counts only from someone who can direct devbot, and
// devbot takes it off once it has acted on it.
const LRebase = "devbot/rebase"

// LApprove approves the change a task in devbot/needs-approval holds, as
// devbot last showed it. It counts only from someone who can direct devbot,
// never from devbot's own bots, and devbot takes it off once it has acted
// on it.
const LApprove = "devbot/approve"

// LParent marks a parent task (see parent.go): an issue that groups child
// tasks, in any repos, into one change. It is not a state label: a parent
// has no work of its own: no implement or review job is ever queued for it,
// only its integration check (integration.go).
const LParent = "devbot/parent"

var LabelDefs = []forge.Label{
	{Name: LReady, Color: "0e8a16", Description: "devbot: queued for an implementer"},
	{Name: LWorking, Color: "1d76db", Description: "devbot: an implementer is on it"},
	{Name: LNeedsInfo, Color: "fbca04", Description: "devbot: asked a question, waiting for an answer"},
	{Name: LNeedsHuman, Color: "b60205", Description: "devbot: stopped, a human needs to look"},
	{Name: LInReview, Color: "5319e7", Description: "devbot: PR open, in review"},
	{Name: LChanges, Color: "d93f0b", Description: "devbot: changes requested, implementer fixing"},
	{Name: LApproved, Color: "0052cc", Description: "devbot: approved, waiting for a human to merge"},
	{Name: LAutomerge, Color: "006b75", Description: "devbot: merge this task once the merge gate passes"},
	{Name: LNeedsApproval, Color: "e99695", Description: "devbot: holding a change to protected files for a person's approval"},
	{Name: LRebase, Color: "c5def5", Description: "devbot: rebase this task's PR onto its base"},
	{Name: LApprove, Color: "bfdadc", Description: "devbot: push the held change devbot last showed on this issue"},
	{Name: LParent, Color: "c2e0c6", Description: "devbot: a parent task, grouping child tasks across repos"},
}

// State returns the item's devbot state label, or "".
func State(it forge.Item) string {
	for _, l := range StateLabels {
		if it.HasLabel(l) {
			return l
		}
	}
	return ""
}

// Meta is the routing block file_task posts as the bot's first comment on
// the task, out of the issue body, which its author can edit. devbot never
// interprets Origin; it only echoes it on events so the client that filed
// the task can route its own feedback.
type Meta struct {
	Origin          string `json:"origin,omitempty"`
	Requester       string `json:"requester,omitempty"`
	RequesterHandle string `json:"requester_handle,omitempty"`
	// Base is the branch file_task was asked to target; a retarget since
	// overrides it.
	Base string `json:"base,omitempty"`
	// Carry is the PR, by number, that the task carries: one a person
	// handed devbot with devbot/ready, whose tracking issue this is.
	Carry int `json:"carry,omitempty"`
	// Supersedes is the PR, by number, that the task carries on a branch of
	// its own (SupersedeBranch) and a PR of its own, which supersedes it: a
	// fork's, which devbot can't push to. Carry is 0 then.
	Supersedes int `json:"supersedes,omitempty"`
	// Kind is KindParent on a parent task's block, "" on a task's.
	Kind string `json:"kind,omitempty"`
	// Parent, on a child task's block, is its parent task, by its full ref.
	Parent string `json:"parent,omitempty"`
	// Plan, on a parent task's block, has devbot plan its children (see
	// plan.go), from Repos, the candidate repos, by their full refs.
	Plan  bool     `json:"plan,omitempty"`
	Repos []string `json:"repos,omitempty"`
	// Model and ReviewModel are the models file_task (or file_project, for
	// its children and its plan) was asked to run the implementer and the
	// reviewer on; a "model" status since overrides each (see model.go).
	Model       string `json:"model,omitempty"`
	ReviewModel string `json:"review_model,omitempty"`
}

// Status is the hidden marker on every comment devbot posts, which is how
// attempts, fix rounds and pending questions are counted from the forge
// rather than from Redis.
type Status struct {
	Kind      string `json:"kind"` // started|pinned|question|answer|failed|fix-requested|needs-human|approved|queued|head-moved|pr|reset|cancelled|ignored|rebase-requested|rebase-skipped|rerun|wait|wait-refused|released|note|superseded|base-refused|model|model-refused|model-ignored|model-fallback|held|approval|child|completed|building|integration|plan|contract|automerge
	Role      string `json:"role,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	Job       string `json:"job,omitempty"`
	PR        int    `json:"pr,omitempty"`
	SHA       string `json:"sha,omitempty"`
	Principal string `json:"principal,omitempty"`
	// Unchanged, on a "pr" status after a fix round, says the push left the
	// PR's diff as it was: a rebase that needed no conflict resolution.
	Unchanged bool `json:"unchanged,omitempty"`
	// Pin, on a "started" or "pinned" status, is the issue's title and body
	// (PinOf) that devbot was approved to work from.
	Pin string `json:"pin,omitempty"`
	// Base, on a retarget's "reset", is the branch the task's work goes
	// into from then on. On a "pr" status it is the branch the pushed head
	// was built on.
	Base string `json:"base,omitempty"`
	// BaseSHA, on a "pr" status, is the commit of Base the pushed head was
	// built on, its merge base with Base: where a replay of the PR's own
	// commits cuts once Base's history is rewritten under it (BuiltOn).
	BaseSHA string `json:"base_sha,omitempty"`
	// Task is another task, by issue number: on a "wait", the one this task
	// waits on (0 ends the wait); on a "released", the one it waited on; on
	// a "note", the task that sent it. Repo is that task's repo when it is
	// another than the issue's own, and "" when it is the same, as on every
	// marker from before a task of another repo could be named (taskRef).
	Task int    `json:"task,omitempty"`
	Repo string `json:"repo,omitempty"`
	// Until, on a "wait", is its release gate past the other task's merge:
	// "tagged" holds this task until a tag of that task's repo, matching
	// Pattern when there is one, contains the merged commit. On a "released"
	// of mode "tagged", Tag is that tag, and SHA the merged commit.
	Until   string `json:"until,omitempty"`
	Pattern string `json:"pattern,omitempty"`
	Tag     string `json:"tag,omitempty"`
	// Build, on a "wait" with that gate, has this task build against the
	// other's unmerged work until the release (see BuildAgainst); on its
	// "released", it says this task did.
	Build bool `json:"build,omitempty"`
	// Branch, on a "held" status, is the branch the held work goes to once
	// it is approved.
	Branch string `json:"branch,omitempty"`
	// Files, on a "held" or an "approval" status, are the protected files
	// the held commit (SHA) changes, as it leaves them: what an approval
	// approves, and what a later push or the merge gate is held to.
	Files []HeldFile `json:"files,omitempty"`
	// Since, on a parent task's "needs-human", is the comment of the child
	// task (Task, Repo) that said it stopped: each stop is reported once.
	Since int64 `json:"since,omitempty"`
	// Heads, on a parent task's "integration", are the children's PR heads
	// (Head) its Mode is about, sorted: a result counts for exactly those.
	Heads []string `json:"heads,omitempty"`
	// Plan, on a parent task's "plan" of mode proposed, is the plan, and
	// SHA its hash (Plan.Hash). Requests, on a "replan" or a "stopped", are
	// the children's requests to change the contract it is about
	// (requestKey). Item, on a "child" a plan filed, is its place in the plan
	// SHA names, from 1.
	Plan     *Plan    `json:"plan,omitempty"`
	Requests []string `json:"requests,omitempty"`
	Item     int      `json:"item,omitempty"`
	// Children, on a parent task's "automerge" (Mode on or off: its
	// devbot/automerge label as devbot last acted on it), are the child
	// tasks, by full ref, whose label that changed.
	Children []string `json:"children,omitempty"`
	// Model, on a "model" status, is the task's own model for Role from
	// then on, "" for none; on a "model-fallback", the task's model that
	// repos.limits.models refused.
	Model string `json:"model,omitempty"`
}

// taskRef is the task s names, read against own, the repo of the issue
// the marker is on: the zero ref for none, or for a repo that doesn't parse.
func (s Status) taskRef(own forge.RepoRef) forge.ItemRef {
	if s.Task <= 0 {
		return forge.ItemRef{}
	}
	if s.Repo == "" {
		return forge.ItemRef{Repo: own, Number: s.Task}
	}
	r, err := forge.ParseRepo(s.Repo)
	if err != nil {
		return forge.ItemRef{}
	}
	return forge.ItemRef{Repo: r, Number: s.Task}
}

// naming is s naming task ref, for a marker on an issue of own.
func (s Status) naming(own forge.RepoRef, ref forge.ItemRef) Status {
	s.Task, s.Repo = ref.Number, ""
	if ref.Number > 0 && ref.Repo != own {
		s.Repo = ref.Repo.String()
	}
	return s
}

// RefText names task ref as a comment or a run on an issue of own says it:
// "#12" in the same repo, its full ref in another.
func RefText(own forge.RepoRef, ref forge.ItemRef) string {
	if ref.Repo == own {
		return "#" + strconv.Itoa(ref.Number)
	}
	return ref.String()
}

var markerRE = regexp.MustCompile(`(?s)<!--\s*devbot:([a-z0-9-]+)\s+(\{.*?\})\s*-->`)

func render(kind string, v any) string {
	b, _ := json.Marshal(v)
	return fmt.Sprintf("<!-- devbot:%s %s -->", kind, b)
}

// find reads the last marker of kind: devbot writes its own at the very end
// of what it posts, after any text it quotes, so an earlier one is forged.
func find(body, kind string, v any) bool {
	ms := markerRE.FindAllStringSubmatch(body, -1)
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i][1] == kind && json.Unmarshal([]byte(ms[i][2]), v) == nil {
			return true
		}
	}
	return false
}

// Defang makes untrusted text (model or check output, CI descriptions,
// review bodies, whatever a caller sends) safe to post above a devbot
// marker: devbot trusts markers on its own comments and PRs, so the text
// must not carry one, whole or as an opener that swallows the real one
// after it.
func Defang(s string) string {
	return strings.ReplaceAll(stripMarkers(s), "<!--", "&lt;!--")
}

// quote is untrusted text, defanged, as a markdown blockquote. Markdown ends
// a line at a lone "\r" too, which would end the quote and let the rest pass
// for devbot's own words, so every line ending is made "\n" first.
func quote(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	return "> " + strings.ReplaceAll(Defang(s), "\n", "\n> ")
}

// codeText is a name someone chose on the forge (a tag's) as inline code:
// defanged, on one line, and with no backtick to end the code early.
func codeText(s string) string {
	s = strings.Join(strings.Fields(Defang(s)), " ")
	return "`" + strings.ReplaceAll(s, "`", "'") + "`"
}

func RenderMeta(m Meta) string { return render("v1", m) }

func ParseMeta(body string) (Meta, bool) {
	var m Meta
	ok := find(body, "v1", &m)
	return m, ok
}

// metaOf reads a task's routing block from the last comment by one of the
// bots that has one. A human can't forge it, and editing the issue doesn't
// move it.
func metaOf(comments []forge.Comment, bots ...string) (Meta, bool) {
	for i := len(comments) - 1; i >= 0; i-- {
		if !forge.HasLogin(bots, comments[i].Author) {
			continue
		}
		if m, ok := ParseMeta(comments[i].Body); ok {
			return m, true
		}
	}
	return Meta{}, false
}

func RenderStatus(s Status) string { return render("status", s) }

// key identifies what a status says happened, for skipping a repeat of it.
// An edit_task's round is one of its own: it doesn't stand for, or in the
// way of, a round asked for what the head lacks. A head's checks can be
// re-run more than once, so a re-run's count is part of it.
func (s Status) key() string {
	k := fmt.Sprintf("%s|%d|%s", s.Kind, s.PR, s.SHA)
	switch {
	case s.Mode == "edit":
		k += "|edit"
	case s.Kind == "rerun":
		k += fmt.Sprintf("|%d", s.Attempt)
	case s.Kind == "approved" && s.Mode != "":
		// Waiting for others, then not, are two things to say on one head.
		k += "|" + s.Mode
	}
	return k
}

func ParseStatus(body string) (Status, bool) {
	var s Status
	ok := find(body, "status", &s)
	return s, ok
}

// PRMeta links a PR back to its issue, and records the files attached to it
// so a rewritten body keeps them and the reviewer can download them.
type PRMeta struct {
	Issue       int            `json:"issue"`
	Attachments []PRAttachment `json:"attachments,omitempty"`
}

type PRAttachment struct {
	Name    string `json:"name"`
	Caption string `json:"caption,omitempty"`
	URL     string `json:"url"`
	Type    string `json:"type"`   // sniffed MIME type
	SHA256  string `json:"sha256"` // so a re-run doesn't attach the same file twice
}

func RenderPRMeta(m PRMeta) string { return render("pr", m) }

// ParsePRMeta reads the PR body's pr marker, which devbot writes after
// everything model-written.
func ParsePRMeta(body string) (PRMeta, bool) {
	var m PRMeta
	ok := find(body, "pr", &m)
	return m, ok
}

// statusComment is a devbot comment with its parsed marker.
type statusComment struct {
	forge.Comment
	Status
}

func statuses(comments []forge.Comment, bots ...string) []statusComment {
	var out []statusComment
	for _, c := range comments {
		if !forge.HasLogin(bots, c.Author) {
			continue // a human can't forge a status marker
		}
		s, ok := ParseStatus(c.Body)
		if !ok {
			continue
		}
		// Anyone with write access can edit devbot's comments too, and a
		// held change is pushed, a parent's children merged, a plan filed and
		// a model run, on what these say: one edited since devbot posted it
		// says what someone else wrote.
		if (s.Kind == "held" || s.Kind == "approval" || s.Kind == "integration" || s.Kind == "plan" || s.Kind == "model") && c.Edited() {
			continue
		}
		out = append(out, statusComment{c, s})
	}
	return out
}

func lastStatus(ss []statusComment, kinds ...string) (statusComment, bool) {
	for i := len(ss) - 1; i >= 0; i-- {
		if contains(kinds, ss[i].Kind) {
			return ss[i], true
		}
	}
	return statusComment{}, false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// AcceptanceCriteria extracts the "Acceptance criteria" section of a body.
func AcceptanceCriteria(body string) string {
	lines := strings.Split(body, "\n")
	var out []string
	in := false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#") {
			if in {
				break
			}
			in = strings.Contains(strings.ToLower(t), "acceptance criteria")
			continue
		}
		if in {
			out = append(out, l)
		}
	}
	return stripMarkers(strings.Join(out, "\n"))
}

func since(t time.Time) time.Duration { return time.Since(t) }

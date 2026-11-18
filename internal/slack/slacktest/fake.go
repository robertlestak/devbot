// Package slacktest is a fake of the Slack Web API, for tests.
package slacktest

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"

	"git.shdw.tech/rob/devbot/internal/slack"
)

// Call is one call the fake took.
type Call struct {
	Method  string // the Web API method: chat.postMessage, views.open...
	User    string
	Message slack.Message
	TS      string
	Trigger string
	ViewID  string
	View    json.RawMessage
	// Err is what the call failed with.
	Err error
}

// Fake is a slack.API that records each call. Users answer users.info,
// Threads conversations.replies (by "<channel>/<ts>");
// Err, when set, fails every call, and Fail, when set, each call it
// returns an error for.
type Fake struct {
	mu      sync.Mutex
	calls   []Call
	n       int
	Users   map[string]slack.User
	Threads map[string][]slack.Reply
	Err     error
	Fail    func(Call) error
}

var _ slack.API = (*Fake)(nil)

func New() *Fake { return &Fake{Users: map[string]slack.User{}, Threads: map[string][]slack.Reply{}} }

// Calls is every call so far, in order.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// record keeps c and returns a fresh ID for what it made.
func (f *Fake) record(c Call) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.Err
	if f.Fail != nil && err == nil {
		err = f.Fail(c)
	}
	c.Err = err
	f.calls = append(f.calls, c)
	f.n++
	return strconv.Itoa(f.n), err
}

func (f *Fake) PostMessage(ctx context.Context, m slack.Message) (string, error) {
	id, err := f.record(Call{Method: "chat.postMessage", Message: m})
	return "1700000000." + id, err
}

func (f *Fake) UpdateMessage(ctx context.Context, ts string, m slack.Message) error {
	_, err := f.record(Call{Method: "chat.update", TS: ts, Message: m})
	return err
}

func (f *Fake) PostEphemeral(ctx context.Context, user string, m slack.Message) error {
	_, err := f.record(Call{Method: "chat.postEphemeral", User: user, Message: m})
	return err
}

func (f *Fake) OpenView(ctx context.Context, triggerID string, view json.RawMessage) (string, error) {
	id, err := f.record(Call{Method: "views.open", Trigger: triggerID, View: view})
	return "V" + id, err
}

func (f *Fake) UpdateView(ctx context.Context, viewID, hash string, view json.RawMessage) error {
	_, err := f.record(Call{Method: "views.update", ViewID: viewID, View: view})
	return err
}

func (f *Fake) UserInfo(ctx context.Context, user string) (slack.User, error) {
	if _, err := f.record(Call{Method: "users.info", User: user}); err != nil {
		return slack.User{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.Users[user]
	if !ok {
		return slack.User{}, &slack.APIError{Method: "users.info", Code: "user_not_found"}
	}
	return u, nil
}

func (f *Fake) Replies(ctx context.Context, channel, ts string, max int) ([]slack.Reply, bool, error) {
	if _, err := f.record(Call{Method: "conversations.replies", Message: slack.Message{Channel: channel}, TS: ts}); err != nil {
		return nil, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	msgs, ok := f.Threads[channel+"/"+ts]
	if !ok {
		return nil, false, &slack.APIError{Method: "conversations.replies", Code: "thread_not_found"}
	}
	if len(msgs) > max {
		return append([]slack.Reply(nil), msgs[:max]...), true, nil
	}
	return append([]slack.Reply(nil), msgs...), false, nil
}

func (f *Fake) Permalink(ctx context.Context, channel, ts string) (string, error) {
	if _, err := f.record(Call{Method: "chat.getPermalink", Message: slack.Message{Channel: channel}, TS: ts}); err != nil {
		return "", err
	}
	return "https://acme.slack.example/archives/" + channel + "/p" + strings.ReplaceAll(ts, ".", ""), nil
}

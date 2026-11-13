package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// API is the part of Slack's Web API devbot uses, as one workspace's bot.
// Client is Slack's; tests use slacktest.Fake.
type API interface {
	// PostMessage posts m and returns its ts.
	PostMessage(ctx context.Context, m Message) (ts string, err error)
	UpdateMessage(ctx context.Context, ts string, m Message) error
	// PostEphemeral posts m to m.Channel, seen only by user.
	PostEphemeral(ctx context.Context, user string, m Message) error
	// OpenView opens a modal for the interaction triggerID came with, and
	// returns its view ID.
	OpenView(ctx context.Context, triggerID string, view json.RawMessage) (viewID string, err error)
	// UpdateView replaces a modal's view; a non-empty hash makes it fail
	// if the view changed since.
	UpdateView(ctx context.Context, viewID, hash string, view json.RawMessage) error
	UserInfo(ctx context.Context, user string) (User, error)
	// Replies is the thread whose first message is ts in channel, that
	// message first, up to max messages; more says there were others.
	Replies(ctx context.Context, channel, ts string, max int) (msgs []Reply, more bool, err error)
	// Permalink is a message's link.
	Permalink(ctx context.Context, channel, ts string) (string, error)
}

// Message is a message to post or an update to one.
type Message struct {
	Channel  string          `json:"channel"`
	Text     string          `json:"text,omitempty"`
	Blocks   json.RawMessage `json:"blocks,omitempty"`
	ThreadTS string          `json:"thread_ts,omitempty"`
}

// User is the part of users.info's user devbot reads.
type User struct {
	ID       string `json:"id"`
	TeamID   string `json:"team_id"`
	Name     string `json:"name"`
	RealName string `json:"real_name"`
	Deleted  bool   `json:"deleted"`
	IsBot    bool   `json:"is_bot"`
	Profile  struct {
		DisplayName string `json:"display_name"`
	} `json:"profile"`
}

// Reply is the part of a message in conversations.replies devbot reads.
type Reply struct {
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts,omitempty"`
	User     string `json:"user,omitempty"`
	// A bot's message has no user: BotID, and Username when it set one.
	BotID       string       `json:"bot_id,omitempty"`
	Username    string       `json:"username,omitempty"`
	Text        string       `json:"text"`
	Files       []File       `json:"files,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// File is a file shared in a message.
type File struct {
	Name      string `json:"name"`
	Title     string `json:"title"`
	Permalink string `json:"permalink"`
}

// Attachment is a message attachment, or a link's unfurl.
type Attachment struct {
	Title     string `json:"title"`
	TitleLink string `json:"title_link"`
	FromURL   string `json:"from_url"`
	Fallback  string `json:"fallback"`
}

// APIError is a Web API call Slack refused, with its error code.
type APIError struct {
	Method, Code string
	// RetryAfter is, for a call still rate limited after its retries, how
	// long Slack asked to wait.
	RetryAfter time.Duration
}

func (e *APIError) Error() string { return "slack " + e.Method + ": " + e.Code }

const (
	// DefaultRetries is how often a rate-limited call is tried again.
	DefaultRetries = 3
	// DefaultMaxWait is the longest Retry-After waited out; a longer one
	// fails the call.
	DefaultMaxWait = 30 * time.Second
)

// Client is Slack's Web API with one token. A call Slack rate limits (429)
// is tried again after the Retry-After it gives.
type Client struct {
	Token   string
	URL     string // the Web API base, https://slack.com/api/
	HTTP    *http.Client
	Retries int           // default DefaultRetries
	MaxWait time.Duration // default DefaultMaxWait
	// sleep waits out a Retry-After; tests replace it.
	sleep func(context.Context, time.Duration) error
}

var _ API = (*Client)(nil)

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// call calls method with args, a url.Values sent as a form or anything else
// as JSON, as the bot, and decodes the response into out.
func (c *Client) call(ctx context.Context, method string, args, out any) error {
	return c.do(ctx, method, args, out, func(r *http.Request) {
		if c.Token != "" {
			r.Header.Set("Authorization", "Bearer "+c.Token)
		}
	})
}

// callBasic is call authenticated as the app, by its client ID and secret.
func (c *Client) callBasic(ctx context.Context, method, id, secret string, args url.Values, out any) error {
	return c.do(ctx, method, args, out, func(r *http.Request) { r.SetBasicAuth(id, secret) })
}

func (c *Client) do(ctx context.Context, method string, args, out any, authn func(*http.Request)) error {
	var body []byte
	ctype := "application/json; charset=utf-8"
	if f, ok := args.(url.Values); ok {
		body, ctype = []byte(f.Encode()), "application/x-www-form-urlencoded"
	} else {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		body = b
	}
	hc, wait := c.HTTP, c.sleep
	if hc == nil {
		hc = http.DefaultClient
	}
	if wait == nil {
		wait = sleep
	}
	retries, maxWait := c.Retries, c.MaxWait
	if retries == 0 {
		retries = DefaultRetries
	}
	if maxWait == 0 {
		maxWait = DefaultMaxWait
	}
	base := c.URL
	if base == "" {
		base = "https://slack.com/api/"
	}
	for try := 0; ; try++ {
		req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(base, "/")+"/"+method, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", ctype)
		authn(req)
		resp, err := hc.Do(req)
		if err != nil {
			apiError(method, "request_failed")
			return fmt.Errorf("slack %s: %w", method, err)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			apiError(method, "ratelimited")
			// Slack always says how long; a second is its smallest window.
			after := time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
				after = time.Duration(s) * time.Second
			}
			if try >= retries || after > maxWait {
				return &APIError{Method: method, Code: "ratelimited", RetryAfter: after}
			}
			if err := wait(ctx, after); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			apiError(method, "request_failed")
			return fmt.Errorf("slack %s: %w", method, err)
		}
		if resp.StatusCode != http.StatusOK {
			apiError(method, "http_"+strconv.Itoa(resp.StatusCode))
			return &APIError{Method: method, Code: "http_" + strconv.Itoa(resp.StatusCode)}
		}
		var res struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(b, &res); err != nil {
			apiError(method, "bad_response")
			return fmt.Errorf("slack %s: %w", method, err)
		}
		if !res.OK {
			apiError(method, res.Error)
			return &APIError{Method: method, Code: res.Error}
		}
		if out != nil {
			return json.Unmarshal(b, out)
		}
		return nil
	}
}

// PostMessage posts m with unfurling off: a link in text devbot didn't
// write never brings a preview of its page into the channel.
func (c *Client) PostMessage(ctx context.Context, m Message) (string, error) {
	var out struct {
		TS string `json:"ts"`
	}
	err := c.call(ctx, "chat.postMessage", struct {
		Message
		UnfurlLinks bool `json:"unfurl_links"`
		UnfurlMedia bool `json:"unfurl_media"`
	}{Message: m}, &out)
	return out.TS, err
}

func (c *Client) UpdateMessage(ctx context.Context, ts string, m Message) error {
	return c.call(ctx, "chat.update", struct {
		Message
		TS string `json:"ts"`
	}{m, ts}, nil)
}

func (c *Client) PostEphemeral(ctx context.Context, user string, m Message) error {
	return c.call(ctx, "chat.postEphemeral", struct {
		Message
		User string `json:"user"`
	}{m, user}, nil)
}

func (c *Client) OpenView(ctx context.Context, triggerID string, view json.RawMessage) (string, error) {
	var out struct {
		View struct {
			ID string `json:"id"`
		} `json:"view"`
	}
	err := c.call(ctx, "views.open", map[string]any{"trigger_id": triggerID, "view": view}, &out)
	return out.View.ID, err
}

func (c *Client) UpdateView(ctx context.Context, viewID, hash string, view json.RawMessage) error {
	args := map[string]any{"view_id": viewID, "view": view}
	if hash != "" {
		args["hash"] = hash
	}
	return c.call(ctx, "views.update", args, nil)
}

// UserInfo is users.info, which takes only a form.
func (c *Client) UserInfo(ctx context.Context, user string) (User, error) {
	var out struct {
		User User `json:"user"`
	}
	err := c.call(ctx, "users.info", url.Values{"user": {user}}, &out)
	return out.User, err
}

// Replies is conversations.replies, a page at a time. It takes only a
// form.
func (c *Client) Replies(ctx context.Context, channel, ts string, max int) ([]Reply, bool, error) {
	var all []Reply
	cursor := ""
	for {
		f := url.Values{"channel": {channel}, "ts": {ts}, "limit": {strconv.Itoa(min(max, 200))}}
		if cursor != "" {
			f.Set("cursor", cursor)
		}
		var out struct {
			Messages []Reply `json:"messages"`
			Meta     struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.call(ctx, "conversations.replies", f, &out); err != nil {
			return nil, false, err
		}
		all = append(all, out.Messages...)
		if len(all) >= max {
			return all[:max], len(all) > max || out.Meta.NextCursor != "", nil
		}
		if out.Meta.NextCursor == "" {
			return all, false, nil
		}
		cursor = out.Meta.NextCursor
	}
}

// Permalink is chat.getPermalink, which takes only a form.
func (c *Client) Permalink(ctx context.Context, channel, ts string) (string, error) {
	var out struct {
		Permalink string `json:"permalink"`
	}
	err := c.call(ctx, "chat.getPermalink", url.Values{"channel": {channel}, "message_ts": {ts}}, &out)
	return out.Permalink, err
}

// IsCode reports whether err is Slack's error code.
func IsCode(err error, code string) bool {
	var e *APIError
	return errors.As(err, &e) && e.Code == code
}

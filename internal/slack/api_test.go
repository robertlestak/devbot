package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// webAPI is a Web API that rate limits the first limited calls of each
// method, asking for retryAfter, and records every call.
type webAPI struct {
	*httptest.Server
	mu         sync.Mutex
	limited    int
	retryAfter string
	calls      []string // method, content type, auth, body
}

func newWebAPI(t *testing.T, limited int, retryAfter string) *webAPI {
	w := &webAPI{limited: limited, retryAfter: retryAfter}
	w.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		defer w.mu.Unlock()
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		w.calls = append(w.calls, method+" "+r.Header.Get("Content-Type")+" "+r.Header.Get("Authorization")+" "+string(b))
		if w.limited > 0 {
			w.limited--
			if w.retryAfter != "" {
				rw.Header().Set("Retry-After", w.retryAfter)
			}
			rw.WriteHeader(http.StatusTooManyRequests)
			return
		}
		switch method {
		case "chat.postMessage":
			json.NewEncoder(rw).Encode(map[string]any{"ok": true, "ts": "1700000000.000100"})
		case "views.open":
			json.NewEncoder(rw).Encode(map[string]any{"ok": true, "view": map[string]any{"id": "V1"}})
		case "users.info":
			json.NewEncoder(rw).Encode(map[string]any{"ok": true, "user": map[string]any{"id": "U1", "team_id": "T1", "name": "alice", "deleted": true}})
		case "conversations.replies":
			if strings.Contains(string(b), "cursor=c2") {
				json.NewEncoder(rw).Encode(map[string]any{"ok": true, "messages": []any{map[string]any{"ts": "3", "user": "U3", "text": "c"}}})
				return
			}
			json.NewEncoder(rw).Encode(map[string]any{"ok": true, "messages": []any{map[string]any{"ts": "1", "user": "U1", "text": "a",
				"files": []any{map[string]any{"name": "f.txt", "permalink": "https://x/f"}}}, map[string]any{"ts": "2", "bot_id": "B1", "text": "b"}},
				"response_metadata": map[string]any{"next_cursor": "c2"}})
		case "chat.getPermalink":
			json.NewEncoder(rw).Encode(map[string]any{"ok": true, "permalink": "https://acme.slack.com/archives/C1/p1"})
		case "chat.update":
			json.NewEncoder(rw).Encode(map[string]any{"ok": false, "error": "message_not_found"})
		default:
			json.NewEncoder(rw).Encode(map[string]any{"ok": true})
		}
	}))
	t.Cleanup(w.Close)
	return w
}

// client is a Client of w whose waits are recorded, not slept.
func (w *webAPI) client(waits *[]time.Duration) *Client {
	return &Client{Token: "xoxb-t", URL: w.URL + "/api/", sleep: func(_ context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return nil
	}}
}

func TestClientRetriesAfterRateLimits(t *testing.T) {
	w := newWebAPI(t, 2, "7")
	var waits []time.Duration
	ts, err := w.client(&waits).PostMessage(context.Background(), Message{Channel: "C1", Text: "hi"})
	if err != nil || ts != "1700000000.000100" {
		t.Fatalf("%q, %v", ts, err)
	}
	if len(waits) != 2 || waits[0] != 7*time.Second || waits[1] != 7*time.Second {
		t.Fatalf("waited %v", waits)
	}
	if len(w.calls) != 3 || w.calls[2] != `chat.postMessage application/json; charset=utf-8 Bearer xoxb-t {"channel":"C1","text":"hi","unfurl_links":false,"unfurl_media":false}` {
		t.Fatalf("calls %q", w.calls)
	}
	var b bytes.Buffer
	WriteMetrics(&b)
	if !strings.Contains(b.String(), `devbot_slack_api_errors_total{method="chat.postMessage",error="ratelimited"}`) {
		t.Fatalf("metrics:\n%s", b.String())
	}
}

func TestClientGivesUpOnRateLimits(t *testing.T) {
	// Limited past its retries.
	w := newWebAPI(t, 10, "1")
	var waits []time.Duration
	err := w.client(&waits).PostEphemeral(context.Background(), "U1", Message{Channel: "C1", Text: "x"})
	if !IsCode(err, "ratelimited") || len(waits) != DefaultRetries || len(w.calls) != DefaultRetries+1 {
		t.Fatalf("%v after %v, %d calls", err, waits, len(w.calls))
	}
	// Asked to wait longer than it will.
	w = newWebAPI(t, 1, "120")
	waits = nil
	err = w.client(&waits).PostEphemeral(context.Background(), "U1", Message{Channel: "C1", Text: "x"})
	var e *APIError
	if !IsCode(err, "ratelimited") || len(waits) != 0 || !errors.As(err, &e) || e.RetryAfter != 2*time.Minute {
		t.Fatalf("%v after %v", err, waits)
	}
	// A cancelled context stops the wait.
	w = newWebAPI(t, 1, "1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Client{Token: "xoxb-t", URL: w.URL + "/api/"}
	if err := c.PostEphemeral(ctx, "U1", Message{Channel: "C1"}); err == nil {
		t.Fatal("no error with the context cancelled")
	}
}

func TestClientMethods(t *testing.T) {
	w := newWebAPI(t, 0, "")
	var waits []time.Duration
	c := w.client(&waits)
	ctx := context.Background()
	if id, err := c.OpenView(ctx, "tr1", json.RawMessage(`{"type":"modal"}`)); err != nil || id != "V1" {
		t.Fatalf("views.open: %q %v", id, err)
	}
	if err := c.UpdateView(ctx, "V1", "h1", json.RawMessage(`{"type":"modal"}`)); err != nil {
		t.Fatalf("views.update: %v", err)
	}
	u, err := c.UserInfo(ctx, "U1")
	if err != nil || u.Name != "alice" || u.TeamID != "T1" || !u.Deleted {
		t.Fatalf("users.info: %+v %v", u, err)
	}
	if err := c.UpdateMessage(ctx, "1.2", Message{Channel: "C1", Text: "x"}); !IsCode(err, "message_not_found") {
		t.Fatalf("chat.update: %v", err)
	}
	want := []string{
		`views.open application/json; charset=utf-8 Bearer xoxb-t {"trigger_id":"tr1","view":{"type":"modal"}}`,
		`views.update application/json; charset=utf-8 Bearer xoxb-t {"hash":"h1","view":{"type":"modal"},"view_id":"V1"}`,
		`users.info application/x-www-form-urlencoded Bearer xoxb-t user=U1`,
		`chat.update application/json; charset=utf-8 Bearer xoxb-t {"channel":"C1","text":"x","ts":"1.2"}`,
	}
	if strings.Join(w.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(w.calls, "\n"))
	}
	var b bytes.Buffer
	WriteMetrics(&b)
	if !strings.Contains(b.String(), `devbot_slack_api_errors_total{method="chat.update",error="message_not_found"} 1`) {
		t.Fatalf("metrics:\n%s", b.String())
	}
}

// A thread is read a page at a time, up to the most asked for.
func TestClientReplies(t *testing.T) {
	w := newWebAPI(t, 0, "")
	var waits []time.Duration
	c := w.client(&waits)
	ctx := context.Background()
	msgs, more, err := c.Replies(ctx, "C1", "1", 10)
	if err != nil || more || len(msgs) != 3 || msgs[0].Files[0].Permalink != "https://x/f" || msgs[1].BotID != "B1" || msgs[2].Text != "c" {
		t.Fatalf("all of it: %+v %v %v", msgs, more, err)
	}
	if msgs, more, err := c.Replies(ctx, "C1", "1", 2); err != nil || !more || len(msgs) != 2 {
		t.Fatalf("the first two: %+v %v %v", msgs, more, err)
	}
	if link, err := c.Permalink(ctx, "C1", "1"); err != nil || link != "https://acme.slack.com/archives/C1/p1" {
		t.Fatalf("permalink: %q %v", link, err)
	}
	want := []string{
		`conversations.replies application/x-www-form-urlencoded Bearer xoxb-t channel=C1&limit=10&ts=1`,
		`conversations.replies application/x-www-form-urlencoded Bearer xoxb-t channel=C1&cursor=c2&limit=10&ts=1`,
		`conversations.replies application/x-www-form-urlencoded Bearer xoxb-t channel=C1&limit=2&ts=1`,
		`chat.getPermalink application/x-www-form-urlencoded Bearer xoxb-t channel=C1&message_ts=1`,
	}
	if strings.Join(w.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(w.calls, "\n"))
	}
}

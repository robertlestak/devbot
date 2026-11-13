package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/redis/go-redis/v9"
)

// Thread is the Slack thread a task's updates go to: the message devbot
// posted when the task was filed from Slack, or the thread it was filed
// from with the message shortcut, and its replies.
type Thread struct {
	Workspace Workspace `json:"ws"`
	Channel   string    `json:"channel"`
	TS        string    `json:"ts"`
}

// threadsKey is a hash of task ref to its Thread, one for every workspace,
// so whatever follows tasks reads them all at once.
const threadsKey = prefix + "threads"

// Bind records t as task's thread. Only devbot's own Slack handlers write
// one: a task's origin, which its filer chooses, never makes a binding.
func (a *App) Bind(ctx context.Context, task string, t Thread) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return a.o.Redis.HSet(ctx, threadsKey, task, b).Err()
}

// ErrNoThread is a task filed some other way than from Slack.
var ErrNoThread = errors.New("slack: the task has no thread")

// Thread is task's thread, or ErrNoThread.
func (a *App) Thread(ctx context.Context, task string) (Thread, error) {
	b, err := a.o.Redis.HGet(ctx, threadsKey, task).Bytes()
	if errors.Is(err, redis.Nil) {
		return Thread{}, ErrNoThread
	} else if err != nil {
		return Thread{}, err
	}
	var t Thread
	return t, json.Unmarshal(b, &t)
}

// Threads is every task's thread, by task ref.
func (a *App) Threads(ctx context.Context) (map[string]Thread, error) {
	all, err := a.o.Redis.HGetAll(ctx, threadsKey).Result()
	if err != nil {
		return nil, err
	}
	out := make(map[string]Thread, len(all))
	for task, b := range all {
		var t Thread
		if json.Unmarshal([]byte(b), &t) == nil {
			out[task] = t
		}
	}
	return out, nil
}

// unbind forgets the threads in ws, whose messages devbot can no longer
// post to, and what the notifier posted in them.
func (a *App) unbind(ctx context.Context, ws Workspace) error {
	all, err := a.Threads(ctx)
	if err != nil {
		return err
	}
	var gone []string
	for task, t := range all {
		if t.Workspace == ws {
			gone = append(gone, task)
		}
	}
	if len(gone) == 0 {
		return nil
	}
	p := a.o.Redis.TxPipeline()
	p.HDel(ctx, threadsKey, gone...)
	p.HDel(ctx, postsKey, gone...)
	_, err = p.Exec(ctx)
	return err
}

// Respond answers a command through its response_url, with a message as
// a command's reply is: only its user sees it unless it says otherwise. A
// URL that isn't Slack's is refused, so a request can't have devbot post
// elsewhere.
func (a *App) Respond(ctx context.Context, responseURL string, reply any) error {
	if !strings.HasPrefix(responseURL, a.o.HooksURL) {
		return fmt.Errorf("slack: response_url %q isn't under %s", responseURL, a.o.HooksURL)
	}
	b, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", responseURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := a.o.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("slack response_url: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack response_url: %s", resp.Status)
	}
	return nil
}

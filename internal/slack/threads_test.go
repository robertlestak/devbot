package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"git.shdw.tech/rob/devbot/internal/auth"
)

func TestThreadsGoWithTheirWorkspace(t *testing.T) {
	v := newEnv(t, allowedTeam, "T0OTHER")
	ctx := context.Background()
	here := Thread{Workspace: Workspace{Team: allowedTeam}, Channel: "C1", TS: "1700000000.000100"}
	there := Thread{Workspace: Workspace{Team: "T0OTHER"}, Channel: "C9", TS: "1700000000.000200"}
	for task, th := range map[string]Thread{"fake:acme/app#1": here, "fake:acme/app#2": there} {
		if err := v.app.Bind(ctx, task, th); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := v.app.Thread(ctx, "fake:acme/app#1"); err != nil || got != here {
		t.Fatalf("thread: %+v %v", got, err)
	}
	if _, err := v.app.Thread(ctx, "fake:acme/app#3"); !errors.Is(err, ErrNoThread) {
		t.Fatalf("a task never bound: %v", err)
	}
	// Uninstalling, twice, forgets that workspace's threads and no other's.
	for range 2 {
		if err := v.app.Uninstall(ctx, Workspace{Team: allowedTeam}, "app_uninstalled"); err != nil {
			t.Fatal(err)
		}
	}
	all, err := v.app.Threads(ctx)
	if err != nil || len(all) != 1 || all["fake:acme/app#2"] != there {
		t.Fatalf("threads after the uninstall: %+v %v", all, err)
	}
}

func TestRespondOnlyToSlack(t *testing.T) {
	var got []map[string]any
	hooks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		got = append(got, m)
	}))
	t.Cleanup(hooks.Close)
	sealer, _ := auth.NewSealer([]byte(testKey), nil)
	a, err := New(Options{ClientID: "c", ClientSecret: "s", SigningSecret: testSecret, ResourceURL: "http://devbot",
		Redis: redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()}), Sealer: sealer, HooksURL: hooks.URL + "/",
		Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.Respond(ctx, hooks.URL+"/commands/T1/1/x", map[string]string{"response_type": "ephemeral", "text": "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Respond(ctx, "http://elsewhere.example/commands/T1/1/x", map[string]string{"text": "hi"}); err == nil {
		t.Fatal("a response_url that isn't Slack's must be refused")
	}
	if len(got) != 1 || got[0]["text"] != "hi" || got[0]["response_type"] != "ephemeral" {
		t.Fatalf("posted %v", got)
	}
}

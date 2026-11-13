package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/forge/fake"
)

// hookForge accepts every webhook, as one item event for the delivery the
// X-Test-Delivery header names.
type hookForge struct{ *fake.Forge }

func (hookForge) Name() string { return "hook" }

func (hookForge) ParseWebhook(r *http.Request, _ []byte) ([]forge.Event, error) {
	return []forge.Event{{DeliveryID: r.Header.Get("X-Test-Delivery"), Kind: forge.EventItem, Repo: appRepo, Number: 1}}, nil
}

func postHook(t *testing.T, h http.Handler, delivery string) {
	t.Helper()
	r := httptest.NewRequest("POST", "/webhooks/hook", strings.NewReader("{}"))
	r.Header.Set("X-Test-Delivery", delivery)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("webhook %s: %d %s", delivery, w.Code, w.Body.String())
	}
}

// handled records the events a server handles, each held until release
// closes.
type handled struct {
	mu      sync.Mutex
	ids     []string
	release chan struct{}
}

func (h *handled) handle(ctx context.Context, ev forge.Event) error {
	select {
	case <-h.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	h.mu.Lock()
	h.ids = append(h.ids, ev.DeliveryID)
	h.mu.Unlock()
	return nil
}

func (h *handled) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.ids)
}

func runServer(t *testing.T, s *Server) (stop func()) {
	t.Helper()
	s.Cfg.Listen, s.Cfg.MetricsAddr = "127.0.0.1:0", "127.0.0.1:0"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Run didn't return")
		}
	}
}

// A rollout doesn't lose the webhooks serve accepted: on shutdown every
// buffered event is handled before the workers stop.
func TestShutdownDrainsAcceptedEvents(t *testing.T) {
	s, f := newServer(t, "")
	s.E.Forges["hook"] = hookForge{f}
	h := &handled{release: make(chan struct{})}
	s.handleEvent = h.handle
	stop := runServer(t, s)
	const n = 20
	hh := s.Handler()
	for i := range n {
		postHook(t, hh, "d"+string(rune('a'+i)))
	}
	// The workers are all busy, the rest buffered, when shutdown starts.
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(h.release)
	}()
	stop()
	if h.count() != n {
		t.Fatalf("handled %d of %d accepted events", h.count(), n)
	}
}

// Events still unhandled at the drain deadline are given up, and their
// deliveries unmarked, so a redelivery isn't taken for a duplicate.
func TestShutdownDeadlineUnmarksDeliveries(t *testing.T) {
	s, f := newServer(t, "")
	s.E.Forges["hook"] = hookForge{f}
	s.drainTimeout = 50 * time.Millisecond
	h := &handled{release: make(chan struct{})} // never released
	s.handleEvent = h.handle
	stop := runServer(t, s)
	hh := s.Handler()
	ids := []string{"d1", "d2", "d3", "d4", "d5", "d6"}
	for _, id := range ids {
		postHook(t, hh, id)
	}
	stop()
	ctx := context.Background()
	for _, id := range ids {
		if first, err := s.E.Q.FirstDelivery(ctx, id); err != nil || !first {
			t.Errorf("delivery %s is still marked after it was given up (%v)", id, err)
		}
	}
}

// A delivery dropped on a full buffer, or whose handling failed, is handled
// when the forge (or a person) redelivers it.
func TestRedeliveryOfDroppedEventIsHandled(t *testing.T) {
	s, f := newServer(t, "")
	s.E.Forges["hook"] = hookForge{f}
	s.events = make(chan forge.Event, 1)
	h := s.Handler()
	postHook(t, h, "full")
	postHook(t, h, "dropped")
	if got := (<-s.events).DeliveryID; got != "full" {
		t.Fatalf("buffered %q", got)
	}
	m := httptest.NewRecorder()
	s.MetricsHandler().ServeHTTP(m, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(m.Body.String(), `devbot_webhook_events_total{provider="hook",kind="item",result="dropped"} 1`) {
		t.Fatalf("the drop isn't counted:\n%s", m.Body.String())
	}
	postHook(t, h, "dropped")
	if len(s.events) != 1 || (<-s.events).DeliveryID != "dropped" {
		t.Fatal("the redelivery of a dropped event was taken for a duplicate")
	}

	fail := true
	var got []string
	s.handleEvent = func(_ context.Context, ev forge.Event) error {
		got = append(got, ev.DeliveryID)
		if fail {
			fail = false
			return errors.New("forge unavailable")
		}
		return nil
	}
	ctx := context.Background()
	postHook(t, h, "failed")
	s.handle(ctx, <-s.events)
	postHook(t, h, "failed")
	if len(s.events) != 1 {
		t.Fatal("the redelivery of a failed event was taken for a duplicate")
	}
	s.handle(ctx, <-s.events)
	postHook(t, h, "failed")
	if len(s.events) != 0 || len(got) != 2 {
		t.Fatalf("a handled delivery should stay marked: %v, %d buffered", got, len(s.events))
	}
}

// Anyone can post a bad webhook, so they don't each get a warning.
func TestRejectedWebhooksAreRateLimited(t *testing.T) {
	s, _ := newServer(t, "")
	var logs bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&logs, nil))
	h := s.Handler()
	for range 5 {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/webhooks/fake", strings.NewReader("{}")))
		if w.Code != 401 {
			t.Fatalf("a rejected webhook: %d", w.Code)
		}
	}
	if n := strings.Count(logs.String(), "webhook rejected"); n != 1 {
		t.Fatalf("%d warnings for 5 rejected webhooks in a minute:\n%s", n, logs.String())
	}
	s.mu.Lock()
	s.refused["webhook:fake"].last = time.Now().Add(-refusedEvery)
	s.mu.Unlock()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/webhooks/fake", strings.NewReader("{}")))
	if !strings.Contains(logs.String(), "suppressed_since_last=4") {
		t.Fatalf("the next warning says how many were left out:\n%s", logs.String())
	}
	if c := s.webhooks["fake||rejected"]; c != 6 {
		t.Fatalf("the metric counts every one: %g", c)
	}
}

// The loops that retry Redis back off: doubling to the cap, jittered so
// replicas don't retry in step.
func TestBackoff(t *testing.T) {
	for _, c := range []struct {
		fails    int
		lo, high time.Duration
	}{
		{1, 750 * time.Millisecond, time.Second},
		{2, 1500 * time.Millisecond, 2 * time.Second},
		{4, 6 * time.Second, 8 * time.Second},
		{10, 45 * time.Second, time.Minute},
		{1000, 45 * time.Second, time.Minute},
	} {
		seen := map[time.Duration]bool{}
		for range 50 {
			d := backoff(time.Second, time.Minute, c.fails)
			if d < c.lo || d > c.high {
				t.Fatalf("after %d failures: %v, want %v to %v", c.fails, d, c.lo, c.high)
			}
			seen[d] = true
		}
		if len(seen) < 2 {
			t.Errorf("after %d failures every wait is the same: no jitter", c.fails)
		}
	}
}

// A Redis outage doesn't take this process's own series with it.
func TestMetricsWithoutRedis(t *testing.T) {
	s, _ := newServer(t, "")
	s.countWebhook("fake", "item", "accepted")
	s.E.Q.R.Close()
	w := httptest.NewRecorder()
	s.MetricsHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "devbot_redis_up 0\n") || !strings.Contains(body, "devbot_leader 0\n") ||
		!strings.Contains(body, `devbot_webhook_events_total{provider="fake",kind="item",result="accepted"} 1`) ||
		strings.Contains(body, "devbot_queue_depth") {
		t.Fatalf("metrics without Redis: %d %s", w.Code, body)
	}
}

// A subscriber that refuses an event outright (a 4xx) isn't sent it again:
// it is logged and counted, and the events after it go out.
func TestSubscriberPermanentRefusal(t *testing.T) {
	var mu sync.Mutex
	var first string
	var delivered []string
	rx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		id := r.Header.Get("X-Devbot-Event-Id")
		if first == "" {
			first = id
		}
		if id == first {
			http.Error(w, "unknown event type", http.StatusUnprocessableEntity)
			return
		}
		delivered = append(delivered, id)
	}))
	defer rx.Close()
	s, _ := newServer(t, "client")
	logs := &syncBuffer{}
	s.Log = slog.New(slog.NewTextHandler(logs, nil))
	s.deliverEvery = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	token, err := s.E.Q.TakeLeader(ctx, "test", time.Minute)
	if err != nil || token == "" {
		t.Fatalf("lease: %q %v", token, err)
	}
	go s.deliver(ctx, config.Subscriber{Name: "t", URL: rx.URL}, token)
	time.Sleep(50 * time.Millisecond) // the subscriber starts from now
	h := s.Handler()
	for _, title := range []string{"one", "two"} {
		rpc(t, h, "", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"file_task","arguments":{"repo":"fake:acme/app","title":"`+title+`","acceptance_criteria":"- x"}}}`)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(delivered)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the event after a refused one never went out")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stats, _ := s.E.Q.Stats(ctx)
	if stats["subscriber_rejected|t"] != 1 || stats["subscriber_failures|t"] != 0 {
		t.Fatalf("counters: %v", stats)
	}
	cancel()
	if !strings.Contains(logs.String(), "subscriber refused an event") {
		t.Fatalf("not logged: %s", logs.String())
	}
}

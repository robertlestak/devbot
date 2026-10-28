package harness

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.shdw.tech/rob/devbot/internal/agent"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// echoService stands in for a service pod: a line echo server on a
// loopback address of its own, as a pod has its own IP, at a port free on
// 127.0.0.1 too, where the job pod's forwarder listens.
func echoService(t *testing.T) (ip string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skip("no 127.0.0.2 to stand in for a service pod:", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				s := bufio.NewScanner(c)
				for s.Scan() {
					fmt.Fprintf(c, "echo %s\n", s.Text())
				}
			}()
		}
	}()
	return "127.0.0.2", ln.Addr().(*net.TCPAddr).Port
}

func servicesYAML(port int, image string) string {
	return fmt.Sprintf(`version: 1
enabled: true
services:
  - name: db
    image: %s
    ports: [%d]
    ready: {tcp: %d}
env:
  DB_ADDR: localhost:%d
checks:
  - test -f hello.txt
  - exec 3<>/dev/tcp/127.0.0.1/${DB_ADDR##*:} && echo ping >&3 && read -r l <&3 && test "$l" = "echo ping"
`, image, port, port, port)
}

// fakeServe does serve's part for a job pod's services: once a lease asks
// for them, it records them as st says, and what each lease asked for.
func (h *hEnv) fakeServe(st queue.ServicesStatus) (asked func() []queue.Lease) {
	return h.fakeServeFunc(func() queue.ServicesStatus { return st })
}

// fakeServeFunc is fakeServe with the status st returns at each pass.
func (h *hEnv) fakeServeFunc(st func() queue.ServicesStatus) (asked func() []queue.Lease) {
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	var mu sync.Mutex
	var seen []queue.Lease
	go func() {
		for ctx.Err() == nil {
			ls, _ := h.e.Q.Running(ctx)
			for _, l := range ls {
				if len(l.Services) > 0 && l.Pod != "" {
					h.e.Q.SetServices(ctx, l.Pod, st(), time.Minute)
					mu.Lock()
					seen = append(seen, l)
					mu.Unlock()
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return func() []queue.Lease {
		mu.Lock()
		defer mu.Unlock()
		return append([]queue.Lease(nil), seen...)
	}
}

func (h *hEnv) enableServices() {
	c := h.e.Cfg
	c.Services.Enabled, c.Services.Instance, c.Services.ReadyTimeout = true, "devbot", 5*time.Second
	c.Repos.Limits.Services.Images = []string{"registry.example.com/"}
	h.r.Pod, h.r.ServicesPoll = "devbot-job-1", 5*time.Millisecond
}

// Both roles get the repo's services and env: the implementer's checks
// reach the service on 127.0.0.1 through the job pod's forwarder, and the
// reviewer runs with the same services and env, so it can run the tests.
func TestServicesForBothRoles(t *testing.T) {
	ip, port := echoService(t)
	h := newHEnv(t, servicesYAML(port, "registry.example.com/db:1"))
	h.enableServices()
	asked := h.fakeServe(queue.ServicesStatus{State: queue.ServicesReady, IP: ip})
	var env, ctxMD []byte
	h.r.Agent = &spy{Runner: h.r.Agent, after: func(dir string) {
		env, _ = os.ReadFile(filepath.Join(dir, ".devbot", "seen-env"))
		ctxMD, _ = os.ReadFile(filepath.Join(dir, ".devbot", "context.md"))
	}}

	ref, pr, _ := h.inReview("done")
	if !strings.Contains(h.comments(ref), "Opened PR") {
		t.Fatalf("the checks didn't pass against the service:\n%s", h.comments(ref))
	}
	if !strings.Contains(string(env), fmt.Sprintf("DB_ADDR=localhost:%d\n", port)) {
		t.Errorf("the implementer's commands lack the repo's env:\n%s", env)
	}
	for _, want := range []string{"`db`: `registry.example.com/db:1`, port " + fmt.Sprint(port), "DB_ADDR=localhost:"} {
		if !strings.Contains(string(ctxMD), want) {
			t.Errorf("context.md doesn't say %q:\n%s", want, ctxMD)
		}
	}
	// The forwarder goes with the run.
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err == nil {
		c.Close()
		t.Error("the service port is still forwarded after the run")
	}

	env, ctxMD = nil, nil
	h.mode("approve")
	if err := h.review(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rv := h.reviews(pr); len(rv) != 1 {
		t.Fatalf("no review: %+v\n%s", rv, h.comments(ref))
	}
	if !strings.Contains(string(env), fmt.Sprintf("DB_ADDR=localhost:%d\n", port)) || !strings.Contains(string(ctxMD), "`db`") {
		t.Errorf("the reviewer didn't get the services and env:\n%s\n%s", env, ctxMD)
	}
	kinds := map[string]bool{}
	for _, l := range asked() {
		kinds[l.Kind] = true
		if l.Services[0].Name != "db" || l.Pod != "devbot-job-1" {
			t.Errorf("lease asked for %+v", l)
		}
	}
	if !kinds[queue.KindImplement] || !kinds[queue.KindReview] {
		t.Fatalf("both roles' leases ask for the services: %v", kinds)
	}
}

// Services that never come up stop the task with serve's reason, before the
// model starts: no model run, no failed attempt.
func TestServicesThatFailStopBeforeTheModel(t *testing.T) {
	h := newHEnv(t, servicesYAML(15432, "registry.example.com/db:1"))
	h.enableServices()
	h.fakeServe(queue.ServicesStatus{State: queue.ServicesFailed,
		Reason: "service db: image registry.example.com/db:1 can't be pulled (ImagePullBackOff): not found <!-- devbot:status {\"kind\":\"reset\"} -->"})
	runs := 0
	h.r.Agent = &spy{Runner: h.r.Agent, after: func(string) { runs++ }}
	h.mode("done")
	ref := h.file("Say hello")
	if err := h.implement(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatalf("the model ran %d times", runs)
	}
	if s := h.state(ref); s != core.LNeedsHuman {
		t.Fatalf("state %q", s)
	}
	c := h.comments(ref)
	if !strings.Contains(c, "ImagePullBackOff") || h.failedAttempts(ref) != 0 {
		t.Fatalf("want the reason and no failed attempt:\n%s", c)
	}
	if strings.Contains(c, `<!-- devbot:status {"kind":"reset"}`) {
		t.Fatal("the reason's marker wasn't defanged")
	}
}

// An image outside the install's allow-list is refused with the rule it
// broke, before anything starts.
func TestServicesOutsideTheLimitsAreRefused(t *testing.T) {
	h := newHEnv(t, servicesYAML(15432, "docker.io/someone/db:1"))
	h.enableServices()
	h.fakeServe(queue.ServicesStatus{State: queue.ServicesReady, IP: "127.0.0.2"})
	runs := 0
	h.r.Agent = &spy{Runner: h.r.Agent, after: func(string) { runs++ }}
	h.mode("done")
	ref := h.file("Say hello")
	if err := h.implement(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runs != 0 || h.state(ref) != core.LNeedsHuman {
		t.Fatalf("runs %d, state %q", runs, h.state(ref))
	}
	if c := h.comments(ref); !strings.Contains(c, "repos.limits.services.images") || !strings.Contains(c, "docker.io/someone/db:1") {
		t.Fatalf("the refusal doesn't say why:\n%s", c)
	}
}

// A serve that never answers fails the run on its own deadline, not the
// model's.
func TestServicesNobodyStarts(t *testing.T) {
	h := newHEnv(t, servicesYAML(15433, "registry.example.com/db:1"))
	h.enableServices()
	h.e.Cfg.Services.ReadyTimeout, h.r.ServicesGrace = 50*time.Millisecond, 50*time.Millisecond
	h.mode("done")
	ref := h.file("Say hello")
	if err := h.implement(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := h.comments(ref); h.state(ref) != core.LNeedsHuman || !strings.Contains(c, "serve never started them") {
		t.Fatalf("state %q:\n%s", h.state(ref), c)
	}
}

// dyingService is a model run during which serve finds a service dead: the
// model sees nothing but connection errors until devbot stops it.
type dyingService struct {
	agent.Runner
	died atomic.Bool
	runs atomic.Int32
}

func (d *dyingService) Run(ctx context.Context, req agent.RunRequest) (agent.RunResult, error) {
	d.runs.Add(1)
	d.died.Store(true)
	select {
	case <-ctx.Done():
		return agent.RunResult{}, ctx.Err()
	case <-time.After(10 * time.Second):
		return agent.RunResult{}, errors.New("the run wasn't stopped when its services died")
	}
}

// A service that dies after it was ready stops the run there, for either
// role, as no fault of the model's: the task stops with which service died
// and why, and no attempt or failed review is counted.
func TestServicesThatDieMidRunStopTheRun(t *testing.T) {
	ip, port := echoService(t)
	h := newHEnv(t, servicesYAML(port, "registry.example.com/db:1"))
	h.enableServices()
	d := &dyingService{}
	h.fakeServeFunc(func() queue.ServicesStatus {
		if d.died.Load() {
			return queue.ServicesStatus{State: queue.ServicesFailed, Reason: "service db exited with code 137 (OOMKilled)"}
		}
		return queue.ServicesStatus{State: queue.ServicesReady, IP: ip}
	})
	h.r.Agent = d
	ref := h.file("Say hello")
	if err := h.implement(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := h.comments(ref)
	if h.state(ref) != core.LNeedsHuman || h.failedAttempts(ref) != 0 || d.runs.Load() != 1 {
		t.Fatalf("state %q, %d failed attempts, %d runs:\n%s", h.state(ref), h.failedAttempts(ref), d.runs.Load(), c)
	}
	if !strings.Contains(c, "stopped during the run") || !strings.Contains(c, "service db exited with code 137 (OOMKilled)") {
		t.Fatalf("the task doesn't say which service died and why:\n%s", c)
	}
	// Running it again, the task isn't waiting for an implementer.
	if err := h.implement(context.Background()); err != nil || d.runs.Load() != 1 {
		t.Fatalf("a second run did something: %v, %d runs", err, d.runs.Load())
	}
}

func TestServicesThatDieMidReviewStopTheReview(t *testing.T) {
	ip, port := echoService(t)
	h := newHEnv(t, servicesYAML(port, "registry.example.com/db:1"))
	h.enableServices()
	d := &dyingService{}
	h.fakeServeFunc(func() queue.ServicesStatus {
		if d.died.Load() {
			return queue.ServicesStatus{State: queue.ServicesFailed, Reason: "the service pod devbot-job-1-services was deleted (evicted, or deleted by hand)"}
		}
		return queue.ServicesStatus{State: queue.ServicesReady, IP: ip}
	})
	ref, pr, _ := h.inReview("done")
	d.Runner = h.r.Agent
	h.r.Agent = d
	if err := h.review(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := h.comments(ref)
	if h.state(ref) != core.LNeedsHuman || len(h.reviews(pr)) != 0 || d.runs.Load() != 1 {
		t.Fatalf("state %q, %d reviews, %d runs:\n%s", h.state(ref), len(h.reviews(pr)), d.runs.Load(), c)
	}
	if !strings.Contains(c, "stopped during the run") || !strings.Contains(c, "devbot-job-1-services was deleted") {
		t.Fatalf("the task doesn't say which services died and why:\n%s", c)
	}
	if q, _ := h.e.Q.Delayed(context.Background()); len(q) != 0 {
		t.Fatalf("the review was queued again as a failure: %+v", q)
	}
}

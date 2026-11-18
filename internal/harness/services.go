package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/queue"
)

// servicesError is a run's services failing: before the model started (an
// image that can't be pulled, a service that exits, one that is never
// ready), or during the run, after they were ready (a service killed for
// memory, its pod evicted). Another attempt would most likely fail the
// same way, and it is no model's fault, so it stops the task with the
// reason and doesn't count as an attempt.
type servicesError struct {
	reason string
	midRun bool
}

func (e *servicesError) Error() string { return "services: " + e.reason }

// servicesText explains services that failed, for the issue. The reason
// quotes a container's log, so it is fenced.
func servicesText(e *servicesError) string {
	what := "didn't start, so devbot didn't start the model"
	if e.midRun {
		what = "stopped during the run, so devbot stopped it there; it doesn't count as an attempt"
	}
	return fmt.Sprintf("This repo's `services` (%s) %s:\n\n```text\n%s\n```\n\n"+
		"Fix the cause (the image, its settings or the install's limits), then add `%s` to retry.",
		config.RepoFile, what, core.Defang(strings.ReplaceAll(e.reason, "```", "'''")), core.LReady)
}

// badServices stops a task whose .devbot.yaml asks for services the install
// won't run. Another attempt would be refused the same way.
func (r *Runner) badServices(ctx context.Context, settle settleFunc, err error) (string, error) {
	return "config", r.needsHuman(ctx, settle, fmt.Sprintf("This repo's `services` (%s) are outside what the install allows:\n\n```text\n%s\n```\n\n"+
		"Fix it on the default branch, then add `%s` to retry.", config.RepoFile, err, core.LReady))
}

// servicesDied stops a task whose services stopped during its run, which
// ended the run with them: a failed attempt would charge the model for it.
// It returns settle's errCancelled or errLeaseLost; anything else is logged.
func (r *Runner) servicesDied(ctx context.Context, se *servicesError, settle settleFunc) error {
	err := r.needsHuman(ctx, settle, servicesText(se))
	if err != nil && !errors.Is(err, errCancelled) && !errors.Is(err, errLeaseLost) {
		r.Log.Warn("stopping the task after its services stopped", "err", err)
		return nil
	}
	return err
}

// forwarder listens on the job pod's loopback on each of a run's service
// ports and relays every connection to the service pod, so the agent's
// commands and the checks reach a service on 127.0.0.1 at the port it
// listens on, as its own services do inside the service pod.
type forwarder struct {
	mu     sync.Mutex
	target string // the service pod's address
	lns    []net.Listener
	conns  map[net.Conn]bool
	done   bool
	stop   chan struct{} // closed by Close, for watch
	wg     sync.WaitGroup
}

// startServices waits for serve to start the run's services (which this
// job's lease asked for) and forwards their ports. It returns nil when the
// repo has none, and a *servicesError when they fail. Once they are ready
// it watches them, and calls died with a *servicesError if they stop
// before the run ends. Close the forwarder when the run ends; serve deletes
// the pod once the lease is gone.
func (r *Runner) startServices(ctx context.Context, died context.CancelCauseFunc, rc *config.Repo) (*forwarder, error) {
	if len(rc.Services) == 0 {
		return nil, nil
	}
	if r.Pod == "" {
		return nil, &servicesError{reason: "the job isn't running in a pod (DEVBOT_POD_NAME is unset), so serve can't start its services"}
	}
	// The ports are the run's from the start, whatever the agent does with
	// its loopback meanwhile.
	f := &forwarder{conns: map[net.Conn]bool{}, stop: make(chan struct{})}
	for _, s := range rc.Services {
		for _, p := range s.Ports {
			if err := f.listen(p); err != nil {
				f.Close()
				return nil, &servicesError{reason: fmt.Sprintf("service %s: can't listen on port %d in the job pod: %v", s.Name, p, err)}
			}
		}
	}
	// serve fails them at ready_timeout; past that it isn't answering.
	deadline := time.Now().Add(r.E.Cfg.Services.ReadyTimeout + r.ServicesGrace)
	last := ""
	for {
		st, ok, err := r.E.Q.Services(ctx, r.Pod)
		switch {
		case err != nil:
			r.Log.Warn("services: reading their status", "err", err)
		case ok && st.State == queue.ServicesReady && st.IP != "":
			r.Log.Info("services ready", "ip", st.IP)
			f.mu.Lock()
			f.target = st.IP
			f.mu.Unlock()
			f.wg.Go(func() { r.watchServices(ctx, f, st.IP, died) })
			return f, nil
		case ok && st.State == queue.ServicesFailed:
			f.Close()
			return nil, &servicesError{reason: st.Reason}
		case ok && st.Reason != last:
			r.Log.Info("waiting for services", "status", st.Reason)
			last = st.Reason
		}
		if time.Now().After(deadline) {
			f.Close()
			why := "serve never started them"
			if last != "" {
				why = "they weren't ready in time: " + last
			}
			return nil, &servicesError{reason: fmt.Sprintf("%s (services.ready_timeout is %s)", why, r.E.Cfg.Services.ReadyTimeout)}
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(r.ServicesPoll):
		}
	}
}

// watchServices reads the services' status until the forwarder closes, and
// stops the run through died when serve says they failed (a service
// exited, its pod was evicted or deleted) or moved them to another
// address: the model would otherwise see connection errors it can't fix,
// and spend its attempt trying. A status it can't read is no news, as when
// Redis is out of reach; serve writes it again once it is back.
func (r *Runner) watchServices(ctx context.Context, f *forwarder, ip string, died context.CancelCauseFunc) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.stop:
			return
		case <-time.After(r.ServicesPoll):
		}
		st, ok, err := r.E.Q.Services(ctx, r.Pod)
		why := ""
		switch {
		case err != nil || !ok:
		case st.State == queue.ServicesFailed:
			why = st.Reason
		case st.State == queue.ServicesReady && st.IP != ip:
			why = fmt.Sprintf("the services were started again at another address (%s, not %s), without what the run had put in them", st.IP, ip)
		}
		if why != "" {
			r.Log.Warn("services stopped during the run", "reason", why)
			died(&servicesError{reason: why, midRun: true})
			return
		}
	}
}

// listen takes port on the IPv4 loopback, and on the IPv6 one where the pod
// has it, since "localhost" may resolve to either.
func (f *forwarder) listen(port int) error {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	f.serve(ln, port)
	if ln6, err := net.Listen("tcp", net.JoinHostPort("::1", strconv.Itoa(port))); err == nil {
		f.serve(ln6, port)
	}
	return nil
}

func (f *forwarder) serve(ln net.Listener, port int) {
	f.mu.Lock()
	f.lns = append(f.lns, ln)
	f.mu.Unlock()
	f.wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.wg.Go(func() { f.relay(c, port) })
		}
	})
}

func (f *forwarder) relay(c net.Conn, port int) {
	f.mu.Lock()
	target, done := f.target, f.done
	if !done {
		f.conns[c] = true
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.conns, c)
		f.mu.Unlock()
		c.Close()
	}()
	// Before the services are ready there is nothing to reach.
	if done || target == "" {
		return
	}
	up, err := net.DialTimeout("tcp", net.JoinHostPort(target, strconv.Itoa(port)), 10*time.Second)
	if err != nil {
		return
	}
	f.mu.Lock()
	if f.done {
		f.mu.Unlock()
		up.Close()
		return
	}
	f.conns[up] = true
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.conns, up)
		f.mu.Unlock()
		up.Close()
	}()
	var wg sync.WaitGroup
	wg.Go(func() { pipe(up, c) })
	pipe(c, up)
	wg.Wait()
}

// pipe copies until either side ends, then half-closes dst, so the other
// direction still finishes what it is sending.
func pipe(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	if tc, ok := dst.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	} else {
		_ = dst.Close()
	}
}

// Close stops listening and drops every connection, and waits for them.
// It may be called on nil, and more than once.
func (f *forwarder) Close() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	if f.done {
		f.mu.Unlock()
		return nil
	}
	f.done = true
	close(f.stop)
	var errs []error
	for _, ln := range f.lns {
		errs = append(errs, ln.Close())
	}
	for c := range f.conns {
		c.Close()
	}
	f.mu.Unlock()
	f.wg.Wait()
	return errors.Join(errs...)
}

// servicesContext tells the model about the run's services and env, which
// are as much this run's as its checkout.
func servicesContext(rc *config.Repo) string {
	var b strings.Builder
	if len(rc.Services) > 0 {
		b.WriteString("Services, started for this run alone and ready before you started (from `services` in .devbot.yaml). " +
			"Each listens on 127.0.0.1 (localhost) at its ports, for your commands and the checks alike; run the tests against them:\n\n")
		for _, s := range rc.Services {
			ports := "no ports"
			if len(s.Ports) > 0 {
				var ps []string
				for _, p := range s.Ports {
					ps = append(ps, strconv.Itoa(p))
				}
				ports = "port " + strings.Join(ps, ", ")
			}
			fmt.Fprintf(&b, "- `%s`: `%s`, %s\n", s.Name, config.NormalizeImage(s.Image), ports)
		}
		b.WriteString("\n")
	}
	if len(rc.Env) > 0 {
		b.WriteString("Environment devbot sets for your commands and the checks (`env` in .devbot.yaml):\n\n```bash\n")
		for _, k := range slices.Sorted(maps.Keys(rc.Env)) {
			fmt.Fprintf(&b, "%s=%s\n", k, rc.Env[k])
		}
		b.WriteString("```\n\n")
	}
	return b.String()
}

// Package server is `devbot serve`: webhook ingest, the connectors (HTTP API
// and MCP), metrics, the reconcile loop and outbound event delivery.
package server

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	mrand "math/rand/v2"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"git.shdw.tech/rob/devbot/internal/audit"
	"git.shdw.tech/rob/devbot/internal/auth"
	"git.shdw.tech/rob/devbot/internal/config"
	"git.shdw.tech/rob/devbot/internal/core"
	"git.shdw.tech/rob/devbot/internal/forge"
	"git.shdw.tech/rob/devbot/internal/kube"
	"git.shdw.tech/rob/devbot/internal/queue"
)

type Server struct {
	E       *core.Engine
	Cfg     *config.Config
	Log     *slog.Logger
	Version string

	// Auth is the OAuth broker in auth.mode oauth; nil in mode none.
	Auth   *auth.Server
	events chan forge.Event

	// Pods labels job pods with their attribution, when pod_labels is on.
	Pods     PodLabeler
	labelled map[string]string // pod -> what it was labelled with
	// ServicePods runs job pods' service pods, when services are on.
	ServicePods ServicePods
	// JobPods reads job pods, with pod_labels or services on: how a job's agent
	// container ended, for a harness whose agent stopped answering.
	JobPods JobPods

	mu       sync.Mutex
	webhooks map[string]float64 // provider|kind|result -> count
	// refused is, per kind of refused request, when one was last logged
	// and how many have gone unlogged since.
	refused map[string]*quietLog
	titles  titleCache
	// owner names this process in the leader lease; leading is whether it
	// holds it now.
	owner   string
	leading atomic.Bool
	// deliverEvery is how often a subscriber's delivery looks for new
	// events.
	deliverEvery time.Duration
	// handleEvent handles a webhook event: E.HandleEvent, but for tests.
	handleEvent func(context.Context, forge.Event) error
	// drainTimeout is how long a shutdown handles accepted events for.
	drainTimeout time.Duration
	// now is the clock kept webhook events come due by, and laterEvery how
	// often each replica looks for them.
	now        func() time.Time
	laterEvery time.Duration
}

func New(e *core.Engine, cfg *config.Config, log *slog.Logger, version string) (*Server, error) {
	host, _ := os.Hostname()
	s := &Server{E: e, Cfg: cfg, Log: log, Version: version, owner: host + "-" + rand.Text()[:8],
		events: make(chan forge.Event, 1024), webhooks: map[string]float64{}, labelled: map[string]string{},
		deliverEvery: 5 * time.Second, drainTimeout: drainTimeout, now: time.Now, laterEvery: 5 * time.Second}
	s.handleEvent = e.HandleEvent
	if cfg.PodLabels.Enabled {
		k, err := kube.InCluster()
		if err != nil {
			return nil, fmt.Errorf("pod_labels: %w", err)
		}
		s.Pods = k
		s.JobPods = k
	}
	if cfg.Services.Enabled {
		k, err := kube.InCluster()
		if err != nil {
			return nil, fmt.Errorf("services: %w", err)
		}
		s.ServicePods = k
		s.JobPods = k
	}
	if cfg.Auth.OAuth() {
		p, ok := e.Forges[cfg.Auth.Provider]
		if !ok {
			return nil, fmt.Errorf("auth.provider %q isn't configured", cfg.Auth.Provider)
		}
		o := AuthOptions(cfg, p, e.Q.R, log)
		o.Audit = e.Audit
		a, err := auth.New(o)
		if err != nil {
			return nil, fmt.Errorf("auth: %w", err)
		}
		s.Auth = a
		if cfg.Auth.DynamicDeviceGrant && len(cfg.Proxies) == 0 {
			log.Warn("auth.dynamic_device_grant is on with no trusted_proxies: behind a proxy, every client shares one per-address limit, " +
				"so ten wrong device codes from anyone block `devbot login` for everyone for ten minutes. List the proxies in trusted_proxies (docs/operating.md, Behind a proxy).")
		}
	}
	return s, nil
}

// AuthOptions is the OAuth broker's configuration, from the install's.
func AuthOptions(cfg *config.Config, p forge.Provider, r *redis.Client, log *slog.Logger) auth.Options {
	rate := func(l config.RateLimit) auth.Rate { return auth.Rate{PerMinute: l.PerMinute, Burst: l.Burst} }
	rl := cfg.Auth.RateLimits
	return auth.Options{
		ResourceURL: cfg.Auth.ResourceURL, Forge: p, Redis: r, Log: log,
		Key: []byte(config.Env(cfg.Auth.KeyEnv)), PreviousKey: []byte(config.Env(cfg.Auth.PreviousKeyEnv)),
		AccessTTL: cfg.Auth.AccessTokenTTL, RefreshTTL: cfg.Auth.RefreshTokenTTL, RefreshReuseGrace: cfg.Auth.ReuseGrace(), DeviceGrant: cfg.Auth.DynamicDeviceGrant,
		TrustedProxies: cfg.Proxies,
		Limits: auth.Limits{Register: rate(rl.Register), Authorize: rate(rl.Authorize), DeviceAuthorization: rate(rl.DeviceAuthorization),
			Token: rate(rl.Token), Device: rate(rl.Device), ClientsPerAddress: rl.ClientsPerAddress},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Liveness: the process answers. A Redis outage doesn't make restarting
	// serve help, so it isn't checked here.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	// Readiness: without Redis this replica can't queue, authenticate or
	// lead, so the Service sends its requests to one that can.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.E.Q.R.Ping(r.Context()).Err(); err != nil {
			s.Log.Warn("readyz", "err", err)
			http.Error(w, "redis unavailable", 503)
			return
		}
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("POST /webhooks/{provider}", s.webhook)

	api := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, s.connector(s.auth(h))) }
	api("POST /api/tasks", s.apiFile)
	api("POST /api/projects", s.apiFileProject)
	api("GET /api/tasks", s.apiList)
	api("GET /api/task", s.apiStatus)
	api("POST /api/task/answer", s.apiAnswer)
	api("POST /api/task/cancel", s.apiCancel)
	api("POST /api/task/retry", s.apiRetry)
	api("POST /api/task/rebase", s.apiRebase)
	api("POST /api/task/approve", s.apiApprove)
	api("POST /api/task/retarget", s.apiRetarget)
	api("POST /api/task/edit", s.apiEdit)
	api("POST /api/task/wait", s.apiWait)
	api("GET /api/events", s.apiEvents)
	api("GET /api/events/recent", s.apiRecentEvents)
	api("GET /api/jobs", s.apiJobs)
	api("GET /api/task/timeline", s.apiTimeline)
	api("GET /api/status", s.apiQueueStatus)
	if s.Auth != nil {
		s.Auth.Routes(mux)
	}
	if s.Cfg.UI.Enabled {
		s.uiRoutes(mux)
	}

	mux.HandleFunc("POST /mcp", s.connector(s.mcp))
	mux.HandleFunc("GET /mcp", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "this MCP server does not stream; POST JSON-RPC requests", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("DELETE /mcp", s.connector(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	return withRequestID(mux)
}

// requestIDRE is a request ID devbot takes from a proxy in front of it.
var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// withRequestID gives every request an ID, the proxy's X-Request-Id when it
// sent a sane one, else one made up. The response echoes it, and the logs
// and the audit records of what the request did carry it, so a caller told
// only that something failed can say which call it was.
func withRequestID(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !requestIDRE.MatchString(id) {
			id = rand.Text()[:16]
		}
		w.Header().Set("X-Request-Id", id)
		h.ServeHTTP(w, r.WithContext(audit.With(r.Context(), audit.Fields{Request: id})))
	})
}

func requestID(r *http.Request) string {
	return audit.From(r.Context()).Request
}

// connector keeps browsers on other sites away from /mcp and /api. A
// browser names the page's origin on a cross-origin request and on any
// POST; one not allowed is refused. A POST must be JSON: a page can send a
// form or text/plain cross-origin without asking first, but not
// application/json. Neither stops DNS rebinding's reads: the attacker's
// page points its own name at serve, so its GETs are same-origin and carry
// no Origin. Their Host is the attacker's name, though, which knownHost
// refuses.
func (s *Server) connector(h http.HandlerFunc) http.HandlerFunc {
	return s.knownHost(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && !s.originAllowed(o) {
			http.Error(w, "cross-origin requests aren't allowed; see allowed_origins", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost {
			if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
				http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		h(w, r)
	})
}

// knownHost refuses, in auth mode none, a request whose Host isn't a name
// serve answers to. Mode oauth needs no such check: a rebinding page has
// no devbot token or session cookie to send.
func (s *Server) knownHost(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Auth == nil && !s.hostAllowed(r.Host) {
			s.warnRefused("host", "request to an unknown host refused", "host", r.Host, "path", r.URL.Path, "remote", r.RemoteAddr)
			http.Error(w, "devbot doesn't answer to this host name; see allowed_hosts", http.StatusMisdirectedRequest)
			return
		}
		h(w, r)
	}
}

// hostAllowed: an IP address or localhost, which no rebinding page's name
// is, or auth.resource_url's, allowed_origins' or allowed_hosts' host.
func (s *Server) hostAllowed(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil {
		return true
	}
	for _, a := range s.Cfg.AllowedHosts {
		if strings.EqualFold(host, strings.TrimSuffix(a, ".")) {
			return true
		}
	}
	for _, a := range append([]string{s.Cfg.Auth.ResourceURL}, s.Cfg.AllowedOrigins...) {
		if u, err := url.Parse(a); err == nil && u.Hostname() != "" && strings.EqualFold(host, u.Hostname()) {
			return true
		}
	}
	return false
}

// originAllowed: serve's own origin, from auth.resource_url, and those in
// allowed_origins. "null", an opaque origin, is never one of them.
func (s *Server) originAllowed(o string) bool {
	for _, a := range append([]string{s.Cfg.Auth.ResourceURL}, s.Cfg.AllowedOrigins...) {
		if u, err := url.Parse(a); err == nil && u.Host != "" && strings.EqualFold(o, u.Scheme+"://"+u.Host) {
			return true
		}
	}
	return false
}

// MetricsHandler serves /metrics, on its own listener (metrics_addr): its
// labels name repos, and the connector listener may be public.
func (s *Server) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", s.metrics)
	return mux
}

// Run serves until ctx ends, with the background loops alongside. Then it
// stops the listeners, finishing the requests they have, handles the
// webhook events already accepted, for at most drainTimeout, and returns.
func (s *Server) Run(ctx context.Context) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	// The workers outlive ctx, to drain what the listeners accepted.
	wctx, cancelWorkers := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWorkers()
	drain := make(chan struct{})
	workers := s.workers(wctx, drain)
	loops := s.background(ctx)
	// Waited for after the listeners stop, so leadership is resigned before
	// this replica exits.
	defer loops.Wait()
	srvs := []*http.Server{httpServer(s.Cfg.Listen, s.Handler()), httpServer(s.Cfg.MetricsAddr, s.MetricsHandler())}
	errs := make(chan error, len(srvs))
	for _, srv := range srvs {
		go func() {
			err := srv.ListenAndServe()
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			errs <- err
			stop() // one listener failing takes serve down
		}()
	}
	s.Log.Info("serving", "addr", s.Cfg.Listen, "metrics_addr", s.Cfg.MetricsAddr, "version", s.Version)
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	for _, srv := range srvs {
		if err := srv.Shutdown(sctx); err != nil {
			s.Log.Warn("stopping a listener", "addr", srv.Addr, "err", err)
		}
	}
	s.drainEvents(drain, workers, cancelWorkers)
	return errors.Join(<-errs, <-errs)
}

// A shutdown's deadlines: the listeners' requests get httpShutdownTimeout
// to finish, then the accepted webhook events drainTimeout to be handled.
// The chart's terminationGracePeriodSeconds (45) covers both after its
// preStop sleep (5), so Kubernetes doesn't kill serve mid-drain.
const (
	httpShutdownTimeout = 10 * time.Second
	drainTimeout        = 20 * time.Second
)

// drainEvents has the workers handle every buffered event and stop, and
// cancels them at the drain deadline. An event it couldn't handle has its
// delivery unmarked, so the forge's redelivery is handled.
func (s *Server) drainEvents(drain chan struct{}, workers *sync.WaitGroup, cancel context.CancelFunc) {
	close(drain)
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(s.drainTimeout):
		s.Log.Warn("shutdown: webhook events still unhandled at the deadline; their deliveries are unmarked for redelivery",
			"buffered", len(s.events))
		cancel()
		<-done
	}
	for {
		select {
		case ev := <-s.events:
			s.forget(ev)
		default:
			return
		}
	}
}

// httpServer bounds how long a slow client can hold a connection: the
// request read in 30s, the answer written within 2m (a connector call may
// make several forge calls), and 2m idle between requests. No endpoint
// streams; one that did would lift its write deadline with
// http.ResponseController.
func httpServer(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: 2 * time.Minute}
}

// ---- webhooks ----

func (s *Server) countWebhook(provider, kind, result string) {
	s.mu.Lock()
	s.webhooks[provider+"|"+kind+"|"+result]++
	s.mu.Unlock()
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	p, ok := s.E.Forges[name]
	if !ok {
		http.Error(w, "unknown provider", 404)
		return
	}
	// Anyone can post here, so the body is bounded before it is read, let
	// alone checked against its signature.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.Cfg.WebhookMaxBytes))
	if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
		s.countWebhook(name, "", "too_large")
		http.Error(w, "body over webhook_max_bytes", http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		http.Error(w, "couldn't read the body", 400)
		return
	}
	evs, err := p.ParseWebhook(r, body)
	if err != nil {
		s.countWebhook(name, "", "rejected")
		// One record a minute per forge: anyone can post here.
		if ok, quiet := s.logRefused("webhook:" + name); ok {
			s.E.Audit.Log(r.Context(), audit.Record{Event: "authz.refused", Outcome: audit.Refused, Err: err,
				Detail: map[string]any{"action": "webhook", "provider": name, "reason": "rejected delivery", "remote": r.RemoteAddr, "suppressed_since_last": quiet}})
			s.Log.Warn("webhook rejected", "provider", name, "err", err, "remote", r.RemoteAddr, "suppressed_since_last", quiet)
		}
		http.Error(w, "rejected", 401)
		return
	}
	for _, ev := range evs {
		first, err := s.E.Q.FirstDelivery(r.Context(), ev.DeliveryID)
		if err == nil && !first {
			s.countWebhook(name, string(ev.Kind), "duplicate")
			continue
		}
		if ev.Kind == forge.EventIgnored {
			s.countWebhook(name, string(ev.Kind), "ignored")
			continue
		}
		select {
		case s.events <- ev:
			s.countWebhook(name, string(ev.Kind), "accepted")
		default:
			// The reconciler will catch up on whatever this would have done,
			// and a redelivery is handled.
			s.countWebhook(name, string(ev.Kind), "dropped")
			s.forget(ev)
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

// forget unmarks ev's delivery, accepted but not handled, so a redelivery
// of it (the forge's, or a person's) isn't taken for a duplicate.
func (s *Server) forget(ev forge.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.E.Q.ForgetDelivery(ctx, ev.DeliveryID); err != nil {
		s.Log.Warn("unmarking a webhook delivery", "delivery", ev.DeliveryID, "err", err)
	}
}

// worker handles webhook events until ctx ends, or, once drain closes,
// until none are left.
func (s *Server) worker(ctx context.Context, drain <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-s.events:
			s.handle(ctx, ev)
		case <-drain:
			for {
				select {
				case ev := <-s.events:
					s.handle(ctx, ev)
				default:
					return
				}
			}
		}
	}
}

func (s *Server) handle(ctx context.Context, ev forge.Event) { s.handleTry(ctx, ev, 0) }

// maxEventTries is how many times a rate limit may put one webhook event
// off before it is dropped, and left to reconcile.
const maxEventTries = 5

// handleTry handles ev, which a rate limit has put off tries times. One
// that hits a rate limit again (or is deferred for a low quota) is kept in
// Redis until the limit resets, rather than lost: its delivery stays
// marked, so the forge's redeliveries are duplicates of it.
func (s *Server) handleTry(ctx context.Context, ev forge.Event, tries int) {
	if ctx.Err() != nil {
		if tries > 0 {
			s.keep(ev, tries, s.now(), "")
		} else {
			s.forget(ev)
		}
		return
	}
	ectx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ectx = audit.With(ectx, audit.Fields{Request: ev.DeliveryID})
	err := s.handleEvent(ectx, ev)
	if err == nil {
		return
	}
	if rl := (*forge.RateLimitError)(nil); errors.As(err, &rl) {
		s.Log.Info("webhook event put off for the forge's rate limit", "repo", ev.Repo, "kind", ev.Kind, "number", ev.Number, "until", rl.Until, "tries", tries+1, "err", err)
		s.keep(ev, tries+1, rl.Until, rl.Kind)
		return
	}
	if tries > 0 && ctx.Err() != nil {
		// Stopped mid-way: a kept event has no redelivery to fall back on.
		s.keep(ev, tries, s.now(), "")
		return
	}
	s.Log.Warn("handling event", "repo", ev.Repo, "kind", ev.Kind, "number", ev.Number, "err", err)
	s.forget(ev)
}

// keep puts ev in Redis to be handled at at (queue.Later), and counts what
// came of it by kind, the rate limit's kind. One it can't keep, or that
// has used up its tries, is forgotten, so a redelivery is handled.
func (s *Server) keep(ev forge.Event, tries int, at time.Time, kind string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := json.Marshal(ev)
	if err != nil {
		s.forget(ev)
		return
	}
	outcome, err := s.E.Q.Later(ctx, laterKey(ev), b, at, tries, maxEventTries)
	if err != nil {
		s.Log.Warn("keeping a rate-limited webhook event", "repo", ev.Repo, "kind", ev.Kind, "number", ev.Number, "err", err)
		s.forget(ev)
		return
	}
	switch {
	case outcome == queue.LaterDropped:
		s.Log.Warn("dropping a webhook event the forge's rate limit put off too often; reconcile makes up for it",
			"repo", ev.Repo, "kind", ev.Kind, "number", ev.Number, "tries", tries)
		s.forget(ev)
	case outcome == queue.LaterKept && kind == forge.LimitLowQuota:
		outcome = "deferred"
	}
	if kind != "" || outcome == queue.LaterDropped {
		s.E.Count(ctx, "webhook_requeue", []string{outcome}, 1)
	}
}

// laterKey names what ev is about, so that of the events about one item a
// rate limit put off, one is handled: its evaluation reads everything
// again. A comment's stands alone, as it may be a command another event
// doesn't carry.
func laterKey(ev forge.Event) string {
	if ev.Comment != "" && ev.DeliveryID != "" {
		return "delivery:" + ev.DeliveryID
	}
	return fmt.Sprintf("%s|%s|%d|%s", ev.Kind, ev.Repo, ev.Number, ev.SHA)
}

// laterBatch is how many kept events a replica takes at a time.
const laterBatch = 16

// handleLater handles the kept webhook events that are due, until none
// are. Each is taken by one replica.
func (s *Server) handleLater(ctx context.Context) {
	for ctx.Err() == nil {
		kept, err := s.E.Q.TakeLater(ctx, s.now(), laterBatch)
		if err != nil {
			s.Log.Warn("taking kept webhook events", "err", err)
			return
		}
		for _, k := range kept {
			var ev forge.Event
			if err := json.Unmarshal(k.Payload, &ev); err != nil {
				continue
			}
			s.handleTry(ctx, ev, k.Tries)
		}
		if len(kept) < laterBatch {
			return
		}
	}
}

// laterLoop handles kept webhook events as they come due, on every replica.
func (s *Server) laterLoop(ctx context.Context) {
	t := time.NewTicker(s.laterEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.handleLater(ctx)
		}
	}
}

// workers starts the webhook workers, which every replica runs.
func (s *Server) workers(ctx context.Context, drain <-chan struct{}) *sync.WaitGroup {
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Go(func() { s.worker(ctx, drain) })
	}
	return &wg
}

// background starts the requested evaluations and the campaign for the
// singleton loops, until ctx ends.
func (s *Server) background(ctx context.Context) *sync.WaitGroup {
	var wg sync.WaitGroup
	// Requested evaluations are popped atomically, so every replica helps.
	wg.Go(func() { s.evalLoop(ctx) })
	// Kept events are taken atomically too.
	wg.Go(func() { s.laterLoop(ctx) })
	wg.Go(func() { s.campaign(ctx) })
	return &wg
}

func (s *Server) reconcileLoop(ctx context.Context) {
	t := time.NewTimer(min(5*time.Second, s.Cfg.ReconcileInterval))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		start := time.Now()
		rctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		n, err := s.E.Reconcile(rctx)
		cancel()
		if ctx.Err() != nil {
			return // stopped leading mid-pass; the next leader starts over
		}
		if err := s.E.Q.SetLastReconcile(ctx, time.Now()); err != nil {
			s.Log.Warn("recording the reconcile", "err", err)
		}
		if err != nil {
			s.Log.Warn("reconcile", "acted", n, "err", err, "took", time.Since(start))
		} else if n > 0 {
			s.Log.Info("reconcile", "acted", n, "took", time.Since(start))
		}
		t.Reset(s.Cfg.ReconcileInterval)
	}
}

// releaseLoop moves due delayed jobs back onto their queues every 10s, so
// backoff is honoured on time whatever the reconcile interval.
func (s *Server) releaseLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.E.Q.ReleaseDue(ctx); err != nil {
				s.Log.Warn("release delayed", "err", err)
			}
		}
	}
}

// evalLoop evaluates what job pods asked for, which they can't do
// themselves: each holds only its own bot's credential. It looks every
// second, and backs off while Redis or the forge fails.
func (s *Server) evalLoop(ctx context.Context) {
	fails := 0
	for {
		wait := time.Second
		if fails > 0 {
			wait = backoff(time.Second, time.Minute, fails)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if _, err := s.E.EvaluateRequested(ctx); err != nil && ctx.Err() == nil {
			fails++
			s.Log.Warn("requested evaluations", "err", err, "failures", fails)
		} else {
			fails = 0
		}
	}
}

// backoff is the wait before a retry after fails failures in a row: base,
// doubling up to limit, less up to a quarter at random, so replicas that lost
// Redis together don't retry in step.
func backoff(base, limit time.Duration, fails int) time.Duration {
	d := limit
	if fails <= 20 {
		d = min(base<<(max(fails, 1)-1), limit)
	}
	return d - mrand.N(d/4+1)
}

// ---- connector auth ----

type callerKey struct{}

func callerOf(r *http.Request) core.Caller {
	v, _ := r.Context().Value(callerKey{}).(core.Caller)
	return v
}

var errNoCaller = errors.New("unauthorized")

// errBadName is a caller whose name devbot won't write into a comment.
var errBadName = errors.New("bad caller name")

// caller resolves who a connector request is for. In auth mode none that is
// the anonymous principal, when the install allows one. In mode oauth it is
// the person behind a devbot access token, acting on the forge as
// themselves; anything else, a forge token included, is refused.
func (s *Server) caller(r *http.Request) (core.Caller, error) {
	if s.Auth == nil {
		if s.Cfg.AnonymousPrincipal == "" {
			return core.Caller{}, errNoCaller
		}
		// An unverified client label, for the record: "chat-relay", "claude-desktop".
		if c := r.Header.Get("X-Devbot-Client"); c != "" {
			if !config.ValidName(c) {
				return core.Caller{}, fmt.Errorf("%w: X-Devbot-Client must match %s", errBadName, config.NameRule)
			}
			return core.Caller{Principal: s.Cfg.AnonymousPrincipal + "/" + c}, nil
		}
		return core.Caller{Principal: s.Cfg.AnonymousPrincipal}, nil
	}
	id, err := s.Auth.Authenticate(r)
	if err != nil {
		return core.Caller{}, err
	}
	if !config.ValidName(id.Login) {
		return core.Caller{}, fmt.Errorf("%w: login %q doesn't match %s", errBadName, id.Login, config.NameRule)
	}
	u, err := s.Auth.User(r.Context(), id)
	if err != nil {
		return core.Caller{}, err
	}
	return core.Caller{Principal: id.Login, User: u, Provider: id.Provider, Login: id.Login}, nil
}

// connectorWaitBudget is the most, in all, a connector call (MCP, HTTP API,
// web UI) waits out a forge's rate limits. Its caller is waiting on the
// answer: past this, it is told when the limit resets instead.
const connectorWaitBudget = 10 * time.Second

// callerContext is ctx for c: what the call does is audited as asked by c,
// and waits out rate limits for no more than connectorWaitBudget.
func callerContext(ctx context.Context, c core.Caller) context.Context {
	ctx = audit.With(ctx, audit.Fields{Principal: c.Principal, Login: c.Login})
	ctx = forge.WaitBudget(ctx, connectorWaitBudget)
	return context.WithValue(ctx, callerKey{}, c)
}

// refuse answers a request that has no caller: a 401 that tells an OAuth
// client where to sign in, a 400 for a name devbot won't record, or a 503
// when devbot couldn't check.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errNoCaller) || errors.Is(err, auth.ErrInvalidToken) {
		s.E.Audit.Log(r.Context(), audit.Record{Event: "authz.refused", Outcome: audit.Refused,
			Detail: map[string]any{"action": r.Method + " " + r.URL.Path, "reason": "no valid devbot credential"}})
	}
	switch {
	case s.Auth != nil && errors.Is(err, auth.ErrInvalidToken):
		s.Auth.Challenge(w, r)
	case errors.Is(err, errNoCaller):
		http.Error(w, "unauthorized", 401)
	case errors.Is(err, errBadName):
		http.Error(w, err.Error(), 400)
	default:
		s.Log.Warn("authenticating a connector call", "err", err)
		http.Error(w, "couldn't check credentials", 503)
	}
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := s.caller(r)
		if err != nil {
			s.refuse(w, r, err)
			return
		}
		h(w, r.WithContext(callerContext(r.Context(), c)))
	}
}

// publicError is what a connector caller is told about err, and with which
// status. Errors about the request itself (refused, not found, invalid)
// are theirs to read. A forge's rate limit says when it resets
// (with Retry-After, see retryAfter). Anything else may carry a forge's response or
// Redis's address, so it is logged under the request's ID and the caller
// gets only that ID.
func (s *Server) publicError(r *http.Request, err error) (int, string) {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	var rl *forge.RateLimitError
	switch {
	case errors.As(err, &rl):
		s.Log.Warn("connector call rate limited", "path", r.URL.Path, "request_id", requestID(r), "err", err)
		text := err.Error()
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			// What failed alongside it is the forge's or devbot's own
			// detail, logged above.
			text = rl.Error()
		}
		return http.StatusTooManyRequests, fmt.Sprintf("rate limited: %s; try again after %s", text, rl.Until.UTC().Format(time.RFC3339))
	case errors.Is(err, core.ErrRefused) || errors.Is(err, forge.ErrForbidden):
		return 403, err.Error()
	case errors.Is(err, config.ErrNotOptedIn) || errors.Is(err, forge.ErrNotFound):
		return 404, err.Error()
	case errors.Is(err, core.ErrInvalid) || errors.As(err, &syntax) || errors.As(err, &typ):
		return 400, err.Error()
	}
	id := requestID(r)
	s.Log.Error("connector call failed", "path", r.URL.Path, "request_id", id, "err", err)
	return 500, "internal error; request ID " + id
}

// retryAfter sets Retry-After on a response whose err is a forge's rate
// limit, to when it resets.
func retryAfter(w http.ResponseWriter, err error) {
	var rl *forge.RateLimitError
	if errors.As(err, &rl) {
		secs := int(math.Ceil(time.Until(rl.Until).Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
	}
}

// incomplete is a list a connector call read only part of, and why.
type incomplete struct {
	list any
	err  error
}

// partial makes a list read with an error a result when it has something
// in it: the caller gets what was read and is told the rest is missing.
// With nothing read, the error is the answer.
func partial[T any](list []T, err error) (any, error) {
	if err != nil && len(list) > 0 {
		return incomplete{list, err}, nil
	}
	return list, err
}

func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, v any, err error) {
	retryAfter(w, err)
	if p, ok := v.(incomplete); ok {
		retryAfter(w, p.err)
		_, msg := s.publicError(r, p.err)
		w.Header().Set("X-Devbot-Incomplete", msg)
		v = p.list
	}
	if err != nil {
		if errors.Is(err, auth.ErrInvalidToken) {
			// The forge stopped accepting the caller's sign-in mid-call.
			s.refuse(w, r, err)
			return
		}
		code, msg := s.publicError(r, err)
		http.Error(w, msg, code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ---- HTTP API ----

func (s *Server) apiFile(w http.ResponseWriter, r *http.Request) {
	var req core.FileRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	st, err := s.E.FileTask(r.Context(), req, callerOf(r))
	s.writeJSON(w, r, st, err)
}

func (s *Server) apiList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("detail") != "" && q.Get("parent") == "" {
		v, err := partial(s.E.ListTasksDetailed(r.Context(), callerOf(r), q.Get("repo"), q.Get("state"), q.Get("origin_prefix")))
		s.writeJSON(w, r, v, err)
		return
	}
	v, err := s.listTasks(r.Context(), callerOf(r), q.Get("repo"), q.Get("state"), q.Get("origin_prefix"), q.Get("parent"))
	s.writeJSON(w, r, v, err)
}

// listTasks is list_tasks: given a parent, that parent task's children.
func (s *Server) listTasks(ctx context.Context, c core.Caller, repo, state, originPrefix, parent string) (any, error) {
	if parent != "" {
		return s.E.ListChildren(ctx, c, parent, repo, state, originPrefix)
	}
	return partial(s.E.ListTasks(ctx, c, repo, state, originPrefix))
}

func (s *Server) apiFileProject(w http.ResponseWriter, r *http.Request) {
	var req core.ProjectRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	st, err := s.E.FileProject(r.Context(), req, callerOf(r))
	s.writeJSON(w, r, st, err)
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.E.Status(r.Context(), callerOf(r), r.URL.Query().Get("ref"))
	s.writeJSON(w, r, st, err)
}

type refText struct {
	Ref  string `json:"ref"`
	Text string `json:"text"`
	Base string `json:"base"`
	On   string `json:"on"`
	SHA  string `json:"sha"`
	// Until is a wait's release gate, "tagged" and any tag pattern, which
	// On may carry itself instead.
	Until string `json:"until"`
	// BuildAgainst has the wait build against its task while that task is
	// unmerged, after its release gate, which On may also say itself
	// (", build against").
	BuildAgainst bool `json:"build_against"`
}

// wait is the wait v asks for, as core.ParseWait reads it.
func (v refText) wait() string {
	on := v.On
	if u := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v.Until), "until ")); u != "" {
		on += " until " + u
	}
	if v.BuildAgainst && !strings.HasSuffix(strings.TrimSpace(on), "build against") {
		on += ", build against"
	}
	return on
}

func decodeRef(r *http.Request) (refText, error) {
	var v refText
	err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&v)
	return v, err
}

func (s *Server) apiAnswer(w http.ResponseWriter, r *http.Request) {
	v, err := decodeRef(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	st, err := s.E.Answer(r.Context(), v.Ref, v.Text, callerOf(r))
	s.writeJSON(w, r, st, err)
}

func (s *Server) apiCancel(w http.ResponseWriter, r *http.Request) {
	v, err := decodeRef(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	st, err := s.E.Cancel(r.Context(), v.Ref, callerOf(r))
	s.writeJSON(w, r, st, err)
}

func (s *Server) apiRetry(w http.ResponseWriter, r *http.Request) {
	v, err := decodeRef(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	st, err := s.E.Retry(r.Context(), v.Ref, callerOf(r))
	s.writeJSON(w, r, st, err)
}

func (s *Server) apiRebase(w http.ResponseWriter, r *http.Request) {
	v, err := decodeRef(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	st, err := s.E.Rebase(r.Context(), v.Ref, callerOf(r))
	s.writeJSON(w, r, st, err)
}

func (s *Server) apiApprove(w http.ResponseWriter, r *http.Request) {
	v, err := decodeRef(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	st, err := s.E.Approve(r.Context(), v.Ref, v.SHA, callerOf(r))
	s.writeJSON(w, r, st, err)
}

func (s *Server) apiRetarget(w http.ResponseWriter, r *http.Request) {
	v, err := decodeRef(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	st, err := s.E.Retarget(r.Context(), v.Ref, v.Base, callerOf(r))
	s.writeJSON(w, r, st, err)
}

func (s *Server) apiWait(w http.ResponseWriter, r *http.Request) {
	v, err := decodeRef(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	st, err := s.E.WaitTask(r.Context(), v.Ref, v.wait(), callerOf(r))
	s.writeJSON(w, r, st, err)
}

func (s *Server) apiEdit(w http.ResponseWriter, r *http.Request) {
	var req core.EditRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	st, err := s.E.EditTask(r.Context(), req, callerOf(r))
	s.writeJSON(w, r, st, err)
}

// EventsPage is one page of the feed.
type EventsPage struct {
	Events  []queue.Event `json:"events"`
	Cursor  string        `json:"cursor"`
	Expired bool          `json:"cursor_expired,omitempty"`
}

// eventFilter picks the events a feed read returns.
type eventFilter struct {
	repo, originPrefix string
	types              map[string]bool
}

func newEventFilter(repo, originPrefix, types string) eventFilter {
	f := eventFilter{repo: repo, originPrefix: originPrefix, types: map[string]bool{}}
	for _, t := range strings.Split(types, ",") {
		if t = strings.TrimSpace(t); t != "" {
			f.types[t] = true
		}
	}
	return f
}

// match reports whether c should see e: events for tasks on repos c can't
// read are skipped, as if they weren't there.
func (s *Server) match(ctx context.Context, c core.Caller, f eventFilter, e queue.Event) (bool, error) {
	if f.originPrefix != "" && !strings.HasPrefix(e.Origin, f.originPrefix) {
		return false, nil
	}
	if len(f.types) > 0 && !f.types[e.Type] {
		return false, nil
	}
	if c.User == nil && f.repo == "" {
		return true, nil
	}
	ref, err := forge.ParseItem(e.Task)
	if err != nil || (f.repo != "" && ref.Repo.String() != f.repo) {
		return false, nil
	}
	return s.E.CanRead(ctx, c, ref.Repo)
}

// A page of events is defaultEventLimit long unless the caller asks for
// another length, and never more than maxEventLimit.
const (
	defaultEventLimit = 100
	maxEventLimit     = 500
)

func eventLimit(n int64) int64 {
	if n <= 0 {
		return defaultEventLimit
	}
	return min(n, maxEventLimit)
}

// events_ reads the feed for c, oldest first, after cursor.
func (s *Server) events_(ctx context.Context, c core.Caller, cursor string, f eventFilter, limit int64) (EventsPage, error) {
	limit = eventLimit(limit)
	expired, _ := s.E.Q.EventsExpired(ctx, cursor)
	page := EventsPage{Cursor: cursor, Expired: expired, Events: []queue.Event{}}
	// Scan forward until a page of matching events or the end of the stream.
	for i := 0; i < 20; i++ {
		evs, next, err := s.E.Q.Events(ctx, page.Cursor, 500)
		if err != nil {
			return page, err
		}
		for _, e := range evs {
			page.Cursor = e.ID
			if ok, err := s.match(ctx, c, f, e); err != nil {
				return page, err
			} else if !ok {
				continue
			}
			page.Events = append(page.Events, e)
			if int64(len(page.Events)) >= limit {
				return page, nil
			}
		}
		page.Cursor = next
		if len(evs) < 500 {
			break
		}
	}
	return page, nil
}

// recentEvents reads the feed for c, newest first, before cursor (""
// = from the newest). The page's cursor continues further back; it is ""
// once the start of the feed is reached.
func (s *Server) recentEvents(ctx context.Context, c core.Caller, before string, f eventFilter, limit int64) (EventsPage, error) {
	limit = eventLimit(limit)
	page := EventsPage{Cursor: before, Events: []queue.Event{}}
	for i := 0; i < 20; i++ {
		evs, next, err := s.E.Q.EventsBefore(ctx, page.Cursor, 500)
		if err != nil {
			return page, err
		}
		for _, e := range evs {
			page.Cursor = e.ID
			if ok, err := s.match(ctx, c, f, e); err != nil {
				return page, err
			} else if !ok {
				continue
			}
			page.Events = append(page.Events, e)
			if int64(len(page.Events)) >= limit {
				return page, nil
			}
		}
		page.Cursor = next
		if len(evs) < 500 {
			page.Cursor = ""
			break
		}
	}
	return page, nil
}

func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.ParseInt(q.Get("limit"), 10, 64)
	page, err := s.events_(r.Context(), callerOf(r), q.Get("cursor"), newEventFilter(q.Get("repo"), q.Get("origin_prefix"), q.Get("types")), limit)
	s.writeJSON(w, r, page, err)
}

func (s *Server) apiRecentEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.ParseInt(q.Get("limit"), 10, 64)
	page, err := s.recentEvents(r.Context(), callerOf(r), q.Get("before"), newEventFilter(q.Get("repo"), q.Get("origin_prefix"), q.Get("types")), limit)
	s.writeJSON(w, r, page, err)
}

type QueueStatus struct {
	Queues        map[string]int64 `json:"queues"`
	Leases        map[string]int64 `json:"leases"`
	HoldUntil     *time.Time       `json:"ratelimit_hold_until,omitempty"`
	LastReconcile *time.Time       `json:"last_reconcile,omitempty"`
	Version       string           `json:"version"`
}

func (s *Server) queueStatus(ctx context.Context) (QueueStatus, error) {
	st := QueueStatus{Version: s.Version}
	var err error
	if st.Queues, err = s.E.Q.Depths(ctx); err != nil {
		return st, err
	}
	if st.Leases, err = s.E.Q.Leases(ctx); err != nil {
		return st, err
	}
	if h, _ := s.E.Q.Hold(ctx); time.Now().Before(h) {
		st.HoldUntil = &h
	}
	if t, _ := s.E.Q.LastReconcile(ctx); !t.IsZero() {
		st.LastReconcile = &t
	}
	return st, nil
}

func (s *Server) apiQueueStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.queueStatus(r.Context())
	s.writeJSON(w, r, st, err)
}

// ---- metrics ----

// metrics writes the Prometheus text format by hand: a few gauges from Redis,
// the counters jobs keep in Redis, and this process's own series. Without
// Redis it still serves this process's, with devbot_redis_up 0, so an
// outage doesn't blank the dashboards that would show it.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	var b bytes.Buffer
	up := 1
	if err := s.redisMetrics(r.Context(), &b); err != nil {
		s.Log.Warn("metrics: reading Redis", "err", err)
		up = 0
	}
	fmt.Fprintf(&b, "# TYPE devbot_redis_up gauge\ndevbot_redis_up %d\n", up)
	leading := 0
	if s.leading.Load() {
		leading = 1
	}
	fmt.Fprintf(&b, "# TYPE devbot_leader gauge\ndevbot_leader %d\n", leading)
	s.mu.Lock()
	b.WriteString("# TYPE devbot_webhook_events_total counter\n")
	for _, k := range sortedKeys(s.webhooks) {
		p := strings.SplitN(k, "|", 3)
		fmt.Fprintf(&b, "devbot_webhook_events_total{provider=%q,kind=%q,result=%q} %g\n", p[0], p[1], p[2], s.webhooks[k])
	}
	s.mu.Unlock()
	waits := forge.RateLimitWaits.Snapshot()
	keys := slices.SortedFunc(maps.Keys(waits), func(a, b [2]string) int { return cmp.Compare(a[0]+"|"+a[1], b[0]+"|"+b[1]) })
	b.WriteString("# TYPE devbot_forge_ratelimit_waits_total counter\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "devbot_forge_ratelimit_waits_total{provider=%q,kind=%q} %d\n", k[0], k[1], waits[k].Count)
	}
	b.WriteString("# TYPE devbot_forge_ratelimit_wait_seconds_total counter\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "devbot_forge_ratelimit_wait_seconds_total{provider=%q,kind=%q} %g\n", k[0], k[1], waits[k].Seconds)
	}
	reqs := forge.Requests.Snapshot()
	b.WriteString("# TYPE devbot_forge_requests_total counter\n")
	for _, k := range slices.SortedFunc(maps.Keys(reqs), func(a, b forge.Request) int {
		return cmp.Or(cmp.Compare(a.Provider, b.Provider), cmp.Compare(a.Bucket, b.Bucket), cmp.Compare(a.Endpoint, b.Endpoint),
			cmp.Compare(a.Method, b.Method), cmp.Compare(a.Status, b.Status))
	}) {
		fmt.Fprintf(&b, "devbot_forge_requests_total{provider=%q,bucket=%q,method=%q,endpoint=%q,status=%q} %d\n",
			k.Provider, k.Bucket, k.Method, k.Endpoint, k.Status, reqs[k])
	}
	rl := forge.RateLimitErrors.Snapshot()
	b.WriteString("# TYPE devbot_forge_ratelimit_errors_total counter\n")
	for _, k := range slices.SortedFunc(maps.Keys(rl), func(a, b [2]string) int { return cmp.Compare(a[0]+"|"+a[1], b[0]+"|"+b[1]) }) {
		fmt.Fprintf(&b, "devbot_forge_ratelimit_errors_total{provider=%q,kind=%q} %d\n", k[0], k[1], rl[k])
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.Write(b.Bytes())
}

// redisMetrics writes the series kept in Redis.
func (s *Server) redisMetrics(ctx context.Context, b *bytes.Buffer) error {
	st, err := s.queueStatus(ctx)
	if err != nil {
		return err
	}
	b.WriteString("# TYPE devbot_queue_depth gauge\n")
	for _, k := range sortedKeys(st.Queues) {
		fmt.Fprintf(b, "devbot_queue_depth{queue=%q} %d\n", k, st.Queues[k])
	}
	b.WriteString("# TYPE devbot_leases_active gauge\n")
	for _, k := range []string{queue.KindImplement, queue.KindReview, queue.KindIntegrate, queue.KindPlan} {
		fmt.Fprintf(b, "devbot_leases_active{kind=%q} %d\n", k, st.Leases[k])
	}
	hold := 0.0
	if st.HoldUntil != nil {
		hold = float64(st.HoldUntil.Unix())
	}
	fmt.Fprintf(b, "# TYPE devbot_ratelimit_hold_until_seconds gauge\ndevbot_ratelimit_hold_until_seconds %g\n", hold)
	var last int64
	if st.LastReconcile != nil {
		last = st.LastReconcile.Unix()
	}
	fmt.Fprintf(b, "# TYPE devbot_last_reconcile_seconds gauge\ndevbot_last_reconcile_seconds %d\n", last)
	if n, err := s.E.Q.LaterCount(ctx); err == nil {
		fmt.Fprintf(b, "# TYPE devbot_webhook_events_kept gauge\ndevbot_webhook_events_kept %d\n", n)
	}
	s.quotaMetrics(ctx, b)
	if info, err := s.E.Q.R.Info(ctx, "memory").Result(); err == nil {
		b.WriteString(redisMemory(info))
	}
	if behind, err := s.E.Q.Behind(ctx, time.Now().Add(-3*s.Cfg.ReconcileInterval)); err == nil {
		b.WriteString("# TYPE devbot_prs_behind_base gauge\n")
		for _, k := range sortedKeys(behind) {
			fmt.Fprintf(b, "devbot_prs_behind_base{repo=%q} %d\n", k, behind[k])
		}
	}

	stats, err := s.E.Q.Stats(ctx)
	if err == nil {
		// Counters written under other attribution labels, before a config
		// change, have the wrong number of labels and are left out.
		attr := s.Cfg.Metrics.AttributionLabels()
		agent := []string{"harness", "model", "role"}
		byName := map[string][]string{}
		labelNames := map[string][]string{
			"jobs":                     {"role", "repo", "outcome"},
			"job_seconds":              {"role", "phase"},
			"events":                   {"type"},
			"evaluations_acted":        {"source"},
			"webhook_events_skipped":   {"kind"},
			"webhook_requeue":          {"outcome"},
			"reconcile_deferred":       {"provider"},
			"reconcile_actions":        {"state"},
			"subscriber_failures":      {"subscriber"},
			"subscriber_rejected":      {"subscriber"},
			"authz_lookup_errors":      {"repo", "provider"},
			"agent_runs":               {"harness", "model", "role", "outcome"},
			"agent_run_seconds":        slices.Concat(agent, attr),
			"agent_tokens":             slices.Concat(agent, attr, []string{"kind"}),
			"agent_cost_usd":           slices.Concat(agent, attr),
			"agent_mcp_calls":          {"harness", "role", "server"},
			"protection_lookup_errors": {"repo", "provider"},
			"check_reruns":             {"repo", "provider"},
			"search_errors":            {"provider"},
			"rebases_started":          {"repo", "trigger"},
			"branches_deleted":         {"repo"},
			"held_approvals":           {"repo", "by"},
			"docker_detection_errors":  {"repo"},
		}
		writeHistogram(b, "job_duration_seconds", append([]string{"role"}, attr...), stats)
		for _, k := range sortedKeys(stats) {
			parts := strings.Split(k, "|")
			names, ok := labelNames[parts[0]]
			if !ok || len(parts)-1 != len(names) {
				continue
			}
			var ls []string
			for i, n := range names {
				ls = append(ls, fmt.Sprintf("%s=%q", n, parts[i+1]))
			}
			byName[parts[0]] = append(byName[parts[0]], fmt.Sprintf("devbot_%s_total{%s} %g", parts[0], strings.Join(ls, ","), stats[k]))
		}
		for _, n := range sortedKeys(byName) {
			fmt.Fprintf(b, "# TYPE devbot_%s_total counter\n", n)
			for _, l := range byName[n] {
				b.WriteString(l + "\n")
			}
		}
	}
	return nil
}

// quotaMetrics writes what the forges last said of each credential
// bucket's rate limits, as every process noted it (queue.NoteQuota).
// People's own tokens are left out: there is a bucket for each.
func (s *Server) quotaMetrics(ctx context.Context, b *bytes.Buffer) {
	type series struct {
		labels string
		q      forge.Quota
	}
	var all []series
	for _, p := range slices.Sorted(maps.Keys(s.E.Forges)) {
		qs, err := s.E.Q.AllQuotas(ctx, p)
		if err != nil {
			continue
		}
		for _, k := range slices.SortedFunc(maps.Keys(qs), func(a, b [2]string) int { return cmp.Compare(a[0]+"|"+a[1], b[0]+"|"+b[1]) }) {
			if strings.HasPrefix(k[0], "user ") {
				continue
			}
			all = append(all, series{fmt.Sprintf("provider=%q,bucket=%q,resource=%q", p, k[0], k[1]), qs[k]})
		}
	}
	if len(all) == 0 {
		return
	}
	for _, g := range []struct {
		name string
		v    func(forge.Quota) int64
	}{
		{"remaining", func(q forge.Quota) int64 { return int64(q.Remaining) }},
		{"limit", func(q forge.Quota) int64 { return int64(q.Limit) }},
		{"reset_seconds", func(q forge.Quota) int64 { return q.Reset.Unix() }},
	} {
		fmt.Fprintf(b, "# TYPE devbot_forge_ratelimit_%s gauge\n", g.name)
		for _, sr := range all {
			fmt.Fprintf(b, "devbot_forge_ratelimit_%s{%s} %d\n", g.name, sr.labels, g.v(sr.q))
		}
	}
}

// writeHistogram writes the histogram queue.Observe keeps in stats: per
// label set, its buckets in order of bound, then its sum and count.
func writeHistogram(b *bytes.Buffer, name string, labels []string, stats map[string]float64) {
	type bucket struct {
		le    float64
		raw   string
		count float64
	}
	buckets := map[string][]bucket{}
	for k, v := range stats {
		parts := strings.Split(k, "|")
		if parts[0] != name+"_bucket" || len(parts) != len(labels)+2 {
			continue
		}
		raw := parts[len(parts)-1]
		le, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}
		series := strings.Join(parts[1:len(parts)-1], "|")
		buckets[series] = append(buckets[series], bucket{le, raw, v})
	}
	if len(buckets) == 0 {
		return
	}
	fmt.Fprintf(b, "# TYPE devbot_%s histogram\n", name)
	for _, series := range sortedKeys(buckets) {
		var ls []string
		for i, v := range strings.Split(series, "|") {
			ls = append(ls, fmt.Sprintf("%s=%q", labels[i], v))
		}
		l := strings.Join(ls, ",")
		bs := buckets[series]
		sort.Slice(bs, func(i, j int) bool { return bs[i].le < bs[j].le })
		for _, bk := range bs {
			fmt.Fprintf(b, "devbot_%s_bucket{%s,le=%q} %g\n", name, l, bk.raw, bk.count)
		}
		fmt.Fprintf(b, "devbot_%s_sum{%s} %g\n", name, l, stats[name+"_sum|"+series])
		fmt.Fprintf(b, "devbot_%s_count{%s} %g\n", name, l, stats[name+"_count|"+series])
	}
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// ---- outbound subscribers ----

// deliver pushes matching events to one subscriber, at least once, in order,
// signed (sign). The receiver dedupes on the event id.
// It runs on the leader, and stops when the cursor, which only token can
// move, says it no longer leads; the next leader carries on from the
// cursor, so an event is never skipped, and only one whose delivery was
// cut short by the handover goes out twice.
func (s *Server) deliver(ctx context.Context, sub config.Subscriber, token string) {
	secret := []byte(config.Env(sub.SecretEnv))
	hc := &http.Client{Timeout: 15 * time.Second}
	want := map[string]bool{}
	for _, t := range sub.Types {
		want[t] = true
	}
	cursor, fail := "", 0
	for {
		wait := s.deliverEvery
		if fail > 0 {
			wait = min(time.Duration(1<<min(fail, 8))*time.Second, 5*time.Minute)
		}
		if cursor == "" && fail == 0 {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if cursor == "" {
			c, err := s.E.Q.Cursor(ctx, sub.Name)
			if err != nil {
				fail++
				continue
			}
			if c == "" {
				// A new subscriber starts from now, not from two weeks of
				// history. Saved at once, so a handover before the first event
				// doesn't move "now" past it.
				c = strconv.FormatInt(time.Now().UnixMilli(), 10) + "-0"
				if ok, err := s.E.Q.SetCursor(ctx, sub.Name, c, token); err != nil {
					fail++
					continue
				} else if !ok {
					return
				}
			}
			cursor = c
		}
		evs, _, err := s.E.Q.Events(ctx, cursor, 100)
		if err != nil {
			fail++
			continue
		}
		for _, e := range evs {
			if (len(want) == 0 || want[e.Type]) && strings.HasPrefix(e.Origin, sub.Origin) {
				body, _ := json.Marshal(e)
				req, _ := http.NewRequestWithContext(ctx, "POST", sub.URL, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Devbot-Event", e.Type)
				req.Header.Set("X-Devbot-Event-Id", e.ID)
				if len(secret) > 0 {
					req.Header.Set("X-Devbot-Signature", sign(secret, time.Now(), body))
				}
				resp, err := hc.Do(req)
				if err == nil {
					resp.Body.Close()
				}
				if err == nil && permanent(resp.StatusCode) {
					// Sending it again won't change the answer, and would hold up
					// every event after it.
					s.Log.Warn("subscriber refused an event; not retrying it", "subscriber", sub.Name, "event", e.ID, "type", e.Type, "status", resp.StatusCode)
					s.E.Count(ctx, "subscriber_rejected", []string{sub.Name}, 1)
				} else if err != nil || resp.StatusCode >= 300 {
					fail++
					s.E.Count(ctx, "subscriber_failures", []string{sub.Name}, 1)
					break
				}
			}
			ok, err := s.E.Q.SetCursor(ctx, sub.Name, e.ID, token)
			if err != nil {
				fail++
				break
			}
			if !ok {
				return // deposed
			}
			cursor = e.ID
			fail = 0
		}
	}
}

// permanent is a subscriber's answer that a retry won't change: a 4xx,
// but for timeouts (408), "too early" (425) and rate limits (429).
func permanent(code int) bool {
	return code >= 400 && code < 500 && code != http.StatusRequestTimeout && code != http.StatusTooEarly && code != http.StatusTooManyRequests
}

// sign is a delivery's X-Devbot-Signature: t=<unix seconds>,sha256=<hex
// HMAC-SHA256 of "<t>.<body>">. The time is signed with the body, so a
// receiver that refuses one more than five minutes off can't be replayed a
// captured delivery later. Each retry is signed afresh.
func sign(secret []byte, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// redisMemory is Redis's used_memory and maxmemory, from INFO memory, as
// gauges. Redis refuses writes once full (noeviction), and sign-ins and the
// queue share it, so its memory is worth an alert.
func redisMemory(info string) string {
	var b strings.Builder
	for _, l := range strings.Split(info, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), ":")
		if k == "used_memory" || k == "maxmemory" {
			fmt.Fprintf(&b, "# TYPE devbot_redis_%s_bytes gauge\ndevbot_redis_%s_bytes %s\n", k, k, v)
		}
	}
	return b.String()
}

// quietLog is one kind of refused request's log rate.
type quietLog struct {
	last       time.Time
	suppressed int
}

// refusedEvery is how often each kind of refused request is logged.
const refusedEvery = time.Minute

// logRefused reports whether a refused request of this kind should be
// logged (and audited) now, and how many went unlogged before it. Anyone
// can send them, so logging each would let anyone flood the logs; the
// metrics still count every one.
func (s *Server) logRefused(kind string) (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refused == nil {
		s.refused = map[string]*quietLog{}
	}
	q := s.refused[kind]
	if q == nil {
		q = &quietLog{}
		s.refused[kind] = q
	}
	now := time.Now()
	if !q.last.IsZero() && now.Sub(q.last) < refusedEvery {
		q.suppressed++
		return false, 0
	}
	n := q.suppressed
	q.last, q.suppressed = now, 0
	return true, n
}

// warnRefused logs a refused request, rate-limited by logRefused.
func (s *Server) warnRefused(kind, msg string, args ...any) {
	if ok, n := s.logRefused(kind); ok {
		s.Log.Warn(msg, append(args, "suppressed_since_last", n)...)
	}
}

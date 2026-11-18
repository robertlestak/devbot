package forge

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"
)

// The kinds of RateLimitError.
const (
	LimitPrimary   = "primary"
	LimitSecondary = "secondary"
	// LimitLowQuota is low-priority work held back while little is left
	// of the quota (see LowPriority), not a limit the forge enforced.
	LimitLowQuota = "low_quota"
)

// RateLimitError is a call given up because the forge's rate limit wouldn't
// allow it before Until, within the adapter's longest wait. What it was
// for can be tried again then: webhook events are kept until Until.
type RateLimitError struct {
	Provider string // the adapter's type, for the message: "github"
	Kind     string // LimitPrimary, LimitSecondary or LimitLowQuota
	Until    time.Time
}

func (e *RateLimitError) Error() string {
	if e.Kind == LimitLowQuota {
		return fmt.Sprintf("%s: quota low; low-priority call deferred until %s", e.Provider, e.Until.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("%s: %s rate limit until %s", e.Provider, e.Kind, e.Until.UTC().Format(time.RFC3339))
}

type priorityKey struct{}

// LowPriority marks the forge calls made with ctx as work that can wait
// for the rate limit's reset: reconcile's discovery, and webhook events no
// task stands behind. An adapter that paces its calls fails them with a
// LimitLowQuota RateLimitError while little is left of their credential's
// quota, keeping the rest for the tasks' own evaluations, merges and jobs.
func LowPriority(ctx context.Context) context.Context {
	return context.WithValue(ctx, priorityKey{}, true)
}

// IsLowPriority reports whether ctx was marked with LowPriority.
func IsLowPriority(ctx context.Context) bool {
	v, _ := ctx.Value(priorityKey{}).(bool)
	return v
}

type budgetKey struct{}

type waitBudget struct {
	mu   sync.Mutex
	left time.Duration
}

// WaitBudget caps how long, in all, the forge calls made with ctx wait out
// rate limits: a connector's caller is waiting on the answer, and is better
// told when to try again. An adapter that waits fails a call whose wait
// would take more than what is left with a RateLimitError instead.
func WaitBudget(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, budgetKey{}, &waitBudget{left: d})
}

// SpendWait reports whether ctx's WaitBudget has d left, and takes d from
// it if so. A ctx without one may wait as long as the adapter allows.
func SpendWait(ctx context.Context, d time.Duration) bool {
	b, _ := ctx.Value(budgetKey{}).(*waitBudget)
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if d > b.left {
		return false
	}
	b.left -= d
	return true
}

// Quota is what a forge last said of one rate limit: how many calls are
// left of Limit until Reset, when the window starts again.
type Quota struct {
	Remaining int
	Limit     int
	Reset     time.Time
}

// Request is one series of Requests: the calls one credential bucket made
// to one endpoint, by the class of their answer.
type Request struct {
	Provider string
	// Bucket is the rate limit the calls count against, as the adapter
	// names it: "installation 12", "app 3", "token". People's own tokens
	// share one, "user", however many people there are.
	Bucket string
	Method string
	// Endpoint is the path's template, "/repos/{owner}/{repo}/pulls/{}",
	// never the path itself, so the series stay bounded.
	Endpoint string
	// Status is "2xx", "304", "3xx", "4xx", "5xx", or "error" for a call
	// that got no answer.
	Status string
}

// Requests counts, in this process, the calls adapters made, for serve's
// /metrics.
var Requests = &RequestCounter{}

type RequestCounter struct {
	mu sync.Mutex
	m  map[Request]int
}

func (c *RequestCounter) Add(r Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[Request]int{}
	}
	c.m[r]++
}

// Snapshot is the counts so far.
func (c *RequestCounter) Snapshot() map[Request]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.m)
}

// StatusClass is the Status label for an HTTP status code.
func StatusClass(code int) string {
	switch {
	case code == 304:
		return "304"
	case code >= 200 && code < 600:
		return fmt.Sprintf("%dxx", code/100)
	}
	return "error"
}

// RateLimitErrors counts, in this process, the calls adapters gave up on
// with a RateLimitError, by provider and kind, for serve's /metrics.
var RateLimitErrors = &Counter{}

// Counter counts by a pair of labels.
type Counter struct {
	mu sync.Mutex
	m  map[[2]string]int
}

func (c *Counter) Add(a, b string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[[2]string]int{}
	}
	c.m[[2]string{a, b}]++
}

// Snapshot is the counts so far.
func (c *Counter) Snapshot() map[[2]string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.m)
}

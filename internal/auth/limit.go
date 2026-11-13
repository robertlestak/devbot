package auth

import (
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

// Rate is a token bucket: PerMinute requests a minute on average, up to
// Burst at once. A zero PerMinute is no limit.
type Rate struct {
	PerMinute float64
	Burst     int
}

// Limits are the per-address limits on the endpoints anyone can reach.
type Limits struct {
	Register, Authorize, DeviceAuthorization, Token, Device Rate
	// ClientsPerAddress caps the clients one address registers per
	// clientTTL. Zero is no cap.
	ClientsPerAddress int
}

// maxBuckets bounds the limiter's memory. Past it, buckets that have
// refilled are dropped; if that isn't enough, all of them are, which only
// forgets limits, and only for someone spreading requests over this many
// addresses.
const maxBuckets = 100_000

// limiter keeps its buckets in memory, not in Redis: keeping Redis from
// filling up is much of what it is for. Each serve replica has its own, so
// an address that the Service spreads over N replicas gets up to N times
// the limit, which still bounds what it can write.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
	full   time.Time // when it has refilled, if nothing is taken
}

// allow takes a token from key's bucket, or says how long until there is
// one.
func (l *limiter) allow(key string, r Rate, now time.Time) (bool, time.Duration) {
	if r.PerMinute <= 0 {
		return true, 0
	}
	burst, perSec := float64(max(r.Burst, 1)), r.PerMinute/60
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= maxBuckets {
			l.sweep(now)
		}
		if l.buckets == nil {
			l.buckets = map[string]*bucket{}
		}
		b = &bucket{tokens: burst, at: now}
		l.buckets[key] = b
	}
	b.tokens = min(burst, b.tokens+now.Sub(b.at).Seconds()*perSec)
	b.at = now
	ok := b.tokens >= 1
	if ok {
		b.tokens--
	}
	b.full = now.Add(time.Duration((burst - b.tokens) / perSec * float64(time.Second)))
	if !ok {
		return false, time.Duration((1 - b.tokens) / perSec * float64(time.Second))
	}
	return true, 0
}

func (l *limiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if !b.full.After(now) {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) >= maxBuckets {
		l.buckets = nil
	}
}

// addrKey is who a limit applies to: the address, or for IPv6 its /64,
// which one host usually has to itself.
func addrKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil || a.Is4() {
		return ip
	}
	p, _ := a.Prefix(64)
	return p.String()
}

// limited wraps h in the per-address limit r, named name. A request whose
// address can't be told isn't limited, as with device codes: one bucket
// shared by everyone would let anyone turn everyone away. Pages get a page
// back; the rest an OAuth error with errCode.
func (s *Server) limited(name string, r Rate, html bool, errCode string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if ip := s.clientIP(req); ip != "" {
			if ok, wait := s.lim.allow(name+"|"+addrKey(ip), r, s.o.Now()); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
				if html {
					page(w, http.StatusTooManyRequests, "Too many requests", "Too many requests from your address. Wait a minute, then try again.")
				} else {
					oauthError(w, http.StatusTooManyRequests, errCode, "too many requests from your address; retry after "+strconv.Itoa(int(math.Ceil(wait.Seconds())))+"s")
				}
				return
			}
		}
		h(w, req)
	}
}

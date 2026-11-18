package core

import (
	"sync"
	"time"
)

// readCacheTTL is how long a signed-in person's read access to a repo is
// remembered. Losing access takes at most this long to hide a repo's tasks.
const readCacheTTL = time.Minute

// readCacheMax caps the entries, one per person and repo they asked about,
// so a busy install's memory doesn't grow with everyone who ever signed in.
const readCacheMax = 10000

// readCache remembers CanRead's answers for readCacheTTL, holding at most
// readCacheMax of them.
type readCache struct {
	mu  sync.Mutex
	m   map[string]readable
	now func() time.Time // for tests
}

type readable struct {
	ok bool
	at time.Time
}

func (c *readCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *readCache) get(key string) (ok, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, found := c.m[key]
	if found && c.clock().Sub(v.at) >= readCacheTTL {
		delete(c.m, key)
		return false, false
	}
	return v.ok, found
}

func (c *readCache) put(key string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	if c.m == nil {
		c.m = map[string]readable{}
	}
	if _, there := c.m[key]; !there && len(c.m) >= readCacheMax {
		// Expired entries go first; if none has, the oldest does.
		oldest := ""
		for k, v := range c.m {
			if now.Sub(v.at) >= readCacheTTL {
				delete(c.m, k)
			} else if oldest == "" || v.at.Before(c.m[oldest].at) {
				oldest = k
			}
		}
		if len(c.m) >= readCacheMax {
			delete(c.m, oldest)
		}
	}
	c.m[key] = readable{ok: ok, at: now}
}

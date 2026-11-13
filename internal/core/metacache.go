package core

import (
	"container/list"
	"sync"
)

// metaCacheMax caps the routing blocks remembered, one per task, so a
// long-running serve doesn't grow with every task it has ever seen. A task
// that falls out is read from its comments again.
const metaCacheMax = 10000

// metaCache remembers tasks' routing blocks, dropping the least recently
// used past metaCacheMax.
type metaCache struct {
	mu    sync.Mutex
	m     map[string]*list.Element
	order list.List // front is the most recently used
	max   int       // for tests; metaCacheMax when 0
}

type metaEntry struct {
	key string
	m   Meta
}

func (c *metaCache) get(key string) (Meta, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[key]
	if !ok {
		return Meta{}, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*metaEntry).m, true
}

func (c *metaCache) put(key string, m Meta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*list.Element{}
	}
	if el, ok := c.m[key]; ok {
		el.Value.(*metaEntry).m = m
		c.order.MoveToFront(el)
		return
	}
	c.m[key] = c.order.PushFront(&metaEntry{key: key, m: m})
	limit := c.max
	if limit == 0 {
		limit = metaCacheMax
	}
	for c.order.Len() > limit {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.m, last.Value.(*metaEntry).key)
	}
}

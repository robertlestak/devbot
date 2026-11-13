package core

import (
	"fmt"
	"testing"
	"time"
)

func TestReadCacheExpiresAndIsCapped(t *testing.T) {
	now := time.Now()
	c := &readCache{now: func() time.Time { return now }}
	c.put("a", true)
	if ok, found := c.get("a"); !found || !ok {
		t.Fatal("a fresh answer should be remembered")
	}
	now = now.Add(readCacheTTL)
	if _, found := c.get("a"); found {
		t.Fatal("an answer older than the TTL must be asked again")
	}
	if len(c.m) != 0 {
		t.Fatal("an expired entry should be dropped when read")
	}

	for i := range readCacheMax {
		c.put(fmt.Sprint(i), true)
		now = now.Add(time.Microsecond)
	}
	c.put("one more", false)
	if len(c.m) != readCacheMax {
		t.Fatalf("the cache must stay at %d entries, has %d", readCacheMax, len(c.m))
	}
	if _, found := c.get("0"); found {
		t.Fatal("the oldest entry should make room")
	}
	if ok, found := c.get("one more"); !found || ok {
		t.Fatal("the newest entry should be kept")
	}
	// Once they've all expired, a new entry clears them out.
	now = now.Add(readCacheTTL)
	c.put("fresh", true)
	if len(c.m) != 1 {
		t.Fatalf("expired entries should go first, %d left", len(c.m))
	}
}

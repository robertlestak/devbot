package core

import (
	"fmt"
	"testing"
)

func TestMetaCacheDropsLeastRecentlyUsed(t *testing.T) {
	c := &metaCache{}
	for i := range metaCacheMax {
		c.put(fmt.Sprint(i), Meta{Origin: fmt.Sprint(i)})
	}
	if m, ok := c.get("0"); !ok || m.Origin != "0" {
		t.Fatal("a block within the cap should be remembered")
	}
	c.put("one more", Meta{Origin: "x"})
	if len(c.m) != metaCacheMax || c.order.Len() != metaCacheMax {
		t.Fatalf("the cache must stay at %d entries, has %d", metaCacheMax, len(c.m))
	}
	if _, ok := c.get("1"); ok {
		t.Fatal("the least recently used block should go first")
	}
	if _, ok := c.get("0"); !ok {
		t.Fatal("a block read just now should stay")
	}
	c.put("0", Meta{Origin: "y"})
	if m, _ := c.get("0"); m.Origin != "y" || len(c.m) != metaCacheMax {
		t.Fatalf("storing a key again replaces it in place: %+v, %d entries", m, len(c.m))
	}
}

package core

import "testing"

// Passes over twice as many keys as fit still hit for a good share of
// them, where emptying the cache when full hit for none.
func TestMakeRoomKeepsMostOfABigWorkingSet(t *testing.T) {
	const limit = 1000
	m := map[int]bool{}
	hits := 0
	for pass := range 5 {
		for k := range 2 * limit {
			if m[k] && pass > 0 {
				hits++
				continue
			}
			makeRoom(m, limit)
			m[k] = true
			if len(m) > limit {
				t.Fatalf("%d entries, past the limit of %d", len(m), limit)
			}
		}
	}
	if share := float64(hits) / (4 * 2 * limit); share < 0.2 {
		t.Fatalf("%.0f%% of lookups hit, want at least 20%%", share*100)
	}
}

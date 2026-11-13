package core

// makeRoom drops one entry of m when it holds limit or more, so one more
// fits. Which one is arbitrary (Go randomises a map's order), so a pass
// over a working set bigger than limit, such as a reconcile's or a task
// list's, still finds most entries: emptying the map when full, or
// dropping the least recently used, would miss on every one and turn each
// into a forge call.
func makeRoom[K comparable, V any](m map[K]V, limit int) {
	for k := range m {
		if len(m) < limit {
			return
		}
		delete(m, k)
	}
}

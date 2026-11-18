package slack

import (
	"bytes"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// counters are this process's Slack counters, by series name and then by
// rendered labels.
type counters struct {
	mu sync.Mutex
	m  map[string]map[string]int
}

var metrics = &counters{m: map[string]map[string]int{}}

// seriesNames are the counters WriteMetrics writes, each even at nothing.
var seriesNames = []string{
	"devbot_slack_installs_total",
	"devbot_slack_uninstalls_total",
	"devbot_slack_requests_total",
	"devbot_slack_duplicates_total",
	"devbot_slack_disallowed_total",
	"devbot_slack_api_errors_total",
	"devbot_slack_posts_dropped_total",
}

// add counts one in name's series with labels, given as name, value pairs.
func (c *counters) add(name string, labels ...string) {
	var parts []string
	for i := 0; i+1 < len(labels); i += 2 {
		parts = append(parts, fmt.Sprintf("%s=%q", labels[i], labels[i+1]))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m[name] == nil {
		c.m[name] = map[string]int{}
	}
	c.m[name][strings.Join(parts, ",")]++
}

// codeRE is a Slack error code. Anything else, which Slack doesn't send, is
// counted as "other", so a label can't grow without bound.
var codeRE = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

func apiError(method, code string) {
	if !codeRE.MatchString(code) {
		code = "other"
	}
	metrics.add("devbot_slack_api_errors_total", "method", method, "error", code)
}

// WriteMetrics writes the Slack counters in the Prometheus text format.
func WriteMetrics(b *bytes.Buffer) {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	for _, name := range seriesNames {
		fmt.Fprintf(b, "# TYPE %s counter\n", name)
		for _, l := range slices.Sorted(maps.Keys(metrics.m[name])) {
			fmt.Fprintf(b, "%s{%s} %d\n", name, l, metrics.m[name][l])
		}
	}
}

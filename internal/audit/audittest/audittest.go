// Package audittest captures audit records in tests.
package audittest

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"git.shdw.tech/rob/devbot/internal/audit"
)

// Recorder is an audit.Logger whose output a test can read back.
type Recorder struct {
	*audit.Logger
	t   testing.TB
	buf *syncBuffer
}

func New(t testing.TB, component string) *Recorder {
	b := &syncBuffer{}
	return &Recorder{Logger: audit.New(b, component), t: t, buf: b}
}

// Raw is everything written so far.
func (r *Recorder) Raw() string { return r.buf.String() }

// Records parses every line written so far. A line that isn't a JSON object
// fails the test.
func (r *Recorder) Records() []map[string]any {
	r.t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(r.buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			r.t.Fatalf("audit line isn't a JSON object: %v\n%s", err, l)
		}
		out = append(out, m)
	}
	return out
}

// Of is the records of event, in order.
func (r *Recorder) Of(event string) []map[string]any {
	r.t.Helper()
	var out []map[string]any
	for _, m := range r.Records() {
		if m["event"] == event {
			out = append(out, m)
		}
	}
	return out
}

// Events lists every record's event, in order.
func (r *Recorder) Events() []string {
	r.t.Helper()
	var out []string
	for _, m := range r.Records() {
		out = append(out, m["event"].(string))
	}
	return out
}

// Reset forgets what was written.
func (r *Recorder) Reset() { r.buf.Reset() }

// Detail is m's detail object.
func Detail(m map[string]any) map[string]any {
	d, _ := m["detail"].(map[string]any)
	return d
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

package forge

import (
	"strings"
	"testing"
)

func TestTailLines(t *testing.T) {
	long := strings.Repeat("x", 200<<10)
	for _, c := range []struct {
		in, want string
		n        int
		cut      bool
	}{
		{"", "", 3, false},
		{"a\nb\n", "a\nb", 3, false},
		{"a\nb\nc\nd", "b\nc\nd", 3, true},
		{"a\n\n\nb\n", "\n\nb", 3, true},
		{"a\n" + long + "\nb\n", "a\n" + long[:MaxLogLine] + "\nb", 3, false},
		{"a\nb\n", "", 0, false},
	} {
		got, cut, err := TailLines(strings.NewReader(c.in), c.n)
		if err != nil || got != c.want || cut != c.cut {
			t.Errorf("%.20q, %d: got %.40q, cut %v, %v; want %.40q, cut %v", c.in, c.n, got, cut, err, c.want, c.cut)
		}
	}
}

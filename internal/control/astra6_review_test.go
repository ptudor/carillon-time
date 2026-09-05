package control

import (
	"math"
	"testing"
	"time"
)

// TestAstra6WaitDurationIsBounded covers the control-server half of
// RA6X-035. A request arrives as JSON on a local socket and need not come
// from carillonctl, so an out-of-range or non-finite timeout must not
// overflow the seconds-to-duration conversion into a deadline in the past.
func TestAstra6WaitDurationIsBounded(t *testing.T) {
	cases := []struct {
		name  string
		secs  float64
		want  time.Duration
		exact bool
	}{
		{"nan", math.NaN(), 0, true},
		{"negative", -1, 0, true},
		{"zero", 0, 0, true},
		{"+inf", math.Inf(1), time.Duration(maxWaitDuration), true},
		{"-inf", math.Inf(-1), 0, true},
		{"overflowing", 1e30, time.Duration(maxWaitDuration), true},
		{"one second", 1, time.Second, true},
		{"an hour", 3600, time.Hour, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := waitDuration(c.secs)
			if got < 0 {
				t.Fatalf("waitDuration(%v) = %v, which is a deadline in the past", c.secs, got)
			}
			if got > time.Duration(maxWaitDuration) {
				t.Fatalf("waitDuration(%v) = %v, above the bound", c.secs, got)
			}
			if c.exact && got != c.want {
				t.Fatalf("waitDuration(%v) = %v, want %v", c.secs, got, c.want)
			}
			// The server adds a reply allowance on top; that must not
			// overflow either.
			if got+replyWriteTimeout < 0 {
				t.Fatalf("waitDuration(%v) + reply allowance overflowed", c.secs)
			}
		})
	}
}

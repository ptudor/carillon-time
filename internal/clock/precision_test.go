package clock

import (
	"testing"
	"time"
)

// stepping returns a fake clock read that advances by step on every call,
// with an occasional larger gap so the minimum-delta search has something
// to reject.
func stepping(step time.Duration) func() time.Time {
	t := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	n := 0
	return func() time.Time {
		n++
		t = t.Add(step)
		if n%97 == 0 {
			t = t.Add(50 * step)
		}
		return t
	}
}

func TestMeasurePrecision(t *testing.T) {
	cases := []struct {
		name string
		step time.Duration
		want int8
	}{
		{"1 µs", time.Microsecond, -20},
		{"30 ns", 30 * time.Nanosecond, -25}, // 2^-25 s is 29.8 ns; floor(log2(30e-9)) = -25
		{"1 ns", time.Nanosecond, -30},       // floor(log2(1e-9)) = -30
		{"1 ms", time.Millisecond, -10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := measurePrecision(stepping(c.step)); got != c.want {
				t.Fatalf("got %d want %d", got, c.want)
			}
		})
	}
}

func TestMeasurePrecisionFrozenClock(t *testing.T) {
	fixed := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	if got := measurePrecision(func() time.Time { return fixed }); got != -30 {
		t.Fatalf("frozen clock: got %d want -30", got)
	}
}

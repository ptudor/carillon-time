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

// TestMeasurePrecisionIsNotTheMinimum covers RF5X-020. A clock that mostly
// ticks in microseconds but occasionally shows a one-nanosecond difference —
// which is what a TSC-backed CLOCK_REALTIME does between back-to-back reads —
// must report its typical increment, not the smallest one ever seen.
func TestMeasurePrecisionIsNotTheMinimum(t *testing.T) {
	base := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	n := 0
	occasionalNanosecond := func() time.Time {
		n++
		if n%50 == 0 {
			base = base.Add(time.Nanosecond)
		} else {
			base = base.Add(time.Microsecond)
		}
		return base
	}
	if got := measurePrecision(occasionalNanosecond); got != -20 {
		t.Fatalf("precision %d, want -20 (1 µs): a single 1 ns delta must not set it", got)
	}
}

// TestMeasurePrecisionCoarseClockTerminates checks the read budget: a clock
// that only moves every so often must not spin the startup path.
func TestMeasurePrecisionCoarseClockTerminates(t *testing.T) {
	base := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	n := 0
	coarse := func() time.Time {
		n++
		if n%1000 == 0 {
			base = base.Add(time.Millisecond)
		}
		return base
	}
	if got := measurePrecision(coarse); got != -10 {
		t.Fatalf("precision %d, want -10 (1 ms)", got)
	}
	if n > precisionMaxReads+2 {
		t.Fatalf("%d clock reads, budget is %d", n, precisionMaxReads)
	}
}

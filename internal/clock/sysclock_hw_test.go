//go:build (linux || freebsd) && hwtest

package clock

import (
	"os"
	"testing"
	"time"
)

// TestSystemClock exercises the real backend on a target host. It never
// changes the clock: the frequency is written back with the value it
// already had. Run as a user allowed to adjust the clock with:
//
//	CARILLON_HW_TESTS=1 go test -tags hwtest ./internal/clock/ -run TestSystemClock
func TestSystemClock(t *testing.T) {
	if os.Getenv("CARILLON_HW_TESTS") != "1" {
		t.Skip("set CARILLON_HW_TESTS=1 on a target host to run")
	}
	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	freq, err := c.Frequency()
	if err != nil {
		t.Fatalf("Frequency: %v", err)
	}
	if freq < -maxFrequencyPPM || freq > maxFrequencyPPM {
		t.Fatalf("frequency %v ppm out of range", freq)
	}
	if err := c.SetFrequency(freq); err != nil {
		t.Fatalf("SetFrequency(%v): %v", freq, err)
	}
	again, err := c.Frequency()
	if err != nil {
		t.Fatalf("Frequency: %v", err)
	}
	if d := again - freq; d > 1/freqScale || d < -1/freqScale {
		t.Fatalf("frequency changed by write-back: %v -> %v", freq, again)
	}
	if p := c.Precision(); p < -30 || p > -6 {
		t.Fatalf("precision %d out of range", p)
	}

	w0, m0 := c.Now(), c.Monotonic()
	time.Sleep(20 * time.Millisecond)
	w1, m1 := c.Now(), c.Monotonic()
	if !w1.After(w0) {
		t.Fatalf("wall clock did not advance: %v -> %v", w0, w1)
	}
	if m1-m0 < 0.015 || m1-m0 > 1 {
		t.Fatalf("monotonic advanced %v s over a 20 ms sleep", m1-m0)
	}
}

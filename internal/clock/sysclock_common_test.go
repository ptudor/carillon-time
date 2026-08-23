package clock

import (
	"testing"
	"time"

	"carillon/internal/ntp"
)

func TestFreqWord(t *testing.T) {
	cases := []struct {
		ppm  float64
		want int64
	}{
		{0, 0},
		{1, 65536},
		{-1, -65536},
		{12.5, 819200},
		{500, 500 * 65536},
		{600, 500 * 65536},
		{-600, -500 * 65536},
		{0.0000076, 0}, // below half a unit rounds to 0
	}
	for _, c := range cases {
		if got := freqWord(c.ppm); got != c.want {
			t.Errorf("freqWord(%v) = %d, want %d", c.ppm, got, c.want)
		}
	}
	if got := freqPPM(819200); got != 12.5 {
		t.Errorf("freqPPM(819200) = %v", got)
	}
	// Round trip within a unit of the scaled representation.
	for _, ppm := range []float64{-499.999, -37.25, 0.001, 123.456, 499.999} {
		if back := freqPPM(freqWord(ppm)); back-ppm > 1/freqScale || ppm-back > 1/freqScale {
			t.Errorf("round trip %v -> %v", ppm, back)
		}
	}
}

func TestErrorMicros(t *testing.T) {
	if errorMicros(-time.Second) != 0 {
		t.Error("negative must clamp to 0")
	}
	if errorMicros(1500*time.Microsecond) != 1500 {
		t.Error("1.5 ms")
	}
	if errorMicros(time.Hour) != maxErrorMicros {
		t.Error("must clamp to 16 s")
	}
}

func TestStatusWord(t *testing.T) {
	const ro = 0x2000 | 0x0100 // STA_NANO | STA_PPSSIGNAL: not ours, must survive
	current := int32(ro | staPLL | staPPSFREQ | staFLL | staINS)
	if w := statusWord(current, Status{Synced: true}); w != ro {
		t.Errorf("synced: %#x want %#x", w, ro)
	}
	if w := statusWord(current, Status{Synced: false}); w != ro|staUNSYNC {
		t.Errorf("unsynced: %#x", w)
	}
	if w := statusWord(current, Status{Synced: true, Leap: ntp.LeapInsert}); w != ro|staINS {
		t.Errorf("insert: %#x", w)
	}
	if w := statusWord(current, Status{Synced: true, Leap: ntp.LeapDelete}); w != ro|staDEL {
		t.Errorf("delete: %#x", w)
	}
	if w := statusWord(current, Status{Synced: true, Leap: ntp.LeapUnsync}); w != ro|staUNSYNC {
		t.Errorf("leap-unsync: %#x", w)
	}
}

func TestSplitDuration(t *testing.T) {
	cases := []struct {
		d         time.Duration
		sec, nsec int64
	}{
		{0, 0, 0},
		{1500 * time.Millisecond, 1, 500_000_000},
		{-1500 * time.Millisecond, -2, 500_000_000},
		{-time.Nanosecond, -1, 999_999_999},
		{-time.Second, -1, 0},
		{3 * time.Second, 3, 0},
	}
	for _, c := range cases {
		sec, nsec := splitDuration(c.d)
		if sec != c.sec || nsec != c.nsec {
			t.Errorf("splitDuration(%v) = (%d, %d), want (%d, %d)", c.d, sec, nsec, c.sec, c.nsec)
		}
	}
}

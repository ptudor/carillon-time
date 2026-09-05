package discipline

import (
	"math"
	"testing"
)

func TestFilterSingleSample(t *testing.T) {
	f := NewFilter(1e-6)
	out, ok := f.Add(0.010, 0.020, 0.001, 100)
	if !ok {
		t.Fatal("first sample must update")
	}
	if out.Offset != 0.010 || out.Delay != 0.020 || out.At != 100 || out.Samples != 1 {
		t.Fatalf("got %+v", out)
	}
	if out.Jitter != 1e-6 {
		t.Fatalf("jitter must floor at precision, got %v", out.Jitter)
	}
	if out.Dispersion != 0.0005 { // 0.001 · 2^-1
		t.Fatalf("dispersion got %v", out.Dispersion)
	}
}

func TestFilterMinDelayWins(t *testing.T) {
	f := NewFilter(1e-6)
	f.Add(0.001, 0.010, 0, 0)
	out, ok := f.Add(0.002, 0.005, 0, 64)
	if !ok || out.Offset != 0.002 || out.Delay != 0.005 {
		t.Fatalf("lower delay sample must be chosen: ok=%v %+v", ok, out)
	}
	// A new sample with a larger delay does not displace the older best;
	// the filter reports it as not updated.
	out, ok = f.Add(0.003, 0.020, 0, 128)
	if ok {
		t.Fatalf("stale best must not report an update: %+v", out)
	}
	if out.Offset != 0.002 {
		t.Fatalf("output must still describe the best sample: %+v", out)
	}
	if f.Len() != 3 {
		t.Fatalf("len %d", f.Len())
	}
}

func TestFilterJitter(t *testing.T) {
	f := NewFilter(1e-6)
	f.Add(0.000, 0.010, 0, 0)
	f.Add(0.004, 0.011, 0, 64)
	// Ranking is by root distance, so by t=128 the first sample's grown
	// dispersion (φ·128 = 1.92 ms) has cost it its 1 ms delay advantage and
	// the fresh sample wins (RA6X-002).
	out, ok := f.Add(-0.004, 0.012, 0, 128)
	if !ok {
		t.Fatal("the fresh sample must displace one whose dispersion has grown past its delay advantage")
	}
	if out.Offset != -0.004 || out.At != 128 {
		t.Fatalf("output must describe the fresh sample: %+v", out)
	}
	// RMS of the other two offsets against the best (-0.004):
	// sqrt((4² + 8²)/2) ms.
	if want := math.Sqrt((16 + 64) / 2.0) * 1e-3; math.Abs(out.Jitter-want) > 1e-9 {
		t.Fatalf("jitter got %v want %v", out.Jitter, want)
	}
	if out.Samples != 3 {
		t.Fatalf("samples %d", out.Samples)
	}
}

func TestFilterDispersionAges(t *testing.T) {
	f := NewFilter(1e-6)
	f.Add(0, 0.010, 0.001, 0)
	out, _ := f.Add(0, 0.010, 0.001, 1000)
	// The old sample's dispersion grew by Phi·1000 = 15 ms. With equal
	// delays the fresher sample ranks first and contributes ε/2; the old
	// one ranks second and contributes ε/4.
	want := 0.001/2 + (0.001+Phi*1000)/4
	if math.Abs(out.Dispersion-want) > 1e-12 {
		t.Fatalf("dispersion got %v want %v", out.Dispersion, want)
	}
}

func TestFilterOldSamplesDemoted(t *testing.T) {
	f := NewFilter(1e-6)
	f.Add(0.001, 0.001, 0, 0) // excellent delay, but will be very old
	out, ok := f.Add(0.005, 0.030, 0, AllanIntercept+100)
	if !ok || out.Offset != 0.005 {
		t.Fatalf("sample older than the Allan intercept must not win: ok=%v %+v", ok, out)
	}
}

func TestFilterMaxDispersionUnusable(t *testing.T) {
	f := NewFilter(1e-6)
	if _, ok := f.Add(0, 0.010, MaxDispersion, 0); ok {
		t.Fatal("a sample at MaxDispersion carries no information")
	}
}

func TestFilterReset(t *testing.T) {
	f := NewFilter(1e-6)
	f.Add(0.001, 0.010, 0, 0)
	f.Reset()
	if f.Len() != 0 {
		t.Fatal("reset must empty the register")
	}
	if _, ok := f.Add(0.002, 0.050, 0, 64); !ok {
		t.Fatal("after reset the next sample is new")
	}
}

func TestFilterRingWraps(t *testing.T) {
	f := NewFilter(1e-6)
	for i := 0; i < 20; i++ {
		f.Add(float64(i)*1e-3, 0.010+float64(i)*1e-4, 0, float64(i)*64)
	}
	if f.Len() != FilterStages {
		t.Fatalf("len %d", f.Len())
	}
}

// TestFilterWithholdsUpdatesWhileAnEarlyBestSampleStands is the starvation
// regression, observed live on 2026-09-05 and fixed by RA6X-002.
//
// Add reports updated = false whenever the best sample in the register is one
// already reported, and System's loop-update gate keys on the same thing, so
// a withheld run is a run with no loop update at all. Ranking by raw delay,
// as RFC 5905 §10 and ntpd do, let one unusually fast early reply hold that
// place until the Allan intercept demoted it: 2048 s at poll 8, and 2533 s
// measured on `gummi`. Ranking by root distance costs it the place as soon as
// φ·age exceeds half its delay advantage, which is what this bounds.
func TestFilterWithholdsUpdatesWhileAnEarlyBestSampleStands(t *testing.T) {
	f := NewFilter(1e-6)
	const poll = 256.0 // seconds; poll 8

	// One unusually fast reply, then a long run of ordinary ones.
	if _, ok := f.Add(0.001, 0.010, 1e-4, poll); !ok {
		t.Fatal("the first sample must update")
	}
	lastAccepted, maxGap := poll, 0.0
	for i := 2; i <= 12; i++ {
		now := float64(i) * poll
		if _, ok := f.Add(0.001+float64(i)*1e-3, 0.030, 1e-4, now); ok {
			if gap := now - lastAccepted; gap > maxGap {
				maxGap = gap
			}
			lastAccepted = now
		}
	}
	t.Logf("longest run with no filter update: %.0f s (%.1f min) at poll 8", maxGap, maxGap/60)
	t.Logf("at the 17 ppm gummi was carrying, that is %.0f ms of uncorrected time error",
		maxGap*17e-6*1e3)
	// The 20 ms delay advantage is worth 10 ms to the offset estimate, so
	// the early sample loses its place once φ·age passes 10 ms: about 667 s,
	// which at poll 8 is the third arrival after it. The bound is the
	// crossover, not the Allan intercept.
	if want := 10e-3 / Phi; maxGap > want+poll {
		t.Fatalf("withholding run %.0f s, longer than the %.0f s distance crossover", maxGap, want)
	}
	if maxGap >= AllanIntercept*0.9 {
		t.Fatalf("the run still lasts until the Allan intercept: %.0f s", maxGap)
	}
}

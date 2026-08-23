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
	out, ok := f.Add(-0.004, 0.012, 0, 128)
	if ok {
		t.Fatal("no update expected")
	}
	// RMS of the other two offsets against the best (0): sqrt((16+16)/2) ms
	if math.Abs(out.Jitter-0.004) > 1e-9 {
		t.Fatalf("jitter got %v want 0.004", out.Jitter)
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

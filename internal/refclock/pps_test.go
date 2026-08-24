package refclock

import (
	"math"
	"testing"
	"time"

	"carillon/internal/clock"
	"carillon/internal/ntp"
	"carillon/internal/pps"
)

func testPPS(t *testing.T) (*PPS, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	cfg := PPSConfig{
		Name: "pps0", Device: "/dev/pps0", Edge: pps.Assert,
		LockJitter: 200e-6, PollMin: 4, PollMax: 4,
	}
	return newPPS(cfg, clk, nil, nil, nil), clk
}

func TestEdgeOffset(t *testing.T) {
	base := time.Unix(100, 0)
	for _, tc := range []struct {
		d    time.Duration
		want float64
	}{
		{200 * time.Microsecond, -200e-6},
		{999800 * time.Microsecond, 200e-6},
		{0, 0},
	} {
		if got := edgeOffset(base.Add(tc.d)); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("edgeOffset(%v) = %.9f, want %.9f", tc.d, got, tc.want)
		}
	}
}

func TestMedianMAD(t *testing.T) {
	median, mad := medianMAD([]float64{10, 1, 3, 2, 100, 4})
	if median != 3.5 || mad != 2 {
		t.Fatalf("median/MAD = %v/%v", median, mad)
	}
}

func TestPPSStableWindowEmitsMeasurement(t *testing.T) {
	p, clk := testPPS(t)
	p.cfg.Offset = 50e-6
	base := time.Unix(1_800_000_000, 0)
	noise := []time.Duration{-200, 100, -100, 200}
	var lastValid bool
	var gotOffset float64
	for i := 1; i <= 16; i++ {
		clk.Advance(time.Second)
		ts := base.Add(time.Duration(i)*time.Second + 200*time.Microsecond + noise[i%len(noise)]*time.Nanosecond)
		m := p.accept(pps.Sample{Sequence: uint32(i), Time: ts})
		if i < 16 && m.Valid {
			t.Fatalf("measurement emitted at sample %d", i)
		}
		if i == 16 {
			lastValid = m.Valid
			gotOffset = m.Offset
			if m.Stratum != 0 || m.RefID != ntp.RefIDFromString("PPS") || m.SourceRefID != m.RefID {
				t.Fatalf("bad PPS identity: %+v", m)
			}
			if m.Invalidate {
				t.Fatal("stable measurement marked invalidating")
			}
		}
	}
	if !lastValid {
		t.Fatal("full low-jitter window did not lock")
	}
	if math.Abs(gotOffset-(-150e-6)) > 500e-9 {
		t.Fatalf("offset %.9f, want about -0.000150", gotOffset)
	}
	info := p.Info()
	if info.Reach != 0xff || info.Received != 16 || !info.Refclock.Stable || info.Refclock.WindowSamples != 16 {
		t.Fatalf("info: %+v refclock=%+v", info, info.Refclock)
	}
}

func TestPPSSpikeGapGlitchAndTimeout(t *testing.T) {
	p, clk := testPPS(t)
	base := time.Unix(1_800_000_000, 0)
	for i := 1; i <= 4; i++ {
		clk.Advance(time.Second)
		p.accept(pps.Sample{Sequence: uint32(i), Time: base.Add(time.Duration(i) * time.Second)})
	}
	clk.Advance(time.Second)
	p.accept(pps.Sample{Sequence: 5, Time: base.Add(5*time.Second + 10*time.Millisecond)})
	if p.Info().Refclock.Spikes != 1 || p.reach&1 != 0 {
		t.Fatalf("spike not rejected: reach=%08b info=%+v", p.reach, p.Info().Refclock)
	}

	// Sequence 6 was missed; sequence 7 at the correct two-second interval
	// is accepted and records one gap.
	clk.Advance(2 * time.Second)
	p.accept(pps.Sample{Sequence: 7, Time: base.Add(7 * time.Second)})
	if p.Info().Refclock.Gaps != 1 || p.reach&1 == 0 {
		t.Fatalf("gap handling: reach=%08b info=%+v", p.reach, p.Info().Refclock)
	}

	clk.Advance(time.Second)
	p.accept(pps.Sample{Sequence: 8, Time: base.Add(7500 * time.Millisecond)})
	if p.Info().Refclock.Glitches != 1 || p.reach&1 != 0 {
		t.Fatalf("glitch handling: reach=%08b info=%+v", p.reach, p.Info().Refclock)
	}

	p.timeout()
	if p.Info().Refclock.Timeouts != 1 || p.Info().Timeouts != 1 {
		t.Fatalf("timeout counters: %+v", p.Info())
	}
}

func TestPPSUnstableWindowInvalidates(t *testing.T) {
	p, clk := testPPS(t)
	p.cfg.LockJitter = 1e-9
	base := time.Unix(1_800_000_000, 0)
	var mValid, mInvalid bool
	for i := 1; i <= 16; i++ {
		clk.Advance(time.Second)
		noise := time.Duration((i%4)-2) * time.Microsecond
		m := p.accept(pps.Sample{Sequence: uint32(i), Time: base.Add(time.Duration(i)*time.Second + noise)})
		mValid, mInvalid = m.Valid, m.Invalidate
	}
	if mValid || !mInvalid || p.Info().Refclock.Stable {
		t.Fatalf("unstable window: valid=%v invalidate=%v info=%+v", mValid, mInvalid, p.Info().Refclock)
	}
}

func TestPPSResetDropsWindowKeepsReach(t *testing.T) {
	p, _ := testPPS(t)
	p.window = append(p.window, 0, 1e-9)
	p.intervals = append(p.intervals, 1e-9)
	p.reach = 0x7f
	p.stable = true
	p.Reset()
	p.accept(pps.Sample{Sequence: 1, Time: time.Unix(100, 0)})
	if len(p.window) != 1 || p.reach != 0xff || p.stable {
		t.Fatalf("reset: window=%d reach=%08b stable=%v", len(p.window), p.reach, p.stable)
	}
}

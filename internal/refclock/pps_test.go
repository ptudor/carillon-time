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
		LockJitter: 200e-6, PollMin: 4, PollMax: 4, MaxSlewPPM: 500,
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

func TestPPSLockHysteresis(t *testing.T) {
	if ppsWindowStable(false, 3, 0, 1) || ppsWindowStable(false, 8, 1, 1) {
		t.Fatal("window locked without enough samples or below-threshold jitter")
	}
	if !ppsWindowStable(false, 8, 0.9, 1) {
		t.Fatal("window did not acquire below threshold")
	}
	if !ppsWindowStable(true, 8, 4, 1) || ppsWindowStable(true, 8, 4.1, 1) {
		t.Fatal("window did not retain lock through the 4x hysteresis band")
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

// TestPPSSpikeGateSurvivesLoopSlew reproduces RF5X-001: while the PPS is the
// system source the discipline loop slews the local clock by the offset the
// PPS just reported, so every subsequent edge moves. A gate anchored to a
// window that rejected samples can no longer refresh rejects the whole train
// and never recovers.
func TestPPSSpikeGateSurvivesLoopSlew(t *testing.T) {
	p, clk := testPPS(t)
	p.cfg.LockJitter = 20e-6
	base := time.Unix(1_800_000_000, 0)
	noise := []time.Duration{-100, 50, -50, 100, 0, 80, -80, 20} // ns
	seq := uint32(0)
	accepted, rejected := 0, 0
	feed := func(theta time.Duration) {
		seq++
		clk.Advance(time.Second)
		before := p.Info().Refclock.Spikes
		p.accept(pps.Sample{
			Sequence: seq,
			Time:     base.Add(time.Duration(seq)*time.Second + theta + noise[int(seq)%len(noise)]),
		})
		if p.Info().Refclock.Spikes > before {
			rejected++
		} else {
			accepted++
		}
	}

	for range 32 { // stable window at a fixed offset
		feed(200 * time.Microsecond)
	}
	if !p.stable || p.reach != 0xff {
		t.Fatalf("stable phase: stable=%v reach=%08b", p.stable, p.reach)
	}

	accepted, rejected = 0, 0
	for i := 1; i <= 300; i++ { // the loop slews it away with tau = 64 s
		feed(time.Duration(200e-6 * math.Exp(-float64(i)/64) * 1e9))
	}
	if accepted <= 250 {
		t.Fatalf("slew phase: accepted=%d rejected=%d, want more than 250 accepted", accepted, rejected)
	}

	accepted, rejected = 0, 0
	for range 100 { // steady at zero offset once the correction is done
		feed(0)
	}
	if accepted != 100 {
		t.Fatalf("steady phase: accepted=%d rejected=%d, want all 100 accepted", accepted, rejected)
	}
	if p.reach != 0xff {
		t.Fatalf("reach = %08b, want 11111111", p.reach)
	}
	if spikes := p.Info().Refclock.Spikes; spikes > 8 {
		t.Fatalf("spikes = %d, want at most a handful", spikes)
	}
}

// TestPPSSpikeRejectedAndRecovers is the other half of RF5X-001: widening the
// gate for the loop's own slew must not stop it catching a real outlier, and
// the pulse after the outlier must still be accepted.
func TestPPSSpikeRejectedAndRecovers(t *testing.T) {
	p, clk := testPPS(t)
	base := time.Unix(1_800_000_000, 0)
	noise := []time.Duration{-100, 50, -50, 100}
	seq := uint32(0)
	feed := func(theta time.Duration) {
		seq++
		clk.Advance(time.Second)
		p.accept(pps.Sample{
			Sequence: seq,
			Time:     base.Add(time.Duration(seq)*time.Second + theta + noise[int(seq)%len(noise)]),
		})
	}
	for range 16 {
		feed(200 * time.Microsecond)
	}
	if !p.stable {
		t.Fatal("window did not lock before the spike")
	}

	before := len(p.window)
	feed(200*time.Microsecond + 10*time.Millisecond)
	if p.Info().Refclock.Spikes != 1 {
		t.Fatalf("spikes = %d, want 1", p.Info().Refclock.Spikes)
	}
	if len(p.window) != before || p.reach&1 != 0 {
		t.Fatalf("spike entered the window (%d -> %d) or set reach %08b", before, len(p.window), p.reach)
	}

	feed(200 * time.Microsecond)
	if p.reach&1 != 1 {
		t.Fatalf("pulse after the spike was not accepted: reach=%08b", p.reach)
	}
}

// TestPPSTimeoutRePrimesWindowWhileUnreachable covers the second half of the
// RF5X-001 fix: the window must be dropped on every unreachable fetch, not
// only on the transition into unreachable.
func TestPPSTimeoutRePrimesWindowWhileUnreachable(t *testing.T) {
	p, clk := testPPS(t)
	base := time.Unix(1_800_000_000, 0)
	for i := 1; i <= 8; i++ {
		clk.Advance(time.Second)
		p.accept(pps.Sample{Sequence: uint32(i), Time: base.Add(time.Duration(i) * time.Second)})
	}
	for range 8 { // reach empties, window is dropped on the transition
		p.timeout()
	}
	if p.reach != 0 || len(p.window) != 0 {
		t.Fatalf("after the transition: reach=%08b window=%d", p.reach, len(p.window))
	}
	// A pulse arrives, is accepted, and is then lost again without reach ever
	// leaving zero: the stale single-sample window must still be dropped.
	clk.Advance(time.Second)
	p.accept(pps.Sample{Sequence: 100, Time: base.Add(100 * time.Second)})
	p.reach = 0
	p.timeout()
	if len(p.window) != 0 {
		t.Fatalf("window kept while unreachable: %d samples", len(p.window))
	}
}

// TestPPSSequenceRestartIsNotFourBillionGaps covers RF5X-026. The sequence
// delta is unsigned, so a device-side counter restart — ldattach restarting,
// /dev/ppsN recreated under the same name, another process issuing
// PPS_IOC_DESTROY/CREATE on the same tty — read as about 2^32 missed pulses
// and added that to a monotonic Gaps counter that could never look right
// again, while overflowing the emit cadence.
func TestPPSSequenceRestartIsNotFourBillionGaps(t *testing.T) {
	p, clk := testPPS(t)
	base := time.Unix(1_800_000_000, 0)
	for i := 1; i <= 1000; i++ {
		clk.Advance(time.Second)
		p.accept(pps.Sample{Sequence: uint32(i), Time: base.Add(time.Duration(i) * time.Second)})
	}
	gaps := p.Info().Refclock.Gaps
	glitches := p.Info().Refclock.Glitches

	// The device comes back with its counter at 5.
	clk.Advance(time.Second)
	p.accept(pps.Sample{Sequence: 5, Time: base.Add(1001 * time.Second)})
	info := p.Info().Refclock
	if info.Gaps != gaps {
		t.Fatalf("gaps %d -> %d: a counter restart is not a gap", gaps, info.Gaps)
	}
	if info.Glitches != glitches+1 {
		t.Fatalf("glitches %d -> %d, want one more", glitches, info.Glitches)
	}
	if len(p.window) != 0 || p.slots != 0 {
		t.Fatalf("window kept across a counter restart: %d samples, %d slots", len(p.window), p.slots)
	}

	// The next pulses form a fresh train and are accepted normally.
	for i := 6; i <= 12; i++ {
		clk.Advance(time.Second)
		p.accept(pps.Sample{Sequence: uint32(i), Time: base.Add(time.Duration(1001+i-5) * time.Second)})
	}
	if p.Info().Refclock.Gaps != gaps {
		t.Fatalf("gaps after the restart: %d, want %d", p.Info().Refclock.Gaps, gaps)
	}
	if len(p.window) != 7 {
		t.Fatalf("window re-primed to %d samples, want 7", len(p.window))
	}
}

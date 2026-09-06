package refclock

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/ptudor/carillon-time/internal/clock"
	"github.com/ptudor/carillon-time/internal/discipline"
	"github.com/ptudor/carillon-time/internal/ntp"
	"github.com/ptudor/carillon-time/internal/pps"
)

// ppsSim runs the real PPS refclock in a closed loop against a real
// discipline.System and a fake clock: the pulses the refclock sees are
// timestamped with the local clock the loop is correcting, which is the
// feedback path RF5X-001 broke. A numbering NTP-like source is present
// because a PPS cannot qualify without one.
type ppsSim struct {
	t    *testing.T
	p    *PPS
	clk  *clock.Fake
	sys  *discipline.System
	rng  *rand.Rand
	ntpF *discipline.Filter

	now      float64 // seconds of true time since the start
	nextNTP  float64
	ntpReach uint8
	steps    int
}

func newPPSSim(t *testing.T, offset time.Duration, drift float64) *ppsSim {
	t.Helper()
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	clk.SetLocalOffset(offset)
	clk.SetDrift(drift)
	cfg := PPSConfig{
		Name: "pps0", Device: "/dev/pps0", Edge: pps.Assert,
		LockJitter: 200e-6, PollMin: 4, PollMax: 4, MaxSlewPPM: 500,
	}
	sys := discipline.New(discipline.Config{
		Loop: discipline.LoopConfig{
			StepThreshold: 0.5, StepLimit: 3, Panic: 1000, MaxSlewPPM: 500,
			Precision: 1e-6, FreqMeasure: 900,
		},
		MinSurvivors: 1, HoldoverMax: 3600, SettleUpdates: 3,
	}, 0, false)
	sys.AddSource("ntp", discipline.Options{Numbering: true})
	sys.AddSource("pps0", discipline.Options{PPS: true, Prefer: true})
	return &ppsSim{
		t: t, p: newPPS(cfg, clk, nil, nil, nil), clk: clk, sys: sys,
		rng: rand.New(rand.NewSource(7)), ntpF: discipline.NewFilter(1e-6),
	}
}

func (s *ppsSim) apply(res discipline.Result) {
	for _, a := range res.Actions {
		switch a.Kind {
		case discipline.ActionSetFrequency:
			if err := s.clk.SetFrequency(a.Value); err != nil {
				s.t.Fatalf("SetFrequency: %v", err)
			}
		case discipline.ActionStep:
			if err := s.clk.Step(time.Duration(a.Value * float64(time.Second))); err != nil {
				s.t.Fatalf("Step: %v", err)
			}
			s.steps++
		case discipline.ActionResetFilters:
			s.p.Reset()
			s.ntpF.Reset()
		}
	}
}

// pollNTP feeds one numbering measurement, with a 20 ms round trip and an
// offset error of half the excess delay, as in the discipline simulation.
func (s *ppsSim) pollNTP() {
	excess := math.Abs(s.rng.NormFloat64() * 200e-6)
	sign := 1.0
	if s.rng.Intn(2) == 0 {
		sign = -1
	}
	offset := s.clk.Offset() + sign*excess/2 + s.rng.NormFloat64()*20e-6
	delay := 0.020 + excess
	s.ntpReach = s.ntpReach<<1 | 1
	out, ok := s.ntpF.Add(offset, delay, 2e-6+discipline.Phi*delay, s.now)
	m := discipline.Measurement{
		Source: "ntp", Now: s.now, Reach: s.ntpReach, Poll: 6, Valid: ok,
	}
	if ok {
		m.At = out.At
		m.Offset, m.Delay, m.Dispersion, m.Jitter = out.Offset, out.Delay, out.Dispersion, out.Jitter
		m.Stratum = 2
		m.Leap = ntp.LeapNone
		m.RootDelay, m.RootDisp = 0.001, 0.002
		m.Precision = -20
		m.SourceRefID = ntp.RefIDFromString("ntp")
	}
	s.apply(s.sys.Update(m))
}

// run advances the simulation one true second per pulse.
func (s *ppsSim) run(seconds int, each func()) {
	for i := 0; i < seconds; i++ {
		s.clk.Advance(time.Second)
		s.now++
		if s.now >= s.nextNTP {
			s.pollNTP()
			s.nextNTP = s.now + 64
		}
		// The edge lands on the true second boundary; the kernel stamps it
		// with the local clock, plus a little capture jitter.
		stamp := s.clk.Now().Add(time.Duration(s.rng.NormFloat64() * 300))
		s.apply(s.sys.Update(s.p.accept(pps.Sample{Sequence: uint32(i + 1), Time: stamp})))
		s.apply(s.sys.Tick(s.clk.Monotonic()))
		if each != nil {
			each()
		}
	}
}

// TestPPSSimClosesTheLoop is the closed-loop half of the RF5X-001
// verification: starting 500 µs out, the PPS must end up the system source
// with a full reach register and a sub-10 µs residual, which it cannot do
// while the spike gate rejects the very correction the loop is applying.
func TestPPSSimClosesTheLoop(t *testing.T) {
	s := newPPSSim(t, 500*time.Microsecond, -20)
	s.run(2*3600, nil)

	st := s.sys.Status(s.clk.Monotonic())
	if st.SystemSource != "pps0" {
		t.Fatalf("system source = %q, want pps0 (state %v, pps qualified %v)", st.SystemSource, st.State, st.PPSQualified)
	}
	if s.p.Info().Reach != 0xff {
		t.Fatalf("PPS reach = %08b, want 11111111 (spikes %d)", s.p.Info().Reach, s.p.Info().Refclock.Spikes)
	}
	if st.Stratum != 1 || st.RefID != ntp.RefIDFromString("PPS") {
		t.Fatalf("status stratum=%d refid=%v, want stratum 1 refid PPS", st.Stratum, st.RefID)
	}

	var sum float64
	n := 0
	s.run(600, func() {
		o := s.clk.Offset()
		sum += o * o
		n++
	})
	rms := math.Sqrt(sum / float64(n))
	t.Logf("closed loop: RMS=%.2f µs spikes=%d steps=%d freq=%.2f ppm",
		rms*1e6, s.p.Info().Refclock.Spikes, s.steps, st.Frequency)
	if rms > 10e-6 {
		t.Fatalf("steady-state RMS %.2f µs, want under 10 µs", rms*1e6)
	}
}

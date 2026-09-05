package discipline

import (
	"math"
	"math/rand"
	"testing"

	"carillon/internal/ntp"
)

// simClock models a local clock with an intrinsic frequency error (drift)
// on top of which the loop's frequency word is applied. Offsets follow the
// daemon's convention: true minus local.
type simClock struct {
	trueT float64
	local float64
	drift float64 // ppm, intrinsic
	freq  float64 // ppm, applied by the loop
}

func (c *simClock) advance(dt float64) {
	c.trueT += dt
	c.local += dt * (1 + (c.drift+c.freq)*1e-6)
}

func (c *simClock) offset() float64 { return c.trueT - c.local }

// simSource is an NTP-like source polled at a fixed interval. Its delay
// varies, and its offset error is half the excess delay (as on a real
// network, where queuing on one leg shifts the measured offset), so the
// clock filter's minimum-delay rule genuinely helps.
type simSource struct {
	name    string
	poll    int8
	base    float64 // base round-trip delay
	noise   float64 // σ of the delay excess
	bias    float64 // a wrong source's constant error
	stratum uint8
	filter  *Filter
	rng     *rand.Rand
	next    float64
	reach   uint8
	stopAt  float64 // no samples at or after this time (0 = never stops)
}

func newSimSource(name string, seed int64) *simSource {
	return &simSource{
		name: name, poll: 6, base: 0.020, noise: 200e-6, stratum: 2,
		filter: NewFilter(1e-6), rng: rand.New(rand.NewSource(seed)),
	}
}

func (s *simSource) sample(sys *System, clk *simClock, now float64) Result {
	if s.stopAt > 0 && now >= s.stopAt {
		s.reach <<= 1
		return sys.Update(Measurement{Source: s.name, Now: now, Reach: s.reach, Poll: s.poll})
	}
	excess := math.Abs(s.rng.NormFloat64() * s.noise)
	sign := 1.0
	if s.rng.Intn(2) == 0 {
		sign = -1
	}
	offset := clk.offset() + s.bias + sign*excess/2 + s.rng.NormFloat64()*10e-6
	delay := s.base + excess
	disp := 2e-6 + Phi*delay
	s.reach = s.reach<<1 | 1
	out, ok := s.filter.Add(offset, delay, disp, now)
	m := Measurement{Source: s.name, Now: now, Reach: s.reach, Poll: s.poll, Valid: ok}
	if ok {
		m.At = out.At
		m.Offset, m.Delay, m.Dispersion, m.Jitter = out.Offset, out.Delay, out.Dispersion, out.Jitter
		m.Stratum = s.stratum
		m.Leap = ntp.LeapNone
		m.RootDelay, m.RootDisp = 0.001, 0.002
		m.Precision = -20
		m.SourceRefID = ntp.RefIDFromString(s.name)
	}
	return sys.Update(m)
}

type simRun struct {
	sys     *System
	clk     *simClock
	sources []*simSource
	now     float64
	events  []Event
	steps   int

	// tickJitter, when non-zero, varies the length of each simulated
	// second by ±tickJitter to model an engine ticker that is late or
	// early. True time and the loop's accounting both use the real
	// interval, so a correct loop is unaffected by it.
	tickJitter float64
	tickRNG    *rand.Rand
}

func (r *simRun) apply(res Result) {
	for _, a := range res.Actions {
		switch a.Kind {
		case ActionSetFrequency:
			r.clk.freq = a.Value
		case ActionStep:
			r.clk.local += a.Value
			r.steps++
		case ActionResetFilters:
			for _, s := range r.sources {
				s.filter.Reset()
			}
		}
	}
	r.events = append(r.events, res.Events...)
}

// run advances the simulation by seconds, calling each every second after
// the tick (nil allowed).
func (r *simRun) run(seconds int, each func(t float64)) {
	for i := 0; i < seconds; i++ {
		for _, s := range r.sources {
			if r.now >= s.next {
				r.apply(s.sample(r.sys, r.clk, r.now))
				s.next = r.now + math.Ldexp(1, int(s.poll))
			}
		}
		r.apply(r.sys.Tick(r.now))
		if each != nil {
			each(r.now)
		}
		dt := 1.0
		if r.tickJitter > 0 {
			dt += (r.tickRNG.Float64()*2 - 1) * r.tickJitter
		}
		r.clk.advance(dt)
		r.now += dt
	}
}

func (r *simRun) count(kind EventKind) int {
	n := 0
	for _, e := range r.events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func simConfig() Config {
	return Config{
		Loop: LoopConfig{
			StepThreshold: 0.5, StepLimit: 3, Panic: 1000, MaxSlewPPM: 500,
			Precision: 1e-6, FreqMeasure: 900,
		},
		MinSurvivors:  1,
		HoldoverMax:   3600,
		SettleUpdates: 1,
	}
}

func newSim(cfg Config, drift, offset float64, freqKnown bool, sources ...*simSource) *simRun {
	freq := 0.0
	if freqKnown {
		freq = -drift
	}
	sys := New(cfg, freq, freqKnown)
	for _, s := range sources {
		sys.AddSource(s.name, Options{Numbering: true})
	}
	clk := &simClock{drift: drift, local: -offset}
	return &simRun{sys: sys, clk: clk, sources: sources}
}

// rms of the clock offset over the last `window` seconds of a run.
func (r *simRun) rmsOver(window int) float64 {
	var sum float64
	n := 0
	r.run(window, func(float64) {
		o := r.clk.offset()
		sum += o * o
		n++
	})
	return math.Sqrt(sum / float64(n))
}

func TestSimConvergesFromUnknownFrequency(t *testing.T) {
	// Local clock 50 ppm slow, 100 ms behind, nothing known at start.
	r := newSim(simConfig(), -50, 0.100, false, newSimSource("a", 1))
	r.run(5*3600, nil)
	if r.steps != 0 {
		t.Fatalf("100 ms is below the step threshold; steps=%d", r.steps)
	}
	if r.sys.State() != StateSynced {
		t.Fatalf("state %v", r.sys.State())
	}
	if f := r.clk.freq; math.Abs(f-50) > 1 {
		t.Fatalf("frequency correction %v ppm, want ≈50", f)
	}
	if o := r.clk.offset(); math.Abs(o) > 1e-3 {
		t.Fatalf("offset after 5 h: %v", o)
	}
	rms := r.rmsOver(3600)
	t.Logf("unknown frequency: after 5 h freq=%.2f ppm offset=%.1f µs, 6th-hour RMS=%.1f µs", r.clk.freq, r.clk.offset()*1e6, rms*1e6)
	if rms > 300e-6 {
		t.Fatalf("steady-state RMS %v", rms)
	}
	st := r.sys.Status(r.now)
	if st.Stratum != 3 || st.RefID != ntp.RefIDFromString("a") || st.SystemSource != "a" {
		t.Fatalf("status %+v", st)
	}
	if st.RootDisp <= 0 || st.RootDisp > 0.1 {
		t.Fatalf("root dispersion %v", st.RootDisp)
	}
}

func TestSimConvergesFasterWithKnownFrequency(t *testing.T) {
	r := newSim(simConfig(), 30, 0.050, true, newSimSource("a", 2))
	within := -1.0
	r.run(2*3600, func(now float64) {
		if within < 0 && math.Abs(r.clk.offset()) < 1e-3 {
			within = now
		}
	})
	t.Logf("known frequency: |offset| < 1 ms after %.0f s; after 2 h offset=%.1f µs", within, r.clk.offset()*1e6)
	if r.steps != 0 {
		t.Fatalf("steps=%d", r.steps)
	}
	if o := r.clk.offset(); math.Abs(o) > 1e-3 {
		t.Fatalf("offset after 2 h with a drift file: %v", o)
	}
	if math.Abs(r.clk.freq+30) > 1 {
		t.Fatalf("freq %v", r.clk.freq)
	}
}

func TestSimStepsOnceAtStartup(t *testing.T) {
	r := newSim(simConfig(), -20, 2.0, false, newSimSource("a", 3))
	r.run(3600, nil)
	if r.steps != 1 || r.count(EventStep) != 1 {
		t.Fatalf("steps=%d events=%d", r.steps, r.count(EventStep))
	}
	if r.sys.State() != StateSynced {
		t.Fatalf("state %v", r.sys.State())
	}
	// After the step the offset is already small; the loop takes it from there.
	if o := r.clk.offset(); math.Abs(o) > 0.02 {
		t.Fatalf("offset one hour after the step: %v", o)
	}
	// A late jump is slewed, never stepped, and the slew rate is bounded.
	r.run(3*3600, nil)
	before := r.clk.offset()
	r.clk.local -= 1.0 // the local clock suddenly reads 1 s early
	start := r.now
	r.run(3*3600, func(now float64) {
		if now-start == 100 {
			// At 500 ppm at most 50 ms can have been slewed in 100 s.
			if slewed := 1.0 - (r.clk.offset() - before); slewed > 0.06 {
				t.Fatalf("slew too fast: %v s in 100 s", slewed)
			}
		}
	})
	if r.steps != 1 {
		t.Fatalf("a 1 s offset after startup must be slewed; steps=%d", r.steps)
	}
	if o := r.clk.offset(); math.Abs(o) > 0.01 {
		t.Fatalf("offset 3 h after the jump: %v", o)
	}
}

func TestSimPanicRefused(t *testing.T) {
	r := newSim(simConfig(), 0, 5000, false, newSimSource("a", 4))
	r.run(600, nil)
	if r.steps != 0 || r.count(EventPanicRefused) == 0 {
		t.Fatalf("steps=%d refused=%d", r.steps, r.count(EventPanicRefused))
	}
	if r.sys.State() != StateUnsynced {
		t.Fatalf("state %v", r.sys.State())
	}
	if st := r.sys.Status(r.now); st.Stratum != 16 || st.Leap != ntp.LeapUnsync {
		t.Fatalf("status %+v", st)
	}
	// HOLD means "was synchronized, lost its sources" and sends the
	// operator to look at the network; a clock past the panic threshold is
	// a different problem and says so.
	if st := r.sys.Status(r.now); st.RefID != ntp.KissPANC {
		t.Fatalf("refid %v after a panic refusal, want PANC", st.RefID)
	}

	cfg := simConfig()
	cfg.Loop.PanicAtStartup = true
	r = newSim(cfg, 0, 5000, false, newSimSource("a", 4))
	r.run(3600, nil)
	if r.steps != 1 {
		t.Fatalf("panic_at_startup must step once; steps=%d", r.steps)
	}
	if o := r.clk.offset(); math.Abs(o) > 0.01 {
		t.Fatalf("offset %v", o)
	}
}

func TestSimFalseticker(t *testing.T) {
	a, b, bad := newSimSource("a", 5), newSimSource("b", 6), newSimSource("bad", 7)
	bad.bias = 3.0
	r := newSim(simConfig(), -10, 0.0, true, a, b, bad)
	r.run(2*3600, nil)
	if r.steps != 0 {
		t.Fatalf("a falseticker must not cause a step; steps=%d", r.steps)
	}
	if r.count(EventFalseticker) == 0 {
		t.Fatal("expected a falseticker event")
	}
	st := r.sys.Status(r.now)
	for _, s := range st.Sources {
		if s.Name == "bad" && s.Status != StatusFalseticker {
			t.Fatalf("bad: %v", s.Status)
		}
	}
	if o := r.clk.offset(); math.Abs(o) > 1e-3 {
		t.Fatalf("offset %v", o)
	}
}

func TestSimPreferLost(t *testing.T) {
	a, b, p := newSimSource("a", 8), newSimSource("b", 9), newSimSource("p", 10)
	r := newSim(simConfig(), 0, 0, true, a, b, p)
	r.sys.AddSource("p", Options{Numbering: true, Prefer: true})
	// Before any source has ever been a survivor there is nothing to have
	// lost. Reporting it there logged an ERROR at every daemon start,
	// followed by "back in charge" seconds later: a false page per restart.
	firstSurvivor := -1.0
	lostBeforeSurvivor := 0
	r.run(1800, func(now float64) {
		if firstSurvivor < 0 && len(r.sys.sel.Survivors) > 0 {
			firstSurvivor = now
		}
		if firstSurvivor < 0 {
			lostBeforeSurvivor = r.count(EventPreferLost)
		}
	})
	if lostBeforeSurvivor != 0 {
		t.Fatalf("%d prefer-lost events before the first survivor at t=%.0f", lostBeforeSurvivor, firstSurvivor)
	}
	if st := r.sys.Status(r.now); st.SystemSource != "p" || st.PreferLost {
		t.Fatalf("prefer must drive: %+v", st)
	}
	p.bias = 2.0
	r.run(1800, nil)
	// The filter keeps eight samples, so the prefer source can flap between
	// its old good samples and new bad ones for a few polls; what matters
	// is that it is lost by the end.
	if r.count(EventPreferLost) < 1 {
		t.Fatalf("prefer-lost events %d", r.count(EventPreferLost))
	}
	st := r.sys.Status(r.now)
	if st.SystemSource == "p" || !st.PreferLost {
		t.Fatalf("%+v", st)
	}
	if r.steps != 0 || math.Abs(r.clk.offset()) > 1e-3 {
		t.Fatalf("steps=%d offset=%v", r.steps, r.clk.offset())
	}
	p.bias = 0
	r.run(3600, nil)
	if r.count(EventPreferRegained) < 1 {
		t.Fatalf("prefer-regained events %d", r.count(EventPreferRegained))
	}
	if st := r.sys.Status(r.now); st.SystemSource != "p" || st.PreferLost {
		t.Fatalf("prefer must be back in charge: %+v", st)
	}
}

func TestSimHoldover(t *testing.T) {
	a := newSimSource("a", 11)
	r := newSim(simConfig(), 25, 0, true, a)
	r.run(3600, nil)
	if r.sys.State() != StateSynced {
		t.Fatalf("state %v", r.sys.State())
	}
	dispBefore := r.sys.Status(r.now).RootDisp
	a.stopAt = r.now
	r.run(10*64, nil) // 8 misses empty the reach register
	if r.sys.State() != StateHoldover {
		t.Fatalf("state %v after losing the only source", r.sys.State())
	}
	st := r.sys.Status(r.now)
	if st.Stratum == 16 || st.RootDisp <= dispBefore {
		t.Fatalf("holdover must keep serving with growing dispersion: %+v", st)
	}
	if math.Abs(r.clk.freq+25) > 1 {
		t.Fatalf("holdover must hold frequency: %v", r.clk.freq)
	}
	r.run(3700, nil)
	if r.sys.State() != StateUnsynced {
		t.Fatalf("state %v after holdover_max", r.sys.State())
	}
	if st := r.sys.Status(r.now); st.Stratum != 16 || st.RefID != ntp.KissHOLD {
		t.Fatalf("%+v", st)
	}
	// The source returns: settle, then synced again, without a step.
	a.stopAt = 0
	r.run(1800, nil)
	if r.sys.State() != StateSynced || r.steps != 0 {
		t.Fatalf("state %v steps %d", r.sys.State(), r.steps)
	}
}

func TestSimSettling(t *testing.T) {
	a := newSimSource("a", 12)
	r := newSim(simConfig(), 0, 0, true, a)
	states := map[State]bool{}
	settlingAfterLastStep := 0
	r.run(1800, func(float64) {
		states[r.sys.State()] = true
		if r.sys.State() == StateSettling {
			settlingAfterLastStep++
		} else if r.sys.State() == StateSynced {
			settlingAfterLastStep = 0
		}
	})
	if !states[StateUnsynced] || !states[StateSettling] || !states[StateSynced] {
		t.Fatalf("states seen: %v", states)
	}
	// SETTLING is counted in measurements for the system source, not in
	// loop updates, so it must not outlast a couple of poll intervals.
	if settlingAfterLastStep > 2*64 {
		t.Fatalf("spent %d s settling; a restarted server answers LI=3 for that long", settlingAfterLastStep)
	}
	st := r.sys.Status(r.now)
	if st.Leap != ntp.LeapNone || st.Stratum != 3 {
		t.Fatalf("%+v", st)
	}
}

func TestSystemUnknownSource(t *testing.T) {
	sys := New(simConfig(), 0, false)
	res := sys.Update(Measurement{Source: "nobody", Now: 1, Valid: true})
	if len(res.Events) != 1 || res.Events[0].Kind != EventUnknownSource {
		t.Fatalf("%+v", res)
	}
}

func TestSystemRemoveSource(t *testing.T) {
	a := newSimSource("a", 13)
	r := newSim(simConfig(), 0, 0, true, a)
	r.run(600, nil)
	res := r.sys.RemoveSource("a", r.now)
	r.apply(res)
	if r.sys.State() != StateHoldover {
		t.Fatalf("state %v", r.sys.State())
	}
	if len(r.sys.Status(r.now).Sources) != 0 {
		t.Fatal("source must be gone")
	}
}

// TestSystemSecondStepNeedsMoreThanOneSample is RF5X-002 item 5, the defence
// in depth behind the generation counter: once the clock has been stepped, a
// second step must not rest on a single post-step sample from a single
// source. A measurement computed before the step and applied after it is
// exactly that, and it used to step the clock again by the same amount in the
// opposite direction.
func TestSystemSecondStepNeedsMoreThanOneSample(t *testing.T) {
	sys := New(simConfig(), 0, true)
	sys.AddSource("a", Options{Numbering: true})
	steps := func(res Result) int {
		n := 0
		for _, a := range res.Actions {
			if a.Kind == ActionStep {
				n++
			}
		}
		return n
	}
	m := func(offset, now float64) Measurement {
		return Measurement{
			Source: "a", Now: now, At: now, Reach: 0xff, Poll: 6, Valid: true,
			Offset: offset, Delay: 0.010, Dispersion: 0.001, Jitter: 50e-6,
			Stratum: 2, Leap: ntp.LeapNone, Precision: -20,
			SourceRefID: ntp.RefIDFromString("a"),
		}
	}

	if n := steps(sys.Update(m(2.0, 64))); n != 1 {
		t.Fatalf("the first step must be immediate; steps=%d", n)
	}
	// The pre-step measurement that arrives next is worth one post-step
	// sample and must not step.
	res := sys.Update(m(-2.0, 128))
	if n := steps(res); n != 0 {
		t.Fatalf("a second step on one post-step sample; steps=%d", n)
	}
	if sys.loop.Updates != 1 || sys.loop.Pending != 0 {
		t.Fatalf("a deferred step must leave the loop untouched: updates=%d pending=%v",
			sys.loop.Updates, sys.loop.Pending)
	}
	// A second post-step sample that still wants a step gets one.
	if n := steps(sys.Update(m(-2.0, 192))); n != 1 {
		t.Fatalf("a genuine second step must still be allowed; steps=%d", n)
	}
}

// TestSystemSecondStepWithTwoAgreeingSurvivors is the other arm of the gate:
// two survivors that have both reported since the step are enough, because
// survivors have already passed the intersection and so agree.
func TestSystemSecondStepWithTwoAgreeingSurvivors(t *testing.T) {
	sys := New(simConfig(), 0, true)
	sys.AddSource("a", Options{Numbering: true})
	sys.AddSource("b", Options{Numbering: true})
	m := func(name string, offset, now float64) Measurement {
		return Measurement{
			Source: name, Now: now, At: now, Reach: 0xff, Poll: 6, Valid: true,
			Offset: offset, Delay: 0.010, Dispersion: 0.001, Jitter: 50e-6,
			Stratum: 2, Leap: ntp.LeapNone, Precision: -20,
			SourceRefID: ntp.RefIDFromString(name),
		}
	}
	steps := 0
	apply := func(res Result) {
		for _, a := range res.Actions {
			if a.Kind == ActionStep {
				steps++
			}
		}
	}
	apply(sys.Update(m("a", 2.0, 64)))
	if steps != 1 {
		t.Fatalf("first step: steps=%d", steps)
	}
	apply(sys.Update(m("a", -2.0, 128))) // one post-step sample: deferred
	apply(sys.Update(m("b", -2.0, 129))) // two survivors agree: allowed
	if steps != 2 {
		t.Fatalf("two agreeing survivors must permit the step; steps=%d", steps)
	}
}

// TestSystemSecondStepResetsSettling covers RF5X-010: setState is a no-op
// when the state is already SETTLING, so a second step inside the startup
// window left the settling evidence standing and declared SYNCED early.
func TestSystemSecondStepResetsSettling(t *testing.T) {
	cfg := simConfig()
	cfg.SettleUpdates = 3
	cfg.Loop.StepLimit = -1 // the point here is the second step, not the budget
	sys := New(cfg, 0, true)
	sys.AddSource("a", Options{Numbering: true})
	m := func(offset, now float64) Measurement {
		return Measurement{
			Source: "a", Now: now, At: now, Reach: 0xff, Poll: 6, Valid: true,
			Offset: offset, Delay: 0.010, Dispersion: 0.001, Jitter: 50e-6,
			Stratum: 2, Leap: ntp.LeapNone, Precision: -20,
			SourceRefID: ntp.RefIDFromString("a"),
		}
	}
	now := 0.0
	feed := func(offset float64) State {
		now += 64
		sys.Update(m(offset, now))
		return sys.State()
	}
	if st := feed(2.0); st != StateSettling { // first step
		t.Fatalf("after the first step: %v", st)
	}
	feed(0.001)
	feed(0.001)
	// Two post-step samples so far; the third would declare SYNCED. Step
	// again instead: the count must start over.
	if st := feed(2.0); st != StateSettling {
		t.Fatalf("after the second step: %v", st)
	}
	if sys.sinceStep != 0 || sys.postStepUpdates != 0 {
		t.Fatalf("settling evidence survived the second step: sinceStep=%d updates=%d",
			sys.sinceStep, sys.postStepUpdates)
	}
	if st := feed(0.001); st != StateSettling {
		t.Fatalf("one post-step sample: %v, want settling", st)
	}
	if st := feed(0.001); st != StateSettling {
		t.Fatalf("two post-step samples: %v, want settling", st)
	}
	if st := feed(0.001); st != StateSynced {
		t.Fatalf("three post-step samples: %v, want synced", st)
	}
}

// TestSystemResyncSkipsSettling covers RF5X-005 at the System level: after a
// leap reset the first fresh measurement restores SYNCED without SETTLING.
func TestSystemResyncSkipsSettling(t *testing.T) {
	sys := New(simConfig(), 0, true)
	sys.AddSource("a", Options{Numbering: true})
	m := func(now float64) Measurement {
		return Measurement{
			Source: "a", Now: now, At: now, Reach: 0xff, Poll: 6, Valid: true,
			Offset: 0.001, Delay: 0.010, Dispersion: 0.001, Jitter: 50e-6,
			Stratum: 2, Leap: ntp.LeapNone, Precision: -20,
			SourceRefID: ntp.RefIDFromString("a"),
		}
	}
	for i := 1; i <= 4; i++ {
		sys.Update(m(float64(i) * 64))
	}
	if sys.State() != StateSynced {
		t.Fatalf("state before the leap: %v", sys.State())
	}
	sys.Resync(320)
	if sys.State() != StateHoldover {
		t.Fatalf("state after Resync: %v, want holdover", sys.State())
	}
	sys.Update(m(384))
	if sys.loop.Updates == 0 {
		t.Fatal("the first post-leap measurement produced no loop update")
	}
	if sys.State() != StateSynced {
		t.Fatalf("state after the first post-leap update: %v, want synced", sys.State())
	}
	if st := sys.Status(384); st.Leap != ntp.LeapNone || st.Stratum != 3 {
		t.Fatalf("post-leap status: %+v", st)
	}
}

// TestSimTolerantOfTickJitter covers RF5X-011's second verification step. The
// engine's ticker can be late, and the kernel keeps running at the phase
// transient for the whole delay. Once Tick charges the real elapsed time, a
// ±200 ms jitter on every tick must not change the steady-state accuracy.
func TestSimTolerantOfTickJitter(t *testing.T) {
	steady := func(jitter float64) float64 {
		r := newSim(simConfig(), -50, 0.100, false, newSimSource("a", 1))
		r.tickJitter = jitter
		r.tickRNG = rand.New(rand.NewSource(99))
		r.run(5*3600, nil)
		if r.sys.State() != StateSynced {
			t.Fatalf("jitter %v: state %v", jitter, r.sys.State())
		}
		return r.rmsOver(3600)
	}
	even := steady(0)
	rough := steady(0.200)
	t.Logf("steady-state RMS: even ticker %.1f µs, ±200 ms jitter %.1f µs", even*1e6, rough*1e6)
	if rough > 300e-6 {
		t.Fatalf("RMS with a jittery ticker %v", rough)
	}
	if rough > 2*even+50e-6 {
		t.Fatalf("tick jitter degraded the RMS from %v to %v", even, rough)
	}
}

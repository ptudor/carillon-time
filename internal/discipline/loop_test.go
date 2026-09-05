package discipline

import (
	"math"
	"testing"
)

func loopCfg() LoopConfig {
	return LoopConfig{
		StepThreshold: 0.5, StepLimit: 3, Panic: 1000, MaxSlewPPM: 500,
		Precision: 1e-6, FreqMeasure: 900,
	}
}

func hasAction(acts []Action, k ActionKind) (Action, bool) {
	for _, a := range acts {
		if a.Kind == k {
			return a, true
		}
	}
	return Action{}, false
}

func TestLoopFirstUpdate(t *testing.T) {
	l := NewLoop(loopCfg(), 12.5, true)
	u := l.Update(0.010, 6, 1000, false, true)
	if u.Stepped || u.Ignored || u.PanicRefused {
		t.Fatalf("%+v", u)
	}
	if l.Pending != 0.010 || l.Updates != 1 {
		t.Fatalf("pending %v updates %d", l.Pending, l.Updates)
	}
	// An update issues no frequency action of its own: writing the base
	// alone would remove the phase transient the last Tick applied and
	// pause the slew until the next one. The Tick that follows carries the
	// new base plus the transient for the new pending phase.
	if a, ok := hasAction(u.Actions, ActionSetFrequency); ok {
		t.Fatalf("update issued a frequency action of %v ppm", a.Value)
	}
	if a, ok := hasAction(l.Tick(1001), ActionSetFrequency); !ok || a.Value <= 12.5 {
		t.Fatalf("the next tick must carry the base plus the transient: %v", a.Value)
	}
	// RF5X-025: the first update's jitter is the offset averaged in from
	// the precision floor, not the whole offset.
	if want := 0.010 / math.Sqrt(jitterAverage); math.Abs(l.Jitter-want) > 1e-12 {
		t.Fatalf("first-update jitter %v, want %v", l.Jitter, want)
	}
}

// TestLoopJitterSeedAndStep covers RF5X-025: a 100 ms initial offset must not
// be reported as 100 ms of clock jitter, and the update after a step must not
// treat the zeroed lastOffset as a real change.
func TestLoopJitterSeedAndStep(t *testing.T) {
	l := NewLoop(loopCfg(), 0, true)
	l.Update(0.100, 6, 0, false, true)
	if want := 0.100 / math.Sqrt(jitterAverage); math.Abs(l.Jitter-want) > 1e-12 {
		t.Fatalf("first-update jitter %v, want about %v (35 ms)", l.Jitter, want)
	}

	l = NewLoop(loopCfg(), 0, true)
	l.Update(0.001, 6, 0, false, true)
	l.Update(0.001, 6, 64, false, true)
	before := l.Jitter
	if u := l.Update(2.0, 6, 128, false, true); !u.Stepped {
		t.Fatal("expected a step")
	}
	if l.Jitter != before {
		t.Fatalf("a step changed the jitter estimate: %v -> %v", before, l.Jitter)
	}
	l.Update(0.002, 6, 192, false, true)
	if l.Jitter != before {
		t.Fatalf("the update after a step must not fold in the zeroed lastOffset: %v -> %v", before, l.Jitter)
	}
	l.Update(0.003, 6, 256, false, true)
	if l.Jitter == before {
		t.Fatal("ordinary updates must still move the jitter estimate")
	}
}

// TestLoopTickChargesRealElapsedTime covers RF5X-011 and RA6X-008: the kernel
// runs at whatever transient is actually in it, for however long the engine's
// ticker actually took. A late tick must debit that word for that interval —
// not a nominal second, and not the word it is about to issue.
func TestLoopTickChargesRealElapsedTime(t *testing.T) {
	l := NewLoop(loopCfg(), 0, true)
	l.Update(0.010, 6, 0, false, true) // tau = 256

	// The first tick issues a transient but charges nothing: until it does,
	// the kernel is holding the base and no transient has run (RA6X-008).
	l.Tick(1)
	if l.Pending != 0.010 {
		t.Fatalf("the first tick debited a transient that had not run yet: %v", l.Pending)
	}
	applied := l.applied - l.appliedBase

	pending := l.Pending
	l.Tick(1 + maxTickInterval) // late, but inside the accounting window
	if want := pending - maxTickInterval*applied*1e-6; math.Abs(l.Pending-want) > 1e-15 {
		t.Fatalf("pending %v after a %.0f s tick, want %v", l.Pending, maxTickInterval, want)
	}

	// A stall longer than the accounting window is charged at the window,
	// not reduced to a nominal second: the word really did stay applied.
	pending = l.Pending
	applied = l.applied - l.appliedBase
	l.Tick(1 + maxTickInterval + 10)
	if want := pending - maxTickInterval*applied*1e-6; math.Abs(l.Pending-want) > 1e-15 {
		t.Fatalf("pending %v after a stall, want %v", l.Pending, want)
	}

	// Time that has not moved debits nothing.
	pending = l.Pending
	l.Tick(1 + maxTickInterval + 10)
	if l.Pending != pending {
		t.Fatalf("a zero-length tick debited the phase: %v -> %v", pending, l.Pending)
	}
}

func TestLoopStepPolicy(t *testing.T) {
	l := NewLoop(loopCfg(), 0, true)
	u := l.Update(1.0, 6, 0, false, true)
	if !u.Stepped || l.Steps != 1 {
		t.Fatalf("expected step: %+v", u)
	}
	if a, ok := hasAction(u.Actions, ActionStep); !ok || a.Value != 1.0 {
		t.Fatalf("step action: %+v", u.Actions)
	}
	if _, ok := hasAction(u.Actions, ActionResetFilters); !ok {
		t.Fatal("step must reset filters")
	}
	// Two more updates use up the limit of 3.
	l.Update(0.001, 6, 64, false, true)
	l.Update(0.001, 6, 128, false, true)
	// Two poll intervals after the previous update the popcorn gate no
	// longer applies, so the offset is accepted — and slewed, not stepped.
	u = l.Update(1.0, 6, 128+128, true, true)
	if u.Stepped {
		t.Fatal("no step allowed after the startup window")
	}
	if u.Ignored {
		t.Fatal("a large offset outside the popcorn window must be slewed, not ignored")
	}
	if l.Pending != 1.0 {
		t.Fatalf("pending %v", l.Pending)
	}

	never := loopCfg()
	never.StepLimit = 0
	l = NewLoop(never, 0, true)
	if u := l.Update(5, 6, 0, false, true); u.Stepped {
		t.Fatal("limit 0 must never step")
	}
	always := loopCfg()
	always.StepLimit = -1
	l = NewLoop(always, 0, true)
	for i := 0; i < 5; i++ {
		l.Update(0.001, 6, float64(i)*64, false, true)
	}
	if u := l.Update(5, 6, 1000, true, true); !u.Stepped {
		t.Fatal("limit -1 must always step")
	}
}

func TestLoopPanic(t *testing.T) {
	l := NewLoop(loopCfg(), 0, true)
	u := l.Update(2000, 6, 0, false, true)
	if !u.PanicRefused || len(u.Actions) != 0 || l.Updates != 0 {
		t.Fatalf("panic must refuse: %+v", u)
	}
	cfg := loopCfg()
	cfg.PanicAtStartup = true
	l = NewLoop(cfg, 0, true)
	if u := l.Update(2000, 6, 0, false, true); !u.Stepped {
		t.Fatalf("panic_at_startup must step: %+v", u)
	}
	if u := l.Update(2000, 6, 64, false, true); !u.PanicRefused {
		t.Fatal("only the first correction may exceed the panic threshold")
	}
	// Panic with stepping disabled is still refused rather than slewed.
	cfg = loopCfg()
	cfg.StepLimit = 0
	l = NewLoop(cfg, 0, true)
	if u := l.Update(2000, 6, 0, false, true); !u.PanicRefused {
		t.Fatal("panic with limit 0 must refuse")
	}
}

func TestLoopTickSlew(t *testing.T) {
	l := NewLoop(loopCfg(), 10, true)
	l.Update(0.010, 6, 0, false, true) // tau = 4·64 = 256
	acts := l.Tick(1)
	a, ok := hasAction(acts, ActionSetFrequency)
	if !ok {
		t.Fatal("tick must set frequency")
	}
	wantAdj := 0.010 / 256
	if math.Abs(a.Value-(10+wantAdj*1e6)) > 1e-9 {
		t.Fatalf("transient: got %v want %v", a.Value, 10+wantAdj*1e6)
	}
	// The debit lands on the tick after the word has run for a second.
	l.Tick(2)
	if math.Abs(l.Pending-(0.010-wantAdj)) > 1e-15 {
		t.Fatalf("pending %v", l.Pending)
	}
	// Exponential approach: after 256 ticks about 1/e remains.
	for i := 3; i <= 257; i++ {
		l.Tick(float64(i))
	}
	if r := l.Pending / 0.010; r < 0.35 || r > 0.38 {
		t.Fatalf("after one time constant %v remains", r)
	}
}

func TestLoopTickClamps(t *testing.T) {
	cfg := loopCfg()
	cfg.StepLimit = 0 // slew everything, so a 10 s offset becomes pending phase
	l := NewLoop(cfg, 400, true)
	l.Update(10, 6, 0, false, true) // pending 10 s, slew wants 9.7 ms/s
	acts := l.Tick(1)
	a, _ := hasAction(acts, ActionSetFrequency)
	if a.Value != 500 {
		t.Fatalf("total must clamp at 500 ppm, got %v", a.Value)
	}
	// Only the 100 ppm the kernel clamp let through was slewed, and it is
	// charged on the following tick, once it has actually run for a second.
	l.Tick(2)
	if math.Abs(l.Pending-(10-100e-6)) > 1e-12 {
		t.Fatalf("pending %v", l.Pending)
	}
	l = NewLoop(cfg, 0, true)
	l.Update(10, 6, 0, false, true)
	acts = l.Tick(1)
	a, _ = hasAction(acts, ActionSetFrequency)
	if a.Value != 500 {
		t.Fatalf("max_slew_ppm bound: got %v", a.Value)
	}
}

func TestLoopTickIdle(t *testing.T) {
	l := NewLoop(loopCfg(), 5, true)
	if acts := l.Tick(0); len(acts) != 1 || acts[0].Value != 5 {
		t.Fatalf("first tick must issue the base frequency: %+v", acts)
	}
	if acts := l.Tick(1); len(acts) != 0 {
		t.Fatalf("idle tick must be silent: %+v", acts)
	}
}

func TestLoopFrequencyClamp(t *testing.T) {
	l := NewLoop(loopCfg(), 900, true)
	if l.Freq != 500 {
		t.Fatalf("initial clamp: %v", l.Freq)
	}
	l = NewLoop(loopCfg(), 0, true)
	l.Update(0, 2, 0, false, true)
	for i := 1; i < 2000; i++ {
		l.Update(0.007, 2, float64(i)*4, false, true) // just inside the linear region at poll 2 (τ = 16 s)
	}
	if l.Freq != 500 {
		t.Fatalf("a persistent positive offset must drive the frequency to the +500 ppm clamp: %v", l.Freq)
	}
}

func TestLoopAntiWindup(t *testing.T) {
	cfg := loopCfg()
	cfg.StepLimit = 0
	l := NewLoop(cfg, 10, true)
	l.Update(0, 6, 0, false, true)
	// A 1 s offset at τ = 256 s cannot be slewed within the 500 ppm bound
	// (needs 2000 s); the frequency must not integrate meanwhile.
	for i := 1; i <= 40; i++ {
		l.Update(1.0-float64(i)*0.02, 6, float64(i)*64, false, true)
	}
	if l.Freq != 10 {
		t.Fatalf("frequency wound up to %v during a saturated slew", l.Freq)
	}
}

func TestLoopPopcorn(t *testing.T) {
	l := NewLoop(loopCfg(), 0, true)
	now := 0.0
	for i := 0; i < 10; i++ {
		l.Update(1e-5*float64(i%2), 6, now, i > 2, true)
		now += 64
	}
	jit := l.Jitter
	u := l.Update(0.050, 6, now, true, true) // a spike 64 s after the previous update
	if !u.Ignored {
		t.Fatalf("spike must be ignored (jitter %v): %+v", jit, u)
	}
	if l.Jitter != jit {
		t.Fatal("an ignored spike must not inflate the jitter")
	}
	// The same offset arriving after two poll intervals is accepted.
	u = l.Update(0.050, 6, now+128, true, true)
	if u.Ignored {
		t.Fatal("a persistent offset must be accepted after two poll intervals")
	}
}

func TestLoopBootstrapFrequency(t *testing.T) {
	// With no known frequency, the loop measures it directly over
	// FreqMeasure seconds: an offset growing 50 µs/s means +50 ppm.
	l := NewLoop(loopCfg(), 0, false)
	l.Update(0, 6, 0, false, true)
	for i := 1; i <= 15; i++ {
		now := float64(i) * 64
		// Simulate: offset grows at 50 ppm, minus what the loop slewed.
		for k := 0; k < 64; k++ {
			l.Tick(now - 64 + float64(k))
		}
		offset := 50e-6*now - l.slewed
		l.Update(offset, 6, now, false, true)
	}
	if !l.FreqKnown {
		t.Fatal("frequency must be known after the measurement window")
	}
	if math.Abs(l.Freq-50) > 0.5 {
		t.Fatalf("freq %v want ≈50", l.Freq)
	}
}

// TestLoopConvergesWhateverTheUpdateSpacing pins down something that looks
// alarming and is not. A loop update happens only when the clock filter
// yields a new lowest-delay sample, which is uncorrelated with the poll
// interval that sets tau, so `mu` — the interval the frequency integration
// uses — routinely runs to one or two times tau on a quiet path. That makes
// individual integration steps large (`theta·mu/(4·tau²)`), and after a
// restart the frequency word can visibly walk tens of ppm.
//
// It converges anyway, and the peak excursion does not grow with mu: the
// phase slew removes most of the offset between sparse updates, so the next
// update integrates a smaller theta. Checked here from 0.06·tau to 2·tau
// against a 15.4 ppm drift and a 10 ms initial offset, which is the shape of
// a real restart.
func TestLoopConvergesWhateverTheUpdateSpacing(t *testing.T) {
	const drift, offset0 = -15.4, 0.010
	tau := 4 * math.Ldexp(1, 6) // poll 6
	for _, spacing := range []float64{16, 64, 128, 256, 448, 512} {
		cfg := loopCfg()
		cfg.StepLimit = 0 // slew everything; the integrator is what is under test
		l := NewLoop(cfg, -drift, true)
		clk := &simClock{drift: drift, local: -offset0}
		apply := func(acts []Action) {
			for _, a := range acts {
				if a.Kind == ActionSetFrequency {
					clk.freq = a.Value
				}
			}
		}
		peak, next := 0.0, 0.0
		for s := 0; s < 7200; s++ {
			now := float64(s)
			if now >= next {
				apply(l.Update(clk.offset(), 6, now, true, true).Actions)
				next = now + spacing
			}
			apply(l.Tick(now))
			if d := math.Abs(l.Freq - -drift); d > peak {
				peak = d
			}
			clk.advance(1)
		}
		t.Logf("mu = %5.0f s (%.2f tau): peak |freq-true| %5.2f ppm, final offset %+.3f ms, freq %+.2f ppm",
			spacing, spacing/tau, peak, clk.offset()*1e3, l.Freq)
		if math.Abs(l.Freq - -drift) > 0.5 {
			t.Errorf("mu = %.0f s: converged to %v ppm, want about %v", spacing, l.Freq, -drift)
		}
		if math.Abs(clk.offset()) > 1e-4 {
			t.Errorf("mu = %.0f s: final offset %v", spacing, clk.offset())
		}
		if peak > 12 {
			t.Errorf("mu = %.0f s: peak excursion %v ppm", spacing, peak)
		}
	}
}

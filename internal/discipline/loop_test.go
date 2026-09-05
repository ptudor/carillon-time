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
	if a, ok := hasAction(u.Actions, ActionSetFrequency); !ok || a.Value != 12.5 {
		t.Fatalf("frequency must be re-issued unchanged on the first update: %+v", u.Actions)
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
	if math.Abs(l.Pending-(0.010-wantAdj)) > 1e-15 {
		t.Fatalf("pending %v", l.Pending)
	}
	// Exponential approach: after 256 ticks about 1/e remains.
	for i := 2; i <= 256; i++ {
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
	// Only the 100 ppm the kernel clamp let through was slewed.
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

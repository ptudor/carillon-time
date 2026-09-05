package engine

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"carillon/internal/clock"
	"carillon/internal/discipline"
)

func newBoundsEngine(t *testing.T) (*clock.Fake, *Engine) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
	e, err := New(testConfig(""), clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	return clk, e
}

func actions(a ...discipline.Action) discipline.Result {
	return discipline.Result{Actions: a}
}

func actionStep(v float64) discipline.Action {
	return discipline.Action{Kind: discipline.ActionStep, Value: v}
}

func actionSetFrequency(v float64) discipline.Action {
	return discipline.Action{Kind: discipline.ActionSetFrequency, Value: v}
}

// TestAstra6StepRejectsUnrepresentableDuration is the review's RA6X-041
// probe. A finite offset need not fit in a time.Duration, and the plain
// seconds-to-duration cast wraps: +1e20 s used to be recorded as a step of
// about +292 years.
func TestAstra6StepRejectsUnrepresentableDuration(t *testing.T) {
	clk, e := newBoundsEngine(t)
	err := e.handle(actions(actionStep(1e20)), 1)
	if err == nil || len(clk.Steps) != 0 {
		t.Fatalf("unrepresentable positive step reached actuator: err=%v steps=%v", err, clk.Steps)
	}
}

// TestAstra6StepConversionBounds covers RA6X-041's verification list: both
// signs, NaN and both infinities, the representable limit and its
// neighbouring floats, and the ordinary corrections that must still work.
func TestAstra6StepConversionBounds(t *testing.T) {
	// The largest magnitude Seconds accepts, and the next float above it.
	const limit = 9.223372036e9
	cases := []struct {
		name  string
		value float64
		ok    bool
	}{
		{"nan", math.NaN(), false},
		{"+inf", math.Inf(1), false},
		{"-inf", math.Inf(-1), false},
		{"+1e20", 1e20, false},
		{"-1e20", -1e20, false},
		{"+past limit", math.Nextafter(limit, math.Inf(1)), false},
		{"-past limit", math.Nextafter(-limit, math.Inf(-1)), false},
		{"+limit", limit, true},
		{"-limit", -limit, true},
		{"+under limit", math.Nextafter(limit, 0), true},
		{"zero", 0, true},
		// A host whose RTC is stuck at the epoch needs a correction of
		// about 56 years; that must remain possible.
		{"rtc at 1970", 1.77e9, true},
		{"rtc ahead by 56 years", -1.77e9, true},
		{"ordinary forward", 1.25, true},
		{"ordinary backward", -1.25, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clk, e := newBoundsEngine(t)
			err := e.handle(actions(actionStep(c.value)), 1)
			switch {
			case c.ok && err != nil:
				t.Fatalf("step of %v refused: %v", c.value, err)
			case !c.ok && err == nil:
				t.Fatalf("step of %v accepted", c.value)
			}
			if c.ok {
				if len(clk.Steps) != 1 {
					t.Fatalf("step of %v produced %d actuator calls", c.value, len(clk.Steps))
				}
				if want := time.Duration(c.value * float64(time.Second)); clk.Steps[0] != want {
					t.Fatalf("step of %v applied %v, want %v", c.value, clk.Steps[0], want)
				}
				return
			}
			if len(clk.Steps) != 0 {
				t.Fatalf("refused step of %v still called the actuator: %v", c.value, clk.Steps)
			}
		})
	}
}

// TestAstra6RefusedStepDoesNotBumpGeneration checks that refusing an
// impossible step leaves the epoch alone: bumping it would invalidate every
// sample in flight for a discontinuity that never happened.
func TestAstra6RefusedStepDoesNotBumpGeneration(t *testing.T) {
	_, e := newBoundsEngine(t)
	before := e.gen.Load()
	if err := e.handle(actions(actionStep(math.Inf(1))), 1); err == nil {
		t.Fatal("infinite step accepted")
	}
	if got := e.gen.Load(); got != before {
		t.Fatalf("generation moved %d -> %d for a refused step", before, got)
	}
}

// TestAstra6NonFiniteUncertaintyStaysRepresentable checks the error-bound
// half of RA6X-041. Unlike a phase correction, an uncertainty that cannot be
// represented must not stop discipline: it saturates at the 16 s ceiling both
// kernels apply, which is the honest reading of an unknown bound.
func TestAstra6NonFiniteUncertaintyStaysRepresentable(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), 1e30, -1} {
		got := clock.BoundSeconds(v, maxKernelError)
		if got < 0 || got > maxKernelError {
			t.Fatalf("BoundSeconds(%v) = %v, outside [0, %v]", v, got, maxKernelError)
		}
	}
	if got := clock.BoundSeconds(0.25, maxKernelError); got != 250*time.Millisecond {
		t.Fatalf("BoundSeconds(0.25) = %v, want 250ms", got)
	}
}

// TestAstra6RestoresBaseBeforeFirstTick covers RA6X-016. The startup
// frequency write bypassed the loop's applied-state bookkeeping, so if
// several measurements moved the base before the first Tick and the daemon
// then stopped, Applied() reported that nothing had been written and
// restoration returned early — leaving the kernel at the startup value while
// status reported the final base.
func TestAstra6RestoresBaseBeforeFirstTick(t *testing.T) {
	dir := t.TempDir()
	drift := filepath.Join(dir, "drift")
	if err := os.WriteFile(drift, []byte("12.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
	src := &scripted{
		name: "a", clk: clk,
		script: []discipline.Measurement{good(0.002), good(0.004), good(0.006), good(0.008)},
	}
	cfg := testConfig(drift, SourceSpec{Source: src, Options: discipline.Options{Numbering: true}})
	e, err := New(cfg, clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately delay the ticker past the whole run, so every base change
	// happens before the first Tick.
	e.tick = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	waitFor(t, func() bool { return int(src.sent.Load()) == len(src.script) })
	// Let the last measurement be consumed before stopping.
	waitFor(t, func() bool { return e.Status().Updates >= len(src.script) })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	base := e.Status().Frequency
	if len(clk.Frequencies) == 0 {
		t.Fatal("no frequency was ever written")
	}
	last := clk.Frequencies[len(clk.Frequencies)-1]
	if last != base {
		t.Fatalf("kernel left at %v ppm, but the reported base is %v ppm", last, base)
	}
}

// TestAstra6RestoreEdgeCases covers the rest of RA6X-016's verification list.
func TestAstra6RestoreEdgeCases(t *testing.T) {
	t.Run("no updates leaves the startup word", func(t *testing.T) {
		dir := t.TempDir()
		drift := filepath.Join(dir, "drift")
		if err := os.WriteFile(drift, []byte("12.5\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		clk := clock.NewFake(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
		e, err := New(testConfig(drift), clk, quietLog())
		if err != nil {
			t.Fatal(err)
		}
		e.tick = time.Hour
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- e.Run(ctx) }()
		// Fake.Status is mutex-protected, unlike the recorded slices; the
		// engine writes the kernel status straight after the startup
		// frequency, so this proves that write has happened.
		waitFor(t, func() bool { return clk.Status().MaxError != 0 })
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if len(clk.Frequencies) != 1 || clk.Frequencies[0] != 12.5 {
			t.Fatalf("frequency writes %v, want exactly the startup 12.5 ppm", clk.Frequencies)
		}
	})

	t.Run("a refused startup write is not retried", func(t *testing.T) {
		clk := clock.NewFake(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
		refusing := &astraRefusingClock{Fake: clk}
		e, err := New(testConfig(""), refusing, quietLog())
		if err != nil {
			t.Fatal(err)
		}
		e.tick = time.Hour
		if err := e.Run(context.Background()); err == nil {
			t.Fatal("a refused startup frequency must be fatal")
		}
		if refusing.calls != 1 {
			t.Fatalf("SetFrequency called %d times, want 1", refusing.calls)
		}
	})
}

// astraRefusingClock refuses every frequency change, so a test can check the
// engine does not retry one on the way out.
type astraRefusingClock struct {
	*clock.Fake
	calls int
}

func (c *astraRefusingClock) SetFrequency(float64) error {
	c.calls++
	return errors.New("EPERM")
}

func waitFor(t *testing.T, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !pred() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached within 5 s")
		}
		time.Sleep(time.Millisecond)
	}
}

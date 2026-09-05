package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"carillon/internal/clock"
	"carillon/internal/discipline"
	"carillon/internal/leap"
	"carillon/internal/ntp"
	"carillon/internal/source"
)

// steppingClock runs a callback from inside Step, so a test can observe what
// a source starting a sample mid-discontinuity would see.
type steppingClock struct {
	*clock.Fake
	during func()
	fail   error
}

func (c *steppingClock) Step(d time.Duration) error {
	if c.during != nil {
		c.during()
	}
	if c.fail != nil {
		return c.fail
	}
	return c.Fake.Step(d)
}

// TestAstra6GenerationCoversTheStep covers RA6X-006. The engine used to
// increment the epoch before the syscall and leave it there, so a source that
// started a sample after the increment but before the clock actually moved
// labelled a pre-step observation with the post-step epoch — exactly the case
// the stamp exists to catch.
func TestAstra6GenerationCoversTheStep(t *testing.T) {
	gen := new(atomic.Uint64)
	fake := clock.NewFake(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
	var duringStep uint64
	clk := &steppingClock{Fake: fake, during: func() { duringStep = gen.Load() }}
	cfg := testConfig("")
	cfg.Generation = gen
	e, err := New(cfg, clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	e.sys.AddSource("a", discipline.Options{Numbering: true})
	before := gen.Load()

	if err := e.handle(actions(actionStep(2)), 1); err != nil {
		t.Fatal(err)
	}
	if source.StableEpoch(duringStep) {
		t.Fatalf("epoch %d was settled while the clock was being stepped", duringStep)
	}
	after := gen.Load()
	if !source.StableEpoch(after) || after <= before {
		t.Fatalf("epoch %d -> %d, want a later settled epoch", before, after)
	}
	// A sample stamped with the epoch a source read during the step is not
	// admitted afterwards.
	if !e.stale(discipline.Measurement{Source: "a", Generation: duringStep}) {
		t.Fatalf("a sample taken during the step (epoch %d) was admitted at epoch %d", duringStep, after)
	}
	if !e.stale(discipline.Measurement{Source: "a", Generation: before}) {
		t.Fatal("a pre-step sample was admitted")
	}
	if e.stale(discipline.Measurement{Source: "a", Generation: after}) {
		t.Fatal("a post-step sample was rejected")
	}
}

// TestAstra6StepFailureCompletesTheEpoch checks the failure path: a refused
// step must not leave acquisition permanently in progress.
func TestAstra6StepFailureCompletesTheEpoch(t *testing.T) {
	gen := new(atomic.Uint64)
	fake := clock.NewFake(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
	clk := &steppingClock{Fake: fake, fail: errors.New("EPERM")}
	cfg := testConfig("")
	cfg.Generation = gen
	e, err := New(cfg, clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.handle(actions(actionStep(2)), 1); err == nil {
		t.Fatal("a refused step must be fatal")
	}
	if got := gen.Load(); !source.StableEpoch(got) {
		t.Fatalf("epoch %d is still in progress after a failed step", got)
	}
}

// TestAstra6LeapCheckedBeforeQueuedMeasurement covers RA6X-007. Run evaluated
// sys.Update(m) before handle looked at the leap boundary, and handle applied
// the resulting actions before that check, so a queued observation spanning
// the transition could step the clock a whole second before the sources were
// reset — and resetting afterwards cannot undo an actuator call.
func TestAstra6LeapCheckedBeforeQueuedMeasurement(t *testing.T) {
	for _, dir := range []struct {
		name string
		leap ntp.Leap
	}{{"insert", ntp.LeapInsert}, {"delete", ntp.LeapDelete}} {
		t.Run(dir.name, func(t *testing.T) {
			transition := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
			clk := clock.NewFake(transition.Add(-2 * time.Second))
			gen := new(atomic.Uint64)
			src := &scripted{name: "gps", clk: clk, gen: gen.Load}
			cfg := testConfig("", SourceSpec{Source: src, Options: discipline.Options{Numbering: true}})
			cfg.Generation = gen
			cfg.LeapTable = &leap.Table{
				Expiry:      time.Date(2026, 12, 28, 0, 0, 0, 0, time.UTC),
				Transitions: []leap.Transition{{At: transition, Leap: dir.leap}},
			}
			e, err := New(cfg, clk, quietLog())
			if err != nil {
				t.Fatal(err)
			}
			// Synchronize before the boundary.
			for i := 0; i < 3; i++ {
				clk.Advance(100 * time.Millisecond)
				m := good(0.001)
				m.Source, m.Now, m.At = "gps", clk.Monotonic(), clk.Monotonic()
				m.Generation = gen.Load()
				if err := e.handle(e.sys.Update(m), m.Now); err != nil {
					t.Fatal(err)
				}
			}
			steps := len(clk.Steps)
			freqs := len(clk.Frequencies)

			// A source computes an observation just before the boundary and
			// stamps the epoch it was taken in.
			queued := good(1.0)
			queued.Source = "gps"
			queued.Generation = gen.Load()

			// The engine reaches it only after the kernel has applied the
			// leap. Drive the real Run loop so the ordering is exercised
			// where it matters: the boundary must be processed before the
			// queued observation is admitted, because that observation is
			// wrong by exactly one second and resetting the sources
			// afterwards cannot undo a step.
			e.tick = time.Hour // the measurement arm, not the ticker
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- e.Run(ctx) }()
			clk.Advance(transition.Sub(clk.TrueTime()) + time.Second)
			e.meas <- queued
			waitFor(t, func() bool { return src.resets.Load() >= 1 })
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}

			if len(clk.Steps) != steps {
				t.Fatalf("a boundary-spanning observation stepped the clock: %v", clk.Steps[steps:])
			}
			if len(clk.Frequencies) != freqs+1 {
				// Exactly the shutdown base-frequency restore, nothing from
				// the boundary-spanning observation.
				t.Fatalf("frequency writes %v, want only the restore on exit", clk.Frequencies[freqs:])
			}
			if src.resets.Load() != 1 {
				t.Fatalf("sources reset %d times, want 1", src.resets.Load())
			}
			if e.staleDrops["gps"] != 1 {
				t.Fatalf("boundary-spanning observations dropped: %d, want 1", e.staleDrops["gps"])
			}
		})
	}
}

// TestAstra6LeapResetBracketsItsEpoch checks the reset after a leap uses the
// same in-progress epoch a step does, and settles again afterwards.
func TestAstra6LeapResetBracketsItsEpoch(t *testing.T) {
	transition := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(transition.Add(-time.Second))
	gen := new(atomic.Uint64)
	src := &scripted{name: "gps", clk: clk}
	cfg := testConfig("", SourceSpec{Source: src, Options: discipline.Options{Numbering: true}})
	cfg.Generation = gen
	cfg.LeapTable = &leap.Table{
		Expiry:      time.Date(2026, 12, 28, 0, 0, 0, 0, time.UTC),
		Transitions: []leap.Transition{{At: transition, Leap: ntp.LeapInsert}},
	}
	e, err := New(cfg, clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	// Establish lastLeapWall before the boundary.
	e.crossLeap(e.clk.Monotonic())
	before := gen.Load()
	clk.Advance(2 * time.Second)
	e.crossLeap(e.clk.Monotonic())
	after := gen.Load()
	if after != before+2 {
		t.Fatalf("epoch %d -> %d, want a bracketed change", before, after)
	}
	if !source.StableEpoch(after) {
		t.Fatalf("epoch %d is still in progress after the leap reset", after)
	}
	if src.resets.Load() != 1 {
		t.Fatalf("sources reset %d times, want 1", src.resets.Load())
	}
	// Crossing is once-only.
	e.crossLeap(e.clk.Monotonic())
	if gen.Load() != after {
		t.Fatalf("the boundary was processed twice: epoch %d", gen.Load())
	}
}

// TestAstra6LeapOnTheSameIterationAsATick covers the review's "ready on the
// same select iteration as the ticker" case: whichever arm runs, the boundary
// is processed before any correction.
func TestAstra6LeapOnTheSameIterationAsATick(t *testing.T) {
	transition := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(transition.Add(-time.Second))
	src := &scripted{name: "gps", clk: clk}
	cfg := testConfig("", SourceSpec{Source: src, Options: discipline.Options{Numbering: true}})
	cfg.LeapTable = &leap.Table{
		Expiry:      time.Date(2026, 12, 28, 0, 0, 0, 0, time.UTC),
		Transitions: []leap.Transition{{At: transition, Leap: ntp.LeapInsert}},
	}
	e, err := New(cfg, clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		clk.Advance(100 * time.Millisecond)
		m := good(0.001)
		m.Source, m.Now, m.At = "gps", clk.Monotonic(), clk.Monotonic()
		if err := e.handle(e.sys.Update(m), m.Now); err != nil {
			t.Fatal(err)
		}
	}
	steps := len(clk.Steps)
	clk.Advance(2 * time.Second)
	now := clk.Monotonic()
	// The tick arm.
	e.crossLeap(now)
	if err := e.handle(e.sys.Tick(now), now); err != nil {
		t.Fatal(err)
	}
	if len(clk.Steps) != steps {
		t.Fatalf("the leap boundary produced a step: %v", clk.Steps[steps:])
	}
	if src.resets.Load() != 1 {
		t.Fatalf("sources reset %d times, want 1", src.resets.Load())
	}
}

// TestAstra6RunProcessesLeapBeforeMeasurements drives the real Run loop, so
// the ordering is checked where it actually matters rather than only through
// direct handle calls.
func TestAstra6RunProcessesLeapBeforeMeasurements(t *testing.T) {
	transition := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(transition.Add(-time.Second))
	src := &scripted{
		name: "gps", clk: clk, gap: time.Millisecond,
		script: []discipline.Measurement{good(0.001), good(0.001), good(0.001), good(0.001)},
	}
	cfg := testConfig("", SourceSpec{Source: src, Options: discipline.Options{Numbering: true}})
	cfg.LeapTable = &leap.Table{
		Expiry:      time.Date(2026, 12, 28, 0, 0, 0, 0, time.UTC),
		Transitions: []leap.Transition{{At: transition, Leap: ntp.LeapInsert}},
	}
	e, err := New(cfg, clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	e.tick = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	// The scripted source advances the fake clock 64 s per measurement, so
	// the boundary is crossed during the run.
	waitFor(t, func() bool { return src.resets.Load() >= 1 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(clk.Steps) != 0 {
		t.Fatalf("crossing a leap boundary stepped the clock: %v", clk.Steps)
	}
}

// TestAstra6QueuedMeasurementCannotReviveStoppedSource covers RA6X-017.
// Measurements and lifecycle events travel on different channels with no
// ordering between them, so after a stop event marked a source unreachable an
// older valid sample still sitting in the queue could revive it and make it
// the system source again.
func TestAstra6QueuedMeasurementCannotReviveStoppedSource(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
	e, err := New(testConfig(""), clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	e.sources["a"] = SourceSpec{Source: &scripted{name: "a", clk: clk}, Options: discipline.Options{Numbering: true}}
	e.order = append(e.order, "a")
	e.sys.AddSource("a", discipline.Options{Numbering: true})

	// The source produces a good sample, which is still in the queue when
	// its goroutine exits.
	good := good(0.001)
	good.Source, good.Now, good.At = "a", 1, 1
	e.meas <- good

	if err := e.sourceStopped("a", errors.New("port vanished"), time.Second); err != nil {
		t.Fatal(err)
	}
	if len(e.meas) != 0 {
		t.Fatalf("%d measurements from the stopped source survived in the queue", len(e.meas))
	}

	// Even if one had survived, the stop must have revoked the estimate.
	st := e.Status()
	if st.SystemSource == "a" {
		t.Fatalf("a stopped source is still the system source: %+v", st.Status)
	}
	for _, src := range st.Sources {
		if src.Name == "a" && src.Status == discipline.StatusSystem {
			t.Fatalf("stopped source still selected: %+v", src)
		}
	}
}

// refusingStatusClock starts working and then refuses every kernel status
// update, which is the actuator call handle makes on every path.
type refusingStatusClock struct {
	*clock.Fake
	fail atomic.Bool
}

func (c *refusingStatusClock) SetStatus(s clock.Status) error {
	if c.fail.Load() {
		return errors.New("EPERM")
	}
	return c.Fake.SetStatus(s)
}

// TestAstra6SourceStopPropagatesFatalErrors covers the second half of
// RA6X-017: sourceStopped logged and discarded the result of handle, so an
// actuator failure raised on the source-stop path was not fatal, unlike the
// same failure raised from a measurement or a tick.
func TestAstra6SourceStopPropagatesFatalErrors(t *testing.T) {
	fake := clock.NewFake(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
	clk := &refusingStatusClock{Fake: fake}
	e, err := New(testConfig(""), clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	e.sources["a"] = SourceSpec{Source: &scripted{name: "a", clk: fake}, Options: discipline.Options{Numbering: true}}
	e.order = append(e.order, "a")
	e.sys.AddSource("a", discipline.Options{Numbering: true})
	m := good(0.001)
	m.Source, m.Now, m.At = "a", 1, 1
	if err := e.handle(e.sys.Update(m), 1); err != nil {
		t.Fatal(err)
	}
	clk.fail.Store(true)
	if err := e.sourceStopped("a", errors.New("gone"), time.Second); err == nil {
		t.Fatal("an actuator failure during source-stop reselection was swallowed")
	}
}

// TestAstra6SourceStopEndsRunOnActuatorFailure is the same defect seen
// through the real lifecycle: the failure must end Run, not be logged.
func TestAstra6SourceStopEndsRunOnActuatorFailure(t *testing.T) {
	fake := clock.NewFake(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
	clk := &refusingStatusClock{Fake: fake}
	src := &scripted{name: "a", clk: fake, fail: errors.New("port vanished"), failOnce: true}
	cfg := testConfig("", SourceSpec{Source: src, Options: discipline.Options{Numbering: true}})
	e, err := New(cfg, clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	e.tick = time.Hour
	clk.fail.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil after a fatal actuator failure on the source-stop path")
		}
	case <-ctx.Done():
		t.Fatal("Run kept going after a fatal actuator failure on the source-stop path")
	}
}

// TestAstra6RestartKeepsHealthUntilFreshEvidence checks the acquisition reset:
// the recorded failure survives the intention to restart and is cleared only
// when the new run actually delivers something.
func TestAstra6RestartKeepsHealthUntilFreshEvidence(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
	e, err := New(testConfig(""), clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	e.sources["a"] = SourceSpec{Source: &scripted{name: "a", clk: clk}, Options: discipline.Options{Numbering: true}}
	e.order = append(e.order, "a")
	e.sys.AddSource("a", discipline.Options{Numbering: true})

	if err := e.sourceStopped("a", errors.New("port vanished"), time.Second); err != nil {
		t.Fatal(err)
	}
	if e.Status().Infos["a"].LastError == "" {
		t.Fatal("a stopped source must report why")
	}
	e.sourceRestarting("a")
	if got := e.Status().Infos["a"].LastError; got == "" {
		t.Fatal("the failure was cleared by the intention to restart, before any fresh evidence")
	}
	e.noteSourceAlive("a")
	e.publish(e.clk.Monotonic())
	if got := e.Status().Infos["a"].LastError; got != "" {
		t.Fatalf("the failure survived fresh evidence: %q", got)
	}
}

// TestAstra6DropQueuedPreservesOtherSources checks the drain only removes the
// stopped source's entries and keeps the rest in order.
func TestAstra6DropQueuedPreservesOtherSources(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
	e, err := New(testConfig(""), clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"a", "b", "a", "c", "a", "b"} {
		m := good(float64(i) / 1000)
		m.Source = name
		e.meas <- m
	}
	e.dropQueued("a")
	var got []string
	for len(e.meas) > 0 {
		got = append(got, (<-e.meas).Source)
	}
	want := []string{"b", "c", "b"}
	if len(got) != len(want) {
		t.Fatalf("queue %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("queue %v, want %v", got, want)
		}
	}
	if e.staleDrops["a"] != 3 {
		t.Fatalf("dropped %d of the stopped source's measurements, want 3", e.staleDrops["a"])
	}
}

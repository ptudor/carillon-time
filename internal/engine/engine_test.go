package engine

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"carillon/internal/clock"
	"carillon/internal/discipline"
	"carillon/internal/leap"
	"carillon/internal/ntp"
	"carillon/internal/source"
)

// scripted is a Source that advances the fake clock and emits a prepared
// list of measurements, then either fails or idles until cancelled.
type scripted struct {
	name   string
	clk    *clock.Fake
	script []discipline.Measurement
	gap    time.Duration
	fail   error
	resets atomic.Int32
	sent   atomic.Int32
}

func (s *scripted) Name() string { return s.name }
func (s *scripted) Info() source.Info {
	return source.Info{Name: s.name, Reach: 0xff}
}
func (s *scripted) Reset() { s.resets.Add(1) }
func (s *scripted) Run(ctx context.Context, out chan<- discipline.Measurement) error {
	for _, m := range s.script {
		s.clk.Advance(64 * time.Second)
		m.Source = s.name
		m.Now = s.clk.Monotonic()
		m.At = m.Now
		select {
		case out <- m:
			s.sent.Add(1)
		case <-ctx.Done():
			return nil
		}
		select {
		case <-time.After(s.gap):
		case <-ctx.Done():
			return nil
		}
	}
	if s.fail != nil {
		return s.fail
	}
	<-ctx.Done()
	return nil
}

func good(offset float64) discipline.Measurement {
	return discipline.Measurement{
		Valid: true, Reach: 0xff, Poll: 6,
		Offset: offset, Delay: 0.010, Dispersion: 0.001, Jitter: 50e-6,
		Stratum: 2, Leap: ntp.LeapNone, Precision: -20,
		SourceRefID: ntp.RefIDFromString("TEST"),
	}
}

func testConfig(drift string, srcs ...SourceSpec) Config {
	return Config{
		Discipline: discipline.Config{
			Loop: discipline.LoopConfig{
				StepThreshold: 0.5, StepLimit: 3, Panic: 1000, MaxSlewPPM: 500, Precision: 1e-6, FreqMeasure: 900,
			},
			MinSurvivors: 1, HoldoverMax: 3600, SettleUpdates: 3,
		},
		DriftFile: drift,
		Sources:   srcs,
		Version:   "test",
	}
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestEngineStepsSettlesAndPersistsDrift(t *testing.T) {
	dir := t.TempDir()
	drift := filepath.Join(dir, "drift")
	if err := os.WriteFile(drift, []byte("12.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	src := &scripted{name: "a", clk: clk, gap: 20 * time.Millisecond,
		script: []discipline.Measurement{good(2.0), good(0.001), good(0.0005), good(0.0002), good(0.0001)}}
	e, err := New(testConfig(drift, SourceSpec{Source: src, Options: discipline.Options{Numbering: true}}), clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	e.tick = 10 * time.Millisecond
	if st := e.Status(); st.Frequency != 12.5 || !st.FreqKnown || st.State != discipline.StateUnsynced {
		t.Fatalf("initial status %+v", st.Status)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	if err := e.Wait(ctx, func(s *Status) bool { return s.State == discipline.StateSynced }); err != nil {
		t.Fatalf("never synced: %v (status %+v)", err, e.Status().Status)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(clk.Steps) != 1 || clk.Steps[0] < 1990*time.Millisecond || clk.Steps[0] > 2010*time.Millisecond {
		t.Fatalf("steps %v", clk.Steps)
	}
	if src.resets.Load() != 1 {
		t.Fatalf("filters reset %d times, want 1", src.resets.Load())
	}
	if len(clk.Frequencies) == 0 || clk.Frequencies[0] != 12.5 {
		t.Fatalf("initial frequency must be applied first: %v", clk.Frequencies)
	}
	ks := clk.Status()
	if !ks.Synced || ks.Leap != ntp.LeapNone || ks.MaxError <= 0 || ks.MaxError >= 16*time.Second {
		t.Fatalf("kernel status %+v", ks)
	}
	st := e.Status()
	if st.Stratum != 3 || st.RefID != ntp.RefIDFromString("TEST") || st.SystemSource != "a" || st.RefTime.IsZero() || st.Precision != clk.Precision() {
		t.Fatalf("status %+v", st)
	}
	if _, ok := st.Infos["a"]; !ok {
		t.Fatal("source info missing")
	}
	// The drift file holds the frequency as it stands at shutdown, which
	// the loop has moved from the 12.5 it started at.
	v, err := readDrift(drift)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(v-st.Frequency) > 1e-6 || v < 11 || v > 14 {
		t.Fatalf("drift file %v, status %v", v, st.Frequency)
	}
}

func TestEngineUsesKernelFrequencyWithoutDriftFile(t *testing.T) {
	clk := clock.NewFake(time.Now())
	_ = clk.SetFrequency(-7.25)
	clk.Frequencies = nil
	e, err := New(testConfig(""), clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if st := e.Status(); st.Frequency != -7.25 || !st.FreqKnown {
		t.Fatalf("%+v", st.Status)
	}
}

func TestEngineLeapfileOverridesAndResetsAtTransition(t *testing.T) {
	transition := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(transition.Add(-24 * time.Hour))
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
	for i := 1; i <= 3; i++ {
		clk.Advance(time.Second)
		m := good(0.001)
		m.Source, m.Now, m.At = "gps", clk.Monotonic(), clk.Monotonic()
		if err := e.handle(e.sys.Update(m), m.Now); err != nil {
			t.Fatal(err)
		}
	}
	if st := e.Status(); st.State != discipline.StateSynced || st.Leap != ntp.LeapInsert || st.LeapSource != "file" || !st.LeapExpiry.Equal(cfg.LeapTable.Expiry) {
		t.Fatalf("pre-transition status: %+v", st)
	}
	if ks := clk.Status(); !ks.Synced || ks.Leap != ntp.LeapInsert {
		t.Fatalf("pre-transition kernel status: %+v", ks)
	}

	clk.Advance(transition.Sub(clk.TrueTime()) + time.Second)
	now := clk.Monotonic()
	if err := e.handle(e.sys.Tick(now), now); err != nil {
		t.Fatal(err)
	}
	if st := e.Status(); st.State != discipline.StateHoldover || st.Leap != ntp.LeapNone {
		t.Fatalf("post-transition status: %+v", st)
	}
	if src.resets.Load() != 1 {
		t.Fatalf("source reset count = %d", src.resets.Load())
	}
	if ks := clk.Status(); !ks.Synced || ks.Leap != ntp.LeapNone {
		t.Fatalf("post-transition kernel status: %+v", ks)
	}
}

func TestEngineRejectsBadDriftFile(t *testing.T) {
	dir := t.TempDir()
	drift := filepath.Join(dir, "drift")
	if err := os.WriteFile(drift, []byte("9999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Now())
	e, err := New(testConfig(drift), clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if st := e.Status(); st.FreqKnown {
		t.Fatalf("an out-of-range drift value must not be trusted: %+v", st.Status)
	}
}

func TestEngineSourceExitRemovesIt(t *testing.T) {
	clk := clock.NewFake(time.Now())
	src := &scripted{name: "dying", clk: clk, gap: 5 * time.Millisecond,
		script: []discipline.Measurement{good(0.001), good(0.001), good(0.001)}, fail: errors.New("port vanished")}
	e, err := New(testConfig("", SourceSpec{Source: src, Options: discipline.Options{Numbering: true}}), clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	e.tick = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	if err := e.Wait(ctx, func(s *Status) bool { return len(s.Sources) == 0 }); err != nil {
		t.Fatalf("source never removed: %v", err)
	}
	if st := e.Status(); st.State != discipline.StateHoldover {
		t.Fatalf("losing the only source must enter holdover, got %v", st.State)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
}

type refusingClock struct {
	*clock.Fake
	fail atomic.Bool
}

func (r *refusingClock) SetFrequency(ppm float64) error {
	if r.fail.Load() {
		return errors.New("EPERM")
	}
	return r.Fake.SetFrequency(ppm)
}

func TestEngineActuatorFailureIsFatal(t *testing.T) {
	clk := &refusingClock{Fake: clock.NewFake(time.Now())}
	var script []discipline.Measurement
	for i := 0; i < 20; i++ {
		script = append(script, good(0.001))
	}
	src := &scripted{name: "a", clk: clk.Fake, gap: 20 * time.Millisecond, script: script}
	e, err := New(testConfig("", SourceSpec{Source: src, Options: discipline.Options{Numbering: true}}), clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	e.tick = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	// Let the engine apply at least one update, then have the kernel start
	// refusing: the next action must end the run.
	if err := e.Wait(ctx, func(s *Status) bool { return s.Updates >= 1 }); err != nil {
		t.Fatal(err)
	}
	clk.fail.Store(true)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "kernel refused") {
			t.Fatalf("expected a fatal actuator error, got %v", err)
		}
	case <-ctx.Done():
		t.Fatal("engine kept running after the actuator failed")
	}
}

func TestEngineDuplicateSource(t *testing.T) {
	clk := clock.NewFake(time.Now())
	a := &scripted{name: "a", clk: clk}
	_, err := New(testConfig("", SourceSpec{Source: a}, SourceSpec{Source: a}), clk, quietLog())
	if err == nil {
		t.Fatal("duplicate names must be rejected")
	}
}

func TestDriftFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "drift")
	if err := writeDrift(p, -33.125); err != nil {
		t.Fatal(err)
	}
	v, err := readDrift(p)
	if err != nil || v != -33.125 {
		t.Fatalf("got %v %v", v, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
	if err := os.WriteFile(p, []byte("not a number\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readDrift(p); err == nil {
		t.Fatal("garbage must be rejected")
	}
}

// TestEngineLeavesBaseFrequencyInKernel covers RF5X-004. While a slew is in
// progress the kernel holds the base frequency plus a transient of up to
// ±MaxSlewPPM. Shutdown writes the base to the drift file, so it must write
// the same value to the kernel; otherwise the host keeps running fast or slow
// by the transient until something else sets the frequency.
func TestEngineLeavesBaseFrequencyInKernel(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	// 0.3 s is under the step threshold, so it is slewed, and at 500 ppm a
	// slew that size is still saturated when the engine is asked to stop.
	src := &scripted{name: "a", clk: clk, gap: 20 * time.Millisecond,
		script: []discipline.Measurement{good(0.3), good(0.3), good(0.3)}}
	e, err := New(testConfig("", SourceSpec{Source: src, Options: discipline.Options{Numbering: true}}), clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	e.tick = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	if err := e.Wait(ctx, func(s *Status) bool { return s.Pending != 0 && s.Updates >= 2 }); err != nil {
		t.Fatalf("never started slewing: %v (status %+v)", err, e.Status().Status)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	st := e.Status()
	if st.Pending == 0 {
		t.Skip("the slew finished before shutdown; nothing to assert")
	}
	last := clk.Frequencies[len(clk.Frequencies)-1]
	if math.Abs(last-st.Frequency) > 1e-9 {
		t.Fatalf("kernel left at %.6f ppm with %.6f s of phase still pending; want the base estimate %.6f ppm",
			last, st.Pending, st.Frequency)
	}
	if f, _ := clk.Frequency(); math.Abs(f-st.Frequency) > 1e-9 {
		t.Fatalf("clock frequency %.6f ppm, want %.6f", f, st.Frequency)
	}
}

// TestEngineDoesNotRewriteFrequencyAfterTheKernelRefusedOne is the second
// half of RF5X-004: when the fatal error was SetFrequency itself, shutdown
// must not call it again just to produce a second failure.
func TestEngineDoesNotRewriteFrequencyAfterTheKernelRefusedOne(t *testing.T) {
	clk := clock.NewFake(time.Now())
	fc := &failingClock{Fake: clk, failFrequencyAfter: 2}
	src := &scripted{name: "a", clk: clk, gap: 10 * time.Millisecond,
		script: []discipline.Measurement{good(0.3), good(0.3), good(0.3), good(0.3)}}
	e, err := New(testConfig("", SourceSpec{Source: src, Options: discipline.Options{Numbering: true}}), fc, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	e.tick = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = e.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "refused a frequency change") {
		t.Fatalf("run error %v, want a refused frequency change", err)
	}
	if n := fc.frequencyCalls.Load(); n != 3 {
		t.Fatalf("SetFrequency called %d times; the failing call must not be retried on exit", n)
	}
}

// failingClock refuses SetFrequency after a given number of successful calls.
type failingClock struct {
	*clock.Fake
	failFrequencyAfter int32
	frequencyCalls     atomic.Int32
}

func (c *failingClock) SetFrequency(ppm float64) error {
	if c.frequencyCalls.Add(1) > c.failFrequencyAfter {
		return errors.New("simulated EPERM")
	}
	return c.Fake.SetFrequency(ppm)
}

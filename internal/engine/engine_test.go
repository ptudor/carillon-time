package engine

import (
	"context"
	"errors"
	"io/fs"
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
	gen    func() uint64
	// failOnce makes fail apply to the first Run only, so a test can watch
	// the engine restart the source and see it stay up.
	failOnce bool
	runs     atomic.Int32
	resets   atomic.Int32
	sent     atomic.Int32
}

func (s *scripted) Name() string { return s.name }
func (s *scripted) Info() source.Info {
	return source.Info{Name: s.name, Reach: 0xff}
}
func (s *scripted) Reset() { s.resets.Add(1) }
func (s *scripted) Run(ctx context.Context, out chan<- discipline.Measurement) error {
	run := s.runs.Add(1)
	for _, m := range s.script {
		if s.gen != nil {
			m.Generation = s.gen()
		}
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
	if s.fail != nil && (!s.failOnce || run == 1) {
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
			MinSurvivors: 1, HoldoverMax: 3600, SettleUpdates: 1,
		},
		DriftFile: drift,
		Sources:   srcs,
		Version:   "test",
		// The scripted source advances the fake clock 64 s per measurement,
		// so 100 s of monotonic history is a couple of measurements.
		DriftStableWindow: 100 * time.Second,
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

	// RF5X-005: the first fresh measurement after the transition restores
	// SYNCED directly. A leap moves the clock by a whole second and changes
	// neither the frequency nor the phase error, so passing through
	// SETTLING would answer LI=3 / stratum 16 for as long as the state
	// machine took to work back — at exactly the moment a leap second makes
	// a good server most valuable.
	clk.Advance(time.Second)
	m := good(0.001)
	m.Source, m.Now, m.At = "gps", clk.Monotonic(), clk.Monotonic()
	if err := e.handle(e.sys.Update(m), m.Now); err != nil {
		t.Fatal(err)
	}
	st := e.Status()
	if st.State != discipline.StateSynced || st.Leap != ntp.LeapNone {
		t.Fatalf("first post-transition update: state=%v leap=%v, want synced/none", st.State, st.Leap)
	}
	if ks := clk.Status(); !ks.Synced || ks.Leap != ntp.LeapNone {
		t.Fatalf("kernel status after the first post-transition update: %+v", ks)
	}
	// The mapping main.go gives the NTP listener must never read false
	// across the transition: that is what makes clients reject the server.
	if !serverSynced(st) {
		t.Fatalf("the listener would have answered unsynchronized: state=%v", st.State)
	}
}

// serverSynced mirrors the Status mapping in cmd/carillon: what the NTP
// listener answers with.
func serverSynced(st *Status) bool {
	return st.State == discipline.StateSynced || st.State == discipline.StateHoldover
}

// TestEngineNeverServesUnsyncedAcrossALeap walks the whole transition and
// asserts the listener's view is synchronized at every step (RF5X-005).
func TestEngineNeverServesUnsyncedAcrossALeap(t *testing.T) {
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
	feed := func() {
		clk.Advance(time.Second)
		m := good(0.001)
		m.Source, m.Now, m.At = "gps", clk.Monotonic(), clk.Monotonic()
		if err := e.handle(e.sys.Update(m), m.Now); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		feed()
	}
	if !serverSynced(e.Status()) {
		t.Fatal("not synced before the transition")
	}
	clk.Advance(transition.Sub(clk.TrueTime()) + time.Second)
	now := clk.Monotonic()
	if err := e.handle(e.sys.Tick(now), now); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if !serverSynced(e.Status()) {
			t.Fatalf("the listener answered unsynchronized %d updates after the transition (state %v)",
				i, e.Status().State)
		}
		feed()
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

// TestEngineSourceExitMarksItUnreachableAndRestarts covers RF5X-032. A source
// whose goroutine returns used to be deleted outright: it vanished from
// carillonctl sources, from /api/v1/status and from every per-source metric
// series — Prometheus sees the series disappear rather than reach drop to
// zero — and nothing ever brought it back. DESIGN.md §14 promises the
// opposite: marked unreachable, reopen retried with backoff.
func TestEngineSourceExitMarksItUnreachableAndRestarts(t *testing.T) {
	clk := clock.NewFake(time.Now())
	src := &scripted{name: "dying", clk: clk, gap: 5 * time.Millisecond,
		script:   []discipline.Measurement{good(0.001), good(0.001), good(0.001)},
		fail:     errors.New("port vanished"),
		failOnce: true}
	e, err := New(testConfig("", SourceSpec{Source: src, Options: discipline.Options{Numbering: true}}), clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	e.tick = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	unreachable := func(s *Status) bool {
		if len(s.Sources) != 1 || s.Sources[0].Name != "dying" {
			return false
		}
		info, ok := s.Infos["dying"]
		return ok && s.Sources[0].Reach == 0 && info.Reach == 0 && info.LastError == "port vanished"
	}
	if err := e.Wait(ctx, unreachable); err != nil {
		t.Fatalf("source never reported unreachable: %v (status %+v)", err, e.Status().Status)
	}
	if st := e.Status(); st.State != discipline.StateHoldover {
		t.Fatalf("losing the only source must enter holdover, got %v", st.State)
	}

	// The first retry is one second away; the source runs its script again.
	if err := e.Wait(ctx, func(s *Status) bool { return src.sent.Load() > 3 }); err != nil {
		t.Fatalf("source was never restarted: %v (sent %d)", err, src.sent.Load())
	}
	if err := e.Wait(ctx, func(s *Status) bool {
		info, ok := s.Infos["dying"]
		return ok && info.LastError == "" && info.Reach != 0
	}); err != nil {
		t.Fatalf("the restarted source never cleared its error: %v", err)
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

// TestEngineStepsOnceWithTwoSources reproduces RF5X-002. Both sources report
// the same +2 s offset. The engine steps on the first, tells every source to
// reset, and then reads the second source's measurement — which was computed
// against the pre-step clock and was already sitting in the channel. Without
// a generation stamp it is applied, becomes the only valid candidate, and
// steps the clock a second time by the same amount in the wrong direction.
func TestEngineStepsOnceWithTwoSources(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	gen := new(atomic.Uint64)
	script := []discipline.Measurement{good(2.0), good(0.001), good(0.0005), good(0.0002), good(0.0001)}
	a := &scripted{name: "a", clk: clk, gap: 30 * time.Millisecond, gen: gen.Load, script: script}
	b := &scripted{name: "b", clk: clk, gap: 30 * time.Millisecond, gen: gen.Load, script: script}
	cfg := testConfig("",
		SourceSpec{Source: a, Options: discipline.Options{Numbering: true}},
		SourceSpec{Source: b, Options: discipline.Options{Numbering: true}})
	cfg.Generation = gen
	e, err := New(cfg, clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	e.tick = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	if err := e.Wait(ctx, func(s *Status) bool { return s.Updates >= 6 }); err != nil {
		t.Fatalf("never reached six updates: %v (status %+v)", err, e.Status().Status)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(clk.Steps) != 1 {
		t.Fatalf("steps applied to the clock: %v (want exactly one)", clk.Steps)
	}
	if got := gen.Load(); got != 2 {
		t.Fatalf("generation %d, want 2 (one step)", got)
	}
	// Which of the two guards fired depends on the interleaving: if the
	// second source had already read the old generation, its measurement is
	// dropped as stale; if it had not yet sent, System.mayStep refuses a
	// step that rests on one post-step sample. Either way there is one step.
}

// TestEngineDropsStaleMeasurement is the narrow version: a measurement whose
// generation is behind the engine's must not reach the discipline at all.
func TestEngineDropsStaleMeasurement(t *testing.T) {
	clk := clock.NewFake(time.Now())
	gen := new(atomic.Uint64)
	cfg := testConfig("")
	cfg.Generation = gen
	e, err := New(cfg, clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if got := gen.Load(); got != 1 {
		t.Fatalf("generation starts at %d, want 1", got)
	}
	gen.Store(4)
	if !e.stale(discipline.Measurement{Source: "a", Generation: 3}) {
		t.Fatal("a measurement from an older generation must be dropped")
	}
	if e.stale(discipline.Measurement{Source: "a", Generation: 4}) {
		t.Fatal("a current measurement must not be dropped")
	}
	if e.stale(discipline.Measurement{Source: "a", Generation: 0}) {
		t.Fatal("an unstamped measurement must not be dropped")
	}
	if e.staleDrops["a"] != 1 {
		t.Fatalf("stale drops %d, want 1", e.staleDrops["a"])
	}
}

// TestEngineSweepsStaleDriftTemporaries covers RF5X-019. writeDrift is atomic
// — write, fsync, rename — but a SIGKILL or a power cut between CreateTemp
// and Rename leaves a .drift-NNNN file behind, and nothing ever removed one.
func TestEngineSweepsStaleDriftTemporaries(t *testing.T) {
	dir := t.TempDir()
	drift := filepath.Join(dir, "drift")
	if err := os.WriteFile(drift, []byte("3.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, ".drift-123456")
	if err := os.WriteFile(stale, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	// A temporary that another instance may still be writing is left alone.
	fresh := filepath.Join(dir, ".drift-999999")
	if err := os.WriteFile(fresh, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}

	clk := clock.NewFake(time.Now())
	if _, err := New(testConfig(drift), clk, quietLog()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale drift temporary survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a temporary less than a minute old must be left alone: %v", err)
	}
	if _, err := os.Stat(drift); err != nil {
		t.Fatalf("the drift file itself must survive: %v", err)
	}
}

// TestEngineDoesNotPersistAMovingFrequency is the drift-file stability gate.
// A PLL locked to an upstream that is itself slewing follows that upstream's
// rate — correctly — so the frequency word can sit tens of ppm from the
// host's own crystal error until the upstream settles. Persisting that makes
// the next start begin from a frequency nothing on this host needs, and
// produces the same excursion again. It must keep the older, settled value.
func TestEngineDoesNotPersistAMovingFrequency(t *testing.T) {
	dir := t.TempDir()
	drift := filepath.Join(dir, "drift")
	if err := os.WriteFile(drift, []byte("6.125000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Now())
	// Offsets large enough, and spaced far enough apart, that the loop walks
	// the frequency a long way: this is the shape of the twocom transient.
	src := &scripted{name: "a", clk: clk, gap: 20 * time.Millisecond, script: []discipline.Measurement{
		good(0.05), good(0.05), good(0.05), good(0.05), good(0.05), good(0.05),
	}}
	cfg := testConfig(drift, SourceSpec{Source: src, Options: discipline.Options{Numbering: true}})
	cfg.DriftStableWindow = 100 * time.Second
	cfg.DriftStableSpread = 1.0
	e, err := New(cfg, clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	e.tick = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	if err := e.Wait(ctx, func(s *Status) bool { return s.Updates >= 5 }); err != nil {
		t.Fatalf("never reached five updates: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	moved := e.Status().Frequency
	if math.Abs(moved-6.125) <= cfg.DriftStableSpread {
		t.Fatalf("the frequency only moved to %v ppm; this test needs it to move further than %v",
			moved, cfg.DriftStableSpread)
	}
	v, err := readDrift(drift)
	if err != nil {
		t.Fatal(err)
	}
	if v != 6.125 {
		t.Fatalf("drift file overwritten with %v while the frequency was in motion at %v ppm; "+
			"it must keep the last settled value 6.125", v, moved)
	}
}

// TestEngineFrequencySettledGate covers the gate directly: not enough
// history, too much movement, and steady.
func TestEngineFrequencySettledGate(t *testing.T) {
	clk := clock.NewFake(time.Now())
	cfg := testConfig("")
	cfg.DriftStableWindow = 100 * time.Second
	cfg.DriftStableSpread = 1.0
	e, err := New(cfg, clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}

	e.noteFrequency(0, 10)
	if ok, why := e.frequencySettled(50); ok || !strings.Contains(why, "frequency history") {
		t.Fatalf("50 s of history must not settle: ok=%v why=%q", ok, why)
	}
	for at := 0.0; at <= 200; at += 10 {
		e.noteFrequency(at, 10+at/500) // 0.4 ppm across the whole run
	}
	if ok, why := e.frequencySettled(200); !ok {
		t.Fatalf("a steady frequency must settle: %q", why)
	}
	e.noteFrequency(210, 40) // a 30 ppm jump
	if ok, why := e.frequencySettled(210); ok || !strings.Contains(why, "frequency moved") {
		t.Fatalf("a moving frequency must not settle: ok=%v why=%q", ok, why)
	}
	// Once the jump ages out of the window it settles again.
	for at := 220.0; at <= 340; at += 10 {
		e.noteFrequency(at, 40)
	}
	if ok, why := e.frequencySettled(340); !ok {
		t.Fatalf("steady at the new value must settle again: %q", why)
	}
}

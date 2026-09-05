// Package engine runs the daemon's owning goroutine: it collects
// measurements from the sources, feeds the discipline, applies the resulting
// actions to the clock, maintains the kernel's view of synchronization, and
// publishes immutable status snapshots for the server and control socket.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"carillon/internal/clock"
	"carillon/internal/discipline"
	"carillon/internal/leap"
	"carillon/internal/ntp"
	"carillon/internal/source"
)

// maxKernelError is the ceiling both kernels apply to maxerror and esterror
// (MAXPHASE, 16 s), and the value the daemon reports while unsynchronized.
const maxKernelError = 16 * time.Second

// errFrequencyRefused marks a fatal error that came from SetFrequency
// itself, so the shutdown path knows not to make the same call again.
var errFrequencyRefused = errors.New("kernel refused a frequency change")

// Bounds on the delay before a source whose Run returned is started again.
const (
	sourceRestartMin = time.Second
	sourceRestartMax = time.Minute
)

// Defaults for the drift-file stability gate. The drift file exists to give
// the next start a good frequency, and a value the loop is still moving
// through is worse than a stale one: the loop corrects a stale start on its
// own, but a persisted transient starts the next run wrong and invites the
// same transient again.
const (
	defaultDriftStableWindow = 15 * time.Minute
	defaultDriftStableSpread = 1.0 // ppm
)

// SourceSpec pairs a source with its discipline options.
type SourceSpec struct {
	Source  source.Source
	Options discipline.Options
}

// Config configures an Engine.
type Config struct {
	Discipline discipline.Config

	// DriftFile is where the frequency is persisted; "" disables it.
	DriftFile string

	// DriftInterval is how often the drift file is rewritten; zero means
	// hourly.
	DriftInterval time.Duration

	// DriftStableWindow and DriftStableSpread gate that rewrite: the
	// frequency must have stayed inside a band of DriftStableSpread ppm for
	// DriftStableWindow before it is worth persisting. Zero selects the
	// defaults.
	DriftStableWindow time.Duration
	DriftStableSpread float64

	Sources []SourceSpec

	// LeapTable, when non-nil, is authoritative over survivor LI bits.
	LeapTable *leap.Table

	// Version is reported in status snapshots.
	Version string

	// Generation is the measurement epoch shared with the sources: the
	// engine bumps it on every clock step and leap reset, and each source
	// stamps the value it read when it began a sample. It is created here
	// rather than by the engine because the sources are constructed first.
	// Nil means the engine keeps a private counter, which is right for
	// tests whose sources do not stamp.
	Generation *atomic.Uint64

	// Observe receives each immutable status snapshot after publication. It
	// must return promptly; optional statistics use a bounded non-blocking
	// queue so disk I/O never enters the clock-discipline path.
	Observe func(*Status)
}

// Status is the engine's immutable snapshot.
type Status struct {
	discipline.Status

	// Now is the clock reading when the snapshot was taken; RefTime is the
	// clock reading at the last loop update (zero if none).
	Now        time.Time
	RefTime    time.Time
	Uptime     time.Duration
	Precision  int8
	Version    string
	LeapSource string
	LeapExpiry time.Time

	// Infos carries each source's own view, keyed by name.
	Infos map[string]source.Info
}

// Engine is the daemon core. Create it with New and drive it with Run.
type Engine struct {
	cfg     Config
	clk     clock.Clock
	log     *slog.Logger
	sys     *discipline.System
	sources map[string]SourceSpec
	order   []string

	meas   chan discipline.Measurement
	reqs   chan func() error
	status atomic.Pointer[Status]

	tick           time.Duration // ticker period; one second outside tests
	started        time.Time
	startedMono    float64
	refWall        time.Time
	lastLoopUpdate float64
	lastKernel     clock.Status
	haveKernel     bool

	// appliedFreq is the last frequency word the actuator actually
	// accepted, and haveAppliedFreq whether one ever has been. The engine
	// tracks this itself because the startup write in Run bypasses the
	// loop's own bookkeeping, and because the loop records what it issued
	// rather than what the kernel took (RA6X-016).
	appliedFreq     float64
	haveAppliedFreq bool

	// procNow is the engine's processing clock: the monotonic time at which
	// the event now being handled is being consumed, held nondecreasing.
	// Measurements carry the producer's own timestamp, and a buffered or
	// cross-source one can arrive after a newer tick; using it as "now"
	// understated candidate ages, restarted expired timeouts and made
	// uptime and root uncertainty regress while the real clock advanced
	// (RA6X-059). Observation time stays in Measurement.At.
	procNow        float64
	haveProcNow    bool
	lastDriftWrite float64
	driftErrShown  bool
	lastLeapWall   time.Time
	lastFileLeap   ntp.Leap

	gen        *atomic.Uint64
	staleDrops map[string]uint64

	// sourceErrors holds the reason a source's goroutine is not running,
	// overlaid on its own Info snapshot until it is running again.
	sourceErrors map[string]string

	// restarting names the sources whose goroutine is being started again
	// but has not yet delivered anything. Their recorded failure survives
	// until it does, so health rests on fresh evidence.
	restarting map[string]bool

	// freqHistory is the recent base frequency, for the drift-file gate.
	freqHistory    []freqSample
	driftSkipShown bool
}

// freqSample is one reading of the loop's base frequency.
type freqSample struct {
	at  float64 // monotonic seconds
	ppm float64
}

// New builds an engine. It loads the initial frequency (drift file, then the
// kernel's current value) but does not touch the clock until Run.
func New(cfg Config, clk clock.Clock, log *slog.Logger) (*Engine, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.DriftInterval <= 0 {
		cfg.DriftInterval = time.Hour
	}
	if cfg.DriftStableWindow <= 0 {
		cfg.DriftStableWindow = defaultDriftStableWindow
	}
	if cfg.DriftStableSpread <= 0 {
		cfg.DriftStableSpread = defaultDriftStableSpread
	}
	freq, known, origin := initialFrequency(cfg.DriftFile, clk, log)
	log.Info("initial frequency", "ppm", freq, "known", known, "from", origin)
	sweepDriftTemps(cfg.DriftFile, clk.Now(), log)

	gen := cfg.Generation
	if gen == nil {
		gen = new(atomic.Uint64)
	}
	// Generation 0 means "unstamped" and is never stale, so the live
	// counter starts at the first settled epoch. Even values are settled
	// epochs; an odd value means a discontinuity is executing. See the
	// epoch protocol in internal/source.
	gen.CompareAndSwap(0, source.FirstEpoch)

	e := &Engine{
		cfg:          cfg,
		clk:          clk,
		log:          log,
		sys:          discipline.New(cfg.Discipline, freq, known),
		sources:      make(map[string]SourceSpec, len(cfg.Sources)),
		meas:         make(chan discipline.Measurement, 64),
		reqs:         make(chan func() error, 16),
		tick:         time.Second,
		gen:          gen,
		staleDrops:   make(map[string]uint64, len(cfg.Sources)),
		sourceErrors: make(map[string]string, len(cfg.Sources)),
		restarting:   make(map[string]bool, len(cfg.Sources)),
	}
	for _, s := range cfg.Sources {
		name := s.Source.Name()
		if _, dup := e.sources[name]; dup {
			return nil, fmt.Errorf("engine: duplicate source name %q", name)
		}
		e.sources[name] = s
		e.order = append(e.order, name)
		e.sys.AddSource(name, s.Options)
	}
	e.started = clk.Now()
	e.lastLeapWall = e.started
	e.startedMono = clk.Monotonic()
	e.publish(e.startedMono)
	return e, nil
}

// initialFrequency returns the starting frequency correction and whether
// it is trustworthy: a drift file is, a non-zero kernel value left by a
// previous daemon is, zero is a guess.
func initialFrequency(path string, clk clock.Clock, log *slog.Logger) (freq float64, known bool, origin string) {
	if path != "" {
		v, err := readDrift(path)
		switch {
		case err == nil:
			return v, true, "drift file"
		case errors.Is(err, fs.ErrNotExist):
			log.Info("no drift file yet", "path", path)
		default:
			log.Warn("ignoring drift file", "path", path, "error", err)
		}
	}
	if kf, err := clk.Frequency(); err == nil && kf != 0 {
		return kf, true, "kernel"
	}
	return 0, false, "none"
}

func readDrift(path string) (float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", path, err)
	}
	// strconv accepts "NaN", "Inf", "+Inf" and their case variants, and a
	// non-finite value passes both of the comparisons below: NaN compares
	// false against everything. Trusting one marks the frequency known,
	// initializes the loop with it, survives clampFreq (math.Min/math.Max
	// propagate NaN) and converts to an arbitrary kernel word. Reject it
	// here so it takes the same fallback as any other unusable drift file
	// (RA6X-014). A NaN is not a measured zero and is not treated as one.
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%s: %v is not a finite frequency", path, v)
	}
	if v > discipline.MaxFrequency || v < -discipline.MaxFrequency {
		return 0, fmt.Errorf("%s: %v ppm is outside ±%v", path, v, discipline.MaxFrequency)
	}
	return v, nil
}

// driftTempPattern is the os.CreateTemp pattern writeDrift uses. It is
// derived from the destination's own name so that two carillon instances
// sharing a state directory cannot see each other's temporaries as garbage,
// and so that no configured destination can match the sweep's glob: a name of
// the form ".<base>-tmp-<digits>" is always longer than "<base>".
func driftTempPattern(path string) string {
	return "." + filepath.Base(path) + "-tmp-*"
}

// driftTempName matches exactly what os.CreateTemp produces from that
// pattern: the literal prefix followed by the decimal digits of a random
// number, and nothing else. Anything else in the directory — including a
// drift file the operator happened to name .drift-calibrated — is not this
// writer's and is never removed.
var driftTempName = regexp.MustCompile(`-tmp-[0-9]+$`)

// legacyDriftTempName matches the temporaries carillon wrote before the
// pattern became destination-specific. They are only swept when the
// destination is the default "drift", which is the only case in which they
// provably belonged to it.
var legacyDriftTempName = regexp.MustCompile(`^\.drift-[0-9]+$`)

// sweepDriftTemps removes leftover drift temporaries. writeDrift is atomic —
// write, fsync, rename — but a SIGKILL or a power cut between CreateTemp and
// Rename leaves one behind, and nothing else ever removes it. A year of
// unclean shutdowns leaves clutter in the state directory that -check cannot
// explain.
//
// Every candidate must clear four tests before it is removed (RA6X-015):
// its name must be one this writer generates, it must be a regular file
// (Lstat, so a symlink is never followed and a directory is never touched),
// it must not be the configured destination itself by device and inode, and
// it must be older than a minute so a concurrent write is left alone. The
// filename is operator-configurable, so the destination can legitimately look
// like a temporary; deleting it would throw away the host's calibration.
func sweepDriftTemps(path string, now time.Time, log *slog.Logger) {
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	dest, destErr := os.Lstat(path)
	sweepLegacy := filepath.Base(path) == "drift"

	matches, err := filepath.Glob(filepath.Join(dir, ".*"))
	if err != nil {
		return
	}
	for _, m := range matches {
		base := filepath.Base(m)
		mine := strings.HasPrefix(base, "."+filepath.Base(path)+"-tmp-") && driftTempName.MatchString(base)
		if !mine && !(sweepLegacy && legacyDriftTempName.MatchString(base)) {
			continue
		}
		info, err := os.Lstat(m)
		if err != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) < time.Minute {
			continue
		}
		if destErr == nil && os.SameFile(dest, info) {
			log.Debug("not removing the configured drift file", "path", m)
			continue
		}
		if err := os.Remove(m); err != nil {
			log.Debug("cannot remove a stale drift temporary", "path", m, "error", err)
			continue
		}
		log.Debug("removed a stale drift temporary", "path", m)
	}
}

// writeDrift persists the frequency atomically (write, fsync, rename).
func writeDrift(path string, ppm float64) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, driftTempPattern(path))
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := fmt.Fprintf(f, "%.6f\n", ppm); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// Status returns the most recent snapshot. It never returns nil.
func (e *Engine) Status() *Status { return e.status.Load() }

// Wait blocks until pred is true of a status snapshot or ctx is done.
func (e *Engine) Wait(ctx context.Context, pred func(*Status) bool) error {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		if pred(e.Status()) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Run drives the engine until ctx is done or the clock refuses an
// adjustment. On return the frequency is left in the kernel and the drift
// file is written.
func (e *Engine) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := e.setFrequency(e.sys.Frequency()); err != nil {
		return fmt.Errorf("engine: setting initial frequency: %w", err)
	}
	if err := e.setKernel(clock.Status{Leap: ntp.LeapUnsync, MaxError: maxKernelError, EstError: maxKernelError}); err != nil {
		return err
	}

	var wg sync.WaitGroup
	for _, name := range e.order {
		spec := e.sources[name]
		wg.Add(1)
		go func(name string, s source.Source) {
			defer wg.Done()
			e.runSource(ctx, name, s)
		}(name, spec.Source)
	}

	ticker := time.NewTicker(e.tick)
	defer ticker.Stop()
	var runErr error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case m := <-e.meas:
			// Processing time is established here, at consumption, not
			// taken from the producer's enqueue-time reading, and a leap
			// boundary is processed before the measurement is admitted:
			// both change whether this observation may be used at all.
			now := e.processing(e.clk.Monotonic())
			e.crossLeap(now)
			if e.stale(m) {
				continue
			}
			e.noteSourceAlive(m.Source)
			m.Now = now
			if err := e.handle(e.sys.Update(m), now); err != nil {
				runErr = err
				break loop
			}
		case <-ticker.C:
			now := e.processing(e.clk.Monotonic())
			e.crossLeap(now)
			if err := e.handle(e.sys.Tick(now), now); err != nil {
				runErr = err
				break loop
			}
			e.maybeWriteDrift(now, false)
		case f := <-e.reqs:
			// Lifecycle work runs on this goroutine like everything else,
			// and a fatal result from it reaches the same shutdown path a
			// fatal result from a measurement does (RA6X-017).
			if err := f(); err != nil {
				runErr = err
				break loop
			}
		}
	}
	cancel()
	wg.Wait()
	e.restoreBaseFrequency(runErr)
	e.maybeWriteDrift(e.processing(e.clk.Monotonic()), true)
	return runErr
}

// restoreBaseFrequency puts the base frequency estimate back into the kernel
// on the way out. While the daemon runs, the word the kernel holds is the
// base plus the one-second phase-slew transient the last Tick issued — up to
// ±MaxSlewPPM. The drift file records the base, so leaving the transient
// behind means the host runs fast or slow by up to 500 ppm (43 s/day) from
// the moment carillon stops until something else writes the frequency word.
//
// The pending phase is abandoned rather than finished: it can take
// arbitrarily long, and exit never steps.
func (e *Engine) restoreBaseFrequency(runErr error) {
	// What the kernel is actually running, not what the loop last issued:
	// the startup write happens before the first Tick, so several
	// measurements can move the base with the loop still reporting that it
	// has applied nothing (RA6X-016).
	applied, ok := e.appliedFreq, e.haveAppliedFreq
	base := e.sys.Frequency()
	if !ok || applied == base {
		return
	}
	if errors.Is(runErr, errFrequencyRefused) {
		// The actuator already refused a frequency change; a second attempt
		// would only produce a second error on the way out.
		return
	}
	if err := e.setFrequency(base); err != nil {
		e.log.Warn("cannot restore the base frequency on exit", "ppm", base, "error", err)
		return
	}
	e.log.Info("kernel frequency left at the base estimate",
		"ppm", base, "abandoned_slew_ppm", applied-base, "abandoned_phase", e.sys.Pending())
}

// processing advances the engine's processing clock to now and returns it.
// It never goes backwards: an event delivered late must not make a source
// look younger, restart a timeout that has already expired, or rewind the
// published uptime (RA6X-059).
func (e *Engine) processing(now float64) float64 {
	if !e.haveProcNow || now > e.procNow {
		e.procNow, e.haveProcNow = now, true
	}
	return e.procNow
}

// setFrequency writes a frequency word and records it if the actuator took
// it. Every frequency write in the daemon goes through here so the shutdown
// path knows what the kernel is holding.
func (e *Engine) setFrequency(ppm float64) error {
	if err := e.clk.SetFrequency(ppm); err != nil {
		return err
	}
	e.appliedFreq, e.haveAppliedFreq = ppm, true
	return nil
}

// stale reports whether a measurement describes a clock reading the engine
// has since invalidated. Sources stamp the generation they read when they
// began a sample and discard the sample themselves if it changed before they
// emitted; this catches the remaining window, where the engine bumped the
// generation after the source's last look but before this measurement came
// off the channel. Applying such a measurement is what stepped the clock a
// second time by the same amount.
func (e *Engine) stale(m discipline.Measurement) bool {
	if m.Generation == 0 {
		return false // the source does not stamp generations
	}
	current := e.gen.Load()
	// Exactly the current settled epoch, and nothing else. An odd epoch is
	// one a discontinuity was executing during, and a value ahead of the
	// engine's own counter is not a reading of this clock at all.
	if m.Generation == current && source.StableEpoch(current) {
		return false
	}
	e.staleDrops[m.Source]++
	e.log.Debug("discarding a measurement taken outside the current clock epoch",
		"source", m.Source, "generation", m.Generation, "current", current)
	return true
}

// beginEpochChange marks a clock discontinuity as in progress and returns the
// function that completes it. Between the two the counter is odd, so every
// sample a source starts, finishes, or emits inside the window is recognisably
// untrustworthy rather than being labelled with the new epoch (RA6X-006). The
// completion must run even when the discontinuity fails, or acquisition would
// stay permanently in progress.
func (e *Engine) beginEpochChange() func() {
	e.gen.Add(1)
	done := false
	return func() {
		if !done {
			done = true
			e.gen.Add(1)
		}
	}
}

// runSource runs one source, restarting it whenever Run returns.
//
// In practice no production source returns: all three reopen their device
// internally. But DESIGN.md §14 promises that a source whose goroutine exits
// is marked unreachable and retried with backoff, and forgetting it instead
// made it vanish from carillonctl sources, /api/v1/status and every
// per-source metric series — where Prometheus sees the series disappear
// rather than reach drop to zero — with nothing to bring it back.
func (e *Engine) runSource(ctx context.Context, name string, s source.Source) {
	backoff := sourceRestartMin
	for {
		err := s.Run(ctx, e.meas)
		if ctx.Err() != nil {
			return
		}
		delay := backoff
		if !e.request(ctx, func() error { return e.sourceStopped(name, err, delay) }) {
			return
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if !e.request(ctx, func() error { e.sourceRestarting(name); return nil }) {
			return
		}
		if backoff < sourceRestartMax {
			backoff *= 2
			if backoff > sourceRestartMax {
				backoff = sourceRestartMax
			}
		}
	}
}

// request hands f to the engine goroutine, or reports false if the daemon is
// shutting down. A non-nil result from f ends the run.
func (e *Engine) request(ctx context.Context, f func() error) bool {
	select {
	case e.reqs <- f:
		return true
	case <-ctx.Done():
		return false
	}
}

// sourceStopped records that a source's goroutine returned. The source stays
// registered; reach 0 is how every other unreachable source is reported, so
// the selector, the status output and the metrics all say the same thing.
func (e *Engine) sourceStopped(name string, err error, retryIn time.Duration) error {
	spec, ok := e.sources[name]
	if !ok {
		return nil
	}
	reason := "source stopped"
	if err != nil {
		reason = err.Error()
		e.log.Error("source stopped; restarting", "source", name, "error", err, "retry_in", retryIn)
	} else {
		e.log.Warn("source stopped; restarting", "source", name, "retry_in", retryIn)
	}
	e.sourceErrors[name] = reason
	// Run has already returned, so every measurement of this source's still
	// in the queue belongs to the run that has stopped, and nothing more can
	// arrive until it is restarted. Drop them here: lifecycle events and
	// measurements travel on different channels, so a stop event can
	// otherwise be overtaken by an older valid sample that revives a dead
	// source (RA6X-017).
	e.dropQueued(name)
	// Nothing is producing for this source any more, so its estimate is
	// revoked rather than merely marked unreachable: a source whose
	// goroutine has exited must not stay selected on the strength of the
	// last thing it said.
	now := e.processing(e.clk.Monotonic())
	m := discipline.Measurement{
		Source: name, Now: now, Reach: 0, Invalidate: true,
		Poll: spec.Source.Info().Poll, Generation: e.gen.Load(),
	}
	// A fallback selection here can issue an actuator action, and an
	// actuator failure is fatal wherever it happens: return it rather than
	// logging it and carrying on (RA6X-017).
	return e.handle(e.sys.Update(m), now)
}

// sourceRestarting prepares for a source's Run to be entered again. It is
// called from the source's own goroutine just before that happens.
//
// The recorded failure is not cleared yet. It is cleared when the restarted
// run actually delivers something, so health reflects fresh evidence rather
// than the mere intention to restart.
func (e *Engine) sourceRestarting(name string) {
	e.restarting[name] = true
	e.log.Info("restarting source", "source", name)
	e.publish(e.clk.Monotonic())
}

// dropQueued removes every queued measurement from one source, preserving the
// order of the others. It runs on the engine goroutine, so nothing else is
// reading e.meas while it does.
func (e *Engine) dropQueued(name string) {
	n := len(e.meas)
	if n == 0 {
		return
	}
	keep := make([]discipline.Measurement, 0, n)
	for i := 0; i < n; i++ {
		select {
		case m := <-e.meas:
			if m.Source == name {
				e.staleDrops[name]++
				continue
			}
			keep = append(keep, m)
		default:
			i = n
		}
	}
	for _, m := range keep {
		select {
		case e.meas <- m:
		default:
			e.staleDrops[m.Source]++
		}
	}
}

// noteSourceAlive clears a recorded failure once the restarted run has
// actually produced something.
func (e *Engine) noteSourceAlive(name string) {
	if e.restarting[name] {
		delete(e.restarting, name)
		delete(e.sourceErrors, name)
	}
}

// crossLeap processes a leap-second boundary the kernel has just applied. It
// must run *before* a queued measurement is fed to the discipline and before
// any correction action is evaluated: a leap moves the clock by exactly one
// second, so an observation spanning the boundary is wrong by that much, and
// resetting the sources afterwards cannot undo a step the loop has already
// asked for (RA6X-007).
//
// The reset is bracketed by the same in-progress epoch a step uses, so a
// sample a source starts while the reset runs is not labelled with the new
// epoch either.
func (e *Engine) crossLeap(now float64) {
	if e.cfg.LeapTable == nil {
		return
	}
	wall := e.clk.Now()
	if !e.cfg.LeapTable.Crossed(e.lastLeapWall, wall) {
		e.lastLeapWall = wall
		return
	}
	e.lastLeapWall = wall
	// A leap moves the clock by a second: every measurement in flight is
	// wrong by exactly that much.
	finish := e.beginEpochChange()
	for _, src := range e.sources {
		src.Source.Reset()
	}
	finish()
	reset := e.sys.Resync(now)
	for _, ev := range reset.Events {
		e.logEvent(ev)
	}
	e.log.Warn("leap transition crossed; source filters reset", "at", wall.UTC().Format(time.RFC3339Nano))
}

// handle applies actions, logs events, refreshes the kernel status and
// publishes a snapshot. An actuator failure is fatal: the daemon cannot do
// its job without the clock.
//
// Callers that consume a queued measurement must call crossLeap first; handle
// repeats the check for the benefit of direct callers, and it is idempotent
// because a boundary is only crossed once.
func (e *Engine) handle(res discipline.Result, now float64) error {
	now = e.processing(now)
	e.crossLeap(now)
	for _, a := range res.Actions {
		switch a.Kind {
		case discipline.ActionSetFrequency:
			if err := clock.CheckFrequency(a.Value); err != nil {
				return fmt.Errorf("engine: %w: %w", errFrequencyRefused, err)
			}
			if err := e.setFrequency(a.Value); err != nil {
				return fmt.Errorf("engine: %w of %.3f ppm: %w", errFrequencyRefused, a.Value, err)
			}
		case discipline.ActionStep:
			// A finite offset need not fit in a time.Duration, and the plain
			// conversion wraps in silence: +1e20 s becomes a step of about
			// +292 years. Refuse before anything is committed — in
			// particular before the generation bump, so a refused step does
			// not invalidate every sample in flight (RA6X-041).
			delta, err := clock.Seconds(a.Value)
			if err != nil {
				return fmt.Errorf("engine: refusing a step of %v s: %w", a.Value, err)
			}
			// Bracket the whole discontinuity: anything a source is
			// holding, anything already queued, and anything started while
			// the syscall runs is recognisable as not belonging to the new
			// epoch.
			finish := e.beginEpochChange()
			before := e.clk.Now()
			err = e.clk.Step(delta)
			finish()
			if err != nil {
				return fmt.Errorf("engine: kernel refused step of %.6f s: %w", a.Value, err)
			}
			after := e.clk.Now()
			e.log.Warn("clock stepped", "seconds", a.Value, "before", before.UTC().Format(time.RFC3339Nano), "after", after.UTC().Format(time.RFC3339Nano))
		case discipline.ActionResetFilters:
			for _, s := range e.sources {
				s.Source.Reset()
			}
		}
	}
	for _, ev := range res.Events {
		e.logEvent(ev)
	}
	st := e.sys.Status(now)
	wall := e.clk.Now()
	if e.cfg.LeapTable != nil {
		indicator := e.cfg.LeapTable.Indicator(wall)
		if indicator != e.lastFileLeap {
			if indicator == ntp.LeapNone {
				e.log.Info("leap warning cleared")
			} else {
				e.log.Warn("leap warning active", "leap", indicator.String())
			}
			e.lastFileLeap = indicator
		}
		if st.State == discipline.StateSynced || st.State == discipline.StateHoldover {
			st.Leap = indicator
		}
	}
	if st.LastUpdate != e.lastLoopUpdate {
		e.lastLoopUpdate = st.LastUpdate
		e.refWall = e.clk.Now()
	}
	e.noteFrequency(now, st.Frequency)
	if err := e.syncKernel(&st); err != nil {
		return err
	}
	e.publishStatus(&st, now)
	return nil
}

func (e *Engine) logEvent(ev discipline.Event) {
	switch ev.Kind {
	case discipline.EventStep:
		// Logged with before/after when the action was applied.
	case discipline.EventPanicRefused:
		e.log.Error("offset exceeds the panic threshold; refusing to correct the clock",
			"offset", ev.Value, "hint", "set [step] panic_at_startup = true for a first boot without an RTC, or set the clock by hand")
	case discipline.EventPopcorn:
		e.log.Warn("ignored offset spike", "offset", ev.Value)
	case discipline.EventStateChange:
		switch ev.To {
		case discipline.StateSynced:
			e.log.Info("clock synchronized", "from", ev.From.String())
		case discipline.StateSettling:
			e.log.Info("clock settling", "from", ev.From.String())
		default:
			e.log.Warn("clock state changed", "from", ev.From.String(), "to", ev.To.String())
		}
	case discipline.EventFalseticker:
		e.log.Warn("source is a falseticker", "source", ev.Source, "offset", ev.Value)
	case discipline.EventTruechimer:
		e.log.Info("source agrees again", "source", ev.Source, "offset", ev.Value)
	case discipline.EventPreferLost:
		e.log.Error("preferred source is not usable; falling back to the other survivors")
	case discipline.EventPreferRegained:
		e.log.Info("preferred source is back in charge")
	case discipline.EventSystemSource:
		e.log.Info("system source", "source", ev.Source)
	case discipline.EventUnknownSource:
		e.log.Error("measurement from an unregistered source", "source", ev.Source)
	case discipline.EventPPSUnqualified:
		e.log.Error("PPS present but nothing to number its seconds — add an NTP server or use a gps refclock", "source", ev.Source)
	case discipline.EventPPSQualified:
		e.log.Info("PPS seconds qualified again", "source", ev.Source)
	}
}

// syncKernel keeps STA_UNSYNC, the leap flags and the error bounds current.
func (e *Engine) syncKernel(st *discipline.Status) error {
	synced := st.State == discipline.StateSynced || st.State == discipline.StateHoldover
	ks := clock.Status{Synced: synced, Leap: st.Leap}
	if synced {
		// Saturating rather than failing: an uncertainty too large to
		// express is honestly reported as the largest bound both kernels
		// accept, which is what an unknown error bound is (RA6X-041).
		ks.MaxError = clock.BoundSeconds(st.RootDisp+st.RootDelay/2, maxKernelError)
		ks.EstError = clock.BoundSeconds(st.Jitter, maxKernelError)
	} else {
		ks.Leap = ntp.LeapUnsync
		ks.MaxError = maxKernelError
		ks.EstError = maxKernelError
	}
	if e.haveKernel && ks == e.lastKernel {
		return nil
	}
	return e.setKernel(ks)
}

func (e *Engine) setKernel(ks clock.Status) error {
	if err := e.clk.SetStatus(ks); err != nil {
		return fmt.Errorf("engine: kernel refused status update: %w", err)
	}
	e.lastKernel, e.haveKernel = ks, true
	return nil
}

func (e *Engine) publish(now float64) {
	now = e.processing(now)
	st := e.sys.Status(now)
	if e.cfg.LeapTable != nil && (st.State == discipline.StateSynced || st.State == discipline.StateHoldover) {
		st.Leap = e.cfg.LeapTable.Indicator(e.clk.Now())
	}
	e.publishStatus(&st, now)
}

func (e *Engine) publishStatus(st *discipline.Status, now float64) {
	s := &Status{
		Status:    *st,
		Now:       e.clk.Now(),
		RefTime:   e.refWall,
		Uptime:    time.Duration((now - e.startedMono) * float64(time.Second)),
		Precision: e.clk.Precision(),
		Version:   e.cfg.Version,
		Infos:     make(map[string]source.Info, len(e.sources)),
	}
	if e.cfg.LeapTable != nil {
		s.LeapSource = "file"
		s.LeapExpiry = e.cfg.LeapTable.Expiry
	} else {
		s.LeapSource = "sources"
	}
	for name, spec := range e.sources {
		info := spec.Source.Info()
		info.Stale += e.staleDrops[name]
		if reason := e.sourceErrors[name]; reason != "" {
			// The goroutine is not running, so the source's own snapshot is
			// as stale as the moment it stopped.
			info.Reach, info.LastError = 0, reason
		}
		s.Infos[name] = info
	}
	e.status.Store(s)
	if e.cfg.Observe != nil {
		e.cfg.Observe(s)
	}
}

// noteFrequency records the base frequency for the drift-file stability gate,
// keeping only the last DriftStableWindow of history.
func (e *Engine) noteFrequency(now, ppm float64) {
	e.freqHistory = append(e.freqHistory, freqSample{at: now, ppm: ppm})
	// Keep the newest sample at or before the cut, so the retained history
	// spans the whole window rather than starting inside it.
	cut := now - e.cfg.DriftStableWindow.Seconds()
	drop := 0
	for drop+1 < len(e.freqHistory) && e.freqHistory[drop+1].at <= cut {
		drop++
	}
	if drop > 0 {
		e.freqHistory = append(e.freqHistory[:0], e.freqHistory[drop:]...)
	}
}

// frequencySettled reports whether the frequency estimate has held still long
// enough to be worth persisting, and if not, why.
//
// A PLL locked to an upstream that is itself slewing follows that upstream's
// rate — correctly, that is what a PLL is for — so the frequency word can sit
// tens of ppm away from the host's own crystal error for as long as the
// upstream takes to settle. Writing that to the drift file makes the next
// start begin from a frequency nothing on this host needs, which produces the
// same excursion again. Requiring the estimate to have stopped moving costs a
// stale file at worst, and the loop corrects a stale start by itself.
func (e *Engine) frequencySettled(now float64) (bool, string) {
	window := e.cfg.DriftStableWindow.Seconds()
	if len(e.freqHistory) == 0 || now-e.freqHistory[0].at < window {
		return false, fmt.Sprintf("less than %s of frequency history", e.cfg.DriftStableWindow)
	}
	lo, hi := e.freqHistory[0].ppm, e.freqHistory[0].ppm
	for _, f := range e.freqHistory[1:] {
		lo = math.Min(lo, f.ppm)
		hi = math.Max(hi, f.ppm)
	}
	if spread := hi - lo; spread > e.cfg.DriftStableSpread {
		return false, fmt.Sprintf("frequency moved %.3f ppm in the last %s, more than the %.3f ppm the estimate must hold to",
			spread, e.cfg.DriftStableWindow, e.cfg.DriftStableSpread)
	}
	return true, ""
}

// maybeWriteDrift writes the drift file when the frequency is trustworthy
// and either the interval has elapsed or the daemon is shutting down.
func (e *Engine) maybeWriteDrift(now float64, final bool) {
	if e.cfg.DriftFile == "" || !e.sys.FreqKnown() {
		return
	}
	if !final && e.lastDriftWrite != 0 && now-e.lastDriftWrite < e.cfg.DriftInterval.Seconds() {
		return
	}
	if settled, why := e.frequencySettled(now); !settled {
		// Keep what is already on disk: it is the last estimate that did
		// hold still, which is a better start than one caught in motion.
		if final {
			e.log.Info("leaving the drift file alone", "reason", why,
				"current_ppm", e.sys.Frequency(), "path", e.cfg.DriftFile)
		} else if !e.driftSkipShown {
			e.log.Info("deferring the drift file write", "reason", why, "path", e.cfg.DriftFile)
			e.driftSkipShown = true
		}
		return
	}
	e.driftSkipShown = false
	if err := writeDrift(e.cfg.DriftFile, e.sys.Frequency()); err != nil {
		if !e.driftErrShown {
			e.log.Warn("cannot write drift file", "path", e.cfg.DriftFile, "error", err)
			e.driftErrShown = true
		}
		return
	}
	if e.driftErrShown {
		e.log.Info("drift file writable again", "path", e.cfg.DriftFile)
		e.driftErrShown = false
	}
	e.lastDriftWrite = now
}

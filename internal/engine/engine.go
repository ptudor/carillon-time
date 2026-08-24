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
	"os"
	"path/filepath"
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

	Sources []SourceSpec

	// LeapTable, when non-nil, is authoritative over survivor LI bits.
	LeapTable *leap.Table

	// Version is reported in status snapshots.
	Version string

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
	reqs   chan func()
	status atomic.Pointer[Status]

	tick           time.Duration // ticker period; one second outside tests
	started        time.Time
	startedMono    float64
	refWall        time.Time
	lastLoopUpdate float64
	lastKernel     clock.Status
	haveKernel     bool
	lastDriftWrite float64
	driftErrShown  bool
	lastLeapWall   time.Time
	lastFileLeap   ntp.Leap
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
	freq, known, origin := initialFrequency(cfg.DriftFile, clk, log)
	log.Info("initial frequency", "ppm", freq, "known", known, "from", origin)

	e := &Engine{
		cfg:     cfg,
		clk:     clk,
		log:     log,
		sys:     discipline.New(cfg.Discipline, freq, known),
		sources: make(map[string]SourceSpec, len(cfg.Sources)),
		meas:    make(chan discipline.Measurement, 64),
		reqs:    make(chan func(), 16),
		tick:    time.Second,
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
	if v > discipline.MaxFrequency || v < -discipline.MaxFrequency {
		return 0, fmt.Errorf("%s: %v ppm is outside ±%v", path, v, discipline.MaxFrequency)
	}
	return v, nil
}

// writeDrift persists the frequency atomically (write, fsync, rename).
func writeDrift(path string, ppm float64) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".drift-*")
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

	if err := e.clk.SetFrequency(e.sys.Frequency()); err != nil {
		return fmt.Errorf("engine: setting initial frequency: %w", err)
	}
	if err := e.setKernel(clock.Status{Leap: ntp.LeapUnsync, MaxError: 16 * time.Second, EstError: 16 * time.Second}); err != nil {
		return err
	}

	var wg sync.WaitGroup
	for _, name := range e.order {
		spec := e.sources[name]
		wg.Add(1)
		go func(name string, s source.Source) {
			defer wg.Done()
			err := s.Run(ctx, e.meas)
			if ctx.Err() != nil {
				return
			}
			select {
			case e.reqs <- func() { e.sourceExited(name, err) }:
			case <-ctx.Done():
			}
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
			if err := e.handle(e.sys.Update(m), m.Now); err != nil {
				runErr = err
				break loop
			}
		case <-ticker.C:
			now := e.clk.Monotonic()
			if err := e.handle(e.sys.Tick(now), now); err != nil {
				runErr = err
				break loop
			}
			e.maybeWriteDrift(now, false)
		case f := <-e.reqs:
			f()
		}
	}
	cancel()
	wg.Wait()
	e.maybeWriteDrift(e.clk.Monotonic(), true)
	return runErr
}

func (e *Engine) sourceExited(name string, err error) {
	if err != nil {
		e.log.Error("source stopped", "source", name, "error", err)
	} else {
		e.log.Warn("source stopped", "source", name)
	}
	delete(e.sources, name)
	for i, n := range e.order {
		if n == name {
			e.order = append(e.order[:i], e.order[i+1:]...)
			break
		}
	}
	now := e.clk.Monotonic()
	if err := e.handle(e.sys.RemoveSource(name, now), now); err != nil {
		e.log.Error("after source removal", "error", err)
	}
}

// handle applies actions, logs events, refreshes the kernel status and
// publishes a snapshot. An actuator failure is fatal: the daemon cannot do
// its job without the clock.
func (e *Engine) handle(res discipline.Result, now float64) error {
	for _, a := range res.Actions {
		switch a.Kind {
		case discipline.ActionSetFrequency:
			if err := e.clk.SetFrequency(a.Value); err != nil {
				return fmt.Errorf("engine: kernel refused frequency %.3f ppm: %w", a.Value, err)
			}
		case discipline.ActionStep:
			before := e.clk.Now()
			if err := e.clk.Step(time.Duration(a.Value * float64(time.Second))); err != nil {
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
		if e.cfg.LeapTable.Crossed(e.lastLeapWall, wall) {
			for _, src := range e.sources {
				src.Source.Reset()
			}
			reset := e.sys.InvalidateSources(now)
			for _, ev := range reset.Events {
				e.logEvent(ev)
			}
			st = e.sys.Status(now)
			e.log.Warn("leap transition crossed; source filters reset", "at", wall.UTC().Format(time.RFC3339Nano))
		}
		e.lastLeapWall = wall
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
		ks.MaxError = time.Duration((st.RootDisp + st.RootDelay/2) * float64(time.Second))
		ks.EstError = time.Duration(st.Jitter * float64(time.Second))
	} else {
		ks.Leap = ntp.LeapUnsync
		ks.MaxError = 16 * time.Second
		ks.EstError = 16 * time.Second
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
		s.Infos[name] = spec.Source.Info()
	}
	e.status.Store(s)
	if e.cfg.Observe != nil {
		e.cfg.Observe(s)
	}
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

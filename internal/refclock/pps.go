// Package refclock implements carillon's local PPS reference clock.
package refclock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sync/atomic"
	"time"

	"carillon/internal/clock"
	"carillon/internal/discipline"
	"carillon/internal/ntp"
	"carillon/internal/pps"
	"carillon/internal/source"
)

const (
	defaultPPSPollMin    = 4
	defaultPPSPollMax    = 7
	defaultLockJitter    = 200e-6
	defaultFetchTimeout  = 1500 * time.Millisecond
	maxPulseIntervalSkew = 100 * time.Millisecond
	spikeFloor           = 1e-6
)

// PPSConfig configures a bare PPS reference clock.
type PPSConfig struct {
	Name       string
	Device     string
	Edge       pps.Edge
	Offset     float64
	LockJitter float64
	PollMin    int8
	PollMax    int8
}

// PPS consumes kernel-timestamped pulse edges and emits robust, averaged
// offset measurements. Its mutable filter state belongs to Run; Info and
// Reset communicate through atomically replaced snapshots/flags.
type PPS struct {
	cfg PPSConfig
	clk clock.Clock
	log *slog.Logger

	reader pps.Reader
	opener func() (pps.Reader, error)

	resetRequested atomic.Bool
	info           atomic.Pointer[source.Info]

	// The fields below are owned by the Run goroutine.
	reach         uint8
	poll          int8
	window        []float64
	intervals     []float64
	slots         int
	pendingMisses int
	havePrevious  bool
	previousSeq   uint32
	previousTime  time.Time
	stable        bool
	haveEverPulse bool
}

// NewPPS validates cfg and opens its kernel PPS device. Opening at daemon
// construction makes capability and permission failures fatal before any
// source starts disciplining the clock.
func NewPPS(cfg PPSConfig, clk clock.Clock, log *slog.Logger) (*PPS, error) {
	if err := defaultAndValidatePPS(&cfg, clk); err != nil {
		return nil, err
	}
	opener := func() (pps.Reader, error) { return pps.Open(cfg.Device, cfg.Edge) }
	r, err := opener()
	if err != nil {
		return nil, fmt.Errorf("refclock %q: %w", cfg.Name, err)
	}
	return newPPS(cfg, clk, log, r, opener), nil
}

func newPPS(cfg PPSConfig, clk clock.Clock, log *slog.Logger, r pps.Reader, opener func() (pps.Reader, error)) *PPS {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &PPS{
		cfg: cfg, clk: clk, log: log.With("source", cfg.Name),
		reader: r, opener: opener, poll: cfg.PollMin,
		window:    make([]float64, 0, 1<<cfg.PollMax),
		intervals: make([]float64, 0, 1<<cfg.PollMax),
	}
	p.info.Store(&source.Info{
		Name: cfg.Name, Address: cfg.Device, Poll: cfg.PollMin,
		Refclock: &source.RefclockInfo{Type: "pps", Device: cfg.Device, Edge: cfg.Edge.String()},
	})
	return p
}

func defaultAndValidatePPS(cfg *PPSConfig, clk clock.Clock) error {
	if cfg.Name == "" {
		cfg.Name = "pps"
	}
	if cfg.Device == "" {
		return fmt.Errorf("refclock %q: device is empty", cfg.Name)
	}
	if cfg.Edge != pps.Assert && cfg.Edge != pps.Clear {
		return fmt.Errorf("refclock %q: invalid edge %d", cfg.Name, cfg.Edge)
	}
	if cfg.LockJitter == 0 {
		cfg.LockJitter = defaultLockJitter
	}
	if !finite(cfg.Offset) {
		return fmt.Errorf("refclock %q: offset must be finite", cfg.Name)
	}
	if !finite(cfg.LockJitter) || cfg.LockJitter <= 0 {
		return fmt.Errorf("refclock %q: lock_jitter must be greater than zero", cfg.Name)
	}
	if cfg.PollMin == 0 {
		cfg.PollMin = defaultPPSPollMin
	}
	if cfg.PollMax == 0 {
		cfg.PollMax = defaultPPSPollMax
	}
	if cfg.PollMin < discipline.MinPoll || cfg.PollMax > discipline.MaxPoll || cfg.PollMin > cfg.PollMax {
		return fmt.Errorf("refclock %q: poll range %d..%d is invalid", cfg.Name, cfg.PollMin, cfg.PollMax)
	}
	if clk == nil {
		return fmt.Errorf("refclock %q: nil clock", cfg.Name)
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Name implements source.Source.
func (p *PPS) Name() string { return p.cfg.Name }

// Info implements source.Source.
func (p *PPS) Info() source.Info { return *p.info.Load() }

// Reset implements source.Source. Reach is intentionally retained, while
// samples captured before a clock step are discarded.
func (p *PPS) Reset() { p.resetRequested.Store(true) }

// Run implements source.Source.
func (p *PPS) Run(ctx context.Context, out chan<- discipline.Measurement) error {
	defer p.closeReader()
	for {
		if p.resetRequested.Swap(false) {
			p.resetWindow()
		}
		s, err := p.reader.Fetch(defaultFetchTimeout)
		switch {
		case err == nil:
			m := p.accept(s)
			if !p.emit(ctx, out, m) {
				return nil
			}
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, pps.ErrTimeout):
			p.timeout()
			if !p.emit(ctx, out, p.emptyMeasurement()) {
				return nil
			}
		default:
			p.updateInfo(func(i *source.Info, r *source.RefclockInfo) { i.LastError = err.Error() })
			p.log.Warn("PPS device unavailable", "error", err)
			if err := p.reopen(ctx); err != nil {
				return err
			}
			if ctx.Err() != nil {
				return nil
			}
		}
	}
}

func (p *PPS) reopen(ctx context.Context) error {
	p.closeReader()
	if p.opener == nil {
		return fmt.Errorf("refclock %q: PPS reader stopped and cannot be reopened", p.cfg.Name)
	}
	backoff := time.Second
	for {
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		r, err := p.opener()
		if err == nil {
			p.reader = r
			p.reach = 0
			p.pendingMisses = 0
			p.havePrevious = false
			p.resetWindow()
			p.updateInfo(func(i *source.Info, r *source.RefclockInfo) { i.LastError = ""; i.Reach = 0 })
			p.log.Info("PPS device reopened")
			return nil
		}
		p.updateInfo(func(i *source.Info, r *source.RefclockInfo) { i.LastError = err.Error() })
		if backoff < time.Minute {
			backoff *= 2
			if backoff > time.Minute {
				backoff = time.Minute
			}
		}
	}
}

func (p *PPS) closeReader() {
	if p.reader != nil {
		_ = p.reader.Close()
		p.reader = nil
	}
}

func (p *PPS) timeout() {
	wasReachable := p.reach != 0
	p.reach <<= 1
	p.pendingMisses++
	p.slots++
	if wasReachable && p.reach == 0 {
		p.resetWindow()
		p.log.Warn("PPS pulse lost")
	}
	p.updateInfo(func(i *source.Info, r *source.RefclockInfo) {
		i.Reach = p.reach
		i.Timeouts++
		i.LastError = "PPS timeout"
		r.Timeouts++
	})
}

// accept validates one edge and returns a measurement. Invalid samples still
// produce an empty measurement so reachability changes are visible at once.
func (p *PPS) accept(s pps.Sample) discipline.Measurement {
	if p.resetRequested.Swap(false) {
		p.resetWindow()
	}
	m := p.emptyMeasurement()
	if s.Sequence == 0 || s.Time.IsZero() {
		p.reject("glitch", &m)
		return m
	}
	delta := uint32(1)
	interval := 0.0
	if p.havePrevious {
		delta = s.Sequence - p.previousSeq
		if delta == 0 {
			p.reject("glitch", &m)
			return m
		}
		interval = s.Time.Sub(p.previousTime).Seconds()
		if math.Abs(interval-float64(delta)) > maxPulseIntervalSkew.Seconds() {
			p.accountSequence(s, delta, interval, false)
			p.reject("glitch", &m)
			return m
		}
	}
	p.accountSequence(s, delta, interval, true)

	theta := edgeOffset(s.Time)
	if len(p.window) >= 4 {
		med, mad := medianMAD(p.windowTail(1 << p.poll))
		limit := math.Max(5*mad, spikeFloor)
		if math.Abs(theta-med) > limit {
			p.reject("spike", &m)
			return m
		}
	}
	wasUnreachable := p.reach == 0
	p.reach = p.reach<<1 | 1
	if wasUnreachable && p.haveEverPulse {
		p.log.Info("PPS pulse returned")
	}
	p.haveEverPulse = true
	m.Reach = p.reach
	p.appendWindow(theta)
	p.updateInfo(func(i *source.Info, r *source.RefclockInfo) {
		i.Reach = p.reach
		i.LastRx = s.Time
		i.LastError = ""
		i.Received++
		r.Sequence = s.Sequence
		r.LastPulse = s.Time
		r.Samples++
	})

	if p.slots < 1<<p.poll {
		return m
	}
	p.slots = 0
	values := p.windowTail(1 << p.poll)
	median, mad := medianMAD(values)
	sigma := mad * 1.4826
	stable := ppsWindowStable(p.stable, len(values), sigma, p.cfg.LockJitter)
	if stable != p.stable {
		if stable {
			p.log.Info("PPS window stable", "jitter", sigma, "samples", len(values))
		} else {
			p.log.Warn("PPS window unstable", "jitter", sigma, "limit", p.cfg.LockJitter)
		}
		p.stable = stable
	}
	precision := ntp.Log2Seconds(p.clk.Precision())
	m.Valid = stable
	m.Invalidate = !stable
	m.At = p.clk.Monotonic()
	m.Offset = median + p.cfg.Offset
	m.Delay = 0
	m.Dispersion = sigma + precision
	m.Jitter = math.Max(sigma, precision)
	m.Leap = ntp.LeapNone
	m.Stratum = 0
	m.RefID = ntp.RefIDFromString("PPS")
	m.SourceRefID = m.RefID
	m.Precision = p.clk.Precision()
	m.RefTime = s.Time
	p.poll = adaptPoll(p.poll, m.Offset, m.Jitter, p.cfg.PollMin, p.cfg.PollMax)
	m.Poll = p.poll
	p.updateInfo(func(i *source.Info, r *source.RefclockInfo) {
		i.Poll = p.poll
		i.Offset = m.Offset
		i.Delay = 0
		i.Dispersion = m.Dispersion
		i.Jitter = m.Jitter
		i.Stratum = 0
		i.RefID = m.RefID
		i.Leap = m.Leap
		r.WindowSamples = len(values)
		r.WindowJitter = sigma
		r.IntervalJitter = intervalJitter(p.intervals)
		r.Stable = stable
	})
	return m
}

func (p *PPS) accountSequence(s pps.Sample, delta uint32, interval float64, validInterval bool) {
	misses := int(delta - 1)
	additional := misses - p.pendingMisses
	if additional > 0 {
		shiftReach(&p.reach, additional)
		p.slots += additional
	}
	if misses > 0 {
		p.updateInfo(func(i *source.Info, r *source.RefclockInfo) { r.Gaps += uint64(misses) })
	}
	p.pendingMisses = 0
	p.slots++
	if p.havePrevious && delta > 0 && validInterval {
		perPulse := interval / float64(delta)
		p.appendInterval(perPulse - 1)
		p.updateInfo(func(i *source.Info, r *source.RefclockInfo) {
			r.LastInterval = perPulse
		})
	}
	p.previousSeq = s.Sequence
	p.previousTime = s.Time
	p.havePrevious = true
}

func (p *PPS) reject(kind string, m *discipline.Measurement) {
	p.reach <<= 1
	m.Reach = p.reach
	p.updateInfo(func(i *source.Info, r *source.RefclockInfo) {
		i.Reach = p.reach
		switch kind {
		case "spike":
			r.Spikes++
		case "glitch":
			r.Glitches++
		}
	})
}

func (p *PPS) emptyMeasurement() discipline.Measurement {
	return discipline.Measurement{Source: p.cfg.Name, Now: p.clk.Monotonic(), Reach: p.reach, Poll: p.poll}
}

func (p *PPS) emit(ctx context.Context, out chan<- discipline.Measurement, m discipline.Measurement) bool {
	m.Now = p.clk.Monotonic()
	m.Reach = p.reach
	m.Poll = p.poll
	select {
	case out <- m:
		return true
	case <-ctx.Done():
		return false
	}
}

func (p *PPS) resetWindow() {
	p.window = p.window[:0]
	p.intervals = p.intervals[:0]
	p.slots = 0
	p.stable = false
	p.updateInfo(func(i *source.Info, r *source.RefclockInfo) {
		r.WindowSamples = 0
		r.WindowJitter = 0
		r.IntervalJitter = 0
		r.Stable = false
	})
}

func (p *PPS) appendWindow(v float64) {
	max := 1 << p.cfg.PollMax
	if len(p.window) == max {
		copy(p.window, p.window[1:])
		p.window[len(p.window)-1] = v
		return
	}
	p.window = append(p.window, v)
}

func (p *PPS) appendInterval(v float64) {
	max := 1 << p.cfg.PollMax
	if len(p.intervals) == max {
		copy(p.intervals, p.intervals[1:])
		p.intervals[len(p.intervals)-1] = v
		return
	}
	p.intervals = append(p.intervals, v)
}

func (p *PPS) windowTail(n int) []float64 {
	if n > len(p.window) {
		n = len(p.window)
	}
	return p.window[len(p.window)-n:]
}

func edgeOffset(t time.Time) float64 {
	frac := float64(t.Nanosecond()) / 1e9
	if frac < 0.5 {
		return -frac
	}
	return 1 - frac
}

func medianMAD(values []float64) (median, mad float64) {
	if len(values) == 0 {
		return 0, 0
	}
	v := slices.Clone(values)
	slices.Sort(v)
	median = middle(v)
	for i := range v {
		v[i] = math.Abs(values[i] - median)
	}
	slices.Sort(v)
	return median, middle(v)
}

func middle(v []float64) float64 {
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

func intervalJitter(values []float64) float64 {
	_, mad := medianMAD(values)
	return mad * 1.4826
}

func ppsWindowStable(wasStable bool, samples int, sigma, limit float64) bool {
	if samples < 4 {
		return false
	}
	if wasStable {
		return sigma <= 4*limit
	}
	return sigma < limit
}

func adaptPoll(poll int8, offset, jitter float64, min, max int8) int8 {
	if math.Abs(offset) < 4*jitter {
		poll++
	} else {
		poll--
	}
	if poll < min {
		return min
	}
	if poll > max {
		return max
	}
	return poll
}

func shiftReach(reach *uint8, n int) {
	if n >= 8 {
		*reach = 0
		return
	}
	*reach <<= n
}

func (p *PPS) updateInfo(f func(*source.Info, *source.RefclockInfo)) {
	cur := *p.info.Load()
	r := *cur.Refclock
	f(&cur, &r)
	cur.Refclock = &r
	p.info.Store(&cur)
}

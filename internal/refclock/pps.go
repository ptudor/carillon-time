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
	defaultMaxSlewPPM    = 500

	// spikeFitMin is the number of accepted offsets below which no spike
	// test runs at all: the trend line needs a few points before it means
	// anything, and a freshly primed window must be allowed to fill.
	spikeFitMin = 4
	// spikeFitMax bounds the trend line to the recent past so that a slew
	// that changes rate is tracked rather than averaged away.
	spikeFitMax = 8
	// spikeSlewPulses caps the slew allowance the gate grants for missed
	// pulses. Beyond the depth of the reach register the source is
	// unreachable and the window is re-primed anyway, so letting the
	// allowance grow without bound would only open a hole.
	spikeSlewPulses = 8
	// maxSequenceGap is the largest run of missed pulses treated as a gap.
	// A larger jump is a device-side counter restart (ldattach restarting,
	// a /dev/ppsN recreated under the same name, another process issuing
	// PPS_IOC_DESTROY/CREATE on the same tty), not an hour of silence: the
	// unsigned delta then reads as about 2^32, which would add four billion
	// to a monotonic Gaps counter that could never look right again and
	// overflow the emit cadence.
	maxSequenceGap = 3600

	// spikeResetAfter is the run of consecutive rejections that re-primes
	// the window. A rejected pulse never enters the window, so without this
	// a window that has drifted away from the signal can never recover.
	spikeResetAfter = 4
)

// PPSConfig configures a bare PPS reference clock.
type PPSConfig struct {
	Name       string
	Type       string
	Device     string
	Edge       pps.Edge
	Offset     float64
	LockJitter float64
	PollMin    int8
	PollMax    int8
	// MaxSlewPPM is the discipline loop's phase-slew ceiling. The spike
	// gate widens its limit by the phase the loop can legitimately move
	// between two pulses; without it the daemon's own correction of the
	// offset the PPS reported looks like a spike.
	MaxSlewPPM float64

	// Generation returns the engine's measurement epoch. It is read before
	// each fetch and again when the edge arrives; a change means the clock
	// was stepped while we waited, so the kernel's timestamp for that edge
	// predates the step. Nil disables the check.
	Generation func() uint64

	// OnPulse receives every accepted edge as an immutable record. It must
	// return promptly: it runs on the refclock's own goroutine, between one
	// pulse and the next.
	OnPulse func(source.Pulse)
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
	windowSeq     []uint32
	intervals     []float64
	slots         int
	pendingMisses int
	havePrevious  bool
	previousSeq   uint32
	previousTime  time.Time
	stable        bool
	haveEverPulse bool
	rejectRun     int

	// pendingInvalidate is armed by resetWindow and carried on the next
	// emitted measurement, so the selector drops the estimate this source
	// no longer has.
	pendingInvalidate bool
	staleSeen         uint64
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
		windowSeq: make([]uint32, 0, 1<<cfg.PollMax),
		intervals: make([]float64, 0, 1<<cfg.PollMax),
	}
	p.info.Store(&source.Info{
		Name: cfg.Name, Address: cfg.Device, Poll: cfg.PollMin,
		Refclock: &source.RefclockInfo{Type: cfg.Type, Device: cfg.Device, Edge: cfg.Edge.String()},
	})
	return p
}

func defaultAndValidatePPS(cfg *PPSConfig, clk clock.Clock) error {
	if cfg.Name == "" {
		cfg.Name = "pps"
	}
	if cfg.Type == "" {
		cfg.Type = "pps"
	}
	if cfg.Type != "pps" && cfg.Type != "gps-pps" {
		return fmt.Errorf("refclock %q: invalid PPS type %q", cfg.Name, cfg.Type)
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
	if cfg.MaxSlewPPM == 0 {
		cfg.MaxSlewPPM = defaultMaxSlewPPM
	}
	if !finite(cfg.MaxSlewPPM) || cfg.MaxSlewPPM <= 0 {
		return fmt.Errorf("refclock %q: max_slew_ppm must be greater than zero", cfg.Name)
	}
	if clk == nil {
		return fmt.Errorf("refclock %q: nil clock", cfg.Name)
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// generation reads the engine's measurement epoch, or 0 when the source was
// built without one.
func (p *PPS) generation() uint64 {
	if p.cfg.Generation == nil {
		return 0
	}
	return p.cfg.Generation()
}

// Name implements source.Source.
func (p *PPS) Name() string { return p.cfg.Name }

// Info implements source.Source.
func (p *PPS) Info() source.Info { return *p.info.Load() }

// Reset implements source.Source. Reach is intentionally retained, while
// samples captured before a clock step are discarded.
func (p *PPS) Reset() { p.resetRequested.Store(true) }

// Close releases the PPS device before Run starts. Run owns closure after it
// has started; Close principally supports unwinding a multi-source GPS setup.
func (p *PPS) Close() error {
	if p == nil || p.reader == nil {
		return nil
	}
	err := p.reader.Close()
	p.reader = nil
	return err
}

// Run implements source.Source.
func (p *PPS) Run(ctx context.Context, out chan<- discipline.Measurement) error {
	defer p.closeReader()
	for {
		if p.resetRequested.Swap(false) {
			p.resetWindow()
		}
		gen := p.generation()
		s, err := p.reader.Fetch(defaultFetchTimeout)
		switch {
		case err == nil:
			// The pulse is usable only if the epoch was the same settled
			// value before and after the fetch. An unchanged but odd epoch
			// means a discontinuity was executing throughout, so the
			// kernel's timestamp straddles it even though the counter did
			// not move (RA6X-006).
			if now := p.generation(); now != gen || (now != 0 && !source.StableEpoch(now)) {
				// The clock was stepped while we waited for this edge, so
				// the kernel's timestamp for it is on the wrong side of the
				// step. Drop the pulse rather than let it into the window.
				p.discardStale(now)
				if !p.emit(ctx, out, p.emptyMeasurement()) {
					return nil
				}
				continue
			}
			m := p.accept(s)
			m.Generation = gen
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
			// A hard device error is a loss, not a missed pulse: report it
			// before disappearing into the reconnect loop, or the engine
			// hears nothing at all while the device is gone and a
			// disconnected PPS stays system source indefinitely (RA6X-004).
			p.updateInfo(func(i *source.Info, r *source.RefclockInfo) { i.LastError = err.Error() })
			p.log.Warn("PPS device unavailable", "error", err)
			p.deviceLost()
			if !p.emit(ctx, out, p.emptyMeasurement()) {
				return nil
			}
			if err := p.reopen(ctx); err != nil {
				return err
			}
			if ctx.Err() != nil {
				return nil
			}
		}
	}
}

// discardStale drops a pulse whose kernel timestamp predates a clock step.
// The edge itself happened, so reach is unaffected; only the offset is
// unusable, and the window is dropped because everything in it is too.
func (p *PPS) discardStale(now uint64) {
	p.staleSeen++
	p.resetWindow()
	p.updateInfo(func(i *source.Info, r *source.RefclockInfo) { i.Stale++ })
	p.log.Info("discarding a pulse that spans a clock step", "count", p.staleSeen, "generation", now)
}

// deviceLost revokes everything derived from a device that has gone away:
// reach empties, the estimate is invalidated, and the sequence and window
// state is cleared so the replacement device has to prime and qualify from
// scratch. LastError is left as the caller set it, so health keeps explaining
// the outage while the reconnect loop runs.
func (p *PPS) deviceLost() {
	p.reach = 0
	p.pendingMisses = 0
	p.havePrevious = false
	p.resetWindow()
	p.updateInfo(func(i *source.Info, r *source.RefclockInfo) { i.Reach = 0 })
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
	if p.reach == 0 {
		// Re-prime on every unreachable fetch, not only on the transition
		// into it: a window kept across a silent period is a window that
		// predates whatever the clock has done since.
		p.resetWindow()
		if wasReachable {
			p.log.Warn("PPS pulse lost")
		}
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
		if delta > maxSequenceGap {
			// The kernel-side counter restarted. Treat it as a reopen:
			// forget the previous sample rather than book four billion
			// missed pulses.
			p.sequenceRestart(s)
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
	if predicted, limit, ok := p.spikeGate(s.Sequence); ok && math.Abs(theta-predicted) > limit {
		p.reject("spike", &m)
		return m
	}
	p.rejectRun = 0
	// The edge passed sequence, interval and spike checks: this is a
	// successful acquisition even if the window is not yet deep enough for
	// the measurement to be Valid (RA6X-010).
	m.Acquired = true
	wasUnreachable := p.reach == 0
	p.reach = p.reach<<1 | 1
	if wasUnreachable && p.haveEverPulse {
		p.log.Info("PPS pulse returned")
	}
	p.haveEverPulse = true
	if p.cfg.OnPulse != nil {
		p.cfg.OnPulse(source.Pulse{
			Source: p.cfg.Name, At: s.Time, Offset: theta + p.cfg.Offset, Sequence: s.Sequence,
		})
	}
	m.Reach = p.reach
	p.appendWindow(s.Sequence, theta)
	p.updateInfo(func(i *source.Info, r *source.RefclockInfo) {
		i.Reach = p.reach
		i.LastRx = s.Time
		i.LastError = ""
		i.Received++
		r.Sequence = s.Sequence
		r.LastPulse = s.Time
		r.LastOffset = theta + p.cfg.Offset
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

// sequenceRestart handles a PPS sequence counter that jumped or went
// backwards, which means the device was re-created under us. Everything
// derived from the old counter is discarded and the next pulse starts a fresh
// train; the normal gap accounting is left alone.
func (p *PPS) sequenceRestart(s pps.Sample) {
	p.log.Warn("PPS sequence counter restarted; re-priming",
		"previous", p.previousSeq, "sequence", s.Sequence)
	p.havePrevious = false
	p.pendingMisses = 0
	p.resetWindow()
	p.previousSeq, p.previousTime = s.Sequence, s.Time
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
	// A rejected pulse never enters the window, so a window that has stopped
	// describing the signal would otherwise reject everything for ever. Give
	// up on it well before the reach register empties and re-prime.
	p.rejectRun++
	if p.rejectRun >= spikeResetAfter {
		p.rejectRun = 0
		p.resetWindow()
		p.log.Warn("PPS window re-primed after consecutive rejections", "rejections", spikeResetAfter)
	}
}

func (p *PPS) emptyMeasurement() discipline.Measurement {
	return discipline.Measurement{
		Source: p.cfg.Name, Now: p.clk.Monotonic(), Reach: p.reach, Poll: p.poll,
		Generation: p.generation(),
	}
}

func (p *PPS) emit(ctx context.Context, out chan<- discipline.Measurement, m discipline.Measurement) bool {
	m.Now = p.clk.Monotonic()
	m.Reach = p.reach
	m.Poll = p.poll
	if p.pendingInvalidate {
		// Carry exactly one invalidation per reset: the estimate the
		// selector holds for this source is gone until the window primes
		// again.
		m.Invalidate = true
		p.pendingInvalidate = false
	}
	if !m.Valid {
		// Nothing was measured, so nothing is tied to a clock reading.
		m.Generation = p.generation()
	}
	select {
	case out <- m:
		return true
	case <-ctx.Done():
		return false
	}
}

// resetWindow empties the offset window and arms an invalidation: every local
// reset here revokes the estimate the selector is holding, so the next
// measurement must say so. Reporting the reset only through reach left the
// previous estimate valid in SourceState, and an unlocked PPS with a nonzero
// reach stayed selected on the strength of a window it no longer has
// (RA6X-005).
func (p *PPS) resetWindow() {
	p.pendingInvalidate = true
	p.window = p.window[:0]
	p.windowSeq = p.windowSeq[:0]
	p.intervals = p.intervals[:0]
	p.slots = 0
	p.stable = false
	p.rejectRun = 0
	p.updateInfo(func(i *source.Info, r *source.RefclockInfo) {
		r.WindowSamples = 0
		r.WindowJitter = 0
		r.IntervalJitter = 0
		r.Stable = false
	})
}

func (p *PPS) appendWindow(seq uint32, v float64) {
	max := 1 << p.cfg.PollMax
	if len(p.window) == max {
		copy(p.window, p.window[1:])
		p.window[len(p.window)-1] = v
		copy(p.windowSeq, p.windowSeq[1:])
		p.windowSeq[len(p.windowSeq)-1] = seq
		return
	}
	p.window = append(p.window, v)
	p.windowSeq = append(p.windowSeq, seq)
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

// spikeGate predicts where the next edge offset should fall and returns the
// distance beyond which it is treated as a spike. The prediction is a
// least-squares line through the recent accepted offsets rather than their
// median, because the discipline loop slews the local clock by up to
// MaxSlewPPM while the PPS is the system source: a median of past offsets is
// exactly as stale as the correction the daemon itself is applying, and
// comparing against it rejects every pulse once a correction starts. The
// limit is the residual scatter of that line widened by the phase the loop is
// entitled to move between the last accepted pulse and this one.
//
// ok is false while the window is too short to fit, which is what lets a
// re-primed window fill again.
func (p *PPS) spikeGate(seq uint32) (predicted, limit float64, ok bool) {
	n := len(p.window)
	if n < spikeFitMin || len(p.windowSeq) != n {
		return 0, 0, false
	}
	if n > spikeFitMax {
		n = spikeFitMax
	}
	xs := p.windowSeq[len(p.windowSeq)-n:]
	ys := p.window[len(p.window)-n:]

	// Offsets are taken relative to the most recent sample so the sums stay
	// small and the uint32 sequence counter's wrap is handled by int32
	// arithmetic.
	last := xs[n-1]
	fn := float64(n)
	var sx, sy, sxx, sxy float64
	for i := range ys {
		x := float64(int32(xs[i] - last))
		sx += x
		sy += ys[i]
		sxx += x * x
		sxy += x * ys[i]
	}
	den := fn*sxx - sx*sx
	if den == 0 {
		return 0, 0, false
	}
	slope := (fn*sxy - sx*sy) / den
	intercept := (sy - slope*sx) / fn

	residuals := make([]float64, n)
	for i := range ys {
		residuals[i] = ys[i] - (intercept + slope*float64(int32(xs[i]-last)))
	}
	_, mad := medianMAD(residuals)

	ahead := float64(int32(seq - last))
	if ahead < 1 {
		ahead = 1
	}
	if ahead > spikeSlewPulses {
		ahead = spikeSlewPulses
	}
	predicted = intercept + slope*float64(int32(seq-last))
	limit = math.Max(5*mad, spikeFloor) + p.cfg.MaxSlewPPM*1e-6*ahead
	return predicted, limit, true
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

// reachBits is the width of the reach register both refclocks keep: eight
// slots, so a gap of eight or more empties it.
const reachBits = 8

func shiftReach(reach *uint8, n int) {
	if n >= reachBits {
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

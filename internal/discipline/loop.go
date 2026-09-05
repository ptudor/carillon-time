package discipline

import "math"

// Loop constants. The structure follows RFC 5905 §11.3 / ntpd's loop
// filter; the gains do not (see LoopConfig.TimeConstant).
const (
	// DefaultTimeConstant is the default ratio of the loop time constant to
	// the poll interval. ntpd uses 16 with an integral gain a quarter of
	// critical, which leaves a slow mode with a time constant of ~15τ
	// (hours at poll 6); this loop is critically damped, so both modes
	// settle in ~2τ, and 4 keeps τ well above the poll interval for
	// discrete-time stability.
	DefaultTimeConstant = 4

	// spikeGate is ntpd's CLOCK_SGATE: an offset change beyond this many
	// times the clock jitter, arriving sooner than two poll intervals after
	// the previous update, is a popcorn spike and is ignored.
	spikeGate = 3

	// jitterAverage is ntpd's CLOCK_AVG, the exponential averaging weight
	// of the clock jitter estimate.
	jitterAverage = 8

	// maxTickInterval bounds the elapsed time a single Tick will account
	// for. The engine's ticker can be late — GC, a VM pause, a slow
	// drift-file write — and the kernel keeps running at the transient for
	// the whole delay, so the phase must be debited for the real interval
	// rather than a nominal second. Beyond this the delay is a stall of
	// unknown length; charging one second is the conservative choice,
	// because over-debiting the phase leaves a correction that was never
	// applied believed to be done.
	maxTickInterval = 2.0
)

// LoopConfig holds the operator-tunable parts of the loop.
type LoopConfig struct {
	// StepThreshold is the offset beyond which the clock is stepped rather
	// than slewed, subject to StepLimit.
	StepThreshold float64

	// StepLimit bounds stepping: -1 always allowed, 0 never, N only within
	// the first N loop updates.
	StepLimit int

	// Panic is the offset beyond which no correction is attempted at all,
	// unless PanicAtStartup permits it for the very first correction.
	Panic          float64
	PanicAtStartup bool

	// MaxSlewPPM bounds the rate at which phase is slewed.
	MaxSlewPPM float64

	// Precision is the clock resolution in seconds; the floor for jitter.
	Precision float64

	// TimeConstant is the loop time constant as a multiple of the poll
	// interval; zero selects DefaultTimeConstant.
	TimeConstant float64

	// FreqMeasure is the length in seconds of the initial direct frequency
	// measurement performed when no frequency is known at start (ntpd's
	// FREQ state). Zero disables it and the PLL learns from scratch.
	FreqMeasure float64
}

// ActionKind identifies an actuator operation requested by the loop.
type ActionKind int

const (
	// ActionSetFrequency sets the kernel frequency word; Value is ppm.
	ActionSetFrequency ActionKind = iota
	// ActionStep steps the clock; Value is seconds to add.
	ActionStep
	// ActionResetFilters tells every source to discard its samples.
	ActionResetFilters
)

// Action is one actuator operation.
type Action struct {
	Kind  ActionKind
	Value float64
}

// UpdateResult reports what a loop update decided.
type UpdateResult struct {
	Actions      []Action
	Stepped      bool
	Ignored      bool // popcorn spike
	Deferred     bool // a step was wanted but the caller withheld permission
	PanicRefused bool
	Mu           float64 // seconds since the previous update
}

// Loop is the clock discipline loop: a critically damped type-II
// phase-locked loop driving the kernel through a frequency word and, for
// phase, a per-second transient on that word.
//
// With τ the time constant, the phase is slewed at θ/τ per second and the
// frequency integrates at θ/(4τ²) per second, which places both closed-loop
// poles at −1/(2τ). While the phase slew is saturated at MaxSlewPPM the
// frequency is not integrated, so a large slew cannot wind the integrator
// up.
type Loop struct {
	cfg LoopConfig

	// Freq is the current frequency correction (ppm); FreqKnown is false
	// until it has been measured or loaded.
	Freq      float64
	FreqKnown bool

	// Pending is the residual phase (seconds) still to be slewed.
	Pending float64

	// Jitter is the clock jitter estimate: an exponential average of the
	// offset change between updates.
	Jitter float64

	Updates int
	Steps   int

	lastUpdate  float64
	lastOffset  float64
	tau         float64
	applied     float64 // frequency word most recently issued
	appliedBase float64 // the base frequency that word was computed against
	haveApply   bool
	chargeFrom  float64 // monotonic time from which `applied` is unaccounted
	haveTick    bool
	skipJitter  bool // the next update follows a step; lastOffset means nothing

	// Bootstrap frequency measurement state.
	firstTime   float64
	firstOffset float64
	slewed      float64

	// slewLog is the cumulative phase this loop has applied, sampled at
	// every accounting point, newest last. It answers "how much of an
	// observation's offset has this loop already corrected since that
	// observation was taken", which is what makes a historical filter
	// output usable as present-time feedback (RA6X-001). The word is
	// constant between accounting points, so interpolating within one is
	// exact.
	slewLog []slewPoint
	cumSlew float64
}

// slewPoint is the cumulative applied phase at one accounting point.
type slewPoint struct {
	at  float64
	cum float64
}

// slewLogSpan bounds the history kept. Nothing older than the Allan intercept
// can win the clock filter's ranking, so an observation cannot need more.
const slewLogSpan = 2 * AllanIntercept

// NewLoop returns a loop starting from the given frequency; known reports
// whether that value came from a drift file or the kernel rather than being
// a guess.
func NewLoop(cfg LoopConfig, freq float64, known bool) *Loop {
	if cfg.TimeConstant <= 0 {
		cfg.TimeConstant = DefaultTimeConstant
	}
	return &Loop{
		cfg:       cfg,
		Freq:      clampFreq(freq),
		FreqKnown: known,
		Jitter:    cfg.Precision,
		tau:       cfg.TimeConstant * math.Ldexp(1, 6),
	}
}

func clampFreq(f float64) float64 {
	return math.Max(-MaxFrequency, math.Min(MaxFrequency, f))
}

// stepAllowed applies the StepLimit policy. It gates the panic-at-startup
// exception too: an operator who configured limit = 0 has said the clock is
// never to be stepped, and no offset magnitude changes that.
func (l *Loop) stepAllowed() bool {
	switch {
	case l.cfg.StepLimit < 0:
		return true
	case l.cfg.StepLimit == 0:
		return false
	default:
		return l.Updates < l.cfg.StepLimit
	}
}

// Update feeds the loop a new system offset measured with the given poll
// exponent at monotonic time now. synced enables the popcorn spike gate.
//
// mayStep is the caller's veto on the step policy: the System withholds it
// when a step would rest on a single post-step sample (see reselect). A
// withheld step is reported as Deferred and changes no loop state at all —
// it consumes neither the step budget nor an update — so the decision is
// simply retaken when more evidence arrives.
func (l *Loop) Update(offset float64, poll int8, now float64, synced, mayStep bool) UpdateResult {
	var u UpdateResult
	abs := math.Abs(offset)

	// Step and panic policy. Precedence, in order:
	//
	//  1. Beyond Panic: refuse, unless this is the very first update and
	//     PanicAtStartup is set.
	//  2. The startup exception still obeys StepLimit. limit = 0 means never
	//     step, and a configuration that says never must not be overridden
	//     by a threshold that only says "this is a big offset". Refusing is
	//     the right answer, not converting thousands of seconds into a slew
	//     that would take weeks (RA6X-011).
	//  3. The caller's mayStep veto defers rather than refuses: no state
	//     changes and the decision is retaken with more evidence.
	if abs > l.cfg.Panic {
		if !(l.Updates == 0 && l.cfg.PanicAtStartup) || !l.stepAllowed() {
			u.PanicRefused = true
			return u
		}
		if !mayStep {
			u.Deferred = true
			return u
		}
		return l.step(offset, now)
	}
	if abs > l.cfg.StepThreshold && l.stepAllowed() {
		if !mayStep {
			u.Deferred = true
			return u
		}
		return l.step(offset, now)
	}

	if l.Updates > 0 {
		u.Mu = now - l.lastUpdate
	}
	tau := l.cfg.TimeConstant * math.Ldexp(1, int(poll))

	// Popcorn spike suppressor.
	if synced && l.Updates > 0 {
		if math.Abs(offset-l.lastOffset) > spikeGate*l.Jitter && u.Mu < 2*math.Ldexp(1, int(poll)) {
			u.Ignored = true
			return u
		}
	}

	// Clock jitter: exponential average of the offset change.
	d := math.Max(math.Abs(offset-l.lastOffset), l.cfg.Precision)
	switch {
	case l.Updates == 0:
		// Seed from the precision floor and average the first sample in,
		// as ntpd does. Taking the whole initial offset reported 100 ms of
		// jitter for a 100 ms offset and was still 26 ms out twenty updates
		// later, which fed carillon_jitter_seconds, carillonctl tracking
		// and the kernel's esterror.
		l.Jitter = math.Max(l.cfg.Precision, d/math.Sqrt(jitterAverage))
	case l.skipJitter:
		// The previous update was a step, which zeroed lastOffset, so the
		// difference above describes nothing.
		l.skipJitter = false
	default:
		l.Jitter = math.Sqrt(l.Jitter*l.Jitter + (d*d-l.Jitter*l.Jitter)/jitterAverage)
	}

	if l.Updates == 0 {
		// First update: take the offset as pending phase and start the
		// frequency measurement window if needed.
		l.firstTime = now
		l.firstOffset = offset
		l.slewed = 0
	} else {
		switch {
		case !l.FreqKnown && l.cfg.FreqMeasure > 0:
			// Direct measurement: the offset drift over the window, with
			// the phase we slewed meanwhile added back, is the frequency
			// error.
			if elapsed := now - l.firstTime; elapsed >= l.cfg.FreqMeasure {
				l.Freq = clampFreq(l.Freq + (offset-l.firstOffset+l.slewed)/elapsed*1e6)
				l.FreqKnown = true
			}
		case abs > l.cfg.MaxSlewPPM*1e-6*tau:
			// The phase slew will run saturated; integrating the offset
			// meanwhile would only wind the frequency up. Wait for the
			// linear region.
		default:
			mu := math.Min(u.Mu, AllanIntercept)
			l.Freq = clampFreq(l.Freq + offset*mu/(4*tau*tau)*1e6)
			l.FreqKnown = true
		}
	}
	// A new observation *replaces* the residual phase: whatever the loop
	// corrected before the observation was taken is already reflected in
	// the offset it reports. Move the accounting point here so the next
	// tick charges only the interval since, and that correction is not
	// debited a second time (RA6X-008).
	l.chargeApplied(now)
	l.Pending = offset
	l.lastOffset = offset
	l.lastUpdate = now
	l.tau = tau
	l.Updates++
	// No frequency action here. Issuing the base alone would remove the
	// phase transient the last Tick applied, pausing the slew until the
	// next one — about 6 % of the slew capacity with a PPS at poll 4. The
	// next Tick issues base plus the transient for the new pending phase,
	// at most one tick away.
	return u
}

// step performs a step and resets the phase state; the frequency estimate
// is kept, since a step says nothing about rate.
func (l *Loop) step(offset float64, now float64) UpdateResult {
	l.Steps++
	l.Updates++
	// The step supersedes the residual phase entirely. Start a fresh
	// accounting interval, and stop treating the word still in the kernel
	// as a transient: it is left running until the next tick issues one,
	// but there is no longer a residual for it to be charged against, and
	// the post-step observation will report wherever it has taken the
	// clock.
	l.elapsed(now)
	l.appliedBase = l.applied
	// The phase reference is gone, so the record of how much of it has been
	// corrected means nothing to an observation taken after the step.
	l.slewLog, l.cumSlew = l.slewLog[:0], 0
	l.Pending = 0
	l.lastOffset = 0
	l.lastUpdate = now
	l.firstTime = now
	l.firstOffset = 0
	l.slewed = 0
	l.skipJitter = true
	return UpdateResult{
		Actions: []Action{{ActionStep, offset}, {ActionResetFilters, 0}},
		Stepped: true,
	}
}

// Tick runs once per second: it charges the phase the transient already in
// the kernel has moved since the last accounting point, then issues the base
// frequency plus a fresh transient for whatever phase remains. When nothing
// is pending it re-issues the base frequency only if it changed.
//
// The order matters. Charging first, against the word that actually ran, is
// what makes the accounting agree with the kernel: the previous code computed
// the *next* word and debited that instead, so even successive ordinary ticks
// disagreed with the applied-word integral, and an intervening loop update or
// a changed base made the discrepancy larger (RA6X-008).
func (l *Loop) Tick(now float64) []Action {
	l.chargeApplied(now)
	if l.Pending == 0 {
		if !l.haveApply || l.applied != l.Freq {
			l.setApplied(l.Freq)
			return []Action{{ActionSetFrequency, l.Freq}}
		}
		return nil
	}
	adj := l.Pending / l.tau
	maxSlew := l.cfg.MaxSlewPPM * 1e-6
	adj = math.Max(-maxSlew, math.Min(maxSlew, adj))
	if math.Abs(l.Pending) < 1e-9 {
		adj = l.Pending
	}
	total := clampFreq(l.Freq + adj*1e6)
	l.setApplied(total)
	return []Action{{ActionSetFrequency, total}}
}

// chargeApplied debits the phase the currently applied transient has moved
// since the last accounting point, and moves that point to now.
//
// The transient is the difference between the word that was issued and the
// base that was in effect when it was issued — not the current base, which a
// loop update may have changed since. Before anything has been issued there
// is nothing to charge, so the first tick of a run charges nothing rather
// than a nominal second for a transient that never ran.
func (l *Loop) chargeApplied(now float64) {
	dt := l.elapsed(now)
	moved := 0.0
	if l.haveApply && dt > 0 {
		if ran := (l.applied - l.appliedBase) * 1e-6; ran != 0 {
			moved = ran * dt
			l.Pending -= moved
			l.slewed += moved
			if math.Abs(l.Pending) < 1e-12 {
				l.Pending = 0
			}
		}
	}
	l.noteSlew(now, moved)
}

// noteSlew records the cumulative applied phase at an accounting point and
// drops history that can no longer be asked about.
func (l *Loop) noteSlew(now, moved float64) {
	if n := len(l.slewLog); n > 0 && now < l.slewLog[n-1].at {
		return // backward time: nothing ran, and the log must stay ordered
	}
	l.cumSlew += moved
	l.slewLog = append(l.slewLog, slewPoint{at: now, cum: l.cumSlew})
	cut := now - slewLogSpan
	drop := 0
	for drop+1 < len(l.slewLog) && l.slewLog[drop+1].at <= cut {
		drop++
	}
	if drop > 0 {
		l.slewLog = append(l.slewLog[:0], l.slewLog[drop:]...)
	}
}

// cumulativeSlew is the phase this loop had applied, in total, at time t.
// Before the first recorded point it is zero: nothing is known to have been
// applied then.
func (l *Loop) cumulativeSlew(t float64) float64 {
	if len(l.slewLog) == 0 {
		return 0
	}
	first, last := l.slewLog[0], l.slewLog[len(l.slewLog)-1]
	if t <= first.at {
		return first.cum
	}
	if t >= last.at {
		// The currently applied word has been running since the last
		// accounting point, at a constant rate.
		if !l.haveApply {
			return last.cum
		}
		return last.cum + (l.applied-l.appliedBase)*1e-6*(t-last.at)
	}
	lo, hi := 0, len(l.slewLog)-1
	for lo+1 < hi {
		mid := (lo + hi) / 2
		if l.slewLog[mid].at <= t {
			lo = mid
		} else {
			hi = mid
		}
	}
	a, b := l.slewLog[lo], l.slewLog[hi]
	if b.at == a.at {
		return b.cum
	}
	return a.cum + (b.cum-a.cum)*(t-a.at)/(b.at-a.at)
}

// AppliedSince returns the phase this loop has corrected between the two
// times: exactly the amount by which an offset measured at `at` overstates
// the error remaining at `now`.
func (l *Loop) AppliedSince(at, now float64) float64 {
	if now <= at {
		return 0
	}
	return l.cumulativeSlew(now) - l.cumulativeSlew(at)
}

// setApplied records a word the loop is issuing, together with the base it
// was computed against, and starts a fresh accounting interval for it.
func (l *Loop) setApplied(total float64) {
	l.applied, l.appliedBase, l.haveApply = total, l.Freq, true
}

// elapsed returns the seconds to charge, and records now as the accounting
// point. The point is moved by every loop update as well as by every tick:
// an update's offset was measured *after* whatever correction had run up to
// that moment, so charging that interval again would debit it twice.
//
// Backward time charges nothing. An interval longer than maxTickInterval is
// charged at maxTickInterval: the word really did stay in the kernel for the
// whole stall, but a daemon stalled that long has a phase estimate the next
// measurement will replace outright, and an unbounded charge from a
// pathological monotonic jump would swing the residual wildly. Reducing it to
// a nominal second, as this used to, under-charged a real stall instead.
func (l *Loop) elapsed(now float64) float64 {
	dt := 0.0
	if l.haveTick {
		switch d := now - l.chargeFrom; {
		case d < 0:
			dt = 0
		case d > maxTickInterval:
			dt = maxTickInterval
		default:
			dt = d
		}
	}
	l.chargeFrom, l.haveTick = now, true
	return dt
}

// TimeConstant returns the current loop time constant in seconds.
func (l *Loop) TimeConstant() float64 { return l.tau }

// Applied returns the frequency word most recently issued to the actuator —
// the base frequency plus whatever phase-slew transient the last Tick added —
// and whether anything has been issued at all. The caller needs it to know
// what the kernel is actually running at, which is not Freq while a slew is
// in progress.
func (l *Loop) Applied() (float64, bool) { return l.applied, l.haveApply }

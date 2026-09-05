package discipline

import (
	"math"
	"sort"
)

// Filter is the RFC 5905 §10 clock filter: an eight-stage shift register of
// (offset, delay, dispersion, time) samples from which the sample with the
// lowest delay is chosen as the source's current estimate. It is a value
// type used by each source goroutine; it never blocks and never reads a
// clock.
type Filter struct {
	stages [FilterStages]stage
	n      int     // number of samples ever added, capped at FilterStages
	next   int     // ring index for the next sample
	epoch  float64 // time of the most recently *used* sample; older ones are stale
	update float64 // time of the previous Add call, for dispersion aging
	floor  float64 // clock precision, the floor for the jitter estimate
}

type stage struct {
	offset, delay, disp, t float64
}

// Output is the filter's estimate after an Add.
type Output struct {
	// Offset, Delay and Dispersion are those of the chosen (lowest-delay,
	// non-stale) sample, with the dispersion being the whole filter's
	// weighted dispersion.
	Offset     float64
	Delay      float64
	Dispersion float64

	// Jitter is the RMS distance of the other usable samples' offsets from
	// the chosen one, floored at the clock precision.
	Jitter float64

	// At is the time of the chosen sample.
	At float64

	// Samples is the number of usable samples in the register.
	Samples int
}

// NewFilter returns a filter whose jitter estimate is floored at the given
// clock precision (seconds).
func NewFilter(precision float64) *Filter {
	return &Filter{floor: precision}
}

// Add shifts a new sample into the register and returns the current
// estimate. updated is false when the estimate should not replace the
// source's current one: either no sample is usable or the best sample is one
// that has already been used (the new sample had a larger delay than an
// older, already-reported one).
func (f *Filter) Add(offset, delay, disp, now float64) (out Output, updated bool) {
	// Age the dispersion of the existing samples by the time since the
	// previous call, then shift in the new one.
	if f.n > 0 && now > f.update {
		aging := Phi * (now - f.update)
		for i := range f.stages {
			f.stages[i].disp += aging
		}
	}
	f.update = now
	f.stages[f.next] = stage{offset: offset, delay: delay, disp: disp, t: now}
	f.next = (f.next + 1) % FilterStages
	if f.n < FilterStages {
		f.n++
	}

	// Rank the samples by root distance — half the delay plus the
	// dispersion — rather than by raw delay.
	//
	// RFC 5905 §10 and ntpd rank by delay alone, which means a sample's
	// growing dispersion never costs it its place: one unusually fast early
	// reply outranks every later one until the Allan intercept demotes it,
	// and no loop update happens for that whole period. Measured live, that
	// was 42 minutes of blind running (DESIGN.md §6.3). Ranking by distance
	// demotes it as soon as φ·age exceeds *half* its delay advantage —
	// about 11 minutes for a 10 ms advantage — because a delay advantage is
	// worth δ/2 to the offset estimate, not δ.
	//
	// This is a deliberate deviation, made with the simulation pass DESIGN
	// asked for: in the takeover reproduction it takes the maximum age of
	// the selected observation from 448 s to 0 s at poll 6 and from 1792 s
	// to 0 s at poll 8, delivers an update on essentially every poll, and
	// leaves the symmetric cases converging exactly as before (RA6X-002).
	// It is safe only together with the observation-time propagation in
	// Select (RA6X-001): on its own it does not fix delayed feedback.
	//
	// Samples older than the Allan intercept are still pushed behind
	// everything current, and samples whose dispersion has reached
	// MaxDispersion carry no information at all.
	type ranked struct {
		idx  int
		dist float64
	}
	var order []ranked
	for i := 0; i < f.n; i++ {
		s := &f.stages[i]
		var d float64
		switch {
		case s.disp >= MaxDispersion:
			s.disp = MaxDispersion
			d = MaxDispersion
		case now-s.t > AllanIntercept:
			d = MaxDistance + s.disp
		default:
			d = s.delay/2 + s.disp
		}
		order = append(order, ranked{i, d})
	}
	// Rank by distance; among equals prefer the fresher sample so that a
	// steady delay does not pin the estimate to an old sample.
	sort.SliceStable(order, func(a, b int) bool {
		if order[a].dist != order[b].dist {
			return order[a].dist < order[b].dist
		}
		return f.stages[order[a].idx].t > f.stages[order[b].idx].t
	})

	// Usable samples: not MaxDispersion, and once we have two good ones,
	// nothing at or beyond MaxDistance.
	m := 0
	for _, r := range order {
		if r.dist >= MaxDispersion || (m >= 2 && r.dist >= MaxDistance) {
			break
		}
		m++
	}
	if m == 0 {
		return Output{}, false
	}
	best := &f.stages[order[0].idx]

	// Filter dispersion: Σ ε_i · 2^-(i+1) in rank order, plus a priming
	// term for the stages that have never been filled.
	//
	// RFC 5905 initialises the absent stages to MAXDISP (16 s), so a
	// single-sample filter reports about 7.9 s. carillon cannot do that:
	// 7.9 s is past MaxDistance, so a fresh association would be
	// inadmissible until five or six packets had arrived — several poll
	// intervals for anyone without iburst. Omitting the absent stages
	// entirely is the other extreme and is what the code did: one packet
	// with 1 ms of dispersion was reported as 0.5 ms, *more* certain than
	// the single measurement it rests on, which distorted admission,
	// weighting and the root dispersion this host then served (RA6X-024).
	//
	// The policy is the conservative middle the finding allows: absent
	// stages contribute primingDispersion at their rank weight, which is
	// 2^-n of it in total. One sample is therefore reported with at least
	// 500 ms of uncertainty — three orders of magnitude more honest than
	// before — decaying to 2 ms by the eighth. primingDispersion is chosen
	// so that an unprimed source stays admissible on an ordinary path
	// (λ ≈ 0.51 s against a 1.5 s limit), so a host with no other evidence
	// can still bootstrap from it, while any primed source outranks it.
	// Seed the recurrence with what the absent stages would have
	// accumulated: running d = (d + P)/2 over (FilterStages - n) of them
	// from zero gives P·(1 - 2^(n-8)), and the loop below then halves it
	// once per real stage, so the absent stages end up contributing
	// P·(2^-n - 2^-8) in total — the same shape as the RFC's sum, with a
	// bounded P in place of MAXDISP.
	var dispersion, jitter float64
	if f.n < FilterStages {
		dispersion = primingDispersion * (1 - math.Ldexp(1, f.n-FilterStages))
	}
	for i := len(order) - 1; i >= 0; i-- {
		s := &f.stages[order[i].idx]
		dispersion = 0.5 * (dispersion + s.disp)
		if i < m && i > 0 {
			d := s.offset - best.offset
			jitter += d * d
		}
	}
	if m > 1 {
		jitter = math.Sqrt(jitter / float64(m-1))
	}
	jitter = math.Max(jitter, f.floor)

	out = Output{
		Offset:     best.offset,
		Delay:      best.delay,
		Dispersion: dispersion,
		Jitter:     jitter,
		At:         best.t,
		Samples:    m,
	}
	if best.t <= f.epoch {
		return out, false // the best sample has been reported before
	}
	f.epoch = best.t
	return out, true
}

// Reset empties the register, as after a clock step.
func (f *Filter) Reset() {
	*f = Filter{floor: f.floor}
}

// Len returns the number of samples in the register.
func (f *Filter) Len() int { return f.n }

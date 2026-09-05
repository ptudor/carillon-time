package discipline

import (
	"math"
	"sort"
	"time"

	"carillon/internal/ntp"
)

const (
	// MinDispersion is the floor applied to the delay term of the root
	// distance so that a source with zero delay (a reference clock) still
	// has a non-empty correctness interval (ntpd `tos mindist`).
	MinDispersion = 0.001

	// ClusterMin is the number of survivors below which the clustering
	// algorithm stops discarding outliers (ntpd `tos minclock`).
	ClusterMin = 3

	// PPSGuard is how close to true time a numbering source must say the
	// clock is before a PPS source may be trusted to identify seconds. The
	// PPS sample itself is only unambiguous within ±0.5 s; the guard band
	// leaves 0.1 s of margin.
	PPSGuard = 0.4

	// ppsAgreementFloor is the smallest slack allowed when comparing a PPS
	// offset with a numbering source's: a millisecond covers the numbering
	// source's own jitter budget on any real path.
	ppsAgreementFloor = 1e-3
)

// SelectStatus is a source's role after the most recent selection.
type SelectStatus int

const (
	StatusUnreachable SelectStatus = iota // reach register is zero
	StatusNoSelect                        // configured noselect
	StatusInvalid                         // reachable but unusable: no estimate, stratum 16, leap unsync, or too distant
	StatusUnqualified                     // PPS source with no numbering source to identify seconds
	StatusFalseticker                     // outside the intersection of correct sources
	StatusOutlier                         // discarded by the clustering algorithm
	StatusSurvivor                        // survived selection and clustering
	StatusSystem                          // the survivor driving the clock
)

func (s SelectStatus) String() string {
	switch s {
	case StatusUnreachable:
		return "unreachable"
	case StatusNoSelect:
		return "noselect"
	case StatusInvalid:
		return "invalid"
	case StatusUnqualified:
		return "unqualified"
	case StatusFalseticker:
		return "falseticker"
	case StatusOutlier:
		return "outlier"
	case StatusSurvivor:
		return "survivor"
	case StatusSystem:
		return "system"
	default:
		return "unknown"
	}
}

// SourceState is the discipline's view of one registered source: its most
// recent measurement plus the outcome of the last selection.
type SourceState struct {
	Name string
	Options

	Reach uint8
	Poll  int8

	// Valid reports whether the source has delivered an estimate.
	Valid bool

	// At is the sample time of the estimate; Updated is when the estimate
	// was received, from which its dispersion ages.
	At      float64
	Updated float64

	Offset     float64
	Delay      float64
	Dispersion float64
	Jitter     float64

	Leap        ntp.Leap
	Stratum     uint8
	RefID       ntp.RefID
	SourceRefID ntp.RefID
	RootDelay   float64
	RootDisp    float64
	Precision   int8
	RefTime     time.Time

	// Status and Distance are outputs of the last Select call. Current is
	// the estimate propagated to the selection instant: the stored offset
	// minus the phase the loop has already corrected since the observation
	// was taken. Offset stays the raw stored estimate, for status.
	Status   SelectStatus
	Distance float64
	Current  float64

	// usedAt is the sample time of the last estimate the loop consumed from
	// this source. It is per source rather than one system-wide value,
	// because switching the system source and switching back must not make
	// an unchanged observation look new (RA6X-003, RA6X-001).
	usedAt float64

	// stale is set by Select when the estimate is older than this source's
	// own freshness deadline. See freshnessDeadline.
	stale bool

	// DisagreesWith and Disagreement describe the comparison made in the
	// most recent Select, for a PPS source whose offset is
	// too far from the numbering source that should be vouching for it: the
	// signature of a pulse captured on the wrong edge. Empty otherwise.
	DisagreesWith string
	Disagreement  float64

	// inLoop remembers that this source was last seen to be a timing loop,
	// so the event is reported once on each transition.
	inLoop bool

	// everSurvived remembers that this source has been usable at least
	// once, so that "the preferred source is not usable" is not reported
	// during the initial acquisition phase, when nothing has established
	// service yet.
	everSurvived bool

	// sinceStep counts the valid measurements this source has delivered
	// since the last clock step. A second step must not rest on a single
	// post-step sample, which is how a measurement computed before the
	// first step used to step the clock a second time.
	sinceStep int
}

// SinceStep reports how many valid measurements the source has delivered
// since the last clock step invalidated its estimate.
func (s *SourceState) SinceStep() int { return s.sinceStep }

// apply records a measurement.
func (s *SourceState) apply(m Measurement) {
	s.Reach = m.Reach
	s.Poll = m.Poll
	if m.Invalidate {
		s.Valid = false
	}
	if !m.Valid {
		return
	}
	s.Valid = true
	s.sinceStep++
	s.At = m.At
	s.Updated = m.Now
	s.Offset = m.Offset
	s.Delay = m.Delay
	s.Dispersion = m.Dispersion
	s.Jitter = m.Jitter
	s.Leap = m.Leap
	s.Stratum = m.Stratum
	s.RefID = m.RefID
	s.SourceRefID = m.SourceRefID
	s.RootDelay = m.RootDelay
	s.RootDisp = m.RootDisp
	s.Precision = m.Precision
	s.RefTime = m.RefTime
}

// invalidate discards the estimate (after a step) while keeping reach.
func (s *SourceState) invalidate() {
	s.Valid = false
	s.sinceStep = 0
}

// RootDistance is the source's synchronization distance at time now
// (RFC 5905 §11.2.1): half the total delay, plus all dispersions, aged.
func (s *SourceState) RootDistance(now float64) float64 {
	age := now - s.Updated
	if age < 0 {
		age = 0
	}
	return math.Max(MinDispersion, s.RootDelay+s.Delay)/2 + s.RootDisp + s.Dispersion + Phi*age + s.Jitter
}

// synch is the sort key for survivors: stratum first, then distance.
func (s *SourceState) synch() float64 {
	return float64(s.Stratum)*MaxDistance + s.Distance
}

func (s *SourceState) candidate() (bool, SelectStatus) {
	switch {
	case s.Reach == 0:
		return false, StatusUnreachable
	case s.NoSelect:
		return false, StatusNoSelect
	case s.stale:
		// Nothing has been heard from this source for longer than its own
		// polling makes plausible. Reach normally says this first, but reach
		// only moves when a producer emits something: a source whose
		// goroutine is wedged, or whose device is in a reconnect loop that
		// reports nothing, would otherwise stay selected for the 27 hours
		// it takes Phi ageing alone to push its distance past MaxDistance
		// (RA6X-003).
		return false, StatusUnreachable
	case !s.Valid, s.Leap == ntp.LeapUnsync, s.Stratum >= 16, s.Distance >= MaxDistance:
		return false, StatusInvalid
	}
	return true, StatusSurvivor
}

// freshnessDeadline is how long a source's estimate may stand without a new
// measurement before it stops being eligible. It is derived from the source's
// own poll interval, so a legitimately sparse NTP association at poll 17 is
// not punished for being sparse, and a 1 Hz refclock is not carried for a day.
//
// Eight poll intervals is the width of the reach register: a source that has
// missed that many slots would have reach 0 if anything were still reporting
// on its behalf. The floor keeps a very short poll from producing a deadline
// so tight that ordinary jitter trips it.
func freshnessDeadline(poll int8) float64 {
	if poll < MinPoll {
		poll = MinPoll
	}
	if poll > MaxPoll {
		poll = MaxPoll
	}
	d := reachBits * math.Ldexp(1, int(poll))
	if d < minFreshness {
		return minFreshness
	}
	return d
}

// ppsAgreement compares a PPS source's offset with the surviving numbering
// sources. It reports agreement as soon as one of them is within its own root
// distance plus a jitter allowance; otherwise it names the closest and by how
// much it disagrees, which is what points an operator at edge, pps_mode 0x10
// or offset.
func ppsAgreement(p *SourceState, numbering []*SourceState, now float64) (name string, delta float64, agrees bool) {
	closest := math.Inf(1)
	for _, n := range numbering {
		d := math.Abs(n.Current - p.Current)
		if d <= n.RootDistance(now)+math.Max(4*n.Jitter, ppsAgreementFloor) {
			return "", 0, true
		}
		if d < closest {
			closest, name = d, n.Name
		}
	}
	return name, closest, false
}

// Selection is the outcome of Select.
type Selection struct {
	// Survivors are the sources that passed intersection and clustering,
	// sorted by stratum then distance; System is the one driving the clock.
	Survivors []*SourceState
	System    *SourceState

	// Offset and Jitter are the combined system offset and jitter.
	Offset float64
	Jitter float64

	// PreferLost is set when a prefer source is configured but is not
	// among the survivors.
	PreferLost bool

	// PPSQualified reports whether a numbering source vouched for the
	// current second, allowing PPS sources to be used.
	PPSQualified bool

	// Low and High are the intersection interval, for diagnostics.
	Low, High float64

	// Events carries notable things selection itself noticed, such as a
	// source entering or leaving a timing loop.
	Events []Event
}

// Select runs the RFC 5905 §11.2 selection, clustering and combining
// algorithms over the sources at time now and writes each source's Status
// and Distance. minSurvivors is the number of survivors required before a
// system source is declared.
// timingLoop reports whether a source is this daemon, or is synchronized to
// it. RFC 5905's fitness test (appendix A.5.5.3) includes the same check.
//
// Both directions matter. SourceRefID is the peer's own address: matching it
// means the configured server *is* this host. RefID is what the peer says it
// is synchronized to: matching it means our time would come back to us. A
// textual identifier — "GPS ", "PPS ", a kiss code — is never an address and
// is never a loop.
//
// Two peers that share a third upstream have equal RefIDs to each other, not
// to ours, so ordinary shared-upstream configurations are unaffected. For
// IPv6 the identifier is a 32-bit hash of the address, so a collision could
// reject a legitimate peer; that fails closed, costs one source, and has a
// probability of about 2^-32 per local address. A peer behind NAT reporting a
// public address this host does not itself hold cannot be detected here.
func timingLoop(s *SourceState, local map[ntp.RefID]bool) bool {
	if len(local) == 0 {
		return false
	}
	if !s.SourceRefID.IsText() && local[s.SourceRefID] {
		return true
	}
	if s.Stratum > 1 && !s.RefID.IsText() && local[s.RefID] {
		return true
	}
	return false
}

func Select(sources []*SourceState, now float64, minSurvivors int) Selection {
	return SelectAt(sources, now, minSurvivors, SelectOptions{})
}

// SelectOptions carries the context selection needs beyond the sources
// themselves.
type SelectOptions struct {
	// LocalRefIDs identifies this host, for the timing-loop check.
	LocalRefIDs map[ntp.RefID]bool

	// AppliedSince returns the phase the discipline has corrected between an
	// observation's time and now. Every stored offset is reduced by it
	// before the offsets are compared or combined, so observations taken at
	// different moments are all expressed as the error remaining *now*
	// (RA6X-001, RA6X-025). Nil means no correction is known, which is the
	// right answer for a caller that is not driving a clock.
	AppliedSince func(at, now float64) float64
}

// SelectAt is Select with the selection context.
func SelectAt(sources []*SourceState, now float64, minSurvivors int, opts SelectOptions) Selection {
	local := opts.LocalRefIDs
	var sel Selection
	var cands, pps []*SourceState
	preferConfigured, anySurvived := false, false
	for _, s := range sources {
		s.Distance = s.RootDistance(now)
		s.stale = s.Valid && now-s.Updated > freshnessDeadline(s.Poll)
		// Express the estimate at the selection instant. The clock filter
		// can release an observation several polls old, and feeding its
		// offset to the loop as present-time feedback re-integrates a
		// correction the loop has already made — which is what wound the
		// frequency up by hundreds of ppm in the takeover reproduction
		// (RA6X-001). The applied correction is known exactly; the
		// oscillator's own drift over the interval is already carried by
		// the dispersion the filter ages at Phi.
		s.Current = s.Offset
		if opts.AppliedSince != nil && s.Valid {
			s.Current -= opts.AppliedSince(s.At, now)
		}
		if s.PPS {
			// These describe the comparison made in *this* selection.
			// Retaining them when no comparison happens — the PPS was
			// rejected as a candidate, or nothing survived to number its
			// seconds — makes a historical finding read as a current one
			// and sends an operator hunting for an edge or calibration
			// error when the present problem is numbering loss (RA6X-051).
			s.DisagreesWith, s.Disagreement = "", 0
		}
		ok, st := s.candidate()
		if ok && timingLoop(s, local) {
			ok, st = false, StatusInvalid
			if !s.inLoop {
				s.inLoop = true
				sel.Events = append(sel.Events, Event{Kind: EventTimingLoop, Source: s.Name})
			}
		} else if s.inLoop {
			s.inLoop = false
			sel.Events = append(sel.Events, Event{Kind: EventTimingLoopCleared, Source: s.Name})
		}
		s.Status = st
		if s.everSurvived {
			anySurvived = true
		}
		if s.Prefer && !s.NoSelect {
			preferConfigured = true
		}
		if !ok {
			continue
		}
		if s.PPS {
			s.Status = StatusUnqualified
			pps = append(pps, s)
			continue
		}
		cands = append(cands, s)
	}

	survivors := intersect(cands, &sel)
	survivors = cluster(survivors)

	// A PPS edge only says "a second starts here". It may be used once a
	// surviving numbering source agrees the clock is within the guard band.
	var numbering []*SourceState
	for _, s := range survivors {
		if s.Numbering && math.Abs(s.Current) < PPSGuard {
			numbering = append(numbering, s)
		}
	}
	sel.PPSQualified = len(numbering) > 0
	if sel.PPSQualified {
		for _, p := range pps {
			// Proximity to zero is not agreement. A PPS captured on the
			// wrong edge — "assert" on a receiver whose second mark is the
			// falling edge, or an inverted signal that pps_mode should have
			// had 0x10 for — reports a rock-steady offset equal to the
			// pulse width, typically 20–200 ms, with microsecond jitter. It
			// locks, and the numbering source then reports roughly minus
			// the pulse width, still comfortably inside the 0.4 s band. The
			// daemon would discipline the clock 20–200 ms wrong while
			// advertising stratum 1, refid PPS and a few microseconds of
			// root dispersion, and the correct NTP source would be the one
			// that looked wrong. So require the PPS to agree with a
			// surviving numbering source to within that source's own
			// uncertainty.
			name, delta, agrees := ppsAgreement(p, numbering, now)
			p.DisagreesWith, p.Disagreement = name, delta
			if !agrees {
				p.Status = StatusFalseticker
				continue
			}
			p.Status = StatusSurvivor
			survivors = append(survivors, p)
		}
	}

	sort.SliceStable(survivors, func(i, j int) bool { return survivors[i].synch() < survivors[j].synch() })
	for _, s := range survivors {
		s.Status = StatusSurvivor
	}
	sel.Survivors = survivors
	for _, s := range survivors {
		s.everSurvived = true
	}
	// Before anything has ever been usable there is nothing to have lost.
	// Reporting it there logs an ERROR at every daemon start, followed by
	// "preferred source is back in charge" a few seconds later, which is a
	// false page for anyone alerting on ERROR lines.
	//
	// What is suppressed is the *initial acquisition phase*, not the case
	// the operator most needs to hear about. Requiring the preferred source
	// itself to have been reachable once meant a miswired or misconfigured
	// preferred GPS could be absent for ever while fallback service was
	// reported healthy (RA6X-050). Once some source has established
	// service, a configured preferred source that is not usable is
	// reported, whether or not it has ever answered.
	fallbackEstablished := anySurvived || len(survivors) > 0
	reportPreferLost := preferConfigured && fallbackEstablished
	if len(survivors) == 0 || len(survivors) < minSurvivors {
		sel.PreferLost = reportPreferLost
		return sel
	}

	// System source: a qualified prefer PPS, else the prefer source, else
	// the best-ranked survivor.
	var sys *SourceState
	for _, s := range survivors {
		if s.PPS && s.Prefer {
			sys = s
			break
		}
	}
	if sys == nil {
		for _, s := range survivors {
			if s.Prefer {
				sys = s
				break
			}
		}
	}
	sel.PreferLost = reportPreferLost && sys == nil
	if sys == nil {
		sys = survivors[0]
	}
	sys.Status = StatusSystem
	sel.System = sys

	// Combine: a prefer source's offset is used as-is; otherwise a mean
	// weighted by inverse root distance. The system jitter is the system
	// source's own jitter combined with the spread of the survivors.
	var sumW, sumWO float64
	for _, s := range survivors {
		w := 1 / math.Max(s.Distance, MinDispersion/2)
		sumW += w
		sumWO += w * s.Current
	}
	mean := sumWO / sumW
	if sys.Prefer {
		sel.Offset = sys.Current
	} else {
		sel.Offset = mean
	}
	var spread float64
	for _, s := range survivors {
		w := 1 / math.Max(s.Distance, MinDispersion/2)
		d := s.Current - sel.Offset
		spread += w * d * d
	}
	spread = math.Sqrt(spread / sumW)
	sel.Jitter = math.Sqrt(sys.Jitter*sys.Jitter + spread*spread)
	return sel
}

// intersect is the Marzullo/Mills intersection algorithm of RFC 5905
// §11.2.1: find the smallest interval containing points from the largest
// possible number of correctness intervals, allowing up to f < n/2
// falsetickers. Sources whose interval does not overlap it are marked
// falsetickers.
func intersect(c []*SourceState, sel *Selection) []*SourceState {
	n := len(c)
	if n == 0 {
		return nil
	}
	type edge struct {
		v float64
		t int // -1 low endpoint, 0 midpoint, +1 high endpoint
	}
	edges := make([]edge, 0, 3*n)
	for _, s := range c {
		edges = append(edges,
			edge{s.Current - s.Distance, -1},
			edge{s.Current, 0},
			edge{s.Current + s.Distance, +1})
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].v != edges[j].v {
			return edges[i].v < edges[j].v
		}
		return edges[i].t < edges[j].t
	})

	var low, high float64
	found := false
	for allow := 0; 2*allow < n; allow++ {
		mids, chime := 0, 0
		lo, hi := math.NaN(), math.NaN()
		for _, e := range edges {
			chime -= e.t
			if chime >= n-allow {
				lo = e.v
				break
			}
			if e.t == 0 {
				mids++
			}
		}
		chime = 0
		for i := len(edges) - 1; i >= 0; i-- {
			e := edges[i]
			chime += e.t
			if chime >= n-allow {
				hi = e.v
				break
			}
			if e.t == 0 {
				mids++
			}
		}
		// Midpoints outside the interval mean a truechimer straddles it;
		// allow one more falseticker and retry.
		if mids > allow {
			continue
		}
		if hi > lo {
			low, high, found = lo, hi, true
			break
		}
	}
	if !found {
		for _, s := range c {
			s.Status = StatusFalseticker
		}
		return nil
	}
	sel.Low, sel.High = low, high
	var surv []*SourceState
	for _, s := range c {
		if s.Current+s.Distance < low || s.Current-s.Distance > high {
			s.Status = StatusFalseticker
			continue
		}
		surv = append(surv, s)
	}
	return surv
}

// cluster is the clustering algorithm of RFC 5905 §11.2.2: repeatedly
// discard the survivor whose offset is farthest from the others (largest
// selection jitter) while that improves the estimate, keeping at least
// ClusterMin.
func cluster(surv []*SourceState) []*SourceState {
	for len(surv) > ClusterMin {
		n := len(surv)
		maxIdx, maxSel := -1, -1.0
		minJit := math.Inf(1)
		for i, s := range surv {
			var sum float64
			for j, o := range surv {
				if i != j {
					d := s.Current - o.Current
					sum += d * d
				}
			}
			sj := math.Sqrt(sum / float64(n-1))
			if sj > maxSel {
				maxSel, maxIdx = sj, i
			}
			if s.Jitter < minJit {
				minJit = s.Jitter
			}
		}
		if maxSel < minJit {
			break
		}
		surv[maxIdx].Status = StatusOutlier
		surv = append(surv[:maxIdx:maxIdx], surv[maxIdx+1:]...)
	}
	return surv
}

// majorityLeap returns the leap indication agreed by more than half of the
// survivors, or LeapNone.
func majorityLeap(surv []*SourceState) ntp.Leap {
	var ins, del, voters int
	for _, s := range surv {
		// A bare PPS edge has no calendar information. Its selected numbering
		// sources, not the pulse itself, vote on leap warnings.
		if s.PPS {
			continue
		}
		voters++
		switch s.Leap {
		case ntp.LeapInsert:
			ins++
		case ntp.LeapDelete:
			del++
		}
	}
	switch {
	case ins*2 > voters:
		return ntp.LeapInsert
	case del*2 > voters:
		return ntp.LeapDelete
	}
	return ntp.LeapNone
}

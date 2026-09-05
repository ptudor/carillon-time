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

	// Status and Distance are outputs of the last Select call.
	Status   SelectStatus
	Distance float64

	// everReachable and everSurvived remember that this source has been
	// usable at least once, so that "the preferred source is not usable"
	// is not reported before it has ever had a chance to be.
	everReachable bool
	everSurvived  bool

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
	case !s.Valid, s.Leap == ntp.LeapUnsync, s.Stratum >= 16, s.Distance >= MaxDistance:
		return false, StatusInvalid
	}
	return true, StatusSurvivor
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
}

// Select runs the RFC 5905 §11.2 selection, clustering and combining
// algorithms over the sources at time now and writes each source's Status
// and Distance. minSurvivors is the number of survivors required before a
// system source is declared.
func Select(sources []*SourceState, now float64, minSurvivors int) Selection {
	var sel Selection
	var cands, pps []*SourceState
	preferConfigured, preferSeen, anySurvived := false, false, false
	for _, s := range sources {
		s.Distance = s.RootDistance(now)
		ok, st := s.candidate()
		s.Status = st
		if s.Reach != 0 {
			s.everReachable = true
		}
		if s.everSurvived {
			anySurvived = true
		}
		if s.Prefer && !s.NoSelect {
			preferConfigured = true
			preferSeen = preferSeen || s.everReachable
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
	for _, s := range survivors {
		if s.Numbering && math.Abs(s.Offset) < PPSGuard {
			sel.PPSQualified = true
			break
		}
	}
	if sel.PPSQualified {
		for _, p := range pps {
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
	reportPreferLost := preferConfigured && preferSeen && (anySurvived || len(survivors) > 0)
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
		sumWO += w * s.Offset
	}
	mean := sumWO / sumW
	if sys.Prefer {
		sel.Offset = sys.Offset
	} else {
		sel.Offset = mean
	}
	var spread float64
	for _, s := range survivors {
		w := 1 / math.Max(s.Distance, MinDispersion/2)
		d := s.Offset - sel.Offset
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
			edge{s.Offset - s.Distance, -1},
			edge{s.Offset, 0},
			edge{s.Offset + s.Distance, +1})
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
		if s.Offset+s.Distance < low || s.Offset-s.Distance > high {
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
					d := s.Offset - o.Offset
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

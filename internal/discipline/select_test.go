package discipline

import (
	"math"
	"testing"

	"github.com/ptudor/carillon-time/internal/ntp"
)

func src(name string, offset, distance float64, o Options) *SourceState {
	return &SourceState{
		Name:    name,
		Options: o,
		Reach:   0xff,
		Poll:    6,
		Valid:   true,
		At:      100,
		Updated: 100,
		Offset:  offset,
		Delay:   distance, // RootDistance = max(0.001, delay)/2 + ... ≈ distance/2 + jitter
		Jitter:  50e-6,
		Stratum: 2,
		Leap:    ntp.LeapNone,
	}
}

func names(ss []*SourceState) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Name
	}
	return out
}

func TestSelectSingle(t *testing.T) {
	a := src("a", 0.002, 0.020, Options{Numbering: true})
	sel := Select([]*SourceState{a}, 100, 1)
	if sel.System != a || sel.Offset != 0.002 || a.Status != StatusSystem {
		t.Fatalf("%+v status=%v", sel, a.Status)
	}
	if sel.Low >= sel.High {
		t.Fatal("interval must be non-empty")
	}
}

func TestSelectAgreeingCombine(t *testing.T) {
	a := src("a", 0.000, 0.020, Options{})
	b := src("b", 0.001, 0.040, Options{})
	c := src("c", -0.001, 0.040, Options{})
	sel := Select([]*SourceState{a, b, c}, 100, 1)
	if len(sel.Survivors) != 3 {
		t.Fatalf("survivors %v", names(sel.Survivors))
	}
	if sel.System != a {
		t.Fatalf("closest source must be system, got %s", sel.System.Name)
	}
	if math.Abs(sel.Offset) > 1e-4 {
		t.Fatalf("combined offset %v", sel.Offset)
	}
	if sel.Jitter <= 0 {
		t.Fatal("jitter must be positive")
	}
}

func TestSelectFalseticker(t *testing.T) {
	a := src("a", 0.000, 0.020, Options{})
	b := src("b", 0.001, 0.020, Options{})
	bad := src("bad", 5.0, 0.020, Options{})
	sel := Select([]*SourceState{a, bad, b}, 100, 1)
	if bad.Status != StatusFalseticker {
		t.Fatalf("bad status %v", bad.Status)
	}
	if len(sel.Survivors) != 2 || math.Abs(sel.Offset) > 1e-3 {
		t.Fatalf("survivors %v offset %v", names(sel.Survivors), sel.Offset)
	}
}

func TestSelectTwoDisagree(t *testing.T) {
	a := src("a", 0.0, 0.020, Options{})
	b := src("b", 1.0, 0.020, Options{})
	sel := Select([]*SourceState{a, b}, 100, 1)
	if sel.System != nil {
		t.Fatalf("two disagreeing sources cannot elect a system source: %s", sel.System.Name)
	}
	if a.Status != StatusFalseticker || b.Status != StatusFalseticker {
		t.Fatalf("statuses %v %v", a.Status, b.Status)
	}
}

func TestSelectPrefer(t *testing.T) {
	a := src("a", 0.000, 0.010, Options{})
	p := src("p", 0.003, 0.050, Options{Prefer: true})
	sel := Select([]*SourceState{a, p}, 100, 1)
	if sel.System != p || sel.Offset != 0.003 || sel.PreferLost {
		t.Fatalf("prefer must drive: sys=%v off=%v lost=%v", sel.System.Name, sel.Offset, sel.PreferLost)
	}

	// The prefer source goes wrong: it is a falseticker, prefer is lost,
	// and the others carry on.
	b := src("b", 0.0002, 0.010, Options{})
	p.Offset = 4
	sel = Select([]*SourceState{a, b, p}, 100, 1)
	if !sel.PreferLost || sel.System == p || p.Status != StatusFalseticker {
		t.Fatalf("lost=%v sys=%v pstatus=%v", sel.PreferLost, sel.System.Name, p.Status)
	}
}

func TestSelectExclusions(t *testing.T) {
	unreach := src("unreach", 0, 0.01, Options{})
	unreach.Reach = 0
	nosel := src("nosel", 0, 0.01, Options{NoSelect: true})
	s16 := src("s16", 0, 0.01, Options{})
	s16.Stratum = 16
	unsync := src("unsync", 0, 0.01, Options{})
	unsync.Leap = ntp.LeapUnsync
	far := src("far", 0, 4.0, Options{})
	novalid := src("novalid", 0, 0.01, Options{})
	novalid.Valid = false
	good := src("good", 0.001, 0.01, Options{})
	sel := Select([]*SourceState{unreach, nosel, s16, unsync, far, novalid, good}, 100, 1)
	if sel.System != good {
		t.Fatalf("system %v", sel.System)
	}
	want := map[string]SelectStatus{
		"unreach": StatusUnreachable, "nosel": StatusNoSelect, "s16": StatusInvalid,
		"unsync": StatusInvalid, "far": StatusInvalid, "novalid": StatusInvalid, "good": StatusSystem,
	}
	for _, s := range []*SourceState{unreach, nosel, s16, unsync, far, novalid, good} {
		if s.Status != want[s.Name] {
			t.Errorf("%s: status %v want %v", s.Name, s.Status, want[s.Name])
		}
	}
}

func TestSelectCluster(t *testing.T) {
	tight := []*SourceState{
		src("a", 0.0000, 0.010, Options{}),
		src("b", 0.0001, 0.010, Options{}),
		src("c", -0.0001, 0.010, Options{}),
		src("d", 0.00005, 0.010, Options{}),
	}
	loose := src("loose", 0.008, 0.040, Options{}) // overlaps the intersection but is far from the others
	sel := Select(append(tight, loose), 100, 1)
	if loose.Status != StatusOutlier {
		t.Fatalf("loose status %v", loose.Status)
	}
	if len(sel.Survivors) > ClusterMin || len(sel.Survivors) < 1 {
		t.Fatalf("survivors %v", names(sel.Survivors))
	}
	for _, s := range sel.Survivors {
		if s == loose {
			t.Fatal("outlier survived")
		}
	}
}

func TestSelectMinSurvivors(t *testing.T) {
	a := src("a", 0, 0.01, Options{})
	sel := Select([]*SourceState{a}, 100, 2)
	if sel.System != nil {
		t.Fatal("one survivor must not satisfy min_survivors 2")
	}
	if a.Status != StatusSurvivor {
		t.Fatalf("status %v", a.Status)
	}
}

// testPPSSource is a locked, stable PPS at the given offset: rock-steady, a
// couple of microseconds of dispersion, and marked prefer.
func testPPSSource(offset float64) *SourceState {
	return &SourceState{
		Name: "pps", Options: Options{PPS: true, Prefer: true},
		Reach: 0xff, Poll: 4, Valid: true, At: 100, Updated: 100,
		Offset: offset, Dispersion: 2e-6, Jitter: 1e-6, Stratum: 0, Leap: ntp.LeapNone,
	}
}

func TestSelectPPSQualification(t *testing.T) {
	ntpSrc := src("ntp", 60e-6, 0.020, Options{Numbering: true})
	pps := testPPSSource(50e-6)
	sel := Select([]*SourceState{ntpSrc, pps}, 100, 1)
	if !sel.PPSQualified || sel.System != pps || sel.Offset != 50e-6 {
		t.Fatalf("pps must drive when qualified: q=%v sys=%v off=%v", sel.PPSQualified, sel.System.Name, sel.Offset)
	}
	if pps.Status != StatusSystem || ntpSrc.Status != StatusSurvivor {
		t.Fatalf("statuses pps=%v ntp=%v", pps.Status, ntpSrc.Status)
	}

	ntpSrc.Offset = 0.45 // outside the guard band
	sel = Select([]*SourceState{ntpSrc, pps}, 100, 1)
	if sel.PPSQualified || sel.System != ntpSrc || pps.Status != StatusUnqualified {
		t.Fatalf("pps must be unqualified: q=%v sys=%v status=%v", sel.PPSQualified, sel.System.Name, pps.Status)
	}
	if !sel.PreferLost {
		t.Fatal("an unqualified prefer PPS counts as prefer lost")
	}
}

// TestSelectPPSMustAgreeWithItsNumberingSource covers RF5X-007. A PPS
// captured on the wrong edge reports a rock-steady offset equal to the pulse
// width, so it locks and qualifies on proximity to zero alone, and the daemon
// disciplines the clock by that much while advertising stratum 1 refid PPS.
// Agreement with the numbering source, not just its presence, is what makes
// the second numbering trustworthy.
func TestSelectPPSMustAgreeWithItsNumberingSource(t *testing.T) {
	ntpSrc := src("ntp", 0.000, 0.020, Options{Numbering: true})
	pps := testPPSSource(0.120) // a 120 ms pulse width, captured on the wrong edge
	sel := Select([]*SourceState{ntpSrc, pps}, 100, 1)
	if pps.Status != StatusFalseticker {
		t.Fatalf("wrong-edge PPS status %v, want falseticker", pps.Status)
	}
	if sel.System != ntpSrc {
		t.Fatalf("system source %v, want the numbering source", sel.System.Name)
	}
	if !sel.PreferLost {
		t.Fatal("a disagreeing prefer PPS counts as prefer lost")
	}
	if pps.DisagreesWith != "ntp" || math.Abs(pps.Disagreement-0.120) > 1e-6 {
		t.Fatalf("disagreement reported as %q by %v, want ntp by 0.120",
			pps.DisagreesWith, pps.Disagreement)
	}

	// The offset itself is not the test: a clock that is 100 ms out has both
	// sources saying so, and the PPS is then the better of the two.
	ntpSrc = src("ntp", 0.100, 0.020, Options{Numbering: true})
	ntpSrc.Jitter = 2e-3
	pps = testPPSSource(0.101)
	sel = Select([]*SourceState{ntpSrc, pps}, 100, 1)
	if !sel.PPSQualified || sel.System != pps || pps.Status != StatusSystem {
		t.Fatalf("an agreeing PPS must drive: qualified=%v system=%v status=%v",
			sel.PPSQualified, sel.System.Name, pps.Status)
	}
	if pps.DisagreesWith != "" || pps.Disagreement != 0 {
		t.Fatalf("agreeing PPS reported a disagreement: %q %v", pps.DisagreesWith, pps.Disagreement)
	}
}

func TestMeasurementCanInvalidatePreviousEstimate(t *testing.T) {
	s := &SourceState{Name: "pps", Options: Options{PPS: true}}
	s.apply(Measurement{Reach: 1, Poll: 4, Valid: true, Offset: 1e-6, Leap: ntp.LeapNone})
	if !s.Valid {
		t.Fatal("valid sample was not recorded")
	}
	s.apply(Measurement{Reach: 3, Poll: 4, Invalidate: true})
	if s.Valid || s.Reach != 3 {
		t.Fatalf("invalidate did not retain reach and discard estimate: %+v", s)
	}
}

func TestMajorityLeap(t *testing.T) {
	a := src("a", 0, 0.01, Options{})
	b := src("b", 0, 0.01, Options{})
	c := src("c", 0, 0.01, Options{})
	a.LiveLeap, b.LiveLeap = ntp.LeapInsert, ntp.LeapInsert
	a.LeapKnown, b.LeapKnown, c.LeapKnown = true, true, true
	if l := majorityLeap([]*SourceState{a, b, c}, 0); l != ntp.LeapInsert {
		t.Fatalf("got %v", l)
	}
	b.LiveLeap = ntp.LeapNone
	if l := majorityLeap([]*SourceState{a, b, c}, 0); l != ntp.LeapNone {
		t.Fatalf("got %v", l)
	}
	if l := majorityLeap([]*SourceState{a}, 0); l != ntp.LeapInsert {
		t.Fatalf("got %v", l)
	}
	pps := &SourceState{Options: Options{PPS: true}, Leap: ntp.LeapNone}
	if l := majorityLeap([]*SourceState{a, pps}, 0); l != ntp.LeapInsert {
		t.Fatalf("PPS without calendar data must not outvote numbering source: %v", l)
	}
}

func TestRootDistanceAges(t *testing.T) {
	a := src("a", 0, 0.020, Options{})
	d0 := a.RootDistance(100)
	d1 := a.RootDistance(1100)
	if math.Abs((d1-d0)-Phi*1000) > 1e-12 {
		t.Fatalf("distance must grow at Phi: %v", d1-d0)
	}
	// Floor: a zero-delay reference clock still has a non-empty interval.
	r := src("ref", 0, 0, Options{})
	if r.RootDistance(100) < MinDispersion/2 {
		t.Fatal("distance floor")
	}
}

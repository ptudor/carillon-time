package discipline

import (
	"math"
	"testing"

	"carillon/internal/ntp"
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

func TestSelectPPSQualification(t *testing.T) {
	ntpSrc := src("ntp", 0.1, 0.020, Options{Numbering: true})
	pps := &SourceState{
		Name: "pps", Options: Options{PPS: true, Prefer: true},
		Reach: 0xff, Poll: 4, Valid: true, At: 100, Updated: 100,
		Offset: 50e-6, Dispersion: 2e-6, Jitter: 1e-6, Stratum: 0, Leap: ntp.LeapNone,
	}
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

func TestMajorityLeap(t *testing.T) {
	a := src("a", 0, 0.01, Options{})
	b := src("b", 0, 0.01, Options{})
	c := src("c", 0, 0.01, Options{})
	a.Leap, b.Leap = ntp.LeapInsert, ntp.LeapInsert
	if l := majorityLeap([]*SourceState{a, b, c}); l != ntp.LeapInsert {
		t.Fatalf("got %v", l)
	}
	b.Leap = ntp.LeapNone
	if l := majorityLeap([]*SourceState{a, b, c}); l != ntp.LeapNone {
		t.Fatalf("got %v", l)
	}
	if l := majorityLeap([]*SourceState{a}); l != ntp.LeapInsert {
		t.Fatalf("got %v", l)
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

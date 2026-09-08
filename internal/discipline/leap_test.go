package discipline

import (
	"math"
	"testing"

	"github.com/ptudor/carillon-time/internal/ntp"
)

func TestLeapEvidenceRequiresFreshCapableMajority(t *testing.T) {
	a := &SourceState{LeapKnown: true, LiveLeap: ntp.LeapNone, LeapUpdated: 10, Poll: 6}
	b := &SourceState{LeapKnown: true, LiveLeap: ntp.LeapInsert, LeapUpdated: 10, Poll: 6}
	if got := majorityLeap([]*SourceState{a, b}, 10); got != ntp.LeapUnsync {
		t.Fatalf("split vote became %s", got)
	}
	for _, opts := range []Options{{PPS: true}, {Numbering: true, LeapIncapable: true}} {
		c := *a
		c.Options = opts
		if got := majorityLeap([]*SourceState{&c}, 10); got != ntp.LeapUnsync {
			t.Fatal("calendar-incapable input voted no leap")
		}
	}
	for _, at := range []float64{9, 139, math.NaN(), math.Inf(1)} {
		if got := majorityLeap([]*SourceState{a}, at); got != ntp.LeapUnsync {
			t.Fatalf("invalid/stale time %v voted %s", at, got)
		}
	}
	if got := majorityLeap([]*SourceState{a}, 138); got != ntp.LeapNone {
		t.Fatal("two-poll freshness bound excluded its boundary")
	}
	if got := majorityLeap(nil, 10); got != ntp.LeapUnsync {
		t.Fatal("no votes became no leap")
	}
}

func TestLeapEvidenceDoesNotDependOnFilterWinner(t *testing.T) {
	s := &SourceState{Options: Options{Numbering: true}}
	s.apply(Measurement{Valid: true, Now: 1, At: 1, Poll: 6, Leap: ntp.LeapNone})
	s.apply(Measurement{Acquired: true, Now: 20, Poll: 6, LeapSample: true, LeapValue: ntp.LeapInsert, LeapObserved: 19})
	if s.Leap != ntp.LeapNone || s.At != 1 {
		t.Fatal("latest LI rewrote historical filter metadata")
	}
	if got := majorityLeap([]*SourceState{s}, 20); got != ntp.LeapInsert {
		t.Fatalf("unchanged winner hid latest LI: %s", got)
	}
	s.apply(Measurement{Now: 200, Poll: 6}) // timeout cannot refresh evidence
	if got := majorityLeap([]*SourceState{s}, 200); got != ntp.LeapUnsync {
		t.Fatal("heartbeat refreshed LI")
	}
}

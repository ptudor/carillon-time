package engine

import (
	"testing"
	"time"

	"github.com/ptudor/carillon-time/internal/clock"
	"github.com/ptudor/carillon-time/internal/discipline"
	"github.com/ptudor/carillon-time/internal/leap"
	"github.com/ptudor/carillon-time/internal/ntp"
)

func feedNetwork(t *testing.T, e *Engine, clk *clock.Fake, name string, li ntp.Leap, elapsed time.Duration) {
	t.Helper()
	clk.Advance(elapsed)
	m := good(0)
	m.Source, m.Leap = name, li
	m.Now = clk.Monotonic()
	m.At = m.Now
	e.crossLeap(m.Now)
	if err := e.handle(e.sys.Update(m), m.Now); err != nil {
		t.Fatal(err)
	}
}

func tickNetwork(t *testing.T, e *Engine, clk *clock.Fake, elapsed time.Duration) {
	t.Helper()
	clk.Advance(elapsed)
	e.crossLeap(clk.Monotonic())
	if err := e.handle(e.sys.Tick(clk.Monotonic()), clk.Monotonic()); err != nil {
		t.Fatal(err)
	}
}

func TestPlainClientLostPollsHoldoverAndRecovery(t *testing.T) {
	for _, recover := range []bool{false, true} {
		name := "expiry"
		if recover {
			name = "recovery"
		}
		t.Run(name, func(t *testing.T) {
			clk := clock.NewFake(time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC))
			src := &scripted{name: "upstream", clk: clk}
			e, err := New(testConfig("", SourceSpec{Source: src, Options: discipline.Options{Numbering: true}}), clk, quietLog())
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				feedNetwork(t, e, clk, src.name, ntp.LeapNone, 64*time.Second)
			}
			initial := e.Status()
			if initial.State != discipline.StateSynced || !initial.LeapReady {
				t.Fatalf("initial synchronization: %+v", initial)
			}
			for _, stage := range []struct {
				elapsed time.Duration
				state   discipline.State
			}{{129 * time.Second, discipline.StateSynced}, {400 * time.Second, discipline.StateHoldover}} {
				tickNetwork(t, e, clk, stage.elapsed)
				st := e.Status()
				if st.State != stage.state || st.ClockState != stage.state || st.LeapReady || st.Leap != ntp.LeapUnsync || st.Stratum != initial.Stratum || st.RefID == leapMissingRefID {
					t.Fatalf("unknown LI changed discipline: %+v", st)
				}
				if ks := clk.Status(); !ks.Synced || ks.Leap != ntp.LeapNone {
					t.Fatalf("unknown LI changed kernel synchronization or armed an event: %+v", ks)
				}
			}
			if recover {
				feedNetwork(t, e, clk, src.name, ntp.LeapNone, time.Second)
				if st := e.Status(); st.State != discipline.StateSynced || !st.LeapReady || !clk.Status().Synced {
					t.Fatalf("one reply failed to recover: %+v", st)
				}
			} else {
				tickNetwork(t, e, clk, time.Duration(e.cfg.Discipline.HoldoverMax+1)*time.Second)
				if st := e.Status(); st.State != discipline.StateUnsynced || clk.Status().Synced || st.Leap != ntp.LeapUnsync {
					t.Fatalf("holdover expiry failed to withdraw synchronization: %+v", st)
				}
			}
		})
	}
}

func TestPlainClientSplitLeapVote(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC))
	a, b := &scripted{name: "a", clk: clk}, &scripted{name: "b", clk: clk}
	e, err := New(testConfig("", SourceSpec{Source: a}, SourceSpec{Source: b}), clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		feedNetwork(t, e, clk, a.name, ntp.LeapNone, 64*time.Second)
		feedNetwork(t, e, clk, b.name, ntp.LeapNone, 0)
	}
	for range 2 {
		feedNetwork(t, e, clk, a.name, ntp.LeapInsert, 64*time.Second)
		feedNetwork(t, e, clk, b.name, ntp.LeapNone, 0)
	}
	if st := e.Status(); st.State != discipline.StateSynced || st.LeapReady || st.Leap != ntp.LeapUnsync || st.Stratum >= 16 {
		t.Fatalf("split vote changed synchronization: %+v", st)
	}
	if ks := clk.Status(); !ks.Synced || ks.Leap != ntp.LeapNone {
		t.Fatalf("split vote reached kernel as an event or loss of sync: %+v", ks)
	}
}

func TestDelayedTickAcrossInsertionResetsOnce(t *testing.T) {
	boundary := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	o := leapObject(t, boundary.AddDate(0, -1, 0), boundary.AddDate(0, 6, 0), ntp.LeapInsert)
	r := &leap.Record{Object: o, Provider: leap.Provider{Kind: "file", Name: "offline"}, Accepted: boundary.Add(-time.Hour)}
	e, clk, gps, pps := gpsEngine(t, boundary.Add(-7*time.Second), leap.State{Active: r, UTCbound: r.Accepted})
	for range 4 {
		feedGPS(t, e, clk, time.Second)
	}
	// The engine stalls across both the repeat and midnight. It cannot
	// observe the repeated second, but the forward crossing must reset once.
	clk.Advance(6 * time.Second)
	clk.SetLocalOffset(time.Second)
	e.crossLeap(clk.Monotonic())
	if err := e.handle(e.sys.Tick(clk.Monotonic()), clk.Monotonic()); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		feedGPS(t, e, clk, time.Second)
	}
	if gps.resets.Load() != 1 || pps.resets.Load() != 1 || !e.Status().LeapExecuted.Equal(boundary) || len(clk.Steps) != 0 {
		t.Fatalf("delayed crossing lost or repeated the event: %+v", e.Status())
	}
	if !clk.Status().Synced || clk.Status().Leap != ntp.LeapNone || !e.Status().LeapReady {
		t.Fatal("delayed crossing interrupted valid service")
	}
}

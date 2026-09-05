package discipline

import (
	"fmt"
	"math"
	"net/netip"
	"testing"

	"carillon/internal/ntp"
)

// TestAstra6NeverStepIncludesPanicStartup is the review's RA6X-011 probe.
// panic_at_startup used to reach the step directly, so an explicit
// limit = 0 ("never step") could still move the clock by 2000 seconds.
func TestAstra6NeverStepIncludesPanicStartup(t *testing.T) {
	cfg := loopCfg()
	cfg.StepLimit = 0
	cfg.PanicAtStartup = true
	u := NewLoop(cfg, 0, true).Update(2000, 6, 1, false, true)
	if u.Stepped {
		t.Fatal("limit=0 and panic_at_startup=true issued a 2000-second step")
	}
	if !u.PanicRefused {
		t.Fatal("a correction that cannot be stepped must be refused, not slewed")
	}
	if len(u.Actions) != 0 {
		t.Fatalf("refused panic correction produced actions: %+v", u.Actions)
	}
}

// TestAstra6StepPolicyCrossProduct is RA6X-011's verification table: the
// cross product of limit 0/positive/-1, panic_at_startup false/true, first
// and later update, and both signs of offset around both thresholds.
func TestAstra6StepPolicyCrossProduct(t *testing.T) {
	const (
		threshold = 0.5
		panicAt   = 1000.0
	)
	// Offsets either side of each threshold, both signs.
	offsets := []struct {
		name   string
		value  float64
		beyond string // "small", "step", "panic"
	}{
		{"tiny +", 0.1, "small"},
		{"tiny -", -0.1, "small"},
		{"past threshold +", 10, "step"},
		{"past threshold -", -10, "step"},
		{"just past threshold +", threshold + 1e-9, "step"},
		{"just under threshold +", threshold - 1e-9, "small"},
		{"past panic +", 2000, "panic"},
		{"past panic -", -2000, "panic"},
		{"just past panic +", panicAt + 1, "panic"},
		{"just under panic +", panicAt - 1, "step"},
	}
	for _, limit := range []int{0, 3, -1} {
		for _, atStartup := range []bool{false, true} {
			for _, first := range []bool{true, false} {
				for _, o := range offsets {
					name := label(limit, atStartup, first, o.name)
					t.Run(name, func(t *testing.T) {
						cfg := loopCfg()
						cfg.StepThreshold = threshold
						cfg.Panic = panicAt
						cfg.StepLimit = limit
						cfg.PanicAtStartup = atStartup
						l := NewLoop(cfg, 0, true)
						now := 1.0
						if !first {
							// One ordinary in-range update first, so
							// Updates > 0 without consuming a step.
							l.Update(0.001, 6, now, false, true)
							now++
						}
						u := l.Update(o.value, 6, now, false, true)

						stepsAllowed := limit < 0 || (limit > 0 && (first || l.Updates-1 < limit))
						switch o.beyond {
						case "panic":
							// The startup exception applies only on the
							// first update, and only when stepping is
							// permitted at all.
							want := first && atStartup && stepsAllowed
							if u.Stepped != want {
								t.Fatalf("stepped=%v, want %v", u.Stepped, want)
							}
							if !want && !u.PanicRefused {
								t.Fatal("panic offset neither stepped nor refused")
							}
							if want && u.PanicRefused {
								t.Fatal("a permitted startup correction was also refused")
							}
						case "step":
							if u.PanicRefused {
								t.Fatal("an offset below panic was panic-refused")
							}
							if u.Stepped != stepsAllowed {
								t.Fatalf("stepped=%v, want %v", u.Stepped, stepsAllowed)
							}
						case "small":
							if u.Stepped || u.PanicRefused {
								t.Fatalf("small offset produced stepped=%v refused=%v", u.Stepped, u.PanicRefused)
							}
						}
						if u.Stepped {
							if len(u.Actions) == 0 || u.Actions[0].Kind != ActionStep {
								t.Fatalf("Stepped without an ActionStep: %+v", u.Actions)
							}
							if u.Actions[0].Value != o.value {
								t.Fatalf("stepped %v, want %v", u.Actions[0].Value, o.value)
							}
						} else {
							for _, a := range u.Actions {
								if a.Kind == ActionStep {
									t.Fatalf("ActionStep without Stepped: %+v", u.Actions)
								}
							}
						}
					})
				}
			}
		}
	}
}

func label(limit int, atStartup, first bool, offset string) string {
	return fmt.Sprintf("limit=%d/panic_at_startup=%v/first=%v/%s", limit, atStartup, first, offset)
}

// TestAstra6LeapUpdatesWithoutSystemFeedback is the review's RA6X-022 probe.
// The survivor leap majority was recomputed only after a nonignored loop
// update from the *system* source, so a warning announced by other survivors
// while the system source's own filter winner was unchanged was ignored.
func TestAstra6LeapUpdatesWithoutSystemFeedback(t *testing.T) {
	s := New(simConfig(), 0, true)
	s.AddSource("a", Options{Numbering: true, Prefer: true})
	s.AddSource("b", Options{Numbering: true})
	s.AddSource("c", Options{Numbering: true})
	m := Measurement{Now: 1, At: 1, Valid: true, Reach: 255, Poll: 6, Delay: 0.01, Jitter: 1e-6, Stratum: 2, Leap: ntp.LeapNone}
	for _, name := range []string{"a", "b", "c"} {
		m.Source = name
		s.Update(m)
	}
	m.Now = 2
	m.At = 2
	m.Leap = ntp.LeapInsert
	for _, name := range []string{"b", "c"} {
		m.Source = name
		s.Update(m)
	}
	if s.leap != ntp.LeapInsert {
		t.Fatalf("2/3 survivors announce insertion, system still LI=%v", s.leap)
	}
}

// TestAstra6LeapConsensusCases covers the rest of RA6X-022's list at the
// selection level: an unchanged system winner, only non-system sources
// changing LI, both directions, and a bare PPS not voting.
func TestAstra6LeapConsensusCases(t *testing.T) {
	build := func(t *testing.T) *System {
		t.Helper()
		s := New(simConfig(), 0, true)
		s.AddSource("a", Options{Numbering: true, Prefer: true})
		s.AddSource("b", Options{Numbering: true})
		s.AddSource("c", Options{Numbering: true})
		m := Measurement{Now: 1, At: 1, Valid: true, Reach: 255, Poll: 6, Delay: 0.01, Jitter: 1e-6, Stratum: 2, Leap: ntp.LeapNone}
		for _, name := range []string{"a", "b", "c"} {
			m.Source = name
			s.Update(m)
		}
		return s
	}
	announce := func(s *System, at float64, li ntp.Leap, names ...string) {
		m := Measurement{Now: at, At: at, Valid: true, Reach: 255, Poll: 6, Delay: 0.01, Jitter: 1e-6, Stratum: 2, Leap: li}
		for _, name := range names {
			m.Source = name
			s.Update(m)
		}
	}

	t.Run("deletion", func(t *testing.T) {
		s := build(t)
		announce(s, 2, ntp.LeapDelete, "b", "c")
		if s.leap != ntp.LeapDelete {
			t.Fatalf("LI %v, want delete", s.leap)
		}
	})

	t.Run("a minority does not carry", func(t *testing.T) {
		s := build(t)
		announce(s, 2, ntp.LeapInsert, "b")
		if s.leap != ntp.LeapNone {
			t.Fatalf("one of three survivors carried the vote: LI %v", s.leap)
		}
	})

	t.Run("a warning clears without system feedback", func(t *testing.T) {
		s := build(t)
		announce(s, 2, ntp.LeapInsert, "b", "c")
		if s.leap != ntp.LeapInsert {
			t.Fatalf("setup LI %v", s.leap)
		}
		announce(s, 3, ntp.LeapNone, "b", "c")
		if s.leap != ntp.LeapNone {
			t.Fatalf("the warning was not cleared: LI %v", s.leap)
		}
	})

	t.Run("a bare PPS does not vote it down", func(t *testing.T) {
		s := New(simConfig(), 0, true)
		s.AddSource("gps", Options{Numbering: true, Prefer: true})
		s.AddSource("pps0", Options{PPS: true})
		m := Measurement{Now: 1, At: 1, Valid: true, Reach: 255, Poll: 6, Delay: 0.01, Jitter: 1e-6, Stratum: 2, Leap: ntp.LeapInsert, Source: "gps"}
		s.Update(m)
		m = Measurement{Now: 1, At: 1, Valid: true, Reach: 255, Poll: 4, Delay: 0, Jitter: 1e-7, Stratum: 0, Leap: ntp.LeapNone, Source: "pps0"}
		s.Update(m)
		if s.leap != ntp.LeapInsert {
			t.Fatalf("a PPS with no calendar information voted down the warning: LI %v", s.leap)
		}
	})
}

// TestAstra6SettlingLossDoesNotSynchronize is the review's RA6X-009 probe.
// SETTLING and SYNCED both entered HOLDOVER when selection lost its last
// source, and HOLDOVER is served as synchronized by the wire and the kernel —
// so losing evidence increased the trust placed in a clock the daemon had
// never finished settling.
func TestAstra6SettlingLossDoesNotSynchronize(t *testing.T) {
	cfg := simConfig()
	cfg.SettleUpdates = 3
	s := New(cfg, 0, true)
	s.AddSource("a", Options{Numbering: true})
	s.Update(Measurement{Source: "a", Now: 1, At: 1, Valid: true, Reach: 255, Poll: 6, Offset: 0.001, Delay: 0.01, Jitter: 1e-6, Stratum: 2, Leap: ntp.LeapNone})
	if s.State() != StateSettling {
		t.Fatalf("setup state=%v", s.State())
	}
	s.Update(Measurement{Source: "a", Now: 2, Reach: 0, Poll: 6})
	st := s.Status(2)
	if st.State == StateHoldover || st.Leap != ntp.LeapUnsync || st.Stratum != 16 {
		t.Fatalf("never synchronized but loss yields state=%v LI=%v stratum=%d", st.State, st.Leap, st.Stratum)
	}
}

// TestAstra6HoldoverEntryRequiresSynchronization covers the rest of
// RA6X-009's list.
func TestAstra6HoldoverEntryRequiresSynchronization(t *testing.T) {
	valid := func(name string, at float64, offset float64) Measurement {
		return Measurement{
			Source: name, Now: at, At: at, Valid: true, Reach: 255, Poll: 6,
			Offset: offset, Delay: 0.01, Jitter: 1e-6, Stratum: 2, Leap: ntp.LeapNone,
		}
	}
	lost := func(name string, at float64) Measurement {
		return Measurement{Source: name, Now: at, Reach: 0, Poll: 6, Invalidate: true}
	}

	t.Run("an already-synced daemon still holds over", func(t *testing.T) {
		cfg := simConfig()
		cfg.SettleUpdates = 1
		s := New(cfg, 0, true)
		s.AddSource("a", Options{Numbering: true})
		s.Update(valid("a", 1, 0.001))
		s.Update(valid("a", 2, 0.001))
		if s.State() != StateSynced {
			t.Fatalf("setup state=%v", s.State())
		}
		s.Update(lost("a", 3))
		if s.State() != StateHoldover {
			t.Fatalf("a synchronized daemon must hold over: state=%v", s.State())
		}
	})

	t.Run("loss after a step stays unsynchronized", func(t *testing.T) {
		cfg := simConfig()
		cfg.SettleUpdates = 1
		s := New(cfg, 0, true)
		s.AddSource("a", Options{Numbering: true})
		s.Update(valid("a", 1, 0.001))
		s.Update(valid("a", 2, 0.001))
		if s.State() != StateSynced {
			t.Fatalf("setup state=%v", s.State())
		}
		// A step invalidates the synchronization that preceded it.
		res := s.Update(valid("a", 3, 5))
		stepped := false
		for _, a := range res.Actions {
			if a.Kind == ActionStep {
				stepped = true
			}
		}
		if !stepped {
			t.Skip("this configuration did not step")
		}
		s.Update(lost("a", 4))
		if s.State() == StateHoldover {
			t.Fatal("losing the source right after a step was served as holdover")
		}
	})

	t.Run("settling after synchronization may hold over", func(t *testing.T) {
		cfg := simConfig()
		cfg.SettleUpdates = 3
		s := New(cfg, 0, true)
		s.AddSource("a", Options{Numbering: true})
		for i := 1; i <= 5; i++ {
			s.Update(valid("a", float64(i), 0.001))
		}
		if s.State() != StateSynced {
			t.Fatalf("setup state=%v", s.State())
		}
		// Drop to SETTLING without a step: lose the source, come back, and
		// lose it again. Synchronization is still applicable.
		s.Update(lost("a", 10))
		if s.State() != StateHoldover {
			t.Fatalf("state after the first loss: %v", s.State())
		}
		s.Update(valid("a", 11, 0.001))
		s.Update(lost("a", 12))
		if s.State() != StateHoldover {
			t.Fatalf("a previously synchronized daemon must still hold over: %v", s.State())
		}
	})
}

// TestAstra6MissIsNotSettlingEvidence is the review's RA6X-010 probe. Every
// Measurement naming the system source incremented the settling counter,
// including timeouts, bad authentication and rejected packets, so a transport
// heartbeat could complete settling on its own.
func TestAstra6MissIsNotSettlingEvidence(t *testing.T) {
	cfg := simConfig()
	cfg.SettleUpdates = 1
	s := New(cfg, 0, true)
	s.AddSource("a", Options{Numbering: true})
	s.Update(Measurement{Source: "a", Now: 1, At: 1, Valid: true, Reach: 255, Poll: 6, Offset: 0.001, Delay: 0.01, Jitter: 1e-6, Stratum: 2, Leap: ntp.LeapNone})
	s.Update(Measurement{Source: "a", Now: 65, Reach: 254, Poll: 6})
	if s.State() == StateSynced {
		t.Fatal("timeout-only measurement completed settling")
	}
}

// TestAstra6SettlingEvidenceKinds covers RA6X-010's list: which events count
// toward settling and which do not.
func TestAstra6SettlingEvidenceKinds(t *testing.T) {
	base := Measurement{Source: "a", Reach: 255, Poll: 6}
	cases := []struct {
		name   string
		mutate func(*Measurement)
		counts bool
	}{
		{"a good reply with an unchanged filter winner", func(m *Measurement) { m.Acquired = true }, true},
		{"a new estimate", func(m *Measurement) {
			m.Valid, m.At, m.Offset, m.Delay, m.Jitter, m.Stratum = true, 65, 0.001, 0.01, 1e-6, 2
		}, true},
		{"a timeout", func(m *Measurement) { m.Reach = 254 }, false},
		{"a bad MAC", func(m *Measurement) { m.Reach = 254 }, false},
		{"an unusable stratum", func(m *Measurement) { m.Reach = 254 }, false},
		{"a reset notice", func(m *Measurement) { m.Invalidate = true }, false},
		{"a PPS spike", func(m *Measurement) { m.Reach = 254 }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := simConfig()
			cfg.SettleUpdates = 1
			s := New(cfg, 0, true)
			s.AddSource("a", Options{Numbering: true})
			s.Update(Measurement{Source: "a", Now: 1, At: 1, Valid: true, Reach: 255, Poll: 6, Offset: 0.001, Delay: 0.01, Jitter: 1e-6, Stratum: 2, Leap: ntp.LeapNone})
			if s.State() != StateSettling {
				t.Fatalf("setup state=%v", s.State())
			}
			m := base
			m.Now = 65
			c.mutate(&m)
			s.Update(m)
			if got := s.State() == StateSynced; got != c.counts {
				t.Fatalf("settling completed = %v, want %v (state %v)", got, c.counts, s.State())
			}
		})
	}
}

// TestAstra6TickExpiresSilentSources is the RA6X-003 probe. Selection ages
// uncertainty and rejects over-distance sources, but it ran only on a
// measurement or an explicit lifecycle event: if every producer went silent a
// synchronized source stayed selected indefinitely and the holdover timer
// never started.
func TestAstra6TickExpiresSilentSources(t *testing.T) {
	cfg := simConfig()
	cfg.SettleUpdates = 1
	cfg.HoldoverMax = 600
	s := New(cfg, 0, true)
	s.AddSource("a", Options{Numbering: true})
	for i := 1; i <= 2; i++ {
		s.Update(Measurement{
			Source: "a", Now: float64(i), At: float64(i), Valid: true, Reach: 255, Poll: 6,
			Offset: 0.001, Delay: 0.01, Jitter: 1e-6, Stratum: 2, Leap: ntp.LeapNone,
		})
	}
	if s.State() != StateSynced {
		t.Fatalf("setup state=%v", s.State())
	}

	// Nothing reports again. Ticks alone must expire the source, start
	// holdover, and eventually run it out.
	now := 2.0
	holdoverAt := 0.0
	unsyncedAt := 0.0
	for now < 20000 {
		now += 1
		s.Tick(now)
		if holdoverAt == 0 && s.State() == StateHoldover {
			holdoverAt = now
		}
		if s.State() == StateUnsynced {
			unsyncedAt = now
			break
		}
	}
	if holdoverAt == 0 {
		t.Fatal("ticks alone never entered holdover; the source stayed selected for ever")
	}
	// The freshness deadline for poll 6 is eight polls: 512 s.
	if holdoverAt > 700 {
		t.Fatalf("holdover started at %v s, far past the source's own freshness deadline", holdoverAt)
	}
	if unsyncedAt == 0 {
		t.Fatal("holdover never expired")
	}
	if unsyncedAt-holdoverAt < cfg.HoldoverMax {
		t.Fatalf("holdover lasted %v s, less than the configured %v", unsyncedAt-holdoverAt, cfg.HoldoverMax)
	}
	st := s.Status(unsyncedAt)
	if st.Leap != ntp.LeapUnsync || st.Stratum != 16 {
		t.Fatalf("expired holdover still advertises LI=%v stratum=%d", st.Leap, st.Stratum)
	}
}

// TestAstra6TickKeepsSparseButLegalPolls checks the freshness deadline does
// not punish a legitimately sparse association.
func TestAstra6TickKeepsSparseButLegalPolls(t *testing.T) {
	cfg := simConfig()
	cfg.SettleUpdates = 1
	s := New(cfg, 0, true)
	s.AddSource("a", Options{Numbering: true})
	for i := 1; i <= 2; i++ {
		s.Update(Measurement{
			Source: "a", Now: float64(i), At: float64(i), Valid: true, Reach: 255, Poll: 17,
			Offset: 0.001, Delay: 0.01, Jitter: 1e-6, Stratum: 2, Leap: ntp.LeapNone,
		})
	}
	if s.State() != StateSynced {
		t.Fatalf("setup state=%v", s.State())
	}
	// Six hours of silence from a poll-17 association is entirely legal and
	// well inside the RFC root-distance limit, which is what eventually
	// retires such a source (Phi ageing reaches MaxDistance at about 27.8
	// hours). The freshness deadline must not preempt it.
	for now := 3.0; now < 21600; now += 60 {
		s.Tick(now)
	}
	if s.State() != StateSynced {
		t.Fatalf("a legal poll-17 association was expired after six hours: %v", s.State())
	}
}

// TestAstra6FreshnessDeadlineScales pins the deadline to the source's own
// poll interval, so a sparse association is not punished for being sparse and
// a 1 Hz refclock is not carried for a day.
func TestAstra6FreshnessDeadlineScales(t *testing.T) {
	cases := []struct {
		poll int8
		want float64
	}{
		{2, minFreshness},  // 8*4 = 32 s, raised to the floor
		{3, minFreshness},  // 8*8 = 64 s, exactly the floor
		{4, 128},           // a PPS refclock
		{6, 512},           // the default NTP minimum
		{10, 8192},         // the default NTP maximum
		{17, 8 * 131072},   // the largest legal poll
		{0, minFreshness},  // below MinPoll: clamped
		{100, 8 * 131072},  // above MaxPoll: clamped
		{-5, minFreshness}, // nonsense: clamped
	}
	for _, c := range cases {
		if got := freshnessDeadline(c.poll); got != c.want {
			t.Errorf("freshnessDeadline(%d) = %v, want %v", c.poll, got, c.want)
		}
	}
	// The deadline must never be tighter than the reach register it stands
	// in for.
	for poll := int8(MinPoll); poll <= MaxPoll; poll++ {
		if d, reach := freshnessDeadline(poll), reachBits*math.Ldexp(1, int(poll)); d < reach && d != minFreshness {
			t.Errorf("freshnessDeadline(%d) = %v, tighter than the %v s reach register", poll, d, reach)
		}
	}
}

// TestAstra6TickDoesNotReintegrate checks the tick-driven reselection cannot
// re-apply an observation the loop has already consumed, and that switching
// the system source away and back does not either.
func TestAstra6TickDoesNotReintegrate(t *testing.T) {
	cfg := simConfig()
	cfg.SettleUpdates = 1
	s := New(cfg, 0, true)
	s.AddSource("a", Options{Numbering: true})
	s.Update(Measurement{
		Source: "a", Now: 1, At: 1, Valid: true, Reach: 255, Poll: 6,
		Offset: 0.5, Delay: 0.01, Jitter: 1e-6, Stratum: 2, Leap: ntp.LeapNone,
	})
	updates := s.loop.Updates
	for now := 2.0; now < 12; now++ {
		s.Tick(now)
	}
	if s.loop.Updates != updates {
		t.Fatalf("ticks integrated %d further loop updates from one observation", s.loop.Updates-updates)
	}
}

// TestAstra6ReportsNeverReachablePreferAfterFallback covers RA6X-050.
// PreferLost required the preferred source to have been reachable at least
// once, so a miswired or permanently unavailable preferred GPS could be
// absent for ever while fallback service was reported healthy.
func TestAstra6ReportsNeverReachablePreferAfterFallback(t *testing.T) {
	gps := &SourceState{Name: "gps", Options: Options{Prefer: true, Numbering: true}}
	up := &SourceState{
		Name: "up", Options: Options{Numbering: true},
		Reach: 255, Poll: 6, Valid: true, At: 1, Updated: 1,
		Offset: 0.001, Delay: 0.01, Jitter: 1e-6, Stratum: 2,
	}
	// Successful fallback operation; the preferred source never answers.
	var sel Selection
	for now := 1.0; now < 5; now++ {
		up.Updated = now
		sel = Select([]*SourceState{gps, up}, now, 1)
	}
	if sel.System != up {
		t.Fatalf("fallback was not established: system=%v", sel.System)
	}
	if !sel.PreferLost {
		t.Fatal("a preferred source that never answered is never reported lost")
	}
}

// TestAstra6PreferLostPhases covers the rest of RA6X-050's list.
func TestAstra6PreferLostPhases(t *testing.T) {
	newPair := func() (*SourceState, *SourceState) {
		return &SourceState{Name: "gps", Options: Options{Prefer: true, Numbering: true}},
			&SourceState{Name: "up", Options: Options{Numbering: true}}
	}
	makeValid := func(s *SourceState, now, offset float64) {
		s.Reach, s.Poll, s.Valid = 255, 6, true
		s.At, s.Updated = now, now
		s.Offset, s.Delay, s.Jitter, s.Stratum = offset, 0.01, 1e-6, 2
	}

	t.Run("initial acquisition is quiet", func(t *testing.T) {
		gps, up := newPair()
		sel := Select([]*SourceState{gps, up}, 1, 1)
		if sel.PreferLost {
			t.Fatal("prefer loss reported before anything was ever usable")
		}
	})

	t.Run("a delayed first answer clears it", func(t *testing.T) {
		gps, up := newPair()
		makeValid(up, 1, 0.001)
		sel := Select([]*SourceState{gps, up}, 1, 1)
		if !sel.PreferLost {
			t.Fatal("setup: prefer loss not reported")
		}
		makeValid(gps, 2, 0.001)
		sel = Select([]*SourceState{gps, up}, 2, 1)
		if sel.PreferLost {
			t.Fatal("prefer loss still reported after the preferred source answered")
		}
		if sel.System != gps {
			t.Fatalf("the preferred source did not take over: %v", sel.System)
		}
	})

	t.Run("later loss and recovery", func(t *testing.T) {
		gps, up := newPair()
		makeValid(gps, 1, 0.001)
		makeValid(up, 1, 0.001)
		if sel := Select([]*SourceState{gps, up}, 1, 1); sel.PreferLost {
			t.Fatal("setup: prefer loss reported while the preferred source was fine")
		}
		gps.Reach, gps.Valid = 0, false
		if sel := Select([]*SourceState{gps, up}, 2, 1); !sel.PreferLost {
			t.Fatal("losing the preferred source was not reported")
		}
		makeValid(gps, 3, 0.001)
		up.Updated = 3
		if sel := Select([]*SourceState{gps, up}, 3, 1); sel.PreferLost {
			t.Fatal("recovery was not reported")
		}
	})

	t.Run("noselect prefer is not a configured preference", func(t *testing.T) {
		gps, up := newPair()
		gps.NoSelect = true
		makeValid(up, 1, 0.001)
		if sel := Select([]*SourceState{gps, up}, 1, 1); sel.PreferLost {
			t.Fatal("a noselect source counted as a configured preference")
		}
	})
}

// TestAstra6ClearsObsoleteDisagreement covers RA6X-051. DisagreesWith and
// Disagreement were assigned only inside the PPS-qualified branch, so losing
// the numbering source left a previously disagreeing PPS marked with a
// current-looking finding even though no comparison existed any more.
func TestAstra6ClearsObsoleteDisagreement(t *testing.T) {
	pps := &SourceState{
		Name: "pps0", Options: Options{PPS: true},
		Reach: 255, Poll: 4, Valid: true, At: 1, Updated: 1,
		Offset: 0.1, Delay: 0, Jitter: 1e-7, Stratum: 0,
	}
	gps := &SourceState{
		Name: "gps", Options: Options{Numbering: true},
		Reach: 255, Poll: 4, Valid: true, At: 1, Updated: 1,
		Offset: 0.0, Delay: 0.001, Jitter: 1e-6, Stratum: 0,
	}
	sel := Select([]*SourceState{pps, gps}, 1, 1)
	if !sel.PPSQualified || pps.DisagreesWith == "" {
		t.Fatalf("setup: qualified=%v disagreement=%q", sel.PPSQualified, pps.DisagreesWith)
	}

	t.Run("numbering loss clears it", func(t *testing.T) {
		gps.Reach, gps.Valid = 0, false
		Select([]*SourceState{pps, gps}, 2, 1)
		if pps.DisagreesWith != "" || pps.Disagreement != 0 {
			t.Fatalf("obsolete diagnostic retained: %q %v", pps.DisagreesWith, pps.Disagreement)
		}
	})

	t.Run("pps reach zero clears it", func(t *testing.T) {
		gps.Reach, gps.Valid = 255, true
		gps.Updated = 3
		pps.Updated = 3
		Select([]*SourceState{pps, gps}, 3, 1)
		if pps.DisagreesWith == "" {
			t.Fatal("setup: no disagreement to clear")
		}
		pps.Reach, pps.Valid = 0, false
		Select([]*SourceState{pps, gps}, 4, 1)
		if pps.DisagreesWith != "" {
			t.Fatalf("an unreachable PPS kept its disagreement: %q", pps.DisagreesWith)
		}
	})

	t.Run("agreement clears it", func(t *testing.T) {
		pps.Reach, pps.Valid, pps.Offset, pps.Updated = 255, true, 0.0, 5
		gps.Updated = 5
		Select([]*SourceState{pps, gps}, 5, 1)
		if pps.DisagreesWith != "" {
			t.Fatalf("an agreeing PPS kept a disagreement: %q", pps.DisagreesWith)
		}
	})

	t.Run("noselect clears it", func(t *testing.T) {
		pps.Offset, pps.Updated, pps.NoSelect = 0.1, 6, false
		gps.Updated = 6
		Select([]*SourceState{pps, gps}, 6, 1)
		if pps.DisagreesWith == "" {
			t.Fatal("setup: no disagreement to clear")
		}
		pps.NoSelect = true
		Select([]*SourceState{pps, gps}, 7, 1)
		if pps.DisagreesWith != "" {
			t.Fatalf("a noselect PPS kept its disagreement: %q", pps.DisagreesWith)
		}
	})
}

// TestAstra6RejectsTimingLoops covers RA6X-038. A peer reporting this host as
// its reference, or a configured self-address, was still eligible: after
// losing a real upstream two mutually configured instances could begin
// selecting each other's retained time while advertising independence.
func TestAstra6RejectsTimingLoops(t *testing.T) {
	me4 := netip.MustParseAddr("192.0.2.10")
	me6 := netip.MustParseAddr("2001:db8::10")
	local := map[ntp.RefID]bool{
		ntp.RefIDFromAddr(me4): true,
		ntp.RefIDFromAddr(me6): true,
	}
	peer := netip.MustParseAddr("192.0.2.20")
	third := netip.MustParseAddr("198.51.100.7")

	src := func(name string, addr netip.Addr, ref ntp.RefID, stratum uint8) *SourceState {
		return &SourceState{
			Name: name, Options: Options{Numbering: true},
			Reach: 255, Poll: 6, Valid: true, At: 1, Updated: 1,
			Offset: 0.001, Delay: 0.01, Jitter: 1e-6, Stratum: stratum,
			SourceRefID: ntp.RefIDFromAddr(addr), RefID: ref,
		}
	}

	cases := []struct {
		name     string
		source   *SourceState
		rejected bool
	}{
		{"a peer synchronized to us", src("peer", peer, ntp.RefIDFromAddr(me4), 2), true},
		{"a peer synchronized to our v6 address", src("peer6", peer, ntp.RefIDFromAddr(me6), 2), true},
		{"our own address configured as a server", src("self", me4, ntp.RefIDFromAddr(third), 2), true},
		{"our own v6 address configured as a server", src("self6", me6, ntp.RefIDFromAddr(third), 2), true},
		{"a peer sharing a third upstream", src("peer", peer, ntp.RefIDFromAddr(third), 2), false},
		{"a stratum-1 peer with a textual refid", src("gps", peer, ntp.RefIDFromString("GPS"), 1), false},
		{"a stratum-1 peer whose textual id looks like our address", src("odd", peer, ntp.RefIDFromString("PPS"), 1), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sel := SelectAt([]*SourceState{c.source}, 1, 1, SelectOptions{LocalRefIDs: local})
			rejected := sel.System == nil
			if rejected != c.rejected {
				t.Fatalf("rejected=%v, want %v (status %v)", rejected, c.rejected, c.source.Status)
			}
			if c.rejected {
				var reported bool
				for _, ev := range sel.Events {
					if ev.Kind == EventTimingLoop && ev.Source == c.source.Name {
						reported = true
					}
				}
				if !reported {
					t.Fatal("a rejected timing loop was not reported")
				}
			}
		})
	}
}

// TestAstra6TimingLoopTransitions checks the event fires once on each
// transition and that recovery is reported.
func TestAstra6TimingLoopTransitions(t *testing.T) {
	me := netip.MustParseAddr("192.0.2.10")
	local := map[ntp.RefID]bool{ntp.RefIDFromAddr(me): true}
	peer := &SourceState{
		Name: "peer", Options: Options{Numbering: true},
		Reach: 255, Poll: 6, Valid: true, At: 1, Updated: 1,
		Offset: 0.001, Delay: 0.01, Jitter: 1e-6, Stratum: 2,
		SourceRefID: ntp.RefIDFromAddr(netip.MustParseAddr("192.0.2.20")),
		RefID:       ntp.RefIDFromAddr(me),
	}
	count := func(sel Selection, kind EventKind) int {
		n := 0
		for _, ev := range sel.Events {
			if ev.Kind == kind {
				n++
			}
		}
		return n
	}
	if got := count(SelectAt([]*SourceState{peer}, 1, 1, SelectOptions{LocalRefIDs: local}), EventTimingLoop); got != 1 {
		t.Fatalf("first selection reported %d loop events, want 1", got)
	}
	if got := count(SelectAt([]*SourceState{peer}, 2, 1, SelectOptions{LocalRefIDs: local}), EventTimingLoop); got != 0 {
		t.Fatalf("a standing loop was reported again: %d events", got)
	}
	peer.RefID = ntp.RefIDFromAddr(netip.MustParseAddr("198.51.100.7"))
	sel := SelectAt([]*SourceState{peer}, 3, 1, SelectOptions{LocalRefIDs: local})
	if got := count(sel, EventTimingLoopCleared); got != 1 {
		t.Fatalf("recovery reported %d times, want 1", got)
	}
	if sel.System != peer {
		t.Fatal("the peer was not usable again once its reference changed")
	}
}

// TestAstra6NoLocalIdentityDisablesTheCheck confirms the check is inert when
// local addresses could not be enumerated.
func TestAstra6NoLocalIdentityDisablesTheCheck(t *testing.T) {
	me := netip.MustParseAddr("192.0.2.10")
	peer := &SourceState{
		Name: "peer", Options: Options{Numbering: true},
		Reach: 255, Poll: 6, Valid: true, At: 1, Updated: 1,
		Offset: 0.001, Delay: 0.01, Jitter: 1e-6, Stratum: 2,
		SourceRefID: ntp.RefIDFromAddr(netip.MustParseAddr("192.0.2.20")),
		RefID:       ntp.RefIDFromAddr(me),
	}
	if sel := Select([]*SourceState{peer}, 1, 1); sel.System != peer {
		t.Fatal("the loop check fired with no local identity configured")
	}
}

// TestAstra6ChargesTheAppliedWord is the review's RA6X-008 probe. Tick
// computed the *next* frequency word and then debited that from the pending
// phase, so the accounting disagreed with what the kernel had actually been
// running.
//
// The probe's original constant, 0.009902343750 s, was derived from the old
// behaviour in which the first tick also charged a nominal second for a
// transient that had not run yet — which the same finding lists as a defect.
// With that corrected the residual is one first-tick charge higher; the rule
// the finding is about, that the debit is the *applied* word, is unchanged
// and is what this asserts.
func TestAstra6ChargesTheAppliedWord(t *testing.T) {
	l := NewLoop(loopCfg(), 0, true)
	l.Update(0.010, 6, 0, false, true) // tau = 256, so the transient is 39.0625 ppm

	l.Tick(1)
	if l.Pending != 0.010 {
		t.Fatalf("the first tick charged a transient that had not run: pending %v", l.Pending)
	}
	applied := l.applied - l.appliedBase
	if math.Abs(applied-39.0625) > 1e-9 {
		t.Fatalf("issued transient %v ppm, want 39.0625", applied)
	}

	l.Tick(2.5) // the 39.0625 ppm word ran for 1.5 s
	want := 0.010 - 39.0625e-6*1.5
	if math.Abs(l.Pending-want) > 1e-15 {
		t.Fatalf("pending %.12f, want %.12f: the debit must be the word that ran", l.Pending, want)
	}
}

// TestAstra6AppliedWordIntegral is the independent oracle RA6X-008 asks for.
// It records every word the loop issues and, at each accounting point,
// requires the phase the loop debited to equal the integral of the word that
// was actually held over the interval it was actually held for — across
// irregular ticks, a late tick past the accounting window, loop updates that
// change the base, a sign change and saturation.
func TestAstra6AppliedWordIntegral(t *testing.T) {
	type event struct {
		at     float64
		update bool
		offset float64
	}
	script := []event{
		{at: 0, update: true, offset: 0.010},
		{at: 1},
		{at: 2},
		{at: 2.5},
		{at: 5}, // a late tick, past maxTickInterval
		{at: 6, update: true, offset: 0.004},
		{at: 6.5},
		{at: 7},
		{at: 9, update: true, offset: -0.002}, // a sign change
		{at: 10},
		{at: 11},
		{at: 11.25}, // a very short tick
		{at: 12, update: true, offset: 0.3},
		{at: 13},
		{at: 14},
	}
	for _, base := range []float64{0, 12.5, -300, 480} {
		t.Run(fmt.Sprintf("base=%v", base), func(t *testing.T) {
			cfg := loopCfg()
			cfg.StepLimit = 0 // slew everything, so nothing is stepped away
			l := NewLoop(cfg, base, true)

			// The oracle's model of the kernel: the word it holds and the
			// base that word was computed against, captured when issued.
			var word, wordBase float64
			var haveWord bool
			at := 0.0

			// expected is the phase the held word moved over [at, now],
			// clamped the same way the loop clamps a stall.
			expected := func(now float64) float64 {
				if !haveWord || now <= at {
					return 0
				}
				return (word - wordBase) * 1e-6 * math.Min(now-at, maxTickInterval)
			}
			record := func(now float64, acts []Action) {
				at = now
				for _, a := range acts {
					if a.Kind == ActionSetFrequency {
						// Tick does not change Freq, so the loop's current
						// base is the one it used for this word.
						word, wordBase, haveWord = a.Value, l.Freq, true
					}
				}
			}

			for _, e := range script {
				before := l.Pending
				want := expected(e.at)
				if e.update {
					u := l.Update(e.offset, 6, e.at, false, true)
					if u.Stepped {
						t.Fatal("this script must not step")
					}
					// The update charges the interval, then replaces the
					// residual; the debit is observable in slewed.
					record(e.at, u.Actions)
					continue
				}
				acts := l.Tick(e.at)
				if got := before - l.Pending; math.Abs(got-want) > 1e-12 {
					t.Fatalf("at %v the loop debited %.15f s, the held word moved %.15f s", e.at, got, want)
				}
				record(e.at, acts)
			}
		})
	}
}

// TestAstra6NoDoubleDebitAcrossUpdate covers the reconciliation RA6X-008
// asks for: an observation's offset already reflects every correction applied
// before it was taken, so the interval up to the update must not be charged
// against the replacement pending phase as well.
func TestAstra6NoDoubleDebitAcrossUpdate(t *testing.T) {
	l := NewLoop(loopCfg(), 0, true)
	l.Update(0.010, 6, 0, false, true)
	l.Tick(1) // issues 39.0625 ppm
	// Ten seconds later a fresh observation arrives. Whatever ran in the
	// meantime is already in its offset.
	l.Update(0.004, 6, 11, false, true)
	if l.Pending != 0.004 {
		t.Fatalf("the replacement pending phase was debited on arrival: %v", l.Pending)
	}
	// The next tick may only charge the interval since the update.
	before := l.Pending
	applied := l.applied - l.appliedBase
	l.Tick(12)
	if want := before - applied*1e-6; math.Abs(l.Pending-want) > 1e-15 {
		t.Fatalf("pending %v, want %v: only the second since the update may be charged", l.Pending, want)
	}
}

// TestAstra6AccountingEdgeCases covers the remaining semantics RA6X-008 asks
// to be defined.
func TestAstra6AccountingEdgeCases(t *testing.T) {
	t.Run("backward time charges nothing", func(t *testing.T) {
		l := NewLoop(loopCfg(), 0, true)
		l.Update(0.010, 6, 0, false, true)
		l.Tick(1)
		l.Tick(2)
		before := l.Pending
		l.Tick(1.5)
		if l.Pending != before {
			t.Fatalf("backward time debited the phase: %v -> %v", before, l.Pending)
		}
	})

	t.Run("a step restarts the accounting", func(t *testing.T) {
		cfg := loopCfg()
		l := NewLoop(cfg, 0, true)
		l.Update(0.010, 6, 0, false, true)
		l.Tick(1)
		u := l.Update(1.0, 6, 100, false, true)
		if !u.Stepped {
			t.Fatal("setup: the offset was not stepped")
		}
		if l.Pending != 0 {
			t.Fatalf("a step left pending phase behind: %v", l.Pending)
		}
		// The first tick after a step re-issues the base and charges the
		// interval since the step, not since before it.
		l.Tick(101)
		if l.Pending != 0 {
			t.Fatalf("the tick after a step invented pending phase: %v", l.Pending)
		}
	})

	t.Run("the total stays within the frequency bound", func(t *testing.T) {
		cfg := loopCfg()
		cfg.StepLimit = 0
		l := NewLoop(cfg, 499, true)
		l.Update(10, 6, 0, false, true)
		for i := 1; i <= 20; i++ {
			for _, a := range l.Tick(float64(i)) {
				if a.Kind == ActionSetFrequency && (a.Value > MaxFrequency || a.Value < -MaxFrequency) {
					t.Fatalf("issued %v ppm, outside ±%v", a.Value, MaxFrequency)
				}
			}
		}
	})

	t.Run("no base-only pulse between updates", func(t *testing.T) {
		l := NewLoop(loopCfg(), 12.5, true)
		u := l.Update(0.010, 6, 0, false, true)
		for _, a := range u.Actions {
			if a.Kind == ActionSetFrequency {
				t.Fatalf("an update issued a frequency action: %+v", u.Actions)
			}
		}
	})
}

// TestAstra6ObservationsArePropagated covers the core of RA6X-001. The clock
// filter can release an observation several polls old, and feeding its offset
// to the loop as present-time feedback re-integrates a correction the loop has
// already made. Selection now expresses every stored offset as the error
// remaining *now*.
func TestAstra6ObservationsArePropagated(t *testing.T) {
	// A loop that has corrected 4 ms of phase between t=0 and t=100.
	applied := func(at, now float64) float64 {
		if now <= at {
			return 0
		}
		clamp := func(v float64) float64 { return math.Max(0, math.Min(100, v)) }
		return (clamp(now) - clamp(at)) * 4e-3 / 100
	}
	src := &SourceState{
		Name: "a", Options: Options{Numbering: true},
		Reach: 255, Poll: 6, Valid: true, At: 0, Updated: 0,
		Offset: 0.010, Delay: 0.01, Jitter: 1e-6, Stratum: 2,
	}
	sel := SelectAt([]*SourceState{src}, 100, 1, SelectOptions{AppliedSince: applied})
	if sel.System != src {
		t.Fatal("the source was not selected")
	}
	if want := 0.010 - 4e-3; math.Abs(sel.Offset-want) > 1e-12 {
		t.Fatalf("combined offset %v, want %v: the correction already applied must be subtracted", sel.Offset, want)
	}
	if src.Offset != 0.010 {
		t.Fatalf("the stored estimate was mutated: %v", src.Offset)
	}
	// Without the propagation the raw offset is used, which is the defect.
	plain := Select([]*SourceState{src}, 100, 1)
	if math.Abs(plain.Offset-0.010) > 1e-12 {
		t.Fatalf("with no applied-phase information the raw offset must be used, got %v", plain.Offset)
	}
}

// TestAstra6PropagationUsesTheAppliedPhase checks the loop's own record is
// what selection consumes, end to end: after the loop has slewed part of an
// offset away, the residual it is told about is the part that remains.
func TestAstra6PropagationUsesTheAppliedPhase(t *testing.T) {
	l := NewLoop(loopCfg(), 0, true)
	l.Update(0.010, 6, 0, false, true)
	for i := 1; i <= 20; i++ {
		l.Tick(float64(i))
	}
	moved := 0.010 - l.Pending
	if moved <= 0 {
		t.Fatal("the loop slewed nothing")
	}
	if got := l.AppliedSince(0, 20); math.Abs(got-moved) > 1e-12 {
		t.Fatalf("AppliedSince(0,20) = %v, but the loop moved %v", got, moved)
	}
	// An interval entirely inside the run is a proper subset.
	part := l.AppliedSince(5, 10)
	if part <= 0 || part >= moved {
		t.Fatalf("AppliedSince(5,10) = %v, outside (0, %v)", part, moved)
	}
	if l.AppliedSince(20, 5) != 0 {
		t.Fatal("a backward interval must be zero")
	}
	if l.AppliedSince(10, 10) != 0 {
		t.Fatal("an empty interval must be zero")
	}
}

// TestAstra6FilterCadence covers RA6X-002: the run of polls during which the
// clock filter withholds every output — and the discipline loop therefore
// never runs — must be bounded by the point at which a stale sample's grown
// dispersion outweighs half its delay advantage, not by the Allan intercept.
func TestAstra6FilterCadence(t *testing.T) {
	for _, poll := range []float64{16, 64, 256, 1024} {
		t.Run(fmt.Sprintf("poll=%.0fs", poll), func(t *testing.T) {
			f := NewFilter(1e-6)
			// One unusually fast reply, then ordinary ones 20 ms slower.
			if _, ok := f.Add(0.001, 0.010, 1e-4, poll); !ok {
				t.Fatal("the first sample must update")
			}
			last, maxGap := poll, 0.0
			for i := 2; i <= 24; i++ {
				now := float64(i) * poll
				if _, ok := f.Add(0.001, 0.030, 1e-4, now); ok {
					if gap := now - last; gap > maxGap {
						maxGap = gap
					}
					last = now
				}
			}
			// The 20 ms delay advantage is worth 10 ms to the offset
			// estimate, so the crossover is at φ·age = 10 ms.
			crossover := 10e-3 / Phi
			if maxGap > crossover+poll {
				t.Fatalf("withheld for %.0f s, past the %.0f s crossover", maxGap, crossover)
			}
			if maxGap >= AllanIntercept {
				t.Fatalf("withheld for %.0f s, still bounded by the Allan intercept", maxGap)
			}
		})
	}
}

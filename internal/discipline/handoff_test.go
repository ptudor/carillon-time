package discipline

import (
	"math"
	"testing"
)

func TestShorterPollOnSameSource(t *testing.T) {
	l := NewLoop(loopCfg(), 0, true)
	l.Update(0, 10, 100, true, false)
	u := l.Update(-0.001, 4, 1720, true, false)
	if u.Mu != 1620 || l.Pending != -0.001 || l.Freq != -0.9765625 {
		t.Fatalf("shorter poll: elapsed %v, phase %v, frequency %v", u.Mu, l.Pending, l.Freq)
	}
}

func TestFrequencyAfterSourceHandoff(t *testing.T) {
	const initial = -10.23
	s := New(Config{Loop: loopCfg(), SettleUpdates: 1}, initial, true)
	s.AddSource("fast", Options{Prefer: true, Numbering: true})
	s.AddSource("slow", Options{Numbering: true})
	input := func(name string, now, offset float64, poll int8) {
		s.Update(Measurement{Source: name, Now: now, At: now, Valid: true, Reach: 255,
			Poll: poll, Stratum: 2, Offset: offset, Delay: 0.01, Jitter: 1e-6})
	}
	input("fast", 99, 0, 4)
	input("fast", 100, 0, 4)
	input("slow", 101, 0.003, 10)
	// The fast source goes away. The slow source wins selection, but its
	// offset is rejected by the spike gate. Then the fast source returns.
	s.Update(Measurement{Source: "fast", Now: 102, Poll: 4, Invalidate: true})
	if st := s.Status(102); st.SystemSource != "slow" || st.LastUpdate != 100 {
		t.Fatalf("switch without accepted input: %+v", st)
	}
	s.Tick(103)
	if st := s.Status(110); st.UpdateSource != "fast" || st.UpdateAge != 10 || st.UpdateReason != "offset spike rejected" {
		t.Fatalf("last accepted source after rejected selection: %+v", st)
	}
	input("fast", 1720, -0.001, 4)
	st := s.Status(1720)
	// A single new 16-second sample can contribute at most one interval
	// to the frequency estimate. Phase correction starts immediately.
	if st.UpdateSource != "fast" || st.UpdateAge != 0 || st.UpdateReason != "accepted" {
		t.Fatalf("accepted handoff: %+v", st)
	}
	s.Update(Measurement{Source: "fast", Now: 1721, Poll: 4, Reach: 255, Acquired: true})
	if st := s.Status(1721); st.UpdateReason != "filter sample already used" || st.UpdateAge != 1 {
		t.Fatalf("unchanged filter winner: %+v", st)
	}
	want := -0.001 * 16 / (4 * 64 * 64) * 1e6
	if delta := st.Frequency - initial; math.Abs(delta-want) > 1e-8 || st.Pending != -0.001 || st.LastUpdate != 1720 {
		t.Fatalf("27-minute gap: base change %+.6f ppm, want %+.6f; pending %.6f s", delta, want, st.Pending)
	}
	input("fast", 1752, -0.001, 4)
	if delta := s.Frequency() - st.Frequency; math.Abs(delta-2*want) > 1e-8 {
		t.Fatalf("next independent sample: frequency change %.6f ppm, want %.6f", delta, 2*want)
	}
}

package refclock

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"carillon/internal/discipline"
	"carillon/internal/pps"
)

// TestAstra6NMEAFarFutureSentenceIsRejected covers the refclock half of
// RA6X-041. time.Time.Sub saturates at ±(1<<63 - 1) ns instead of reporting
// an overflow, so a checksum-valid sentence dated far enough ahead used to
// produce an offset of about ±292 years that looked like a measurement.
func TestAstra6NMEAFarFutureSentenceIsRejected(t *testing.T) {
	n, _, _ := testNMEA(t)
	arrival := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	if m, ok := n.acceptLine(sentence("GPZDA,120000,23,08,9999,00,00"), arrival, 1); ok || m.Valid {
		t.Fatalf("year 9999 sentence accepted with offset %g s", m.Offset)
	}
}

// TestAstra6NMEAEpochBootstrapIsUsable is the other side of RA6X-041's
// verification: a host whose RTC is stuck near the epoch needs a correction
// of about 56 years, which is representable and must remain measurable.
func TestAstra6NMEAEpochBootstrapIsUsable(t *testing.T) {
	n, _, _ := testNMEA(t)
	arrival := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	m, ok := n.acceptLine(sentence("GPZDA,120000,23,08,2026,00,00"), arrival, 1)
	if !ok {
		t.Fatal("a legitimate epoch-RTC bootstrap sentence was rejected")
	}
	want := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC).Sub(arrival).Seconds() + n.cfg.Offset
	if m.Offset != want {
		t.Fatalf("offset %g s, want %g s", m.Offset, want)
	}
}

// astraSerial is a reader whose every read fails and whose Close runs a
// callback, so a test can cancel the context from inside the reconnect path.
type astraSerial struct{ close func() }

func (r *astraSerial) ReadTimeout([]byte, time.Duration) (int, error) {
	return 0, errors.New("device disconnected")
}
func (r *astraSerial) Close() error { r.close(); return nil }

// TestAstra6NMEACancelDuringReconnect is the review's RA6X-018 probe. reopen
// returned nil after cancellation while leaving the reader nil, so Run looped
// and dereferenced it; the panic in a source goroutine took the whole daemon
// down, skipping base-frequency restoration and the statistics flush.
func TestAstra6NMEACancelDuringReconnect(t *testing.T) {
	n, _, _ := testNMEA(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.reader = &astraSerial{close: cancel}
	n.opener = func() (serialReader, error) { return nil, errors.New("absent") }
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("cancellation panicked: %v", p)
		}
	}()
	if err := n.Run(ctx, make(chan discipline.Measurement, 1)); err != nil {
		t.Fatal(err)
	}
}

// TestAstra6NMEACancellationPoints covers RA6X-018's verification list: every
// place cancellation can land must be a clean, prompt stop with no panic.
func TestAstra6NMEACancellationPoints(t *testing.T) {
	t.Run("before Run", func(t *testing.T) {
		n, _, _ := testNMEA(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		n.reader = &astraSerial{close: func() {}}
		n.opener = func() (serialReader, error) { return nil, errors.New("absent") }
		if err := runNMEA(t, n, ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("during a read", func(t *testing.T) {
		n, _, _ := testNMEA(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		n.reader = &cancellingReader{cancel: cancel}
		n.opener = func() (serialReader, error) { return nil, errors.New("absent") }
		if err := runNMEA(t, n, ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("during backoff", func(t *testing.T) {
		n, _, _ := testNMEA(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		n.reader = &astraSerial{close: func() {}}
		// The opener cancels the first time it is consulted, which is
		// after the first backoff timer has fired.
		n.opener = func() (serialReader, error) {
			cancel()
			return nil, errors.New("absent")
		}
		if err := runNMEA(t, n, ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("just after a successful reopen", func(t *testing.T) {
		n, _, _ := testNMEA(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		n.reader = &astraSerial{close: func() {}}
		reopened := 0
		n.opener = func() (serialReader, error) {
			reopened++
			cancel()
			return &astraSerial{close: func() {}}, nil
		}
		if err := runNMEA(t, n, ctx); err != nil {
			t.Fatal(err)
		}
		if reopened != 1 {
			t.Fatalf("opener called %d times after cancellation", reopened)
		}
	})

	t.Run("no opener", func(t *testing.T) {
		n, _, _ := testNMEA(t)
		n.reader = &astraSerial{close: func() {}}
		n.opener = nil
		if err := runNMEA(t, n, context.Background()); err == nil {
			t.Fatal("an unreopenable device must be reported as an error")
		}
	})
}

// cancellingReader cancels the context from inside ReadTimeout, then reports
// a device error.
type cancellingReader struct{ cancel context.CancelFunc }

func (r *cancellingReader) ReadTimeout([]byte, time.Duration) (int, error) {
	r.cancel()
	return 0, errors.New("device disconnected")
}
func (r *cancellingReader) Close() error { return nil }

// runNMEA runs the source and fails the test if it neither returns nor
// panics within a few seconds. reopen's first backoff is one second.
func runNMEA(t *testing.T, n *NMEA, ctx context.Context) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- fmt.Errorf("panic: %v", p)
			}
		}()
		done <- n.Run(ctx, make(chan discipline.Measurement, 8))
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
		return nil
	}
}

// astraPPSReader is a fake PPS device: it delivers scripted samples, then
// fails on every subsequent fetch.
type astraPPSReader struct {
	samples []pps.Sample
	i       int
	err     error
}

func (r *astraPPSReader) Fetch(time.Duration) (pps.Sample, error) {
	if r.i < len(r.samples) {
		s := r.samples[r.i]
		r.i++
		return s, nil
	}
	return pps.Sample{}, r.err
}
func (r *astraPPSReader) Close() error { return nil }

// TestAstra6PPSResetInvalidatesTheEstimate covers RA6X-005. The
// consecutive-rejection and sequence-restart recovery paths emptied the
// window and cleared stability but emitted an ordinary invalid measurement,
// which by design *retains* the previous estimate in SourceState. Reach was
// still nonzero, so an unlocked PPS stayed selected on a window it no longer
// had.
func TestAstra6PPSResetInvalidatesTheEstimate(t *testing.T) {
	base := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	// runPPS drives the real Run loop over a scripted pulse train and
	// returns every measurement it emitted, so the assertion is made on
	// what the engine actually receives.
	runPPS := func(t *testing.T, samples []pps.Sample) []discipline.Measurement {
		t.Helper()
		p, _ := testPPS(t)
		p.reader = &astraPPSReader{samples: samples, err: errors.New("done")}
		p.opener = nil // a hard error after the script ends stops Run
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out := make(chan discipline.Measurement, 64)
		done := make(chan error, 1)
		go func() { done <- p.Run(ctx, out) }()
		<-done
		close(out)
		var got []discipline.Measurement
		for m := range out {
			got = append(got, m)
		}
		return got
	}

	t.Run("rejection run", func(t *testing.T) {
		var samples []pps.Sample
		for i := 1; i <= 8; i++ {
			samples = append(samples, pps.Sample{Sequence: uint32(i), Time: base.Add(time.Duration(i) * time.Second)})
		}
		// Four consecutive spikes re-prime the window.
		for i := 9; i <= 12; i++ {
			samples = append(samples, pps.Sample{Sequence: uint32(i), Time: base.Add(time.Duration(i)*time.Second + 500*time.Millisecond)})
		}
		got := runPPS(t, samples)
		if len(got) < len(samples) {
			t.Fatalf("only %d of %d pulses were emitted", len(got), len(samples))
		}
		if !got[len(samples)-1].Invalidate {
			t.Fatal("a re-primed PPS window emitted a measurement that keeps the old estimate")
		}
	})

	t.Run("sequence restart", func(t *testing.T) {
		var samples []pps.Sample
		for i := 1; i <= 8; i++ {
			samples = append(samples, pps.Sample{Sequence: uint32(i), Time: base.Add(time.Duration(i) * time.Second)})
		}
		samples = append(samples, pps.Sample{Sequence: 1 << 20, Time: base.Add(9 * time.Second)})
		got := runPPS(t, samples)
		if len(got) < len(samples) {
			t.Fatalf("only %d of %d pulses were emitted", len(got), len(samples))
		}
		if !got[len(samples)-1].Invalidate {
			t.Fatal("a PPS sequence restart emitted a measurement that keeps the old estimate")
		}
	})

	t.Run("an ordinary pulse does not invalidate", func(t *testing.T) {
		var samples []pps.Sample
		for i := 1; i <= 8; i++ {
			samples = append(samples, pps.Sample{Sequence: uint32(i), Time: base.Add(time.Duration(i) * time.Second)})
		}
		got := runPPS(t, samples)
		for i, m := range got[1:len(samples)] {
			if m.Invalidate {
				t.Fatalf("pulse %d invalidated the estimate for no reason: %+v", i+1, m)
			}
		}
	})
}

// TestAstra6PPSDeviceLossIsReported covers RA6X-004. A hard device error used
// to enter the reconnect loop without telling the engine anything: no reach
// zero, no invalidation, so a disconnected PPS could stay system source for
// as long as the opener kept failing.
func TestAstra6PPSDeviceLossIsReported(t *testing.T) {
	p, _ := testPPS(t)
	base := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	var samples []pps.Sample
	for i := 1; i <= 8; i++ {
		samples = append(samples, pps.Sample{Sequence: uint32(i), Time: base.Add(time.Duration(i) * time.Second)})
	}
	p.reader = &astraPPSReader{samples: samples, err: errors.New("device disconnected")}
	p.opener = func() (pps.Reader, error) { return nil, errors.New("absent") }

	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan discipline.Measurement, 32)
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, out) }()

	var loss discipline.Measurement
	deadline := time.After(10 * time.Second)
	for loss.Source == "" {
		select {
		case m := <-out:
			if m.Invalidate {
				loss = m
			}
		case <-deadline:
			cancel()
			<-done
			t.Fatal("no invalidating measurement was emitted for a lost device")
		}
	}
	if loss.Reach != 0 {
		t.Fatalf("device loss reported reach %08b, want 0", loss.Reach)
	}
	if got := p.Info().LastError; got == "" {
		t.Fatal("health must keep explaining the outage while the device is gone")
	}
	cancel()
	<-done
}

// TestAstra6NMEASilenceRequiresFreshWindow is the review's RA6X-019 probe.
// Ordinary silence emptied reach without clearing the offset window, so one
// fresh sentence immediately requalified a mostly historical window and
// stamped its median — computed before the outage — as current.
func TestAstra6NMEASilenceRequiresFreshWindow(t *testing.T) {
	n, _, _ := testNMEA(t)
	base := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	feed := func(stamp time.Time, mono float64, offset time.Duration) discipline.Measurement {
		m, ok := n.acceptLine(sentence("GPRMC,"+stamp.Format("150405")+",A,,,,,,,"+stamp.Format("020106")+",,,"), stamp.Add(150*time.Millisecond-offset), mono)
		if !ok {
			t.Fatal("sentence rejected")
		}
		return m
	}
	for i := 0; i < 16; i++ {
		feed(base.Add(time.Duration(i)*time.Second), float64(i+1), 0)
	}
	n.tick(100)
	if n.reach != 0 {
		t.Fatalf("setup reach=%d", n.reach)
	}
	m := feed(base.Add(100*time.Second), 101, 100*time.Millisecond)
	if m.Valid {
		t.Fatalf("one fresh sample after silence is valid with historical offset=%g, current=0.1", m.Offset)
	}
}

// TestAstra6NMEAFreshnessCases covers the rest of RA6X-019's list.
func TestAstra6NMEAFreshnessCases(t *testing.T) {
	base := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	prime := func(t *testing.T, n *NMEA, count int) {
		t.Helper()
		for i := 0; i < count; i++ {
			stamp := base.Add(time.Duration(i) * time.Second)
			if _, ok := n.acceptLine(sentence("GPRMC,"+stamp.Format("150405")+",A,,,,,,,"+stamp.Format("020106")+",,,"), stamp.Add(150*time.Millisecond), float64(i+1)); !ok {
				t.Fatal("setup sentence rejected")
			}
		}
	}

	t.Run("short packet loss keeps the window", func(t *testing.T) {
		n, _, _ := testNMEA(t)
		prime(t, n, 8)
		// Two missed slots: reach is still nonzero, so the window stands.
		n.tick(11)
		if n.reach == 0 {
			t.Fatalf("setup: reach emptied too soon (%08b)", n.reach)
		}
		if len(n.offsets) == 0 {
			t.Fatal("ordinary short packet loss dropped the window")
		}
	})

	t.Run("a long gap without a tick drops the window", func(t *testing.T) {
		n, _, _ := testNMEA(t)
		prime(t, n, 8)
		// A sentence arriving 40 slots later, with no intervening tick.
		stamp := base.Add(40 * time.Second)
		if _, ok := n.acceptLine(sentence("GPRMC,"+stamp.Format("150405")+",A,,,,,,,"+stamp.Format("020106")+",,,"), stamp.Add(150*time.Millisecond), 41); !ok {
			t.Fatal("sentence rejected")
		}
		if len(n.offsets) != 1 {
			t.Fatalf("window holds %d samples after a long gap, want only the fresh one", len(n.offsets))
		}
	})

	t.Run("requalification needs the full minimum", func(t *testing.T) {
		n, _, _ := testNMEA(t)
		prime(t, n, 16)
		n.tick(100)
		var valid []bool
		for i := 0; i < 4; i++ {
			stamp := base.Add(time.Duration(100+i) * time.Second)
			m, ok := n.acceptLine(sentence("GPRMC,"+stamp.Format("150405")+",A,,,,,,,"+stamp.Format("020106")+",,,"), stamp.Add(150*time.Millisecond), float64(101+i))
			if !ok {
				t.Fatal("sentence rejected")
			}
			valid = append(valid, m.Valid)
		}
		if valid[0] || valid[1] || valid[2] {
			t.Fatalf("requalified before four fresh sentences: %v", valid)
		}
		if !valid[3] {
			t.Fatalf("four fresh sentences did not requalify: %v", valid)
		}
	})
}

// TestAstra6NMEARecoversAfterFutureDate is the review's RA6X-020 probe. One
// checksum-valid future date became the anti-replay watermark before the
// engine had accepted or refused the correction, and every later correct
// timestamp was silently dropped — through window resets and successful
// reconnects alike.
func TestAstra6NMEARecoversAfterFutureDate(t *testing.T) {
	n, _, _ := testNMEA(t)
	arrival := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	n.acceptLine(sentence("GPZDA,120000,23,08,2099,00,00"), arrival, 1)
	n.resetWindow()
	if _, ok := n.acceptLine(sentence("GPZDA,120001,23,08,2026,00,00"), arrival.Add(time.Second), 2); !ok {
		t.Fatalf("correct time rejected after reset; lastStamp=%v", n.lastStamp)
	}
}

// TestAstra6NMEAChronology covers the rest of RA6X-020's list.
func TestAstra6NMEAChronology(t *testing.T) {
	zda := func(stamp time.Time) string {
		return sentence("GPZDA," + stamp.Format("150405") + "," + stamp.Format("02,01,2006") + ",00,00")
	}

	t.Run("a mid-run glitch does not poison the watermark", func(t *testing.T) {
		n, _, _ := testNMEA(t)
		base := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
		for i := 0; i < 4; i++ {
			stamp := base.Add(time.Duration(i) * time.Second)
			if _, ok := n.acceptLine(zda(stamp), stamp.Add(150*time.Millisecond), float64(i+1)); !ok {
				t.Fatal("setup sentence rejected")
			}
		}
		glitch := base.AddDate(73, 0, 0)
		if _, ok := n.acceptLine(zda(glitch), base.Add(4*time.Second+150*time.Millisecond), 5); ok {
			t.Fatal("an implausible forward jump was accepted on its own")
		}
		next := base.Add(5 * time.Second)
		if _, ok := n.acceptLine(zda(next), next.Add(150*time.Millisecond), 6); !ok {
			t.Fatalf("correct time rejected after a single glitch; lastStamp=%v", n.lastStamp)
		}
	})

	t.Run("a corroborated era change is adopted", func(t *testing.T) {
		n, _, _ := testNMEA(t)
		base := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
		for i := 0; i < 4; i++ {
			stamp := base.Add(time.Duration(i) * time.Second)
			if _, ok := n.acceptLine(zda(stamp), stamp.Add(150*time.Millisecond), float64(i+1)); !ok {
				t.Fatal("setup sentence rejected")
			}
		}
		// A replacement receiver whose time is legitimately elsewhere.
		era := base.AddDate(0, 0, 5)
		var accepted int
		for i := 0; i < nmeaEraConfirm; i++ {
			stamp := era.Add(time.Duration(i) * time.Second)
			if _, ok := n.acceptLine(zda(stamp), stamp.Add(150*time.Millisecond), float64(10+i)); ok {
				accepted++
			}
		}
		if accepted != 1 {
			t.Fatalf("%d of %d era sentences accepted; only the corroborating one may be", accepted, nmeaEraConfirm)
		}
		if !n.lastStamp.Equal(era.Add(time.Duration(nmeaEraConfirm-1) * time.Second)) {
			t.Fatalf("era not adopted after corroboration; lastStamp=%v", n.lastStamp)
		}
	})

	t.Run("duplicate seconds are still suppressed", func(t *testing.T) {
		n, _, _ := testNMEA(t)
		stamp := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
		if _, ok := n.acceptLine(zda(stamp), stamp.Add(150*time.Millisecond), 1); !ok {
			t.Fatal("setup sentence rejected")
		}
		if _, ok := n.acceptLine(zda(stamp), stamp.Add(160*time.Millisecond), 2); ok {
			t.Fatal("a repeated second was accepted")
		}
	})

	t.Run("a device replacement re-establishes ordering", func(t *testing.T) {
		n, _, _ := testNMEA(t)
		stamp := time.Date(2099, 8, 23, 12, 0, 0, 0, time.UTC)
		if _, ok := n.acceptLine(zda(stamp), time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC), 1); !ok {
			t.Fatal("setup sentence rejected")
		}
		n.deviceLost()
		good := time.Date(2026, 8, 23, 12, 0, 1, 0, time.UTC)
		if _, ok := n.acceptLine(zda(good), good.Add(150*time.Millisecond), 2); !ok {
			t.Fatalf("correct time rejected after a device replacement; lastStamp=%v", n.lastStamp)
		}
	})
}

// TestAstra6NMEALeapBoundaryIsConsistent covers RA6X-042. RMC validated its
// calendar date before time.Date normalised 23:59:60 into the next midnight
// and so accepted it as an ordinary next-day sample colliding with the real
// midnight; ZDA validated the normalised date and rejected it. The same
// instant was treated two different ways.
func TestAstra6NMEALeapBoundaryIsConsistent(t *testing.T) {
	cases := []struct {
		name string
		line string
		ok   bool
	}{
		{"RMC 23:59:59", "GPRMC,235959,A,,,,,,,300626,,,", true},
		{"ZDA 23:59:59", "GPZDA,235959,30,06,2026,00,00", true},
		{"RMC 23:59:60", "GPRMC,235960,A,,,,,,,300626,,,", false},
		{"ZDA 23:59:60", "GPZDA,235960,30,06,2026,00,00", false},
		{"RMC 00:00:00", "GPRMC,000000,A,,,,,,,010726,,,", true},
		{"ZDA 00:00:00", "GPZDA,000000,01,07,2026,00,00", true},
		{"RMC second 60 at an arbitrary minute", "GPRMC,120060,A,,,,,,,300626,,,", false},
		{"ZDA second 60 at an arbitrary minute", "GPZDA,120060,30,06,2026,00,00", false},
		{"RMC ordinary day 23:59:60", "GPRMC,235960,A,,,,,,,150826,,,", false},
		{"fraction with a malformed suffix", "GPZDA,120000.123456789abc,30,06,2026,00,00", false},
		{"fraction of supported precision", "GPZDA,120000.123456789,30,06,2026,00,00", true},
		{"fraction longer than supported precision", "GPZDA,120000.1234567891,30,06,2026,00,00", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseNMEA(sentence(c.line))
			if c.ok && err != nil {
				t.Fatalf("valid sentence rejected: %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("invalid sentence accepted")
			}
		})
	}
}

// TestAstra6TrueMidnightStaysUsable checks the leap-boundary refusal did not
// cost the real midnight sample either side of it.
func TestAstra6TrueMidnightStaysUsable(t *testing.T) {
	s, err := parseNMEA(sentence("GPZDA,000000,01,07,2026,00,00"))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if !s.Timestamp.Equal(want) {
		t.Fatalf("midnight parsed as %v, want %v", s.Timestamp, want)
	}
}

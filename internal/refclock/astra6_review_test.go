package refclock

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"carillon/internal/discipline"
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

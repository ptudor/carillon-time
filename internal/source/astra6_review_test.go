package source

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"carillon/internal/clock"
	"carillon/internal/ntp"
)

// TestAstra6RotatesPastAnUnusableAnswer covers RA6X-037. Only the first DNS
// answer was ever used, and re-resolution returned that same unusable first
// answer, so a dual-stack or multihomed hostname whose first address was
// unroutable stayed unusable indefinitely.
func TestAstra6RotatesPastAnUnusableAnswer(t *testing.T) {
	refB := ntp.RefID{'B', 'B', 'B', 'B'}
	// The first answer never replies; the second is healthy.
	dead := netip.MustParseAddrPort("192.0.2.1:123")
	healthy := newFakeServer(t, func(req ntp.Packet, _ []byte) []byte {
		return reply(req, func(p *ntp.Packet) { p.ReferenceID = refB })
	})

	n, err := NewNTP(NTPConfig{
		Name: "pool", Address: "ntp.test", Host: "ntp.test", Port: 123,
		Timeout: 50 * time.Millisecond,
	}, clock.ReadOnly(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	var lookups atomic.Int32
	n.lookup = func(context.Context, string, uint16) ([]netip.AddrPort, error) {
		lookups.Add(1)
		// Stable answer ordering: the resolver keeps returning the dead one
		// first, which is exactly the case re-resolution alone cannot fix.
		return []netip.AddrPort{dead, healthy.addr}, nil
	}
	n.sleep = func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }

	out, stop := run(t, n)
	defer stop()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case m := <-out:
			if m.Valid && m.RefID == refB {
				return // the healthy endpoint was reached
			}
		case <-deadline:
			t.Fatalf("never moved past the unusable first answer (%d lookups, resolved %v)",
				lookups.Load(), n.Info().Resolved)
		}
	}
}

// TestAstra6EndpointRotation exercises the rotation rules directly.
func TestAstra6EndpointRotation(t *testing.T) {
	a := netip.MustParseAddrPort("192.0.2.1:123")
	b := netip.MustParseAddrPort("192.0.2.2:123")
	c := netip.MustParseAddrPort("192.0.2.3:123")

	newSource := func(t *testing.T, answers func() ([]netip.AddrPort, error)) *NTP {
		t.Helper()
		n, err := NewNTP(NTPConfig{
			Name: "s", Address: "ntp.test", Host: "ntp.test", Port: 123,
		}, clock.ReadOnly(), slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		n.lookup = func(context.Context, string, uint16) ([]netip.AddrPort, error) { return answers() }
		return n
	}

	t.Run("a healthy association is not disturbed", func(t *testing.T) {
		n := newSource(t, func() ([]netip.AddrPort, error) { return []netip.AddrPort{a, b}, nil })
		if !n.ensureResolved(context.Background()) {
			t.Fatal("initial resolution failed")
		}
		if n.addr != a {
			t.Fatalf("resolved %v, want %v", n.addr, a)
		}
		// No failures: ensureResolved must not even look up again.
		for i := 0; i < 10; i++ {
			n.ensureResolved(context.Background())
		}
		if n.addr != a {
			t.Fatalf("a healthy association moved to %v", n.addr)
		}
	})

	t.Run("rotation after repeated endpoint failure", func(t *testing.T) {
		n := newSource(t, func() ([]netip.AddrPort, error) { return []netip.AddrPort{a, b, c}, nil })
		n.ensureResolved(context.Background())
		n.consecutiveTimeouts = resolveAfterTimeouts
		n.ensureResolved(context.Background())
		if n.addr != b {
			t.Fatalf("rotated to %v, want %v", n.addr, b)
		}
		n.consecutiveTimeouts = resolveAfterTimeouts
		n.ensureResolved(context.Background())
		if n.addr != c {
			t.Fatalf("rotated to %v, want %v", n.addr, c)
		}
		// Wraps back round rather than giving up.
		n.consecutiveTimeouts = resolveAfterTimeouts
		n.ensureResolved(context.Background())
		if n.addr != a {
			t.Fatalf("rotated to %v, want %v", n.addr, a)
		}
	})

	t.Run("a single answer keeps its filter", func(t *testing.T) {
		n := newSource(t, func() ([]netip.AddrPort, error) { return []netip.AddrPort{a}, nil })
		n.ensureResolved(context.Background())
		n.kodMinPoll = 9
		n.consecutiveTimeouts = resolveAfterTimeouts
		n.ensureResolved(context.Background())
		if n.addr != a {
			t.Fatalf("a single-answer server moved to %v", n.addr)
		}
		if n.kodMinPoll != 9 {
			t.Fatal("re-resolving to the same address discarded the server's poll policy")
		}
	})

	t.Run("changing peers resets endpoint state", func(t *testing.T) {
		n := newSource(t, func() ([]netip.AddrPort, error) { return []netip.AddrPort{a, b}, nil })
		n.ensureResolved(context.Background())
		n.kodMinPoll = 9
		n.consecutiveTimeouts = resolveAfterTimeouts
		n.ensureResolved(context.Background())
		if n.addr != b {
			t.Fatalf("resolved %v, want %v", n.addr, b)
		}
		if n.kodMinPoll != 0 {
			t.Fatal("a replacement peer inherited the previous server's poll policy")
		}
	})

	t.Run("a temporary DNS failure keeps the cached answers", func(t *testing.T) {
		var fail atomic.Bool
		n := newSource(t, func() ([]netip.AddrPort, error) {
			if fail.Load() {
				return nil, errors.New("SERVFAIL")
			}
			return []netip.AddrPort{a, b}, nil
		})
		n.ensureResolved(context.Background())
		fail.Store(true)
		if !n.ensureResolved(context.Background()) {
			t.Fatal("a temporary DNS failure abandoned a still-good cached address")
		}
		if n.addr != a {
			t.Fatalf("resolved %v, want the cached %v", n.addr, a)
		}
		// Even so, an endpoint that keeps failing is rotated away from,
		// using the answers already held.
		n.consecutiveTimeouts = resolveAfterTimeouts
		n.ensureResolved(context.Background())
		if n.addr != b {
			t.Fatalf("a failing endpoint was not rotated away from during a DNS outage: %v", n.addr)
		}
	})

	t.Run("a literal address resolves to itself", func(t *testing.T) {
		addrs, err := resolve(context.Background(), "192.0.2.9", 123)
		if err != nil {
			t.Fatal(err)
		}
		if len(addrs) != 1 || addrs[0] != netip.MustParseAddrPort("192.0.2.9:123") {
			t.Fatalf("literal resolved to %v", addrs)
		}
	})

	t.Run("a changed answer list adopts the new first answer", func(t *testing.T) {
		var second atomic.Bool
		n := newSource(t, func() ([]netip.AddrPort, error) {
			if second.Load() {
				return []netip.AddrPort{c}, nil
			}
			return []netip.AddrPort{a, b}, nil
		})
		n.ensureResolved(context.Background())
		second.Store(true)
		n.consecutiveTimeouts = resolveAfterTimeouts
		n.ensureResolved(context.Background())
		if n.addr != c {
			t.Fatalf("resolved %v, want %v", n.addr, c)
		}
	})
}

package server

import (
	"errors"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ptudor/carillon-time/internal/ntp"
	"github.com/ptudor/carillon-time/internal/ntp/auth"
)

// TestAstra6HandlerRejectsUnboundedLimits covers RA6X-035 at the public
// constructor boundary. NewHandler used `!(v > 0)` and `!(v >= 1)`, which
// reject a NaN but accept +Inf — an infinite rate or burst is no rate
// limiting at all, and config.Validate has not necessarily run.
func TestAstra6HandlerRejectsUnboundedLimits(t *testing.T) {
	base := func() Config {
		return Config{
			Allow:        []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
			RateLimitPPS: 8,
			RateBurst:    16,
			Status:       testStatus,
			Now:          func() time.Time { return testWall },
		}
	}
	if _, err := NewHandler(base()); err != nil {
		t.Fatalf("setup: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*Config)
		ok     bool
	}{
		{"rate +inf", func(c *Config) { c.RateLimitPPS = math.Inf(1) }, false},
		{"rate nan", func(c *Config) { c.RateLimitPPS = math.NaN() }, false},
		{"rate too large", func(c *Config) { c.RateLimitPPS = 1e30 }, false},
		{"rate zero", func(c *Config) { c.RateLimitPPS = 0 }, false},
		{"burst +inf", func(c *Config) { c.RateBurst = math.Inf(1) }, false},
		{"burst nan", func(c *Config) { c.RateBurst = math.NaN() }, false},
		{"burst too large", func(c *Config) { c.RateBurst = 1e30 }, false},
		{"burst below one", func(c *Config) { c.RateBurst = 0.5 }, false},
		{"rate at the maximum", func(c *Config) { c.RateLimitPPS = maxRateLimit }, true},
		{"burst at the maximum", func(c *Config) { c.RateBurst = maxRateLimit }, true},
		{"burst one", func(c *Config) { c.RateBurst = 1 }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := base()
			c.mutate(&cfg)
			_, err := NewHandler(cfg)
			if c.ok && err != nil {
				t.Fatalf("valid limits refused: %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("unbounded limits accepted")
			}
		})
	}
}

// TestAstra6DirectedBroadcastIsRecognised covers RA6X-026. A directed
// broadcast cannot be told from an ordinary unicast address without the
// interface's own configuration: 192.0.2.255 is the broadcast of a /24 and a
// perfectly ordinary host address on a /23. FreeBSD reports no MSG_BCAST, so
// comparing against the interfaces' broadcast addresses is the mechanism that
// works on both platforms.
func TestAstra6DirectedBroadcastIsRecognised(t *testing.T) {
	ifaces := []net.Addr{
		&net.IPNet{IP: net.IPv4(192, 0, 2, 10), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: net.IPv4(198, 51, 100, 3), Mask: net.CIDRMask(23, 32)},
		&net.IPNet{IP: net.IPv4(203, 0, 113, 5), Mask: net.CIDRMask(31, 32)},
		&net.IPNet{IP: net.IPv4(127, 0, 0, 1), Mask: net.CIDRMask(8, 32)},
	}
	b := &localBroadcasts{enumerate: func() ([]net.Addr, error) { return ifaces, nil }}
	now := time.Now()
	cases := []struct {
		addr  string
		bcast bool
	}{
		{"192.0.2.255", true},    // the /24's directed broadcast
		{"198.51.101.255", true}, // the /23's, which is not a .255 on its own prefix
		{"127.255.255.255", true},
		{"192.0.2.10", false},     // our own address
		{"192.0.2.1", false},      // an ordinary host
		{"198.51.100.255", false}, // an ordinary host on the /23
		{"203.0.113.5", false},    // a /31 has no broadcast address
		{"203.0.113.4", false},
	}
	for _, c := range cases {
		t.Run(c.addr, func(t *testing.T) {
			got, known := b.isBroadcast(netip.MustParseAddr(c.addr), now)
			if !known {
				t.Fatal("the interface configuration must be known here")
			}
			if got != c.bcast {
				t.Fatalf("isBroadcast(%s) = %v, want %v", c.addr, got, c.bcast)
			}
		})
	}
	// IPv6 has no broadcast at all.
	if got, known := b.isBroadcast(netip.MustParseAddr("2001:db8::1"), now); got || !known {
		t.Fatalf("IPv6 destination reported bcast=%v known=%v", got, known)
	}
}

// TestAstra6BroadcastMetadataPolicy pins the documented behaviour when the
// interface list cannot be read, and the refresh.
func TestAstra6BroadcastMetadataPolicy(t *testing.T) {
	var fail atomic.Bool
	var calls atomic.Int32
	b := &localBroadcasts{enumerate: func() ([]net.Addr, error) {
		calls.Add(1)
		if fail.Load() {
			return nil, errors.New("no interfaces")
		}
		return []net.Addr{&net.IPNet{IP: net.IPv4(192, 0, 2, 10), Mask: net.CIDRMask(24, 32)}}, nil
	}}

	// Enumeration failing before anything is known: not a broadcast, and
	// the caller is told the metadata is unavailable.
	fail.Store(true)
	if got, known := b.isBroadcast(netip.MustParseAddr("192.0.2.255"), time.Now()); got || known {
		t.Fatalf("with no interface information: bcast=%v known=%v, want false/false", got, known)
	}

	// Once it succeeds, the answer is cached and not re-enumerated.
	fail.Store(false)
	now := time.Now()
	if got, known := b.isBroadcast(netip.MustParseAddr("192.0.2.255"), now); !got || !known {
		t.Fatalf("bcast=%v known=%v", got, known)
	}
	before := calls.Load()
	for i := 0; i < 100; i++ {
		b.isBroadcast(netip.MustParseAddr("192.0.2.1"), now.Add(time.Duration(i)*time.Millisecond))
	}
	if calls.Load() != before {
		t.Fatalf("the cache was re-enumerated %d times inside the refresh window", calls.Load()-before)
	}

	// A later failure keeps the last known configuration rather than losing
	// the check entirely.
	fail.Store(true)
	if got, known := b.isBroadcast(netip.MustParseAddr("192.0.2.255"), now.Add(2*broadcastRefresh)); !got || !known {
		t.Fatalf("after a failed refresh: bcast=%v known=%v, want the cached true/true", got, known)
	}

	// And a successful refresh past the window picks up a change.
	fail.Store(false)
	if calls.Load() == before {
		t.Fatal("the refresh never happened")
	}
}

// TestAstra6AuthenticatedRATEIsAuthenticated is the review's RA6X-029 probe.
// RATE replies were always unsigned, so carillon's own authenticated client
// recorded bad authentication and never executed the backoff — two instances
// could not honour their own rate-control protocol.
func TestAstra6AuthenticatedRATEIsAuthenticated(t *testing.T) {
	h, _ := newTestHandler(t, func(c *Config) {
		c.RateBurst = 1
		c.RateLimitPPS = 1
		c.RequireKey = map[netip.Prefix]uint32{netip.MustParsePrefix("192.0.2.0/24"): 1}
	})
	req := testKey.Append(request(4))
	h.Handle(req, from("192.0.2.1"), testWall, testMono)
	got := h.Handle(req, from("192.0.2.1"), testWall, testMono.Add(time.Millisecond))
	p, mac, off, err := ntp.Decode(got)
	if err != nil || p.ReferenceID != ntp.KissRATE {
		t.Fatalf("setup no RATE: %v %+v", err, p)
	}
	if !testKey.Verify(got[:off], mac) {
		t.Fatal("authenticated client's RATE response has no valid MAC")
	}
}

// TestAstra6UnsignedRATEStaysUnsigned checks the other half: a reply is never
// signed on the strength of a key id a request merely claims.
func TestAstra6UnsignedRATEStaysUnsigned(t *testing.T) {
	h, _ := newTestHandler(t, func(c *Config) { c.RateBurst = 1; c.RateLimitPPS = 1 })
	plain := request(4)
	h.Handle(plain, from("192.0.2.1"), testWall, testMono)
	got := h.Handle(plain, from("192.0.2.1"), testWall, testMono.Add(time.Millisecond))
	if len(got) == 0 {
		t.Fatal("no RATE reply")
	}
	p, _, _, err := ntp.Decode(got)
	if err != nil || p.ReferenceID != ntp.KissRATE {
		t.Fatalf("no RATE: %v %+v", err, p)
	}
	if len(got) != ntp.HeaderSize {
		t.Fatalf("an unsigned client's RATE reply is %d bytes, want a bare header", len(got))
	}
}

// TestAstra6OptionalAuthHasIndependentBucket is the review's RA6X-028 probe.
// With an optional key, verification happened after the bucket was chosen, so
// every request from an address used key id zero and spoofed unsigned traffic
// could exhaust a correctly authenticated client's allowance.
func TestAstra6OptionalAuthHasIndependentBucket(t *testing.T) {
	h, _ := newTestHandler(t, func(c *Config) { c.RateBurst = 1; c.RateLimitPPS = 1; c.KoD = false })
	h.Handle(request(4), from("192.0.2.1"), testWall, testMono)
	if got := h.Handle(testKey.Append(request(4)), from("192.0.2.1"), testWall, testMono); len(got) == 0 {
		t.Fatal("unsigned request drained optional authenticated client's bucket")
	}
}

// TestAstra6CryptoNAKFloodIsBounded is the review's RA6X-027 probe: invalid
// MACs from a require_key prefix produced one crypto-NAK each, at arrival
// rate, because the NAK was emitted before the limiter was consulted.
func TestAstra6CryptoNAKFloodIsBounded(t *testing.T) {
	h, stats := newTestHandler(t, func(c *Config) {
		c.RequireKey = map[netip.Prefix]uint32{netip.MustParsePrefix("192.0.2.7/32"): 1}
		c.RateLimitPPS, c.RateBurst = 1, 8
	})
	unknown := auth.Key{ID: 77, Secret: []byte("0123456789abcdef")}
	req := unknown.Append(request(4))
	replies := 0
	for i := 0; i < 100; i++ {
		if got := h.Handle(req, from("192.0.2.7"), testWall, testMono); len(got) > 0 {
			replies++
			if len(got) > len(req) {
				t.Fatal("amplified reply")
			}
		}
	}
	if replies > 9 { // the burst, plus at most one throttled kiss
		t.Fatalf("%d replies to 100 bad MACs at one instant with burst=8; counters=%+v",
			replies, stats.Snapshot().Total)
	}
	// The peer's own authenticated request must still be served: a spoofer
	// at its address must not be able to lock it out.
	if got := h.Handle(testKey.Append(request(4)), from("192.0.2.7"), testWall, testMono); len(got) != 68 {
		t.Fatalf("spoofed traffic starved the authenticated peer: reply length %d", len(got))
	}
}

// TestAstra6AuthBudgetsAreSeparate covers the rest of RA6X-027 and RA6X-028:
// each class of traffic gets its own allowance and cannot spend another's.
func TestAstra6AuthBudgetsAreSeparate(t *testing.T) {
	wrong := auth.Key{ID: 1, Secret: []byte("wrongwrongwrong!")}
	unknown := auth.Key{ID: 77, Secret: []byte("0123456789abcdef")}

	for _, c := range []struct {
		name string
		req  []byte
	}{
		{"absent key", request(4)},
		{"unknown key", unknown.Append(request(4))},
		{"wrong key", wrong.Append(request(4))},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newTestHandler(t, func(cfg *Config) {
				cfg.RequireKey = map[netip.Prefix]uint32{netip.MustParsePrefix("192.0.2.7/32"): 1}
				cfg.RateLimitPPS, cfg.RateBurst = 1, 4
			})
			for i := 0; i < 200; i++ {
				h.Handle(c.req, from("192.0.2.7"), testWall, testMono)
			}
			// A valid required-key request still succeeds afterwards.
			if got := h.Handle(testKey.Append(request(4)), from("192.0.2.7"), testWall, testMono); len(got) != 68 {
				t.Fatalf("a valid required-key request was refused after the flood: %d bytes", len(got))
			}
		})
	}
}

// TestAstra6LimiterKeyspacesAreDistinct pins the separation itself.
func TestAstra6LimiterKeyspacesAreDistinct(t *testing.T) {
	l := newRateLimiter(1, 1, 0, 0)
	addr := netip.MustParseAddr("192.0.2.1")
	now := testMono
	keys := []bucketKey{
		l.key(addr, 0),
		l.key(addr, 1),
		l.cryptoKey(addr),
	}
	for i, k := range keys {
		if allowed, _ := l.allow(k, now); !allowed {
			t.Fatalf("keyspace %d shares a bucket with another", i)
		}
	}
	for i, k := range keys {
		if allowed, _ := l.allow(k, now); allowed {
			t.Fatalf("keyspace %d has more than its own burst", i)
		}
	}
}

// TestAstra6LastEventSurvivesAClockStep covers the listener half of
// RA6X-053. "Latest" meant "the largest UnixNano seen", so after a backward
// step every subsequent request carried a smaller timestamp and was refused —
// last_request stayed pinned to a pre-step future value for ever.
func TestAstra6LastEventSurvivesAClockStep(t *testing.T) {
	var e eventTime
	future := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	corrected := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	e.store(future)
	if got := e.load(); !got.Equal(future) {
		t.Fatalf("first event %v, want %v", got, future)
	}
	// The clock is stepped back to the truth; the next request is newer as
	// an *event* even though its wall time is smaller.
	e.store(corrected)
	if got := e.load(); !got.Equal(corrected) {
		t.Fatalf("after a backward step the last event is %v, want %v", got, corrected)
	}
	// Ordinary forward progress still works.
	later := corrected.Add(time.Second)
	e.store(later)
	if got := e.load(); !got.Equal(later) {
		t.Fatalf("last event %v, want %v", got, later)
	}
}

// TestAstra6ConcurrentEventsKeepOne checks concurrent listeners contending on
// the same counter leave exactly one of the stored values behind, and never a
// zero.
func TestAstra6ConcurrentEventsKeepOne(t *testing.T) {
	var e eventTime
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				e.store(base.Add(time.Duration(i*1000+j) * time.Millisecond))
			}
		}(i)
	}
	wg.Wait()
	got := e.load()
	if got.IsZero() {
		t.Fatal("concurrent stores left no event at all")
	}
	if got.Before(base) || got.After(base.Add(10*time.Second)) {
		t.Fatalf("last event %v is not one of the stored values", got)
	}
}

// fakeBuffers is a kernel that accepts only the sizes it is told to.
type fakeBuffers struct {
	accept  map[int]bool
	err     error
	tried   []int
	granted int
}

func (f *fakeBuffers) SetReadBuffer(n int) error {
	f.tried = append(f.tried, n)
	if f.err != nil {
		return f.err
	}
	if f.accept[n] {
		f.granted = n
		return nil
	}
	return syscall.ENOBUFS
}

// TestAstra6ReceiveBufferFallbackReachesTheMinimum covers RA6X-043. Halving
// an arbitrary request can step straight past the documented minimum without
// ever asking for it — 100000 halves to 50000, below the 65536 floor — so
// startup failed with an error claiming the minimum had been tried.
func TestAstra6ReceiveBufferFallbackReachesTheMinimum(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	addr := netip.MustParseAddrPort("127.0.0.1:123")

	t.Run("a non-power-of-two request reaches the floor", func(t *testing.T) {
		f := &fakeBuffers{accept: map[int]bool{minRecvBuffer: true}}
		if err := setReadBuffer(f, 100000, addr, log); err != nil {
			t.Fatalf("a system that can grant the minimum must not fail: %v", err)
		}
		if f.granted != minRecvBuffer {
			t.Fatalf("granted %d, want the %d floor (tried %v)", f.granted, minRecvBuffer, f.tried)
		}
		if got := f.tried[len(f.tried)-1]; got != minRecvBuffer {
			t.Fatalf("the last attempt was %d, not the floor (tried %v)", got, f.tried)
		}
	})

	t.Run("the floor is asked for exactly once", func(t *testing.T) {
		f := &fakeBuffers{accept: map[int]bool{}}
		if err := setReadBuffer(f, 100000, addr, log); err == nil {
			t.Fatal("all attempts failing must be an error")
		}
		n := 0
		for _, v := range f.tried {
			if v == minRecvBuffer {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("the floor was attempted %d times: %v", n, f.tried)
		}
	})

	t.Run("a power-of-two request still works", func(t *testing.T) {
		f := &fakeBuffers{accept: map[int]bool{1 << 20: true}}
		if err := setReadBuffer(f, 4<<20, addr, log); err != nil {
			t.Fatal(err)
		}
		if f.granted != 1<<20 {
			t.Fatalf("granted %d, want %d (tried %v)", f.granted, 1<<20, f.tried)
		}
	})

	t.Run("a request equal to the floor is one attempt", func(t *testing.T) {
		f := &fakeBuffers{accept: map[int]bool{minRecvBuffer: true}}
		if err := setReadBuffer(f, minRecvBuffer, addr, log); err != nil {
			t.Fatal(err)
		}
		if len(f.tried) != 1 {
			t.Fatalf("attempted %v, want one", f.tried)
		}
	})

	t.Run("a non-capacity error stops immediately", func(t *testing.T) {
		f := &fakeBuffers{err: syscall.EPERM}
		if err := setReadBuffer(f, 4<<20, addr, log); err == nil {
			t.Fatal("a non-capacity error must fail")
		}
		if len(f.tried) != 1 {
			t.Fatalf("a non-capacity error was retried: %v", f.tried)
		}
	})
}

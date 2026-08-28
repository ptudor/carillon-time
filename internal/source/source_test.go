package source

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"carillon/internal/clock"
	"carillon/internal/discipline"
	"carillon/internal/ntp"
	"carillon/internal/ntp/auth"
)

// fakeServer is an in-process NTP server on loopback whose replies are
// produced by a scriptable handler. A nil reply from the handler means "do
// not answer".
type fakeServer struct {
	conn     *net.UDPConn
	addr     netip.AddrPort
	handler  func(req ntp.Packet, raw []byte) []byte
	requests atomic.Int64
	wg       sync.WaitGroup
}

func newFakeServer(t *testing.T, handler func(req ntp.Packet, raw []byte) []byte) *fakeServer {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeServer{conn: conn, addr: conn.LocalAddr().(*net.UDPAddr).AddrPort(), handler: handler}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		buf := make([]byte, ntp.MaxPacketSize)
		for {
			n, from, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			s.requests.Add(1)
			raw := append([]byte(nil), buf[:n]...)
			pkt, _, _, err := ntp.Decode(raw)
			if err != nil {
				continue
			}
			if reply := s.handler(pkt, raw); reply != nil {
				_, _ = conn.WriteToUDPAddrPort(reply, from)
			}
		}
	}()
	t.Cleanup(func() {
		conn.Close()
		s.wg.Wait()
	})
	return s
}

// reply builds a plausible stratum-2 server reply to req, then lets mutate
// alter it. The MAC (if any) is appended by the caller.
func reply(req ntp.Packet, mutate func(*ntp.Packet)) []byte {
	now := ntp.FromTime(time.Now())
	p := ntp.Packet{
		Version:        req.Version,
		Mode:           ntp.ModeServer,
		Stratum:        2,
		Poll:           req.Poll,
		Precision:      -20,
		RootDelay:      ntp.ShortFromSeconds(0.002),
		RootDispersion: ntp.ShortFromSeconds(0.001),
		ReferenceID:    ntp.RefID{192, 0, 2, 1},
		ReferenceTime:  now.Add(-10),
		OriginTime:     req.TransmitTime,
		ReceiveTime:    now,
	}
	if mutate != nil {
		mutate(&p)
	}
	if p.TransmitTime.IsZero() {
		p.TransmitTime = ntp.FromTime(time.Now())
	}
	return p.Marshal()
}

func plain(req ntp.Packet, _ []byte) []byte { return reply(req, nil) }

func testKey() *auth.Key {
	return &auth.Key{ID: 1, Secret: []byte("0123456789abcdef")}
}

// newPoller builds an NTP source against srv whose sleep hook allows
// `sleeps` immediate sleeps and then blocks until the context is done.
func newPoller(t *testing.T, srv *fakeServer, cfg NTPConfig, sleeps int) *NTP {
	t.Helper()
	cfg.Address = srv.addr.String()
	if cfg.Name == "" {
		cfg.Name = "test"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 500 * time.Millisecond
	}
	n, err := NewNTP(cfg, clock.ReadOnly(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewNTP: %v", err)
	}
	var calls atomic.Int32
	n.sleep = func(ctx context.Context, d time.Duration) bool {
		if int(calls.Add(1)) <= sleeps {
			return ctx.Err() == nil
		}
		<-ctx.Done()
		return false
	}
	return n
}

// run starts the poller and returns its output channel and a stop function
// that cancels it and asserts that Run returned promptly.
func run(t *testing.T, n *NTP) (<-chan discipline.Measurement, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan discipline.Measurement, 16)
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx, out) }()
	stop := func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return after cancel")
		}
	}
	return out, stop
}

func next(t *testing.T, out <-chan discipline.Measurement) discipline.Measurement {
	t.Helper()
	select {
	case m := <-out:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no measurement")
		return discipline.Measurement{}
	}
}

func TestNormalExchange(t *testing.T) {
	srv := newFakeServer(t, plain)
	n := newPoller(t, srv, NTPConfig{}, 0)
	out, stop := run(t, n)
	defer stop()

	m := next(t, out)
	if !m.Valid {
		t.Fatalf("measurement not valid: %+v", m)
	}
	if m.Offset > 0.05 || m.Offset < -0.05 {
		t.Fatalf("offset %v too large for loopback", m.Offset)
	}
	if m.Delay <= 0 {
		t.Fatalf("delay %v must be positive", m.Delay)
	}
	if m.Reach != 1 || m.Stratum != 2 || m.Source != "test" {
		t.Fatalf("unexpected measurement %+v", m)
	}
	if m.SourceRefID != (ntp.RefID{127, 0, 0, 1}) {
		t.Fatalf("source refid %v", m.SourceRefID)
	}
	if m.RefID != (ntp.RefID{192, 0, 2, 1}) || m.Leap != ntp.LeapNone {
		t.Fatalf("packet variables not copied: %+v", m)
	}
	if m.RootDelay < 0.0019 || m.RootDelay > 0.0021 {
		t.Fatalf("root delay %v", m.RootDelay)
	}
	info := n.Info()
	if info.Sent != 1 || info.Received != 1 || info.Reach != 1 || info.Timeouts != 0 {
		t.Fatalf("info %+v", info)
	}
	if info.Resolved != srv.addr {
		t.Fatalf("resolved %v want %v", info.Resolved, srv.addr)
	}
}

func TestWrongOriginIsIgnored(t *testing.T) {
	srv := newFakeServer(t, func(req ntp.Packet, _ []byte) []byte {
		return reply(req, func(p *ntp.Packet) { p.OriginTime = req.TransmitTime + 1 })
	})
	n := newPoller(t, srv, NTPConfig{Timeout: 200 * time.Millisecond}, 0)
	out, stop := run(t, n)
	defer stop()

	m := next(t, out)
	if m.Valid || m.Reach != 0 {
		t.Fatalf("expected a miss, got %+v", m)
	}
	info := n.Info()
	if info.Timeouts != 1 || info.Received != 0 {
		t.Fatalf("info %+v", info)
	}
}

func TestKissRATE(t *testing.T) {
	srv := newFakeServer(t, func(req ntp.Packet, _ []byte) []byte {
		return reply(req, func(p *ntp.Packet) {
			p.Stratum = 0
			p.ReferenceID = ntp.KissRATE
			p.Poll = 8
		})
	})
	n := newPoller(t, srv, NTPConfig{}, 0)
	out, stop := run(t, n)
	defer stop()

	m := next(t, out)
	if m.Valid {
		t.Fatal("kiss must not produce a sample")
	}
	if m.Poll != 8 {
		t.Fatalf("poll %d want 8", m.Poll)
	}
	info := n.Info()
	if info.Kiss != 1 || info.Denied {
		t.Fatalf("info %+v", info)
	}
}

func TestUnauthenticatedDENYIsAMiss(t *testing.T) {
	srv := newFakeServer(t, func(req ntp.Packet, _ []byte) []byte {
		return reply(req, func(p *ntp.Packet) {
			p.Stratum = 0
			p.ReferenceID = ntp.KissDENY
		})
	})
	n := newPoller(t, srv, NTPConfig{}, 0)
	out, stop := run(t, n)
	defer stop()

	m := next(t, out)
	if m.Valid || m.Reach != 0 {
		t.Fatalf("expected a miss, got %+v", m)
	}
	if info := n.Info(); info.Denied || info.Kiss != 1 {
		t.Fatalf("info %+v", info)
	}
}

func TestAuthenticatedDENYStopsPolling(t *testing.T) {
	key := testKey()
	srv := newFakeServer(t, func(req ntp.Packet, _ []byte) []byte {
		return key.Append(reply(req, func(p *ntp.Packet) {
			p.Stratum = 0
			p.ReferenceID = ntp.KissDENY
		}))
	})
	// Allow many sleeps: if polling continued, more requests would arrive.
	n := newPoller(t, srv, NTPConfig{Key: key}, 100)
	out, stop := run(t, n)
	defer stop()

	m := next(t, out)
	if m.Valid || m.Reach != 0 {
		t.Fatalf("expected a miss, got %+v", m)
	}
	info := n.Info()
	if !info.Denied || info.Kiss != 1 {
		t.Fatalf("info %+v", info)
	}
	time.Sleep(100 * time.Millisecond)
	if got := srv.requests.Load(); got != 1 {
		t.Fatalf("polling continued after DENY: %d requests", got)
	}
}

func TestAuthenticatedExchange(t *testing.T) {
	key := testKey()
	srv := newFakeServer(t, func(req ntp.Packet, raw []byte) []byte {
		_, mac, off, err := ntp.Decode(raw)
		if err != nil || !key.Verify(raw[:off], mac) {
			return reply(req, func(p *ntp.Packet) { p.Stratum = 0; p.ReferenceID = ntp.KissDENY })
		}
		return key.Append(reply(req, nil))
	})
	n := newPoller(t, srv, NTPConfig{Key: key}, 0)
	out, stop := run(t, n)
	defer stop()

	m := next(t, out)
	if !m.Valid || m.Reach != 1 {
		t.Fatalf("expected a sample, got %+v", m)
	}
}

func TestBadMAC(t *testing.T) {
	other := &auth.Key{ID: 1, Secret: []byte("fedcba9876543210")}
	srv := newFakeServer(t, func(req ntp.Packet, _ []byte) []byte {
		return other.Append(reply(req, nil))
	})
	n := newPoller(t, srv, NTPConfig{Key: testKey()}, 0)
	out, stop := run(t, n)
	defer stop()

	m := next(t, out)
	if m.Valid || m.Reach != 0 {
		t.Fatalf("expected a miss, got %+v", m)
	}
	if info := n.Info(); info.BadAuth != 1 {
		t.Fatalf("info %+v", info)
	}
}

func TestMissingMACWithKey(t *testing.T) {
	srv := newFakeServer(t, plain)
	n := newPoller(t, srv, NTPConfig{Key: testKey()}, 0)
	out, stop := run(t, n)
	defer stop()

	if m := next(t, out); m.Valid {
		t.Fatal("unauthenticated reply accepted with a key configured")
	}
	if info := n.Info(); info.BadAuth != 1 {
		t.Fatalf("info %+v", info)
	}
}

func TestBogusReplies(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ntp.Packet)
	}{
		{"stratum 16", func(p *ntp.Packet) { p.Stratum = 16 }},
		{"leap unsync", func(p *ntp.Packet) { p.Leap = ntp.LeapUnsync }},
		{"stratum 0 no kiss code", func(p *ntp.Packet) { p.Stratum = 0; p.ReferenceID = ntp.RefID{0, 1, 2, 3} }},
		{"wrong mode", func(p *ntp.Packet) { p.Mode = ntp.ModeClient }},
		{"root distance", func(p *ntp.Packet) { p.RootDispersion = ntp.ShortFromSeconds(2) }},
		{"reference time in future", func(p *ntp.Packet) { p.ReferenceTime = ntp.FromTime(time.Now().Add(time.Hour)) }},
		{"zero receive", func(p *ntp.Packet) { p.ReceiveTime = 0 }},
		{"negative delay", func(p *ntp.Packet) { p.TransmitTime = ntp.FromTime(time.Now().Add(time.Second)) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newFakeServer(t, func(req ntp.Packet, _ []byte) []byte { return reply(req, c.mutate) })
			n := newPoller(t, srv, NTPConfig{}, 0)
			out, stop := run(t, n)
			defer stop()

			m := next(t, out)
			if m.Valid || m.Reach != 0 {
				t.Fatalf("expected a miss, got %+v", m)
			}
			if info := n.Info(); info.Bogus != 1 {
				t.Fatalf("info %+v", info)
			}
		})
	}
}

func TestIBurst(t *testing.T) {
	srv := newFakeServer(t, plain)
	// Three burst gaps sleep immediately; the poll interval then blocks.
	n := newPoller(t, srv, NTPConfig{IBurst: true}, 3)
	out, stop := run(t, n)
	defer stop()

	for i := 0; i < 4; i++ {
		if m := next(t, out); !m.Valid && i == 0 {
			t.Fatalf("first burst reply not valid: %+v", m)
		}
	}
	if got := srv.requests.Load(); got != 4 {
		t.Fatalf("burst sent %d requests, want 4", got)
	}
	if info := n.Info(); info.Reach != 0b1111 {
		t.Fatalf("reach %08b after burst", info.Reach)
	}
}

func TestResetEmptiesFilter(t *testing.T) {
	srv := newFakeServer(t, plain)
	n := newPoller(t, srv, NTPConfig{}, 1)
	out, stop := run(t, n)
	defer stop()

	next(t, out)
	if n.filter.Len() != 1 {
		t.Fatalf("filter has %d samples", n.filter.Len())
	}
	n.Reset()
	next(t, out)
	if n.filter.Len() != 1 {
		t.Fatalf("filter has %d samples after reset+poll, want 1", n.filter.Len())
	}
}

// TestReResolveAfterTimeouts checks that a hostname is looked up once, kept
// while the server answers, looked up again only after resolveAfterTimeouts
// consecutive timeouts, and that landing on a different server discards the
// clock filter.
func TestReResolveAfterTimeouts(t *testing.T) {
	refB := ntp.RefID{192, 0, 2, 2}
	var drop atomic.Bool
	srvA := newFakeServer(t, func(req ntp.Packet, raw []byte) []byte {
		if drop.Load() {
			return nil
		}
		return plain(req, raw)
	})
	srvB := newFakeServer(t, func(req ntp.Packet, _ []byte) []byte {
		return reply(req, func(p *ntp.Packet) { p.ReferenceID = refB })
	})

	n, err := NewNTP(NTPConfig{Name: "pool", Address: "ntp.test", Timeout: 100 * time.Millisecond},
		clock.ReadOnly(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewNTP: %v", err)
	}
	var target atomic.Pointer[netip.AddrPort]
	target.Store(&srvA.addr)
	var lookups atomic.Int32
	n.lookup = func(_ context.Context, host string, port uint16) (netip.AddrPort, error) {
		if host != "ntp.test" || port != 123 {
			t.Errorf("lookup %s:%d, want ntp.test:123", host, port)
		}
		lookups.Add(1)
		return *target.Load(), nil
	}
	// Poll without delay until the poller has moved to B, then park it so
	// the assertions below see exactly one exchange with B.
	n.sleep = func(ctx context.Context, _ time.Duration) bool {
		if n.Info().Resolved == srvB.addr {
			<-ctx.Done()
			return false
		}
		return ctx.Err() == nil
	}
	out, stop := run(t, n)
	defer stop()

	if m := next(t, out); !m.Valid || m.RefID == refB {
		t.Fatalf("first measurement %+v, want a valid reply from A", m)
	}
	if got := lookups.Load(); got != 1 {
		t.Fatalf("lookups after first poll = %d, want 1", got)
	}

	// A goes silent while the name already points at B. A request already
	// in flight to A may still be answered; after that exactly
	// resolveAfterTimeouts misses must pass before the name is looked up
	// again and the first reply from B arrives.
	drop.Store(true)
	target.Store(&srvB.addr)
	misses := 0
	for {
		m := next(t, out)
		if m.Valid {
			if m.RefID == refB {
				break
			}
			continue
		}
		misses++
		if misses > resolveAfterTimeouts {
			t.Fatalf("%d misses without re-resolving", misses)
		}
	}
	if misses != resolveAfterTimeouts {
		t.Fatalf("re-resolved after %d misses, want %d", misses, resolveAfterTimeouts)
	}
	if got := lookups.Load(); got != 2 {
		t.Fatalf("lookups = %d, want 2", got)
	}
	info := n.Info()
	if info.Resolved != srvB.addr || info.Reach != 1 || info.Timeouts != resolveAfterTimeouts {
		t.Fatalf("info %+v", info)
	}
	if n.filter.Len() != 1 {
		t.Fatalf("filter has %d samples, want 1 after changing server", n.filter.Len())
	}
}

func TestQuery(t *testing.T) {
	srv := newFakeServer(t, plain)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, err := Query(ctx, srv.addr.String(), nil, clock.ReadOnly(), 500*time.Millisecond)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if r.Server != srv.addr || r.Packet.Stratum != 2 || r.Delay < 0 || r.Offset > 0.05 || r.Offset < -0.05 {
		t.Fatalf("result %+v", r)
	}
	if r.T2.Before(r.T1.Add(-time.Second)) || r.T3.After(r.T4.Add(time.Second)) {
		t.Fatalf("timestamps out of order: %+v", r)
	}

	// A kiss code is an error that names it.
	kiss := newFakeServer(t, func(req ntp.Packet, _ []byte) []byte {
		return reply(req, func(p *ntp.Packet) { p.Stratum = 0; p.ReferenceID = ntp.KissRATE })
	})
	if _, err := Query(ctx, kiss.addr.String(), nil, clock.ReadOnly(), 500*time.Millisecond); err == nil {
		t.Fatal("kiss must be an error")
	} else {
		var ke *kissError
		if !errors.As(err, &ke) || ke.code != "RATE" {
			t.Fatalf("error %v", err)
		}
	}
}

func TestQueryTimeout(t *testing.T) {
	silent := newFakeServer(t, func(ntp.Packet, []byte) []byte { return nil })
	ctx := context.Background()
	_, err := Query(ctx, silent.addr.String(), nil, clock.ReadOnly(), 100*time.Millisecond)
	if !errors.Is(err, errTimeout) {
		t.Fatalf("error %v", err)
	}
}

func TestCancelMidRead(t *testing.T) {
	silent := newFakeServer(t, func(ntp.Packet, []byte) []byte { return nil })
	n := newPoller(t, silent, NTPConfig{Timeout: 10 * time.Second}, 0)
	_, stop := run(t, n)
	time.Sleep(50 * time.Millisecond)
	stop() // asserts Run returns within 2 s despite the 10 s read timeout
}

func TestAdaptPoll(t *testing.T) {
	cases := []struct {
		poll           int8
		offset, jitter float64
		want           int8
	}{
		{6, 0.001, 0.001, 7},  // |θ| < 4ψ → up
		{6, 0.005, 0.001, 5},  // |θ| ≥ 4ψ → down
		{6, -0.001, 0.001, 7}, // sign does not matter
		{10, 0, 0.001, 10},    // clamp at max
		{4, 1, 0.001, 4},      // clamp at min
	}
	for _, c := range cases {
		if got := adaptPoll(c.poll, c.offset, c.jitter, 4, 10); got != c.want {
			t.Errorf("adaptPoll(%d, %v, %v) = %d want %d", c.poll, c.offset, c.jitter, got, c.want)
		}
	}
}

func TestSplitHostPort(t *testing.T) {
	cases := []struct {
		in   string
		host string
		port uint16
		err  bool
	}{
		{"host", "host", 123, false},
		{"host:1234", "host", 1234, false},
		{"192.0.2.1", "192.0.2.1", 123, false},
		{"192.0.2.1:123", "192.0.2.1", 123, false},
		{"[2001:db8::1]", "2001:db8::1", 123, false},
		{"[2001:db8::1]:1234", "2001:db8::1", 1234, false},
		{"2001:db8::1", "2001:db8::1", 123, false},
		{"", "", 0, true},
		{"host:0", "", 0, true},
		{"host:notaport", "", 0, true},
		{":123", "", 0, true},
	}
	for _, c := range cases {
		h, p, err := splitHostPort(c.in)
		if (err != nil) != c.err {
			t.Errorf("%q: err=%v", c.in, err)
			continue
		}
		if !c.err && (h != c.host || p != c.port) {
			t.Errorf("%q: got %s:%d want %s:%d", c.in, h, p, c.host, c.port)
		}
	}
}

func TestNewNTPValidation(t *testing.T) {
	clk := clock.ReadOnly()
	if _, err := NewNTP(NTPConfig{Address: ""}, clk, nil); err == nil {
		t.Error("empty address accepted")
	}
	if _, err := NewNTP(NTPConfig{Address: "h", PollMin: 8, PollMax: 4}, clk, nil); err == nil {
		t.Error("inverted poll range accepted")
	}
	if _, err := NewNTP(NTPConfig{Address: "h", Key: &auth.Key{ID: 1, Secret: []byte("short")}}, clk, nil); err == nil {
		t.Error("short key accepted")
	}
	if _, err := NewNTP(NTPConfig{Address: "h"}, nil, nil); err == nil {
		t.Error("nil clock accepted")
	}
	n, err := NewNTP(NTPConfig{Address: "h"}, clk, nil)
	if err != nil || n.Name() != "h" || n.cfg.PollMin != 6 || n.cfg.PollMax != 10 || n.cfg.Timeout != defaultTimeout {
		t.Errorf("defaults: %v %+v", err, n.cfg)
	}
}

// TestExchangeErrorLoggingIsThrottled checks the log budget for a source that
// keeps failing: one line for the first failure, then one every logEvery-th.
func TestExchangeErrorLoggingIsThrottled(t *testing.T) {
	var n NTP
	logged := 0
	for i := 0; i < 4*logEvery; i++ {
		if n.shouldLogErr() {
			logged++
		}
	}
	// The first, then 4 periodic reminders.
	if want := 5; logged != want {
		t.Fatalf("logged %d of %d failures, want %d", logged, 4*logEvery, want)
	}
	if n.consecutiveErrs != uint64(4*logEvery) {
		t.Fatalf("consecutiveErrs = %d, want %d", n.consecutiveErrs, 4*logEvery)
	}
}

// TestGoodReplyClearsErrorThrottle checks that recovery rearms the warning,
// so the next outage announces itself instead of arriving mid-cycle.
func TestGoodReplyClearsErrorThrottle(t *testing.T) {
	srv := newFakeServer(t, plain)
	n := newPoller(t, srv, NTPConfig{}, 0)
	n.consecutiveErrs = logEvery - 1

	out, stop := run(t, n)
	defer stop()

	if m := next(t, out); !m.Valid {
		t.Fatalf("expected a usable reply, got %+v", m)
	}
	if n.consecutiveErrs != 0 {
		t.Fatalf("consecutiveErrs = %d after a good reply, want 0", n.consecutiveErrs)
	}
	if !n.shouldLogErr() {
		t.Fatal("the first failure after recovery was not logged")
	}
}

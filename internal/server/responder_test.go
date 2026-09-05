package server

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"carillon/internal/ntp"
	"carillon/internal/ntp/auth"
)

var (
	testWall = time.Date(2026, 8, 23, 12, 0, 0, 123456789, time.UTC)
	testMono = time.Unix(1000, 0)
	testKey  = auth.Key{ID: 1, Secret: []byte("0123456789abcdef")}
)

func testStatus() SystemStatus {
	return SystemStatus{
		Synced:         true,
		Leap:           ntp.LeapInsert,
		Stratum:        2,
		Precision:      -20,
		RootDelay:      0.025,
		RootDispersion: 0.004,
		ReferenceID:    ntp.RefIDFromString("GPS"),
		ReferenceTime:  testWall.Add(-time.Second),
	}
}

func newTestHandler(t testing.TB, mutate func(*Config)) (*Handler, *Stats) {
	t.Helper()
	stats := &Stats{}
	cfg := Config{
		Allow:        []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
		Keys:         auth.Keys{1: testKey},
		RateLimitPPS: 100,
		RateBurst:    100,
		KoD:          true,
		Status:       testStatus,
		Now:          func() time.Time { return testWall.Add(time.Millisecond) },
		Stats:        stats,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h, stats
}

// from builds a plausible client address and ephemeral source port. Handle
// takes the whole AddrPort because a source port of zero is itself a reason
// to drop a datagram.
func from(addr string) netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr(addr), 45678)
}

func request(version uint8) []byte {
	return (&ntp.Packet{
		Version:      version,
		Mode:         ntp.ModeClient,
		Poll:         7,
		Precision:    -23,
		TransmitTime: ntp.FromTime(testWall.Add(-10 * time.Millisecond)),
	}).Marshal()
}

func decodeReply(t *testing.T, b []byte) (ntp.Packet, *ntp.MAC, int) {
	t.Helper()
	p, mac, off, err := ntp.Decode(b)
	if err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return p, mac, off
}

func TestReplyFieldsAndVersionEcho(t *testing.T) {
	h, stats := newTestHandler(t, nil)
	for version := uint8(1); version <= 4; version++ {
		req := request(version)
		got := h.Handle(req, from("192.0.2.10"), testWall, testMono.Add(time.Duration(version)*time.Second))
		if len(got) != ntp.HeaderSize {
			t.Fatalf("version %d: reply length %d", version, len(got))
		}
		p, mac, _ := decodeReply(t, got)
		if p.Version != version || p.Mode != ntp.ModeServer || p.Stratum != 2 || p.Leap != ntp.LeapInsert || p.Poll != 7 || p.Precision != -20 {
			t.Fatalf("version %d: header %+v", version, p)
		}
		if mac != nil || p.OriginTime != ntp.Time(binary.BigEndian.Uint64(req[40:48])) {
			t.Fatalf("version %d: origin/mac %+v %v", version, p, mac)
		}
		if p.ReceiveTime != ntp.FromTime(testWall) || p.TransmitTime != ntp.FromTime(testWall.Add(time.Millisecond)) {
			t.Fatalf("timestamps: receive=%v transmit=%v", p.ReceiveTime, p.TransmitTime)
		}
		if p.ReferenceID != ntp.RefIDFromString("GPS") || p.ReferenceTime != ntp.FromTime(testWall.Add(-time.Second)) {
			t.Fatalf("reference: %+v", p)
		}
		if diff := p.RootDelay.Seconds() - 0.025; diff < -1.6e-5 || diff > 0 {
			t.Fatalf("root delay %v", p.RootDelay.Seconds())
		}
	}
	if got := stats.Snapshot().Total; got.Served != 4 || got.Unsynced != 0 || !got.LastRequest.Equal(testWall) || !got.LastServed.Equal(testWall) {
		t.Fatalf("stats %+v", got)
	}
}

func TestUnsynchronizedReply(t *testing.T) {
	h, stats := newTestHandler(t, func(c *Config) {
		c.Status = func() SystemStatus {
			return SystemStatus{Synced: false, Precision: -18, ReferenceID: ntp.KissSTEP}
		}
	})
	p, _, _ := decodeReply(t, h.Handle(request(4), from("192.0.2.1"), testWall, testMono))
	if p.Leap != ntp.LeapUnsync || p.Stratum != 16 || p.ReferenceID != ntp.KissSTEP || p.RootDispersion.Seconds() != 16 || !p.ReferenceTime.IsZero() {
		t.Fatalf("unsynchronized reply %+v", p)
	}
	if got := stats.Snapshot().Total; got.Served != 1 || got.Unsynced != 1 {
		t.Fatalf("stats %+v", got)
	}
}

func TestModesLengthsAndACL(t *testing.T) {
	h, stats := newTestHandler(t, func(c *Config) {
		c.Deny = []netip.Prefix{netip.MustParsePrefix("192.0.2.64/26")}
	})
	if got := h.Handle(request(4)[:47], from("192.0.2.1"), testWall, testMono); got != nil {
		t.Fatal("short request received a reply")
	}
	for _, mode := range []ntp.Mode{ntp.ModeSymmetricActive, ntp.ModeSymmetricPassive, ntp.ModeServer, ntp.ModeBroadcast, ntp.ModeControl, ntp.ModePrivate} {
		r := request(4)
		r[0] = r[0]&^7 | byte(mode)
		if got := h.Handle(r, from("192.0.2.1"), testWall, testMono); got != nil {
			t.Fatalf("mode %v received a reply", mode)
		}
	}
	if got := h.Handle(request(4), from("198.51.100.1"), testWall, testMono); got != nil {
		t.Fatal("address outside allow received a reply")
	}
	if got := h.Handle(request(4), from("192.0.2.70"), testWall, testMono); got != nil {
		t.Fatal("deny did not override allow")
	}
	if got := stats.Snapshot().Total; got.Denied != 2 || got.Served != 0 {
		t.Fatalf("stats %+v", got)
	}
}

func TestAuthentication(t *testing.T) {
	h, stats := newTestHandler(t, func(c *Config) {
		c.RequireKey = map[netip.Prefix]uint32{netip.MustParsePrefix("192.0.2.0/25"): 1}
	})
	client := from("192.0.2.10")
	if got := h.Handle(request(4), client, testWall, testMono); got != nil {
		t.Fatal("missing required MAC received a reply")
	}
	signed := testKey.Append(request(4))
	got := h.Handle(signed, client, testWall, testMono.Add(time.Second))
	if len(got) != ntp.HeaderSize+ntp.MACSizeCMAC || len(got) > len(signed) {
		t.Fatalf("authenticated reply length %d", len(got))
	}
	_, mac, off := decodeReply(t, got)
	if !testKey.Verify(got[:off], mac) {
		t.Fatal("reply MAC did not verify")
	}
	// A digest that does not verify earns a crypto-NAK: a plain reply tells
	// the client only that the answer was unauthenticated, while the NAK
	// says the key is the problem (RF5X-035).
	bad := append([]byte(nil), signed...)
	bad[len(bad)-1] ^= 1
	nak := h.Handle(bad, client, testWall, testMono.Add(2*time.Second))
	assertCryptoNAK(t, nak, bad)
	// A key id this server does not have gets the same answer, and does
	// not make an otherwise-open association trusted.
	unknown := auth.Key{ID: 2, Secret: []byte("fedcba9876543210")}.Append(request(4))
	nak = h.Handle(unknown, from("192.0.2.200"), testWall, testMono.Add(3*time.Second))
	assertCryptoNAK(t, nak, unknown)
	if got := stats.Snapshot().Total; got.BadAuth != 3 || got.Served != 1 {
		t.Fatalf("stats %+v", got)
	}
}

// assertCryptoNAK checks that response is a 48-byte header plus a zero key
// id, and no longer than the request that produced it.
func assertCryptoNAK(t *testing.T, response, request []byte) {
	t.Helper()
	if len(response) != ntp.HeaderSize+ntp.CryptoNAKSize {
		t.Fatalf("crypto-NAK length %d, want %d", len(response), ntp.HeaderSize+ntp.CryptoNAKSize)
	}
	if len(response) > len(request) {
		t.Fatalf("crypto-NAK of %d bytes for a %d-byte request amplifies", len(response), len(request))
	}
	_, mac, _, err := ntp.Decode(response)
	if err != nil {
		t.Fatalf("decoding the crypto-NAK: %v", err)
	}
	if !mac.IsCryptoNAK() {
		t.Fatalf("trailer %+v is not a crypto-NAK", mac)
	}
}

// TestAuthenticatedPeerSurvivesASpoofedFlood covers RF5X-008. The rate-limit
// bucket is keyed by source address, so anyone able to forge the peer's
// address could drain it and take the trusted upstream out of the
// association the shared key exists to protect. Verifying first, and giving
// verified requests their own bucket, makes the flood cost the attacker one
// CMAC each and touch nothing.
func TestAuthenticatedPeerSurvivesASpoofedFlood(t *testing.T) {
	h, stats := newTestHandler(t, func(c *Config) {
		c.RequireKey = map[netip.Prefix]uint32{netip.MustParsePrefix("192.0.2.7/32"): 1}
		c.RateLimitPPS = 1
		c.RateBurst = 4
	})
	peer := from("192.0.2.7")
	now := testMono
	for i := range 100 {
		// A spoofed, unsigned request claiming to be the peer.
		if got := h.Handle(request(4), peer, testWall, now.Add(time.Duration(i)*time.Millisecond)); got != nil {
			t.Fatalf("unsigned request %d from a require_key address was answered", i)
		}
	}
	total := stats.Snapshot().Total
	if total.BadAuth != 100 || total.RateLimited != 0 {
		t.Fatalf("the flood must all be bad_auth and never reach the limiter: %+v", total)
	}
	// The peer's own authenticated poll is still served.
	signed := testKey.Append(request(4))
	got := h.Handle(signed, peer, testWall, now.Add(200*time.Millisecond))
	if len(got) != ntp.HeaderSize+ntp.MACSizeCMAC {
		t.Fatalf("authenticated poll after the flood: reply length %d", len(got))
	}
	if s := stats.Snapshot().Total; s.Served != 1 {
		t.Fatalf("served %d, want 1", s.Served)
	}
}

// TestVersionHistogramCountsOnlyAcceptedRequests covers RF5X-023: a request
// that is then rate-limited or fails require_key must not appear as accepted
// traffic, nor advance last_request.
func TestVersionHistogramCountsOnlyAcceptedRequests(t *testing.T) {
	h, stats := newTestHandler(t, func(c *Config) {
		c.RateLimitPPS = 1
		c.RateBurst = 1
	})
	client := from("192.0.2.1")
	if got := h.Handle(request(4), client, testWall, testMono); got == nil {
		t.Fatal("the first request must be served")
	}
	before := stats.Snapshot().Total
	for i := range 10 {
		h.Handle(request(4), client, testWall, testMono.Add(time.Duration(i)*time.Millisecond))
	}
	after := stats.Snapshot().Total
	if after.RateLimited == 0 {
		t.Fatal("the burst was not rate limited")
	}
	if after.Versions[4] != before.Versions[4] {
		t.Fatalf("version histogram counted rate-limited traffic: %d -> %d",
			before.Versions[4], after.Versions[4])
	}
	if !after.LastRequest.Equal(before.LastRequest) {
		t.Fatalf("last_request advanced while nothing was served: %v -> %v",
			before.LastRequest, after.LastRequest)
	}
}

func TestMostSpecificAuthenticationRuleWins(t *testing.T) {
	key2 := auth.Key{ID: 2, Secret: []byte("fedcba9876543210")}
	h, _ := newTestHandler(t, func(c *Config) {
		c.Keys[2] = key2
		c.RequireKey = map[netip.Prefix]uint32{
			netip.MustParsePrefix("192.0.2.0/24"): 1,
			netip.MustParsePrefix("192.0.2.0/25"): 2,
		}
	})
	client := from("192.0.2.10")
	if got := h.Handle(testKey.Append(request(4)), client, testWall, testMono); got != nil {
		t.Fatal("less-specific key authenticated the client")
	}
	if got := h.Handle(key2.Append(request(4)), client, testWall, testMono.Add(time.Second)); len(got) != ntp.HeaderSize+ntp.MACSizeCMAC {
		t.Fatalf("most-specific key reply length %d", len(got))
	}
}

func TestRateLimitAndKoDThrottle(t *testing.T) {
	h, stats := newTestHandler(t, func(c *Config) {
		c.RateLimitPPS = 1
		c.RateBurst = 1
		c.MinPoll = 5
	})
	client := from("192.0.2.1")
	if got := h.Handle(request(4), client, testWall, testMono); len(got) != ntp.HeaderSize {
		t.Fatal("first request should pass")
	}
	got := h.Handle(request(4), client, testWall, testMono.Add(100*time.Millisecond))
	p, _, _ := decodeReply(t, got)
	if p.Stratum != 0 || p.ReferenceID != ntp.KissRATE || p.Leap != ntp.LeapUnsync || p.Poll != 5 {
		t.Fatalf("RATE reply %+v", p)
	}
	if got := h.Handle(request(4), client, testWall, testMono.Add(200*time.Millisecond)); got != nil {
		t.Fatal("second RATE inside four seconds should be dropped")
	}
	if got := h.Handle(request(4), client, testWall, testMono.Add(time.Second)); len(got) != ntp.HeaderSize {
		t.Fatal("refilled bucket should pass")
	}
	if got := stats.Snapshot().Total; got.Served != 2 || got.RateLimited != 2 {
		t.Fatalf("stats %+v", got)
	}
}

func TestRateLimiterExpiryAndBound(t *testing.T) {
	l := newRateLimiter(1, 1, 2, 0)
	a := l.key(netip.MustParseAddr("192.0.2.1"), 0)
	b := l.key(netip.MustParseAddr("192.0.2.2"), 0)
	c := l.key(netip.MustParseAddr("192.0.2.3"), 0)
	l.allow(a, testMono)
	l.allow(b, testMono.Add(time.Second))
	l.allow(c, testMono.Add(2*time.Second))
	if len(l.clients) != 2 || l.clients[a] != nil {
		t.Fatalf("LRU eviction failed: %+v", l.clients)
	}
	l.allow(a, testMono.Add(clientIdleExpiry+3*time.Second))
	if len(l.clients) != 1 || l.clients[a] == nil {
		t.Fatalf("idle expiry failed: %+v", l.clients)
	}
}

// TestRateLimiterKeysIPv6ByPrefix covers RF5X-024: every residential IPv6
// customer controls at least a /64, so a per-/128 bucket gives one host an
// unlimited supply of fresh buckets and fresh LRU slots to evict real
// clients with.
func TestRateLimiterKeysIPv6ByPrefix(t *testing.T) {
	l := newRateLimiter(1, 8, 100, 0)
	now := testMono
	for i := range 20 {
		addr := netip.MustParseAddr(fmt.Sprintf("2001:db8:1:2::%x", i+1))
		l.allow(l.key(addr, 0), now)
	}
	if len(l.clients) != 1 {
		t.Fatalf("%d buckets for 20 addresses in one /64, want 1", len(l.clients))
	}
	// A different /64 is a different client.
	l.allow(l.key(netip.MustParseAddr("2001:db8:1:3::1"), 0), now)
	if len(l.clients) != 2 {
		t.Fatalf("%d buckets after a second /64, want 2", len(l.clients))
	}
	// IPv4 is unchanged: one bucket per address.
	l.allow(l.key(netip.MustParseAddr("192.0.2.1"), 0), now)
	l.allow(l.key(netip.MustParseAddr("192.0.2.2"), 0), now)
	if len(l.clients) != 4 {
		t.Fatalf("%d buckets after two IPv4 addresses, want 4", len(l.clients))
	}
	// An authenticated request has its own bucket, so a spoofed flood on
	// the same address cannot drain it.
	addr := netip.MustParseAddr("192.0.2.1")
	l.allow(l.key(addr, 7), now)
	if len(l.clients) != 5 {
		t.Fatalf("%d buckets after an authenticated request, want 5", len(l.clients))
	}
}

func FuzzReplyNeverAmplifies(f *testing.F) {
	f.Add(request(4), []byte{192, 0, 2, 1})
	f.Add(testKey.Append(request(4)), []byte{192, 0, 2, 2})
	h, _ := newTestHandler(f, nil)
	f.Fuzz(func(t *testing.T, req, addrBytes []byte) {
		var addr netip.Addr
		switch len(addrBytes) {
		case 4:
			var a [4]byte
			copy(a[:], addrBytes)
			addr = netip.AddrFrom4(a)
		case 16:
			var a [16]byte
			copy(a[:], addrBytes)
			addr = netip.AddrFrom16(a)
		default:
			return
		}
		got := h.Handle(req, netip.AddrPortFrom(addr, 123), testWall, testMono)
		if len(got) > len(req) {
			t.Fatalf("amplified %d-byte request to %d bytes", len(req), len(got))
		}
		// A crypto-NAK is 52 bytes, so it may only ever answer a request
		// that carried a MAC trailer of its own (RF5X-035).
		if _, mac, _, err := ntp.Decode(got); err == nil && mac.IsCryptoNAK() && len(req) < ntp.HeaderSize+ntp.CryptoNAKSize {
			t.Fatalf("crypto-NAK sent for a %d-byte request", len(req))
		}
	})
}

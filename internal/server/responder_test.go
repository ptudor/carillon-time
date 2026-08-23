package server

import (
	"encoding/binary"
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
		got := h.Handle(req, netip.MustParseAddr("192.0.2.10"), testWall, testMono.Add(time.Duration(version)*time.Second))
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
	if got := stats.Snapshot(); got.Served != 4 || got.Unsynced != 0 {
		t.Fatalf("stats %+v", got)
	}
}

func TestUnsynchronizedReply(t *testing.T) {
	h, stats := newTestHandler(t, func(c *Config) {
		c.Status = func() SystemStatus {
			return SystemStatus{Synced: false, Precision: -18, ReferenceID: ntp.KissSTEP}
		}
	})
	p, _, _ := decodeReply(t, h.Handle(request(4), netip.MustParseAddr("192.0.2.1"), testWall, testMono))
	if p.Leap != ntp.LeapUnsync || p.Stratum != 16 || p.ReferenceID != ntp.KissSTEP || p.RootDispersion.Seconds() != 16 || !p.ReferenceTime.IsZero() {
		t.Fatalf("unsynchronized reply %+v", p)
	}
	if got := stats.Snapshot(); got.Served != 1 || got.Unsynced != 1 {
		t.Fatalf("stats %+v", got)
	}
}

func TestModesLengthsAndACL(t *testing.T) {
	h, stats := newTestHandler(t, func(c *Config) {
		c.Deny = []netip.Prefix{netip.MustParsePrefix("192.0.2.64/26")}
	})
	if got := h.Handle(request(4)[:47], netip.MustParseAddr("192.0.2.1"), testWall, testMono); got != nil {
		t.Fatal("short request received a reply")
	}
	for _, mode := range []ntp.Mode{ntp.ModeSymmetricActive, ntp.ModeSymmetricPassive, ntp.ModeServer, ntp.ModeBroadcast, ntp.ModeControl, ntp.ModePrivate} {
		r := request(4)
		r[0] = r[0]&^7 | byte(mode)
		if got := h.Handle(r, netip.MustParseAddr("192.0.2.1"), testWall, testMono); got != nil {
			t.Fatalf("mode %v received a reply", mode)
		}
	}
	if got := h.Handle(request(4), netip.MustParseAddr("198.51.100.1"), testWall, testMono); got != nil {
		t.Fatal("address outside allow received a reply")
	}
	if got := h.Handle(request(4), netip.MustParseAddr("192.0.2.70"), testWall, testMono); got != nil {
		t.Fatal("deny did not override allow")
	}
	if got := stats.Snapshot(); got.Denied != 2 || got.Served != 0 {
		t.Fatalf("stats %+v", got)
	}
}

func TestAuthentication(t *testing.T) {
	h, stats := newTestHandler(t, func(c *Config) {
		c.RequireKey = map[netip.Prefix]uint32{netip.MustParsePrefix("192.0.2.0/25"): 1}
	})
	client := netip.MustParseAddr("192.0.2.10")
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
	bad := append([]byte(nil), signed...)
	bad[len(bad)-1] ^= 1
	if got := h.Handle(bad, client, testWall, testMono.Add(2*time.Second)); got != nil {
		t.Fatal("bad known MAC received a reply")
	}
	// An unknown MAC does not make an otherwise-open association trusted,
	// but it is ignored for clients that do not require a key.
	unknown := auth.Key{ID: 2, Secret: []byte("fedcba9876543210")}.Append(request(4))
	if got := h.Handle(unknown, netip.MustParseAddr("192.0.2.200"), testWall, testMono.Add(3*time.Second)); len(got) != ntp.HeaderSize {
		t.Fatalf("unknown optional MAC reply length %d", len(got))
	}
	if got := stats.Snapshot(); got.BadAuth != 2 || got.Served != 2 {
		t.Fatalf("stats %+v", got)
	}
}

func TestRateLimitAndKoDThrottle(t *testing.T) {
	h, stats := newTestHandler(t, func(c *Config) {
		c.RateLimitPPS = 1
		c.RateBurst = 1
		c.MinPoll = 5
	})
	client := netip.MustParseAddr("192.0.2.1")
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
	if got := stats.Snapshot(); got.Served != 2 || got.RateLimited != 2 {
		t.Fatalf("stats %+v", got)
	}
}

func TestRateLimiterExpiryAndBound(t *testing.T) {
	l := newRateLimiter(1, 1)
	l.maxClients = 2
	a := netip.MustParseAddr("192.0.2.1")
	b := netip.MustParseAddr("192.0.2.2")
	c := netip.MustParseAddr("192.0.2.3")
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
		got := h.Handle(req, addr, testWall, testMono)
		if len(got) > len(req) {
			t.Fatalf("amplified %d-byte request to %d bytes", len(req), len(got))
		}
	})
}

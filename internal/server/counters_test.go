package server

import (
	"net/netip"
	"testing"

	"github.com/ptudor/carillon-time/internal/ntp"
)

// modeRequest builds a well-formed packet carrying an arbitrary mode.
func modeRequest(version uint8, mode ntp.Mode) []byte {
	r := request(version)
	r[0] = r[0]&^7 | byte(mode)&7
	return r
}

func TestEveryDropIsCounted(t *testing.T) {
	client := from("192.0.2.10")
	tests := []struct {
		name    string
		request []byte
		want    func(CounterSnapshot) uint64
	}{
		{"short", request(4)[:47], func(c CounterSnapshot) uint64 { return c.Malformed }},
		{"bad trailer", append(request(4), 1, 2, 3), func(c CounterSnapshot) uint64 { return c.Malformed }},
		{"version zero", modeVersion(request(4), 0), func(c CounterSnapshot) uint64 { return c.BadVersion }},
		{"version five", modeVersion(request(4), 5), func(c CounterSnapshot) uint64 { return c.BadVersion }},
		{"control mode", modeRequest(4, ntp.ModeControl), func(c CounterSnapshot) uint64 { return c.NonClient }},
		{"private mode", modeRequest(4, ntp.ModePrivate), func(c CounterSnapshot) uint64 { return c.NonClient }},
		{"server mode", modeRequest(4, ntp.ModeServer), func(c CounterSnapshot) uint64 { return c.NonClient }},
		{"reserved mode, version 4", modeRequest(4, ntp.ModeReserved), func(c CounterSnapshot) uint64 { return c.NonClient }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, stats := newTestHandler(t, nil)
			if got := h.Handle(tc.request, client, testWall, testMono); got != nil {
				t.Fatalf("reply of %d bytes", len(got))
			}
			total := stats.Snapshot().Total
			if tc.want(total) != 1 || total.Requests() != 1 || total.Served != 0 {
				t.Fatalf("stats %+v", total)
			}
		})
	}
}

// TestAmplificationProbesAreVisible is the counter a public server is watched
// for: modes 6 and 7 carry every NTP amplification attack, and until they are
// counted an operator cannot tell a scan from silence.
func TestAmplificationProbesAreVisible(t *testing.T) {
	h, stats := newTestHandler(t, openACL)
	for range 3 {
		h.Handle(modeRequest(4, ntp.ModeControl), from("198.51.100.1"), testWall, testMono)
	}
	h.Handle(modeRequest(4, ntp.ModePrivate), from("198.51.100.1"), testWall, testMono)
	total := stats.Snapshot().Total
	if total.Modes[ntp.ModeControl] != 3 || total.Modes[ntp.ModePrivate] != 1 || total.NonClient != 4 {
		t.Fatalf("stats %+v", total)
	}
	if total.Served != 0 || total.Modes[ntp.ModeClient] != 0 {
		t.Fatalf("a mode 6 or 7 probe must never be answered: %+v", total)
	}
}

// TestNTPv1ClientsUsingModeZero covers the pre-mode-field clients that still
// appear on the public pool. ntpd answers them; every other reserved-mode
// packet is refused.
func TestNTPv1ClientsUsingModeZero(t *testing.T) {
	h, stats := newTestHandler(t, nil)
	got := h.Handle(modeRequest(1, ntp.ModeReserved), from("192.0.2.10"), testWall, testMono)
	if len(got) != ntp.HeaderSize {
		t.Fatalf("NTPv1 mode 0 reply length %d", len(got))
	}
	p, _, _ := decodeReply(t, got)
	if p.Version != 1 || p.Mode != ntp.ModeServer {
		t.Fatalf("reply %+v", p)
	}
	total := stats.Snapshot().Total
	if total.Served != 1 || total.NonClient != 0 || total.Versions[1] != 1 {
		t.Fatalf("stats %+v", total)
	}
}

func TestVersionHistogram(t *testing.T) {
	h, stats := newTestHandler(t, nil)
	for version := uint8(1); version <= 4; version++ {
		for range int(version) {
			h.Handle(request(version), from("192.0.2.10"), testWall, testMono)
		}
	}
	total := stats.Snapshot().Total
	for version := 1; version <= 4; version++ {
		if total.Versions[version] != uint64(version) {
			t.Fatalf("version %d counted %d times: %+v", version, total.Versions[version], total)
		}
	}
	if total.Versions[0] != 0 {
		t.Fatalf("version 0 is rejected by the decoder: %+v", total)
	}
}

func TestCountersSplitByAddressFamily(t *testing.T) {
	h, stats := newTestHandler(t, openACL)
	h.Handle(request(4), from("198.51.100.1"), testWall, testMono)
	h.Handle(request(4), from("198.51.100.2"), testWall, testMono)
	h.Handle(request(4), from("2001:db8::1"), testWall, testMono)
	h.Handle(modeRequest(4, ntp.ModeControl), from("2001:db8::2"), testWall, testMono)

	s := stats.Snapshot()
	if s.IPv4.Served != 2 || s.IPv4.NonClient != 0 {
		t.Fatalf("ipv4 %+v", s.IPv4)
	}
	if s.IPv6.Served != 1 || s.IPv6.NonClient != 1 {
		t.Fatalf("ipv6 %+v", s.IPv6)
	}
	if s.Total.Served != 3 || s.Total.NonClient != 1 || s.Total.Requests() != 4 {
		t.Fatalf("total %+v", s.Total)
	}
	// A v4-mapped source is charted as the IPv4 traffic it is.
	h.Handle(request(4), netip.AddrPortFrom(netip.MustParseAddr("::ffff:198.51.100.3"), 45678), testWall, testMono)
	if got := stats.Snapshot(); got.IPv4.Served != 3 || got.IPv6.Served != 1 {
		t.Fatalf("v4-mapped source charted as %+v / %+v", got.IPv4, got.IPv6)
	}
}

func TestClientGaugeTracksTheRateLimitTable(t *testing.T) {
	h, stats := newTestHandler(t, func(c *Config) {
		openACL(c)
		c.MaxClients = 2
	})
	h.Handle(request(4), from("198.51.100.1"), testWall, testMono)
	if got := stats.Snapshot().IPv4.Clients; got != 1 {
		t.Fatalf("clients %d", got)
	}
	h.Handle(request(4), from("198.51.100.2"), testWall, testMono)
	if got := stats.Snapshot().IPv4.Clients; got != 2 {
		t.Fatalf("clients %d", got)
	}
	// The table is bounded: a third client evicts the least recently used
	// one rather than growing without limit.
	h.Handle(request(4), from("198.51.100.3"), testWall, testMono)
	if got := stats.Snapshot().IPv4.Clients; got != 2 {
		t.Fatalf("bounded table reported %d clients", got)
	}
}

// modeVersion overwrites a packet's version field in place.
func modeVersion(b []byte, version uint8) []byte {
	out := append([]byte(nil), b...)
	out[0] = out[0]&^0x38 | (version&0x7)<<3
	return out
}

// TestOverflowDelta covers the arithmetic behind the kernel drop counter.
// The kernel's own counter is cumulative and 32 bits wide.
func TestOverflowDelta(t *testing.T) {
	tests := []struct {
		name     string
		previous uint32
		seen     bool
		current  uint32
		want     uint64
	}{
		{"first reading is taken whole", 0, false, 12, 12},
		{"first reading of a quiet socket", 0, false, 0, 0},
		{"no new drops", 40, true, 40, 0},
		{"steady drops", 40, true, 57, 17},
		{"counter wrapped", 0xffffffff - 2, true, 5, 8},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := overflowDelta(tc.previous, tc.seen, tc.current); got != tc.want {
				t.Fatalf("delta %d, want %d", got, tc.want)
			}
		})
	}
}

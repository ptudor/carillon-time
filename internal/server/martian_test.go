package server

import (
	"net/netip"
	"testing"

	"carillon/internal/ntp"
)

// openACL is the configuration of a public NTP pool server: every source
// address is inside the ACL, so the martian checks are the only thing left
// standing between a forged datagram and a reply.
func openACL(c *Config) {
	c.Allow = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("2000::/3")}
	c.Deny = nil
}

func TestMartianSourcesAreRefusedByAnOpenACL(t *testing.T) {
	refused := []string{
		"0.0.0.0",         // unspecified
		"0.1.2.3",         // 0.0.0.0/8, "this network"
		"224.0.0.1",       // multicast
		"239.1.2.3",       // administratively scoped multicast
		"255.255.255.255", // limited broadcast
		"::",              // unspecified
		"ff02::101",       // the NTP multicast group
	}
	for _, addr := range refused {
		h, stats := newTestHandler(t, openACL)
		if got := h.Handle(request(4), from(addr), testWall, testMono); got != nil {
			t.Fatalf("%s: martian source received a %d-byte reply", addr, len(got))
		}
		total := stats.Snapshot().Total
		if total.Martian != 1 || total.Served != 0 || total.Denied != 0 {
			t.Fatalf("%s: stats %+v", addr, total)
		}
	}

	// A source port of zero leaves nowhere to send the answer.
	h, stats := newTestHandler(t, openACL)
	zeroPort := netip.AddrPortFrom(netip.MustParseAddr("198.51.100.1"), 0)
	if got := h.Handle(request(4), zeroPort, testWall, testMono); got != nil {
		t.Fatal("source port zero received a reply")
	}
	if total := stats.Snapshot().Total; total.Martian != 1 {
		t.Fatalf("stats %+v", total)
	}
}

func TestRoutableSourcesAreStillServedByAnOpenACL(t *testing.T) {
	// Loopback is deliberately served: a host queries its own server over
	// 127.0.0.1, and the kernel already drops loopback-sourced packets that
	// arrive on a real interface.
	for _, addr := range []string{"198.51.100.1", "127.0.0.1", "2001:db8::1"} {
		h, stats := newTestHandler(t, openACL)
		if got := h.Handle(request(4), from(addr), testWall, testMono); len(got) != ntp.HeaderSize {
			t.Fatalf("%s: reply length %d", addr, len(got))
		}
		if total := stats.Snapshot().Total; total.Served != 1 || total.Martian != 0 {
			t.Fatalf("%s: stats %+v", addr, total)
		}
	}
}

// TestOpenIPv6ACLExcludesLoopback records a consequence of the recommended
// public prefix: 2000::/3 is global unicast only, so a host that also wants
// to query itself over ::1 must allow ::1/128 explicitly. That is an ACL
// decision, not a martian, and it is counted as one.
func TestOpenIPv6ACLExcludesLoopback(t *testing.T) {
	h, stats := newTestHandler(t, openACL)
	if got := h.Handle(request(4), from("::1"), testWall, testMono); got != nil {
		t.Fatalf("::1 is outside 2000::/3 but got a %d-byte reply", len(got))
	}
	if total := stats.Snapshot().Total; total.Denied != 1 || total.Martian != 0 {
		t.Fatalf("stats %+v", total)
	}
}

func TestMartianDestination(t *testing.T) {
	for _, addr := range []string{"224.0.0.1", "255.255.255.255", "0.0.0.0", "ff02::101", "::"} {
		if !martianDestination(netip.MustParseAddr(addr)) {
			t.Fatalf("%s must not be answered as a unicast destination", addr)
		}
	}
	for _, addr := range []string{"198.51.100.1", "127.0.0.1", "2001:db8::1"} {
		if martianDestination(netip.MustParseAddr(addr)) {
			t.Fatalf("%s is a legitimate destination", addr)
		}
	}
	// No destination control message: the kernel chooses the reply's source.
	if martianDestination(netip.Addr{}) {
		t.Fatal("an absent destination must not drop the request")
	}
}

package server

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/carillon-time/internal/ntp"
)

func TestUDPListenerRoundTrip(t *testing.T) {
	// Exercise both source-selection paths. FreeBSD rejects IP_SENDSRCADDR
	// with a specific IPv4 bind; wildcard listeners need it to answer from
	// the address the client queried.
	for _, bind := range []string{"127.0.0.1:0", "0.0.0.0:0"} {
		t.Run(bind, func(t *testing.T) { testUDPListenerRoundTrip(t, bind) })
	}
}

func testUDPListenerRoundTrip(t *testing.T, bind string) {
	t.Helper()
	stats := &Stats{}
	s, err := Listen(ServiceConfig{
		Listen: []netip.AddrPort{netip.MustParseAddrPort(bind)},
		Handler: Config{
			Allow:        []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
			RateLimitPPS: 8,
			RateBurst:    16,
			KoD:          true,
			Status:       testStatus,
			Now:          time.Now,
			Stats:        stats,
		},
		Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Addrs()) != 1 || s.Addrs()[0].Port() == 0 {
		t.Fatalf("bound addresses %v", s.Addrs())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve shutdown: %v", err)
		}
	}()

	peer := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), s.Addrs()[0].Port())
	conn, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(peer))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	req := request(4)
	before := time.Now()
	if _, err := conn.Write(req); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, ntp.MaxPacketSize)
	n, err := conn.Read(buf)
	after := time.Now()
	if err != nil {
		t.Fatal(err)
	}
	p, _, _ := decodeReply(t, buf[:n])
	if p.Mode != ntp.ModeServer || p.Stratum != testStatus().Stratum || p.OriginTime != ntp.FromTime(testWall.Add(-10*time.Millisecond)) {
		t.Fatalf("reply %+v", p)
	}
	rx := p.ReceiveTime.Time(before)
	if rx.Before(before.Add(-time.Millisecond)) || rx.After(after.Add(time.Millisecond)) {
		t.Fatalf("receive timestamp %v outside exchange %v..%v", rx, before, after)
	}
	if got := stats.Snapshot().Total; got.Served != 1 {
		t.Fatalf("stats %+v", got)
	}

}

func TestListenRejectsEmptyAddresses(t *testing.T) {
	if _, err := Listen(ServiceConfig{}); err == nil {
		t.Fatal("expected an error")
	}
}

// TestListenAcceptsAnOversizedReceiveBuffer covers RF5X-009: FreeBSD refuses
// an SO_RCVBUF above kern.ipc.maxsockbuf with ENOBUFS instead of clamping it
// the way Linux does, so the example configuration's 4 MB request stopped the
// daemon starting. The listener must halve the request until the kernel
// accepts it and carry on serving time.
func TestListenAcceptsAnOversizedReceiveBuffer(t *testing.T) {
	s, err := Listen(ServiceConfig{
		Listen: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:0")},
		Handler: Config{
			Allow:        []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
			RateLimitPPS: 8,
			RateBurst:    16,
			KoD:          true,
			Status:       testStatus,
			Now:          time.Now,
			Stats:        &Stats{},
		},
		Log:        slog.New(slog.DiscardHandler),
		RecvBuffer: 256 << 20,
	})
	if err != nil {
		t.Fatalf("Listen with an oversized receive buffer: %v", err)
	}
	defer s.Close()
	if got := s.ReceiveBuffers(); len(got) != 1 || got[0] <= 0 {
		t.Fatalf("effective receive buffers %v", got)
	}
}

func TestReceiveBufferCapacity(t *testing.T) {
	const request = 4 << 20
	for _, tc := range []struct {
		os             string
		readback, want int
	}{
		{"freebsd", request, request}, {"freebsd", request / 2, request / 2},
		{"linux", 2 * request, request}, {"linux", request, request / 2},
	} {
		var out bytes.Buffer
		reportReceiveBuffer(slog.New(slog.NewTextHandler(&out, nil)), netip.MustParseAddrPort("[::1]:123"), request, tc.readback, tc.os)
		if tc.want == request && out.Len() != 0 {
			t.Errorf("full grant warned: %s", out.String())
		}
		if tc.want < request && !strings.Contains(out.String(), "requested=4194304 granted=2097152") {
			t.Errorf("missing shortfall: %s", out.String())
		}
	}
}

func TestRecvBufferSysctlNamesAKnob(t *testing.T) {
	if RecvBufferSysctl() == "" {
		t.Fatal("no sysctl name to point the operator at")
	}
}

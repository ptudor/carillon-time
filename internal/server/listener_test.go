package server

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"

	"carillon/internal/ntp"
)

func TestUDPListenerRoundTrip(t *testing.T) {
	stats := &Stats{}
	s, err := Listen(ServiceConfig{
		Listen: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:0")},
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

	conn, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(s.Addrs()[0]))
	if err != nil {
		cancel()
		<-done
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
	if got := stats.Snapshot(); got.Served != 1 {
		t.Fatalf("stats %+v", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve shutdown: %v", err)
	}
}

func TestListenRejectsEmptyAddresses(t *testing.T) {
	if _, err := Listen(ServiceConfig{}); err == nil {
		t.Fatal("expected an error")
	}
}

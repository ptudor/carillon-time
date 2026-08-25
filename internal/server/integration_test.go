package server_test

import (
	"context"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"carillon/internal/clock"
	"carillon/internal/ntp"
	"carillon/internal/ntp/auth"
	"carillon/internal/server"
	"carillon/internal/source"
)

func TestClientServerEndToEnd(t *testing.T) {
	key := auth.Key{ID: 1, Secret: []byte("0123456789abcdef")}
	stats := &server.Stats{}
	s, err := server.Listen(server.ServiceConfig{
		Listen: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:0")},
		Handler: server.Config{
			Allow:        []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
			RequireKey:   map[netip.Prefix]uint32{netip.MustParsePrefix("127.0.0.0/8"): 1},
			Keys:         auth.Keys{1: key},
			RateLimitPPS: 8,
			RateBurst:    16,
			KoD:          true,
			Status: func() server.SystemStatus {
				return server.SystemStatus{
					Synced: true, Leap: ntp.LeapNone, Stratum: 2, Precision: -20,
					RootDelay: 0.001, RootDispersion: 0.002,
					ReferenceID: ntp.RefIDFromString("TEST"), ReferenceTime: time.Now().Add(-time.Second),
				}
			},
			Now:   time.Now,
			Stats: stats,
		},
		Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("server shutdown: %v", err)
		}
	}()

	qctx, qcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer qcancel()
	result, err := source.Query(qctx, s.Addrs()[0].String(), &key, clock.ReadOnly(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Authenticated || result.Packet.Stratum != 2 || result.Packet.ReferenceID != ntp.RefIDFromString("TEST") {
		t.Fatalf("query result %+v", result)
	}
	if result.Delay < 0 || result.Delay > (50*time.Millisecond).Seconds() {
		t.Fatalf("loopback delay %.6f", result.Delay)
	}
	if got := stats.Snapshot().Total; got.Served != 1 || got.BadAuth != 0 {
		t.Fatalf("stats %+v", got)
	}
}

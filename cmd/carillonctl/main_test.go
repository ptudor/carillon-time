package main

import (
	"testing"
	"time"

	"carillon/internal/control"
	"carillon/internal/ntp"
)

func TestHistogramSkipsEmptyBuckets(t *testing.T) {
	name := func(i int) string { return ntp.Mode(i).String() }
	if got := histogram([]uint64{0, 0, 0, 0, 0, 0, 0, 0}, name); got != "none" {
		t.Fatalf("empty histogram rendered %q", got)
	}
	got := histogram([]uint64{0, 0, 0, 0, 12, 0, 340, 5}, name)
	if got != "server=12 control=340 private=5" {
		t.Fatalf("histogram rendered %q", got)
	}
}

// TestPrintServerStatsLayout renders a populated snapshot so `go test -v`
// shows the operator view that carillonctl prints.
func TestPrintServerStatsLayout(t *testing.T) {
	now := time.Date(2026, 8, 24, 23, 37, 21, 0, time.UTC)
	v4 := control.CounterStats{
		Served: 3200, Unsynced: 24, KoD: 3, Denied: 0, Martian: 3,
		RateLimited: 10, BadVersion: 1, NonClient: 64, Malformed: 2,
		Clients: 380, Modes: []uint64{0, 0, 0, 0, 58, 0, 6, 0},
		Versions: []uint64{0, 1, 14, 900, 2285}, LastRequest: now, LastServed: now,
	}
	v6 := control.CounterStats{
		Served: 546, RateLimited: 2, NonClient: 6, KoD: 1, Clients: 32,
		Modes: []uint64{0, 0, 0, 0, 6, 0, 0, 0}, Versions: []uint64{0, 0, 0, 40, 506},
		LastRequest: now, LastServed: now,
	}
	stats := &control.ServerStats{CounterStats: sumForTest(v4, v6), IPv4: v4, IPv6: v6}
	if got := stats.Requests(); got != 3834 {
		t.Fatalf("received datagrams %d", got)
	}
	printServerStats(stats)
}

func sumForTest(a, b control.CounterStats) control.CounterStats {
	out := control.CounterStats{
		Served: a.Served + b.Served, Unsynced: a.Unsynced + b.Unsynced, KoD: a.KoD + b.KoD,
		Denied: a.Denied + b.Denied, Martian: a.Martian + b.Martian,
		RateLimited: a.RateLimited + b.RateLimited, BadAuth: a.BadAuth + b.BadAuth,
		BadVersion: a.BadVersion + b.BadVersion, NonClient: a.NonClient + b.NonClient,
		Malformed: a.Malformed + b.Malformed, Oversize: a.Oversize + b.Oversize,
		NoKernelTS: a.NoKernelTS + b.NoKernelTS, KernelDrops: a.KernelDrops + b.KernelDrops,
		Clients: a.Clients + b.Clients, LastRequest: a.LastRequest, LastServed: a.LastServed,
		Modes: make([]uint64, len(a.Modes)), Versions: make([]uint64, len(a.Versions)),
	}
	for i := range a.Modes {
		out.Modes[i] = a.Modes[i] + b.Modes[i]
	}
	for i := range a.Versions {
		out.Versions[i] = a.Versions[i] + b.Versions[i]
	}
	return out
}

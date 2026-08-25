package main

import (
	"testing"
	"time"

	"carillon/internal/control"
)

func TestHistogramSkipsEmptyBuckets(t *testing.T) {
	if got := histogram(nil, control.ModeOrder, ""); got != "none" {
		t.Fatalf("empty histogram rendered %q", got)
	}
	// Rendering follows ModeOrder, not the map's iteration order.
	got := histogram(map[string]uint64{"private": 5, "server": 12, "control": 340}, control.ModeOrder, "")
	if got != "server=12 control=340 private=5" {
		t.Fatalf("histogram rendered %q", got)
	}
	if got := histogram(map[string]uint64{"3": 900, "4": 2300}, control.VersionOrder, "v"); got != "v3=900 v4=2300" {
		t.Fatalf("version histogram rendered %q", got)
	}
}

// TestPrintServerStatsLayout renders a populated snapshot so `go test -v`
// shows the operator view that carillonctl prints.
func TestPrintServerStatsLayout(t *testing.T) {
	now := time.Date(2026, 8, 24, 23, 37, 21, 0, time.UTC)
	v4 := control.CounterStats{
		Served: 3200, Unsynced: 24, KoD: 3, Denied: 0, Martian: 3,
		RateLimited: 10, BadVersion: 1, NonClient: 64, Malformed: 2,
		Clients: 380, Modes: map[string]uint64{"server": 58, "control": 6},
		Versions:    map[string]uint64{"1": 1, "2": 14, "3": 900, "4": 2285},
		LastRequest: &now, LastServed: &now,
	}
	v6 := control.CounterStats{
		Served: 546, RateLimited: 2, NonClient: 6, KoD: 1, Clients: 32,
		Modes: map[string]uint64{"server": 6}, Versions: map[string]uint64{"3": 40, "4": 506},
		LastRequest: &now, LastServed: &now,
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
		Modes: map[string]uint64{}, Versions: map[string]uint64{},
	}
	for _, from := range []control.CounterStats{a, b} {
		for k, v := range from.Modes {
			out.Modes[k] += v
		}
		for k, v := range from.Versions {
			out.Versions[k] += v
		}
	}
	return out
}

package stats

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carillon/internal/discipline"
	"carillon/internal/engine"
	"carillon/internal/ntp"
	ntpserver "carillon/internal/server"
	"carillon/internal/source"
)

func TestRecorderWritesAndDeduplicatesDailyFiles(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir})
	now := time.Date(2026, 8, 23, 23, 59, 59, 123, time.UTC)
	pulse := now.Add(-time.Millisecond)
	st := &engine.Status{
		Status: discipline.Status{
			State: discipline.StateSynced, Stratum: 1, SystemSource: "pps\t0",
			Offset: 1e-6, Frequency: 2.5, Jitter: 3e-7, Pending: 4e-6,
			RootDelay: 0, RootDisp: 5e-6, Updates: 1,
			Sources: []discipline.SourceStatus{{
				Name: "pps\t0", Status: discipline.StatusSystem, Reach: 0xff, Poll: 4,
				Offset: 1e-6, Jitter: 3e-7, Distance: 5e-6,
				RefID: ntp.RefIDFromString("PPS"), Leap: ntp.LeapNone,
			}},
		},
		Now: now,
		Infos: map[string]source.Info{
			"pps\t0": {Refclock: &source.RefclockInfo{Sequence: 42, LastPulse: pulse, LastOffset: 9e-7}},
		},
	}
	r.Record(st)
	r.Record(st)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx)

	// Files live under Dir/YYYY/MM/DD so that a directory never collects a
	// year of them and retention is a per-day removal (RF5X-029).
	want := map[string][]string{
		"2026/08/23/loop.tsv":    {"time\tstate", "pps\\t0", "\t2.5\t"},
		"2026/08/23/sources.tsv": {"time\tsource\tstatus", "pps\\t0\tsystem\t255"},
		"2026/08/23/pps.tsv":     {"time\tsource\toffset_seconds\tsequence", "pps\\t0\t9e-07\t42"},
	}
	for name, parts := range want {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		for _, part := range parts {
			if !strings.Contains(text, part) {
				t.Errorf("%s missing %q:\n%s", name, part, text)
			}
		}
		if lines := strings.Count(strings.TrimSpace(text), "\n") + 1; lines != 2 {
			t.Errorf("%s has %d lines, want header + one row:\n%s", name, lines, text)
		}
	}
}

func TestRecorderRotatesAtUTCMidnight(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir})
	for i, day := range []int{23, 24} {
		at := time.Date(2026, 8, day, 12, 0, 0, 0, time.FixedZone("west", -7*3600))
		r.Record(&engine.Status{Status: discipline.Status{State: discipline.StateSynced, Updates: i + 1}, Now: at, Infos: map[string]source.Info{}})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx)
	for _, name := range []string{"2026/08/23/loop.tsv", "2026/08/24/loop.tsv"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
}

// TestRecorderPrunesExpiredDays covers the retention half of RF5X-029: four
// files a day with pps.tsv at 86,400 rows is tens of GB over a soak, and
// there was no way to age any of it out.
func TestRecorderPrunesExpiredDays(t *testing.T) {
	dir := t.TempDir()
	planted := filepath.Join(dir, "2026", "08", "20")
	if err := os.MkdirAll(planted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planted, "loop.tsv"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "2026", "08", "23")
	if err := os.MkdirAll(keep, 0o755); err != nil {
		t.Fatal(err)
	}

	r := New(Config{Dir: dir, KeepDays: 1})
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	r.Record(&engine.Status{Status: discipline.Status{State: discipline.StateSynced, Updates: 1}, Now: at, Infos: map[string]source.Info{}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx)

	if _, err := os.Stat(planted); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expired day survived: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("a day inside keep_days was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026", "08", "24", "loop.tsv")); err != nil {
		t.Errorf("today's file: %v", err)
	}
}

// TestRecorderKeepsEverythingByDefault: keep_days 0 removes nothing.
func TestRecorderKeepsEverythingByDefault(t *testing.T) {
	dir := t.TempDir()
	planted := filepath.Join(dir, "2020", "01", "01")
	if err := os.MkdirAll(planted, 0o755); err != nil {
		t.Fatal(err)
	}
	r := New(Config{Dir: dir})
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	r.Record(&engine.Status{Status: discipline.Status{State: discipline.StateSynced, Updates: 1}, Now: at, Infos: map[string]source.Info{}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx)
	if _, err := os.Stat(planted); err != nil {
		t.Errorf("keep_days 0 must keep everything: %v", err)
	}
}

func TestRecorderWritesServerTrafficPerFamily(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 8, 24, 23, 37, 21, 0, time.UTC)
	snapshot := ntpserver.StatsSnapshot{
		IPv4: ntpserver.CounterSnapshot{
			Served: 3200, Unsynced: 24, KoD: 3, Martian: 3, NonClient: 64,
			Malformed: 2, Clients: 380, KernelDrops: 7,
			Modes:    [8]uint64{4: 58, 6: 6},
			Versions: [5]uint64{3: 900, 4: 2300},
		},
		IPv6: ntpserver.CounterSnapshot{Served: 546, Clients: 32, Versions: [5]uint64{4: 546}},
	}
	r := New(Config{
		Dir:    dir,
		Server: func() ntpserver.StatsSnapshot { return snapshot },
		Now:    func() time.Time { return at },
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx)

	text, err := os.ReadFile(filepath.Join(dir, "2026", "08", "24", "server.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(text)), "\n")
	if len(lines) != 3 {
		t.Fatalf("want a header and one row per family:\n%s", text)
	}
	header := strings.Split(lines[0], "\t")
	v4 := strings.Split(lines[1], "\t")
	v6 := strings.Split(lines[2], "\t")
	if len(v4) != len(header) || len(v6) != len(header) {
		t.Fatalf("row width %d/%d against %d columns", len(v4), len(v6), len(header))
	}
	column := func(row []string, name string) string {
		for i, h := range header {
			if h == name {
				return row[i]
			}
		}
		t.Fatalf("no column %q in %v", name, header)
		return ""
	}
	if column(v4, "family") != "ipv4" || column(v6, "family") != "ipv6" {
		t.Fatalf("families %q %q", column(v4, "family"), column(v6, "family"))
	}
	for _, want := range []struct{ name, value string }{
		{"time", at.Format(time.RFC3339Nano)},
		{"served", "3200"},
		{"martian", "3"},
		{"nonclient", "64"},
		{"mode_control", "6"},
		{"mode_private", "0"},
		{"kernel_drops", "7"},
		{"clients", "380"},
		{"v4", "2300"},
	} {
		if got := column(v4, want.name); got != want.value {
			t.Errorf("ipv4 %s = %q, want %q", want.name, got, want.value)
		}
	}
	if column(v6, "served") != "546" || column(v6, "clients") != "32" {
		t.Errorf("ipv6 row: %v", v6)
	}
}

// TestRecorderWithoutAServerWritesNoTrafficFile covers a client-only daemon.
func TestRecorderWithoutAServerWritesNoTrafficFile(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir, Now: func() time.Time { return time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC) }})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx)
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("entries %v, err %v", entries, err)
	}
}

package stats

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carillon/internal/discipline"
	"carillon/internal/engine"
	"carillon/internal/ntp"
	"carillon/internal/source"
)

func TestRecorderWritesAndDeduplicatesDailyFiles(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, nil)
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

	want := map[string][]string{
		"loop.2026-08-23.tsv":    {"time\tstate", "pps\\t0", "\t2.5\t"},
		"sources.2026-08-23.tsv": {"time\tsource\tstatus", "pps\\t0\tsystem\t255"},
		"pps.2026-08-23.tsv":     {"time\tsource\toffset_seconds\tsequence", "pps\\t0\t9e-07\t42"},
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
	r := New(dir, nil)
	for i, day := range []int{23, 24} {
		at := time.Date(2026, 8, day, 12, 0, 0, 0, time.FixedZone("west", -7*3600))
		r.Record(&engine.Status{Status: discipline.Status{State: discipline.StateSynced, Updates: i + 1}, Now: at, Infos: map[string]source.Info{}})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx)
	for _, name := range []string{"loop.2026-08-23.tsv", "loop.2026-08-24.tsv"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
}

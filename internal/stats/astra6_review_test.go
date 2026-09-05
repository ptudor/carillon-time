package stats

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"carillon/internal/discipline"
	"carillon/internal/engine"
	ntpserver "carillon/internal/server"
	"carillon/internal/source"
)

// plantDay creates a dated statistics directory with one row in it and
// returns the path of the file.
func plantDay(t *testing.T, dir string, y, m, d int) string {
	t.Helper()
	p := filepath.Join(dir, fmtDay(y, m, d), "loop.tsv")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func fmtDay(y, m, d int) string {
	return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC).Format("2006/01/02")
}

func syncedAt(at time.Time) *engine.Status {
	return &engine.Status{
		Status: discipline.Status{State: discipline.StateSynced, Updates: 1},
		Now:    at,
		Infos:  map[string]source.Info{},
	}
}

func drain(t *testing.T, r *Recorder) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx)
}

// TestAstra6UntrustedWallTimeDoesNotPrune is the review's RA6X-046 probe. Any
// new dated file used to invoke retention from that row's own wall date, so a
// future RTC at boot erased every retained day on the first minute tick — or
// on the shutdown server row — with no synchronized engine state anywhere in
// the picture.
func TestAstra6UntrustedWallTimeDoesNotPrune(t *testing.T) {
	dir := t.TempDir()
	p := plantDay(t, dir, 2026, 9, 5)
	r := New(Config{
		Dir: dir, KeepDays: 7,
		Now:    func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) },
		Server: func() ntpserver.StatsSnapshot { return ntpserver.StatsSnapshot{} },
	})
	defer r.closeFiles()
	if err := r.recordServer(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("untrusted future RTC deleted retained evidence: %v", err)
	}
}

// TestAstra6RetentionHorizon covers RA6X-046's verification list.
func TestAstra6RetentionHorizon(t *testing.T) {
	t.Run("a future RTC before sync prunes nothing", func(t *testing.T) {
		dir := t.TempDir()
		p := plantDay(t, dir, 2026, 9, 5)
		r := New(Config{Dir: dir, KeepDays: 7})
		// Unsynchronized snapshots at a wildly future wall time: the rows
		// are still written, under their own dates, but nothing is removed.
		st := syncedAt(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
		st.State = discipline.StateUnsynced
		r.Record(st)
		drain(t, r)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("retained evidence deleted before the clock was trusted: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, fmtDay(2099, 1, 1), "loop.tsv")); err != nil {
			t.Fatalf("the unsynchronized observation was not recorded: %v", err)
		}
	})

	t.Run("a past RTC before sync prunes nothing", func(t *testing.T) {
		dir := t.TempDir()
		p := plantDay(t, dir, 2026, 9, 5)
		r := New(Config{Dir: dir, KeepDays: 7})
		st := syncedAt(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC))
		st.State = discipline.StateSettling
		r.Record(st)
		drain(t, r)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("retained evidence deleted while settling: %v", err)
		}
	})

	t.Run("a corrected RTC prunes once trusted", func(t *testing.T) {
		dir := t.TempDir()
		expired := plantDay(t, dir, 2026, 1, 1)
		recent := plantDay(t, dir, 2026, 9, 4)
		r := New(Config{Dir: dir, KeepDays: 7})
		// Boot at a wrong time, then synchronize at the right one.
		wrong := syncedAt(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC))
		wrong.State = discipline.StateUnsynced
		r.Record(wrong)
		good := syncedAt(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
		good.Updates = 2
		r.Record(good)
		drain(t, r)
		if _, err := os.Stat(expired); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("expired day survived a trusted horizon: %v", err)
		}
		if _, err := os.Stat(recent); err != nil {
			t.Errorf("a day inside keep_days was removed: %v", err)
		}
	})

	t.Run("the horizon never retreats", func(t *testing.T) {
		dir := t.TempDir()
		old := plantDay(t, dir, 2026, 1, 1)
		r := New(Config{Dir: dir, KeepDays: 7})
		good := syncedAt(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
		r.Record(good)
		drain(t, r)
		if _, err := os.Stat(old); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("setup: expired day survived: %v", err)
		}
		// A backward excursion must not become the retention clock.
		before := r.horizon
		back := syncedAt(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
		back.Updates = 2
		r.noteHorizon(back)
		if !r.horizon.Equal(before) {
			t.Fatalf("horizon retreated from %v to %v", before, r.horizon)
		}
	})

	t.Run("a misdated pulse row does not prune", func(t *testing.T) {
		dir := t.TempDir()
		p := plantDay(t, dir, 2026, 9, 5)
		r := New(Config{Dir: dir, KeepDays: 7})
		// A PPS row carries the source's own timestamp. Even a wildly
		// misdated one must not move retention.
		st := &engine.Status{
			Status: discipline.Status{State: discipline.StateUnsynced},
			Now:    time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
			Infos: map[string]source.Info{
				"pps0": {Refclock: &source.RefclockInfo{
					LastPulse: time.Date(2099, 6, 1, 0, 0, 0, 0, time.UTC),
					Sequence:  1,
				}},
			},
		}
		r.Record(st)
		drain(t, r)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("a misdated pulse row deleted retained evidence: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, fmtDay(2099, 6, 1), "pps.tsv")); err != nil {
			t.Fatalf("the pulse row was not recorded under its own date: %v", err)
		}
	})

	t.Run("keep_days 0 removes nothing even when trusted", func(t *testing.T) {
		dir := t.TempDir()
		p := plantDay(t, dir, 2020, 1, 1)
		r := New(Config{Dir: dir})
		r.Record(syncedAt(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)))
		drain(t, r)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("keep_days 0 must keep everything: %v", err)
		}
	})
}

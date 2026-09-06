package stats

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/carillon-time/internal/discipline"
	"github.com/ptudor/carillon-time/internal/engine"
	ntpserver "github.com/ptudor/carillon-time/internal/server"
	"github.com/ptudor/carillon-time/internal/source"
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
		r.Record(&engine.Status{
			Status: discipline.Status{State: discipline.StateUnsynced},
			Now:    time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
			Infos:  map[string]source.Info{},
		})
		r.Pulse(source.Pulse{
			Source: "pps0", At: time.Date(2099, 6, 1, 0, 0, 0, 0, time.UTC), Sequence: 1,
		})
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

// TestAstra6EveryAcceptedPulseIsRecorded covers RA6X-048. Pulse rows were
// derived from each source's latest Info when the engine published a
// snapshot, so several pulses arriving between two publications collapsed
// into one row and the earlier ones vanished with no drop counted.
func TestAstra6EveryAcceptedPulseIsRecorded(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir})
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	// Several pulses accepted while nothing is draining the recorder — the
	// case that used to coalesce.
	const pulses = 25
	for i := 0; i < pulses; i++ {
		r.Pulse(source.Pulse{
			Source:   "pps0",
			At:       base.Add(time.Duration(i) * time.Second),
			Offset:   float64(i) * 1e-9,
			Sequence: uint32(100 + i),
		})
	}
	drain(t, r)

	b, err := os.ReadFile(filepath.Join(dir, "2026", "09", "05", "pps.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if got := len(lines) - 1; got != pulses {
		t.Fatalf("%d rows for %d accepted pulses:\n%s", got, pulses, b)
	}
	// Each row must carry its own timestamp, offset and sequence — not a
	// mixture from different events.
	for i, line := range lines[1:] {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			t.Fatalf("row %d has %d fields: %q", i, len(fields), line)
		}
		wantAt := base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		if fields[0] != wantAt {
			t.Fatalf("row %d time %q, want %q", i, fields[0], wantAt)
		}
		if wantSeq := strconv.Itoa(100 + i); fields[3] != wantSeq {
			t.Fatalf("row %d sequence %q, want %q", i, fields[3], wantSeq)
		}
	}
}

// TestAstra6PulseLossIsCounted checks the other half: a pulse that cannot be
// queued is counted rather than silently lost, so zero loss is never inferred
// from an empty snapshot-drop counter.
func TestAstra6PulseLossIsCounted(t *testing.T) {
	r := New(Config{Dir: t.TempDir()})
	defer r.closeFiles()
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	// Overfill the queue without ever running the writer.
	for i := 0; i < pulseQueueSize+50; i++ {
		r.Pulse(source.Pulse{Source: "pps0", At: base.Add(time.Duration(i) * time.Second)})
	}
	if got := r.pulsesDropped.Load(); got != 50 {
		t.Fatalf("dropped count %d, want 50", got)
	}
	if got := r.dropped.Load(); got != 0 {
		t.Fatalf("pulse drops were counted as snapshot drops: %d", got)
	}
}

// TestAstra6PulseQueueNeverBlocks checks the refclock's goroutine is never
// held up, which is what keeps disk I/O out of the clock-discipline path.
func TestAstra6PulseQueueNeverBlocks(t *testing.T) {
	r := New(Config{Dir: t.TempDir()})
	defer r.closeFiles()
	done := make(chan struct{})
	go func() {
		defer close(done)
		base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
		for i := 0; i < 10*pulseQueueSize; i++ {
			r.Pulse(source.Pulse{Source: "pps0", At: base.Add(time.Duration(i) * time.Second)})
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Pulse blocked the caller")
	}
}

// TestAstra6EventsReconstructAnOutage covers RA6X-047. loop.tsv and
// sources.tsv are sampled per loop update, so a synchronized → loss →
// holdover → unsynchronized → recovery sequence with no intervening update
// left no record of any of it. events.tsv is a separate stream that records
// the transitions without redefining what Updates means.
func TestAstra6EventsReconstructAnOutage(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir})
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }

	snap := func(sec int, state discipline.State, sysSource string, reach uint8, status discipline.SelectStatus) *engine.Status {
		return &engine.Status{
			Status: discipline.Status{
				State: state, SystemSource: sysSource, Updates: 1,
				Sources: []discipline.SourceStatus{{Name: "a", Status: status, Reach: reach, Poll: 6}},
			},
			Now:   at(sec),
			Infos: map[string]source.Info{},
		}
	}
	// Synchronized, then the source is lost, holdover, expiry, recovery —
	// with Updates never changing, so nothing else records any of it.
	r.Record(snap(0, discipline.StateSynced, "a", 0xff, discipline.StatusSystem))
	r.Record(snap(10, discipline.StateSynced, "a", 0, discipline.StatusUnreachable))
	r.Record(snap(20, discipline.StateHoldover, "", 0, discipline.StatusUnreachable))
	r.Record(snap(30, discipline.StateUnsynced, "", 0, discipline.StatusUnreachable))
	r.Record(snap(40, discipline.StateSynced, "a", 0xff, discipline.StatusSystem))
	drain(t, r)

	b, err := os.ReadFile(filepath.Join(dir, "2026", "09", "05", "events.tsv"))
	if err != nil {
		t.Fatalf("no event stream: %v", err)
	}
	text := string(b)
	for _, want := range []string{
		"state\t\tsynced\tholdover",
		"state\t\tholdover\tunsynced",
		"state\t\tunsynced\tsynced",
		"source\ta\tsystem/reachable\tunreachable/unreachable",
		"system_source\t\ta\t",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("events.tsv is missing %q:\n%s", want, text)
		}
	}
	// The existing files must not have been redefined: with Updates
	// unchanged, loop.tsv holds one row from the first snapshot only.
	loop, err := os.ReadFile(filepath.Join(dir, "2026", "09", "05", "loop.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(string(loop)), "\n") + 1; lines != 2 {
		t.Fatalf("loop.tsv has %d lines; the per-update contract must be unchanged:\n%s", lines, loop)
	}
}

// TestAstra6EventVolumeIsBounded checks a flapping source cannot fill the
// disk: one row per subject per second at most.
func TestAstra6EventVolumeIsBounded(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir})
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	// Fewer than the snapshot queue holds, so this measures the event rate
	// bound rather than the queue's own drop policy. 200 snapshots 100 ms
	// apart is 100 transitions across 20 s.
	const snapshots = 200
	for i := 0; i < snapshots; i++ {
		reach := uint8(0xff)
		status := discipline.StatusSystem
		if i%2 == 1 {
			reach, status = 0, discipline.StatusUnreachable
		}
		r.Record(&engine.Status{
			Status: discipline.Status{
				State: discipline.StateSynced, SystemSource: "a", Updates: 1,
				Sources: []discipline.SourceStatus{{Name: "a", Status: status, Reach: reach, Poll: 6}},
			},
			Now:   base.Add(time.Duration(i) * 100 * time.Millisecond),
			Infos: map[string]source.Info{},
		})
	}
	drain(t, r)
	b, err := os.ReadFile(filepath.Join(dir, "2026", "09", "05", "events.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Count(strings.TrimSpace(string(b)), "\n") // the header is one line, so newlines = rows
	if rows > 30 {
		t.Fatalf("%d rows for %d flaps across 20 s; the one-per-second bound is not holding", rows, snapshots/2)
	}
	if rows < 10 {
		t.Fatalf("%d rows; the flapping was not recorded at all", rows)
	}
}

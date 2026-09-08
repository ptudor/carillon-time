package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ptudor/carillon-time/internal/clock"
	"github.com/ptudor/carillon-time/internal/discipline"
	"github.com/ptudor/carillon-time/internal/leap"
	"github.com/ptudor/carillon-time/internal/ntp"
)

func leapObject(t *testing.T, updated, expiry time.Time, kind ntp.Leap) *leap.Object {
	t.Helper()
	const epoch = 2208988800
	body := fmt.Sprintf("#$ %d\n#@ %d\n2272060800 10\n2287785600 11\n", updated.Unix()+epoch, expiry.Unix()+epoch)
	if kind == ntp.LeapInsert || kind == ntp.LeapDelete {
		offset := 12
		if kind == ntp.LeapDelete {
			offset = 10
		}
		boundary := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
		body += fmt.Sprintf("%d %d\n", boundary.Unix()+epoch, offset)
	}
	o, err := leap.NewObject([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func gpsEngine(t *testing.T, start time.Time, state leap.State) (*Engine, *clock.Fake, *scripted, *scripted) {
	t.Helper()
	clk := clock.NewFake(start)
	gps := &scripted{name: "gps/nmea", clk: clk}
	pps := &scripted{name: "gps/pps", clk: clk}
	cfg := testConfig("", SourceSpec{Source: gps, Options: discipline.Options{Numbering: true, LeapIncapable: true}}, SourceSpec{Source: pps, Options: discipline.Options{PPS: true, Prefer: true}})
	cfg.LeapState = state
	cfg.LeapRequired = true
	e, err := New(cfg, clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	return e, clk, gps, pps
}

func feedGPS(t *testing.T, e *Engine, clk *clock.Fake, elapsed time.Duration) {
	t.Helper()
	clk.Advance(elapsed)
	for _, name := range []string{"gps/nmea", "gps/pps"} {
		m := good(0)
		m.Source = name
		m.Now = clk.Monotonic()
		m.At = m.Now
		m.Stratum = 0
		m.Poll = 2
		m.SourceRefID = ntp.RefIDFromString("GPS")
		if name == "gps/pps" {
			m.Delay = 0
			m.Dispersion = 1e-6
			m.Jitter = 1e-6
		}
		// Same ordering as Run: reset/discard old epochs before Update.
		e.crossLeap(m.Now)
		if err := e.handle(e.sys.Update(m), m.Now); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDisconnectedGPSPPSPositiveAndNegativeLeap(t *testing.T) {
	boundary := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for _, kind := range []ntp.Leap{ntp.LeapInsert, ntp.LeapDelete} {
		t.Run(kind.String(), func(t *testing.T) {
			o := leapObject(t, boundary.AddDate(0, -1, 0), boundary.AddDate(0, 6, 0), kind)
			r := &leap.Record{Object: o, Provider: leap.Provider{Kind: "file", Name: "offline"}, Accepted: boundary.Add(-time.Hour)}
			e, clk, gps, pps := gpsEngine(t, boundary.Add(-6*time.Second), leap.State{Active: r, UTCbound: r.Accepted})
			for range 4 {
				feedGPS(t, e, clk, time.Second)
			}
			if st := e.Status(); st.State != discipline.StateSynced || !st.LeapReady || !st.PPSQualified || st.Stratum != 1 || st.Leap != kind {
				t.Fatalf("not ready before isolated leap: %+v", st)
			}
			if ks := clk.Status(); !ks.Synced || ks.Leap != kind {
				t.Fatalf("kernel not armed: %+v", ks)
			}
			// Move to the final second, then model only the kernel's leap.
			// SetLocalOffset changes fake wall time without recording a daemon
			// Step. TrueTime here counts elapsed SI seconds continuously.
			if kind == ntp.LeapInsert {
				feedGPS(t, e, clk, time.Second)
			}
			clk.Advance(time.Second)
			delta := time.Second
			if kind == ntp.LeapDelete {
				delta = -time.Second
			}
			clk.SetLocalOffset(delta)
			e.crossLeap(clk.Monotonic())
			if err := e.handle(e.sys.Tick(clk.Monotonic()), clk.Monotonic()); err != nil {
				t.Fatal(err)
			}
			if gps.resets.Load() != 1 || pps.resets.Load() != 1 {
				t.Fatalf("boundary not reset once: GPS=%d PPS=%d", gps.resets.Load(), pps.resets.Load())
			}
			if !e.Status().LeapReady || !serverSynced(e.Status()) || !clk.Status().Synced || clk.Status().Leap != ntp.LeapNone {
				t.Fatalf("service interrupted at event: %+v", e.Status())
			}
			for range 5 {
				feedGPS(t, e, clk, time.Second)
				if !serverSynced(e.Status()) || e.Status().Leap != ntp.LeapNone {
					t.Fatalf("post-event service: %+v", e.Status())
				}
			}
			if gps.resets.Load() != 1 || pps.resets.Load() != 1 || len(clk.Steps) != 0 {
				t.Fatal("event executed twice or induced a daemon step")
			}
			if !clk.Now().Equal(clk.TrueTime().Add(-delta)) {
				t.Fatalf("wrong post-event UTC: %s", clk.Now())
			}
			if !e.Status().LeapExecuted.Equal(boundary) {
				t.Fatal("execution record missing")
			}
		})
	}
}

func TestRequiredTableMissingExpiredAndClockSetback(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	e, clk, _, _ := gpsEngine(t, now, leap.State{})
	for range 4 {
		feedGPS(t, e, clk, time.Second)
	}
	if st := e.Status(); st.ClockState != discipline.StateSynced || !st.PPSQualified || st.State != discipline.StateUnsynced || st.Leap != ntp.LeapUnsync || st.RefID != leapMissingRefID || clk.Status().Synced {
		t.Fatalf("PPS without leap data claimed service: %+v", st)
	}
	o := leapObject(t, now.Add(-24*time.Hour), now.Add(10*time.Second), ntp.LeapNone)
	r := &leap.Record{Object: o, Provider: leap.Provider{Kind: "file", Name: "local"}, Accepted: now}
	e.activeLeap = r
	e.leapAnchor = o
	feedGPS(t, e, clk, time.Second)
	if !e.Status().LeapReady {
		t.Fatal("current durable table not used")
	}
	feedGPS(t, e, clk, 10*time.Second)
	if st := e.Status(); st.LeapReady || st.LeapReason != "expired" || st.LeapObject != nil || clk.Status().Synced {
		t.Fatalf("expired table still authoritative: %+v", st)
	}
	bound := e.Status().UTCbound
	clk.SetLocalOffset(10 * time.Second)
	feedGPS(t, e, clk, 0)
	if e.Status().LeapReady || e.CurrentLeap() != nil {
		t.Fatal("clock setback revived expired table")
	}
	state := leap.State{Active: r, UTCbound: bound}
	restarted, restartClock, _, _ := gpsEngine(t, now, state)
	for range 4 {
		feedGPS(t, restarted, restartClock, time.Second)
	}
	if restarted.Status().LeapReady {
		t.Fatal("startup behind UTC bound revived expired cache")
	}
}

// Drive the real bounded request queue without starting sources, keeping
// clock tests deterministic while exercising both controller methods.
func requestLeap(t *testing.T, e *Engine, r *leap.Record, activate bool) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		if activate {
			done <- e.ActivateLeap(ctx, r)
		} else {
			done <- e.ApproveLeap(ctx, r)
		}
	}()
	select {
	case f := <-e.reqs:
		if err := f(); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("request was not queued")
	}
	return <-done
}

func TestLeapActivationArmedConflictAndExpiryRace(t *testing.T) {
	boundary := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	old := leapObject(t, boundary.AddDate(0, -1, 0), boundary.AddDate(0, 3, 0), ntp.LeapInsert)
	r := &leap.Record{Object: old, Provider: leap.Provider{Kind: "file", Name: "local"}, Accepted: boundary.Add(-time.Hour)}
	e, clk, _, _ := gpsEngine(t, boundary.Add(-time.Minute), leap.State{Active: r, UTCbound: r.Accepted})
	for range 4 {
		feedGPS(t, e, clk, time.Second)
	}
	changed := leapObject(t, boundary.Add(-time.Hour), boundary.AddDate(0, 6, 0), ntp.LeapDelete)
	candidate := &leap.Record{Object: changed, Provider: r.Provider, Accepted: clk.Now()}
	if err := requestLeap(t, e, candidate, false); leap.Reason(err) != "armed" {
		t.Fatalf("armed conflict accepted: %v", err)
	}
	renew := leapObject(t, old.Updated(), boundary.AddDate(0, 6, 0), ntp.LeapInsert)
	candidate = &leap.Record{Object: renew, Provider: r.Provider, Accepted: clk.Now()}
	if err := requestLeap(t, e, candidate, true); leap.Reason(err) != "generation" {
		t.Fatalf("unapproved activation: %v", err)
	}
	if err := requestLeap(t, e, candidate, false); err != nil {
		t.Fatal(err)
	}
	if err := requestLeap(t, e, candidate, true); err != nil {
		t.Fatal(err)
	}
	if e.activeLeap.Object.Manifest() != renew.Manifest() || e.Status().Leap != ntp.LeapInsert {
		t.Fatal("consistent renewal did not preserve warning")
	}
	// The engine checks UTC again after approval, independently of the
	// worker. Expiry between durable commit and callback cannot activate.
	late := leapObject(t, renew.Updated(), renew.Expiry().Add(time.Hour), ntp.LeapInsert)
	candidate = &leap.Record{Object: late, Provider: r.Provider, Accepted: clk.Now()}
	if err := requestLeap(t, e, candidate, false); err != nil {
		t.Fatal(err)
	}
	clk.Advance(late.Expiry().Sub(clk.Now()))
	// Establish a new GPS estimate; the loop is separate from leap readiness.
	for range 4 {
		feedGPS(t, e, clk, time.Second)
	}
	if err := requestLeap(t, e, candidate, true); leap.Reason(err) != "expired" {
		t.Fatalf("expired activation accepted: %v", err)
	}
	if e.activeLeap.Object.Manifest() != renew.Manifest() {
		t.Fatal("rejected activation replaced active object")
	}
}

func TestLateTableDoesNotExecutePastEvent(t *testing.T) {
	boundary := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	e, clk, gps, pps := gpsEngine(t, boundary.Add(time.Hour), leap.State{})
	for range 4 {
		feedGPS(t, e, clk, time.Second)
	}
	o := leapObject(t, boundary.Add(-time.Hour), boundary.AddDate(0, 6, 0), ntp.LeapInsert)
	r := &leap.Record{Object: o, Provider: leap.Provider{Kind: "peer", Name: "home", KeyID: 1}, Accepted: clk.Now()}
	if err := requestLeap(t, e, r, false); err != nil {
		t.Fatal(err)
	}
	if err := requestLeap(t, e, r, true); err != nil {
		t.Fatal(err)
	}
	if gps.resets.Load() != 0 || pps.resets.Load() != 0 || len(clk.Steps) != 0 || !e.pendingLeap.IsZero() {
		t.Fatal("late learned event executed retroactively")
	}
	if !e.Status().LeapReady || e.Status().Leap != ntp.LeapNone {
		t.Fatal("past event did not remain history")
	}
}

func TestWorkerActivatesTableIntoRunningGPSClock(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	src := &scripted{name: "gps", clk: clk, gap: time.Millisecond, script: []discipline.Measurement{good(0), good(0), good(0)}}
	cfg := testConfig("", SourceSpec{Source: src, Options: discipline.Options{Numbering: true, LeapIncapable: true}})
	cfg.LeapRequired = true
	cfg.LeapReport = new(leap.Report)
	e, err := New(cfg, clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	o := leapObject(t, now.Add(-time.Hour), now.AddDate(0, 6, 0), ntp.LeapNone)
	dir := t.TempDir()
	manual := filepath.Join(dir, "manual.list")
	if err := os.WriteFile(manual, o.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := leap.OpenStore(filepath.Join(dir, "leap"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	u := leap.NewUpdater(leap.UpdaterConfig{Mode: "manual", ManualPath: manual, Store: store, Controller: e, Report: cfg.LeapReport})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	workerDone := make(chan struct{})
	go func() { done <- e.Run(ctx) }()
	go func() { u.Run(ctx); close(workerDone) }()
	if err := e.Wait(ctx, func(s *Status) bool { return s.LeapReady && s.State == discipline.StateSynced }); err != nil {
		t.Fatalf("runtime activation: %v; %+v", err, e.Status())
	}
	cancel()
	<-workerDone
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if e.Status().LeapHash != o.Manifest().Hash() || !clk.Status().Synced {
		t.Fatal("durable activation did not reach effective service and kernel")
	}
	disk, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if disk.Anchor() == nil || disk.Anchor().Manifest() != o.Manifest() {
		t.Fatal("running engine received an uncommitted object")
	}
	if b, err := os.ReadFile(manual); err != nil || string(b) != string(o.Bytes()) {
		t.Fatal("operator file was modified")
	}
}

func TestExpiredTableFallsBackOnlyToFreshNetworkEvidence(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	src := &scripted{name: "upstream", clk: clk}
	cfg := testConfig("", SourceSpec{Source: src, Options: discipline.Options{Numbering: true}})
	o := leapObject(t, now.AddDate(0, -1, 0), now.Add(-24*time.Hour), ntp.LeapNone)
	r := &leap.Record{Object: o, Provider: leap.Provider{Kind: "file", Name: "expired"}, Accepted: now.Add(-48 * time.Hour)}
	cfg.LeapState = leap.State{Active: r, UTCbound: r.Accepted}
	e, err := New(cfg, clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		clk.Advance(time.Second)
		m := good(0)
		m.Source = src.name
		m.Leap = ntp.LeapInsert
		m.Now = clk.Monotonic()
		m.At = m.Now
		if err := e.handle(e.sys.Update(m), m.Now); err != nil {
			t.Fatal(err)
		}
	}
	if st := e.Status(); !st.LeapReady || st.LeapSource != "sources" || st.Leap != ntp.LeapInsert || st.LeapObject != nil {
		t.Fatalf("expired file suppressed survivor warning: %+v", st)
	}
	clk.Advance(129 * time.Second)
	if err := e.handle(e.sys.Tick(clk.Monotonic()), clk.Monotonic()); err != nil {
		t.Fatal(err)
	}
	if st := e.Status(); st.LeapReady || st.Leap != ntp.LeapUnsync || clk.Status().Synced {
		t.Fatalf("stale LI still authoritative: %+v", st)
	}
}

func TestSettlingUTCMayExportWithoutArmingKernel(t *testing.T) {
	boundary := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(boundary.Add(-time.Hour))
	src := &scripted{name: "gps", clk: clk}
	cfg := testConfig("", SourceSpec{Source: src, Options: discipline.Options{Numbering: true, LeapIncapable: true}})
	cfg.Discipline.SettleUpdates = 10
	cfg.LeapRequired = true
	o := leapObject(t, boundary.AddDate(0, -1, 0), boundary.AddDate(0, 6, 0), ntp.LeapInsert)
	r := &leap.Record{Object: o, Provider: leap.Provider{Kind: "file", Name: "cache"}, Accepted: clk.Now().Add(-time.Hour)}
	cfg.LeapState = leap.State{Active: r, UTCbound: r.Accepted}
	e, err := New(cfg, clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	m := good(0)
	m.Source = src.name
	m.Now = clk.Monotonic()
	m.At = m.Now
	if err := e.handle(e.sys.Update(m), m.Now); err != nil {
		t.Fatal(err)
	}
	if st := e.Status(); !st.TimeKnown || st.State != discipline.StateSettling || e.CurrentLeap() == nil {
		t.Fatalf("coarse UTC did not permit independent distribution: %+v", st)
	}
	if !e.pendingLeap.IsZero() || clk.Status().Synced || clk.Status().Leap != ntp.LeapUnsync {
		t.Fatal("settling clock armed a kernel leap")
	}
}

package control

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"carillon/internal/clock"
	"carillon/internal/discipline"
	"carillon/internal/engine"
	"carillon/internal/source"
)

func socketPath(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "s")
	if len(p) > 100 {
		t.Skipf("temp dir path too long for a unix socket: %s", p)
	}
	return p
}

func newEngine(t *testing.T) *engine.Engine {
	clk := clock.NewFake(time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	cfg := engine.Config{
		Discipline: discipline.Config{
			Loop:         discipline.LoopConfig{StepThreshold: 0.5, StepLimit: 3, Panic: 1000, MaxSlewPPM: 500, Precision: 1e-6},
			MinSurvivors: 1, HoldoverMax: 3600, SettleUpdates: 3,
		},
		Version: "v-test",
	}
	e, err := engine.New(cfg, clk, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestServerRoundTrip(t *testing.T) {
	path := socketPath(t)
	eng := newEngine(t)
	srv, err := Listen(path, eng, nil, "v-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o660 {
		t.Fatalf("socket mode: %v %v", fi, err)
	}

	resp, err := Call(ctx, path, Request{Command: CmdVersion})
	if err != nil || resp.Version != "v-test" {
		t.Fatalf("version: %+v %v", resp, err)
	}
	resp, err = Call(ctx, path, Request{Command: CmdTracking})
	if err != nil || resp.Tracking == nil {
		t.Fatalf("tracking: %+v %v", resp, err)
	}
	if resp.Tracking.State != "unsynced" || resp.Tracking.Stratum != 16 || resp.Tracking.RefID != "INIT" {
		t.Fatalf("tracking: %+v", resp.Tracking)
	}
	resp, err = Call(ctx, path, Request{Command: CmdSources})
	if err != nil || len(resp.Sources) != 0 {
		t.Fatalf("sources: %+v %v", resp, err)
	}
	resp, err = Call(ctx, path, Request{Command: CmdRefclock})
	if err != nil || len(resp.Refclocks) != 0 {
		t.Fatalf("refclock: %+v %v", resp, err)
	}
	resp, err = Call(ctx, path, Request{Command: CmdServerStats})
	if err != nil || resp.ServerStats == nil || *resp.ServerStats != (ServerStats{}) {
		t.Fatalf("serverstats: %+v %v", resp, err)
	}
	resp, err = Call(ctx, path, Request{Command: CmdWaitSync, Timeout: 0.3})
	if err != nil || resp.Synced == nil || *resp.Synced {
		t.Fatalf("waitsync on an unsynced engine must report false: %+v %v", resp, err)
	}
	if _, err = Call(ctx, path, Request{Command: "bogus"}); err == nil {
		t.Fatal("unknown command must error")
	}

	// A second daemon must not be able to take the socket while it is live.
	if _, err := Listen(path, eng, nil, "v-test", nil); err == nil {
		t.Fatal("live socket must be refused")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("socket file must be removed on shutdown")
	}
}

// TestWaitSyncFollowsRestart checks that WaitSync keeps trying while the
// daemon is not yet listening, connects once it is, and reports the last
// connection error when it never appears.
func TestWaitSyncFollowsRestart(t *testing.T) {
	path := socketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// No daemon at all: retry until the deadline, then surface why.
	start := time.Now()
	synced, err := WaitSync(ctx, path, 600*time.Millisecond)
	if synced || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("WaitSync without a daemon = %v, %v; want false, not-exist", synced, err)
	}
	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Fatalf("WaitSync gave up after %v, before its %v deadline", elapsed, 600*time.Millisecond)
	}

	// The daemon appears while WaitSync is already waiting: it must connect
	// and get the daemon's verdict (an unsynced engine reports false)
	// instead of a connection error.
	type result struct {
		synced bool
		err    error
	}
	done := make(chan result, 1)
	go func() {
		s, e := WaitSync(ctx, path, 3*time.Second)
		done <- result{s, e}
	}()
	time.Sleep(2 * connectRetry)
	srv, err := Listen(path, newEngine(t), nil, "v-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ctx) }()

	select {
	case r := <-done:
		if r.synced || r.err != nil {
			t.Fatalf("WaitSync after daemon start = %v, %v; want false, nil", r.synced, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("WaitSync did not return")
	}
	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("serve: %v", err)
	}
}

func TestListenRemovesStaleSocket(t *testing.T) {
	path := socketPath(t)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := Listen(path, newEngine(t), nil, "v", nil)
	if err != nil {
		t.Fatalf("stale socket must be replaced: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = srv.Serve(ctx)
}

func TestConversions(t *testing.T) {
	eng := newEngine(t)
	st := eng.Status()
	tr := TrackingOf(st)
	if tr.Version != "v-test" || tr.Leap != "unsynchronized" || tr.Uptime < 0 {
		t.Fatalf("%+v", tr)
	}
	if ReachOctal(0xff) != "377" || ReachOctal(1) != "001" {
		t.Fatal("reach octal")
	}
}

func TestRefclockConversion(t *testing.T) {
	st := &engine.Status{
		Status: discipline.Status{
			PPSQualified: true,
			Sources:      []discipline.SourceStatus{{Name: "pps0", Status: discipline.StatusSystem, Reach: 0xff, Poll: 4}},
		},
		Infos: map[string]source.Info{
			"pps0": {
				Name: "pps0",
				Refclock: &source.RefclockInfo{
					Type: "pps", Device: "/dev/pps0", Edge: "assert",
					Stable: true, Sequence: 42, WindowSamples: 16, WindowJitter: 2e-6,
				},
			},
		},
	}
	rs := RefclocksOf(st)
	if len(rs) != 1 || !rs[0].Qualified || !rs[0].Locked || rs[0].Sequence != 42 {
		t.Fatalf("refclocks: %+v", rs)
	}
}

package control

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/carillon-time/internal/clock"
	"github.com/ptudor/carillon-time/internal/discipline"
	"github.com/ptudor/carillon-time/internal/engine"
	"github.com/ptudor/carillon-time/internal/source"
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
	if err != nil || resp.ServerStats == nil || resp.ServerStats.Requests() != 0 {
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

// TestListenRemovesStaleSocket uses a real abandoned unix socket, the only
// thing Listen is entitled to remove. It used to plant a regular file, which
// encoded the assumption RA6X-032 is about: that anything at the control path
// which does not answer is disposable.
func TestListenRemovesStaleSocket(t *testing.T) {
	path := socketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	// Close the listener without unlinking, exactly as a SIGKILLed daemon
	// leaves it: the inode stays, and connecting to it is refused.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode().Type() != fs.ModeSocket {
		t.Fatalf("setup: %v %v", info, err)
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
			Sources: []discipline.SourceStatus{
				{Name: "pps0", Status: discipline.StatusSystem, Reach: 0xff, Poll: 4},
				{Name: "gps/nmea", Status: discipline.StatusSurvivor, Reach: 0x0f, Poll: 4},
			},
		},
		Infos: map[string]source.Info{
			"pps0": {
				Name: "pps0",
				Refclock: &source.RefclockInfo{
					Type: "pps", Device: "/dev/pps0", Edge: "assert",
					Stable: true, Sequence: 42, WindowSamples: 16, WindowJitter: 2e-6,
				},
			},
			"gps/nmea": {
				Name: "gps/nmea",
				Refclock: &source.RefclockInfo{
					Type: "gps-nmea", Device: "/dev/ttyS0", Stable: true,
					FixKnown: true, FixValid: true, Satellites: 9, MeasuredLag: 0.15, LagSamples: 8,
				},
			},
		},
	}
	rs := RefclocksOf(st)
	if len(rs) != 2 || !rs[0].Qualified || !rs[0].Locked || rs[0].Sequence != 42 {
		t.Fatalf("refclocks: %+v", rs)
	}
	if !rs[1].Qualified || !rs[1].Locked || !rs[1].FixValid || rs[1].Satellites != 9 || rs[1].LagSamples != 8 {
		t.Fatalf("NMEA refclock: %+v", rs[1])
	}
}

// TestFreshEngineOmitsNeverHappenedTimestamps covers RF5X-015.
// encoding/json never omits a zero struct, so `omitempty` on a time.Time did
// nothing: a source that had never received a reply serialised
// "last_rx":"0001-01-01T00:00:00Z", which a client written against the
// documented "absent" convention parses as a real instant in year 1.
func TestFreshEngineOmitsNeverHappenedTimestamps(t *testing.T) {
	eng := newEngine(t)
	st := eng.Status()
	payload := struct {
		Tracking  *Tracking  `json:"tracking"`
		Sources   []Source   `json:"sources"`
		Refclocks []Refclock `json:"refclocks"`
	}{TrackingOf(st), SourcesOf(st), RefclocksOf(st)}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	tracking, ok := decoded["tracking"].(map[string]any)
	if !ok {
		t.Fatalf("tracking missing from %s", raw)
	}
	for _, key := range []string{"reftime", "leapfile_expires"} {
		if v, present := tracking[key]; present {
			t.Errorf("%q present on a fresh engine as %v; it has not happened yet", key, v)
		}
	}
	if s := string(raw); strings.Contains(s, "0001-01-01") {
		t.Fatalf("a year-1 timestamp reached the wire: %s", s)
	}
}

// TestShutdownUnreadWaitsync covers RF5X-033. A waitsync with no
// timeout clears the connection's deadline, so a client that connected, asked
// for one, and then stopped reading used to hold the handler — and through
// it Serve, and through that the daemon's shutdown — open indefinitely.
func TestShutdownUnreadWaitsync(t *testing.T) {
	path := socketPath(t)
	eng := newEngine(t) // no sources, so it never reaches SYNCED
	srv, err := Listen(path, eng, nil, "v-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	conn, err := net.Dial("unix", path)
	if err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(`{"command":"waitsync","timeout":0}` + "\n")); err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	// The accept loop is sequential, so a second request that completes
	// proves the first connection was already accepted and handed to a
	// handler that is now blocked in Wait.
	if resp, err := Call(ctx, path, Request{Command: CmdVersion}); err != nil || resp.Version != "v-test" {
		cancel()
		<-done
		t.Fatalf("second request: %v %+v", err, resp)
	}

	// The client never reads. Shutdown must still complete.
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Serve did not return within 6 s with an unread waitsync connection open")
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("shutdown took %v", elapsed)
	}
}

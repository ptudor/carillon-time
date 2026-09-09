package engine

import (
	"context"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/ptudor/carillon-time/internal/clock"
	"github.com/ptudor/carillon-time/internal/discipline"
	"github.com/ptudor/carillon-time/internal/leap"
	"github.com/ptudor/carillon-time/internal/ntp"
	"github.com/ptudor/carillon-time/internal/ntp/auth"
	"github.com/ptudor/carillon-time/internal/server"
)

func TestUDPRelayActivatesDurableTableInRunningEngine(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	o := leapObject(t, now.Add(-time.Hour), now.AddDate(0, 6, 0), ntp.LeapNone)
	r := &leap.Record{Object: o, Provider: leap.Provider{Kind: "peer", Name: "home", KeyID: 9}, Accepted: now}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := func(name string, initial leap.State) (*Engine, *leap.Store, *leap.Report) {
		t.Helper()
		clk := clock.NewFake(now)
		src := &scripted{name: name + "/gps", clk: clk, gap: time.Millisecond, script: []discipline.Measurement{good(0), good(0), good(0)}}
		cfg := testConfig("", SourceSpec{Source: src, Options: discipline.Options{Numbering: true, LeapIncapable: true}})
		cfg.LeapRequired, cfg.LeapState, cfg.LeapReport = true, initial, new(leap.Report)
		store, err := leap.OpenStore(filepath.Join(t.TempDir(), "leap"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		if err := store.Save(initial); err != nil {
			t.Fatal(err)
		}
		e, err := New(cfg, clk, quietLog())
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- e.Run(ctx) }()
		t.Cleanup(func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
		if err := e.Wait(ctx, func(s *Status) bool { return s.ClockState == discipline.StateSynced && s.Updates >= 3 }); err != nil {
			t.Fatal(err)
		}
		return e, store, cfg.LeapReport
	}
	relay, _, _ := start("relay", leap.State{Active: r, UTCbound: now})
	learner, store, report := start("learner", leap.State{})
	before := learner.Status()
	if before.State != discipline.StateUnsynced || before.LeapReady {
		t.Fatal("empty learner cache did not withhold service")
	}
	key := auth.Key{ID: 1, Secret: []byte("0123456789abcdef")}
	stats := new(server.Stats)
	srv, err := server.Listen(server.ServiceConfig{
		Listen: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:0")},
		Handler: server.Config{
			Allow:      []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
			RequireKey: map[netip.Prefix]uint32{netip.MustParsePrefix("127.0.0.0/8"): key.ID},
			Keys:       auth.Keys{key.ID: key}, RateLimitPPS: 8, RateBurst: 16, KoD: true,
			Leap: leap.NewDistributor([]uint32{key.ID}, relay.CurrentLeap, nil), Stats: stats,
			Now: relay.clk.Now,
			Status: func() server.SystemStatus {
				st := relay.Status()
				return server.SystemStatus{Synced: serverSynced(st), Leap: st.Leap, Stratum: st.Stratum, Precision: st.Precision, ReferenceID: st.RefID}
			},
		},
		Log: quietLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Error(err)
		}
	})
	u := leap.NewUpdater(leap.UpdaterConfig{Mode: "peers", Peers: []leap.Peer{{Name: "relay", Address: srv.Addrs()[0].String(), Key: key}}, Store: store, Controller: learner, Report: report})
	worker := make(chan struct{})
	go func() { u.Run(ctx); close(worker) }()
	t.Cleanup(func() { cancel(); <-worker })
	if err := learner.Wait(ctx, func(s *Status) bool {
		return s.State == discipline.StateSynced && s.LeapReady && s.LeapUpdate.Counts.Accepted == 1 && s.LeapUpdate.Pending == ""
	}); err != nil {
		t.Fatalf("peer activation: %v; %+v", err, learner.Status())
	}
	st := learner.Status()
	if st.LeapHash != o.Manifest().Hash() || st.LeapProvider.Kind != "peer" || st.LeapProvider.Name != "relay" || st.LeapProvider.KeyID != key.ID {
		t.Fatalf("wrong object or immediate provenance: %+v", st)
	}
	if st.Updates != before.Updates || len(st.Sources) != 1 || st.Sources[0].Updated != before.Sources[0].Updated || st.Sources[0].Reach != before.Sources[0].Reach {
		t.Fatal("CLPS traffic entered clock measurements or source reach")
	}
	if !learner.clk.(*clock.Fake).Status().Synced {
		t.Fatal("activation did not synchronize the learner kernel")
	}
	cancel()
	<-worker // activation compaction or the final checkpoint must finish first
	disk, err := store.Load()
	if err != nil || disk.Active == nil || disk.Active.Object.Manifest() != o.Manifest() || disk.Pending != nil {
		t.Fatalf("learner restart state: %+v %v", disk, err)
	}
	counts := report.Snapshot().Counts
	if counts.Probes != 1 || counts.Bytes != uint64(o.Manifest().Size) || counts.Accepted != 1 || stats.Snapshot().Total.BadAuth != 0 {
		t.Fatalf("transfer counters: %+v", counts)
	}
}

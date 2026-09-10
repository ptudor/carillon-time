package leap

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestNISTRestartRetainsSuccessfulCheckSchedule(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	u, c := testUpdater(t, "nist", now)
	o := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "")
	r := &Record{Object: o, Provider: Provider{Kind: "nist", Name: NISTURL}, Accepted: now.Add(-48 * time.Hour)}
	s := State{Active: r, UTCbound: now, LastCheck: now.Add(-time.Hour)}
	if err := u.cfg.Store.Save(s); err != nil {
		t.Fatal(err)
	}
	disk, err := u.cfg.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	c.active.Store(r)
	restarted := NewUpdater(UpdaterConfig{Mode: "nist", Store: u.cfg.Store, Initial: disk, Controller: c})
	var calls atomic.Int32
	restarted.fetchSeed = func(context.Context, Validators) (SeedResult, error) {
		calls.Add(1)
		return SeedResult{}, errors.New("egress denied")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	restarted.Run(ctx)
	if calls.Load() != 0 {
		t.Fatal("restart forced another NIST check despite valid recent check")
	}
	if c.active.Load().Object.manifest != o.manifest {
		t.Fatal("offline restart discarded cache")
	}
}

func TestUnknownUTCDoesNotFetchOrActivate(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	u, c := testUpdater(t, "nist", now)
	c.view.Store(&View{Now: now})
	var calls atomic.Int32
	u.fetchSeed = func(context.Context, Validators) (SeedResult, error) {
		calls.Add(1)
		return SeedResult{}, errors.New("must not fetch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	u.Run(ctx)
	if calls.Load() != 0 || c.active.Load() != nil {
		t.Fatal("unknown clock bypassed bootstrap")
	}
}

func TestGlobalDistributorBudget(t *testing.T) {
	keys := make([]uint32, 33)
	for i := range keys {
		keys[i] = uint32(i + 1)
	}
	d := NewDistributor(keys, nil, new(Counters))
	now := d.epoch
	req := &Message{Operation: Probe, ID: [16]byte{1}}
	for i, key := range keys {
		r, err := d.Respond(req, key, time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC), now)
		if i < 32 && (err != nil || r == nil) {
			t.Fatalf("burst %d: %v", i, err)
		}
		if i == 32 && Reason(err) != "rate" {
			t.Fatal("global request budget exceeded")
		}
	}
	if _, err := d.Respond(req, 33, time.Now(), now.Add(time.Second/32)); err != nil {
		t.Fatalf("global budget did not refill: %v", err)
	}
}

func TestCacheCorruptionAndSymlinkRefusal(t *testing.T) {
	u, _ := testUpdater(t, "manual", time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC))
	if err := u.cfg.Store.Save(State{}); err != nil {
		t.Fatal(err)
	}
	// A cache never follows a replacement state symlink, even when it
	// happens to point to another otherwise valid private cache file.
	if err := u.cfg.Store.root.Rename("state.json", "saved.json"); err != nil {
		t.Fatal(err)
	}
	if err := u.cfg.Store.root.Symlink("saved.json", "state.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := u.cfg.Store.Load(); err == nil {
		t.Fatal("state symlink followed")
	}
	if err := u.cfg.Store.root.Remove("state.json"); err != nil {
		t.Fatal(err)
	}
	if err := u.cfg.Store.root.WriteFile("state.json", []byte(`{"version":1,"unexpected":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := u.cfg.Store.Load(); err == nil {
		t.Fatal("corrupt state accepted")
	}
}

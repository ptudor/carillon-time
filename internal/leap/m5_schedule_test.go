package leap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type scheduledCalls[T any] struct {
	mu     sync.Mutex
	values []T
}

func (c *scheduledCalls[T]) add(v T) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values = append(c.values, v)
	return len(c.values)
}

func (c *scheduledCalls[T]) snapshot() []T {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.values)
}

func runScheduledUpdater(t *testing.T, u *Updater) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	synctest.Wait()
	return func() { cancel(); <-done }
}

func TestUpdaterPeerOrderAndTransientBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		u, _ := testUpdater(t, "peers", time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC))
		u.cfg.Peers = []Peer{{Name: "first"}, {Name: "second"}}
		type call struct {
			name string
			at   time.Time
		}
		var recorded scheduledCalls[call]
		u.fetchPeer = func(p Peer, _ context.Context, _ *Object, _ time.Time, _ *Counters, _ time.Duration) (*Object, error) {
			recorded.add(call{p.Name, time.Now()})
			return nil, Reject("timeout", "temporary packet loss")
		}
		stop := runScheduledUpdater(t, u)
		defer stop()
		calls := recorded.snapshot()
		if len(calls) != 1 || calls[0].name != "first" {
			t.Fatalf("initial peer order: %+v", calls)
		}
		time.Sleep(4 * time.Second)
		synctest.Wait()
		calls = recorded.snapshot()
		if len(calls) != 2 || calls[1].name != "second" {
			t.Fatalf("failover order: %+v", calls)
		}
		time.Sleep(14 * time.Minute)
		synctest.Wait()
		calls = recorded.snapshot()
		if len(calls) != 2 {
			t.Fatalf("retry ignored 15-minute backoff: %+v", calls)
		}
		time.Sleep(3 * time.Minute)
		synctest.Wait()
		calls = recorded.snapshot()
		if len(calls) != 4 {
			t.Fatalf("timeout parked a peer past normal backoff: %+v", calls)
		}
		for _, retry := range calls[2:] {
			var first time.Time
			for _, c := range calls[:2] {
				if c.name == retry.name {
					first = c.at
				}
			}
			if d := retry.at.Sub(first); d < 15*time.Minute || d > 17*time.Minute {
				t.Fatalf("retry interval %s", d)
			}
		}
	})
}

func TestUpdaterUnsupportedAndDenialSchedules(t *testing.T) {
	for _, reason := range []string{"unsupported", "DENY", "RSTR"} {
		t.Run(reason, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				u, _ := testUpdater(t, "peers", time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC))
				u.cfg.Peers = []Peer{{Name: "peer"}}
				var calls atomic.Int32
				u.fetchPeer = func(Peer, context.Context, *Object, time.Time, *Counters, time.Duration) (*Object, error) {
					calls.Add(1)
					return nil, &PeerError{Reason: reason, RetryAfter: 24 * time.Hour, Stop: reason != "unsupported"}
				}
				stop := runScheduledUpdater(t, u)
				defer stop()
				time.Sleep(24*time.Hour - time.Second)
				synctest.Wait()
				if calls.Load() != 1 {
					t.Fatalf("capability floor or stop ignored: %d calls", calls.Load())
				}
				time.Sleep(3 * time.Hour)
				synctest.Wait()
				want := int32(1)
				if reason == "unsupported" {
					want = 2
				}
				if calls.Load() != want {
					t.Fatalf("retry policy: %d calls, want %d", calls.Load(), want)
				}
			})
		})
	}
}

func TestUpdaterRATESpacingRecoversAfterSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		u, _ := testUpdater(t, "peers", time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC))
		u.cfg.Peers = []Peer{{Name: "peer"}}
		var recorded scheduledCalls[time.Duration]
		u.fetchPeer = func(_ Peer, _ context.Context, _ *Object, _ time.Time, _ *Counters, spacing time.Duration) (*Object, error) {
			if recorded.add(spacing) == 1 {
				return nil, &PeerError{Reason: "kiss RATE", RetryAfter: 64 * time.Second, MinInterval: 64 * time.Second}
			}
			return nil, nil
		}
		stop := runScheduledUpdater(t, u)
		defer stop()
		time.Sleep(17 * time.Minute)
		synctest.Wait()
		intervals := recorded.snapshot()
		if len(intervals) != 2 || intervals[1] != 64*time.Second {
			t.Fatalf("RATE was not retained for retry: %v", intervals)
		}
		time.Sleep(7 * time.Hour)
		synctest.Wait()
		intervals = recorded.snapshot()
		if len(intervals) != 3 || intervals[2] != 4*time.Second {
			t.Fatalf("successful fetch did not clear RATE: %v", intervals)
		}
	})
}

func TestUpdaterCacheFailureRetriesAfterFifteenMinutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
		u, c := testUpdater(t, "nist", now)
		old := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "")
		renew := objectForTest(t, "2016-07-08T00:00:00Z", "2027-06-28T00:00:00Z", "")
		if err := u.install(context.Background(), old, Provider{Kind: "file", Name: "local"}); err != nil {
			t.Fatal(err)
		}
		// Make the seed's startup check due despite the current active table.
		u.state.LastCheck = now.Add(-48 * time.Hour)
		u.fetchNIST = func(context.Context) (*Object, error) { return renew, nil }
		var failed atomic.Bool
		failed.Store(true)
		var attempts atomic.Int32
		u.cfg.Store.fault = func(stage string) error {
			if stage == "create" {
				attempts.Add(1)
				if failed.Load() {
					return errors.New("disk unavailable")
				}
			}
			return nil
		}
		stop := runScheduledUpdater(t, u)
		defer stop()
		if attempts.Load() != 1 || c.active.Load().Object != old {
			t.Fatal("failed commit activated or retried immediately")
		}
		time.Sleep(15*time.Minute - time.Second)
		synctest.Wait()
		if attempts.Load() != 1 {
			t.Fatal("cache failure retried before fifteen minutes")
		}
		failed.Store(false)
		time.Sleep(time.Second)
		synctest.Wait()
		if attempts.Load() < 2 || c.active.Load().Object != renew {
			t.Fatal("durable retry did not activate pending generation")
		}
	})
}

type retryController struct {
	*testController
	refuse atomic.Bool
}

func (c *retryController) ActivateLeap(ctx context.Context, r *Record) error {
	if c.refuse.Load() {
		return Reject("armed", "temporary activation refusal")
	}
	return c.testController.ActivateLeap(ctx, r)
}

func TestUpdaterPendingActivationRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
		u, c := testUpdater(t, "off", now)
		o := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "")
		u.state.Pending = &Record{Object: o, Provider: Provider{Kind: "file", Name: "local"}, Accepted: now}
		u.state.UTCbound = now
		if err := u.save(); err != nil {
			t.Fatal(err)
		}
		retrying := &retryController{testController: c}
		retrying.refuse.Store(true)
		u.cfg.Controller = retrying
		stop := runScheduledUpdater(t, u)
		defer stop()
		if u.cfg.Report.Counters.Failures.Load() != 1 || c.active.Load() != nil {
			t.Fatal("initial activation refusal lost")
		}
		time.Sleep(15*time.Minute - time.Second)
		synctest.Wait()
		if u.cfg.Report.Counters.Failures.Load() != 1 {
			t.Fatal("activation retried before fifteen minutes")
		}
		retrying.refuse.Store(false)
		time.Sleep(time.Second)
		synctest.Wait()
		if c.active.Load() == nil || c.active.Load().Object != o || u.cfg.Report.Snapshot().Pending != "" {
			t.Fatal("pending generation was not retried")
		}
	})
}

func TestManualReplacementConflictAndMissingFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		u, c := testUpdater(t, "manual", time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC))
		o := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "")
		u.cfg.ManualPath = filepath.Join(t.TempDir(), "manual.list")
		if err := os.WriteFile(u.cfg.ManualPath, o.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		stop := runScheduledUpdater(t, u)
		defer stop()
		if c.active.Load() == nil {
			t.Fatal("manual file not activated")
		}
		if err := os.WriteFile(u.cfg.ManualPath, append(o.Bytes(), []byte("# changed bytes\n")...), 0600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(66 * time.Minute)
		synctest.Wait()
		if u.cfg.Report.Snapshot().LastRejection != "conflict" || c.active.Load().Object.manifest != o.manifest {
			t.Fatal("same-date manual conflict replaced the active table")
		}
		if err := os.Remove(u.cfg.ManualPath); err != nil {
			t.Fatal(err)
		}
		time.Sleep(17 * time.Minute)
		synctest.Wait()
		if u.cfg.Report.Snapshot().LastRejection != "io" || c.active.Load().Object.manifest != o.manifest {
			t.Fatal("missing manual file withdrew the active table")
		}
	})
}

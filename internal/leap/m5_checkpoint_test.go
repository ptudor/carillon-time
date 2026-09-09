package leap

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestUTCCheckpointCadenceAndShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
		u, c := testUpdater(t, "off", now)
		o := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "")
		if err := u.install(context.Background(), o, Provider{Kind: "file", Name: "local"}); err != nil {
			t.Fatal(err)
		}
		var writes atomic.Int32
		u.cfg.Store.fault = func(stage string) error {
			if stage == "create" {
				writes.Add(1)
			}
			return nil
		}
		stop := runScheduledUpdater(t, u)
		defer stop()
		for minute := 1; minute <= 14; minute++ {
			utc := now.Add(time.Duration(minute) * time.Minute)
			c.view.Store(&View{Now: utc, Known: true, UTCbound: utc})
			time.Sleep(time.Second)
			synctest.Wait()
		}
		if writes.Load() != 0 {
			t.Fatalf("routine movement caused %d early full-state writes", writes.Load())
		}
		utc := now.Add(15 * time.Minute)
		c.view.Store(&View{Now: utc, Known: true, UTCbound: utc})
		time.Sleep(time.Second)
		synctest.Wait()
		if writes.Load() != 1 {
			t.Fatalf("fifteen-minute checkpoint: %d writes", writes.Load())
		}
		disk, err := u.cfg.Store.Load()
		if err != nil || !disk.UTCbound.Equal(utc) {
			t.Fatalf("periodic checkpoint: %+v %v", disk, err)
		}
		utc = utc.Add(20 * time.Second)
		c.view.Store(&View{Now: utc, Known: true, UTCbound: utc})
		stop() // no worker tick has seen this last bound yet
		disk, err = u.cfg.Store.Load()
		if err != nil || !disk.UTCbound.Equal(utc) || writes.Load() != 2 {
			t.Fatalf("final checkpoint lost progress: %+v %v", disk, err)
		}
		if disk.Active.Object.manifest != o.manifest {
			t.Fatal("checkpoint changed the active object")
		}
	})
}

func TestExpiryAndExecutionCheckpointImmediately(t *testing.T) {
	for _, event := range []string{"expiry", "execution"} {
		t.Run(event, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Date(2026, 6, 30, 23, 55, 0, 0, time.UTC)
				boundary := now.Add(5 * time.Minute)
				u, c := testUpdater(t, "off", now)
				expiry := "2026-12-28T00:00:00Z"
				if event == "expiry" {
					expiry = boundary.Format(time.RFC3339)
				}
				o := objectForTest(t, "2016-07-08T00:00:00Z", expiry, "")
				if err := u.install(context.Background(), o, Provider{Kind: "file", Name: "local"}); err != nil {
					t.Fatal(err)
				}
				stop := runScheduledUpdater(t, u)
				defer stop()
				view := &View{Now: boundary, Known: true, UTCbound: boundary}
				if event == "execution" {
					view.Executed = boundary
				}
				c.view.Store(view)
				time.Sleep(time.Second)
				synctest.Wait()
				disk, err := u.cfg.Store.Load()
				if err != nil || !disk.UTCbound.Equal(boundary) || !disk.Executed.Equal(view.Executed) {
					t.Fatalf("event waited for periodic checkpoint: %+v %v", disk, err)
				}
			})
		})
	}
}

func TestShutdownPersistsSeedScheduleAndReportsWriteFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "saved"
		if fail {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
			u, c := testUpdater(t, "nist", now)
			o := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "")
			if err := u.install(context.Background(), o, Provider{Kind: "nist", Name: NISTURL}); err != nil {
				t.Fatal(err)
			}
			checked := now.Add(time.Second)
			c.view.Store(&View{Now: checked, Known: true, UTCbound: checked})
			// Model cancellation with a successful check awaiting persistence.
			u.state.LastCheck, u.dirty = checked, true
			var log bytes.Buffer
			u.cfg.Log = slog.New(slog.NewTextHandler(&log, nil))
			if fail {
				u.cfg.Store.fault = func(string) error { return errors.New("disk offline") }
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			u.Run(ctx)
			disk, err := u.cfg.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				if !strings.Contains(log.String(), "leap shutdown checkpoint failed") || u.cfg.Report.Counters.CacheFailures.Load() != 1 || !disk.LastCheck.IsZero() {
					t.Fatalf("shutdown failure was lost: %s", log.String())
				}
			} else if !disk.LastCheck.Equal(checked) || !disk.UTCbound.Equal(checked) {
				t.Fatalf("shutdown lost seed check: %+v", disk)
			}
		})
	}
}

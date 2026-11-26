package leap

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestPrepareSeedSurvivesRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "leap")
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	utc := func() (time.Time, error) { return now, nil }
	o := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "")
	calls := 0
	fetch := func(context.Context) (SeedResult, error) {
		calls++
		return SeedResult{Object: o, Validators: Validators{ETag: "initial"}}, nil
	}
	seed, _ := SeedFor("iers")
	for range 2 {
		r, err := prepareSeed(t.Context(), store, seed, utc, fetch)
		if err != nil || r.Provider.Name != IERSURL || r.Object.Manifest() != o.Manifest() {
			t.Fatalf("prepared record %v, %v", r, err)
		}
	}
	if calls != 1 {
		t.Fatalf("idempotent preparation fetched %d times", calls)
	}
	disk, err := InspectStore(dir)
	if err != nil || disk.Active == nil || disk.Pending != nil || disk.Active.Object.Manifest() != o.Manifest() || !disk.LastCheck.Equal(now) || !disk.UTCbound.Equal(now) {
		t.Fatalf("startup state %+v, %v", disk, err)
	}
	if _, err := PrepareSeed(t.Context(), dir, "iers", "test", utc); err == nil {
		t.Fatal("concurrent store owner allowed")
	}
	if _, err := prepareSeed(t.Context(), store, Seed{Kind: "nist", URL: NISTURL}, utc, fetch); Reason(err) != "provider" {
		t.Fatalf("changed publisher: %v", err)
	}
	now = now.Add(365 * 24 * time.Hour)
	if _, err := prepareSeed(t.Context(), store, seed, utc, fetch); Reason(err) != "expired" || calls != 1 {
		t.Fatalf("expired existing authority: %v", err)
	}
}

func TestPrepareSeedRefusals(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	seed, _ := SeedFor("iers")
	good := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "")
	makeStore := func(t *testing.T) *Store {
		s, err := OpenStore(filepath.Join(t.TempDir(), "leap"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	for _, tc := range []struct {
		name     string
		failRead int
		o        *Object
		calls    int
	}{
		{"UTC unknown before fetch", 1, good, 0}, {"UTC lost before commit", 2, good, 1},
		{"expired table", 0, objectForTest(t, "2016-07-08T00:00:00Z", "2026-08-01T00:00:00Z", ""), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := makeStore(t)
			reads, calls := 0, 0
			_, err := prepareSeed(t.Context(), s, seed, func() (time.Time, error) {
				reads++
				if reads == tc.failRead {
					return time.Time{}, errors.New("unsynced")
				}
				return now, nil
			}, func(context.Context) (SeedResult, error) { calls++; return SeedResult{Object: tc.o}, nil })
			if err == nil || calls != tc.calls {
				t.Fatalf("error %v after %d fetches", err, calls)
			}
			disk, err := s.Load()
			if err != nil || disk.Anchor() != nil {
				t.Fatalf("refused table became authoritative: %+v, %v", disk, err)
			}
		})
	}
	t.Run("publisher retry survives another invocation", func(t *testing.T) {
		s := makeStore(t)
		at := now
		utc := func() (time.Time, error) { return at, nil }
		calls := 0
		fetch := func(context.Context) (SeedResult, error) {
			calls++
			return SeedResult{}, &SeedError{Status: 429, RetryAfter: time.Hour, reason: &Rejection{"http", "retry later"}}
		}
		if _, err := prepareSeed(t.Context(), s, seed, utc, fetch); err == nil {
			t.Fatal("publisher refusal lost")
		}
		at = now.Add(30 * time.Minute)
		if _, err := prepareSeed(t.Context(), s, seed, utc, fetch); err == nil || calls != 1 {
			t.Fatalf("retry not honored: calls=%d error=%v", calls, err)
		}
	})
	t.Run("pending authority preserved", func(t *testing.T) {
		s := makeStore(t)
		if err := s.Save(State{Pending: &Record{Object: good, Provider: Provider{Kind: "iers", Name: IERSURL}, Accepted: now}, UTCbound: now}); err != nil {
			t.Fatal(err)
		}
		_, err := prepareSeed(t.Context(), s, seed, func() (time.Time, error) { return now, nil }, func(context.Context) (SeedResult, error) { t.Fatal("unexpected fetch"); return SeedResult{}, nil })
		if Reason(err) != "pending" {
			t.Fatalf("pending state ignored: %v", err)
		}
	})
}

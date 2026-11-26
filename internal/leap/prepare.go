package leap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// PrepareSeed builds a first-install authority while the incumbent time service
// still establishes UTC. The daemon and this command use the same cache lock.
// Existing authority is verified in place and renewed by the normal updater.
func PrepareSeed(ctx context.Context, dir, mode, userAgent string, utc func() (time.Time, error)) (*Record, error) {
	seed, ok := SeedFor(mode)
	if !ok {
		return nil, fmt.Errorf("leap: prepare-leap requires iers or nist acquisition, got %q", mode)
	}
	// Installing as root in a service account's state directory would leave
	// private files the daemon cannot open after the cutover.
	if os.Geteuid() == 0 {
		i, err := os.Stat(filepath.Dir(dir))
		if err != nil {
			return nil, err
		}
		if i.Sys().(*syscall.Stat_t).Uid != 0 {
			return nil, fmt.Errorf("leap: run prepare-leap as the owner of %s", filepath.Dir(dir))
		}
	}
	store, err := OpenStore(dir)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return prepareSeed(ctx, store, seed, utc, func(ctx context.Context) (SeedResult, error) {
		// A cold cache has no body to validate with a conditional request.
		return seed.Fetch(ctx, userAgent, Validators{})
	})
}

func prepareSeed(ctx context.Context, store *Store, seed Seed, utc func() (time.Time, error), fetch func(context.Context) (SeedResult, error)) (*Record, error) {
	state, err := store.Load()
	if err != nil {
		return nil, err
	}
	now, err := utc()
	if err != nil {
		return nil, err
	}
	if now.IsZero() || now.Before(state.UTCbound) {
		return nil, Reject("time_unknown", "UTC precedes the cache bound")
	}
	if state.Pending != nil {
		return nil, Reject("pending", "start Carillon to recover the committed pending table")
	}
	provider := Provider{Kind: seed.Kind, Name: seed.URL}
	if r := state.Active; r != nil {
		if r.Provider != provider {
			return nil, Reject("provider", "existing cache uses a different provider")
		}
		if err := CheckUpdate(nil, r.Object, now); err != nil {
			return nil, fmt.Errorf("leap: existing cache requires daemon acquisition: %w", err)
		}
		return r, nil
	}
	book, err := store.LoadSeed()
	if err != nil {
		return nil, err
	}
	if retry := book.nextAttempt(); now.Before(retry) {
		return nil, fmt.Errorf("leap: next publisher request allowed at %s", retry.UTC().Format(time.RFC3339))
	}
	book = SeedState{LastAttempt: now}
	if err := store.SaveSeed(book); err != nil {
		return nil, err
	}
	started := time.Now()
	got, err := fetch(ctx)
	if err != nil {
		if wait := publisherDelay(err); wait > 0 {
			book.RetryAt = now.Add(time.Since(started) + wait)
			err = errors.Join(err, store.SaveSeed(book))
		}
		return nil, err
	}
	accepted, err := utc()
	if err != nil {
		return nil, err
	}
	if accepted.Before(now) {
		return nil, Reject("time_unknown", "UTC moved backwards during acquisition")
	}
	if err := CheckUpdate(state.Anchor(), got.Object, accepted); err != nil {
		return nil, err
	}
	r := &Record{Object: got.Object, Provider: provider, Accepted: accepted}
	// There is no live Carillon controller. Commit the complete initial
	// record atomically; startup still establishes UTC before serving it.
	state.Active, state.UTCbound, state.LastCheck = r, accepted, accepted
	if err := store.Save(state); err != nil {
		return nil, err
	}
	book.Validators = got.Validators
	if err := store.SaveSeed(book); err != nil {
		return nil, err
	}
	return r, nil
}

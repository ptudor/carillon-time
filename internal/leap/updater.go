package leap

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"log/slog"
	"time"
)

// View is clock evidence, independent of effective leap synchronization.
// UTCbound never decreases. A worker cannot establish UTC from its packets.
type View struct {
	Now      time.Time
	Known    bool
	UTCbound time.Time
	Executed time.Time
}

type Controller interface {
	LeapView() View
	ApproveLeap(context.Context, *Record) error
	ActivateLeap(context.Context, *Record) error
}

type UpdaterConfig struct {
	Mode       string
	ManualPath string
	Peers      []Peer
	Store      *Store
	Initial    State
	Controller Controller
	Report     *Report
	Log        *slog.Logger
	// UserAgent identifies this installation to the HTTPS publisher in
	// seed modes; the fetch refuses to run anonymously.
	UserAgent string
}

// Updater owns all cache state. A single acquisition job does bounded I/O
// while this owner remains available for expiration checkpoints. Offers and
// acknowledgments use the controller's bounded engine queue. On cancellation
// the network job is cancelled and drained for at most two seconds; a hung
// filesystem cannot hold the engine or process shutdown hostage.
type Updater struct {
	cfg         UpdaterConfig
	state       State
	status      UpdateStatus
	dirty       bool
	committed   bool
	nextPersist time.Time
	// seed is set in the "nist" and "iers" modes; seedState is its
	// bookkeeping from seed.json, owned here like the rest of the cache.
	seed      *Seed
	seedState SeedState
	fetchSeed func(context.Context, Validators) (SeedResult, error)
	fetchPeer func(Peer, context.Context, *Object, time.Time, *Counters, time.Duration) (*Object, error)
	spacing   time.Duration
}

func NewUpdater(cfg UpdaterConfig) *Updater {
	if cfg.Report == nil {
		cfg.Report = new(Report)
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	u := &Updater{cfg: cfg, state: cfg.Initial, committed: true, fetchPeer: Peer.fetch, spacing: 4 * time.Second}
	if seed, ok := SeedFor(cfg.Mode); ok {
		u.seed = &seed
		u.fetchSeed = func(ctx context.Context, v Validators) (SeedResult, error) { return seed.Fetch(ctx, cfg.UserAgent, v) }
		if cfg.Store != nil {
			state, err := cfg.Store.LoadSeed()
			if err != nil {
				// Validators are hints; the price of losing them is one
				// unconditional download, so start clean rather than refuse.
				cfg.Log.Warn("leap seed bookkeeping unreadable; next publisher check is unconditional", "error", err)
				state = SeedState{}
			}
			u.seedState = state
		}
	}
	u.status.Mode = cfg.Mode
	u.status.LastFetch = cfg.Initial.LastCheck
	u.status.LastResult = "awaiting UTC"
	u.publish()
	return u
}

func (u *Updater) publish() {
	u.status.Pending = ""
	if u.state.Pending != nil {
		u.status.Pending = u.state.Pending.Object.manifest.Hash()
	}
	u.cfg.Report.publish(u.status)
}

func (u *Updater) reject(err error, o *Object) {
	c := &u.cfg.Report.Counters
	c.Failures.Add(1)
	reason := Reason(err)
	if reason == "rollback" {
		c.Rollback.Add(1)
	}
	if reason == "conflict" || reason == "armed" {
		c.Conflict.Add(1)
	}
	u.status.LastResult = err.Error()
	u.status.LastRejection = reason
	u.status.Rejected = ""
	if o != nil {
		u.status.Rejected = o.manifest.Hash()
	}
	u.cfg.Log.Warn("leap update rejected", "reason", reason, "error", err)
	u.publish()
}

// saveSeed persists the publisher bookkeeping. It is not part of the
// authority record, so a failure is counted and logged but never blocks
// activation or withdraws a table.
func (u *Updater) saveSeed() {
	if u.cfg.Store == nil {
		return
	}
	if err := u.cfg.Store.SaveSeed(u.seedState); err != nil {
		u.cfg.Report.Counters.CacheFailures.Add(1)
		u.cfg.Log.Warn("leap seed bookkeeping not saved", "error", err)
	}
}

func (u *Updater) save() error {
	if err := u.cfg.Store.Save(u.state); err != nil {
		u.dirty = true
		u.nextPersist = time.Now().Add(15 * time.Minute)
		u.cfg.Report.Counters.CacheFailures.Add(1)
		return err
	}
	u.dirty = false
	u.committed = true
	u.nextPersist = time.Time{}
	return nil
}

// install performs approval -> durable commit -> applicability recheck.
// Retaining pending before Save also handles a directory-fsync failure after
// rename: retrying can never fall back to a lower rollback anchor.
func (u *Updater) install(ctx context.Context, o *Object, provider Provider) error {
	v := u.cfg.Controller.LeapView()
	if !v.Known {
		return Reject("time_unknown", "UTC is not established")
	}
	now := v.Now
	if v.UTCbound.After(now) {
		now = v.UTCbound
	}
	if err := CheckUpdate(u.state.Anchor(), o, now); err != nil {
		return err
	}
	if u.state.Active != nil && u.state.Pending == nil && u.state.Active.Object.manifest == o.manifest {
		u.status.LastResult = "unchanged"
		u.publish()
		return nil
	}
	r := &Record{Object: o, Provider: provider, Accepted: now}
	if err := u.cfg.Controller.ApproveLeap(ctx, r); err != nil {
		return err
	}
	u.state.Pending = r
	u.state.UTCbound = now
	u.committed = false
	u.dirty = true
	if err := u.save(); err != nil {
		return err
	}
	return u.activate(ctx)
}

func (u *Updater) activate(ctx context.Context) error {
	if u.state.Pending == nil || !u.committed {
		return nil
	}
	r := u.state.Pending
	if err := u.cfg.Controller.ApproveLeap(ctx, r); err != nil {
		return err
	}
	if err := u.cfg.Controller.ActivateLeap(ctx, r); err != nil {
		return err
	}
	old := ""
	if u.state.Active != nil {
		old = u.state.Active.Object.manifest.Hash()
	}
	u.state.Active, u.state.Pending = r, nil
	u.cfg.Report.Counters.Accepted.Add(1)
	u.status.LastResult = "activated"
	u.status.LastRejection = ""
	u.status.Rejected = ""
	u.cfg.Log.Info("leap table activated", "provider", r.Provider.Name, "kind", r.Provider.Kind, "key_id", r.Provider.KeyID,
		"old_sha256", old, "sha256", r.Object.manifest.Hash(), "expires", r.Object.table.Expiry)
	u.publish()
	// The object is already durable as pending, even if this compaction
	// fails. The engine can safely keep using that exact generation.
	u.dirty = true
	return u.save()
}

type fetchResult struct {
	object      *Object
	provider    Provider
	peer        int
	err         error
	validators  Validators
	notModified bool
}
type peerSchedule struct {
	next    time.Time
	backoff time.Duration
	stopped bool
	spacing time.Duration
}

const checkpointInterval = 15 * time.Minute

// checkpoint keeps object and rollback metadata in one atomic state file,
// coalescing routine UTC movement while persisting expiry and execution at
// the next worker iteration. Shutdown forces the latest observed bound.
func (u *Updater) checkpoint(v View, force bool) {
	executed := v.Executed.After(u.state.Executed)
	if executed {
		u.state.Executed = v.Executed
		u.dirty = true
	}
	expired := u.state.Active != nil && !v.UTCbound.Before(u.state.Active.Object.table.Expiry) && u.state.UTCbound.Before(u.state.Active.Object.table.Expiry)
	if v.UTCbound.After(u.state.UTCbound) && (force || executed || expired || v.UTCbound.Sub(u.state.UTCbound) >= checkpointInterval) {
		u.state.UTCbound = v.UTCbound
		u.dirty = true
	}
}

func (u *Updater) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan fetchResult, 1)
	inflight := false
	defer func() {
		cancel()
		if inflight {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}
		u.checkpoint(u.cfg.Controller.LeapView(), true)
		if u.dirty {
			// The process's auxiliary shutdown deadline bounds a stuck disk;
			// keep this final attempt on the sole cache-owning goroutine.
			if err := u.save(); err != nil {
				u.cfg.Log.Error("leap shutdown checkpoint failed", "error", err)
			}
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	schedules := make([]peerSchedule, len(u.cfg.Peers))
	nextJob := time.Time{}
	backoff := time.Duration(0)
	nextActivation := time.Time{}
	initialSchedule := true
	for {
		if ctx.Err() != nil {
			return
		}
		v := u.cfg.Controller.LeapView()
		now := time.Now() // scheduling only, never leap validity
		if v.Known && initialSchedule {
			initialSchedule = false
			if u.seed != nil {
				if u.state.Active != nil && leapCurrent(u.state.Active.Object, v) {
					checked := u.state.LastCheck
					if checked.IsZero() {
						checked = u.state.Active.Accepted
					}
					nextJob = now.Add(max(0, 24*time.Hour+jitter(24*time.Hour)-v.Now.Sub(checked)))
				}
				// A restart is not a reason to ask the publisher again: keep
				// the error floor across process lifetimes so a crash or
				// deploy loop cannot turn into a request loop.
				if wait := seedRetryFloor - v.Now.Sub(u.seedState.LastAttempt); !u.seedState.LastAttempt.IsZero() && wait > 0 {
					if floor := now.Add(wait); floor.After(nextJob) {
						nextJob = floor
					}
				}
			}
		}
		u.checkpoint(v, false)
		if u.dirty && !now.Before(u.nextPersist) {
			if err := u.save(); err != nil {
				u.reject(err, nil)
			}
		}
		if v.Known && u.state.Pending != nil && u.committed && !now.Before(nextActivation) {
			if err := u.activate(ctx); err != nil {
				u.reject(err, u.state.Anchor())
				nextActivation = now.Add(15 * time.Minute)
			}
		}
		if v.Known && !inflight && !u.dirty && !now.Before(nextJob) {
			peer := -1
			if u.cfg.Mode == "peers" {
				for i, s := range schedules {
					if !s.stopped && !now.Before(s.next) {
						peer = i
						break
					}
				}
			}
			if u.cfg.Mode == "manual" || u.seed != nil || peer >= 0 {
				inflight = true
				anchor := u.state.Anchor()
				utc := v.Now
				if v.UTCbound.After(utc) {
					utc = v.UTCbound
				}
				spacing := u.spacing
				if peer >= 0 {
					spacing = max(spacing, schedules[peer].spacing)
				}
				validators := u.seedState.Validators
				if u.seed != nil {
					// Record the attempt before it starts so an interrupted
					// process still honors the floor on restart.
					u.seedState.LastAttempt = v.Now
					u.saveSeed()
				}
				go func(peer int) {
					r := fetchResult{peer: peer}
					switch {
					case u.cfg.Mode == "manual":
						r.provider = Provider{Kind: "file", Name: u.cfg.ManualPath}
						r.object, r.err = LoadObject(u.cfg.ManualPath)
					case u.seed != nil:
						r.provider = Provider{Kind: u.seed.Kind, Name: u.seed.URL}
						var res SeedResult
						res, r.err = u.fetchSeed(ctx, validators)
						r.object, r.validators, r.notModified = res.Object, res.Validators, res.NotModified
					default:
						p := u.cfg.Peers[peer]
						r.provider = Provider{Kind: "peer", Name: p.Name, KeyID: p.Key.ID}
						r.object, r.err = u.fetchPeer(p, ctx, anchor, utc, &u.cfg.Report.Counters, spacing)
					}
					done <- r
				}(peer)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case r := <-done:
			inflight = false
			u.status.LastFetch = u.cfg.Controller.LeapView().Now
			err := r.err
			if err == nil && r.object != nil {
				err = u.install(ctx, r.object, r.provider)
			}
			if err == nil && r.object == nil {
				u.status.LastResult = "unchanged"
				if r.notModified {
					u.status.LastResult = "not modified"
				}
			}
			if err == nil && u.seed != nil {
				v := u.cfg.Controller.LeapView()
				if v.Known {
					u.state.LastCheck = v.Now
					if v.UTCbound.After(u.state.UTCbound) {
						u.state.UTCbound = v.UTCbound
					}
					if v.Now.After(u.state.UTCbound) {
						u.state.UTCbound = v.Now
					}
					u.dirty = true
				}
				// Validators describe the body just evaluated. A rejected
				// body keeps the old ones, so the next request is still
				// conditional on what we actually hold.
				if r.validators != u.seedState.Validators {
					u.seedState.Validators = r.validators
					u.saveSeed()
				}
			}
			if err != nil {
				u.reject(err, r.object)
			}
			interval := 24 * time.Hour
			if u.cfg.Mode == "manual" {
				interval = time.Hour
			}
			if r.peer >= 0 {
				s := &schedules[r.peer]
				interval = 6 * time.Hour
				if err != nil {
					s.backoff = errorBackoff(s.backoff)
					interval = s.backoff
				} else {
					s.backoff = 0
					s.spacing = 0
				}
				var pe *PeerError
				if errors.As(err, &pe) {
					interval = max(interval, pe.RetryAfter)
					s.spacing = max(s.spacing, pe.MinInterval)
					s.stopped = pe.Stop
				}
				// Positive-only jitter preserves a 24-hour unsupported floor
				// and the peer's exact RATE minimum.
				s.next = time.Now().Add(interval + jitter(interval)/2 + interval/20)
				nextJob = time.Now().Add(u.spacing)
			} else if err != nil {
				backoff = errorBackoff(backoff)
				interval = backoff
				var se *SeedError
				if errors.As(err, &se) {
					// The publisher's own instructions outrank our backoff:
					// a Retry-After is obeyed, and a request the server says
					// is wrong or gone is not repeated more than daily.
					if se.clientError() {
						interval = max(interval, 24*time.Hour)
					}
					interval = max(interval, se.RetryAfter)
				}
				// Positive-only jitter: never retry earlier than the floor.
				nextJob = time.Now().Add(interval + jitter(interval)/2 + interval/20)
			} else {
				backoff = 0
				nextJob = time.Now().Add(interval + jitter(interval))
			}
			u.publish()
		}
	}
}

func leapCurrent(o *Object, v View) bool {
	now := v.Now
	if v.UTCbound.After(now) {
		now = v.UTCbound
	}
	return CheckUpdate(nil, o, now) == nil
}

// seedRetryFloor is the shortest interval between two requests to a
// publisher, whatever happened in between, restarts included.
const seedRetryFloor = 15 * time.Minute

func errorBackoff(previous time.Duration) time.Duration {
	if previous == 0 {
		return seedRetryFloor
	}
	return min(2*previous, 6*time.Hour)
}

// Uniform +/-10%, using an independent scheduling RNG with a safe fallback.
func jitter(interval time.Duration) time.Duration {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	span := uint64(interval / 5)
	if span == 0 {
		return 0
	}
	return time.Duration(binary.BigEndian.Uint64(b[:])%span) - interval/10
}

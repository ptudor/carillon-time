// Package source defines the time sources the engine consumes and implements
// the NTP client source: a poller that exchanges packets with one upstream
// server, validates the replies, runs them through the RFC 5905 clock filter,
// and delivers the result to the engine as discipline.Measurement values.
//
// Reference clocks (PPS, GPS) implement the same Source interface in the
// refclock package.
package source

import (
	"context"
	"net/netip"
	"time"

	"github.com/ptudor/carillon-time/internal/discipline"
	"github.com/ptudor/carillon-time/internal/ntp"
)

// Clock epochs.
//
// The engine publishes an epoch counter that every source stamps on the
// measurements it emits, so a sample computed against a clock reading that
// has since been discarded can be recognised and dropped. A single "bump the
// counter before the syscall" scheme is not enough: it leaves the counter at
// its new value while the discontinuity is still executing, so a source that
// starts a sample in that window labels a pre-step observation with the
// post-step epoch and defeats the whole check (RA6X-006).
//
// The counter therefore carries two states. Even values are *settled*
// epochs. An odd value means a discontinuity — a clock step, or the source
// reset after a leap second — is executing right now, and no reading taken
// while it is in progress can be trusted. The engine increments once before
// the operation and once after, so the whole discontinuity is bracketed by
// an odd counter and every settled epoch is even.
//
// A sample is usable only when the epoch was the same settled value before
// and after it was taken, and is still that value when the engine consumes
// it. Zero means "this source does not stamp epochs" and is never produced
// by a running engine, whose first settled epoch is FirstEpoch.
const FirstEpoch uint64 = 2

// StableEpoch reports whether g is a settled clock epoch: an epoch in which
// no discontinuity was in progress. Zero is not settled — it is the
// unstamped sentinel — so callers that accept unstamped samples must test
// for it separately.
func StableEpoch(g uint64) bool { return g != 0 && g%2 == 0 }

// Pulse is one accepted reference-clock edge, as an immutable record.
//
// It exists because a per-pulse diagnostic cannot be reconstructed from
// status snapshots: Info holds only the *latest* pulse, so when several
// arrive between two publications every snapshot sees the newest one and the
// earlier ones vanish with no drop counted (RA6X-048). The record travels its
// own bounded path instead, so either a pulse is written or its loss is
// counted.
type Pulse struct {
	// Source is the configured refclock name.
	Source string

	// At is the kernel timestamp of the edge, and Offset the offset it
	// implies including the configured calibration. Sequence is the
	// device's own counter, which wraps.
	At       time.Time
	Offset   float64
	Sequence uint32
}

// Source is a producer of clock measurements.
type Source interface {
	// Name returns the configured name of the source.
	Name() string

	// Run polls the source until ctx is done, sending a Measurement after
	// every poll. It returns nil when ctx is cancelled and a non-nil error
	// only for a condition the source cannot recover from.
	Run(ctx context.Context, out chan<- discipline.Measurement) error

	// Info returns a snapshot of the source's state for status output. It
	// is lock-free and safe to call from any goroutine.
	Info() Info

	// Reset asks the source to empty its clock filter before the next
	// sample is added. The engine calls it after stepping the clock, since
	// samples taken before a step are wrong by the step amount. Reach is
	// left as-is. Safe to call from any goroutine at any time.
	Reset()
}

// Info is a snapshot of a source's state.
type Info struct {
	Name     string
	Address  string
	Resolved netip.AddrPort

	Reach  uint8
	Poll   int8
	Denied bool

	Offset     float64
	Delay      float64
	Dispersion float64
	Jitter     float64
	Stratum    uint8
	RefID      ntp.RefID
	Leap       ntp.Leap

	LastRx    time.Time
	LastError string

	Sent       uint64
	Received   uint64
	Timeouts   uint64
	Bogus      uint64
	BadAuth    uint64
	Kiss       uint64
	NoKernelTS uint64

	// Stale counts samples discarded because the clock was stepped (or a
	// leap crossed) between the start of the sample and its use: the
	// offset was measured against a clock reading that no longer holds.
	Stale uint64

	// Refclock is non-nil for a local reference clock. The pointed-to value
	// is immutable and replaced with every Info snapshot.
	Refclock *RefclockInfo
}

// RefclockInfo is the hardware/filter view of a local reference clock.
// Qualification is added by the control layer because only the discipline
// selector knows whether a numbering source currently survives.
type RefclockInfo struct {
	Type   string
	Device string
	Edge   string

	Sequence       uint32
	WindowSamples  int
	WindowJitter   float64
	IntervalJitter float64
	Stable         bool
	LastPulse      time.Time
	LastOffset     float64
	LastInterval   float64

	FixKnown     bool
	FixValid     bool
	FixQuality   int
	Satellites   int
	Sentence     string
	LastSentence time.Time
	MeasuredLag  float64
	LagSamples   int

	Samples  uint64
	Timeouts uint64
	Gaps     uint64
	Glitches uint64
	Spikes   uint64
}

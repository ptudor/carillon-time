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

	"carillon/internal/discipline"
	"carillon/internal/ntp"
)

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

	// Refclock is non-nil for a local reference clock. The pointed-to value
	// is immutable and replaced with every Info snapshot.
	Refclock *RefclockInfo
}

// RefclockInfo is the hardware/filter view of a local PPS reference clock.
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
	LastInterval   float64

	Samples  uint64
	Timeouts uint64
	Gaps     uint64
	Glitches uint64
	Spikes   uint64
}

// Package discipline is the pure core of the daemon: the RFC 5905 clock
// filter, the selection / clustering / combining algorithms, and the clock
// discipline loop. Nothing here reads a clock, starts a goroutine, or logs;
// all time is passed in as seconds on a monotonic scale, and the loop returns
// actions for the caller to apply. That is what lets the whole thing run
// inside a simulation under `go test`.
package discipline

import (
	"time"

	"carillon/internal/ntp"
)

// Physical constants of RFC 5905 §7.2 and the ntpd implementation.
const (
	// Phi is the maximum frequency tolerance assumed for an undisciplined
	// clock, in seconds per second. Dispersions grow at this rate.
	Phi = 15e-6

	// MaxDispersion is the dispersion above which a sample or source is
	// considered to carry no information (RFC 5905 MAXDISP).
	MaxDispersion = 16.0

	// MaxDistance is the root distance beyond which a source is not used
	// for synchronization (ntpd sys_maxdist default).
	MaxDistance = 1.5

	// MaxFrequency is the largest frequency correction the kernels accept,
	// in ppm (RFC 5905 MAXFREQ, 500 ppm).
	MaxFrequency = 500.0

	// AllanIntercept is the interval in seconds beyond which frequency
	// estimation by the PLL is no longer improved by longer intervals
	// (ntpd CLOCK_ALLAN = 2^11).
	AllanIntercept = 2048.0

	// FilterStages is the depth of the clock filter shift register.
	FilterStages = 8

	// MinPoll and MaxPoll bound the poll exponent (log2 seconds).
	MinPoll = 2
	MaxPoll = 17

	// reachBits is the width of a source's reachability register, and so the
	// number of missed polls after which a source that is still reporting
	// would read as unreachable.
	reachBits = 8

	// minFreshness is the floor on the freshness deadline in seconds, so a
	// very short poll cannot produce a deadline that ordinary jitter trips.
	minFreshness = 64.0
)

// Measurement is what a source delivers to the engine after its own clock
// filter has run. Offsets are "true time minus local clock": a positive
// offset means the local clock is behind.
type Measurement struct {
	// Source is the configured name of the source.
	Source string

	// Now is the monotonic time (seconds) at which the source produced this
	// measurement.
	Now float64

	// Reach is the source's 8-bit reachability register after this event:
	// a 1 bit is a poll that produced a usable sample.
	Reach uint8

	// Poll is the source's current poll exponent (log2 seconds).
	Poll int8

	// Generation is the measurement epoch the source read when it began
	// this sample. The engine bumps its own counter whenever it does
	// something that invalidates work in flight — a clock step, a leap
	// transition — so a measurement arriving with an older generation was
	// computed against a clock reading that no longer holds and must be
	// dropped rather than applied. Zero means the source does not stamp
	// generations and is never treated as stale.
	Generation uint64

	// Valid is false when a poll produced no usable sample; then only
	// Source, Now, Reach and Poll are meaningful.
	//
	// Valid means "a new estimate to consume", not "the poll succeeded".
	// A good reply whose clock filter winner is unchanged is emitted with
	// Valid = false, because there is nothing new to integrate.
	Valid bool

	// Acquired reports that this event *is* a successful acquisition from
	// the source in the current clock epoch: an authenticated, plausible
	// reply that entered the filter, an accepted PPS edge, an accepted NMEA
	// sentence. It is deliberately separate from Valid: a good reply whose
	// filter winner is unchanged is an acquisition without a new estimate,
	// and a timeout, a bad MAC, a rejected packet, an invalidation notice
	// or a sample from a superseded epoch is a heartbeat without one.
	//
	// Settling counts acquisitions, so a transport heartbeat cannot stand
	// in for the post-step evidence the state machine requires (RA6X-010).
	//
	// Valid implies acquisition — there is no new estimate without one — so
	// a producer only has to set this for the acquisition-without-estimate
	// case. Read it through IsAcquisition.
	Acquired bool

	// Invalidate explicitly discards the source's previous estimate while
	// retaining reachability. Ordinary misses leave it false so an older NTP
	// estimate can age naturally; a PPS window that loses lock sets it true.
	Invalidate bool

	// At is the monotonic time of the sample the clock filter selected.
	At float64

	// Offset, Delay, Dispersion and Jitter are the clock filter outputs
	// (RFC 5905 §10) in seconds; Delay is 0 for a reference clock.
	Offset     float64
	Delay      float64
	Dispersion float64
	Jitter     float64

	// The remaining fields are copied from the source's packets (or fixed
	// for a reference clock) and feed the server variables.
	Leap      ntp.Leap
	Stratum   uint8
	RefID     ntp.RefID
	RootDelay float64
	RootDisp  float64
	Precision int8
	RefTime   time.Time

	// SourceRefID is the reference id this daemon advertises when the
	// source is its system source: the source's address for an NTP server,
	// "GPS"/"PPS" for a reference clock. (RefID above is the source's own.)
	SourceRefID ntp.RefID
}

// Options describe a source when it is registered with a System.
type Options struct {
	// Prefer marks the source whose offset is used unaltered whenever it
	// survives selection.
	Prefer bool

	// NoSelect keeps the source visible in status output but never uses it
	// for synchronization.
	NoSelect bool

	// Numbering means the source can vouch for which second it is, and so
	// can qualify a PPS source. True for NTP and NMEA sources, false for PPS.
	Numbering bool

	// PPS marks a pulse-per-second source, which must be qualified by a
	// Numbering source before it may be used (see System).
	PPS bool
}

// IsAcquisition reports whether this event is a successful acquisition from
// the source: either it carries a new estimate, or it is an accepted sample
// whose clock filter winner did not change.
func (m Measurement) IsAcquisition() bool { return m.Valid || m.Acquired }

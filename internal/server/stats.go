package server

import (
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/ptudor/carillon-time/internal/ntp"
)

// modeCount is the number of three-bit NTP association modes.
const modeCount = 8

// versionCount is one past the highest NTP version the decoder accepts, so a
// packet's version indexes the histogram directly.
const versionCount = ntp.Version + 1

// Stats holds the lock-free NTP listener counters shared by every listener.
//
// Each datagram is attributed to the address family it arrived from, so a
// dual-stack server can chart IPv4 and IPv6 separately — the NTP pool scores
// them as two independent monitors, and a v6-only outage is otherwise
// invisible in a combined total.
type Stats struct {
	v4 counters
	v6 counters
}

// counters is one address family's set of monotonic counters. Every datagram
// the listener reads increments exactly one of the outcome counters, so
// served plus every drop reason accounts for all received traffic.
type counters struct {
	// Answered.
	served   atomic.Uint64
	unsynced atomic.Uint64 // subset of served: answered with leap=unsync
	kod      atomic.Uint64 // subset of rateLimited: RATE kiss packets sent

	// Refused.
	denied      atomic.Uint64 // source outside the [serve] ACL
	martian     atomic.Uint64 // source or destination address unusable
	rateLimited atomic.Uint64 // over the per-client token bucket
	badAuth     atomic.Uint64 // MAC verification or require_key failure
	badVersion  atomic.Uint64 // version 0 or above 4
	nonClient   atomic.Uint64 // any mode we do not answer, modes 6 and 7 included
	malformed   atomic.Uint64 // short, or a bad extension field or MAC trailer
	oversize    atomic.Uint64 // truncated or larger than MaxPacketSize

	// Reception quality.
	missingKernelTS atomic.Uint64
	kernelDrops     atomic.Uint64 // socket receive queue overflows, where reported
	clients         atomic.Int64  // distinct clients currently rate-limit tracked

	modes    [modeCount]atomic.Uint64    // nonClient broken out by mode
	versions [versionCount]atomic.Uint64 // accepted requests by client version

	lastRequest eventTime
	lastServed  eventTime
}

// family returns the counters for addr's address family. A nil Stats yields a
// throwaway set so callers never need a nil check on the packet path.
func (s *Stats) family(addr netip.Addr) *counters {
	if s == nil {
		return &counters{}
	}
	if addr.Unmap().Is4() {
		return &s.v4
	}
	return &s.v6
}

// CounterSnapshot is one address family's counters, or the sum of both.
type CounterSnapshot struct {
	Served   uint64 `json:"served"`
	Unsynced uint64 `json:"unsynced"`
	KoD      uint64 `json:"kod"`

	Denied      uint64 `json:"denied"`
	Martian     uint64 `json:"martian"`
	RateLimited uint64 `json:"ratelimited"`
	BadAuth     uint64 `json:"badauth"`
	BadVersion  uint64 `json:"badversion"`
	NonClient   uint64 `json:"nonclient"`
	Malformed   uint64 `json:"malformed"`
	Oversize    uint64 `json:"oversize"`

	NoKernelTS  uint64 `json:"no_kernel_timestamp"`
	KernelDrops uint64 `json:"kernel_drops"`
	Clients     int64  `json:"clients"`

	Modes    [modeCount]uint64    `json:"modes"`
	Versions [versionCount]uint64 `json:"versions"`

	LastRequest time.Time `json:"last_request,omitempty"`
	LastServed  time.Time `json:"last_served,omitempty"`
}

// Requests returns every datagram accounted for by this snapshot.
func (c CounterSnapshot) Requests() uint64 {
	return c.Served + c.Dropped()
}

// Dropped returns the datagrams that were refused for any reason.
func (c CounterSnapshot) Dropped() uint64 {
	return c.Denied + c.Martian + c.RateLimited + c.BadAuth +
		c.BadVersion + c.NonClient + c.Malformed + c.Oversize
}

// StatsSnapshot is a consistent-enough operational view of the independent
// monotonic counters. Exact cross-field simultaneity is not required.
type StatsSnapshot struct {
	Total CounterSnapshot `json:"total"`
	IPv4  CounterSnapshot `json:"ipv4"`
	IPv6  CounterSnapshot `json:"ipv6"`
}

// Snapshot returns the current counters per family and their sum.
func (s *Stats) Snapshot() StatsSnapshot {
	if s == nil {
		return StatsSnapshot{}
	}
	v4, v6 := s.v4.snapshot(), s.v6.snapshot()
	return StatsSnapshot{Total: sumCounters(v4, v6), IPv4: v4, IPv6: v6}
}

func (c *counters) snapshot() CounterSnapshot {
	out := CounterSnapshot{
		Served:      c.served.Load(),
		Unsynced:    c.unsynced.Load(),
		KoD:         c.kod.Load(),
		Denied:      c.denied.Load(),
		Martian:     c.martian.Load(),
		RateLimited: c.rateLimited.Load(),
		BadAuth:     c.badAuth.Load(),
		BadVersion:  c.badVersion.Load(),
		NonClient:   c.nonClient.Load(),
		Malformed:   c.malformed.Load(),
		Oversize:    c.oversize.Load(),
		NoKernelTS:  c.missingKernelTS.Load(),
		KernelDrops: c.kernelDrops.Load(),
		Clients:     c.clients.Load(),
		LastRequest: c.lastRequest.load(),
		LastServed:  c.lastServed.load(),
	}
	for i := range out.Modes {
		out.Modes[i] = c.modes[i].Load()
	}
	for i := range out.Versions {
		out.Versions[i] = c.versions[i].Load()
	}
	return out
}

func sumCounters(a, b CounterSnapshot) CounterSnapshot {
	out := CounterSnapshot{
		Served:      a.Served + b.Served,
		Unsynced:    a.Unsynced + b.Unsynced,
		KoD:         a.KoD + b.KoD,
		Denied:      a.Denied + b.Denied,
		Martian:     a.Martian + b.Martian,
		RateLimited: a.RateLimited + b.RateLimited,
		BadAuth:     a.BadAuth + b.BadAuth,
		BadVersion:  a.BadVersion + b.BadVersion,
		NonClient:   a.NonClient + b.NonClient,
		Malformed:   a.Malformed + b.Malformed,
		Oversize:    a.Oversize + b.Oversize,
		NoKernelTS:  a.NoKernelTS + b.NoKernelTS,
		KernelDrops: a.KernelDrops + b.KernelDrops,
		Clients:     a.Clients + b.Clients,
		LastRequest: later(a.LastRequest, b.LastRequest),
		LastServed:  later(a.LastServed, b.LastServed),
	}
	for i := range out.Modes {
		out.Modes[i] = a.Modes[i] + b.Modes[i]
	}
	for i := range out.Versions {
		out.Versions[i] = a.Versions[i] + b.Versions[i]
	}
	return out
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func atomicTime(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

// eventTime records the wall time of the most recent event, ordered by a
// sequence number rather than by the wall value itself.
//
// "Latest" used to mean "the largest UnixNano seen", which is wrong on a
// daemon whose job is to step the wall clock: after a backward step every
// subsequent request carried a smaller timestamp and was refused, so
// last_request and last_served stayed pinned to a pre-step future value for
// ever (RA6X-053). Ordering by arrival and storing whatever wall time that
// event carried is both monotone in *events* and honest about what the clock
// said. Concurrent listeners contend on the sequence, so the newest event
// wins regardless of which way the clock has moved.
type eventTime struct {
	seq atomic.Uint64
	ns  atomic.Int64
}

func (e *eventTime) store(t time.Time) {
	next := e.seq.Add(1)
	for {
		if cur := e.seq.Load(); cur != next {
			return // a later event has already claimed the slot
		}
		old := e.ns.Load()
		if e.ns.CompareAndSwap(old, t.UnixNano()) {
			return
		}
	}
}

func (e *eventTime) load() time.Time { return atomicTime(e.ns.Load()) }

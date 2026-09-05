package source

import (
	"math/rand/v2"
	"net/netip"
	"time"

	"carillon/internal/ntp"
)

// adaptPoll implements the RFC 5905 poll-interval rule: when the offset is
// within four times the jitter the clock is tracking well and the interval
// grows; otherwise it shrinks. The result is clamped to [min, max].
func adaptPoll(poll int8, offset, jitter float64, min, max int8) int8 {
	if offset < 0 {
		offset = -offset
	}
	if offset < 4*jitter {
		poll++
	} else {
		poll--
	}
	return clampPoll(poll, min, max)
}

func clampPoll(poll, min, max int8) int8 {
	if poll < min {
		return min
	}
	if poll > max {
		return max
	}
	return poll
}

// pollInterval returns 2^poll seconds with ±5 % random jitter so that many
// clients started together do not stay synchronized to the same instant.
func pollInterval(poll int8) time.Duration {
	base := ntp.Log2Seconds(poll)
	jitter := 1 + (rand.Float64()*2-1)*pollJitter
	return time.Duration(base * jitter * float64(time.Second))
}

// clampPrecision bounds a peer's advertised precision to a sane range: a
// positive exponent would mean a clock coarser than a second, and anything
// below 2^-30 is beyond what any host clock delivers.
func clampPrecision(p int8) int8 {
	switch {
	case p > 0:
		return 0
	case p < -30:
		return -30
	}
	return p
}

// sameEndpoint reports whether two address/port pairs name the same peer,
// treating IPv4-mapped IPv6 addresses as their IPv4 form.
func sameEndpoint(a, b netip.AddrPort) bool {
	return a.Port() == b.Port() && a.Addr().Unmap() == b.Addr().Unmap()
}

package source

import (
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"strconv"
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

// splitHostPort parses an upstream address: a hostname, an IPv4 or IPv6
// literal, or any of those with a port ("host:123", "[2001:db8::1]:123").
// A bare IPv6 literal without brackets is accepted as a host. The default
// port is 123.
func splitHostPort(address string) (host string, port uint16, err error) {
	if address == "" {
		return "", 0, fmt.Errorf("empty address")
	}
	if a, perr := netip.ParseAddr(address); perr == nil {
		return a.String(), ntp.Port, nil
	}
	h, p, serr := net.SplitHostPort(address)
	if serr != nil {
		// No port present (a hostname, or a bracketed literal without one).
		if len(address) > 2 && address[0] == '[' && address[len(address)-1] == ']' {
			inner := address[1 : len(address)-1]
			if _, perr := netip.ParseAddr(inner); perr == nil {
				return inner, ntp.Port, nil
			}
		}
		if len(address) > 0 && address[0] != '[' && !containsColon(address) {
			return address, ntp.Port, nil
		}
		return "", 0, fmt.Errorf("invalid address %q: %w", address, serr)
	}
	if h == "" {
		return "", 0, fmt.Errorf("invalid address %q: empty host", address)
	}
	n, aerr := strconv.ParseUint(p, 10, 16)
	if aerr != nil || n == 0 {
		return "", 0, fmt.Errorf("invalid address %q: bad port %q", address, p)
	}
	return h, uint16(n), nil
}

func containsColon(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return true
		}
	}
	return false
}

// sameEndpoint reports whether two address/port pairs name the same peer,
// treating IPv4-mapped IPv6 addresses as their IPv4 form.
func sameEndpoint(a, b netip.AddrPort) bool {
	return a.Port() == b.Port() && a.Addr().Unmap() == b.Addr().Unmap()
}
